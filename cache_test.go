package goproxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCachingProxy builds a proxy with caching enabled and a controllable
// clock, returning the clock and the running proxy server's client.
func newCachingProxy(t *testing.T, cfg goproxy.CacheConfig) (*goproxy.ProxyHttpServer, *goproxy.ManualClock, *http.Client, *httptest.Server) {
	t.Helper()
	clock := goproxy.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg.Clock = clock
	proxy := goproxy.NewProxyHttpServer()
	proxy.Cache = &cfg
	ps := httptest.NewServer(proxy)
	t.Cleanup(ps.Close)
	proxyURL, err := url.Parse(ps.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	return proxy, clock, client, ps
}

func doGet(t *testing.T, client *http.Client, target string, headers ...[2]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	require.NoError(t, err)
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

// recordingStore wraps the default store and counts Put operations per
// selector for direct assertions on stored variants.
type recordingStore struct {
	goproxy.CacheStore
	mu      sync.Mutex
	puts    int
	deletes int
}

func (s *recordingStore) Put(key string, resource *goproxy.CachedResource) {
	s.mu.Lock()
	s.puts++
	s.mu.Unlock()
	s.CacheStore.Put(key, resource)
}

func (s *recordingStore) Delete(key string) {
	s.mu.Lock()
	s.deletes++
	s.mu.Unlock()
	s.CacheStore.Delete(key)
}

func (s *recordingStore) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

// TestCacheDisabledByDefault verifies that without proxy.Cache every
// request reaches the origin.
func TestCacheDisabledByDefault(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Cache-Control", "max-age=60")
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()

	ps := httptest.NewServer(goproxy.NewProxyHttpServer())
	defer ps.Close()
	proxyURL, _ := url.Parse(ps.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	for i := 0; i < 3; i++ {
		resp := doGet(t, client, origin.URL+"/a")
		assert.Equal(t, "ok", readBody(t, resp))
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&hits))
}

// TestCacheFreshAndAge covers a HIT served from cache, the Age header, and
// stale detection under a controllable clock.
func TestCacheFreshAndAge(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, "hello")
	}))
	defer origin.Close()

	_, clock, client, _ := newCachingProxy(t, goproxy.CacheConfig{})

	first := doGet(t, client, origin.URL+"/doc")
	assert.Equal(t, "hello", readBody(t, first))
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits))

	clock.Advance(10 * time.Second)
	second := doGet(t, client, origin.URL+"/doc")
	assert.Equal(t, "hello", readBody(t, second))
	assert.Equal(t, "10", second.Header.Get("Age"))
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits))

	// Advance past freshness: a stale entry triggers revalidation.
	clock.Advance(51 * time.Second)
	third := doGet(t, client, origin.URL+"/doc")
	assert.Equal(t, "hello", readBody(t, third))
	assert.Equal(t, int32(2), atomic.LoadInt32(&hits))
	assert.Contains(t, []string{"", "0"}, third.Header.Get("Age"))
}

// TestCacheETagRevalidation304 verifies that a stale entry sends
// If-None-Match and, on 304, reuses the stored body while refreshing Age.
func TestCacheETagRevalidation304(t *testing.T) {
	var hits int32
	var gotConditional string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if inm := r.Header.Get("If-None-Match"); inm != "" {
			gotConditional = inm
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Cache-Control", "max-age=1")
		w.Header().Set("ETag", `"e"`)
		_, _ = io.WriteString(w, "payload")
	}))
	defer origin.Close()

	_, clock, client, _ := newCachingProxy(t, goproxy.CacheConfig{})

	first := doGet(t, client, origin.URL+"/r")
	assert.Equal(t, "payload", readBody(t, first))

	clock.Advance(2 * time.Second)
	second := doGet(t, client, origin.URL+"/r")
	assert.Equal(t, "payload", readBody(t, second))
	assert.Equal(t, `"e"`, gotConditional)
	assert.Equal(t, int32(2), atomic.LoadInt32(&hits))
}

