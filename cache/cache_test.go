package cache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return string(b)
}

func mustGet(t *testing.T, c *Cache, req *http.Request) *http.Response {
	t.Helper()
	resp, err := c.RoundTrip(req, func(ctx context.Context, r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected fetch for %v", r.URL)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("cached RoundTrip: %v", err)
	}
	return resp
}

// TestBilingual304UpdatesOnlyEnglish is the flagship scenario: the same URL
// has both an English and a Chinese representation selected by Vary. When the
// English entry expires and revalidation yields 304, only the English
// metadata/body relationship changes; the Chinese entry is untouched.
func TestBilingual304UpdatesOnlyEnglish(t *testing.T) {
	clock := NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := New(Options{Clock: clock})

	tsURL := "http://example.com/greet"

	var originHits int
	englishEtag := `"en-v1"`
	chineseEtag := `"zh-v1"`

	fetch := func(ctx context.Context, r *http.Request) (*http.Response, error) {
		originHits++
		// 304 branch for the conditional English revalidation.
		if inm := r.Header.Get("If-None-Match"); inm == englishEtag {
			resp := httptest.NewRecorder().Result()
			resp.StatusCode = http.StatusNotModified
			resp.Status = "304 Not Modified"
			resp.Body = io.NopCloser(emptyReader{})
			resp.Header = http.Header{}
			resp.Header.Set("ETag", englishEtag)
			resp.Header.Set("Cache-Control", "max-age=100")
			resp.Header.Set("Vary", "Accept-Language")
			return resp, nil
		}

		lang := r.Header.Get("Accept-Language")
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{},
			Body:       io.NopCloser(nil),
			Request:    r,
		}
		resp.Header.Set("Cache-Control", "max-age=60")
		resp.Header.Set("Vary", "Accept-Language")
		if lang == "zh" {
			resp.Header.Set("ETag", chineseEtag)
			resp.Header.Set("X-Lang", "chinese")
			resp.Body = io.NopCloser(stringReader("你好"))
		} else {
			resp.Header.Set("ETag", englishEtag)
			resp.Header.Set("X-Lang", "english")
			resp.Body = io.NopCloser(stringReader("hello"))
		}
		return resp, nil
	}

	enReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, tsURL, nil)
		req.Header.Set("Accept-Language", "en")
		return req
	}
	zhReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, tsURL, nil)
		req.Header.Set("Accept-Language", "zh")
		return req
	}

	// Prime both variants from origin.
	if got := readBody(t, mustFetch(t, c, enReq(), fetch)); got != "hello" {
		t.Fatalf("english prime = %q", got)
	}
	if got := readBody(t, mustFetch(t, c, zhReq(), fetch)); got != "你好" {
		t.Fatalf("chinese prime = %q", got)
	}

	key := "GET http://example.com/greet"
	entry, ok := c.store.GetEntry(key)
	if !ok {
		t.Fatal("entry not stored")
	}
	if len(entry.Variants) != 2 {
		t.Fatalf("variants = %d, want 2", len(entry.Variants))
	}
	enBefore := variantFor(entry, "Accept-Language:en")
	zhBefore := variantFor(entry, "Accept-Language:zh")
	if enBefore == nil || zhBefore == nil {
		t.Fatal("missing variant")
	}
	enExpiresBefore := enBefore.ExpiresAt

	// Both are served from cache while fresh.
	if got := readBody(t, mustGet(t, c, enReq())); got != "hello" {
		t.Fatalf("english cached = %q", got)
	}

	// Advance beyond English freshness; Chinese was stored at the same time
	// and also becomes stale, but we only revalidate English.
	clock.Advance(61 * time.Second)

	if got := readBody(t, mustFetch(t, c, enReq(), fetch)); got != "hello" {
		t.Fatalf("english after 304 = %q", got)
	}

	entry2, _ := c.store.GetEntry(key)
	enAfter := variantFor(entry2, "Accept-Language:en")
	zhAfter := variantFor(entry2, "Accept-Language:zh")

	// English metadata refreshed: expiry extended and body relationship
	// retained (304 carried no body).
	if !enAfter.ExpiresAt.After(enExpiresBefore) {
		t.Fatalf("english expiry not extended: %v -> %v", enExpiresBefore, enAfter.ExpiresAt)
	}
	if enAfter.BodyID != enBefore.BodyID {
		t.Fatalf("english body changed on 304: %q -> %q", enBefore.BodyID, enAfter.BodyID)
	}

	// Chinese metadata and body relationship are byte-for-byte unchanged.
	if zhAfter.BodyID != zhBefore.BodyID {
		t.Fatalf("chinese body changed: %q -> %q", zhBefore.BodyID, zhAfter.BodyID)
	}
	if !zhAfter.ExpiresAt.Equal(zhBefore.ExpiresAt) ||
		zhAfter.ResponseTime != zhBefore.ResponseTime ||
		zhAfter.Header.Get("X-Lang") != "chinese" {
		t.Fatal("chinese metadata was mutated by english revalidation")
	}

	// English body still served from the retained body store entry.
	if got := readBody(t, mustGet(t, c, enReq())); got != "hello" {
		t.Fatalf("english final = %q", got)
	}
	if got := readBody(t, mustFetch(t, c, zhReq(), fetch)); got != "你好" {
		t.Fatalf("chinese revalidated separately = %q", got)
	}
}

func variantFor(entry *Entry, key string) *Variant {
	for _, v := range entry.Variants {
		if v.VaryKey == key {
			return v
		}
	}
	return nil
}

func mustFetch(t *testing.T, c *Cache, req *http.Request, fetch Fetcher) *http.Response {
	t.Helper()
	resp, err := c.RoundTrip(req, fetch)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	return resp
}

type stringReader string

func (s stringReader) Read(p []byte) (int, error) {
	n := copy(p, s)
	s = s[n:]
	if len(s) == 0 {
		return n, io.EOF
	}
	return n, nil
}

type emptyReader struct{}

func (emptyReader) Read(p []byte) (int, error) { return 0, io.EOF }
