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
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/alessandrolattao/lambda-framework-go/response"
)

// The REST API twins of the HTTP API tests: the same routes, failures, logs
// and spans, on the events of a REST API with a Lambda proxy integration
// (payload format 1.0).

// restRequest is an API Gateway REST API request for method and path, with
// the header names as clients write them.
func restRequest(method, path, body string) events.APIGatewayProxyRequest {
	return events.APIGatewayProxyRequest{
		Resource:          path,
		Path:              path,
		HTTPMethod:        method,
		Body:              body,
		Headers:           map[string]string{"Content-Type": "application/json"},
		MultiValueHeaders: map[string][]string{"Content-Type": {"application/json"}},
		RequestContext: events.APIGatewayProxyRequestContext{
			HTTPMethod:   method,
			ResourcePath: path,
			Path:         path,
			Identity:     events.APIGatewayRequestIdentity{SourceIP: "203.0.113.7"},
		},
	}
}

// restHeader returns header name of resp: the router answers in
// multiValueHeaders, the 500 envelope of a failed handler in headers.
func restHeader(resp events.APIGatewayProxyResponse, name string) string {
	if values, ok := resp.MultiValueHeaders[name]; ok {
		return strings.Join(values, ", ")
	}
	return resp.Headers[name]
}

// decodeRESTEnvelope reads the error envelope of resp.
func decodeRESTEnvelope(t *testing.T, resp events.APIGatewayProxyResponse) response.ErrorBody {
	t.Helper()
	var body response.ErrorBody
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &body), resp.Body)
	assert.Equal(t, "no-store", restHeader(resp, "Cache-Control"), "an error is never cached")
	return body
}

