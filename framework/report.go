package framework

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
)

// Reject marks err as an expected rejection, for a handler that refuses an
// event on purpose: a Cognito pre-sign-up trigger turning down a disposable
// email, a failed anti-robot check.
//
//	return nil, framework.Reject(errors.New("disposable email"))
//
// The caller receives it as it would receive err: the same message, the same
// errorType in Lambda, and errors.Is and errors.As reach err through it. Only
// the record changes: the framework writes it at Warn, as "handler rejected
// the event", instead of at Error, so that alerting on the error logs does not
// fire for a refusal the handler meant; its exception_name is still err's
// type. A rejection wrapped further (fmt.Errorf("check sign-up: %w",
// rejection)) is still one.
//
// It is for Handle and Consume on events that are not API Gateway requests. A
// consumer's rejection still fails the event: Lambda retries it, and it may
// reach the DLQ. A route, a stream and any other handler of an API Gateway
// request answer an expected failure with a 4xx of package response instead:
// a Reject they return answers the 500 envelope and is written at Error, a
// failure like any other.
//
// Reject(nil) is nil.
func Reject(err error) error {
	if err == nil {
		return nil
	}
	return &rejectionError{err: err}
}

// exceptionNameFallback is the exception_name Powertools gives an error
// without a type of its own (a wrapped sentinel, an errors.New).
const exceptionNameFallback = "error"

// rejectionError is the error Reject returns: err, marked as expected.
type rejectionError struct {
	err error
}

func (e *rejectionError) Error() string { return e.err.Error() }

func (e *rejectionError) Unwrap() error { return e.err }

// isRejection reports whether err's chain holds a rejection, however far it
// was wrapped.
func isRejection(err error) bool {
	var marked *rejectionError
	return errors.As(err, &marked)
}

// reportError writes down, once, the error a business handler returned, and
// returns it unchanged: at Warn for a rejection (see Reject), else at Error as
// reportFailure does. It is the single door failures go through
// (Container.report covers those around the handler, such as a payload that
// does not decode): a handler returns its error and logs nothing, since
// logging it too would report one failure twice, under two messages that
// error tracking files as two issues. The context only the handler knows
// travels wrapped into the error (fmt.Errorf("charge order %v: %w", id, err)),
// so this one line still carries it. Where the line goes from CloudWatch is
// not this package's business.
//
// The Warn record names the error in exception_name as Powertools would
// without the mark, by the rule of errorType, so that the logs and Lambda
// name a failure the same way; exception stays its message.
func reportError(ctx context.Context, logger *slog.Logger, err error) error {
	if isRejection(err) {
		logger.WarnContext(ctx, "handler rejected the event", "err", err,
			logging.KeyExceptionName, errorType(err, exceptionNameFallback))
		return err
	}
	return reportFailure(ctx, logger, err)
}

// reportFailure is reportError without rejections: it writes err down at
// Error whatever its chain holds. An API invocation reports through it, since
// an API answers a rejection with the 500 envelope, a failure like any other:
// a route answers an expected failure with a 4xx of package response.
func reportFailure(ctx context.Context, logger *slog.Logger, err error) error {
	if err != nil {
		logger.ErrorContext(ctx, "handler returned an error", "err", err)
	}
	return err
}

// reportPanic is deferred by every invocation so that a panicking handler is
// written down like any other failure, with its stack, captured here because
// re-panicking replaces it. The panic is then re-raised so Lambda still
// records a failed invocation (Handler.Call answers API Gateway instead).
func reportPanic(ctx context.Context, logger *slog.Logger) {
	rec := recover() //nolint:revive // reportPanic is itself the deferred function: defer reportPanic(...)
	if rec == nil {
		return
	}

	logger.ErrorContext(ctx, "handler panicked", "panic", rec, "stack", string(debug.Stack()))

	panic(rec)
}
