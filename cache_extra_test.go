package goproxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/stretchr/testify/assert"
)

// TestCacheStaleWithoutValidatorFetches verifies a stored response with no
// validator is replaced via a plain fetch once stale, never a conditional.
func TestCacheStaleWithoutValidatorFetches(t *testing.T) {
	var hits int32
	var sawConditional int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			atomic.AddInt32(&sawConditional, 1)
		}
		w.Header().Set("Cache-Control", "max-age=0")
		_, _ = io.WriteString(w, "plain")
	}))
	defer origin.Close()

	_, clock, client, _ := newCachingProxy(t, goproxy.CacheConfig{})
	doGet(t, client, origin.URL+"/nov")
	clock.Advance(time.Second)
	resp := doGet(t, client, origin.URL+"/nov")
	assert.Equal(t, "plain", readBody(t, resp))
	assert.Equal(t, int32(2), atomic.LoadInt32(&hits))
	assert.Equal(t, int32(0), atomic.LoadInt32(&sawConditional))
}

// TestCacheAllWaitersReceiveFullBody ensures every singleflight waiter and
// the owner get an independent, complete response body.
func TestCacheAllWaitersReceiveFullBody(t *testing.T) {
	release := make(chan struct{})
	payload := strings.Repeat("BODY", 2048)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			<-release
			w.Header().Set("Cache-Control", "max-age=60")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Cache-Control", "max-age=0")
		w.Header().Set("ETag", `"big"`)
		_, _ = io.WriteString(w, payload)
	}))
	defer origin.Close()

	_, clock, client, _ := newCachingProxy(t, goproxy.CacheConfig{})
	doGet(t, client, origin.URL+"/big")
	clock.Advance(time.Second)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			resp, err := client.Get(origin.URL + "/big")
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				errs <- err
				return
			}
			if string(b) != payload {
				errs <- &bodyError{got: len(b), want: len(payload)}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
}

type bodyError struct {
	got, want int
}

func (e *bodyError) Error() string {
	return "body length " + strconv.Itoa(e.got) + ", want " + strconv.Itoa(e.want)
}

// TestCacheLastModifiedRevalidation covers the Last-Modified validator in
// addition to ETag, and verifies the conditional request header.
func TestCacheLastModifiedRevalidation(t *testing.T) {
	lastMod := "Mon, 02 Jan 2006 00:00:00 GMT"
	var gotIMS string
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if ims := r.Header.Get("If-Modified-Since"); ims != "" {
			gotIMS = ims
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Cache-Control", "max-age=0")
		w.Header().Set("Last-Modified", lastMod)
		_, _ = io.WriteString(w, "lm")
	}))
	defer origin.Close()

	_, clock, client, _ := newCachingProxy(t, goproxy.CacheConfig{})
	doGet(t, client, origin.URL+"/lm")
	clock.Advance(time.Second)
	resp := doGet(t, client, origin.URL+"/lm")
	assert.Equal(t, "lm", readBody(t, resp))
	assert.Equal(t, lastMod, gotIMS)
	assert.Equal(t, int32(2), atomic.LoadInt32(&hits))
}

// TestCacheNoCacheAlwaysRevalidates verifies Cache-Control: no-cache forces
// revalidation on every request even when a body is cached.
func TestCacheNoCacheAlwaysRevalidates(t *testing.T) {
	var conditional int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			atomic.AddInt32(&conditional, 1)
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("ETag", `"n"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", `"n"`)
		_, _ = io.WriteString(w, "nc")
	}))
	defer origin.Close()

	_, _, client, _ := newCachingProxy(t, goproxy.CacheConfig{})
	doGet(t, client, origin.URL+"/nc")
	doGet(t, client, origin.URL+"/nc")
	doGet(t, client, origin.URL+"/nc")
	assert.Equal(t, int32(2), atomic.LoadInt32(&conditional))
}

// TestCacheMustRevalidateFreshUsable verifies must-revalidate is served
// while fresh and revalidated once stale (never silently served stale).
func TestCacheMustRevalidateFreshUsable(t *testing.T) {
	var conditional int32
	var total int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&total, 1)
		if r.Header.Get("If-None-Match") != "" {
			atomic.AddInt32(&conditional, 1)
			w.Header().Set("Cache-Control", "must-revalidate, max-age=60")
			w.Header().Set("ETag", `"m"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Cache-Control", "must-revalidate, max-age=60")
		w.Header().Set("ETag", `"m"`)
		_, _ = io.WriteString(w, "mr")
	}))
	defer origin.Close()

	_, clock, client, _ := newCachingProxy(t, goproxy.CacheConfig{})
	doGet(t, client, origin.URL+"/mr")
	clock.Advance(10 * time.Second)
	doGet(t, client, origin.URL+"/mr") // fresh: direct HIT
	assert.Equal(t, int32(1), atomic.LoadInt32(&total))
	clock.Advance(51 * time.Second)
	doGet(t, client, origin.URL+"/mr") // stale: conditional revalidation
	assert.Equal(t, int32(1), atomic.LoadInt32(&conditional))
	assert.Equal(t, "mr", readBody(t, doGet(t, client, origin.URL+"/mr")))
}

// TestCacheMaxBodyBytes ensures bodies over the limit are still served but
// never stored.
func TestCacheMaxBodyBytes(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer origin.Close()

	store := goproxy.NewMemoryStore(10)
	_, _, client, _ := newCachingProxy(t, goproxy.CacheConfig{Store: store, MaxBodyBytes: 1024})

	resp := doGet(t, client, origin.URL+"/bigbody")
	body, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Len(t, body, 4096)
	assert.Nil(t, store.Get(origin.URL+"/bigbody"))
}

// TestCacheExpiresHeader verifies Expires-based freshness without
// Cache-Control.
func TestCacheExpiresHeader(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Date", "Mon, 02 Jan 2006 00:00:00 GMT")
		// The origin clock is fixed by its own Date header; set Expires
		// relative to it and rely on the proxy clock for staleness.
		w.Header().Set("Expires", "Mon, 02 Jan 2006 00:00:30 GMT")
		_, _ = io.WriteString(w, "exp")
	}))
	defer origin.Close()

	// Anchor the proxy clock to the origin Date so time travel is
	// deterministic despite the server writing a fixed Date.
	clock := goproxy.NewManualClock(time.Date(2006, 1, 2, 0, 0, 0, 0, time.UTC))
	proxy := goproxy.NewProxyHttpServer()
	proxy.Cache = &goproxy.CacheConfig{Clock: clock}
	ps := httptest.NewServer(proxy)
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}

	doGet(t, client, origin.URL+"/exp")
	clock.Advance(10 * time.Second)
	doGet(t, client, origin.URL+"/exp")
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits))
	clock.Advance(21 * time.Second)
	// Expired without validator: plain fetch, no conditional request.
	doGet(t, client, origin.URL+"/exp")
	assert.Equal(t, int32(2), atomic.LoadInt32(&hits))
}
