package framework

import (
	"context"
	"net/http"

	"github.com/alessandrolattao/powertools-lambda-go/tracing"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// annotateAPI describes an API request on the invocation's span with the
// OpenTelemetry HTTP attributes, which X-Ray turns into the segment's http
// block: the console's http.status and http.url filters work, and a 5xx is a
// fault. Route and status are indexed as annotations too (http_route,
// http_status), so traces can be searched and grouped by route.
func annotateAPI(ctx context.Context, r *apiCall, status int, clientIP string) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		semconv.HTTPRequestMethodKey.String(r.method),
		semconv.HTTPRoute(r.route),
		semconv.URLPath(r.path),
		semconv.HTTPResponseStatusCode(status),
		semconv.ClientAddress(clientIP),
	)
	tracing.PutAnnotation(ctx, "http_route", r.route)
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
