package e2e

import (
	"encoding/json/v2"
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

// apiRequest is what API Gateway (HTTP API, payload 2.0) hands the function
// for a call on routeKey ("GET /orders/{id}") at path.
func apiRequest(routeKey, path string, opts ...func(*events.APIGatewayV2HTTPRequest)) events.APIGatewayV2HTTPRequest {
	method, _, _ := strings.Cut(routeKey, " ")
	r := events.APIGatewayV2HTTPRequest{
		Version:  "2.0",
		RouteKey: routeKey,
		RawPath:  path,
		Headers: map[string]string{
			"content-type":    "application/json",
			"x-forwarded-for": callerIP,
		},
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
	for _, opt := range opts {
		opt(&r)
	}
	return r
}

// withID sets the {id} path parameter.
func withID(id string) func(*events.APIGatewayV2HTTPRequest) {
	return func(r *events.APIGatewayV2HTTPRequest) {
		r.PathParameters = map[string]string{"id": id}
	}
}

func withBody(body string) func(*events.APIGatewayV2HTTPRequest) {
	return func(r *events.APIGatewayV2HTTPRequest) { r.Body = body }
}

func withHeader(name, value string) func(*events.APIGatewayV2HTTPRequest) {
	return func(r *events.APIGatewayV2HTTPRequest) { r.Headers[name] = value }
}

// httpResponse decodes what the function answered API Gateway.
func httpResponse(t *testing.T, inv invocation) events.APIGatewayV2HTTPResponse {
	t.Helper()
	failure, failed := inv.failure()
	require.False(t, failed, "API Gateway must get a response, not a Lambda error: %+v", failure)
	var resp events.APIGatewayV2HTTPResponse
	require.NoError(t, json.Unmarshal(inv.body, &resp))
	return resp
}

type errorEnvelope struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details"`
	Timestamp string         `json:"timestamp"`
}

func envelope(t *testing.T, resp events.APIGatewayV2HTTPResponse) errorEnvelope {
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
	fn := startFunction(t, ordersAPI, functionMemoryMB)

	var created orderBody

	t.Run("create an order", func(t *testing.T) {
		inv := fn.invoke(t, apiRequest("POST /orders", "/orders", withBody(`{"totalCents":1250}`)))
		resp := httpResponse(t, inv)

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
		assert.Equal(t, ordersAPI, received.str("function_name"))
		assert.Equal(t, inv.requestID, received.str("function_request_id"))
		assert.Equal(t, callerIP, received.str("client_ip"))

		assert.Equal(t, inv.requestID, inv.recordWith(t, "container initialized").str("function_request_id"))
		completed := inv.recordWith(t, "API request completed")
		assert.InDelta(t, 201, completed["status"], 0)
		assert.Equal(t, "POST /orders", completed.str("routeKey"))
	})

	t.Run("read it back", func(t *testing.T) {
		resp := httpResponse(t, fn.invoke(t, apiRequest("GET /orders/{id}", "/orders/"+created.ID, withID(created.ID))))

		require.Equal(t, 200, resp.StatusCode, resp.Body)
		assert.Equal(t, "no-store", resp.Headers["Cache-Control"])
		var got orderBody
		require.NoError(t, json.Unmarshal([]byte(resp.Body), &got))
		assert.Equal(t, created, got)
	})

	t.Run("list as JSON, cacheable", func(t *testing.T) {
		resp := httpResponse(t, fn.invoke(t, apiRequest("GET /orders", "/orders")))

		require.Equal(t, 200, resp.StatusCode, resp.Body)
		assert.Equal(t, "public, max-age=60", resp.Headers["Cache-Control"])
		assert.Equal(t, "Accept", resp.Headers["Vary"])
		var list []orderBody
		require.NoError(t, json.Unmarshal([]byte(resp.Body), &list))
		assert.Contains(t, list, created)
	})

	t.Run("list as CSV when asked", func(t *testing.T) {
		resp := httpResponse(t, fn.invoke(t, apiRequest("GET /orders", "/orders", withHeader("accept", "text/csv"))))

		require.Equal(t, 200, resp.StatusCode, resp.Body)
		assert.Equal(t, "text/csv", resp.Headers["Content-Type"])
		lines := strings.Split(strings.TrimSpace(resp.Body), "\n")
		assert.Equal(t, "ID,TotalCents,Status,CreatedAt", lines[0])
		assert.Contains(t, resp.Body, created.ID+",1250,new,")
	})

	t.Run("a path id that is not a UUID", func(t *testing.T) {
		resp := httpResponse(t, fn.invoke(t, apiRequest("GET /orders/{id}", "/orders/42", withID("42"))))

		require.Equal(t, 422, resp.StatusCode)
		assert.Equal(t, "invalidParam", envelope(t, resp).Code)
	})

	t.Run("an order that does not exist", func(t *testing.T) {
		missing := uuid.New().String()
		inv := fn.invoke(t, apiRequest("GET /orders/{id}", "/orders/"+missing, withID(missing)))
		resp := httpResponse(t, inv)

		require.Equal(t, 404, resp.StatusCode)
		assert.Equal(t, "orderNotFound", envelope(t, resp).Code)
		completed := inv.recordWith(t, "API request completed")
		assert.InDelta(t, 404, completed["status"], 0, "a 4xx leaves a trace in the logs")
		assert.Equal(t, "GET /orders/{id}", completed.str("routeKey"))
	})

	t.Run("a body that is not JSON", func(t *testing.T) {
		resp := httpResponse(t, fn.invoke(t, apiRequest("POST /orders", "/orders", withBody(`{totalCents:`))))

		require.Equal(t, 422, resp.StatusCode)
		assert.Equal(t, "invalidBody", envelope(t, resp).Code)
	})

	t.Run("a total that is not positive", func(t *testing.T) {
		resp := httpResponse(t, fn.invoke(t, apiRequest("POST /orders", "/orders", withBody(`{"totalCents":0}`))))

		require.Equal(t, 422, resp.StatusCode, "the schema rejects it before the handler runs")
		e := envelope(t, resp)
		assert.Equal(t, "invalidBody", e.Code)
		assert.Equal(t, []any{map[string]any{"loc": []any{"body", "totalCents"}, "type": "greater_than_equal"}}, e.Details["errors"])
	})

	t.Run("an unknown route", func(t *testing.T) {
		resp := httpResponse(t, fn.invoke(t, apiRequest("DELETE /orders", "/orders")))

		require.Equal(t, 404, resp.StatusCode)
		assert.Equal(t, "routeNotFound", envelope(t, resp).Code)
	})

	t.Run("a handler error still answers with its envelope", func(t *testing.T) {
		inv := fn.invoke(t, apiRequest("GET /boom", "/boom"))
		resp := httpResponse(t, inv)

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
		inv := fn.invoke(t, apiRequest("GET /panic", "/panic"))
		resp := httpResponse(t, inv)

		require.Equal(t, 500, resp.StatusCode)
		assert.Equal(t, "serverError", envelope(t, resp).Code)
		panicked := inv.recordWith(t, "handler panicked")
		assert.Contains(t, panicked.str("stack"), "main.main", "the stack says where")

		after := httpResponse(t, fn.invoke(t, apiRequest("GET /orders/{id}", "/orders/"+created.ID, withID(created.ID))))
		assert.Equal(t, 200, after.StatusCode, "the next invocation is served by the same process")
	})

	t.Run("a payload that is not an API Gateway event", func(t *testing.T) {
		inv := fn.invoke(t, []byte(`[1,2,3]`))

		failure, failed := inv.failure()
		require.True(t, failed, "nothing to answer an HTTP client with: the invocation fails")
		assert.Contains(t, failure.Message, "decode events.APIGatewayV2HTTPRequest event")
		assert.Equal(t, "InvalidEvent", failure.Type)
		inv.assertNoPlainTextFailure(t)
		assert.Equal(t, "ERROR", inv.recordWith(t, msgInvocationError).str("level"))
	})

	t.Run("a successful invocation is a green trace", func(t *testing.T) {
		inv := fn.invoke(t, apiRequest("GET /orders/{id}", "/orders/"+created.ID, withID(created.ID)))
		require.Equal(t, 200, httpResponse(t, inv).StatusCode)

		server, spans := suite.xray.trace(t, inv.requestID)
		assert.Equal(t, ordersAPI, server.GetName())
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
