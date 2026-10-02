package goproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CacheConfig configures the optional shared HTTP response cache of a
// ProxyHttpServer. The cache is disabled by default; assign a non-nil
// *CacheConfig to ProxyHttpServer.Cache to enable it.
//
// The cache implements the relevant parts of RFC 9111 for shared caches:
// Cache-Control freshness, Age, ETag/Last-Modified revalidation and Vary.
// It deliberately takes conservative decisions for responses that may be
// tied to a single user (Authorization requests, Set-Cookie, private and
// no-store responses).
//
// Only plain GET responses are considered. CONNECT, upgraded connections
// (such as WebSockets), Range requests, HEAD and other methods are never
// served from or stored in the cache.
type CacheConfig struct {
	// MaxResources bounds the number of URL resources kept by the default
	// in-memory store. A non-positive value defaults to 100. Custom stores
	// implement their own capacity policy.
	MaxResources int
	// MaxBodyBytes is the maximum size of a response body buffered for
	// caching. Larger responses are proxied through and not stored. A
	// non-positive value defaults to 8 MiB.
	MaxBodyBytes int64
	// RevalidationTimeout bounds how long a caller waits for an in-flight
	// revalidation of the same variant. A non-positive value defaults to
	// 30 seconds.
	RevalidationTimeout time.Duration
	// Clock is the time source for freshness and Age. When nil the wall
	// clock is used. Tests can pass a ManualClock.
	Clock Clock
	// Store is the backing storage. When nil a bounded in-memory LRU store
	// is used. A custom Store must be safe for concurrent use.
	Store CacheStore
}

// Clock abstracts the time source used by the cache.
type Clock interface {
	Now() time.Time
}

// ManualClock is a controllable Clock, primarily intended for tests.
type ManualClock struct {
	mu  sync.RWMutex
	now time.Time
}

// NewManualClock returns a ManualClock set to t.
func NewManualClock(t time.Time) *ManualClock {
	return &ManualClock{now: t}
}

