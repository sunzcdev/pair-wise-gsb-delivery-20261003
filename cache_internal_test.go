package goproxy

import (
	"net/http"
	"testing"
	"time"
)

func TestMerge304KeepsBodyAndBodyHeaders(t *testing.T) {
	stored := &CachedResponse{
		StatusCode:        http.StatusOK,
		Header:            http.Header{},
		Body:              []byte("english body"),
		Date:              time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ResponseTime:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		FreshnessLifetime: time.Second,
	}
	stored.Header.Set("Content-Type", "text/plain")
	stored.Header.Set("Content-Length", "12")
	stored.Header.Set("X-Stamp", "en-1")
	stored.Header.Set("Cache-Control", "max-age=1")
	stored.Header.Set("ETag", `"en"`)

	h := http.Header{}
	h.Set("Cache-Control", "max-age=100")
	h.Set("X-Stamp", "en-2")
	h.Set("Date", time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC).Format(http.TimeFormat))

	now := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	updated := merge304(stored, h, now)

	if string(updated.Body) != "english body" {
		t.Fatalf("body changed: %q", updated.Body)
	}
	if got := updated.Header.Get("Content-Length"); got != "12" {
		t.Fatalf("Content-Length changed: %q", got)
	}
	if got := updated.Header.Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type changed: %q", got)
	}
	if got := updated.Header.Get("X-Stamp"); got != "en-2" {
		t.Fatalf("X-Stamp not refreshed: %q", got)
	}
	if got := updated.Header.Get("Cache-Control"); got != "max-age=100" {
		t.Fatalf("Cache-Control not refreshed: %q", got)
	}
	if updated.FreshnessLifetime != 100*time.Second {
		t.Fatalf("freshness not refreshed: %v", updated.FreshnessLifetime)
	}
	// Stored input must remain untouched.
	if got := stored.Header.Get("X-Stamp"); got != "en-1" {
		t.Fatalf("stored variant mutated: %q", got)
	}
}

func TestFreshnessLifetimePrecedence(t *testing.T) {
	date := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("Cache-Control", "max-age=10, s-maxage=30")
	h.Set("Expires", date.Add(20*time.Second).Format(http.TimeFormat))
	lifetime, explicit := freshnessLifetime(h, date)
	if !explicit || lifetime != 30*time.Second {
		t.Fatalf("s-maxage must win: %v explicit=%v", lifetime, explicit)
	}

	h.Del("Cache-Control")
	lifetime, explicit = freshnessLifetime(h, date)
	if !explicit || lifetime != 20*time.Second {
		t.Fatalf("Expires lifetime wrong: %v explicit=%v", lifetime, explicit)
	}

	h.Del("Expires")
	lifetime, explicit = freshnessLifetime(h, date)
	if explicit || lifetime != 0 {
		t.Fatalf("missing lifetime should be zero/non-explicit: %v explicit=%v", lifetime, explicit)
	}
}

func TestVarySelectorIsolation(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://x/g", nil)
	req.Header.Set("Accept-Language", "en")
	en, enHeaders := varySelector([]string{"Accept-Language"}, req)

	req2, _ := http.NewRequest("GET", "http://x/g", nil)
	req2.Header.Set("Accept-Language", "zh")
	zh, zhHeaders := varySelector([]string{"Accept-Language"}, req2)

	if en == zh {
		t.Fatal("different Vary selectors collapsed")
	}
	if enHeaders.Get("Accept-Language") != "en" || zhHeaders.Get("Accept-Language") != "zh" {
		t.Fatalf("request header snapshots wrong: %v %v", enHeaders, zhHeaders)
	}

	empty, headers := varySelector(nil, req)
	if empty != "" || headers != nil {
		t.Fatalf("no Vary must yield empty selector")
	}
}

func TestVaryWildcardUncacheable(t *testing.T) {
	hc := newHTTPCache(CacheConfig{})
	req, _ := http.NewRequest("GET", "http://x/", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	resp.Header.Set("Cache-Control", "max-age=60")
	resp.Header.Set("Vary", "*")
	if _, ok := hc.responseCacheable(req, resp); ok {
		t.Fatal("Vary: * must be uncacheable")
	}
}
