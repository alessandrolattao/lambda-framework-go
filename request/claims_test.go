package request

import (
	"context"
	"net/http"
	"testing"
	"uuid"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	claimSub   = "sub"
	claimEmail = "email"
)

// requestWithClaims builds an APIGatewayV2HTTPRequest carrying the JWT claim
// map shape produced by API Gateway HTTP API v2 + JWT authorizer.
func requestWithClaims(claims map[string]string) *events.APIGatewayV2HTTPRequest {
	return &events.APIGatewayV2HTTPRequest{
		RequestContext: events.APIGatewayV2HTTPRequestContext{
			Authorizer: &events.APIGatewayV2HTTPRequestContextAuthorizerDescription{
				JWT: &events.APIGatewayV2HTTPRequestContextAuthorizerJWTDescription{
					Claims: claims,
				},
			},
		},
	}
}

func TestClaims_HappyPath(t *testing.T) {
	t.Parallel()

	r := requestWithClaims(map[string]string{claimSub: "sub-123", claimEmail: "a@example.com"})
	claims, err := claimsOf(r)
	require.NoError(t, err)
	assert.Equal(t, "sub-123", claims[claimSub])
	assert.Equal(t, "a@example.com", claims[claimEmail])
}

func TestClaims_MissingAuthorizer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ctx  events.APIGatewayV2HTTPRequestContext
	}{
		{name: "nil authorizer"},
		{name: "nil JWT", ctx: events.APIGatewayV2HTTPRequestContext{
			Authorizer: &events.APIGatewayV2HTTPRequestContextAuthorizerDescription{},
		}},
		{name: "nil claims map", ctx: events.APIGatewayV2HTTPRequestContext{
			Authorizer: &events.APIGatewayV2HTTPRequestContextAuthorizerDescription{
				JWT: &events.APIGatewayV2HTTPRequestContextAuthorizerJWTDescription{},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &events.APIGatewayV2HTTPRequest{RequestContext: tc.ctx}
			claims, err := claimsOf(r)
			require.ErrorIs(t, err, ErrClaimsNotFound)
			assert.Nil(t, claims)
		})
	}
}

func TestUserID_HappyPath(t *testing.T) {
	t.Parallel()

	expected := uuid.New()
	r := requestWithClaims(map[string]string{claimUserID: expected.String()})

	got, err := userIDOf(r)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
}

func TestUserID_MissingClaim(t *testing.T) {
	t.Parallel()

	r := requestWithClaims(map[string]string{claimSub: "sub-123"})
	got, err := userIDOf(r)

	require.ErrorIs(t, err, ErrUserIDNotFound)
	assert.Equal(t, uuid.Nil(), got)
}

func TestUserID_InvalidUUID(t *testing.T) {
	t.Parallel()

	r := requestWithClaims(map[string]string{claimUserID: "not-a-uuid"})
	got, err := userIDOf(r)

	require.ErrorIs(t, err, ErrUserIDInvalid)
	assert.Contains(t, err.Error(), "not-a-uuid")
	assert.Equal(t, uuid.Nil(), got)
}

func TestUserID_MissingAuthorizer(t *testing.T) {
	t.Parallel()

	r := &events.APIGatewayV2HTTPRequest{}
	got, err := userIDOf(r)

	require.ErrorIs(t, err, ErrClaimsNotFound)
	assert.Equal(t, uuid.Nil(), got)
}

func TestClaim_HappyPath(t *testing.T) {
	t.Parallel()

	r := requestWithClaims(map[string]string{claimEmail: "user@example.com"})
	got, err := claimOf(r, claimEmail)
	require.NoError(t, err)
	assert.Equal(t, "user@example.com", got)
}

func TestClaim_MissingClaim(t *testing.T) {
	t.Parallel()

	r := requestWithClaims(map[string]string{claimSub: "x"})
	got, err := claimOf(r, claimEmail)
	require.ErrorIs(t, err, ErrClaimNotFound)
	assert.Empty(t, got)
}

func TestClaim_NoAuthorizer(t *testing.T) {
	t.Parallel()

	r := &events.APIGatewayV2HTTPRequest{}
	_, err := claimOf(r, claimEmail)
	assert.ErrorIs(t, err, ErrClaimsNotFound)
}

// serve runs r through an Event Handler router, with check as its handler.
func serve(t *testing.T, r *events.APIGatewayV2HTTPRequest, check func(ctx context.Context)) {
	t.Helper()
	ran := false
	app := eventhandler.New()
	app.HandleFunc("GET /me", func(_ http.ResponseWriter, req *http.Request) {
		ran = true
		check(req.Context())
	})
	r.RawPath = "/me"
	r.RequestContext.HTTP.Method = http.MethodGet
	_, err := eventhandler.APIGatewayHTTP(app)(t.Context(), *r)
	require.NoError(t, err)
	require.True(t, ran)
}

func TestClaimsFromTheRequestContext(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	serve(t, requestWithClaims(map[string]string{claimSub: "sub-123", "userId": id.String()}), func(ctx context.Context) {
		checkClaims(t, ctx, id)
	})
}

// checkClaims reads the claims of a served request.
func checkClaims(t *testing.T, ctx context.Context, id uuid.UUID) {
	t.Helper()
	claims, err := Claims(ctx)
	require.NoError(t, err)
	assert.Equal(t, "sub-123", claims[claimSub])

	sub, err := Claim(ctx, claimSub)
	require.NoError(t, err)
	assert.Equal(t, "sub-123", sub)

	got, err := UserID(ctx)
	require.NoError(t, err)
	assert.Equal(t, id, got)
}

func TestClaimsOutsideARequest(t *testing.T) {
	t.Parallel()

	_, err := Claims(t.Context())
	require.ErrorIs(t, err, ErrClaimsNotFound)
	_, err = Claim(t.Context(), claimSub)
	require.ErrorIs(t, err, ErrClaimsNotFound)
	_, err = UserID(t.Context())
	require.ErrorIs(t, err, ErrClaimsNotFound)
}
