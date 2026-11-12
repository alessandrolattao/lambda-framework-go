package framework

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/aws/aws-lambda-go/events"
	"go.opentelemetry.io/otel/trace"

	"github.com/alessandrolattao/lambda-framework-go/response"
)

// WithRouter passes options to the container's Event Handler router: CORS,
// debug mode, strip prefixes, OpenAPI metadata.
func WithRouter(opts ...eventhandler.Option) Option {
	return func(c *Container) { c.routerOpts = append(c.routerOpts, opts...) }
}

// Router returns the container's Event Handler router, for what the route
// methods below do not cover: plain http.Handler routes, middlewares,
// sub-routers, error handlers, the OpenAPI document. Register routes in main,
// before serving: the router is built on first use, and registering is not
// safe during requests.
func (c *Container) Router() *eventhandler.Router {
	if c.router == nil {
		c.router = c.newRouter()
	}
	return c.router
}

// Get registers a typed operation for GET path on the container's router:
// the Event Handler binds and validates In from the request (path, query,
// header and cookie fields, the body), and op receives the container, with
// its clients, next to it. Its result is the JSON response; a *response.Error
// is the error envelope.
func (c *Container) Get[In, Out any](path string, op func(context.Context, *Container, In) (Out, error), opts ...eventhandler.RouteOption) {
	eventhandler.Get(c.Router(), path, c.bind(op), opts...)
}

// Post registers a typed operation for POST path; see Get.
func (c *Container) Post[In, Out any](path string, op func(context.Context, *Container, In) (Out, error), opts ...eventhandler.RouteOption) {
	eventhandler.Post(c.Router(), path, c.bind(op), opts...)
}

// Put registers a typed operation for PUT path; see Get.
func (c *Container) Put[In, Out any](path string, op func(context.Context, *Container, In) (Out, error), opts ...eventhandler.RouteOption) {
	eventhandler.Put(c.Router(), path, c.bind(op), opts...)
}

// Patch registers a typed operation for PATCH path; see Get.
func (c *Container) Patch[In, Out any](path string, op func(context.Context, *Container, In) (Out, error), opts ...eventhandler.RouteOption) {
	eventhandler.Patch(c.Router(), path, c.bind(op), opts...)
}

// Delete registers a typed operation for DELETE path; see Get.
func (c *Container) Delete[In, Out any](path string, op func(context.Context, *Container, In) (Out, error), opts ...eventhandler.RouteOption) {
	eventhandler.Delete(c.Router(), path, c.bind(op), opts...)
}

// bind hands op the container.
func (c *Container) bind[In, Out any](op func(context.Context, *Container, In) (Out, error)) func(context.Context, In) (Out, error) {
	return func(ctx context.Context, in In) (Out, error) {
		return op(ctx, c, in)
	}
}

// HTTP returns the Lambda handler serving the container's router to an API
// Gateway HTTP API (payload format 2.0):
//
//	c.Get("/orders/{id}", getOrder)
//	lambda.Start(c.HTTP())
//
// The router runs inside Handle, so every request gets what every invocation
// gets (the Powertools decorators, initialization, failure reporting) plus
// what an API request gets: the "API request completed" record, the
// request described on the invocation's span, and the 500 envelope for a
// failure no error handler answered.
func (c *Container) HTTP() Handler[events.APIGatewayV2HTTPRequest, events.APIGatewayV2HTTPResponse] {
	serve := eventhandler.APIGatewayHTTP(c.Router())
	return c.Handle(func(ctx context.Context, _ *Container, r events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		return serve(ctx, r)
	})
}

// REST returns the Lambda handler serving the container's router to an API
// Gateway REST API with a Lambda proxy integration (payload format 1.0): the
// same routes as HTTP, so moving a Lambda from an HTTP API to a REST API
// changes only the line that starts it:
//
//	c.Get("/orders/{id}", getOrder)
//	lambda.Start(c.REST())
//
// A REST API request gets what an HTTP API request gets: the "API request
// received" and "API request completed" records, the request described on
// the invocation's span, client_ip and ClientIP, and the 500 envelope for a
// failure no error handler answered. The claim functions of package request
// read the claims of a Cognito user pool authorizer.
func (c *Container) REST() Handler[events.APIGatewayProxyRequest, events.APIGatewayProxyResponse] {
	serve := eventhandler.APIGatewayREST(c.Router())
	return c.Handle(func(ctx context.Context, _ *Container, r events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		return serve(ctx, r)
	})
}

