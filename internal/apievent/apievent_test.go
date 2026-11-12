package apievent

import (
	"context"
	"net/http"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestREST_TheEventAStreamingHandlerKeeps(t *testing.T) {
	t.Parallel()

	ctx := WithREST(t.Context(), &events.APIGatewayProxyRequest{Path: "/prices"})
	r, ok := REST(ctx)
	require.True(t, ok)
	assert.Equal(t, "/prices", r.Path)
}

func TestREST_TheEventTheRouterServes(t *testing.T) {
	t.Parallel()

	var got events.APIGatewayProxyRequest
	var ok bool
	app := eventhandler.New()
	app.HandleFunc("GET /orders", func(_ http.ResponseWriter, req *http.Request) {
		got, ok = REST(req.Context())
	})
	_, err := eventhandler.APIGatewayREST(app)(t.Context(), events.APIGatewayProxyRequest{
		HTTPMethod: http.MethodGet, Resource: "/orders", Path: "/orders",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "/orders", got.Path)
}

func TestREST_NoEventOutsideARequest(t *testing.T) {
	t.Parallel()

	for _, ctx := range []context.Context{
		t.Context(),
		WithREST(t.Context(), nil),
	} {
		_, ok := REST(ctx)
		assert.False(t, ok)
	}
}
