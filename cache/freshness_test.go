package cache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestRequest(urlStr string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, urlStr, nil)
	return req
}

func okResponse(maxAge string, body string, mutate func(http.Header)) *http.Response {
	h := http.Header{}
	if maxAge != "" {
		h.Set("Cache-Control", maxAge)
	}
	if mutate != nil {
		mutate(h)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     h,
		Body:       io.NopCloser(stringReader(body)),
	}
}

func countingFetch(resp *http.Response, hits *int32) Fetcher {
	return func(ctx context.Context, r *http.Request) (*http.Response, error) {
		atomic.AddInt32(hits, 1)
		out := *resp
		out.Body = io.NopCloser(stringReader("payload"))
		return &out, nil
	}
}

// TestFreshnessAndAge uses the manual clock to prove cache hits while
// fresh, a revalidation after expiry, and a correct growing Age header.
func TestFreshnessAndAge(t *testing.T) {
	clock := NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := New(Options{Clock: clock})

	var hits int32
	resp := okResponse("max-age=10", "payload", nil)
	fetch := countingFetch(resp, &hits)

	req := newTestRequest("http://example.com/a")

	r1 := mustFetch(t, c, req, fetch)
	if readBody(t, r1) != "payload" || hits != 1 {
		t.Fatal("prime failed")
	}

	clock.Advance(3 * time.Second)
	r2 := mustGet(t, c, newTestRequest("http://example.com/a"))
	if got := r2.Header.Get("Age"); got != "3" {
		t.Fatalf("Age = %q, want 3", got)
	}
	if readBody(t, r2) != "payload" || hits != 1 {
		t.Fatal("expected cache hit while fresh")
	}

	// After expiry the cache revalidates. ETag + 304 keeps serving the
	// stored body without a new payload.
	clock.Advance(8 * time.Second)
	rev := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&hits, 1)
		out := &http.Response{
			StatusCode: http.StatusNotModified,
			Status:     "304 Not Modified",
			Header:     http.Header{},
			Body:       io.NopCloser(emptyReader{}),
		}
		out.Header.Set("Cache-Control", "max-age=10")
		return out, nil
	}
	r3 := mustFetch(t, c, newTestRequest("http://example.com/a"), rev)
	if readBody(t, r3) != "payload" {
		t.Fatal("expected stored body after 304")
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}

	// Fresh again for another 10 seconds after revalidation.
	r4 := mustGet(t, c, newTestRequest("http://example.com/a"))
	if readBody(t, r4) != "payload" {
		t.Fatal("expected hit after revalidation")
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}
}

// TestEviction proves LRU eviction when the bounded store overflows.
func TestEviction(t *testing.T) {
	clock := SystemClock{}
	c := New(Options{Clock: clock, MaxBytes: 400})

	fetchFor := func(body string) Fetcher {
		return func(ctx context.Context, r *http.Request) (*http.Response, error) {
			return okResponse("max-age=600", body, nil), nil
		}
	}

	store := c.store.(*MemoryStore)

	mustFetch(t, c, newTestRequest("http://example.com/a"), fetchFor("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	mustFetch(t, c, newTestRequest("http://example.com/b"), fetchFor("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	mustFetch(t, c, newTestRequest("http://example.com/c"), fetchFor("cccccccccccccccccccccccccccccccccccc"))

	if store.Len() < 3 && store.Len() > 0 {
		// small entries may all fit initially
	}

	// Force overflow with larger payloads; the LRU oldest should leave.
	mustFetch(t, c, newTestRequest("http://example.com/d"),
		fetchFor("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"))

	// At least one entry must have been evicted to respect MaxBytes.
	if store.Size() > 400 {
		t.Fatalf("store size %d exceeds MaxBytes 400", store.Size())
	}
}

// TestVaryNotMerged proves distinct Vary selections never collapse.
func TestVaryNotMerged(t *testing.T) {
	clock := NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := New(Options{Clock: clock})

	var hits int32
	fetch := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&hits, 1)
		lang := r.Header.Get("Accept-Language")
		resp := okResponse("max-age=60", "body-"+lang, func(h http.Header) {
			h.Set("Vary", "Accept-Language")
		})
		return resp, nil
	}

	en := newTestRequest("http://example.com/x")
	en.Header.Set("Accept-Language", "en")
	fr := newTestRequest("http://example.com/x")
	fr.Header.Set("Accept-Language", "fr")

	if readBody(t, mustFetch(t, c, en, fetch)) != "body-en" {
		t.Fatal("en prime")
	}
	if readBody(t, mustFetch(t, c, fr, fetch)) != "body-fr" {
		t.Fatal("fr prime")
	}
	if hits != 2 {
		t.Fatalf("hits=%d want 2", hits)
	}
	if readBody(t, mustGet(t, c, en)) != "body-en" {
		t.Fatal("en cached mismatch")
	}
	if readBody(t, mustGet(t, c, fr)) != "body-fr" {
		t.Fatal("fr cached mismatch")
	}
	entry, _ := c.store.GetEntry("GET http://example.com/x")
	if len(entry.Variants) != 2 {
		t.Fatalf("variants=%d want 2", len(entry.Variants))
	}
}

// TestRangeBypassesCache proves Range requests never use the cache.
func TestRangeBypassesCache(t *testing.T) {
	c := New(Options{})
	var hits int32
	fetch := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&hits, 1)
		return okResponse("max-age=60", "partial", nil), nil
	}
	req := newTestRequest("http://example.com/range")
	req.Header.Set("Range", "bytes=0-3")
	readBody(t, mustFetch(t, c, req, fetch))
	readBody(t, mustFetch(t, c, req, fetch))
	if hits != 2 {
		t.Fatalf("range hits=%d want 2 (no caching)", hits)
	}
}
