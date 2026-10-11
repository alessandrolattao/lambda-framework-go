package framework

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// The tracing tests change the global OpenTelemetry state (the tracer the
// Powertools tracer registers), so none of them runs in parallel.

// recordingTracer returns a tracer provider that keeps the spans it ends in
// memory, and resets the global OpenTelemetry state when the test ends.
func recordingTracer(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()

	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})

	recorder := tracetest.NewSpanRecorder()
	return sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)), recorder
}

// tracedContainer is a container built by New whose spans land in a
// recorder, without AWS clients.
func tracedContainer(t *testing.T) (*Container, *tracetest.SpanRecorder) {
	t.Helper()
	tp, recorder := recordingTracer(t)
	c := New(WithTracerProvider(tp), WithLogging(logging.WithWriter(&bytes.Buffer{})))
	c.lifecycle = nil // no AWS clients
	return c, recorder
}

// spanNamed returns the ended span called name.
func spanNamed(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range recorder.Ended() {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("no span named %q", name)
	return nil
}

// xrayHeader is the X-Ray trace header of the tracing tests, and traceRoot the
// trace id it carries in OpenTelemetry form.
const (
	xrayHeader = "Root=1-5759e988-bd862e3fe1be46a994272793;Parent=53995c3f42cd8ad8;Sampled=1"
	traceRoot  = "5759e988bd862e3fe1be46a994272793"
)

// businessSpan is the name of the span the business handler runs in.
const businessSpan = "## businessLogic"

func TestHandle_TheInvocationIsAServerSpanUnderTheLambdaTrace(t *testing.T) {
	// What Lambda sets for every invocation at concurrency 1.
	t.Setenv("_X_AMZN_TRACE_ID", xrayHeader)
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "orders-get")
	c, recorder := tracedContainer(t)

	h := lambda.NewHandler(c.Handle(func(context.Context, *Container, jsontext.Value) (any, error) {
		return struct{}{}, nil
	}))
	_, err := h.Invoke(lambdaContext(t), []byte(`{}`))
	require.NoError(t, err)

	server := spanNamed(t, recorder, "orders-get")
	assert.Equal(t, trace.SpanKindServer, server.SpanKind())
	assert.Equal(t, traceRoot, server.SpanContext().TraceID().String(), "the span joins the trace Lambda started")

	business := spanNamed(t, recorder, businessSpan)
	assert.Equal(t, server.SpanContext().SpanID(), business.Parent().SpanID(), "the handler's span nests in the invocation")
}

func TestHandle_AFailureMarksTheSpans(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "orders-get")
	c, recorder := tracedContainer(t)

	h := lambda.NewHandler(c.Handle(func(context.Context, *Container, jsontext.Value) (any, error) {
		return nil, errUpsert
	}))
	_, err := h.Invoke(lambdaContext(t), []byte(`{}`))
	require.Error(t, err)

	business := spanNamed(t, recorder, businessSpan)
	assert.Equal(t, codes.Error, business.Status().Code)
	assert.Equal(t, codes.Error, spanNamed(t, recorder, "orders-get").Status().Code)
}

func TestHandle_AWSCallsAreSpansUnderTheHandler(t *testing.T) {
	tp, recorder := recordingTracer(t)

	// A local stand-in for S3, so the call needs neither network nor AWS.
	s3Stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if _, err := w.Write([]byte(`<ListAllMyBucketsResult></ListAllMyBucketsResult>`)); err != nil {
			t.Errorf("s3 stub: %v", err)
		}
	}))
	t.Cleanup(s3Stub.Close)
	t.Setenv("AWS_ENDPOINT_URL_S3", s3Stub.URL)
	t.Setenv("AWS_REGION", testRegion)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	c := New(WithTracerProvider(tp), WithLogging(logging.WithWriter(&bytes.Buffer{})))

	h := c.Consume(func(ctx context.Context, c *Container, _ jsontext.Value) error {
		_, err := c.S3.ListBuckets(ctx, &s3.ListBucketsInput{})
		return err
	})
	require.NoError(t, h.Call(lambdaContext(t), jsontext.Value(`{}`)))

	business := spanNamed(t, recorder, businessSpan)
	var children []string
	for _, span := range recorder.Ended() {
		if span.Parent().SpanID() == business.SpanContext().SpanID() {
			children = append(children, span.Name())
		}
	}
	assert.NotEmpty(t, children, "the S3 call is traced as a child of the handler's span")
}

