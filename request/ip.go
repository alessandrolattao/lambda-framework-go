package request

import (
	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/lambda-framework-go/internal/clientip"
)

// headerForwardedFor is X-Forwarded-For as API Gateway hands it over: HTTP
// API lowercases every header name.
const headerForwardedFor = "x-forwarded-for"

// ClientIP returns the address of the client that called an API Gateway HTTP
// API (payload format 2.0).
//
// With trustedProxies 0 it is requestContext.http.sourceIp: the peer API
// Gateway itself saw, which the client cannot forge. The first entry of
// X-Forwarded-For is never used, since the client writes it.
//
// When trustedProxies proxies of yours sit in front of API Gateway (CloudFront
// is one), sourceIp is the last of them and each of them appended to
// X-Forwarded-For the address it saw, so the client is the trustedProxies-th
// entry counting from the right (after dropping the sourceIp API Gateway may
// append itself). This holds only if API Gateway cannot be reached around
// those proxies: a caller that goes straight to it can still choose that
// entry.
//
// Behind a REST API, Container.ClientIP applies the same rule to
// requestContext.identity.sourceIp.
func ClientIP(r *events.APIGatewayV2HTTPRequest, trustedProxies int) string {
	return clientip.Resolve(r.RequestContext.HTTP.SourceIP, r.Headers[headerForwardedFor], trustedProxies)
}
