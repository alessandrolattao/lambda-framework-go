package request

import (
	"errors"
	"net/netip"
	"strconv"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// FuzzClientIP checks that whatever a client writes in X-Forwarded-For, the
// answer is an address, the peer API Gateway saw, or unknownIP: never the
// client's text.
func FuzzClientIP(f *testing.F) {
	f.Add("1.2.3.4, 203.0.113.7, 198.51.100.9", "198.51.100.9", 1)
	f.Add("203.0.113.7:51234", "198.51.100.9", 1)
	f.Add("[2001:db8::1]:443", "198.51.100.9", 1)
	f.Add("<script>", "", 2)
	f.Add(" , ,, ", "203.0.113.7", 0)

	f.Fuzz(func(t *testing.T, xff, sourceIP string, proxies int) {
		r := &events.APIGatewayV2HTTPRequest{
			Headers: map[string]string{headerForwardedFor: xff},
			RequestContext: events.APIGatewayV2HTTPRequestContext{
				HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{SourceIP: sourceIP},
			},
		}
		got := ClientIP(r, proxies%5)
		if got == unknownIP || got == sourceIP {
			return
		}
		if _, err := netip.ParseAddr(got); err != nil {
			t.Fatalf("ClientIP(%q, %q, %d) = %q, not an address", xff, sourceIP, proxies%5, got)
		}
	})
}

// FuzzParseCSVIDs checks that the cap holds and that every failure is one of
// the two the function documents.
func FuzzParseCSVIDs(f *testing.F) {
	f.Add("1,2,3", 10)
	f.Add(" 1 , , 2 ,", 2)
	f.Add("1,2,3,bad", 3)
	f.Add("", 0)

	f.Fuzz(func(t *testing.T, raw string, maxIDs int) {
		maxIDs %= 50
		ids, err := ParseCSVIDs(raw, maxIDs, func(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) })
		if err != nil {
			checkCSVError(t, err)
			return
		}
		if ids == nil || len(ids) > max(maxIDs, 0) {
			t.Fatalf("ParseCSVIDs(%q, %d) = %v: nil or over the cap", raw, maxIDs, ids)
		}
	})
}

// checkCSVError fails the test for an error ParseCSVIDs does not document.
func checkCSVError(t *testing.T, err error) {
	t.Helper()
	if invalid, ok := errors.AsType[*InvalidCSVIDError](err); ok {
		if invalid.Value == "" {
			t.Fatal("an invalid id without the token that broke it")
		}
		return
	}
	if !errors.Is(err, ErrTooManyCSVIDs) {
		t.Fatalf("undocumented error %v", err)
	}
}