func TestHandle_AFailedAPIHandlerMarksTheInvocationSpan(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "orders-get")
	c, recorder := tracedContainer(t)

	h := lambda.NewHandler(c.Handle(func(context.Context, *Container, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		return events.APIGatewayV2HTTPResponse{}, errUpsert
	}))
	_, err := h.Invoke(lambdaContext(t), []byte(`{}`))
	require.NoError(t, err, "the client gets the 500 envelope")

	assert.Equal(t, codes.Error, spanNamed(t, recorder, "orders-get").Status().Code,
		"X-Ray still shows the invocation failed")
}

func TestHandle_ATraceInTheContextIsJoinedWithoutTheEnvironment(t *testing.T) {
	t.Setenv("_X_AMZN_TRACE_ID", "") // what aws-lambda-go leaves with concurrency above 1
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "orders-get")
	c, recorder := tracedContainer(t)

	h := lambda.NewHandler(c.Consume(func(context.Context, *Container, jsontext.Value) error { return nil }))
	//nolint:staticcheck,revive // aws-lambda-go stores the header under this very string key
	ctx := context.WithValue(lambdaContext(t), "x-amzn-trace-id", xrayHeader)
	_, err := h.Invoke(ctx, []byte(`{}`))
	require.NoError(t, err)

	assert.Equal(t, traceRoot, spanNamed(t, recorder, "orders-get").SpanContext().TraceID().String())
}

func TestNew_OpenTelemetryErrorsAreWarningsAtMost(t *testing.T) { //nolint:paralleltest // it changes the global OpenTelemetry state
	recordingTracer(t) // restores the global state New changes
	var logged bytes.Buffer
	New(WithLogging(logging.WithWriter(&logged)))

	otel.Handle(errBoom)

	assert.Contains(t, logged.String(), `"level":"WARN"`, "error tracking reads Error as a failed invocation")
	assert.Contains(t, logged.String(), "boom")
}

func TestNew_TheServiceDefaultsToTheFunction(t *testing.T) {
	t.Setenv("POWERTOOLS_SERVICE_NAME", "")
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "orders-get")
	recordingTracer(t)
	assert.Equal(t, "orders-get", New().Logger.Service())

	t.Setenv("POWERTOOLS_SERVICE_NAME", "orders")
	assert.Equal(t, "orders", New().Logger.Service())
	assert.Equal(t, "billing", New(WithService("billing")).Logger.Service())
}

func TestClose_LeavesAProvidedTracerProviderAlone(t *testing.T) { //nolint:paralleltest // it changes the global OpenTelemetry state
	tp, recorder := recordingTracer(t)
	c := New(WithTracerProvider(tp), WithLogging(logging.WithWriter(&bytes.Buffer{})))
	require.NoError(t, c.Close(t.Context()))

	_, span := tp.Tracer("test").Start(t.Context(), "after close")
	span.End()
	assert.Len(t, recorder.Ended(), 1, "the caller's provider still works")
}

func TestHandle_TheInvocationSpanDescribesTheAPIRequest(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "orders-get")
	c, recorder := tracedContainer(t)

	for _, status := range []int{404, 503} {
		recorder.Reset()
		h := lambda.NewHandler(apiHandler(c, func() (events.APIGatewayV2HTTPResponse, error) {
			return events.APIGatewayV2HTTPResponse{StatusCode: status}, nil
		}))
		request := events.APIGatewayV2HTTPRequest{
			RouteKey: "GET /orders/{id}",
			RawPath:  "/orders/42",
			RequestContext: events.APIGatewayV2HTTPRequestContext{
				HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{Method: "GET", SourceIP: "203.0.113.7"},
			},
		}
		payload, err := json.Marshal(request)
		require.NoError(t, err)
		_, err = h.Invoke(lambdaContext(t), payload)
		require.NoError(t, err)

		server := spanNamed(t, recorder, "orders-get")
		attrs := map[attribute.Key]attribute.Value{}
		for _, kv := range server.Attributes() {
			attrs[kv.Key] = kv.Value
		}
		assert.Equal(t, "GET", attrs[semconv.HTTPRequestMethodKey].AsString())
		assert.Equal(t, "/orders/{id}", attrs[semconv.HTTPRouteKey].AsString())
		assert.Equal(t, "/orders/42", attrs[semconv.URLPathKey].AsString())
		assert.Equal(t, int64(status), attrs[semconv.HTTPResponseStatusCodeKey].AsInt64())
		assert.Equal(t, "203.0.113.7", attrs[semconv.ClientAddressKey].AsString())
		assert.Equal(t, "/orders/{id}", attrs["http_route"].AsString())
		assert.Equal(t, int64(status), attrs["http_status"].AsInt64())
		assert.Subset(t, attrs["aws.xray.annotations"].AsStringSlice(), []string{"http_route", "http_status"})

		wantFault := status >= 500
		assert.Equal(t, wantFault, server.Status().Code == codes.Error, "status %d: only a 5xx is a fault", status)
	}
}
