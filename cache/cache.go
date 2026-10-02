package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// Fetcher performs an upstream request for the cache.
type Fetcher func(ctx context.Context, req *http.Request) (*http.Response, error)

// Options configures a Cache. Build one with New.
type Options struct {
	// Store is the persistence layer. Defaults to a bounded MemoryStore.
	Store Store
	// MaxBytes bounds the default MemoryStore. Ignored when Store is set.
	MaxBytes int
	// MaxEntryBytes skips caching bodies larger than this. Zero uses
	// DefaultMaxEntryBytes.
	MaxEntryBytes int
	// Clock controls time. Defaults to SystemClock.
	Clock Clock
}

// DefaultMaxEntryBytes is the largest single response body the cache stores
// by default (8 MiB).
const DefaultMaxEntryBytes = 8 << 20

// Cache is a shared, conservative HTTP GET cache.
//
// It is safe for concurrent use. Concurrent callers needing the same stale
// representation are coalesced onto a single conditional revalidation; one
// caller cancelling its own request does not cancel the shared revalidation
// or deprive the other callers of its result.
type Cache struct {
	store         Store
	clock         Clock
	maxEntryBytes int

	flightsMu sync.Mutex
	flights   map[string]*flight
}

// New constructs a Cache.
func New(opts Options) *Cache {
	store := opts.Store
	if store == nil {
		store = NewMemoryStore(opts.MaxBytes)
	}
	clock := opts.Clock
	if clock == nil {
		clock = SystemClock{}
	}
	maxEntry := opts.MaxEntryBytes
	if maxEntry <= 0 {
		maxEntry = DefaultMaxEntryBytes
	}
	return &Cache{
		store:         store,
		clock:         clock,
		maxEntryBytes: maxEntry,
		flights:       make(map[string]*flight),
	}
}

// Store exposes the persistence layer (primarily for tests/tools).
func (c *Cache) Store() Store { return c.store }

// snapshot is the immutable result handed to every waiter.
type snapshot struct {
	statusCode int
	header     http.Header
	body       []byte
}

// flight represents one in-progress upstream call shared by callers. The
// upstream context is independent of any single caller, so cancelling one
// waiter cannot cancel the work for the others.
type flight struct {
	done    chan struct{}
	waiters int
	status  flightStatus
	snap    *snapshot
	err     error
}

type flightStatus int

const (
	flightPending flightStatus = iota
	flightNotModified
	flightFull
)

// Transport returns an http.RoundTripper that serves cacheable GETs from c
// and delegates everything else to base.
func (c *Cache) Transport(base http.RoundTripper) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return c.RoundTrip(req, func(ctx context.Context, r *http.Request) (*http.Response, error) {
			return base.RoundTrip(r.WithContext(ctx))
		})
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// RoundTrip serves a request using the cache. fetch performs the upstream
// call and is given a cache-owned context during revalidation.
func (c *Cache) RoundTrip(req *http.Request, fetch Fetcher) (*http.Response, error) {
	if !requestMayUseCache(req) {
		return fetch(req.Context(), req)
	}

	key := primaryKey(req)
	now := c.clock.Now()

	entry, _ := c.store.GetEntry(key)
	variant := selectVariant(entry, req.Header)

	if variant != nil {
		if !requestRevalidate(req.Header) && variant.fresh(now) &&
			requestMaxAgeOK(req.Header, variant.age(now)) {
			if resp, ok := c.buildResponse(variant, req, now); ok {
				return resp, nil
			}
		}
		return c.revalidate(req, fetch, key, variant)
	}

	return c.fetchAndStore(req, fetch, key)
}

// requestMaxAgeOK enforces a client Cache-Control: max-age=... limit.
func requestMaxAgeOK(reqHeader http.Header, age time.Duration) bool {
	d := parseCacheControl(reqHeader)
	maxAge, present := d.seconds("max-age")
	if !present {
		return true
	}
	return age <= maxAge
}

// revalidate joins or starts a shared conditional request for one variant.
func (c *Cache) revalidate(req *http.Request, fetch Fetcher, key string, stale *Variant) (*http.Response, error) {
	flightKey := key + "\x00" + stale.VaryKey

	f, isLeader := c.joinFlight(flightKey)
	if isLeader {
		go c.runRevalidation(f, flightKey, req, fetch, key, stale)
	}
	return c.await(req.Context(), f, stale)
}

func (c *Cache) joinFlight(flightKey string) (*flight, bool) {
	c.flightsMu.Lock()
	defer c.flightsMu.Unlock()
	if f, ok := c.flights[flightKey]; ok {
		f.waiters++
		return f, false
	}
	f := &flight{done: make(chan struct{}), waiters: 1}
	c.flights[flightKey] = f
	return f, true
}

