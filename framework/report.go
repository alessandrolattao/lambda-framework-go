package framework

import (
	"context"
	"log/slog"
	"runtime/debug"
)

// reportError writes down, once and at Error, the error a business handler
// returned, and returns it unchanged. It is the single door failures go
// through (Container.report covers those around the handler, such as a payload
// that does not decode): a handler returns its error and logs nothing, since
// logging it too would report one failure twice, under two messages that error
// tracking files as two issues. The context only the handler knows travels
// wrapped into the error (fmt.Errorf("charge order %v: %w", id, err)), so this
// one line still carries it. Where the line goes from CloudWatch is not this
// package's business.
func reportError(ctx context.Context, logger *slog.Logger, err error) error {
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
