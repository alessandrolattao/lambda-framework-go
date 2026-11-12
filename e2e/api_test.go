package e2e

import (
	"encoding/json/v2"
	"maps"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// callerIP is the address API Gateway saw the caller at.
const callerIP = "203.0.113.7"

// call is a request the orders tests send, whatever the API type it goes
// through.
type call struct {
	headers        map[string]string
	pathParameters map[string]string
	body           string
}

// callOption sets a part of a call.
type callOption func(*call)

// withID sets the {id} path parameter.
func withID(id string) callOption {
	return func(c *call) {
		c.pathParameters = map[string]string{"id": id}
	}
}

func withBody(body string) callOption {
	return func(c *call) { c.body = body }
}

func withHeader(name, value string) callOption {
	return func(c *call) { c.headers[name] = value }
}

func newCall(opts []callOption) call {
	c := call{headers: map[string]string{}}
	for _, opt := range opts {
		opt(&c)
	}
	return c
}

// apiRequest is what API Gateway (HTTP API, payload 2.0) hands the function
// for a call on routeKey ("GET /orders/{id}") at path.
func apiRequest(routeKey, path string, opts ...callOption) events.APIGatewayV2HTTPRequest {
	c := newCall(opts)
	method, _, _ := strings.Cut(routeKey, " ")
	headers := map[string]string{
		"content-type":    "application/json",
		"x-forwarded-for": callerIP,
	}
	maps.Copy(headers, c.headers)
	return events.APIGatewayV2HTTPRequest{
		Version:        "2.0",
		RouteKey:       routeKey,
		RawPath:        path,
		Headers:        headers,
		PathParameters: c.pathParameters,
		Body:           c.body,
		RequestContext: events.APIGatewayV2HTTPRequestContext{
			RouteKey:  routeKey,
			RequestID: uuid.New().String(),
			HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{
				Method:   method,
				Path:     path,
				Protocol: "HTTP/1.1",
				SourceIP: callerIP,
			},
		},
	}
}

// restStage is the stage of the REST API the orders-rest tests call.
const restStage = "e2e"

// restRequest is what API Gateway (REST API with a Lambda proxy integration,
// payload 1.0) hands the function for the same call: the route is the
// resource ("/orders/{id}"), the header names come as the client wrote them,
// and every header is in multiValueHeaders too.
func restRequest(routeKey, path string, opts ...callOption) events.APIGatewayProxyRequest {
	c := newCall(opts)
	method, resource, _ := strings.Cut(routeKey, " ")
	headers := map[string]string{
		"Content-Type":    "application/json",
		"X-Forwarded-For": callerIP,
	}
	maps.Copy(headers, c.headers)
	multi := make(map[string][]string, len(headers))
	for name, value := range headers {
		multi[name] = []string{value}
	}
	return events.APIGatewayProxyRequest{
		Resource:          resource,
		Path:              path,
		HTTPMethod:        method,
		Headers:           headers,
		MultiValueHeaders: multi,
		PathParameters:    c.pathParameters,
		Body:              c.body,
		RequestContext: events.APIGatewayProxyRequestContext{
			AccountID:    "123456789012",
			Stage:        restStage,
			RequestID:    uuid.New().String(),
			Protocol:     "HTTP/1.1",
			Identity:     events.APIGatewayRequestIdentity{SourceIP: callerIP},
			ResourcePath: resource,
			Path:         "/" + restStage + path,
			HTTPMethod:   method,
		},
	}
}

// apiResponse is what the function answered API Gateway, whatever the API
// type, with one value per header.
type apiResponse struct {
	StatusCode int
	Headers    map[string]string
	Body       string
}

// httpResponse decodes what the function answered an HTTP API.
func httpResponse(t *testing.T, inv invocation) apiResponse {
	t.Helper()
	failure, failed := inv.failure()
	require.False(t, failed, "API Gateway must get a response, not a Lambda error: %+v", failure)
	var resp events.APIGatewayV2HTTPResponse
	require.NoError(t, json.Unmarshal(inv.body, &resp))
	return apiResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}
}

