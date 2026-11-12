package framework

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
	"github.com/alessandrolattao/powertools-lambda-go/middleware"
	"github.com/alessandrolattao/powertools-lambda-go/tracing"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"go.opentelemetry.io/otel/trace"
)

var (
	// errPanicked marks the span of an invocation that panicked.
	errPanicked = errors.New("handler panicked")

	// errInitialize wraps a failure to initialize the container.
	errInitialize = errors.New("initialize container")
)

// Handle adapts h, a business handler that answers with a result, into the
// handler lambda.Start expects:
//
//	lambda.Start(c.Handle(HandleRequest))
//
// E and R are whatever event and response types h declares: API Gateway,
// Cognito triggers, a jsontext.Value carrying the raw payload of a direct
// invocation.
//
// Every invocation runs through the Powertools decorators, as Powertools
// recommends stacking them: the logger's InjectContext (invocation keys,
// correlation id, sampling, buffering), the tracer's CaptureLambdaHandler
// (the invocation span, ColdStart and Service annotations) and the metrics'
// LogMetrics (one metric set per invocation, flushed at its end). Inside
// them the container is initialized (once per cold start), the business
// handler runs in a "## businessLogic" span, and a returned error or a panic
// is reported through the logger.
func (c *Container) Handle[E, R any](h func(context.Context, *Container, E) (R, error)) Handler[E, R] {
	handler := Handler[E, R]{c: c, fn: h}
	correlation := correlationPath[E]()
	handler.call = decorate(c, handler.callInner, correlation)
	handler.run = decorate(c, handler.invoke, correlation)
	return handler
}

// Consume is Handle for business handlers that only succeed or fail, such as
// EventBridge, SQS, S3 and CloudWatch Logs consumers:
//
//	lambda.Start(c.Consume(HandleEvent))
//
// The handler returns an error so a failure is a failure: Lambda retries the
// event and, when the retries run out, it lands in the DLQ. A consumer that
// swallowed its errors would turn "the event started nothing" into silence.
func (c *Container) Consume[E any](h func(context.Context, *Container, E) error) Consumer[E] {
	return Consumer[E]{handler: c.Handle(func(ctx context.Context, c *Container, event E) (noResult, error) {
		return noResult{}, h(ctx, c, event)
	})}
}

// Handler is what Handle returns. lambda.Start takes it as a lambda.Handler:
// it decodes the payload into E, runs the business handler and encodes the
// result, with encoding/json/v2 both ways. Call runs it with an event that is
// already decoded, which is what a test wants.
//
// Because the JSON is done here, the JSON options of aws-lambda-go
// (WithSetEscapeHTML, WithUseNumber, WithDisallowUnknownFields, ...) do not
// apply to it; its runtime options (WithContext, WithEnableSIGTERM) do.
type Handler[E, R any] struct {
	c    *Container
	fn   func(context.Context, *Container, E) (R, error)
	call middleware.Handler[E, R]                           // the decorated Call
	run  middleware.Handler[jsontext.Value, jsontext.Value] // the decorated Invoke
}

// Consumer is what Consume returns: a Handler without a result.
type Consumer[E any] struct {
	handler Handler[E, noResult]
}

// noResult is the result of a Consumer. It encodes as JSON null, which is what
// aws-lambda-go answers for a handler without a result.
type noResult struct{}

// MarshalJSON implements json.Marshaler.
func (noResult) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

var (
	_ lambda.Handler = Handler[struct{}, struct{}]{}
	_ lambda.Handler = Consumer[struct{}]{}
)

// decorate stacks the Powertools decorators around h, the outermost first, as
// Powertools documents them: logger, tracer, metrics. Invoke runs them on the
// raw payload, so that a payload that does not decode still runs in the
// invocation's span and log context; Call runs them on the decoded event. A
// utility the container lacks is skipped.
func decorate[E, R any](c *Container, h middleware.Handler[E, R], correlation string) middleware.Handler[E, R] {
	if c.Metrics != nil {
		h = c.Metrics.LogMetrics(h)
	}
	if c.Tracer != nil {
		h = c.Tracer.CaptureLambdaHandler(h)
	}
	if c.Logger != nil {
		var opts []logging.InjectOption
		if correlation != "" {
			opts = append(opts, logging.WithCorrelationIDPath(correlation))
		}
		h = c.Logger.InjectContext(h, opts...)
	}
	return h
}

// correlationPath returns the Powertools correlation id path of the event
// type E, or "" for a type without one.
func correlationPath[E any]() string {
	var event E
	switch any(event).(type) {
	case events.APIGatewayV2HTTPRequest, events.APIGatewayProxyRequest:
		return logging.CorrelationAPIGatewayHTTP
	case events.LambdaFunctionURLRequest:
		return logging.CorrelationLambdaFunctionURL
	case events.ALBTargetGroupRequest:
		return logging.CorrelationApplicationLoadBalancer
	case events.EventBridgeEvent:
		return logging.CorrelationEventBridge
	default:
		return ""
	}
}

// Invoke implements lambda.Handler. A payload that does not decode and a
// result that does not encode are failures like any other: reported, and
// returned to the runtime.
func (h Handler[E, R]) Invoke(ctx context.Context, payload []byte) ([]byte, error) {
	out, err := h.run(ctx, payload)
	return out, err
}

