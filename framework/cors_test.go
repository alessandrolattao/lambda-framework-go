package framework

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/lambda-framework-go/response"
)

// appOrigin is the origin the CORS tests allow.
const appOrigin = "https://app.example.com"

// corsContainer is a test container that allows appOrigin, with the routes
// the CORS tests call.
func corsContainer(origins ...string) *Container {
	c, _ := testContainer()
	if len(origins) > 0 {
		WithCORSOrigins(origins...)(c)
	}
	c.Get("/items", func(context.Context, *Container, struct{}) (item, error) {
		return item{ID: "a", Quantity: 1}, nil
	})
	c.Put("/items/{id}", func(context.Context, *Container, itemInput) (item, error) {
		return item{}, nil
	})
	c.Get("/unhandled", func(context.Context, *Container, struct{}) (item, error) {
		return item{}, errBoom
	})
	c.Router().HandleFunc("GET /negotiated", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusNoContent)
	})
	c.Router().HandleFunc("GET /origin-listed", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Vary", "Accept, origin")
		w.WriteHeader(http.StatusNoContent)
	})
	return c
}

// withOrigin is req with an Origin header, named as API Gateway hands it over
// to an HTTP API: in lower case.
func withOrigin(req events.APIGatewayV2HTTPRequest, origin string) events.APIGatewayV2HTTPRequest {
	req.Headers["origin"] = origin
	return req
}

// withRESTOrigin is req with an Origin header in both maps, as a REST API
// hands it over: named as the client wrote it.
func withRESTOrigin(req events.APIGatewayProxyRequest, origin string) events.APIGatewayProxyRequest {
	req.Headers["Origin"] = origin
	req.MultiValueHeaders["Origin"] = []string{origin}
	return req
}

// headerNamed returns every value of header name, matched whatever its case,
// across headers and multiValueHeaders, the way API Gateway merges them.
func headerNamed(headers map[string]string, multiValueHeaders map[string][]string, name string) []string {
	var values []string
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			values = append(values, value)
		}
	}
	for key, value := range multiValueHeaders {
		if strings.EqualFold(key, name) {
			values = append(values, value...)
		}
	}
	return values
}

// varyTokens returns the names Vary lists, in order.
func varyTokens(values []string) []string {
	var tokens []string
	for _, value := range values {
		for token := range strings.SplitSeq(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				tokens = append(tokens, token)
			}
		}
	}
	return tokens
}

// assertCORS checks that a response allows origin: one
// Access-Control-Allow-Origin, and Origin once in Vary, after the vary names
// the response already had.
func assertCORS(t *testing.T, headers map[string]string, multiValueHeaders map[string][]string, origin string, vary ...string) {
	t.Helper()
	assert.Equal(t, []string{origin}, headerNamed(headers, multiValueHeaders, "Access-Control-Allow-Origin"))
	assert.Equal(t, append(vary, "Origin"), varyTokens(headerNamed(headers, multiValueHeaders, "Vary")))
}

// assertNoCORS checks that a response carries no CORS header.
func assertNoCORS(t *testing.T, headers map[string]string, multiValueHeaders map[string][]string) {
	t.Helper()
	assert.Empty(t, headerNamed(headers, multiValueHeaders, "Access-Control-Allow-Origin"))
	assert.NotContains(t, varyTokens(headerNamed(headers, multiValueHeaders, "Vary")), "Origin")
}

func TestOptions_CORSOrigins(t *testing.T) {
	t.Setenv("CORS_ALLOW_ORIGINS", " https://app.example.com ,, https://admin.example.com,  ,")
	assert.Equal(t, []string{"https://app.example.com", "https://admin.example.com"}, New().corsOrigins,
		"CORS_ALLOW_ORIGINS by default: spaces trimmed, empty entries ignored")
	assert.Equal(t, []string{"https://other.example.com"},
		New(WithCORSOrigins("https://other.example.com")).corsOrigins, "the option wins")
	assert.Empty(t, New(WithCORSOrigins()).corsOrigins, "an option with no origin turns CORS off")

	t.Setenv("CORS_ALLOW_ORIGINS", "")
	assert.Empty(t, New().corsOrigins, "with neither, CORS is off")
}

