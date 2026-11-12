package framework

import (
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// apiCall is what the framework reads of an API Gateway request, whatever
// the API type: an HTTP API (payload format 2.0) or a REST API with a Lambda
// proxy integration (payload format 1.0).
type apiCall struct {
	method                string            // GET
	routeKey              string            // GET /orders/{id}
	route                 string            // /orders/{id}
	path                  string            // /orders/42
	pathParameters        map[string]string // what the route matched
	queryStringParameters map[string]string
	headers               map[string]string
	body                  string
}

// describeAPI describes event, and tells whether it is an API Gateway
// request.
func describeAPI(event any) (apiCall, bool) {
	switch r := event.(type) {
	case events.APIGatewayV2HTTPRequest:
		route := r.RouteKey // "$default" has no method in front
		if _, path, found := strings.Cut(r.RouteKey, " "); found {
			route = path
		}
		return apiCall{
			method:                r.RequestContext.HTTP.Method,
			routeKey:              r.RouteKey,
			route:                 route,
			path:                  r.RawPath,
			pathParameters:        r.PathParameters,
			queryStringParameters: r.QueryStringParameters,
			headers:               r.Headers,
			body:                  r.Body,
		}, true
	case events.APIGatewayProxyRequest:
		return apiCall{
			method:                r.HTTPMethod,
			routeKey:              r.HTTPMethod + " " + r.Resource,
			route:                 r.Resource,
			path:                  r.Path,
			pathParameters:        r.PathParameters,
			queryStringParameters: r.QueryStringParameters,
			headers:               r.Headers,
			body:                  r.Body,
		}, true
	default:
		return apiCall{}, false
	}
}

// apiClientIP returns the address of the client that sent event, looking past
// the trusted proxies set with WithTrustedProxies.
func (c *Container) apiClientIP(event any) string {
	if r, ok := event.(events.APIGatewayProxyRequest); ok {
		return c.restClientIP(&r)
	}
	r, _ := event.(events.APIGatewayV2HTTPRequest) //nolint:errcheck // a non-API event leaves the fields empty
	return c.clientIP(&r)
}

// apiResponse returns, when result points to an API Gateway response, its
// status code and the function that turns it into the 500 envelope; nil
// otherwise.
func apiResponse(result any) (status *int, unhandled func()) {
	switch resp := result.(type) {
	case *events.APIGatewayV2HTTPResponse:
		return &resp.StatusCode, func() { *resp = errUnhandled.APIGatewayV2() }
	case *events.APIGatewayProxyResponse:
		return &resp.StatusCode, func() { *resp = errUnhandled.APIGatewayProxy() }
	default:
		return nil, nil
	}
}