func (h Handler[E, R]) invoke(ctx context.Context, payload jsontext.Value) (jsontext.Value, error) {
	event, err := decodeEvent[E](payload)
	if err != nil {
		return nil, toRuntime(h.c.report(ctx, err), errorTypeInvalidEvent)
	}
	result, err := h.callInner(ctx, event)
	if err != nil {
		if errors.Is(err, errInitialize) {
			return nil, toRuntime(err, errorTypeInit)
		}
		return nil, toRuntime(err, errorType(err, errorTypeHandler))
	}
	out, err := json.Marshal(result, json.Deterministic(true))
	if err != nil {
		return nil, toRuntime(h.c.report(ctx, fmt.Errorf("encode %T result: %w", result, err)), errorTypeInvalidResult)
	}
	return out, nil
}

// Call runs one invocation with an event that is already decoded, through the
// same Powertools decorators as Invoke.
func (h Handler[E, R]) Call(ctx context.Context, event E) (R, error) {
	return h.call(ctx, event)
}

// callInner runs the business handler for one invocation.
//
// An API Gateway handler (HTTP API or REST API) that fails still answers its
// client. The error or the panic is reported. Then the client gets the
// response the handler returned if it built one, and the standard 500
// envelope if it did not. Without this, API Gateway would replace the body
// with its own generic {"message":"Internal Server Error"}. The price is that
// Lambda counts the invocation as a success: alarm on the error logs, not on
// the Errors metric.
func (h Handler[E, R]) callInner(ctx context.Context, event E) (result R, err error) {
	if status, unhandled := apiResponse(&result); status != nil {
		start := time.Now()
		defer func() {
			// A panic has already been written down by reportPanic.
			if recover() != nil || err != nil {
				if *status == 0 {
					unhandled()
				}
				err = nil
			}
			h.c.apiCompleted(ctx, event, *status, time.Since(start))
		}()
	}

	err = h.c.invoke(ctx, event, func(ctx context.Context, c *Container) error {
		var fnErr error
		result, fnErr = h.fn(ctx, c, event)
		return fnErr
	})
	return result, err
}

// Invoke implements lambda.Handler.
func (h Consumer[E]) Invoke(ctx context.Context, payload []byte) ([]byte, error) {
	return h.handler.Invoke(ctx, payload)
}

// Call runs one invocation with an event that is already decoded.
func (h Consumer[E]) Call(ctx context.Context, event E) error {
	_, err := h.handler.Call(ctx, event)
	return err
}

// decodeEvent decodes the invocation payload into the event type the handler
// declares.
func decodeEvent[E any](payload []byte) (E, error) {
	var event E
	if err := json.Unmarshal(payload, &event); err != nil {
		return event, fmt.Errorf("decode %T event: %w", event, err)
	}
	return event, nil
}

// invoke runs one invocation of a business handler: everything Handler and
// Consumer share.
//
// The business handler shares the container: it is read-only once
// initialized, and what belongs to the invocation (the request id, the
// client's address, the metric set) lives in the context, which the
// Powertools logger and metrics read it from.
func (c *Container) invoke(ctx context.Context, event any, run func(context.Context, *Container) error) (err error) {
	logger := c.logger()
	invocation := trace.SpanFromContext(ctx)

	err = errPanicked // what the span records if the handler never returns
	_, api := describeAPI(event)
	if api {
		// Call answers a failed API request with a 500 and no error, so the
		// tracer would not see the failure: mark the invocation span here.
		defer func() { recordError(invocation, err) }()
	}
	defer reportPanic(ctx, logger)

	if api {
		logging.AppendKeys(ctx, "client_ip", c.apiClientIP(event))
	}

	if initErr := c.initialize(ctx, logger); initErr != nil {
		return reportError(ctx, logger, fmt.Errorf("%w: %w", errInitialize, initErr))
	}

	logEvent(ctx, logger, event)

	return reportError(ctx, logger, c.businessLogic(ctx, run))
}

// businessLogic runs the business handler in its own span, "## businessLogic",
// so that X-Ray tells the time it took from the framework's.
func (c *Container) businessLogic(ctx context.Context, run func(context.Context, *Container) error) error {
	if c.Tracer == nil {
		return run(ctx, c)
	}
	_, err := c.Tracer.Capture(ctx, "businessLogic", func(ctx context.Context) (struct{}, error) {
		return struct{}{}, run(ctx, c)
	}, tracing.WithCaptureResponse(false))
	return err //nolint:wrapcheck // the business handler's own error, reported as it is
}

// logger returns the container's logger, or slog's default for a container
// assembled without one.
func (c *Container) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.Default()
	}
	return c.Logger.Logger
}

// report writes down a failure that happened around the business handler
// rather than in it: a payload that does not decode, a result that does not
// encode.
func (c *Container) report(ctx context.Context, err error) error {
	c.logger().ErrorContext(ctx, "invocation failed", "err", err)
	return err
}

// apiCompleted records how an API request ended: a log line, which the 4xx
// leave no other trace in, and the request on the invocation's span.
func (c *Container) apiCompleted(ctx context.Context, event any, status int, took time.Duration) {
	r, _ := describeAPI(event) // a non-API event leaves the fields empty
	c.logger().InfoContext(ctx, "API request completed",
		"status", status,
		"routeKey", r.routeKey,
		"durationMs", milliseconds(took),
	)
	annotateAPI(ctx, &r, status, c.apiClientIP(event))
}

// milliseconds is d as fractional milliseconds, the unit every duration in
// the logs is written in.
func milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
