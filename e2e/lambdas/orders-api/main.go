// Command orders-api is the API Gateway HTTP API Lambda the end-to-end tests
// run: a small orders API on Postgres, with a route for every path a handler
// can take.
package main

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/jackc/pgx/v5"

	"github.com/alessandrolattao/lambda-framework-go/framework"
	"github.com/alessandrolattao/lambda-framework-go/response"
)

// errBoom is the failure the /boom route reports.
var errBoom = errors.New("boom: the payment provider is down")

type order struct {
	ID         uuid.UUID `json:"id"`
	TotalCents int64     `json:"totalCents"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
}

func main() {
	c := framework.New()
	c.Get("/orders/{id}", getOrder)
	c.Get("/orders", listOrders)
	c.Post("/orders", createOrder, eventhandler.StatusCode(201))
	c.Get("/boom", boom)
	c.Get("/panic", func(context.Context, *framework.Container, struct{}) (order, error) {
		panic("the handler lost its mind")
	})
	lambda.Start(c.HTTP())
}

// orderID is the input of the routes that address one order.
type orderID struct {
	ID uuid.UUID `path:"id" format:"uuid"`
}

func getOrder(ctx context.Context, c *framework.Container, in orderID) (eventhandler.Response[order], error) {
	var o order
	err := c.DB.QueryRow(ctx, "SELECT id, total_cents, status, created_at FROM orders WHERE id = $1", in.ID).
		Scan(&o.ID, &o.TotalCents, &o.Status, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return eventhandler.Response[order]{}, response.NotFound("orderNotFound", "no order "+in.ID.String(), nil)
	}
	if err != nil {
		return eventhandler.Response[order]{}, response.InternalServerError(
			fmt.Errorf("read order %s: %w", in.ID, err), "serverError", "could not read the order", nil)
	}
	return response.OK(o, response.WithNoCache()), nil
}

// listInput is the input of GET /orders: the Accept header picks JSON or CSV.
type listInput struct {
	Accept string `header:"Accept"`
}

func listOrders(ctx context.Context, c *framework.Container, in listInput) (eventhandler.Response[[]byte], error) {
	rows, err := c.DB.Query(ctx, "SELECT id, total_cents, status, created_at FROM orders ORDER BY created_at, id")
	if err != nil {
		return eventhandler.Response[[]byte]{}, response.InternalServerError(
			fmt.Errorf("list orders: %w", err), "serverError", "could not list the orders", nil)
	}
	orders, err := pgx.CollectRows(rows, pgx.RowToStructByPos[order])
	if err != nil {
		return eventhandler.Response[[]byte]{}, response.InternalServerError(
			fmt.Errorf("scan orders: %w", err), "serverError", "could not list the orders", nil)
	}
	return response.Negotiate(in.Accept, orders, response.WithCacheMaxAge(time.Minute))
}

// newOrder is the input of POST /orders; the router rejects a total that is
// not positive with a 422, before the handler runs.
type newOrder struct {
	Body struct {
		TotalCents int64 `json:"totalCents" minimum:"1" required:"true"`
	}
}

func createOrder(ctx context.Context, c *framework.Container, in newOrder) (order, error) {
	var o order
	err := c.DB.QueryRow(ctx,
		"INSERT INTO orders (total_cents) VALUES ($1) RETURNING id, total_cents, status, created_at", in.Body.TotalCents).
		Scan(&o.ID, &o.TotalCents, &o.Status, &o.CreatedAt)
	if err != nil {
		return order{}, response.InternalServerError(fmt.Errorf("insert order: %w", err), "serverError", "could not create the order", nil)
	}
	return o, nil
}

func boom(context.Context, *framework.Container, struct{}) (order, error) {
	return order{}, response.InternalServerError(errBoom, "paymentUnavailable", "the payment provider did not answer", nil)
}
