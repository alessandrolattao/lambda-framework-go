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
	claims := map[string]string{claimUserID: expected.String()}

	got, err := userIDOf(claims)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
}

func TestUserID_MissingClaim(t *testing.T) {
	t.Parallel()

	claims := map[string]string{claimSub: "sub-123"}
	got, err := userIDOf(claims)

	require.ErrorIs(t, err, ErrUserIDNotFound)
	assert.Equal(t, uuid.Nil(), got)
}

func TestUserID_InvalidUUID(t *testing.T) {
	t.Parallel()

	claims := map[string]string{claimUserID: "not-a-uuid"}
	got, err := userIDOf(claims)

	require.ErrorIs(t, err, ErrUserIDInvalid)
	assert.Contains(t, err.Error(), "not-a-uuid")
	assert.Equal(t, uuid.Nil(), got)
}

func TestUserID_MissingAuthorizer(t *testing.T) {
	t.Parallel()

	serve(t, &events.APIGatewayV2HTTPRequest{}, func(ctx context.Context) {
		got, err := UserID(ctx)

		require.ErrorIs(t, err, ErrClaimsNotFound)
		assert.Equal(t, uuid.Nil(), got)
	})
}

func TestClaim_HappyPath(t *testing.T) {
	t.Parallel()

	claims := map[string]string{claimEmail: "user@example.com"}
	got, err := claimOf(claims, claimEmail)
	require.NoError(t, err)
	assert.Equal(t, "user@example.com", got)
}

func TestClaim_MissingClaim(t *testing.T) {
	t.Parallel()

	claims := map[string]string{claimSub: "x"}
	got, err := claimOf(claims, claimEmail)
	require.ErrorIs(t, err, ErrClaimNotFound)
	assert.Empty(t, got)
}

func TestClaim_NoAuthorizer(t *testing.T) {
	t.Parallel()

	serve(t, &events.APIGatewayV2HTTPRequest{}, func(ctx context.Context) {
		_, err := Claim(ctx, claimEmail)
		assert.ErrorIs(t, err, ErrClaimsNotFound)
	})
}

// restRequestWithClaims builds an APIGatewayProxyRequest carrying the claims
// a Cognito user pool authorizer places in a REST API request, as the event
// decodes from JSON.
func restRequestWithClaims(claims map[string]any) *events.APIGatewayProxyRequest {
	return &events.APIGatewayProxyRequest{
		RequestContext: events.APIGatewayProxyRequestContext{
			Authorizer: map[string]any{"claims": claims},
		},
	}
}

func TestRESTClaims_HappyPath(t *testing.T) {
	t.Parallel()

	r := restRequestWithClaims(map[string]any{claimSub: "sub-123", claimEmail: "a@example.com"})
	claims, err := restClaimsOf(r)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{claimSub: "sub-123", claimEmail: "a@example.com"}, claims)
}

func TestRESTClaims_EveryValueInItsStringForm(t *testing.T) {
	t.Parallel()

	r := restRequestWithClaims(map[string]any{
		"auth_time":      float64(1700000000), // how JSON decodes a number into any
		"email_verified": true,
		"amr":            []any{"pwd", "mfa"},
		"address":        map[string]any{"locality": "Rome", "country": "IT"},
		"nickname":       nil,
	})
	claims, err := restClaimsOf(r)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"auth_time":      "1700000000",
		"email_verified": "true",
		"amr":            `["pwd","mfa"]`,
		"address":        `{"country":"IT","locality":"Rome"}`,
	}, claims, "a null claim counts as missing")

	_, err = claimOf(claims, "nickname")
	require.ErrorIs(t, err, ErrClaimNotFound)
}

func TestRESTClaims_MissingAuthorizer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		authorizer map[string]any
	}{
		{name: "nil authorizer"},
		{name: "no authorization: claims null", authorizer: map[string]any{"claims": nil, "scopes": nil}},
		{name: "a Lambda authorizer's context", authorizer: map[string]any{"principalId": "user-1", claimUserID: uuid.New().String()}},
		{name: "claims that are not an object", authorizer: map[string]any{"claims": "sub=x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &events.APIGatewayProxyRequest{RequestContext: events.APIGatewayProxyRequestContext{Authorizer: tc.authorizer}}
			claims, err := restClaimsOf(r)
			require.ErrorIs(t, err, ErrClaimsNotFound)
			assert.Nil(t, claims)
		})
	}
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

// serveREST runs r through an Event Handler router behind a REST API, with
// check as its handler.
func serveREST(t *testing.T, r *events.APIGatewayProxyRequest, check func(ctx context.Context)) {
	t.Helper()
	ran := false
	app := eventhandler.New()
	app.HandleFunc("GET /me", func(_ http.ResponseWriter, req *http.Request) {
		ran = true
		check(req.Context())
	})
	r.Resource, r.Path = "/me", "/me"
	r.HTTPMethod = http.MethodGet
	_, err := eventhandler.APIGatewayREST(app)(t.Context(), *r)
	require.NoError(t, err)
	require.True(t, ran)
}

func TestClaimsFromTheRESTRequestContext(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	serveREST(t, restRequestWithClaims(map[string]any{claimSub: "sub-123", "userId": id.String()}), func(ctx context.Context) {
		checkClaims(t, ctx, id)
	})
}

func TestRESTUserID_InvalidUUID(t *testing.T) {
	t.Parallel()

	serveREST(t, restRequestWithClaims(map[string]any{claimUserID: "not-a-uuid"}), func(ctx context.Context) {
		got, err := UserID(ctx)
		require.ErrorIs(t, err, ErrUserIDInvalid)
		assert.Equal(t, uuid.Nil(), got)
	})
}

func TestClaimsOfARESTRequestWithoutAuthorizer(t *testing.T) {
	t.Parallel()

	serveREST(t, &events.APIGatewayProxyRequest{}, func(ctx context.Context) {
		_, err := Claims(ctx)
		require.ErrorIs(t, err, ErrClaimsNotFound)
		_, err = Claim(ctx, claimSub)
		require.ErrorIs(t, err, ErrClaimsNotFound)
		_, err = UserID(ctx)
		require.ErrorIs(t, err, ErrClaimsNotFound)
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
