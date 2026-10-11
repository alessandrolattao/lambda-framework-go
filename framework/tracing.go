package framework

import (
	"context"
	"net/http"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/tracing"
	"github.com/aws/aws-lambda-go/events"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// annotateAPI describes an API request on the invocation's span with the
// OpenTelemetry HTTP attributes, which X-Ray turns into the segment's http
// block: the console's http.status and http.url filters work, and a 5xx is a
// fault. Route and status are indexed as annotations too (http_route,
// http_status), so traces can be searched and grouped by route.
func annotateAPI(ctx context.Context, r *events.APIGatewayV2HTTPRequest, status int, clientIP string) {
	route := r.RouteKey // "$default" has no method in front
	if _, path, found := strings.Cut(r.RouteKey, " "); found {
		route = path
	}
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		semconv.HTTPRequestMethodKey.String(r.RequestContext.HTTP.Method),
		semconv.HTTPRoute(route),
		semconv.URLPath(r.RawPath),
		semconv.HTTPResponseStatusCode(status),
		semconv.ClientAddress(clientIP),
	)
	tracing.PutAnnotation(ctx, "http_route", route)
	tracing.PutAnnotation(ctx, "http_status", status)
	if status >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, http.StatusText(status))
	}
}

// recordError marks span as failed by err, when err is not nil.
func recordError(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}
