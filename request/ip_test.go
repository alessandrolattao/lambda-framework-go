package request

import (
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
)

// The addresses the ClientIP cases use: the real client, and the CloudFront
// edge in front of API Gateway.
const (
	clientAddr = "203.0.113.7"
	edgeAddr   = "198.51.100.9"
)

func TestClientIP(t *testing.T) {
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
			want: unknownIP,
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
			r := &events.APIGatewayV2HTTPRequest{
				Headers: map[string]string{headerForwardedFor: tc.xff},
				RequestContext: events.APIGatewayV2HTTPRequestContext{
					HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{SourceIP: tc.source},
				},
			}
			assert.Equal(t, tc.want, ClientIP(r, tc.proxies))
		})
	}
}

func TestClientIP_OnlyAddresses(t *testing.T) {
	t.Parallel()

	request := func(xff string) *events.APIGatewayV2HTTPRequest {
		return &events.APIGatewayV2HTTPRequest{
			Headers: map[string]string{headerForwardedFor: xff},
			RequestContext: events.APIGatewayV2HTTPRequestContext{
				HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{SourceIP: edgeAddr},
			},
		}
	}

	assert.Equal(t, clientAddr, ClientIP(request(clientAddr+":51234"), 1), "a port is dropped")
	assert.Equal(t, "2001:db8::1", ClientIP(request("[2001:db8::1]:443"), 1), "IPv6 with a port too")
	assert.Equal(t, edgeAddr, ClientIP(request("<script>"), 1), "junk falls back to the peer")
}
