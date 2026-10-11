// Command orders-events is the EventBridge Lambda the end-to-end tests run: it
// marks orders paid when an OrderPaid event arrives, and fails on anything it
// cannot handle, so that EventBridge retries it and sends it to the DLQ.
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"uuid"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/alessandrolattao/lambda-framework-go/framework"
)

var (
	errUnknownEvent  = errors.New("unknown detail-type")
	errOrderNotFound = errors.New("order not found")
)

func main() {
	c := framework.New()
	lambda.Start(c.Consume(handle))
}

func handle(ctx context.Context, c *framework.Container, e events.CloudWatchEvent) error {
	if e.DetailType != "OrderPaid" {
		return fmt.Errorf("%w %q", errUnknownEvent, e.DetailType)
	}

	var detail struct {
		OrderID uuid.UUID `json:"orderId"`
	}
	if err := json.Unmarshal(e.Detail, &detail); err != nil {
		return fmt.Errorf("decode OrderPaid detail (eventId=%s): %w", e.ID, err)
	}

	tag, err := c.DB.Exec(ctx, "UPDATE orders SET status = 'paid' WHERE id = $1", detail.OrderID)
	if err != nil {
		return fmt.Errorf("mark order %s paid: %w", detail.OrderID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark order %s paid: %w", detail.OrderID, errOrderNotFound)
	}
	c.Logger.InfoContext(ctx, "order paid", "orderId", detail.OrderID)
	return nil
}