// await waits for the shared outcome but returns promptly when this caller's
// own context ends; it never cancels the shared upstream work.
func (c *Cache) await(ctx context.Context, f *flight, stale *Variant) (*http.Response, error) {
	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		switch f.status {
		case flightNotModified:
			// The stored variant was refreshed; rebuild from the updated
			// metadata/body in the store.
			if refreshed := c.lookupVariant(stale); refreshed != nil {
				if resp, ok := c.buildResponse(refreshed, nil, c.clock.Now()); ok {
					return resp, nil
				}
			}
			return nil, errors.New("cache: revalidated variant unavailable")
		case flightFull:
			return snapshotResponse(f.snap, nil), nil
		}
		return nil, errors.New("cache: invalid revalidation state")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// lookupVariant re-reads the refreshed variant from the store using the
// stale variant's coordinate (primary key + vary key).
func (c *Cache) lookupVariant(stale *Variant) *Variant {
	entry, ok := c.store.GetEntry(stale.PrimaryKey)
	if !ok {
		return nil
	}
	for _, v := range entry.Variants {
		if v.VaryKey == stale.VaryKey {
			return v
		}
	}
	return nil
}

// runRevalidation performs the detached upstream conditional request and
// publishes exactly one outcome for all waiters.
func (c *Cache) runRevalidation(f *flight, flightKey string, req *http.Request, fetch Fetcher, key string, stale *Variant) {
	defer func() {
		close(f.done)
		c.flightsMu.Lock()
		delete(c.flights, flightKey)
		c.flightsMu.Unlock()
	}()

	cond := cloneRequestForValidation(req, stale)

	// A detached context keeps the shared revalidation alive even after an
	// individual waiter gives up.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resp, err := fetch(ctx, cond)
	if err != nil {
		f.err = err
		return
	}

	switch resp.StatusCode {
	case http.StatusNotModified:
		drainAndClose(resp.Body)
		if err := c.applyNotModified(key, stale, resp, c.clock.Now()); err != nil {
			f.err = err
			return
		}
		f.status = flightNotModified
	case http.StatusOK:
		snap, _, err := c.bufferAndMaybeStore(req, key, resp)
		if err != nil {
			f.err = err
			return
		}
		f.status = flightFull
		f.snap = snap
	default:
		// Any other status is passed through unbuffered; waiters receive a
		// best-effort response but the body is a single stream. v1 only
		// revalidates against caches that yield 304/200; other codes are
		// rare here. Read the (typically empty) body to allow reuse.
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		f.status = flightFull
		f.snap = &snapshot{statusCode: resp.StatusCode, header: resp.Header.Clone(), body: body}
	}
}

func drainAndClose(b io.ReadCloser) {
	if b == nil {
		return
	}
	_, _ = io.Copy(io.Discard, b)
	_ = b.Close()
}

// fetchAndStore performs an unconditional miss and stores the response when
// policy allows.
func (c *Cache) fetchAndStore(req *http.Request, fetch Fetcher, key string) (*http.Response, error) {
	requestTime := c.clock.Now()
	resp, err := fetch(req.Context(), req)
	if err != nil {
		return nil, err
	}
	responseTime := c.clock.Now()

	if !responseStorePolicy(req.Header, resp.Header, resp.StatusCode).store {
		return resp, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxEntryBytes)+1))
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	if err := resp.Body.Close(); err != nil {
		return nil, err
	}
	if len(body) > c.maxEntryBytes {
		// Too large: do not store, but still return the buffered body.
		return snapshotResponse(&snapshot{
			statusCode: resp.StatusCode,
			header:     resp.Header.Clone(),
			body:       body,
		}, req), nil
	}

	c.writeEntry(key, req, resp.Header, resp.StatusCode, body, requestTime, responseTime)

	return snapshotResponse(&snapshot{
		statusCode: resp.StatusCode,
		header:     resp.Header.Clone(),
		body:       body,
	}, req), nil
}

// bufferAndMaybeStore buffers a 200 from revalidation and stores it using
// the miss policy, returning the snapshot to hand to waiters.
func (c *Cache) bufferAndMaybeStore(req *http.Request, key string, resp *http.Response) (*snapshot, bool, error) {
	requestTime := c.clock.Now()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxEntryBytes)+1))
	if err != nil {
		_ = resp.Body.Close()
		return nil, false, err
	}
	if err := resp.Body.Close(); err != nil {
		return nil, false, err
	}
	snap := &snapshot{statusCode: resp.StatusCode, header: resp.Header.Clone(), body: body}
	if len(body) <= c.maxEntryBytes && responseStorePolicy(req.Header, resp.Header, resp.StatusCode).store {
		c.writeEntry(key, req, resp.Header, resp.StatusCode, body, requestTime, c.clock.Now())
		return snap, true, nil
	}
	return snap, false, nil
}

func snapshotResponse(s *snapshot, req *http.Request) *http.Response {
	header := http.Header{}
	if s.header != nil {
		header = s.header.Clone()
	}
	return &http.Response{
		StatusCode:    s.statusCode,
		Status:        http.StatusText(s.statusCode),
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(s.body)),
		ContentLength: int64(len(s.body)),
		Request:       req,
	}
}
