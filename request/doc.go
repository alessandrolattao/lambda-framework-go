// Package request reads what a handler needs out of an API Gateway HTTP API
// (payload format 2.0) request that typed binding does not cover: the JWT
// claims API Gateway's authorizer verified, the caller's IP behind trusted
// proxies, comma-separated id lists.
//
// Path, query, header and body values are bound and validated by the typed
// operations of the Event Handler router
// (github.com/alessandrolattao/powertools-lambda-go/eventhandler), whose
// context the claim functions read:
//
//	userID, err := request.UserID(ctx)
package request
