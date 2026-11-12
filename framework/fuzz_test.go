package framework

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda/messages"
)

// FuzzInvoke feeds an API handler any payload: it must never panic, and it
// either answers an HTTP response or fails as InvalidEvent.
func FuzzInvoke(f *testing.F) {
	f.Add([]byte(`{"version":"2.0","routeKey":"GET /orders/{id}","rawPath":"/orders/42"}`))
	f.Add([]byte(`[1,2,3]`))
	f.Add([]byte(`"just a string"`))
	f.Add([]byte(`{"headers":{"a":"b","a":"c"}}`))
	f.Add([]byte{0xff, 0xfe})

	c := &Container{Logger: quietLogger()}
	h := c.Handle(func(context.Context, *Container, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		return events.APIGatewayV2HTTPResponse{StatusCode: http.StatusOK}, nil
	})

	f.Fuzz(func(t *testing.T, payload []byte) {
		out, err := h.Invoke(context.Background(), payload)
		if err != nil {
			failure, ok := errors.AsType[messages.InvokeResponse_Error](err)
			if !ok || failure.Type != errorTypeInvalidEvent {
				t.Fatalf("a payload that does not decode must fail as InvalidEvent, got %#v", err)
			}
			return
		}
		var resp events.APIGatewayV2HTTPResponse
		if json.Unmarshal(out, &resp) != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("decoded payload %q answered %q", payload, out)
		}
	})
}

// FuzzInvokeREST is FuzzInvoke for a REST API handler.
func FuzzInvokeREST(f *testing.F) {
	f.Add([]byte(`{"resource":"/orders/{id}","path":"/orders/42","httpMethod":"GET"}`))
	f.Add([]byte(`{"requestContext":{"authorizer":{"claims":{"sub":"x","exp":1700000000}}}}`))
	f.Add([]byte(`[1,2,3]`))
	f.Add([]byte(`"just a string"`))
	f.Add([]byte(`{"headers":{"a":"b","a":"c"}}`))
	f.Add([]byte{0xff, 0xfe})

	c := &Container{Logger: quietLogger()}
	h := c.Handle(func(context.Context, *Container, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		return events.APIGatewayProxyResponse{StatusCode: http.StatusOK}, nil
	})

	f.Fuzz(func(t *testing.T, payload []byte) {
		out, err := h.Invoke(context.Background(), payload)
		if err != nil {
			failure, ok := errors.AsType[messages.InvokeResponse_Error](err)
			if !ok || failure.Type != errorTypeInvalidEvent {
				t.Fatalf("a payload that does not decode must fail as InvalidEvent, got %#v", err)
			}
			return
		}
		var resp events.APIGatewayProxyResponse
		if json.Unmarshal(out, &resp) != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("decoded payload %q answered %q", payload, out)
		}
	})
}