// TestVaryEnglishChinese304UpdatesOnlyEnglish is the anchor scenario: one
// URL has Chinese and English Vary variants. When the English variant goes
// stale and the origin answers 304, only English metadata is refreshed;
// the Chinese variant (body and metadata) is untouched.
func TestVaryEnglishChinese304UpdatesOnlyEnglish(t *testing.T) {
	store := &recordingStore{CacheStore: goproxy.NewMemoryStore(10)}
	var mu sync.Mutex
	chineseMeta := "X-Stamp: zh-1"
	englishMeta := "X-Stamp: en-1"
	revalidations := make(chan string, 8)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := r.Header.Get("Accept-Language")
		mu.Lock()
		stamp := chineseMeta
		body := "你好"
		if strings.HasPrefix(lang, "en") {
			stamp = englishMeta
			body = "hello"
		}
		mu.Unlock()

		w.Header().Set("Vary", "Accept-Language")
		w.Header().Set("Cache-Control", "max-age=0")
		w.Header().Set("ETag", `"`+lang+`"`)
		w.Header().Set(strings.SplitN(stamp, ": ", 2)[0], strings.SplitN(stamp, ": ", 2)[1])

		if r.Header.Get("If-None-Match") != "" {
			revalidations <- r.Header.Get("If-None-Match")
			// Refresh metadata for the revalidated language.
			mu.Lock()
			if strings.HasPrefix(lang, "en") {
				englishMeta = "X-Stamp: en-2"
			} else {
				chineseMeta = "X-Stamp: zh-2"
			}
			w.Header().Set("X-Stamp", strings.SplitN(
				map[bool]string{true: englishMeta, false: chineseMeta}[strings.HasPrefix(lang, "en")],
				": ", 2)[1])
			mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer origin.Close()

	_, clock, client, _ := newCachingProxy(t, goproxy.CacheConfig{Store: store})

	zh1 := doGet(t, client, origin.URL+"/greet", [2]string{"Accept-Language", "zh"})
	assert.Equal(t, "你好", readBody(t, zh1))
	en1 := doGet(t, client, origin.URL+"/greet", [2]string{"Accept-Language", "en"})
	assert.Equal(t, "hello", readBody(t, en1))

	// Both variants coexist independently in the resource.
	resource := store.Get(origin.URL + "/greet")
	require.NotNil(t, resource)
	assert.Len(t, resource.Variants, 2)

	clock.Advance(time.Second)

	// Revalidate English only.
	en2 := doGet(t, client, origin.URL+"/greet", [2]string{"Accept-Language", "en"})
	assert.Equal(t, "hello", readBody(t, en2))
	assert.Equal(t, "en-2", en2.Header.Get("X-Stamp"))
	select {
	case etag := <-revalidations:
		assert.Equal(t, `"en"`, etag)
	case <-time.After(time.Second):
		t.Fatal("expected English 304 revalidation")
	}

	resource = store.Get(origin.URL + "/greet")
	require.NotNil(t, resource)
	assert.Len(t, resource.Variants, 2)

	// The Chinese variant must remain on its original metadata and body.
	var zhVariant, enVariant *goproxy.CachedResponse
	for _, v := range resource.Variants {
		if string(v.Body) == "你好" {
			zhVariant = v
		}
		if string(v.Body) == "hello" {
			enVariant = v
		}
	}
	require.NotNil(t, zhVariant)
	require.NotNil(t, enVariant)
	assert.Equal(t, "zh-1", zhVariant.Header.Get("X-Stamp"))
	assert.Equal(t, "en-2", enVariant.Header.Get("X-Stamp"))
	assert.Equal(t, []byte("你好"), zhVariant.Body)
	assert.Equal(t, []byte("hello"), enVariant.Body)
}

// TestCacheLRUEviction bounds the store and verifies the least recently
// used resource is evicted first, using the controllable clock only for
// determinism.
func TestCacheLRUEviction(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	defer origin.Close()

	store := goproxy.NewMemoryStore(2)
	_, _, client, _ := newCachingProxy(t, goproxy.CacheConfig{Store: store})

	doGet(t, client, origin.URL+"/a")
	doGet(t, client, origin.URL+"/b")
	// Touch /a so /b is the least recently used.
	doGet(t, client, origin.URL+"/a")
	doGet(t, client, origin.URL+"/c")

	assert.Nil(t, store.Get(origin.URL+"/b"))
	assert.NotNil(t, store.Get(origin.URL+"/a"))
	assert.NotNil(t, store.Get(origin.URL+"/c"))

	// /b was evicted: a new request hits the origin again.
	before := atomic.LoadInt32(&hits)
	doGet(t, client, origin.URL+"/b")
	assert.Equal(t, before+1, atomic.LoadInt32(&hits))
}

// TestCacheSingleflightRevalidation makes several callers wait on the same
// stale variant: exactly one conditional request reaches the origin.
func TestCacheSingleflightRevalidation(t *testing.T) {
	var active int32
	var maxActive int32
	release := make(chan struct{})
	var conditionalHits int32

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			atomic.AddInt32(&conditionalHits, 1)
			cur := atomic.AddInt32(&active, 1)
			for {
				old := atomic.LoadInt32(&maxActive)
				if cur <= old || atomic.CompareAndSwapInt32(&maxActive, old, cur) {
					break
				}
			}
			<-release
			atomic.AddInt32(&active, -1)
			w.Header().Set("Cache-Control", "max-age=60")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Cache-Control", "max-age=0")
		w.Header().Set("ETag", `"x"`)
		_, _ = io.WriteString(w, "shared")
	}))
	defer origin.Close()

	_, clock, client, _ := newCachingProxy(t, goproxy.CacheConfig{})

	doGet(t, client, origin.URL+"/same")
	clock.Advance(time.Second)

	const callers = 5
	var wg sync.WaitGroup
	wg.Add(callers)
	errs := make(chan error, callers)
	bodies := make(chan string, callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			resp, err := client.Get(origin.URL + "/same")
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			bodies <- string(b)
		}()
	}

	// Give the single shared revalidation time to park at the origin.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&conditionalHits))
	assert.Equal(t, int32(1), atomic.LoadInt32(&maxActive))
	close(release)
	wg.Wait()
	close(errs)
	close(bodies)
	for err := range errs {
		assert.NoError(t, err)
	}
	for body := range bodies {
		assert.Equal(t, "shared", body)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&conditionalHits))
}

