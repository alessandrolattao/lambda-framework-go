package request

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"uuid"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/aws/aws-lambda-go/events"
)

// claimUserID is the custom claim carrying the internal user id.
const claimUserID = "userId"

// claimsOf extracts JWT claims from the API Gateway HTTP API v2 request context.
//
// With payload format 2.0 the JWT authorizer places claims under
// `requestContext.authorizer.jwt.claims` as a `map[string]string` typed
// field on the event struct (APIGatewayV2HTTPRequestContextAuthorizerJWTDescription).
// All claim values therefore surface as strings; parsers downstream coerce
// to the desired types.
func claimsOf(r *events.APIGatewayV2HTTPRequest) (map[string]string, error) {
	auth := r.RequestContext.Authorizer
	if auth == nil || auth.JWT == nil || auth.JWT.Claims == nil {
		return nil, ErrClaimsNotFound
	}
	return auth.JWT.Claims, nil
}

// restClaimsOf extracts the claims a Cognito user pool authorizer verified
// from the API Gateway REST API (payload format 1.0) request context.
//
// The authorizer places them under `requestContext.authorizer.claims`, which
// the event struct leaves untyped (map[string]any). Every value is turned into
// its string form, as payload format 2.0 delivers them: a string stays as it
// is, a number, a boolean, an array or an object becomes its JSON text
// (1700000000 → "1700000000", true → "true"), and a null claim counts as
// missing. A Lambda authorizer's context has no `claims` and is not read.
func restClaimsOf(r *events.APIGatewayProxyRequest) (map[string]string, error) {
	raw, ok := r.RequestContext.Authorizer["claims"].(map[string]any)
	if !ok {
		return nil, ErrClaimsNotFound
	}
	claims := make(map[string]string, len(raw))
	for name, value := range raw {
		if text, ok := claimString(value); ok {
			claims[name] = text
		}
	}
	return claims, nil
}

// claimString is the string form of a claim value, and false for a claim
// that counts as missing: null, or a value that is not JSON.
func claimString(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	default:
		text, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			return "", false
		}
		return string(text), true
	}
}

// userIDOf extracts the internal user UUID from the custom `userId` claim. The
// claim is meant to be injected by a Cognito pre-token-generation Lambda on
// every sign-in and token refresh, so authenticated handlers can address a
// user by its primary key without a sub → user id lookup on every call.
func userIDOf(claims map[string]string) (uuid.UUID, error) {
	raw := claims[claimUserID]
	if raw == "" {
		return uuid.Nil(), ErrUserIDNotFound
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("%w: %s", ErrUserIDInvalid, raw)
	}
	return parsed, nil
}

// claimOf returns the value of a single claim. Every claim surfaces as
// `string`; callers that need another type parse the string themselves
// (strconv.ParseBool, uuid.Parse, ...).
func claimOf(claims map[string]string, name string) (string, error) {
	value := claims[name]
	if value == "" {
		return "", fmt.Errorf("claim %q: %w", name, ErrClaimNotFound)
	}
	return value, nil
}

// claimsFrom returns the claims of the API Gateway request ctx is serving:
// the Event Handler router keeps the event in the context of every request,
// an HTTP API one (JWT authorizer) or a REST API one (Cognito user pool
// authorizer).
func claimsFrom(ctx context.Context) (map[string]string, error) {
	if r, ok := eventhandler.APIGatewayHTTPEvent(ctx); ok {
		return claimsOf(&r)
	}
	if r, ok := eventhandler.APIGatewayRESTEvent(ctx); ok {
		return restClaimsOf(&r)
	}
	return nil, ErrClaimsNotFound
}

// Claims returns the claims API Gateway's authorizer verified for the request
// ctx belongs to (a typed operation's context, or r.Context() in a plain
// handler): the JWT authorizer's behind an HTTP API, the Cognito user pool
// authorizer's behind a REST API. Every value is a string, as payload format
// 2.0 delivers them.
func Claims(ctx context.Context) (map[string]string, error) {
	return claimsFrom(ctx)
}

// Claim returns one claim of the request ctx belongs to.
func Claim(ctx context.Context, name string) (string, error) {
	claims, err := claimsFrom(ctx)
	if err != nil {
		return "", err
	}
	return claimOf(claims, name)
}

// UserID returns the internal user id, the custom userId claim, of the
// request ctx belongs to.
func UserID(ctx context.Context) (uuid.UUID, error) {
	claims, err := claimsFrom(ctx)
	if err != nil {
		return uuid.Nil(), err
	}
	return userIDOf(claims)
}