// Now returns the current mocked time.
func (c *ManualClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

// Advance moves the clock forward by d and returns the new time.
func (c *ManualClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// Set sets the clock to t.
func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// CacheStore is the replaceable backing storage of the cache.
//
// Keys are absolute request URLs; each CachedResource groups all Vary
// variants of one URL. Implementations must be safe for concurrent use.
type CacheStore interface {
	// Get returns the resource stored under key, or nil when absent. A hit
	// must record the access for recency tracking (LRU).
	Get(key string) *CachedResource
	// Put stores or replaces the resource under key.
	Put(key string, resource *CachedResource)
	// Delete removes the resource stored under key, if any.
	Delete(key string)
}

// CachedResource groups all Vary variants of one URL.
type CachedResource struct {
	// Vary is the Vary header used by every variant, or nil. A wildcard
	// ("Vary: *") response is never stored.
	Vary []string
	// Variants maps the request-header selector to the cached response;
	// the empty selector belongs to responses without Vary.
	Variants map[string]*CachedResponse
}

// CachedResponse is one stored variant of a URL.
type CachedResponse struct {
	// StatusCode is the stored response status (typically 200).
	StatusCode int
	// Header is the stored header set without hop-by-hop headers.
	Header http.Header
	// Trailer holds response trailers captured while streaming the body.
	Trailer http.Header
	// Body is the buffered response entity. A 304 revalidation refreshes
	// only metadata and keeps the existing Body reference untouched.
	Body []byte
	// RequestHeaders records the request headers this variant was selected
	// for, restricted to the Vary header names.
	RequestHeaders http.Header
	// Date is the origin Date, falling back to response reception time.
	Date time.Time
	// ResponseTime is when the proxy finished receiving the response.
	ResponseTime time.Time
	// AgeValue is the parsed Age response header at storage time.
	AgeValue time.Duration
	// FreshnessLifetime is the s-maxage/max-age or Expires lifetime.
	FreshnessLifetime time.Duration
}

const (
	// defaultCacheMaxResources is the default capacity of the memory store.
	defaultCacheMaxResources = 100
	// defaultCacheMaxBodyBytes bounds buffered response bodies (8 MiB).
	defaultCacheMaxBodyBytes = 8 << 20
	// defaultRevalidationTimeout bounds waiting on a shared revalidation.
	defaultRevalidationTimeout = 30 * time.Second
)

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// httpCache is the cache coordinator. Freshness, Vary, revalidation and
// single-flight coordination live here; the Store is a passive map.
type httpCache struct {
	cfg     CacheConfig
	store   CacheStore
	clock   Clock
	mu      sync.Mutex
	flights map[string]*revalidationFlight
}

// revalidationFlight collapses concurrent revalidations of the same URL and
// Vary variant into one upstream conditional request.
type revalidationFlight struct {
	done   chan struct{}
	result *http.Response
	body   []byte
	err    error
}

func (proxy *ProxyHttpServer) cacheImpl() *httpCache {
	proxy.cacheOnce.Do(func() {
		proxy.cacheInstance = newHTTPCache(*proxy.Cache)
	})
	return proxy.cacheInstance
}

func newHTTPCache(cfg CacheConfig) *httpCache {
	if cfg.MaxResources <= 0 {
		cfg.MaxResources = defaultCacheMaxResources
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultCacheMaxBodyBytes
	}
	if cfg.RevalidationTimeout <= 0 {
		cfg.RevalidationTimeout = defaultRevalidationTimeout
	}
	if cfg.Clock == nil {
		cfg.Clock = wallClock{}
	}
	if cfg.Store == nil {
		cfg.Store = newMemoryStore(cfg.MaxResources)
	}
	return &httpCache{
		cfg:     cfg,
		store:   cfg.Store,
		clock:   cfg.Clock,
		flights: make(map[string]*revalidationFlight),
	}
}

// cacheState carries the per-request cache decision through handleHttp.
type cacheState struct {
	fromCache  bool
	capturable bool
}

// requestCacheable reports whether r may use the cache on the request side.
func (hc *httpCache) requestCacheable(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.Header.Get("Range") != "" || r.Header.Get("If-Range") != "" {
		return false
	}
	if headerToken(r.Header, "Connection", "upgrade") || r.Header.Get("Upgrade") != "" {
		return false
	}
	cc := parseCacheControl(r.Header.Get("Cache-Control"))
	if cc["no-store"] != "" || cc["no-cache"] != "" {
		return false
	}
	if r.Header.Get("Pragma") == "no-cache" {
		return false
	}
	// Client-supplied conditionals belong to the client and must bypass
	// stored state to preserve their semantics.
	if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
		return false
	}
	return true
}

// beforeRoundTrip implements the read side. A non-nil response was served
// from the cache (fresh or revalidated); otherwise the caller fetches and
// may store the result.
func (hc *httpCache) beforeRoundTrip(ctx *ProxyCtx, r *http.Request) (*http.Response, *cacheState) {
	state := &cacheState{}
	if !hc.requestCacheable(r) {
		return nil, state
	}

	resource := hc.store.Get(r.URL.String())
	if resource == nil {
		return nil, state
	}

	selector, _ := varySelector(resource.Vary, r)

	variant, ok := resource.Variants[selector]
	if !ok {
		// Different Vary variants must never be merged.
		return nil, state
	}

	now := hc.clock.Now()
	age := variant.currentAge(now)
	if !variant.mustRevalidate() && age < variant.FreshnessLifetime {
		state.fromCache = true
		return variant.toResponse(r, age), state
	}

	// Without a validator a conditional request is impossible: perform a
	// plain fetch that replaces the entry instead of revalidating.
	if variant.Header.Get("ETag") == "" && variant.Header.Get("Last-Modified") == "" {
		return nil, state
	}

	resp, err := hc.revalidate(ctx, r, resource, variant)
	if err != nil {
		// Never serve stale silently: fall through to a normal fetch and
		// leave the stored entry untouched.
		ctx.Warnf("cache revalidation failed for %v: %v", r.URL, err)
		return nil, state
	}
	state.fromCache = true
	return resp, state
}

// revalidate performs a conditional GET, joining any in-flight revalidation
// for the same URL+variant.
func (hc *httpCache) revalidate(ctx *ProxyCtx, clientReq *http.Request, resource *CachedResource, variant *CachedResponse) (*http.Response, error) {
	selector, _ := varySelector(resource.Vary, clientReq)
	flightKey := clientReq.URL.String() + "\x00" + selector

	hc.mu.Lock()
	if flight, ok := hc.flights[flightKey]; ok {
		hc.mu.Unlock()
		return hc.awaitFlight(clientReq, flight)
	}
	flight := &revalidationFlight{done: make(chan struct{})}
	hc.flights[flightKey] = flight
	hc.mu.Unlock()

	defer func() {
		// Remove the flight and broadcast completion atomically: waiters
		// either find the in-flight entry or observe the closed channel.
		hc.mu.Lock()
		delete(hc.flights, flightKey)
		close(flight.done)
		hc.mu.Unlock()
	}()

	// Detach the shared revalidation from any single caller: one client
	// disconnecting must not cancel the result others await.
	req := clientReq.Clone(context.WithoutCancel(clientReq.Context()))
	req.RequestURI = ""
	if etag := variant.Header.Get("ETag"); etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lm := variant.Header.Get("Last-Modified"); lm != "" {
		req.Header.Set("If-Modified-Since", lm)
	}
	if !ctx.Proxy.KeepHeader {
		RemoveProxyHeaders(ctx, req)
	}

	resp, err := ctx.RoundTrip(req)
	if err != nil {
		flight.err = err
		return nil, err
	}

	switch resp.StatusCode {
	case http.StatusNotModified:
		_ = resp.Body.Close()
		now := hc.clock.Now()
		updated := merge304(variant, resp.Header, now)
		hc.replaceVariant(clientReq.URL.String(), resource, selector, updated)
		flight.body = bytes.Clone(updated.Body)
		flight.result = updated.toResponse(clientReq, updated.currentAge(now))
		return cloneResponse(flight.result, flight.body, clientReq), nil
	case http.StatusOK:
		materialized, stored, body, ok := hc.materializeNetwork(resp, req, hc.clock.Now())
		if ok {
			hc.replaceVariant(clientReq.URL.String(), resource, selector, stored)
		}
		flight.body = bytes.Clone(body)
		flight.result = materialized
		return cloneResponse(materialized, flight.body, clientReq), nil
	default:
		// e.g. 5xx: serve without touching stored metadata.
		materialized, _, body, _ := hc.materializeNetwork(resp, req, hc.clock.Now())
		flight.body = bytes.Clone(body)
		flight.result = materialized
		return cloneResponse(materialized, flight.body, clientReq), nil
	}
}

// awaitFlight waits for another goroutine's in-flight revalidation. The
// caller timeout or cancellation applies only to this waiter.
func (hc *httpCache) awaitFlight(r *http.Request, flight *revalidationFlight) (*http.Response, error) {
	timer := time.NewTimer(hc.cfg.RevalidationTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		if flight.err != nil {
			return nil, flight.err
		}
		return cloneResponse(flight.result, flight.body, r), nil
	case <-timer.C:
		return nil, errors.New("cache revalidation timed out")
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
}

// cloneResponse returns an independent response (own body and headers) for
// one caller, based on the shared flight result.
func cloneResponse(src *http.Response, body []byte, r *http.Request) *http.Response {
	return &http.Response{
		StatusCode:    src.StatusCode,
		Status:        src.Status,
		Proto:         src.Proto,
		ProtoMajor:    src.ProtoMajor,
		ProtoMinor:    src.ProtoMinor,
		Header:        src.Header.Clone(),
		Trailer:       cloneHeader(src.Trailer),
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(bytes.NewReader(bytes.Clone(body))),
		Request:       r,
	}
}

// replaceVariant stores one updated variant without disturbing the other
// Vary variants of the same URL.
func (hc *httpCache) replaceVariant(key string, resource *CachedResource, selector string, variant *CachedResponse) {
	updated := &CachedResource{
		Vary:     resource.Vary,
		Variants: make(map[string]*CachedResponse, len(resource.Variants)+1),
	}
	for k, v := range resource.Variants {
		updated.Variants[k] = v
	}
	updated.Variants[selector] = variant
	hc.store.Put(key, updated)
}

// materializeNetwork buffers a network response body (up to MaxBodyBytes)
// and returns the response to serve plus the stored variant when cacheable.
func (hc *httpCache) materializeNetwork(resp *http.Response, req *http.Request, responseTime time.Time) (*http.Response, *CachedResponse, []byte, bool) {
	body, err := readLimitedBody(resp.Body, hc.cfg.MaxBodyBytes)
	closeErr := resp.Body.Close()
	served := buildResponse(resp, req, body)
	if err != nil || closeErr != nil {
		return served, nil, body, false
	}
	names, cacheable := hc.responseCacheable(req, resp)
	if !cacheable {
		return served, nil, body, false
	}
	stored := hc.buildStored(resp, req, body, responseTime, names, resp.Trailer)
	return served, stored, body, true
}

func buildResponse(resp *http.Response, req *http.Request, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    resp.StatusCode,
		Status:        resp.Status,
		Proto:         resp.Proto,
		ProtoMajor:    resp.ProtoMajor,
		ProtoMinor:    resp.ProtoMinor,
		Header:        resp.Header.Clone(),
		Trailer:       cloneHeader(resp.Trailer),
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(bytes.NewReader(body)),
		Request:       req,
	}
}

// captureBody wraps the response body of a cache MISS so the bytes are
// buffered while being streamed to the client. The underlying body is
// closed exactly once; storage happens when the client finishes reading.
func (hc *httpCache) captureBody(resp *http.Response, base io.ReadCloser, req *http.Request, responseTime time.Time) io.ReadCloser {
	return &capturingBody{
		base:         base,
		buf:          bytes.NewBuffer(make([]byte, 0, 4096)),
		limit:        hc.cfg.MaxBodyBytes,
		hc:           hc,
		resp:         resp,
		req:          req,
		responseTime: responseTime,
	}
}

type capturingBody struct {
	base         io.ReadCloser
	buf          *bytes.Buffer
	limit        int64
	hc           *httpCache
	resp         *http.Response
	req          *http.Request
	responseTime time.Time
	tooLarge     bool
	readFailed   bool
	closed       bool
}

func (c *capturingBody) Read(p []byte) (int, error) {
	n, err := c.base.Read(p)
	if n > 0 && !c.tooLarge {
		if int64(c.buf.Len())+int64(n) > c.limit {
			c.tooLarge = true
		} else {
			c.buf.Write(p[:n])
		}
	}
	if err != nil && err != io.EOF {
		c.readFailed = true
	}
	return n, err
}

func (c *capturingBody) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.base.Close()
	if !c.tooLarge && !c.readFailed {
		c.hc.storeStreamed(c.resp, c.req, c.responseTime, c.buf.Bytes(), c.resp.Trailer)
	}
	return err
}