// TestCacheConservativePolicies verifies the explicit conservative
// treatment of private, no-store, Authorization and user state.
func TestCacheConservativePolicies(t *testing.T) {
	type scenario struct {
		name       string
		setHeaders func(h http.Header)
		request    [2]string
		stored     bool
	}

	scenarios := []scenario{
		{
			name:       "private not stored",
			setHeaders: func(h http.Header) { h.Set("Cache-Control", "private, max-age=60") },
			stored:     false,
		},
		{
			name:       "response no-store not stored",
			setHeaders: func(h http.Header) { h.Set("Cache-Control", "no-store, max-age=60") },
			stored:     false,
		},
		{
			name:       "authorization without marker not stored",
			setHeaders: func(h http.Header) { h.Set("Cache-Control", "max-age=60") },
			request:    [2]string{"Authorization", "Bearer token"},
			stored:     false,
		},
		{
			name:       "authorization with public stored",
			setHeaders: func(h http.Header) { h.Set("Cache-Control", "public, max-age=60") },
			request:    [2]string{"Authorization", "Bearer token"},
			stored:     true,
		},
		{
			name:       "set-cookie without public not stored",
			setHeaders: func(h http.Header) { h.Set("Cache-Control", "max-age=60"); h.Set("Set-Cookie", "sid=1") },
			stored:     false,
		},
		{
			name:       "cookie request without vary cookie not stored",
			setHeaders: func(h http.Header) { h.Set("Cache-Control", "max-age=60") },
			request:    [2]string{"Cookie", "sid=1"},
			stored:     false,
		},
		{
			name:       "cookie with vary cookie stored",
			setHeaders: func(h http.Header) { h.Set("Cache-Control", "max-age=60"); h.Set("Vary", "Cookie") },
			request:    [2]string{"Cookie", "sid=1"},
			stored:     true,
		},
		{
			name:       "public cacheable stored",
			setHeaders: func(h http.Header) { h.Set("Cache-Control", "max-age=60") },
			stored:     true,
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sc.setHeaders(w.Header())
				_, _ = io.WriteString(w, "data")
			}))
			defer origin.Close()

			store := goproxy.NewMemoryStore(10)
			_, _, client, _ := newCachingProxy(t, goproxy.CacheConfig{Store: store})

			var headers [][2]string
			if sc.request[0] != "" {
				headers = append(headers, sc.request)
			}
			doGet(t, client, origin.URL+"/p", headers...)

			resource := store.Get(origin.URL + "/p")
			if sc.stored {
				assert.NotNil(t, resource, "expected resource in store")
			} else {
				assert.Nil(t, resource, "expected no resource in store")
			}
		})
	}
}

