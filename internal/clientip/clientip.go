// Package clientip finds the address of the client that called an API
// Gateway API, looking past the proxies the application trusts: one rule for
// HTTP APIs (request.ClientIP) and REST APIs (the framework's REST path).
package clientip

import (
	"cmp"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// Unknown is what Resolve answers when the request carries no address.
const Unknown = "unknown"

// headerForwardedFor is X-Forwarded-For.
const headerForwardedFor = "X-Forwarded-For"

// Resolve returns the address of the client, from sourceIP, the peer API
// Gateway itself saw, and forwardedFor, the X-Forwarded-For header.
//
// With trustedProxies 0 it is sourceIP, which the client cannot forge. The
// first entry of X-Forwarded-For is never used, since the client writes it.
//
// When trustedProxies proxies of yours sit in front of API Gateway (CloudFront
// is one), sourceIP is the last of them and each of them appended to
// X-Forwarded-For the address it saw, so the client is the trustedProxies-th
// entry counting from the right (after dropping the sourceIP API Gateway may
// append itself). This holds only if API Gateway cannot be reached around
// those proxies: a caller that goes straight to it can still choose that
// entry.
func Resolve(sourceIP, forwardedFor string, trustedProxies int) string {
	if trustedProxies <= 0 {
		return cmp.Or(sourceIP, Unknown)
	}

	var hops []string
	for hop := range strings.SplitSeq(forwardedFor, ",") {
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
		return cmp.Or(sourceIP, Unknown)
	}
	// Whatever is not an address (junk, or a port the proxy left on it) must
	// not reach the logs or a rate-limit key as if it were one.
	if addr, err := netip.ParseAddr(hops[i]); err == nil {
		return addr.String()
	}
	if addrPort, err := netip.ParseAddrPort(hops[i]); err == nil {
		return addrPort.Addr().String()
	}
	return cmp.Or(sourceIP, Unknown)
}

// REST returns the address of the client that called an API Gateway REST API
// (Lambda proxy integration, payload format 1.0): Resolve on
// requestContext.identity.sourceIp and X-Forwarded-For.
func REST(r *events.APIGatewayProxyRequest, trustedProxies int) string {
	return Resolve(r.RequestContext.Identity.SourceIP, restForwardedFor(r), trustedProxies)
}

// restForwardedFor returns X-Forwarded-For of a REST API request. A REST API
// hands the header names over as the client wrote them, so the name is
// matched whatever its case; and it keeps every value of a repeated header in
// multiValueHeaders, which is read when present, as Powertools' APIGatewayREST
// adapter does, its values joined with ",". Names are walked in order, so the
// answer never depends on map order.
func restForwardedFor(r *events.APIGatewayProxyRequest) string {
	if len(r.MultiValueHeaders) > 0 {
		var values []string
		for _, name := range slices.Sorted(maps.Keys(r.MultiValueHeaders)) {
			if strings.EqualFold(name, headerForwardedFor) {
				values = append(values, r.MultiValueHeaders[name]...)
			}
		}
		return strings.Join(values, ",")
	}
	for _, name := range slices.Sorted(maps.Keys(r.Headers)) {
		if strings.EqualFold(name, headerForwardedFor) {
			return r.Headers[name]
		}
	}
	return ""
}
