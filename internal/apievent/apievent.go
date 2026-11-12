// Package apievent finds the API Gateway REST API event of the request a
// context belongs to, for the request functions (claims, client IP) of
// packages request and framework. The Event Handler router keeps the event
// it serves where Powertools' own accessor reads it; a streaming handler,
// which runs no router, keeps it here.
package apievent

import (
	"context"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/aws/aws-lambda-go/events"
)

// restKey is the context key of the REST API event a streaming handler
// serves.
type restKey struct{}

// WithREST returns ctx carrying r, the REST API event a handler without a
// router serves.
func WithREST(ctx context.Context, r *events.APIGatewayProxyRequest) context.Context {
	return context.WithValue(ctx, restKey{}, r)
}

// REST returns the REST API event of the request ctx belongs to: the one
// the Event Handler router serves, else the one WithREST put in ctx.
func REST(ctx context.Context) (events.APIGatewayProxyRequest, bool) {
	if r, ok := eventhandler.APIGatewayRESTEvent(ctx); ok {
		return r, true
	}
	if r, ok := ctx.Value(restKey{}).(*events.APIGatewayProxyRequest); ok && r != nil {
		return *r, true
	}
	return events.APIGatewayProxyRequest{}, false
}