func TestHTTP_CORSAllowsAListedOrigin(t *testing.T) {
	t.Parallel()
	h := corsContainer(appOrigin, "https://admin.example.com").HTTP()

	resp, err := h.Call(lambdaContext(t), withOrigin(apiRequest(http.MethodGet, "/items", ""), appOrigin))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, resp.Body)
	assertCORS(t, resp.Headers, resp.MultiValueHeaders, appOrigin)
	assert.JSONEq(t, `{"id":"a","quantity":1}`, resp.Body, "the response is otherwise the route's")

	req := apiRequest(http.MethodGet, "/items", "")
	req.Headers["ORIGIN"] = "https://admin.example.com"
	resp, err = h.Call(lambdaContext(t), req)
	require.NoError(t, err)
	assertCORS(t, resp.Headers, resp.MultiValueHeaders, "https://admin.example.com")
}

func TestHTTP_CORSIgnoresAnOriginNotListed(t *testing.T) {
	t.Parallel()
	h := corsContainer(appOrigin, "*").HTTP()

	// "*" in the list is an origin like any other, not a wildcard.
	for _, origin := range []string{"https://evil.example.com", "https://app.example.com:443", "https://APP.example.com", "null"} {
		resp, err := h.Call(lambdaContext(t), withOrigin(apiRequest(http.MethodGet, "/items", ""), origin))
		require.NoError(t, err, origin)
		require.Equal(t, http.StatusOK, resp.StatusCode, resp.Body)
		assertNoCORS(t, resp.Headers, resp.MultiValueHeaders)
	}
}

func TestHTTP_CORSNeedsAnOrigin(t *testing.T) {
	t.Parallel()
	h := corsContainer(appOrigin).HTTP()

	for _, req := range []events.APIGatewayV2HTTPRequest{
		apiRequest(http.MethodGet, "/items", ""),
		withOrigin(apiRequest(http.MethodGet, "/items", ""), ""),
	} {
		resp, err := h.Call(lambdaContext(t), req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, resp.Body)
		assertNoCORS(t, resp.Headers, resp.MultiValueHeaders)
	}
}

func TestHTTP_CORSKeepsTheVaryAlreadyThere(t *testing.T) {
	t.Parallel()
	h := corsContainer(appOrigin).HTTP()

	resp, err := h.Call(lambdaContext(t), withOrigin(apiRequest(http.MethodGet, "/negotiated", ""), appOrigin))
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, resp.Body)
	assertCORS(t, resp.Headers, resp.MultiValueHeaders, appOrigin, "Accept-Encoding")

	resp, err = h.Call(lambdaContext(t), withOrigin(apiRequest(http.MethodGet, "/origin-listed", ""), appOrigin))
	require.NoError(t, err)
	assert.Equal(t, []string{appOrigin}, headerNamed(resp.Headers, resp.MultiValueHeaders, "Access-Control-Allow-Origin"))
	assert.Equal(t, []string{"Accept", "origin"}, varyTokens(headerNamed(resp.Headers, resp.MultiValueHeaders, "Vary")),
		"Origin is in Vary once")
}

func TestHTTP_CORSReachesTheErrorResponses(t *testing.T) {
	t.Parallel()
	h := corsContainer(appOrigin).HTTP()

	cases := []struct {
		name   string
		req    events.APIGatewayV2HTTPRequest
		status int
		code   string
	}{
		{"a validation error", apiRequest(http.MethodPut, "/items/42", `{"quantity":2}`), 422, "invalidParam"},
		{"an unknown route", apiRequest(http.MethodDelete, "/items", ""), 404, "routeNotFound"},
		{"an error nobody handles", apiRequest(http.MethodGet, "/unhandled", ""), 500, "serverError"},
	}
	for _, tc := range cases {
		resp, err := h.Call(lambdaContext(t), withOrigin(tc.req, appOrigin))
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.status, resp.StatusCode, "%s: %s", tc.name, resp.Body)
		assert.Equal(t, tc.code, decodeEnvelope(t, resp).Code, tc.name)
		assertCORS(t, resp.Headers, resp.MultiValueHeaders, appOrigin)
	}
}

