package framework

import (
	"context"
	"encoding/json/jsontext"
	"math"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/lambda-framework-go/response"
)

// testRequestID is the request id lambdaContext hands every invocation.
const testRequestID = "req-1"

// lambdaContext is what the Lambda runtime hands every invocation.
func lambdaContext(t *testing.T) context.Context {
	t.Helper()
	return lambdacontext.NewContext(t.Context(), &lambdacontext.LambdaContext{AwsRequestID: testRequestID})
}

func TestHandle_TheHandlerLogsCarryTheInvocation(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()

	h := c.Handle(func(ctx context.Context, c *Container, _ jsontext.Value) (any, error) {
		c.Logger.InfoContext(ctx, "inside")
		return struct{}{}, nil
	})

	_, err := h.Call(lambdaContext(t), jsontext.Value(`{}`))
	require.NoError(t, err)
	assert.Contains(t, logged.String(), `"message":"inside"`)
	assert.Contains(t, logged.String(), `"function_request_id":"req-1"`)
}

func TestHandle_ReturnsTheResult(t *testing.T) {
	t.Parallel()

	c := &Container{Logger: quietLogger()}

	h := c.Handle(func(context.Context, *Container, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		return events.APIGatewayV2HTTPResponse{StatusCode: 204}, nil
	})

	resp, err := h.Call(lambdaContext(t), events.APIGatewayV2HTTPRequest{})
	require.NoError(t, err)
	assert.Equal(t, 204, resp.StatusCode)
}