// restResponse decodes what the function answered a REST API: the router
// answers in multiValueHeaders, the 500 envelope of a failed handler in
// headers.
func restResponse(t *testing.T, inv invocation) apiResponse {
	t.Helper()
	failure, failed := inv.failure()
	require.False(t, failed, "API Gateway must get a response, not a Lambda error: %+v", failure)
	var resp events.APIGatewayProxyResponse
	require.NoError(t, json.Unmarshal(inv.body, &resp))
	require.False(t, resp.IsBase64Encoded, "the orders API answers text")
	headers := map[string]string{}
	maps.Copy(headers, resp.Headers)
	for name, values := range resp.MultiValueHeaders {
		headers[name] = strings.Join(values, ", ")
	}
	return apiResponse{StatusCode: resp.StatusCode, Headers: headers, Body: resp.Body}
}

// apiType is an API Gateway API type the orders API runs behind: the Lambda
// serving it, and what its events and responses look like.
type apiType struct {
	name      string // of the subtest
	function  string
	eventType string // the event type the function decodes
	request   func(routeKey, path string, opts ...callOption) any
	response  func(*testing.T, invocation) apiResponse
}

// apiTypes are the API types the behaviour tests of the orders API run on:
// the same routes, the same answers.
var apiTypes = []apiType{
	{
		name:      "HTTP API",
		function:  ordersAPI,
		eventType: "events.APIGatewayV2HTTPRequest",
		request: func(routeKey, path string, opts ...callOption) any {
			return apiRequest(routeKey, path, opts...)
		},
		response: httpResponse,
	},
	{
		name:      "REST API",
		function:  ordersREST,
		eventType: "events.APIGatewayProxyRequest",
		request: func(routeKey, path string, opts ...callOption) any {
			return restRequest(routeKey, path, opts...)
		},
		response: restResponse,
	},
}

type errorEnvelope struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details"`
	Timestamp string         `json:"timestamp"`
}

func envelope(t *testing.T, resp apiResponse) errorEnvelope {
	t.Helper()
	var e errorEnvelope
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &e))
	_, err := time.Parse(time.RFC3339, e.Timestamp)
	require.NoError(t, err, "every envelope carries an RFC 3339 timestamp")
	assert.Equal(t, "no-store", resp.Headers["Cache-Control"], "an error is never cached")
	return e
}

type orderBody struct {
	ID         string `json:"id"`
	TotalCents int64  `json:"totalCents"`
	Status     string `json:"status"`
	CreatedAt  string `json:"createdAt"`
}

func TestOrdersAPI(t *testing.T) {
	for _, api := range apiTypes {
		t.Run(api.name, func(t *testing.T) { testOrdersAPI(t, api) })
	}
}

