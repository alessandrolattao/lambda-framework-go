package framework

import (
	"cmp"
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// The headers of the CORS rule.
const (
	headerOrigin      = "Origin"
	headerAllowOrigin = "Access-Control-Allow-Origin"
	headerVary        = "Vary"
)

// WithCORSOrigins sets the origins a browser may call the API from, as the
// browser sends them in the Origin header (https://app.example.com: scheme,
// host and a port other than the default). Without it New reads
// CORS_ALLOW_ORIGINS, the origins separated by commas; with neither, CORS is
// off and no response changes.
//
// A request whose Origin is one of them gets Access-Control-Allow-Origin
// with that origin, in place of any the response already had, and Origin in
// Vary; any other request gets no CORS header. The rule covers every
// response HTTP, REST and Stream answer, the error envelopes included, and
// the response of any Handle handler that answers with an API Gateway
// response (events.APIGatewayV2HTTPResponse, events.APIGatewayProxyResponse).
// The preflight OPTIONS request and the responses API Gateway writes itself
// belong to the API's own configuration. Use it or the router's CORS
// (WithRouter(eventhandler.WithCORS(...))), not both.
func WithCORSOrigins(origins ...string) Option {
	return func(c *Container) { c.corsOrigins = slices.Clone(origins) }
}

// parseOrigins reads the origins of CORS_ALLOW_ORIGINS: separated by commas,
// spaces trimmed, empty entries ignored.
func parseOrigins(list string) []string {
	var origins []string
	for origin := range strings.SplitSeq(list, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			origins = append(origins, origin)
		}
	}
	return origins
}

// corsOrigin returns the origin the response to event allows: the Origin of
// the request when it is one of the container's origins, "" otherwise (CORS
// off, a request without an Origin or with one not listed).
func (c *Container) corsOrigin(event any) string {
	origin := requestOrigin(event)
	if origin == "" || !slices.Contains(c.corsOrigins, origin) {
		return ""
	}
	return origin
}

// requestOrigin returns the Origin header of an API Gateway request, its
// name matched whatever its case; "" for a request without one, for a REST
// API request that repeats it, and for any other event. A REST API request
// is read from multiValueHeaders, then from headers. Names are walked in
// order, so the answer never depends on map order.
func requestOrigin(event any) string {
	switch r := event.(type) {
	case events.APIGatewayV2HTTPRequest:
		return headerValue(r.Headers, headerOrigin)
	case events.APIGatewayProxyRequest:
		var values []string
		for _, name := range slices.Sorted(maps.Keys(r.MultiValueHeaders)) {
			if strings.EqualFold(name, headerOrigin) {
				values = append(values, r.MultiValueHeaders[name]...)
			}
		}
		switch len(values) {
		case 0:
			return headerValue(r.Headers, headerOrigin)
		case 1:
			return values[0]
		default:
			return ""
		}
	default:
		return ""
	}
}

// headerValue returns header name of headers, matched whatever its case.
func headerValue(headers map[string]string, name string) string {
	if key, ok := findName(maps.Keys(headers), name); ok {
		return headers[key]
	}
	return ""
}

// findName returns the first of names, in order, that is name whatever its
// case, and whether there is one.
func findName(names iter.Seq[string], name string) (string, bool) {
	for _, key := range slices.Sorted(names) {
		if strings.EqualFold(key, name) {
			return key, true
		}
	}
	return "", false
}

// allowResponseOrigin adds the CORS headers that allow origin to result,
// when it points to an API Gateway response; see allowOrigin.
func allowResponseOrigin(result any, origin string) {
	switch resp := result.(type) {
	case *events.APIGatewayV2HTTPResponse:
		resp.Headers = allowOrigin(origin, resp.Headers, resp.MultiValueHeaders)
	case *events.APIGatewayProxyResponse:
		resp.Headers = allowOrigin(origin, resp.Headers, resp.MultiValueHeaders)
	}
}

// allowOrigin adds the CORS headers that allow origin to the headers of a
// response, which API Gateway reads from two maps: Access-Control-Allow-Origin
// in headers, in place of any the response had in either map, and Origin in
// Vary once, after the names Vary already lists. It returns headers, created
// when the response had none; an empty origin changes nothing.
func allowOrigin(origin string, headers map[string]string, multiValueHeaders map[string][]string) map[string]string {
	if origin == "" {
		return headers
	}
	for name := range headers {
		if strings.EqualFold(name, headerAllowOrigin) {
			delete(headers, name)
		}
	}
	for name := range multiValueHeaders {
		if strings.EqualFold(name, headerAllowOrigin) {
			delete(multiValueHeaders, name)
		}
	}
	if headers == nil {
		headers = map[string]string{}
	}
	headers[headerAllowOrigin] = origin
	varyOnOrigin(headers, multiValueHeaders)
	return headers
}

// varyOnOrigin adds Origin to the Vary header of a response, unless it lists
// it already: to the values in multiValueHeaders when Vary is there, else to
// the value in headers, a new Vary when there is none.
func varyOnOrigin(headers map[string]string, multiValueHeaders map[string][]string) {
	var listed []string
	for name, values := range multiValueHeaders {
		if strings.EqualFold(name, headerVary) {
			listed = append(listed, values...)
		}
	}
	for name, value := range headers {
		if strings.EqualFold(name, headerVary) {
			listed = append(listed, value)
		}
	}
	if slices.ContainsFunc(listed, listsOrigin) {
		return
	}

	if name, ok := findName(maps.Keys(multiValueHeaders), headerVary); ok {
		multiValueHeaders[name] = append(slices.Clip(multiValueHeaders[name]), headerOrigin)
		return
	}
	name, ok := findName(maps.Keys(headers), headerVary)
	if !ok || strings.TrimSpace(headers[name]) == "" {
		headers[cmp.Or(name, headerVary)] = headerOrigin
		return
	}
	headers[name] += ", " + headerOrigin
}

// listsOrigin reports whether vary, a value of the Vary header, lists Origin.
func listsOrigin(vary string) bool {
	for name := range strings.SplitSeq(vary, ",") {
		if strings.EqualFold(strings.TrimSpace(name), headerOrigin) {
			return true
		}
	}
	return false
}