func TestHandle_APIRequestLogsCarryTheClientIP(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()

	h := c.Handle(func(context.Context, *Container, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		return events.APIGatewayV2HTTPResponse{}, nil
	})

	_, err := h.Call(lambdaContext(t), events.APIGatewayV2HTTPRequest{
		Headers: map[string]string{"x-forwarded-for": "1.2.3.4"},
		RequestContext: events.APIGatewayV2HTTPRequestContext{
			HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{SourceIP: "203.0.113.7"},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, logged.String(), `"client_ip":"203.0.113.7"`, "the peer, not what the client wrote")
}

func TestHandle_ReportsTheErrorOnce(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()

	h := c.Handle(func(context.Context, *Container, jsontext.Value) (any, error) {
		return nil, errUpsert
	})

	_, err := h.Call(lambdaContext(t), jsontext.Value(`{}`))
	require.ErrorIs(t, err, errUpsert, "the runtime must still see the failure")
	assert.Equal(t, 1, strings.Count(logged.String(), `"level":"ERROR"`), "exactly one error record")
}

func TestHandle_ReportsAndReRaisesAPanic(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()

	h := c.Handle(func(context.Context, *Container, jsontext.Value) (any, error) {
		panic(errBoom)
	})

	assert.PanicsWithValue(t, errBoom, func() {
		_, err := h.Call(lambdaContext(t), jsontext.Value(`{}`))
		t.Errorf("returned %v instead of panicking", err)
	})
	assert.Contains(t, logged.String(), "handler panicked")
}

func TestConsume_ReportsTheError(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()

	h := c.Consume(func(context.Context, *Container, events.SQSEvent) error {
		return errUpsert
	})

	err := h.Call(lambdaContext(t), events.SQSEvent{Records: make([]events.SQSMessage, 3)})
	require.ErrorIs(t, err, errUpsert)
	assert.Contains(t, logged.String(), `"recordCount":3`)
	assert.Contains(t, logged.String(), "handler returned an error")
}

func TestHandle_InitializesAContainerBuiltByNew(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-west-1")
	c := New()
	c.Logger = quietLogger()

	h := c.Consume(func(_ context.Context, inv *Container, _ events.CloudWatchEvent) error {
		assert.NotNil(t, inv.S3, "the copy sees the clients the first invocation built")
		return nil
	})

	require.NoError(t, h.Call(lambdaContext(t), events.CloudWatchEvent{}))
	assert.NotNil(t, c.S3, "the clients stay on the shared container for the next invocation")
}

// TestHandle_DirectInvokeThroughTheLambdaRuntime runs the wrapped handler the
// way lambda.Start does, to prove aws-lambda-go can hand it a jsontext.Value.
func TestHandle_DirectInvokeThroughTheLambdaRuntime(t *testing.T) {
	t.Parallel()

	c := &Container{Logger: quietLogger()}

	var got jsontext.Value
	h := lambda.NewHandler(c.Handle(func(_ context.Context, _ *Container, in jsontext.Value) (map[string]int, error) {
		got = in
		return map[string]int{"len": len(in)}, nil
	}))

	out, err := h.Invoke(lambdaContext(t), []byte(`{"orderId":42}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"orderId":42}`, string(got))
	assert.JSONEq(t, `{"len":14}`, string(out))
}

func TestHandle_ResultIsEncodedWithJSONv2(t *testing.T) {
	t.Parallel()

	c := &Container{Logger: quietLogger()}

	type result struct {
		Items []string          `json:"items"`
		Tags  map[string]string `json:"tags"`
	}
	h := lambda.NewHandler(c.Handle(func(context.Context, *Container, jsontext.Value) (result, error) {
		return result{}, nil
	}))

	out, err := h.Invoke(lambdaContext(t), []byte(`{}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"items":[],"tags":{}}`, string(out), "nil slices and maps are empty, not null")
}

func TestHandle_EventIsDecodedWithJSONv2(t *testing.T) {
	t.Parallel()

	c := &Container{Logger: quietLogger()}

	type order struct {
		OrderID int `json:"orderId"`
	}
	var got order
	h := lambda.NewHandler(c.Handle(func(_ context.Context, _ *Container, in order) (any, error) {
		got = in
		return struct{}{}, nil
	}))

	_, err := h.Invoke(lambdaContext(t), []byte(`{"ORDERID":7}`))
	require.NoError(t, err)
	assert.Zero(t, got.OrderID, "names match case-sensitively, as in v2")

	_, err = h.Invoke(lambdaContext(t), []byte(`{"orderId":1,"orderId":2}`))
	require.Error(t, err, "duplicate names are rejected, as in v2")
}

func TestConsume_AnswersNullThroughTheLambdaRuntime(t *testing.T) {
	t.Parallel()

	c := &Container{Logger: quietLogger()}

	h := lambda.NewHandler(c.Consume(func(context.Context, *Container, events.SQSEvent) error {
		return nil
	}))

	out, err := h.Invoke(lambdaContext(t), []byte(`{"Records":[]}`))
	require.NoError(t, err)
	assert.Equal(t, "null", string(out))
}

// apiHandler is an API Gateway HTTP handler that fails the way fail says.
func apiHandler(c *Container, fail func() (events.APIGatewayV2HTTPResponse, error)) Handler[events.APIGatewayV2HTTPRequest, events.APIGatewayV2HTTPResponse] {
	return c.Handle(func(context.Context, *Container, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		return fail()
	})
}

func TestHandle_AFailedAPIHandlerAnswersWithTheEnvelope(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()

	h := apiHandler(c, func() (events.APIGatewayV2HTTPResponse, error) {
		return events.APIGatewayV2HTTPResponse{}, errUpsert
	})

	resp, err := h.Call(lambdaContext(t), events.APIGatewayV2HTTPRequest{})
	require.NoError(t, err, "API Gateway must get a response, not a Lambda error")
	assert.Equal(t, 500, resp.StatusCode)
	assert.Contains(t, resp.Body, `"code":"serverError"`)
	assert.Equal(t, "no-store", resp.Headers["Cache-Control"])
	assert.Contains(t, logged.String(), "upsert intervention: connection refused", "and the error is still reported")
}

func TestHandle_AFailedAPIHandlerKeepsTheResponseItBuilt(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()

	h := apiHandler(c, func() (events.APIGatewayV2HTTPResponse, error) {
		failure := response.InternalServerError(errUpsert, "upsertFailed", "could not save", nil)
		resp := failure.APIGatewayV2()
		return resp, failure
	})

	resp, err := h.Call(lambdaContext(t), events.APIGatewayV2HTTPRequest{})
	require.NoError(t, err)
	assert.Equal(t, 500, resp.StatusCode)
	assert.Contains(t, resp.Body, `"code":"upsertFailed"`)
	assert.Contains(t, logged.String(), "handler returned an error")
}

func TestHandle_APanickingAPIHandlerAnswersWithTheEnvelope(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()

	h := apiHandler(c, func() (events.APIGatewayV2HTTPResponse, error) {
		panic(errBoom)
	})

	resp, err := h.Call(lambdaContext(t), events.APIGatewayV2HTTPRequest{})
	require.NoError(t, err, "the panic stops here, as an envelope")
	assert.Equal(t, 500, resp.StatusCode)
	assert.Contains(t, resp.Body, `"code":"serverError"`)
	assert.Contains(t, logged.String(), "handler panicked")
}

func TestHandle_APayloadThatDoesNotDecodeIsReported(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()

	h := c.Handle(func(context.Context, *Container, events.SQSEvent) (any, error) {
		t.Fatal("the handler must not run on a payload that does not decode")
		return struct{}{}, nil
	})

	_, err := h.Invoke(lambdaContext(t), []byte(`{"Records": 42}`))
	require.Error(t, err)
	assert.Contains(t, logged.String(), `"message":"invocation failed"`)
	assert.Contains(t, logged.String(), `"function_request_id":"req-1"`)
}

func TestHandle_AResultThatDoesNotEncodeIsReported(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()

	h := c.Handle(func(context.Context, *Container, jsontext.Value) (float64, error) {
		return math.NaN(), nil
	})

	_, err := h.Invoke(lambdaContext(t), []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, logged.String(), `"message":"invocation failed"`)
}
