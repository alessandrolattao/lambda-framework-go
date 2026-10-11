package e2e

import (
	"encoding/json/jsontext"
	"testing"
	"time"
	"uuid"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// The messages of the framework's two error records.
const (
	msgHandlerError    = "handler returned an error"
	msgInvocationError = "invocation failed"
)

// eventBridgeEvent is what an EventBridge rule hands the function.
func eventBridgeEvent(detailType, detail string) events.CloudWatchEvent {
	return events.CloudWatchEvent{
		Version:    "0",
		ID:         uuid.New().String(),
		DetailType: detailType,
		Source:     "e2e.orders",
		AccountID:  "123456789012",
		Time:       time.Now().UTC(),
		Region:     "eu-west-1",
		Resources:  []string{},
		Detail:     jsontext.Value(detail),
	}
}

// newOrder puts an order straight into Postgres and returns its id.
func newOrder(t *testing.T) string {
	t.Helper()
	var id string
	require.NoError(t, suite.db.QueryRow(t.Context(),
		"INSERT INTO orders (total_cents) VALUES (999) RETURNING id::text").Scan(&id))
	return id
}

func orderStatus(t *testing.T, id string) string {
	t.Helper()
	var status string
	require.NoError(t, suite.db.QueryRow(t.Context(), "SELECT status FROM orders WHERE id = $1", id).Scan(&status))
	return status
}

func TestOrdersEvents(t *testing.T) {
	fn := startFunction(t, ordersEvents, functionMemoryMB)

	t.Run("OrderPaid marks the order paid", func(t *testing.T) {
		id := newOrder(t)
		inv := fn.invoke(t, eventBridgeEvent("OrderPaid", `{"orderId":"`+id+`"}`))

		_, failed := inv.failure()
		require.False(t, failed, string(inv.body))
		assert.Equal(t, "null", string(inv.body), "a consumer answers null")
		assert.Equal(t, "paid", orderStatus(t, id))
		assert.Equal(t, id, inv.recordWith(t, "order paid").str("orderId"))
		assert.Equal(t, inv.requestID, inv.recordWith(t, "container initialized").str("function_request_id"),
			"the cold start is tied to the invocation that paid for it")

		server, _ := suite.xray.trace(t, inv.requestID)
		assert.NotEqual(t, tracepb.Status_STATUS_CODE_ERROR, server.GetStatus().GetCode())
	})

	// Every failure below must reach Lambda as an error, so that EventBridge
	// retries the event and finally sends it to the DLQ, and must be written
	// down once at Error.
	failures := []struct {
		name      string
		payload   any
		want      string
		errorType string // what Lambda, and Step Functions, call the failure
		logged    string
	}{
		{
			name:      "an order that does not exist",
			payload:   eventBridgeEvent("OrderPaid", `{"orderId":"`+uuid.New().String()+`"}`),
			want:      "order not found",
			errorType: "HandlerError",
			logged:    msgHandlerError,
		},
		{
			name:      "a detail that is not an OrderPaid",
			payload:   eventBridgeEvent("OrderPaid", `{"orderId":"not-a-uuid"}`),
			want:      "decode OrderPaid detail",
			errorType: "SemanticError",
			logged:    msgHandlerError,
		},
		{
			name:      "a detail-type nobody handles",
			payload:   eventBridgeEvent("OrderShipped", `{}`),
			want:      `unknown detail-type "OrderShipped"`,
			errorType: "HandlerError",
			logged:    msgHandlerError,
		},
		{
			name:      "a payload that is not an EventBridge event",
			payload:   []byte(`"just a string"`),
			want:      "decode events.CloudWatchEvent event",
			errorType: "InvalidEvent",
			logged:    msgInvocationError,
		},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			inv := fn.invoke(t, tc.payload)

			failure, failed := inv.failure()
			require.True(t, failed, "the invocation must fail: %s", inv.body)
			assert.Contains(t, failure.Message, tc.want)
			assert.Equal(t, tc.errorType, failure.Type)
			inv.assertNoPlainTextFailure(t)

			rec := inv.recordWith(t, tc.logged)
			assert.Equal(t, "ERROR", rec.str("level"))
			assert.Equal(t, inv.requestID, rec.str("function_request_id"))
			assert.Contains(t, rec.str("exception"), tc.want)

			server, _ := suite.xray.trace(t, inv.requestID)
			assert.Equal(t, tracepb.Status_STATUS_CODE_ERROR, server.GetStatus().GetCode())
		})
	}
}