func TestHTTP_CORSReplacesTheRoutersOwnAllowOrigin(t *testing.T) {
	t.Parallel()
	c, _ := testContainer()
	WithCORSOrigins(appOrigin)(c)
	WithRouter(eventhandler.WithCORS(eventhandler.CORSConfig{AllowOrigin: "*"}))(c)
	c.Get("/items", func(context.Context, *Container, struct{}) (item, error) {
		return item{}, nil
	})

	resp, err := c.HTTP().Call(lambdaContext(t), withOrigin(apiRequest(http.MethodGet, "/items", ""), appOrigin))
	require.NoError(t, err)
	assert.Equal(t, []string{appOrigin}, headerNamed(resp.Headers, resp.MultiValueHeaders, "Access-Control-Allow-Origin"),
		"one Access-Control-Allow-Origin, the framework's")
}

func TestHTTP_WithoutCORSTheResponseIsUnchanged(t *testing.T) {
	t.Parallel()
	h := corsContainer().HTTP()

	for _, path := range []string{"/items", "/negotiated"} {
		plain, err := h.Call(lambdaContext(t), apiRequest(http.MethodGet, path, ""))
		require.NoError(t, err)
		withOrigin, err := h.Call(lambdaContext(t), withOrigin(apiRequest(http.MethodGet, path, ""), appOrigin))
		require.NoError(t, err)
		assert.Equal(t, plain, withOrigin, path)
		assertNoCORS(t, withOrigin.Headers, withOrigin.MultiValueHeaders)
	}
}

func TestREST_CORSAllowsAListedOrigin(t *testing.T) {
	t.Parallel()
	h := corsContainer(appOrigin).REST()

	resp, err := h.Call(lambdaContext(t), withRESTOrigin(restRequest(http.MethodGet, "/items", ""), appOrigin))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, resp.Body)
	assertCORS(t, resp.Headers, resp.MultiValueHeaders, appOrigin)
	assert.JSONEq(t, `{"id":"a","quantity":1}`, resp.Body)

	resp, err = h.Call(lambdaContext(t), withRESTOrigin(restRequest(http.MethodGet, "/items", ""), "https://evil.example.com"))
	require.NoError(t, err)
	assertNoCORS(t, resp.Headers, resp.MultiValueHeaders)

	resp, err = h.Call(lambdaContext(t), restRequest(http.MethodGet, "/items", ""))
	require.NoError(t, err)
	assertNoCORS(t, resp.Headers, resp.MultiValueHeaders)
}

func TestREST_CORSReadsTheOriginFromEitherHeaderMap(t *testing.T) {
	t.Parallel()
	h := corsContainer(appOrigin).REST()

	multi := restRequest(http.MethodGet, "/items", "")
	multi.MultiValueHeaders["origin"] = []string{appOrigin}
	resp, err := h.Call(lambdaContext(t), multi)
	require.NoError(t, err)
	assertCORS(t, resp.Headers, resp.MultiValueHeaders, appOrigin)

	single := restRequest(http.MethodGet, "/items", "")
	single.MultiValueHeaders = nil
	single.Headers["ORIGIN"] = appOrigin
	resp, err = h.Call(lambdaContext(t), single)
	require.NoError(t, err)
	assertCORS(t, resp.Headers, resp.MultiValueHeaders, appOrigin)

	repeated := restRequest(http.MethodGet, "/items", "")
	repeated.MultiValueHeaders["Origin"] = []string{appOrigin, "https://evil.example.com"}
	resp, err = h.Call(lambdaContext(t), repeated)
	require.NoError(t, err)
	assertNoCORS(t, resp.Headers, resp.MultiValueHeaders)
}

func TestREST_CORSKeepsTheVaryAlreadyThere(t *testing.T) {
	t.Parallel()
	h := corsContainer(appOrigin).REST()

	resp, err := h.Call(lambdaContext(t), withRESTOrigin(restRequest(http.MethodGet, "/negotiated", ""), appOrigin))
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, resp.Body)
	assertCORS(t, resp.Headers, resp.MultiValueHeaders, appOrigin, "Accept-Encoding")
}