// newRouter builds the container's router: JSON as the response package
// writes it, and every error the router answers in the error envelope.
func (c *Container) newRouter() *eventhandler.Router {
	opts := append([]eventhandler.Option{eventhandler.WithSerializer(func(v any) ([]byte, error) {
		return json.Marshal(v, json.Deterministic(true))
	})}, c.routerOpts...)
	r := eventhandler.New(opts...)

	eventhandler.OnError(r, func(w http.ResponseWriter, req *http.Request, err *response.Error) error {
		if err.Status >= http.StatusInternalServerError {
			// The one record of the failure, as for an error a handler
			// returns, and the failed business span: the client only gets
			// the envelope.
			c.serverFailure(req.Context(), err)
		}
		err.Write(w)
		return nil
	})
	eventhandler.OnError(r, func(w http.ResponseWriter, _ *http.Request, err *eventhandler.ServiceError) error {
		response.NewError(err.StatusCode, codeFor(err.StatusCode), fmt.Sprint(err.Message), nil).Write(w)
		return nil
	})
	eventhandler.OnError(r, func(w http.ResponseWriter, _ *http.Request, err *eventhandler.RequestValidationError) error {
		response.UnprocessableEntity(validationCode(err.Errors), "the request does not match the operation",
			map[string]any{"errors": err.Errors}).Write(w)
		return nil
	})
	eventhandler.OnError(r, func(w http.ResponseWriter, _ *http.Request, err *eventhandler.UnsupportedContentTypeError) error {
		response.NewError(http.StatusUnsupportedMediaType, "unsupportedContentType",
			"the operation does not read "+err.ContentType, nil).Write(w)
		return nil
	})
	eventhandler.OnError(r, func(w http.ResponseWriter, req *http.Request, err *eventhandler.ResponseValidationError) error {
		c.serverFailure(req.Context(), err)
		errUnhandled.Write(w)
		return nil
	})
	r.HandleNotFound(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		response.NotFound("routeNotFound", "no route "+req.Method+" "+req.URL.Path, nil).Write(w)
	}))
	return r
}

// serverFailure reports a failure an error handler answered with a 5xx: once,
// at Error, and on the span of the business logic the request ran in.
func (c *Container) serverFailure(ctx context.Context, err error) {
	_ = reportError(ctx, c.logger(), err) //nolint:errcheck // reportError returns its argument
	recordError(trace.SpanFromContext(ctx), err)
}

// codeFor names the envelope code of a Powertools ServiceError, after its
// status.
func codeFor(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "badRequest"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "notFound"
	case http.StatusRequestTimeout:
		return "requestTimeout"
	case http.StatusRequestEntityTooLarge:
		return "requestEntityTooLarge"
	case http.StatusServiceUnavailable:
		return "serviceUnavailable"
	default:
		if status >= http.StatusInternalServerError {
			return "serverError"
		}
		return strings.ReplaceAll(strings.ToLower(http.StatusText(status)), " ", "")
	}
}

// validationCode tells a body the client sent wrong ("invalidBody") from
// parameters ("invalidParam"), after where the validation errors are.
func validationCode(details []eventhandler.ValidationErrorDetail) string {
	for _, d := range details {
		if len(d.Loc) > 0 && d.Loc[0] != "body" {
			return "invalidParam"
		}
	}
	return "invalidBody"
}

// ClientIP returns the address of the client that called the API, looking
// past the trusted proxies set with WithTrustedProxies, for the request ctx
// belongs to; "" outside an API Gateway request (HTTP API or REST API).
func (c *Container) ClientIP(ctx context.Context) string {
	if r, ok := eventhandler.APIGatewayHTTPEvent(ctx); ok {
		return c.clientIP(&r)
	}
	if r, ok := eventhandler.APIGatewayRESTEvent(ctx); ok {
		return c.restClientIP(&r)
	}
	return ""
}

// errUnhandled is the envelope of a failure no error handler answered.
var errUnhandled = response.NewError(http.StatusInternalServerError, "serverError", "internal server error", nil)
