package framework

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda/messages"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validationError stands in for an error type an application defines.
type validationError struct{ field string }

func (e *validationError) Error() string { return "invalid " + e.field }

func TestErrorType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"a sentinel says nothing: the place it failed", fmt.Errorf("charge: %w", errBoom), errorTypeHandler},
		{"a joined error says nothing either", errors.Join(errBoom, errNoRows), errorTypeHandler},
		{"an application's own type", fmt.Errorf("charge: %w", &validationError{field: "total"}), "validationError"},
		{"a library's type, however deep", fmt.Errorf("a: %w", fmt.Errorf("b: %w", &pgconn.PgError{Code: "23505"})), "PgError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, errorType(tc.err, errorTypeHandler))
		})
	}
}

func TestRuntimeLog(t *testing.T) {
	t.Parallel()

	logger, logged := bufferLogger()
	w := runtimeLog{logger: logger}

	_, err := w.Write([]byte(`{"errorMessage":"boom","errorType":"HandlerError"}` + "\n"))
	require.NoError(t, err)
	assert.Contains(t, logged.String(), `"level":"DEBUG","msg":"runtime reported the failure","err":"boom","errorType":"HandlerError"`,
		"the framework already wrote it down at Error: this copy is for debugging only")

	logged.Reset()
	_, err = w.Write([]byte("expected AWS Lambda environment variables are not defined\n"))
	require.NoError(t, err)
	assert.Contains(t, logged.String(), `"level":"INFO","msg":"aws-lambda-go","detail":"expected AWS Lambda environment variables are not defined"`)
}

func TestHandle_TheRuntimeGetsANamedFailure(t *testing.T) {
	t.Parallel()

	c := &Container{Logger: quietLogger()}
	failing := c.Handle(func(context.Context, *Container, jsontext.Value) (any, error) {
		return nil, fmt.Errorf("charge: %w", &validationError{field: "total"})
	})
	decoding := c.Consume(func(context.Context, *Container, events.SQSEvent) error { return nil })

	for _, tc := range []struct {
		name     string
		invoke   func() error
		wantType string
	}{
		{"a handler error", func() error { _, err := failing.Invoke(lambdaContext(t), []byte(`{}`)); return err }, "validationError"},
		{"an event that does not decode", func() error { _, err := decoding.Invoke(lambdaContext(t), []byte(`[]`)); return err }, errorTypeInvalidEvent},
	} {
		err := tc.invoke()
		failure, ok := errors.AsType[messages.InvokeResponse_Error](err)
		require.True(t, ok, "%s: aws-lambda-go passes this type through as is", tc.name)
		assert.Equal(t, tc.wantType, failure.Type, tc.name)
		assert.NotEmpty(t, failure.Message, tc.name)
	}
}

func TestHandle_AnInitFailureIsAnInitError(t *testing.T) {
	t.Setenv("AWS_REGION", testRegion)
	t.Setenv("AWS_PROFILE", "profile-that-does-not-exist")
	c := New()
	c.Logger = quietLogger()

	_, err := c.Consume(func(context.Context, *Container, jsontext.Value) error { return nil }).
		Invoke(lambdaContext(t), []byte(`{}`))

	failure, ok := errors.AsType[messages.InvokeResponse_Error](err)
	require.True(t, ok)
	assert.Contains(t, failure.Message, "initialize container")
	assert.Equal(t, errorTypeInit, failure.Type)
}

func TestHandle_EveryAPIRequestLogsHowItEnded(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	h := apiHandler(c, func() (events.APIGatewayV2HTTPResponse, error) {
		return events.APIGatewayV2HTTPResponse{StatusCode: 404}, nil
	})

	_, err := h.Call(lambdaContext(t), events.APIGatewayV2HTTPRequest{RouteKey: "GET /orders/{id}"})
	require.NoError(t, err)
	assert.Contains(t, logged.String(), `"message":"API request completed"`)
	assert.Contains(t, logged.String(), `"function_request_id":"req-1"`)
	assert.Contains(t, logged.String(), `"status":404,"routeKey":"GET /orders/{id}","durationMs":`, "a 4xx leaves a trace too")
}

func TestInitialize_LogsWithTheInvocationRequestID(t *testing.T) {
	t.Setenv("AWS_REGION", testRegion)
	var logged bytes.Buffer
	c := New(WithLogging(logging.WithWriter(&logged)))

	h := c.Consume(func(context.Context, *Container, jsontext.Value) error { return nil })
	require.NoError(t, h.Call(lambdaContext(t), jsontext.Value(`{}`)))
	assert.Contains(t, logged.String(), `"message":"container initialized"`)
	assert.Contains(t, logged.String(), `"function_request_id":"req-1"`)
}