// testOrdersAPI runs the behaviour tests of the orders API behind api.
func testOrdersAPI(t *testing.T, api apiType) {
	t.Helper()
	fn := startFunction(t, api.function, functionMemoryMB)

	var created orderBody

	t.Run("create an order", func(t *testing.T) {
		inv := fn.invoke(t, api.request("POST /orders", "/orders", withBody(`{"totalCents":1250}`)))
		resp := api.response(t, inv)

		require.Equal(t, 201, resp.StatusCode, resp.Body)
		assert.Equal(t, "application/json", resp.Headers["Content-Type"])
		require.NoError(t, json.Unmarshal([]byte(resp.Body), &created))
		assert.Equal(t, int64(1250), created.TotalCents)
		assert.Equal(t, "new", created.Status)
		assert.True(t, strings.HasSuffix(created.CreatedAt, "Z"), "timestamps leave in UTC: %s", created.CreatedAt)

		var total int64
		require.NoError(t, suite.db.QueryRow(t.Context(), "SELECT total_cents FROM orders WHERE id = $1", created.ID).Scan(&total))
		assert.Equal(t, int64(1250), total, "the row is in Postgres")

		received := inv.recordWith(t, "API request received")
		assert.Equal(t, api.function, received.str("function_name"))
		assert.Equal(t, inv.requestID, received.str("function_request_id"))
		assert.Equal(t, callerIP, received.str("client_ip"))

		assert.Equal(t, inv.requestID, inv.recordWith(t, "container initialized").str("function_request_id"))
		completed := inv.recordWith(t, "API request completed")
		assert.InDelta(t, 201, completed["status"], 0)
		assert.Equal(t, "POST /orders", completed.str("routeKey"))
	})

	t.Run("read it back", func(t *testing.T) {
		resp := api.response(t, fn.invoke(t, api.request("GET /orders/{id}", "/orders/"+created.ID, withID(created.ID))))

		require.Equal(t, 200, resp.StatusCode, resp.Body)
		assert.Equal(t, "no-store", resp.Headers["Cache-Control"])
		var got orderBody
		require.NoError(t, json.Unmarshal([]byte(resp.Body), &got))
		assert.Equal(t, created, got)
	})

	t.Run("list as JSON, cacheable", func(t *testing.T) {
		resp := api.response(t, fn.invoke(t, api.request("GET /orders", "/orders")))

		require.Equal(t, 200, resp.StatusCode, resp.Body)
		assert.Equal(t, "public, max-age=60", resp.Headers["Cache-Control"])
		assert.Equal(t, "Accept", resp.Headers["Vary"])
		var list []orderBody
		require.NoError(t, json.Unmarshal([]byte(resp.Body), &list))
		assert.Contains(t, list, created)
	})

	t.Run("list as CSV when asked", func(t *testing.T) {
		resp := api.response(t, fn.invoke(t, api.request("GET /orders", "/orders", withHeader("accept", "text/csv"))))

		require.Equal(t, 200, resp.StatusCode, resp.Body)
		assert.Equal(t, "text/csv", resp.Headers["Content-Type"])
		lines := strings.Split(strings.TrimSpace(resp.Body), "\n")
		assert.Equal(t, "ID,TotalCents,Status,CreatedAt", lines[0])
		assert.Contains(t, resp.Body, created.ID+",1250,new,")
	})

	t.Run("a path id that is not a UUID", func(t *testing.T) {
		resp := api.response(t, fn.invoke(t, api.request("GET /orders/{id}", "/orders/42", withID("42"))))

		require.Equal(t, 422, resp.StatusCode)
		assert.Equal(t, "invalidParam", envelope(t, resp).Code)
	})

	t.Run("an order that does not exist", func(t *testing.T) {
		missing := uuid.New().String()
		inv := fn.invoke(t, api.request("GET /orders/{id}", "/orders/"+missing, withID(missing)))
		resp := api.response(t, inv)

		require.Equal(t, 404, resp.StatusCode)
		assert.Equal(t, "orderNotFound", envelope(t, resp).Code)
		completed := inv.recordWith(t, "API request completed")
		assert.InDelta(t, 404, completed["status"], 0, "a 4xx leaves a trace in the logs")
		assert.Equal(t, "GET /orders/{id}", completed.str("routeKey"))
	})

	t.Run("a body that is not JSON", func(t *testing.T) {
		resp := api.response(t, fn.invoke(t, api.request("POST /orders", "/orders", withBody(`{totalCents:`))))

		require.Equal(t, 422, resp.StatusCode)
		assert.Equal(t, "invalidBody", envelope(t, resp).Code)
	})

	t.Run("a total that is not positive", func(t *testing.T) {
		resp := api.response(t, fn.invoke(t, api.request("POST /orders", "/orders", withBody(`{"totalCents":0}`))))

		require.Equal(t, 422, resp.StatusCode, "the schema rejects it before the handler runs")
		e := envelope(t, resp)
		assert.Equal(t, "invalidBody", e.Code)
		assert.Equal(t, []any{map[string]any{"loc": []any{"body", "totalCents"}, "type": "greater_than_equal"}}, e.Details["errors"])
	})

	t.Run("an unknown route", func(t *testing.T) {
		resp := api.response(t, fn.invoke(t, api.request("DELETE /orders", "/orders")))

		require.Equal(t, 404, resp.StatusCode)
		assert.Equal(t, "routeNotFound", envelope(t, resp).Code)
	})

	t.Run("a handler error still answers with its envelope", func(t *testing.T) {
		inv := fn.invoke(t, api.request("GET /boom", "/boom"))
		resp := api.response(t, inv)

		require.Equal(t, 500, resp.StatusCode)
		e := envelope(t, resp)
		assert.Equal(t, "paymentUnavailable", e.Code, "the client gets the handler's envelope, not API Gateway's")

		failed := inv.recordWith(t, msgHandlerError)
		assert.Equal(t, "ERROR", failed.str("level"))
		assert.Contains(t, failed.str("exception"), "payment provider is down")
		assert.Equal(t, inv.requestID, failed.str("function_request_id"))

		server, spans := suite.xray.trace(t, inv.requestID)
		assert.Equal(t, tracepb.Status_STATUS_CODE_ERROR, server.GetStatus().GetCode(), "X-Ray shows the invocation failed")
		business := spanNamed(t, spans, "## businessLogic")
		assert.Equal(t, tracepb.Status_STATUS_CODE_ERROR, business.GetStatus().GetCode())
		assert.Equal(t, server.GetSpanId(), business.GetParentSpanId())
	})

	t.Run("a panic answers with the envelope and the process lives on", func(t *testing.T) {
		inv := fn.invoke(t, api.request("GET /panic", "/panic"))
		resp := api.response(t, inv)

		require.Equal(t, 500, resp.StatusCode)
		assert.Equal(t, "serverError", envelope(t, resp).Code)
		panicked := inv.recordWith(t, "handler panicked")
		assert.Contains(t, panicked.str("stack"), "main.main", "the stack says where")

		after := api.response(t, fn.invoke(t, api.request("GET /orders/{id}", "/orders/"+created.ID, withID(created.ID))))
		assert.Equal(t, 200, after.StatusCode, "the next invocation is served by the same process")
	})

	t.Run("a payload that is not an API Gateway event", func(t *testing.T) {
		inv := fn.invoke(t, []byte(`[1,2,3]`))

		failure, failed := inv.failure()
		require.True(t, failed, "nothing to answer an HTTP client with: the invocation fails")
		assert.Contains(t, failure.Message, "decode "+api.eventType+" event")
		assert.Equal(t, "InvalidEvent", failure.Type)
		inv.assertNoPlainTextFailure(t)
		assert.Equal(t, "ERROR", inv.recordWith(t, msgInvocationError).str("level"))
	})

	t.Run("a successful invocation is a green trace", func(t *testing.T) {
		inv := fn.invoke(t, api.request("GET /orders/{id}", "/orders/"+created.ID, withID(created.ID)))
		require.Equal(t, 200, api.response(t, inv).StatusCode)

		server, spans := suite.xray.trace(t, inv.requestID)
		assert.Equal(t, api.function, server.GetName())
		assert.NotEqual(t, tracepb.Status_STATUS_CODE_ERROR, server.GetStatus().GetCode())
		business := spanNamed(t, spans, "## businessLogic")
		assert.NotEqual(t, tracepb.Status_STATUS_CODE_ERROR, business.GetStatus().GetCode())

		// What X-Ray turns into the segment's http block and annotations.
		assert.Equal(t, "GET", attribute(server, "http.request.method"))
		assert.Equal(t, "/orders/{id}", attribute(server, "http.route"))
		assert.Equal(t, int64(200), intAttribute(server, "http.response.status_code"))
		assert.Equal(t, callerIP, attribute(server, "client.address"))

		// The query, under the handler's span, with its SQL and no parameter.
		query := spanNamed(t, spans, "SELECT")
		assert.Equal(t, business.GetSpanId(), query.GetParentSpanId())
		assert.Contains(t, attribute(query, "db.query.text"), "FROM orders WHERE id = $1")
		assert.NotContains(t, attribute(query, "db.query.text"), created.ID, "no parameter values in traces")
		for _, s := range spans {
			assert.NotContains(t, attribute(s, "db.query.text"), "statement_timeout", "the timeout set before each query is not traced")
		}
	})
}
