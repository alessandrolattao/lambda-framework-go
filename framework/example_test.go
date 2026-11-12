package framework_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"uuid"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/lambda-framework-go/framework"
	"github.com/alessandrolattao/lambda-framework-go/response"
)

// getOrderInput is what GET /orders/{id} reads from the request; the router
// binds and validates it.
type getOrderInput struct {
	ID uuid.UUID `path:"id" format:"uuid"`
}

type order struct {
	ID string `json:"id"`
}

// getOrder is a business handler: its input is bound, its clients come with
// the container.
func getOrder(ctx context.Context, c *framework.Container, in getOrderInput) (order, error) {
	c.Logger.InfoContext(ctx, "reading order", "orderId", in.ID)
	if in.ID == uuid.Nil() {
		return order{}, response.NotFound("orderNotFound", "no order with this id", nil)
	}
	return order{ID: in.ID.String()}, nil
}

// handleOrderPlaced is a consumer: it only succeeds or fails.
func handleOrderPlaced(ctx context.Context, c *framework.Container, e events.EventBridgeEvent) error {
	c.Logger.InfoContext(ctx, "order placed", "detail", string(e.Detail))
	return nil
}

func ExampleContainer_HTTP() {
	c := framework.New(framework.WithLogging(logging.WithWriter(io.Discard)))
	c.Get("/orders/{id}", getOrder)
	h := c.HTTP() // in main: lambda.Start(h)

	for _, path := range []string{"/orders/0b6f5c1e-4b8e-4f2a-9c3d-2a1b0c9d8e7f", "/orders/42", "/invoices"} {
		resp, err := h.Call(context.Background(), events.APIGatewayV2HTTPRequest{
			RawPath:        path,
			RequestContext: events.APIGatewayV2HTTPRequestContext{HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{Method: "GET"}},
		})
		fmt.Println(resp.StatusCode, err)
	}
	// Output:
	// 200 <nil>
	// 422 <nil>
	// 404 <nil>
}

func ExampleContainer_REST() {
	c := framework.New(framework.WithLogging(logging.WithWriter(io.Discard)))
	c.Get("/orders/{id}", getOrder) // the same routes as behind an HTTP API
	h := c.REST()                   // in main: lambda.Start(h)

	for _, path := range []string{"/orders/0b6f5c1e-4b8e-4f2a-9c3d-2a1b0c9d8e7f", "/orders/42", "/invoices"} {
		resp, err := h.Call(context.Background(), events.APIGatewayProxyRequest{HTTPMethod: "GET", Path: path})
		fmt.Println(resp.StatusCode, err)
	}
	// Output:
	// 200 <nil>
	// 422 <nil>
	// 404 <nil>
}

// watchPrices is a streaming handler: each price reaches the client as a
// server-sent event as soon as it is written.
func watchPrices(_ context.Context, _ *framework.Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, price := range []string{`{"amount":41}`, `{"amount":42}`} {
		if err := response.SSE(w, "price", price); err != nil {
			return err
		}
	}
	return nil
}

func ExampleContainer_Stream() {
	c := framework.New(framework.WithLogging(logging.WithWriter(io.Discard)))
	h := c.Stream(watchPrices) // in main: lambda.Start(h)

	r, err := h(context.Background(), events.APIGatewayProxyRequest{HTTPMethod: "GET", Resource: "/prices", Path: "/prices"})
	if err != nil {
		fmt.Println(err)
		return
	}
	stream, err := io.ReadAll(r)
	if err != nil {
		fmt.Println(err)
		return
	}
	// What Lambda hands API Gateway: the metadata, 8 null bytes, the body.
	metadata, body, _ := strings.Cut(string(stream), strings.Repeat("\x00", 8))
	fmt.Println(metadata)
	fmt.Print(body)
	// Output:
	// {"statusCode":200,"headers":{"Content-Type":"text/event-stream"}}
	// event: price
	// data: {"amount":41}
	//
	// event: price
	// data: {"amount":42}
}

func ExampleContainer_Consume() {
	c := framework.New(framework.WithLogging(logging.WithWriter(io.Discard)))
	h := c.Consume(handleOrderPlaced) // in main: lambda.Start(h)

	err := h.Call(context.Background(), events.EventBridgeEvent{DetailType: "OrderPlaced", Detail: []byte(`{}`)})
	fmt.Println(err)
	// Output: <nil>
}
