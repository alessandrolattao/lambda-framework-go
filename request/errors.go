package request

import "errors"

var (
	// ErrClaimsNotFound is returned when the request carries no JWT claims.
	ErrClaimsNotFound = errors.New("claims not found in JWT")

	// ErrClaimNotFound is returned when a specific claim is missing from the JWT.
	ErrClaimNotFound = errors.New("not found in JWT claims")

	// ErrUserIDNotFound is returned when the JWT has no userId claim.
	ErrUserIDNotFound = errors.New("userId claim not found in JWT")

	// ErrUserIDInvalid is returned when the userId claim is not a valid UUID.
	ErrUserIDInvalid = errors.New("userId claim is not a valid UUID")
)