func TestREST_CORSReachesTheErrorResponses(t *testing.T) {
	t.Parallel()
	h := corsContainer(appOrigin).REST()

	cases := []struct {
		name   string
		req    events.APIGatewayProxyRequest
		status int
		code   string
	}{
		{"a validation error", restRequest(http.MethodPut, "/items/42", `{"quantity":2}`), 422, "invalidParam"},
		{"an unknown route", restRequest(http.MethodDelete, "/items", ""), 404, "routeNotFound"},
		{"an error nobody handles", restRequest(http.MethodGet, "/unhandled", ""), 500, "serverError"},
	}
	for _, tc := range cases {
		resp, err := h.Call(lambdaContext(t), withRESTOrigin(tc.req, appOrigin))
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.status, resp.StatusCode, "%s: %s", tc.name, resp.Body)
		assert.Equal(t, tc.code, decodeRESTEnvelope(t, resp).Code, tc.name)
		assertCORS(t, resp.Headers, resp.MultiValueHeaders, appOrigin)
	}
}

func TestREST_CORSReplacesTheRoutersOwnAllowOrigin(t *testing.T) {
	t.Parallel()
	c, _ := testContainer()
	WithCORSOrigins(appOrigin)(c)
	WithRouter(eventhandler.WithCORS(eventhandler.CORSConfig{AllowOrigin: "*"}))(c)
	c.Get("/items", func(context.Context, *Container, struct{}) (item, error) {
		return item{}, nil
	})

	resp, err := c.REST().Call(lambdaContext(t), withRESTOrigin(restRequest(http.MethodGet, "/items", ""), appOrigin))
	require.NoError(t, err)
	assert.Equal(t, []string{appOrigin}, headerNamed(resp.Headers, resp.MultiValueHeaders, "Access-Control-Allow-Origin"),
		"one Access-Control-Allow-Origin, the framework's, though the router wrote its own in multiValueHeaders")
}

func TestREST_WithoutCORSTheResponseIsUnchanged(t *testing.T) {
	t.Parallel()
	h := corsContainer().REST()

	for _, path := range []string{"/items", "/negotiated"} {
		plain, err := h.Call(lambdaContext(t), restRequest(http.MethodGet, path, ""))
		require.NoError(t, err)
		withOrigin, err := h.Call(lambdaContext(t), withRESTOrigin(restRequest(http.MethodGet, path, ""), appOrigin))
		require.NoError(t, err)
		assert.Equal(t, plain, withOrigin, path)
		assertNoCORS(t, withOrigin.Headers, withOrigin.MultiValueHeaders)
	}
}

func TestStream_CORSAllowsAListedOrigin(t *testing.T) {
	t.Parallel()
	c := corsContainer(appOrigin)
	op := func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Add("Vary", "Accept-Encoding")
		return response.SSE(w, "price", "42")
	}

	metadata, body := readStream(t, c, op, withRESTOrigin(streamRequest(), appOrigin))
	m := decodeMetadata(t, metadata)
	assert.Equal(t, http.StatusOK, m.StatusCode)
	assertCORS(t, m.Headers, m.MultiValueHeaders, appOrigin, "Accept-Encoding")
	assert.Equal(t, "text/event-stream", m.Headers["Content-Type"])
	assert.Equal(t, "event: price\ndata: 42\n\n", body)

	metadata, _ = readStream(t, c, op, withRESTOrigin(streamRequest(), "https://evil.example.com"))
	m = decodeMetadata(t, metadata)
	assertNoCORS(t, m.Headers, m.MultiValueHeaders)

	metadata, _ = readStream(t, c, op, streamRequest())
	m = decodeMetadata(t, metadata)
	assertNoCORS(t, m.Headers, m.MultiValueHeaders)
}

func TestStream_CORSReplacesTheAllowOriginOpSet(t *testing.T) {
	t.Parallel()
	c := corsContainer(appOrigin)
	op := func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		return nil
	}

	metadata, _ := readStream(t, c, op, withRESTOrigin(streamRequest(), appOrigin))
	m := decodeMetadata(t, metadata)
	assertCORS(t, m.Headers, m.MultiValueHeaders, appOrigin)
}