// storeStreamed stores a successfully streamed MISS response under its Vary
// variant. Responses declared uncacheable are ignored.
func (hc *httpCache) storeStreamed(resp *http.Response, req *http.Request, responseTime time.Time, body []byte, trailer http.Header) {
	names, ok := hc.responseCacheable(req, resp)
	if !ok {
		return
	}
	stored := hc.buildStored(resp, req, body, responseTime, names, trailer)
	if stored == nil {
		return
	}
	selector, _ := varySelector(names, req)
	hc.putVariant(req.URL.String(), names, selector, stored)
}

// buildStored constructs a CachedResponse from an already known Vary list.
func (hc *httpCache) buildStored(resp *http.Response, req *http.Request, body []byte, responseTime time.Time, names []string, trailer http.Header) *CachedResponse {
	_, reqHeaders := varySelector(names, req)
	date := parseDate(resp.Header, responseTime)
	lifetime, _ := freshnessLifetime(resp.Header, date)

	age := time.Duration(0)
	if v := resp.Header.Get("Age"); v != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && seconds >= 0 {
			age = time.Duration(seconds) * time.Second
		}
	}

	return &CachedResponse{
		StatusCode:        resp.StatusCode,
		Header:            sanitizeStoredHeader(resp.Header),
		Trailer:           cloneHeader(trailer),
		Body:              bytes.Clone(body),
		RequestHeaders:    reqHeaders,
		Date:              date,
		ResponseTime:      responseTime,
		AgeValue:          age,
		FreshnessLifetime: lifetime,
	}
}

// putVariant stores a variant, preserving the resource's other variants and
// keeping its Vary list consistent.
func (hc *httpCache) putVariant(key string, names []string, selector string, variant *CachedResponse) {
	resource := hc.store.Get(key)
	if resource == nil {
		resource = &CachedResource{
			Vary:     names,
			Variants: map[string]*CachedResponse{},
		}
	} else if !sameVary(resource.Vary, names) {
		// A new response changed the Vary contract: it starts a fresh
		// resource so variants are never merged across different keys.
		resource = &CachedResource{
			Vary:     names,
			Variants: map[string]*CachedResponse{},
		}
	}
	resource.Variants[selector] = variant
	hc.store.Put(key, resource)
}

func sameVary(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
