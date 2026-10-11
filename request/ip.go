package request

import (
	"cmp"
	"net/netip"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

const (
	// unknownIP is what ClientIP answers when the request carries no address.
	unknownIP = "unknown"

	// headerForwardedFor is X-Forwarded-For as API Gateway hands it over:
	// HTTP API lowercases every header name.
	headerForwardedFor = "x-forwarded-for"
)

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
func ClientIP(r *events.APIGatewayV2HTTPRequest, trustedProxies int) string {
	sourceIP := r.RequestContext.HTTP.SourceIP
	if trustedProxies <= 0 {
		return cmp.Or(sourceIP, unknownIP)
	}

	var hops []string
	for hop := range strings.SplitSeq(r.Headers[headerForwardedFor], ",") {
		if hop = strings.TrimSpace(hop); hop != "" {
			hops = append(hops, hop)
		}
	}
	if n := len(hops); n > 0 && hops[n-1] == sourceIP {
		hops = hops[:n-1]
	}

	i := len(hops) - trustedProxies
	if i < 0 {
		// Fewer hops than trusted proxies: the request did not come through
		// them, so the peer is the client.
		return cmp.Or(sourceIP, unknownIP)
	}
	// Whatever is not an address (junk, or a port the proxy left on it) must
	// not reach the logs or a rate-limit key as if it were one.
	if addr, err := netip.ParseAddr(hops[i]); err == nil {
		return addr.String()
	}
	if addrPort, err := netip.ParseAddrPort(hops[i]); err == nil {
		return addrPort.Addr().String()
	}
	return cmp.Or(sourceIP, unknownIP)
}
