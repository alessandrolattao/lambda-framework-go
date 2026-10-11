package framework

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/lambda-framework-go/response"
)

// apiRequest is an API Gateway HTTP API request for method and path.
func apiRequest(method, path, body string) events.APIGatewayV2HTTPRequest {
	return events.APIGatewayV2HTTPRequest{
		RouteKey: method + " " + path,
		RawPath:  path,
		Body:     body,
		Headers:  map[string]string{"content-type": "application/json"},
		RequestContext: events.APIGatewayV2HTTPRequestContext{
			HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{Method: method, SourceIP: "203.0.113.7"},
		},
	}
}

// decodeEnvelope reads the error envelope of resp.
func decodeEnvelope(t *testing.T, resp events.APIGatewayV2HTTPResponse) response.ErrorBody {
	t.Helper()
	var body response.ErrorBody
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &body), resp.Body)
	assert.Equal(t, "no-store", resp.Headers["Cache-Control"], "an error is never cached")
	return body
}

type itemInput struct {
	ID   uuid.UUID `path:"id" format:"uuid"`
	Body struct {
		Quantity int `json:"quantity" minimum:"1"`
	}
}

type item struct {
	ID       string `json:"id"`
	Quantity int    `json:"quantity"`
}

func TestHTTP_ServesTypedOperationsWithTheContainer(t *testing.T) {
	t.Parallel()
	c, _ := testContainer()
	var got *Container
	c.Put("/items/{id}", func(_ context.Context, c *Container, in itemInput) (item, error) {
		got = c
		return item{ID: in.ID.String(), Quantity: in.Body.Quantity}, nil
	})

	id := uuid.New().String()
	resp, err := c.HTTP().Call(lambdaContext(t), apiRequest(http.MethodPut, "/items/"+id, `{"quantity":2}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, resp.Body)
	assert.JSONEq(t, `{"id":"`+id+`","quantity":2}`, resp.Body)
	assert.Same(t, c, got, "the operation receives the container")
}

func TestHTTP_ErrorsAnswerWithTheEnvelope(t *testing.T) {
	t.Parallel()
	c, logged := testContainer()
	c.Put("/items/{id}", func(context.Context, *Container, itemInput) (item, error) {
		return item{}, nil
	})
	c.Get("/missing", func(context.Context, *Container, struct{}) (item, error) {
		return item{}, response.NotFound("itemNotFound", "no such item", map[string]string{"id": "x"})
	})
	c.Get("/forbidden", func(context.Context, *Container, struct{}) (item, error) {
		return item{}, eventhandler.Forbidden("not yours")
	})
	c.Get("/broken", func(context.Context, *Container, struct{}) (item, error) {
		return item{}, response.InternalServerError(errUpsert, "upsertFailed", "could not save", nil)
	})
	c.Get("/unhandled", func(context.Context, *Container, struct{}) (item, error) {
		return item{}, errBoom
	})
	c.Post("/upload", func(context.Context, *Container, struct{ Body item }) (item, error) {
		return item{}, nil
	})
	h := c.HTTP()

	cases := []struct {
		name   string
		req    events.APIGatewayV2HTTPRequest
		status int
		code   string
	}{
		{"a path parameter that is not a UUID", apiRequest(http.MethodPut, "/items/42", `{"quantity":2}`), 422, "invalidParam"},
		{"a body that breaks a constraint", apiRequest(http.MethodPut, "/items/"+uuid.New().String(), `{"quantity":0}`), 422, "invalidBody"},
		{"a body that is not JSON", apiRequest(http.MethodPut, "/items/"+uuid.New().String(), `{quantity:`), 422, "invalidBody"},
		{"a response error", apiRequest(http.MethodGet, "/missing", ""), 404, "itemNotFound"},
		{"a Powertools service error", apiRequest(http.MethodGet, "/forbidden", ""), 403, "forbidden"},
		{"a server error", apiRequest(http.MethodGet, "/broken", ""), 500, "upsertFailed"},
		{"an error nobody handles", apiRequest(http.MethodGet, "/unhandled", ""), 500, "serverError"},
		{"an unknown route", apiRequest(http.MethodDelete, "/items", ""), 404, "routeNotFound"},
	}
	for _, tc := range cases {
		resp, err := h.Call(lambdaContext(t), tc.req)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.status, resp.StatusCode, "%s: %s", tc.name, resp.Body)
		assert.Equal(t, tc.code, decodeEnvelope(t, resp).Code, tc.name)
	}

	unsupported := apiRequest(http.MethodPost, "/upload", "a=1")
	unsupported.Headers["content-type"] = "text/plain"
	resp, err := h.Call(lambdaContext(t), unsupported)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
	assert.Equal(t, "unsupportedContentType", decodeEnvelope(t, resp).Code)

	assert.Equal(t, 1, strings.Count(logged.String(), errUpsert.Error()), "a 5xx is reported once, with its cause")
	assert.Equal(t, 1, strings.Count(logged.String(), errBoom.Error()), "and so is an error nobody handles")
}

func TestHTTP_TheValidationErrorsReachTheClient(t *testing.T) {
	t.Parallel()
	c, _ := testContainer()
	c.Put("/items/{id}", func(context.Context, *Container, itemInput) (item, error) { return item{}, nil })

	resp, err := c.HTTP().Call(lambdaContext(t), apiRequest(http.MethodPut, "/items/"+uuid.New().String(), `{"quantity":0}`))
	require.NoError(t, err)
	body := decodeEnvelope(t, resp)
	details, ok := body.Details.(map[string]any)
	require.True(t, ok, "%#v", body.Details)
	assert.NotEmpty(t, details["errors"])
}

func TestHTTP_ClientIP(t *testing.T) {
	t.Parallel()
	c, _ := testContainer()
	var ip string
	c.Get("/whoami", func(ctx context.Context, c *Container, _ struct{}) (item, error) {
		ip = c.ClientIP(ctx)
		return item{}, nil
	})

	_, err := c.HTTP().Call(lambdaContext(t), apiRequest(http.MethodGet, "/whoami", ""))
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.7", ip)
	assert.Empty(t, c.ClientIP(t.Context()), "no request, no address")
}

func TestHTTP_RoutesOfAHandBuiltContainer(t *testing.T) {
	t.Parallel()
	c := &Container{}
	c.Get("/ping", func(context.Context, *Container, struct{}) (string, error) { return "pong", nil })

	resp, err := c.HTTP().Call(lambdaContext(t), apiRequest(http.MethodGet, "/ping", ""))
	require.NoError(t, err)
	assert.Equal(t, `"pong"`, resp.Body)
}
