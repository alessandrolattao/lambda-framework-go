// Command orders-rest is the API Gateway REST API Lambda the end-to-end tests
// run: the orders API of package orders, behind a REST API with a Lambda
// proxy integration (payload format 1.0). It is orders-api but for the line
// that starts it.
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
	lambda.Start(c.REST())
}
