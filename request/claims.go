package request

import (
	"context"
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

// userIDOf extracts the internal user UUID from the custom `userId` claim. The
// claim is meant to be injected by a Cognito pre-token-generation Lambda on
// every sign-in and token refresh, so authenticated handlers can address a
// user by its primary key without a sub → user id lookup on every call.
func userIDOf(r *events.APIGatewayV2HTTPRequest) (uuid.UUID, error) {
	claims, err := claimsOf(r)
	if err != nil {
		return uuid.Nil(), err
	}
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

// claimOf returns the value of a single JWT claim from the HTTP API v2 request
// context. With payload format 2.0 every JWT claim surfaces as `string`;
// callers that need another type parse the string themselves
// (strconv.ParseBool, uuid.Parse, ...).
func claimOf(r *events.APIGatewayV2HTTPRequest, name string) (string, error) {
	claims, err := claimsOf(r)
	if err != nil {
		return "", err
	}
	value := claims[name]
	if value == "" {
		return "", fmt.Errorf("claim %q: %w", name, ErrClaimNotFound)
	}
	return value, nil
}

// event returns the API Gateway HTTP API request ctx is serving: the Event
// Handler router keeps it in the context of every request.
func event(ctx context.Context) (*events.APIGatewayV2HTTPRequest, error) {
	r, ok := eventhandler.APIGatewayHTTPEvent(ctx)
	if !ok {
		return nil, ErrClaimsNotFound
	}
	return &r, nil
}

// Claims returns the JWT claims API Gateway's JWT authorizer verified for
// the request ctx belongs to (a typed operation's context, or r.Context() in
// a plain handler). Every value is a string, as payload format 2.0 delivers
// them.
func Claims(ctx context.Context) (map[string]string, error) {
	r, err := event(ctx)
	if err != nil {
		return nil, err
	}
	return claimsOf(r)
}

// Claim returns one JWT claim of the request ctx belongs to.
func Claim(ctx context.Context, name string) (string, error) {
	r, err := event(ctx)
	if err != nil {
		return "", err
	}
	return claimOf(r, name)
}

// UserID returns the internal user id, the custom userId claim, of the
// request ctx belongs to.
func UserID(ctx context.Context) (uuid.UUID, error) {
	r, err := event(ctx)
	if err != nil {
		return uuid.Nil(), err
	}
	return userIDOf(r)
}
