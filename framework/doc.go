// Package framework runs AWS Lambda handlers with their dependencies wired in,
// on Powertools for AWS Lambda (Go): its logger, tracer and metrics, its
// Event Handler router, the AWS SDK clients, a database pool (Postgres, or
// Aurora DSQL through the AWS connector), and one place where every failure
// is reported.
//
// A Lambda builds one Container at start-up, in main, and registers its
// business handlers. An API Gateway HTTP API registers typed operations,
// whose input the router binds and validates from the request:
//
//	func main() {
//		c := framework.New()
//		c.Get("/orders/{id}", getOrder)
//		lambda.Start(c.HTTP())
//	}
//
//	type orderID struct {
//		ID uuid.UUID `path:"id" format:"uuid"`
//	}
//
//	func getOrder(ctx context.Context, c *framework.Container, in orderID) (Order, error) {
//		c.Logger.InfoContext(ctx, "reading order", "orderId", in.ID)
//		...
//	}
//
// The same routes serve an API Gateway REST API with a Lambda proxy
// integration: lambda.Start(c.REST()) instead of lambda.Start(c.HTTP()).
// A REST API endpoint that streams its response, server-sent events for one,
// is a Lambda of its own, whose handler writes through an http.ResponseWriter:
//
//	lambda.Start(c.Stream(watchOrder))
//
// Any other event source hands its handler to Handle (for handlers that answer
// with a result) or Consume (for handlers that only succeed or fail):
//
//	lambda.Start(c.Consume(handleOrderPlaced))
//
// Every invocation runs through the Powertools decorators (the logger's
// InjectContext, the tracer's CaptureLambdaHandler, the metrics' LogMetrics
// when metrics are configured). The AWS clients and the database pool are
// created on the first invocation and reused by every warm one. What belongs
// to one invocation (its request id, the client's address, its metric set)
// lives in the context: log with the Context methods (InfoContext,
// ErrorContext) so that records carry it, also when the execution environment
// runs several invocations at once (Lambda Managed Instances).
//
// A handler reports a failure by returning it, and writes nothing itself: the
// framework logs it once, at Error, and that record is what error tracking
// reads. An HTTP API or a REST API answers every failure with the error
// envelope of package response, the router's own (404, validation) included,
// instead of API Gateway's generic 500; so does a stream that fails before
// its first byte. With WithCORSOrigins (or CORS_ALLOW_ORIGINS) the API
// responses, error envelopes included, allow the listed origins.
package framework
