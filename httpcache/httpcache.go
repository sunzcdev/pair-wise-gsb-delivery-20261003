// Package httpcache provides an opt-in, shared HTTP GET cache for goproxy.
//
// The cache is disabled unless explicitly assigned to a ProxyHttpServer:

//	proxy := goproxy.NewProxyHttpServer()
//	proxy.Cache = httpcache.New(httpcache.Options{MaxBytes: 32 << 20})
//
// It honors Cache-Control freshness, Age, ETag/Last-Modified
// revalidation and Vary, uses a bounded replaceable store, and coalesces
// concurrent revalidations for the same representation. It is deliberately
// conservative about Authorization, private, no-store and cookie-bearing
// responses, and does not cache Range, CONNECT or upgraded connections.
package httpcache

import (
	"context"
	"net/http"
	"time"

	"github.com/elazarl/goproxy/cache"
)

// Store is the replaceable persistence layer.
type Store = cache.Store

// Clock abstracts time.
type Clock = cache.Clock

// SystemClock is the wall-clock implementation.
type SystemClock = cache.SystemClock

// ManualClock is a test clock advanced explicitly.
type ManualClock = cache.ManualClock

// Options configures the cache.
type Options = cache.Options

// Cache is the shared GET cache.
type Cache = cache.Cache

// New constructs a Cache.
func New(opts Options) *Cache { return cache.New(opts) }

// NewManualClock creates a ManualClock anchored at start.
func NewManualClock(start time.Time) *ManualClock { return cache.NewManualClock(start) }

// HTTPCache adapts the internal cache to goproxy.ProxyHttpServer.Cache.
type HTTPCache struct {
	*cache.Cache
}

// NewHTTPCache constructs an HTTPCache ready to assign to proxy.Cache.
func NewHTTPCache(opts Options) *HTTPCache {
	return &HTTPCache{Cache: cache.New(opts)}
}

// RoundTrip matches goproxy.HTTPCache.
func (h *HTTPCache) RoundTrip(req *http.Request, fetch func(context.Context, *http.Request) (*http.Response, error)) (*http.Response, error) {
	return h.Cache.RoundTrip(req, fetch)
}
