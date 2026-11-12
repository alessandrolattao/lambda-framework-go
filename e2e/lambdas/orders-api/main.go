// Command orders-api is the API Gateway HTTP API Lambda the end-to-end tests
// run: the orders API of package orders, behind an HTTP API (payload format
// 2.0).
package main

import (
	"context"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/alessandrolattao/lambda-framework-go/e2e/lambdas/orders"
	"github.com/alessandrolattao/lambda-framework-go/framework"
)

func main() {
	c := framework.New()
	orders.Register(c)
	c.Get("/panic", func(context.Context, *framework.Container, struct{}) (struct{}, error) {
		panic("the handler lost its mind")
	})
	lambda.Start(c.HTTP())
}