func TestStream_CORSReachesAFailureBeforeTheFirstByte(t *testing.T) {
	t.Parallel()
	c := corsContainer(appOrigin)

	cases := []struct {
		name   string
		op     streamOp
		status int
	}{
		{"a response error", func(context.Context, *Container, events.APIGatewayProxyRequest, http.ResponseWriter) error {
			return response.NotFound("orderNotFound", "no such order", nil)
		}, http.StatusNotFound},
		{"a plain error", func(context.Context, *Container, events.APIGatewayProxyRequest, http.ResponseWriter) error {
			return errBoom
		}, http.StatusInternalServerError},
		{"a panic", func(context.Context, *Container, events.APIGatewayProxyRequest, http.ResponseWriter) error {
			panic(errBoom)
		}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		metadata, _ := readStream(t, c, tc.op, withRESTOrigin(streamRequest(), appOrigin))
		m := decodeMetadata(t, metadata)
		assert.Equal(t, tc.status, m.StatusCode, tc.name)
		assertCORS(t, m.Headers, m.MultiValueHeaders, appOrigin)
	}
}

func TestStream_WithoutCORSTheMetadataIsUnchanged(t *testing.T) {
	t.Parallel()
	c := corsContainer()
	op := func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Add("Vary", "Accept-Encoding")
		return response.SSE(w, "price", "42")
	}

	plain, _ := readStream(t, c, op, streamRequest())
	withOrigin, _ := readStream(t, c, op, withRESTOrigin(streamRequest(), appOrigin))
	assert.Equal(t, plain, withOrigin)
	m := decodeMetadata(t, withOrigin)
	assertNoCORS(t, m.Headers, m.MultiValueHeaders)
}

func TestHandle_CORSReachesAHandWrittenAPIHandler(t *testing.T) {
	t.Parallel()
	c, _ := testContainer()
	WithCORSOrigins(appOrigin)(c)

	httpAPI := c.Handle(func(context.Context, *Container, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		return events.APIGatewayV2HTTPResponse{StatusCode: http.StatusOK, Body: "ok"}, nil
	})
	resp, err := httpAPI.Call(lambdaContext(t), withOrigin(apiRequest(http.MethodGet, "/", ""), appOrigin))
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Body)
	assertCORS(t, resp.Headers, resp.MultiValueHeaders, appOrigin)

	restAPI := c.Handle(func(context.Context, *Container, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		return events.APIGatewayProxyResponse{StatusCode: http.StatusOK, MultiValueHeaders: map[string][]string{"Vary": {"Accept"}}}, nil
	})
	restResp, err := restAPI.Call(lambdaContext(t), withRESTOrigin(restRequest(http.MethodGet, "/", ""), appOrigin))
	require.NoError(t, err)
	assertCORS(t, restResp.Headers, restResp.MultiValueHeaders, appOrigin, "Accept")
}

func TestHandle_CORSReachesTheEnvelopeOfAFailedAPIHandler(t *testing.T) {
	t.Parallel()
	c, _ := testContainer()
	WithCORSOrigins(appOrigin)(c)

	for name, fail := range map[string]func() (events.APIGatewayV2HTTPResponse, error){
		"an error": func() (events.APIGatewayV2HTTPResponse, error) { return events.APIGatewayV2HTTPResponse{}, errUpsert },
		"a panic":  func() (events.APIGatewayV2HTTPResponse, error) { panic(errBoom) },
	} {
		resp, err := apiHandler(c, fail).Call(lambdaContext(t), withOrigin(apiRequest(http.MethodGet, "/", ""), appOrigin))
		require.NoError(t, err, name)
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, name)
		assert.Equal(t, "serverError", decodeEnvelope(t, resp).Code, name)
		assertCORS(t, resp.Headers, resp.MultiValueHeaders, appOrigin)
	}
}

func TestHandle_WithoutCORSAHandWrittenAPIResponseIsUnchanged(t *testing.T) {
	t.Parallel()
	c, _ := testContainer()
	want := events.APIGatewayProxyResponse{StatusCode: http.StatusOK, Body: "ok"}
	h := c.Handle(func(context.Context, *Container, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		return want, nil
	})

	resp, err := h.Call(lambdaContext(t), withRESTOrigin(restRequest(http.MethodGet, "/", ""), appOrigin))
	require.NoError(t, err)
	assert.Equal(t, want, resp, "not even an empty headers map")
}