func TestREST_ServesTypedOperationsWithTheContainer(t *testing.T) {
	t.Parallel()
	c, _ := testContainer()
	var got *Container
	c.Put("/items/{id}", func(_ context.Context, c *Container, in itemInput) (item, error) {
		got = c
		return item{ID: in.ID.String(), Quantity: in.Body.Quantity}, nil
	})

	id := uuid.New().String()
	resp, err := c.REST().Call(lambdaContext(t), restRequest(http.MethodPut, "/items/"+id, `{"quantity":2}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, resp.Body)
	assert.JSONEq(t, `{"id":"`+id+`","quantity":2}`, resp.Body)
	assert.Same(t, c, got, "the operation receives the container")
}

// registerFailingRoutes registers a route for every way an operation can
// fail, for the error tests of both API types.
func registerFailingRoutes(c *Container) {
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
}

func TestREST_ErrorsAnswerWithTheEnvelope(t *testing.T) {
	t.Parallel()
	c, logged := testContainer()
	registerFailingRoutes(c)
	h := c.REST()

	cases := []struct {
		name   string
		req    events.APIGatewayProxyRequest
		status int
		code   string
	}{
		{"a path parameter that is not a UUID", restRequest(http.MethodPut, "/items/42", `{"quantity":2}`), 422, "invalidParam"},
		{"a body that breaks a constraint", restRequest(http.MethodPut, "/items/"+uuid.New().String(), `{"quantity":0}`), 422, "invalidBody"},
		{"a body that is not JSON", restRequest(http.MethodPut, "/items/"+uuid.New().String(), `{quantity:`), 422, "invalidBody"},
		{"a response error", restRequest(http.MethodGet, "/missing", ""), 404, "itemNotFound"},
		{"a Powertools service error", restRequest(http.MethodGet, "/forbidden", ""), 403, "forbidden"},
		{"a server error", restRequest(http.MethodGet, "/broken", ""), 500, "upsertFailed"},
		{"an error nobody handles", restRequest(http.MethodGet, "/unhandled", ""), 500, "serverError"},
		{"an unknown route", restRequest(http.MethodDelete, "/items", ""), 404, "routeNotFound"},
	}
	for _, tc := range cases {
		resp, err := h.Call(lambdaContext(t), tc.req)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.status, resp.StatusCode, "%s: %s", tc.name, resp.Body)
		assert.Equal(t, tc.code, decodeRESTEnvelope(t, resp).Code, tc.name)
	}

	unsupported := restRequest(http.MethodPost, "/upload", "a=1")
	unsupported.Headers = map[string]string{"Content-Type": "text/plain"}
	unsupported.MultiValueHeaders = map[string][]string{"Content-Type": {"text/plain"}}
	resp, err := h.Call(lambdaContext(t), unsupported)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
	assert.Equal(t, "unsupportedContentType", decodeRESTEnvelope(t, resp).Code)

	assert.Equal(t, 1, strings.Count(logged.String(), errUpsert.Error()), "a 5xx is reported once, with its cause")
	assert.Equal(t, 1, strings.Count(logged.String(), errBoom.Error()), "and so is an error nobody handles")
}

func TestREST_AnswersAsTheHTTPAPIDoes(t *testing.T) {
	t.Parallel()
	c := &Container{Logger: quietLogger()}
	registerFailingRoutes(c)
	c.Get("/items/{id}", func(_ context.Context, _ *Container, in itemInput) (item, error) {
		return item{ID: in.ID.String(), Quantity: 1}, nil
	})
	httpAPI, restAPI := c.HTTP(), c.REST()

	id := uuid.New().String()
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/items/" + id, ""},
		{http.MethodPut, "/items/42", `{"quantity":2}`},
		{http.MethodPut, "/items/" + id, `{"quantity":0}`},
		{http.MethodGet, "/missing", ""},
		{http.MethodGet, "/forbidden", ""},
		{http.MethodGet, "/broken", ""},
		{http.MethodGet, "/unhandled", ""},
		{http.MethodDelete, "/items", ""},
	} {
		got, err := restAPI.Call(lambdaContext(t), restRequest(tc.method, tc.path, tc.body))
		require.NoError(t, err)
		want, err := httpAPI.Call(lambdaContext(t), apiRequest(tc.method, tc.path, tc.body))
		require.NoError(t, err)

		route := tc.method + " " + tc.path
		assert.Equal(t, want.StatusCode, got.StatusCode, route)
		assert.Equal(t, want.Headers["Content-Type"], restHeader(got, "Content-Type"), route)
		assert.Equal(t, want.Headers["Cache-Control"], restHeader(got, "Cache-Control"), route)
		if want.StatusCode < http.StatusBadRequest {
			assert.JSONEq(t, want.Body, got.Body, route)
			continue
		}
		wantBody, gotBody := decodeEnvelope(t, want), decodeRESTEnvelope(t, got)
		assert.Equal(t, wantBody.Code, gotBody.Code, route)
		assert.Equal(t, wantBody.Message, gotBody.Message, route)
		assert.Equal(t, wantBody.Details, gotBody.Details, route)
	}
}

func TestREST_TheValidationErrorsReachTheClient(t *testing.T) {
	t.Parallel()
	c, _ := testContainer()
	c.Put("/items/{id}", func(context.Context, *Container, itemInput) (item, error) { return item{}, nil })

	resp, err := c.REST().Call(lambdaContext(t), restRequest(http.MethodPut, "/items/"+uuid.New().String(), `{"quantity":0}`))
	require.NoError(t, err)
	body := decodeRESTEnvelope(t, resp)
	details, ok := body.Details.(map[string]any)
	require.True(t, ok, "%#v", body.Details)
	assert.NotEmpty(t, details["errors"])
}

func TestREST_ClientIP(t *testing.T) {
	t.Parallel()
	c, _ := testContainer()
	var ip string
	c.Get("/whoami", func(ctx context.Context, c *Container, _ struct{}) (item, error) {
		ip = c.ClientIP(ctx)
		return item{}, nil
	})

	_, err := c.REST().Call(lambdaContext(t), restRequest(http.MethodGet, "/whoami", ""))
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.7", ip)
}

func TestREST_ClientIPBehindTrustedProxies(t *testing.T) {
	t.Parallel()
	c := &Container{Logger: quietLogger(), trustedProxies: 1}
	var ip string
	c.Get("/whoami", func(ctx context.Context, c *Container, _ struct{}) (item, error) {
		ip = c.ClientIP(ctx)
		return item{}, nil
	})

	req := restRequest(http.MethodGet, "/whoami", "")
	req.RequestContext.Identity.SourceIP = "198.51.100.9" // the CloudFront edge
	req.Headers["X-Forwarded-For"] = "1.2.3.4, 203.0.113.7"
	req.MultiValueHeaders["X-Forwarded-For"] = []string{"1.2.3.4, 203.0.113.7"}
	_, err := c.REST().Call(lambdaContext(t), req)
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.7", ip, "the address the edge saw, not what the client wrote")
}

func TestREST_RoutesOfAHandBuiltContainer(t *testing.T) {
	t.Parallel()
	c := &Container{}
	c.Get("/ping", func(context.Context, *Container, struct{}) (string, error) { return "pong", nil })

	resp, err := c.REST().Call(lambdaContext(t), restRequest(http.MethodGet, "/ping", ""))
	require.NoError(t, err)
	assert.Equal(t, `"pong"`, resp.Body)
}

// restHandler is an API Gateway REST API handler that fails the way fail
// says.
func restHandler(c *Container, fail func() (events.APIGatewayProxyResponse, error)) Handler[events.APIGatewayProxyRequest, events.APIGatewayProxyResponse] {
	return c.Handle(func(context.Context, *Container, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		return fail()
	})
}

func TestHandle_ReturnsTheRESTResult(t *testing.T) {
	t.Parallel()

	h := restHandler(&Container{Logger: quietLogger()}, func() (events.APIGatewayProxyResponse, error) {
		return events.APIGatewayProxyResponse{StatusCode: 204}, nil
	})

	resp, err := h.Call(lambdaContext(t), events.APIGatewayProxyRequest{})
	require.NoError(t, err)
	assert.Equal(t, 204, resp.StatusCode)
}

func TestHandle_RESTRequestLogsCarryTheClientIP(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	h := restHandler(c, func() (events.APIGatewayProxyResponse, error) {
		return events.APIGatewayProxyResponse{}, nil
	})

	_, err := h.Call(lambdaContext(t), events.APIGatewayProxyRequest{
		Headers: map[string]string{"X-Forwarded-For": "1.2.3.4"},
		RequestContext: events.APIGatewayProxyRequestContext{
			Identity: events.APIGatewayRequestIdentity{SourceIP: "203.0.113.7"},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, logged.String(), `"client_ip":"203.0.113.7"`, "the peer, not what the client wrote")
}

func TestHandle_EveryRESTRequestLogsHowItArrivedAndEnded(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	h := restHandler(c, func() (events.APIGatewayProxyResponse, error) {
		return events.APIGatewayProxyResponse{StatusCode: 404}, nil
	})

	_, err := h.Call(lambdaContext(t), events.APIGatewayProxyRequest{
		HTTPMethod: http.MethodGet,
		Resource:   "/orders/{id}",
		Path:       "/orders/42",
	})
	require.NoError(t, err)
	assert.Contains(t, logged.String(), `"message":"API request received"`)
	assert.Contains(t, logged.String(), `"method":"GET","path":"/orders/42","routeKey":"GET /orders/{id}"`)
	assert.Contains(t, logged.String(), `"message":"API request completed"`)
	assert.Contains(t, logged.String(), `"function_request_id":"req-1"`)
	assert.Contains(t, logged.String(), `"status":404,"routeKey":"GET /orders/{id}","durationMs":`, "a 4xx leaves a trace too")
}

func TestHandle_AFailedRESTHandlerAnswersWithTheEnvelope(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	h := restHandler(c, func() (events.APIGatewayProxyResponse, error) {
		return events.APIGatewayProxyResponse{}, errUpsert
	})

	resp, err := h.Call(lambdaContext(t), events.APIGatewayProxyRequest{})
	require.NoError(t, err, "API Gateway must get a response, not a Lambda error")
	assert.Equal(t, 500, resp.StatusCode)
	assert.Contains(t, resp.Body, `"code":"serverError"`)
	assert.Equal(t, "no-store", resp.Headers["Cache-Control"])
	assert.Contains(t, logged.String(), "upsert intervention: connection refused", "and the error is still reported")
}

func TestHandle_AFailedRESTHandlerKeepsTheResponseItBuilt(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	h := restHandler(c, func() (events.APIGatewayProxyResponse, error) {
		failure := response.InternalServerError(errUpsert, "upsertFailed", "could not save", nil)
		resp := failure.APIGatewayProxy()
		return resp, failure
	})

	resp, err := h.Call(lambdaContext(t), events.APIGatewayProxyRequest{})
	require.NoError(t, err)
	assert.Equal(t, 500, resp.StatusCode)
	assert.Contains(t, resp.Body, `"code":"upsertFailed"`)
	assert.Contains(t, logged.String(), "handler returned an error")
}

func TestHandle_APanickingRESTHandlerAnswersWithTheEnvelope(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	h := restHandler(c, func() (events.APIGatewayProxyResponse, error) {
		panic(errBoom)
	})

	resp, err := h.Call(lambdaContext(t), events.APIGatewayProxyRequest{})
	require.NoError(t, err, "the panic stops here, as an envelope")
	assert.Equal(t, 500, resp.StatusCode)
	assert.Contains(t, resp.Body, `"code":"serverError"`)
	assert.Contains(t, logged.String(), "handler panicked")
}

func TestLogEvent_RedactsTheCredentialsOfARESTRequest(t *testing.T) {
	t.Parallel()

	logger, logged := bufferLogger()

	logEvent(t.Context(), logger, events.APIGatewayProxyRequest{
		Headers: map[string]string{
			"Authorization": "Bearer eyJhbGciOi.secret",
			"Cookie":        "session=secret",
			"Accept":        "application/json",
		},
	})

	assert.NotContains(t, logged.String(), "secret")
	assert.Contains(t, logged.String(), "[redacted]")
	assert.Contains(t, logged.String(), "application/json", "the other headers stay")
}

// The tracing twins change the global OpenTelemetry state, as the tracing
// tests do, so none of them runs in parallel.

func TestHandle_AFailedRESTHandlerMarksTheInvocationSpan(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "orders-get")
	c, recorder := tracedContainer(t)

	h := lambda.NewHandler(restHandler(c, func() (events.APIGatewayProxyResponse, error) {
		return events.APIGatewayProxyResponse{}, errUpsert
	}))
	_, err := h.Invoke(lambdaContext(t), []byte(`{}`))
	require.NoError(t, err, "the client gets the 500 envelope")

	assert.Equal(t, codes.Error, spanNamed(t, recorder, "orders-get").Status().Code,
		"X-Ray still shows the invocation failed")
	assert.Equal(t, codes.Error, spanNamed(t, recorder, businessSpan).Status().Code)
}

func TestHandle_TheInvocationSpanDescribesTheRESTRequest(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "orders-get")
	c, recorder := tracedContainer(t)

	for _, status := range []int{404, 503} {
		recorder.Reset()
		h := lambda.NewHandler(restHandler(c, func() (events.APIGatewayProxyResponse, error) {
			return events.APIGatewayProxyResponse{StatusCode: status}, nil
		}))
		request := events.APIGatewayProxyRequest{
			Resource:   "/orders/{id}",
			Path:       "/orders/42",
			HTTPMethod: "GET",
			RequestContext: events.APIGatewayProxyRequestContext{
				Identity: events.APIGatewayRequestIdentity{SourceIP: "203.0.113.7"},
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