// TestCacheRequestNoStoreBypasses ensures a client no-store request never
// reads from or populates the cache.
func TestCacheRequestNoStoreBypasses(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Cache-Control", "max-age=60")
		_, _ = io.WriteString(w, "x")
	}))
	defer origin.Close()

	store := goproxy.NewMemoryStore(10)
	_, _, client, _ := newCachingProxy(t, goproxy.CacheConfig{Store: store})

	doGet(t, client, origin.URL+"/n", [2]string{"Cache-Control", "no-store"})
	assert.Nil(t, store.Get(origin.URL+"/n"))
	doGet(t, client, origin.URL+"/n")
	// A plain request then populates the cache.
	assert.NotNil(t, store.Get(origin.URL+"/n"))
	doGet(t, client, origin.URL+"/n", [2]string{"Cache-Control", "no-store"})
	assert.Equal(t, int32(3), atomic.LoadInt32(&hits))
}

// TestCacheVaryVariantsDoNotMerge fetches two languages and ensures both
// are stored and served correctly without cross-variant hits.
func TestCacheVaryVariantsDoNotMerge(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Vary", "Accept-Language")
		w.Header().Set("Cache-Control", "max-age=60")
		if strings.HasPrefix(r.Header.Get("Accept-Language"), "en") {
			_, _ = io.WriteString(w, "english")
			return
		}
		_, _ = io.WriteString(w, "中文")
	}))
	defer origin.Close()

	_, _, client, _ := newCachingProxy(t, goproxy.CacheConfig{})

	en := doGet(t, client, origin.URL+"/v", [2]string{"Accept-Language", "en"})
	assert.Equal(t, "english", readBody(t, en))
	zh := doGet(t, client, origin.URL+"/v", [2]string{"Accept-Language", "zh"})
	assert.Equal(t, "中文", readBody(t, zh))

	en2 := doGet(t, client, origin.URL+"/v", [2]string{"Accept-Language", "en"})
	assert.Equal(t, "english", readBody(t, en2))
	zh2 := doGet(t, client, origin.URL+"/v", [2]string{"Accept-Language", "zh"})
	assert.Equal(t, "中文", readBody(t, zh2))

	// Only the two initial fetches reached the origin.
	assert.Equal(t, int32(2), atomic.LoadInt32(&hits))
}

// TestCache200ReplacesVariant ensures a full response during revalidation
// replaces the stored body and metadata for that variant only.
func TestCache200ReplacesVariant(t *testing.T) {
	var version int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			// Origin changed: answer with a fresh 200 and new validator.
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set("ETag", `"v2"`)
			_, _ = io.WriteString(w, "newbody")
			return
		}
		atomic.AddInt32(&version, 1)
		w.Header().Set("Cache-Control", "max-age=0")
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, "oldbody")
	}))
	defer origin.Close()

	_, clock, client, _ := newCachingProxy(t, goproxy.CacheConfig{})

	first := doGet(t, client, origin.URL+"/u")
	assert.Equal(t, "oldbody", readBody(t, first))
	clock.Advance(time.Second)
	second := doGet(t, client, origin.URL+"/u")
	assert.Equal(t, "newbody", readBody(t, second))

	clock.Advance(time.Second)
	third := doGet(t, client, origin.URL+"/u")
	assert.Equal(t, "newbody", readBody(t, third))
	assert.Equal(t, `"v2"`, third.Header.Get("ETag"))
}

