package clientip

import (
	"net/netip"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
)

// The addresses the cases use: the real client, and the CloudFront edge in
// front of API Gateway.
const (
	clientAddr = "203.0.113.7"
	edgeAddr   = "198.51.100.9"
)

// restRequest is a REST API request from source carrying X-Forwarded-For xff,
// with the header names as clients write them.
func restRequest(xff, source string) *events.APIGatewayProxyRequest {
	return &events.APIGatewayProxyRequest{
		Headers:           map[string]string{headerForwardedFor: xff},
		MultiValueHeaders: map[string][]string{headerForwardedFor: {xff}},
		RequestContext: events.APIGatewayProxyRequestContext{
			Identity: events.APIGatewayRequestIdentity{SourceIP: source},
		},
	}
}

// TestREST is the REST API twin of request's TestClientIP: the same cases, on
// requestContext.identity.sourceIp.
func TestREST(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		xff     string
		source  string
		proxies int
		want    string
	}{
		{
			name:   "no trusted proxy: the peer API Gateway saw",
			xff:    "1.2.3.4",
			source: clientAddr,
			want:   clientAddr,
		},
		{
			name: "no trusted proxy and no peer",
			want: Unknown,
		},
		{
			name:    "behind CloudFront, API Gateway appended its peer",
			xff:     "1.2.3.4, " + clientAddr + ", " + edgeAddr,
			source:  edgeAddr,
			proxies: 1,
			want:    clientAddr,
		},
		{
			name:    "behind CloudFront, API Gateway did not append",
			xff:     "1.2.3.4, " + clientAddr,
			source:  edgeAddr,
			proxies: 1,
			want:    clientAddr,
		},
		{
			name:    "two trusted proxies",
			xff:     clientAddr + ", 10.0.0.1",
			source:  "10.0.0.2",
			proxies: 2,
			want:    clientAddr,
		},
		{
			name:    "whitespace and empty hops are ignored",
			xff:     " , " + clientAddr + " ,",
			source:  edgeAddr,
			proxies: 1,
			want:    clientAddr,
		},
		{
			name:    "fewer hops than trusted proxies: the peer is the client",
			source:  clientAddr,
			proxies: 1,
			want:    clientAddr,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, REST(restRequest(tc.xff, tc.source), tc.proxies))
		})
	}
}

func TestREST_OnlyAddresses(t *testing.T) {
	t.Parallel()

	assert.Equal(t, clientAddr, REST(restRequest(clientAddr+":51234", edgeAddr), 1), "a port is dropped")
	assert.Equal(t, "2001:db8::1", REST(restRequest("[2001:db8::1]:443", edgeAddr), 1), "IPv6 with a port too")
	assert.Equal(t, edgeAddr, REST(restRequest("<script>", edgeAddr), 1), "junk falls back to the peer")
}

func TestREST_TheHeaderNameInAnyCase(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"X-Forwarded-For", "x-forwarded-for", "X-FORWARDED-FOR"} {
		multi := &events.APIGatewayProxyRequest{
			MultiValueHeaders: map[string][]string{name: {clientAddr}},
			RequestContext:    events.APIGatewayProxyRequestContext{Identity: events.APIGatewayRequestIdentity{SourceIP: edgeAddr}},
		}
		assert.Equal(t, clientAddr, REST(multi, 1), "multiValueHeaders: %s", name)

		single := &events.APIGatewayProxyRequest{
			Headers:        map[string]string{name: clientAddr},
			RequestContext: events.APIGatewayProxyRequestContext{Identity: events.APIGatewayRequestIdentity{SourceIP: edgeAddr}},
		}
		assert.Equal(t, clientAddr, REST(single, 1), "headers: %s", name)
	}
}

func TestREST_MultiValueHeadersComeFirst(t *testing.T) {
	t.Parallel()

	r := &events.APIGatewayProxyRequest{
		// A repeated header: headers keeps its last value only.
		Headers:           map[string]string{headerForwardedFor: edgeAddr},
		MultiValueHeaders: map[string][]string{headerForwardedFor: {clientAddr, edgeAddr}},
		RequestContext:    events.APIGatewayProxyRequestContext{Identity: events.APIGatewayRequestIdentity{SourceIP: "10.0.0.2"}},
	}
	assert.Equal(t, clientAddr, REST(r, 2), "every value of the repeated header, joined in order")

	r.MultiValueHeaders = map[string][]string{"Accept": {"application/json"}}
	assert.Equal(t, "10.0.0.2", REST(r, 2), "present multiValueHeaders without the header: no hops")
}

// FuzzREST is the REST API twin of request's FuzzClientIP: whatever a client
// writes in X-Forwarded-For, the answer is an address, the peer API Gateway
// saw, or Unknown: never the client's text.
func FuzzREST(f *testing.F) {
	f.Add("1.2.3.4, 203.0.113.7, 198.51.100.9", "198.51.100.9", 1)
	f.Add("203.0.113.7:51234", "198.51.100.9", 1)
	f.Add("[2001:db8::1]:443", "198.51.100.9", 1)
	f.Add("<script>", "", 2)
	f.Add(" , ,, ", "203.0.113.7", 0)

	f.Fuzz(func(t *testing.T, xff, sourceIP string, proxies int) {
		got := REST(restRequest(xff, sourceIP), proxies%5)
		if got == Unknown || got == sourceIP {
			return
		}
		if _, err := netip.ParseAddr(got); err != nil {
			t.Fatalf("REST(%q, %q, %d) = %q, not an address", xff, sourceIP, proxies%5, got)
		}
	})
}
