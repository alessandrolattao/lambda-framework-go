package response

import (
	"fmt"
	"net/http"
	"time"
)

// Option sets a header of a success response.
type Option func(http.Header)

// WithCacheMaxAge lets browsers and shared caches keep the response for d.
func WithCacheMaxAge(d time.Duration) Option {
	return func(h http.Header) {
		h.Set(headerCacheControl, fmt.Sprintf("public, max-age=%d", seconds(d)))
	}
}

// WithCacheMaxAgeShared lets browsers keep the response for browser and
// shared caches (CloudFront) for shared.
func WithCacheMaxAgeShared(browser, shared time.Duration) Option {
	return func(h http.Header) {
		h.Set(headerCacheControl, fmt.Sprintf("public, max-age=%d, s-maxage=%d", seconds(browser), seconds(shared)))
	}
}

// WithNoCache forbids caching the response.
func WithNoCache() Option {
	return func(h http.Header) { h.Set(headerCacheControl, noStore) }
}

// WithHeader sets a header.
func WithHeader(name, value string) Option {
	return func(h http.Header) { h.Set(name, value) }
}

func seconds(d time.Duration) int64 {
	return int64(max(d, 0) / time.Second)
}

// headers builds the headers the options set.
func headers(opts []Option) http.Header {
	h := http.Header{}
	for _, opt := range opts {
		opt(h)
	}
	return h
}