// TestCacheRangeAndHeadBypassed verifies Range and non-GET methods are not
// served from or stored in the cache.
func TestCacheRangeAndHeadBypassed(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Cache-Control", "max-age=60")
		_, _ = io.WriteString(w, "0123456789")
	}))
	defer origin.Close()

	store := goproxy.NewMemoryStore(10)
	_, _, client, _ := newCachingProxy(t, goproxy.CacheConfig{Store: store})

	doGet(t, client, origin.URL+"/b", [2]string{"Range", "bytes=0-3"})
	assert.Nil(t, store.Get(origin.URL+"/b"))

	req, _ := http.NewRequest(http.MethodHead, origin.URL+"/b", nil)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Nil(t, store.Get(origin.URL+"/b"))

	doGet(t, client, origin.URL+"/b")
	assert.NotNil(t, store.Get(origin.URL+"/b"))
}

// countingStore is a minimal custom CacheStore used to prove the storage
// layer is replaceable.
type countingStore struct {
	mu   sync.Mutex
	data map[string]*goproxy.CachedResource
	puts int
}

func newCountingStore() *countingStore {
	return &countingStore{data: make(map[string]*goproxy.CachedResource)}
}

func (s *countingStore) Get(key string) *goproxy.CachedResource {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key]
}

func (s *countingStore) Put(key string, resource *goproxy.CachedResource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = resource
	s.puts++
}

func (s *countingStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
}

// TestCacheCustomStore proves a user-provided CacheStore receives all
// cached resources.
func TestCacheCustomStore(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		_, _ = io.WriteString(w, "z")
	}))
	defer origin.Close()

	store := newCountingStore()
	_, _, client, _ := newCachingProxy(t, goproxy.CacheConfig{Store: store})

	doGet(t, client, origin.URL+"/cs")
	doGet(t, client, origin.URL+"/cs")

	store.mu.Lock()
	defer store.mu.Unlock()
	assert.NotNil(t, store.data[origin.URL+"/cs"])
	assert.Equal(t, 1, store.puts)
}

// TestCacheAgeFromOrigin confirms the Age header from the origin is taken
// into account in the served Age value.
func TestCacheAgeFromOrigin(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=100")
		w.Header().Set("Age", "10")
		_, _ = io.WriteString(w, "aged")
	}))
	defer origin.Close()

	_, clock, client, _ := newCachingProxy(t, goproxy.CacheConfig{})
	doGet(t, client, origin.URL+"/age")
	clock.Advance(5 * time.Second)
	resp := doGet(t, client, origin.URL+"/age")
	assert.Equal(t, "aged", readBody(t, resp))
	assert.Equal(t, "15", resp.Header.Get("Age"))
}

// TestCacheRevalidationCancellationIsolated cancels one waiting caller and
// verifies the shared revalidation still completes for other callers.
func TestCacheRevalidationCancellationIsolated(t *testing.T) {
	release := make(chan struct{})
	var conditionalHits int32

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			atomic.AddInt32(&conditionalHits, 1)
			<-release
			w.Header().Set("Cache-Control", "max-age=60")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Cache-Control", "max-age=0")
		w.Header().Set("ETag", `"c"`)
		_, _ = io.WriteString(w, "persist")
	}))
	defer origin.Close()

	_, clock, client, _ := newCachingProxy(t, goproxy.CacheConfig{RevalidationTimeout: time.Hour})

	doGet(t, client, origin.URL+"/cancel")
	clock.Advance(time.Second)

	cancelledDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/cancel", nil)
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		cancelledDone <- err
	}()

	otherDone := make(chan string, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		resp, err := client.Get(origin.URL + "/cancel")
		if err != nil {
			otherDone <- "ERR:" + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		otherDone <- string(b)
	}()

	// Wait until both waiters are parked before releasing the origin.
	time.Sleep(350 * time.Millisecond)
	close(release)

	select {
	case err := <-cancelledDone:
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return")
	}
	select {
	case body := <-otherDone:
		assert.Equal(t, "persist", body)
	case <-time.After(2 * time.Second):
		t.Fatal("other caller did not receive the revalidated response")
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&conditionalHits))
}
