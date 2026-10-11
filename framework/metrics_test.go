package framework

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
	"github.com/alessandrolattao/powertools-lambda-go/metrics"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_MetricsAreOffUntilConfigured(t *testing.T) {
	t.Setenv("POWERTOOLS_METRICS_NAMESPACE", "")
	recordingTracer(t)
	assert.Nil(t, New().Metrics, "no namespace, no empty metric set flushed on every invocation")

	t.Setenv("POWERTOOLS_METRICS_NAMESPACE", "orders")
	assert.NotNil(t, New().Metrics)
}

func TestHandle_FlushesTheInvocationMetrics(t *testing.T) {
	t.Setenv("POWERTOOLS_METRICS_NAMESPACE", "")
	recordingTracer(t)
	var emf bytes.Buffer
	c := New(
		WithService("orders"),
		WithLogging(logging.WithWriter(&bytes.Buffer{})),
		WithMetrics(metrics.WithNamespace("Shop"), metrics.WithWriter(&emf)),
	)
	c.lifecycle = nil

	h := lambda.NewHandler(c.Consume(func(ctx context.Context, c *Container, _ jsontext.Value) error {
		return c.Metrics.AddMetric(ctx, "OrderPlaced", metrics.Count, 1)
	}))
	_, err := h.Invoke(lambdaContext(t), []byte(`{}`))
	require.NoError(t, err)

	assert.Contains(t, emf.String(), `"Namespace":"Shop"`)
	assert.Contains(t, emf.String(), `"OrderPlaced":[1.0]`)
	assert.Contains(t, emf.String(), `"service":"orders"`)
}
