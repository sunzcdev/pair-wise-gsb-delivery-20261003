package digest

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"gotest.tools/v3/assert"
)

func TestTransport(t *testing.T) {
	username := "foo"
	password := "bar"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NilError(t, err)
		assert.Equal(t, string(body), "The Body")

		chal := &Challenge{
			Realm:     "test",
			Nonce:     "jgdfsijdfisd",
			Algorithm: "MD5",
			QOP:       []string{"auth"},
		}
		var authorized bool
		if auth := r.Header.Get("Authorization"); auth != "" {
			cred, err := ParseCredentials(auth)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			cred2, err := Digest(chal, Options{
				Method:   r.Method,
				URI:      r.URL.RequestURI(),
				Cnonce:   cred.Cnonce,
				Count:    cred.Nc,
				Username: username,
				Password: password,
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			authorized = cred.Response == cred2.Response
		}
		if !authorized {
			w.Header().Add("WWW-Authenticate", chal.String())
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "Hello World")
	}))
	defer ts.Close()
	client := http.Client{
		Transport: &Transport{
			Username: username,
			Password: password,
		},
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader("The Body"))
	assert.NilError(t, err)
	res, err := client.Do(req)
	assert.NilError(t, err)
	body, err := io.ReadAll(res.Body)
	assert.NilError(t, err)
	assert.Equal(t, string(body), "Hello World")
}

func TestTransportProbe(t *testing.T) {
	username := "foo"
	password := "bar"
	chal := &Challenge{Realm: "test", Nonce: "jgdfsijdfisd", Algorithm: "MD5", QOP: []string{"auth"}}

	var reqCount, unauthBodyLen int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reqCount++
		auth := r.Header.Get("Authorization")
		if auth == "" {
			// The probe (challenge-discovery) request must carry no body.
			unauthBodyLen = len(body)
			w.Header().Add("WWW-Authenticate", chal.String())
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// The authenticated request must carry the real body.
		assert.Equal(t, string(body), "The Body")
		cred, err := ParseCredentials(auth)
		assert.NilError(t, err)
		cred2, err := Digest(chal, Options{Method: r.Method, URI: r.URL.RequestURI(), Cnonce: cred.Cnonce, Count: cred.Nc, Username: username, Password: password})
		assert.NilError(t, err)
		if cred.Response != cred2.Response {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "Hello World")
	}))
	defer ts.Close()

	client := http.Client{Transport: &Transport{Username: username, Password: password, Probe: true}}
	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader("The Body"))
	assert.NilError(t, err)
	res, err := client.Do(req)
	assert.NilError(t, err)
	body, err := io.ReadAll(res.Body)
	assert.NilError(t, err)
	assert.Equal(t, string(body), "Hello World")
	assert.Equal(t, reqCount, 2)      // discovery + authenticated upload
	assert.Equal(t, unauthBodyLen, 0) // body was NOT streamed on the unauthenticated request
}

func TestTransportLive(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		username string
		password string
	}{
		{
			name:     "httpbun-auth",
			url:      "https://httpbun.com/digest-auth/auth/foo/bar",
			username: "foo",
			password: "bar",
		},
		{
			name:     "httpbun-auth-int",
			url:      "https://httpbun.com/digest-auth/auth-int/foo/bar",
			username: "foo",
			password: "bar",
		},
		{
			name:     "postman",
			url:      "https://postman-echo.com/digest-auth",
			username: "postman",
			password: "password",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := http.Client{
				Transport: &Transport{
					Username: tt.username,
					Password: tt.password,
				},
			}
			res, err := client.Get(tt.url)
			assert.NilError(t, err)
			defer res.Body.Close()
			assert.Assert(t, res.StatusCode == http.StatusOK)
		})
	}
}

func TestTransportNoChallenge(t *testing.T) {
	var expired bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			if expired {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
			} else {
				fmt.Fprintf(w, "Success")
				expired = true
			}
			return
		}
		chal := &Challenge{
			Realm:     "test",
			Nonce:     "jgdfsijdfisd",
			Algorithm: "MD5",
			QOP:       []string{"auth"},
		}
		w.Header().Add("WWW-Authenticate", chal.String())
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}))
	defer ts.Close()
	client := http.Client{
		Transport: &Transport{
			Username: "test",
			Password: "test",
		},
	}
	// first request should succeed
	res1, err := client.Get(ts.URL)
	assert.NilError(t, err)
	assert.Equal(t, res1.StatusCode, http.StatusOK)
	// second request should return the status Unauthorized
	res2, err := client.Get(ts.URL)
	assert.NilError(t, err)
	assert.Equal(t, res2.StatusCode, http.StatusUnauthorized)
}

// seedCache installs a cached challenge for the transport
func seedCache(tr *Transport, rawurl string, chal *Challenge, n int) {
	u, err := url.Parse(rawurl)
	if err != nil {
		panic(err)
	}
	tr.cache = map[string]*cchal{
		u.Hostname(): {c: chal, n: n},
	}
}

// cachedChallenge returns the currently cached challenge entry
func cachedChallenge(t *testing.T, tr *Transport, rawurl string) *cchal {
	t.Helper()
	u, err := url.Parse(rawurl)
	assert.NilError(t, err)
	tr.cacheMu.Lock()
	defer tr.cacheMu.Unlock()
	cc := tr.cache[u.Hostname()]
	assert.Assert(t, cc != nil)
	return cc
}

// validCredentials parses the Authorization header of the request and
// reports whether the response digest is valid for the challenge
func validCredentials(r *http.Request, chal *Challenge, username, password string) (*Credentials, bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, false
	}
	cred, err := ParseCredentials(auth)
	if err != nil || cred.Nonce != chal.Nonce {
		return cred, false
	}
	want, err := Digest(chal, Options{
		Method:   r.Method,
		URI:      r.URL.RequestURI(),
		Cnonce:   cred.Cnonce,
		Count:    cred.Nc,
		Username: username,
		Password: password,
	})
	if err != nil {
		return cred, false
	}
	return cred, cred.Response == want.Response
}

// recorder collects events from server handlers running in other goroutines
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *recorder) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func TestTransportNextNonce(t *testing.T) {
	username, password := "foo", "bar"
	chal1 := &Challenge{Realm: "test", Nonce: "nonce-1", Algorithm: "MD5", QOP: []string{"auth"}}

	var rec recorder
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			rec.add("bare")
			w.Header().Set("WWW-Authenticate", chal1.String())
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		cred, err := ParseCredentials(auth)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.add(fmt.Sprintf("%s/%08x", cred.Nonce, cred.Nc))
		chal := *chal1
		chal.Nonce = cred.Nonce
		if _, ok := validCredentials(r, &chal, username, password); !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if cred.Nonce == chal1.Nonce {
			w.Header().Set("Authentication-Info", (&AuthenticationInfo{NextNonce: "nonce-2"}).String())
		}
		io.WriteString(w, "ok")
	}))
	defer ts.Close()

	client := http.Client{Transport: &Transport{Username: username, Password: password}}
	for i := 0; i < 2; i++ {
		res, err := client.Get(ts.URL)
		assert.NilError(t, err)
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		assert.Equal(t, res.StatusCode, http.StatusOK)
	}
	assert.DeepEqual(t, rec.get(), []string{
		"bare",
		"nonce-1/00000001",
		"nonce-2/00000001", // counting restarted on the committed nextnonce
	})
}

// Two in-flight requests share a challenge generation. The second response
// commits its nextnonce first; the stale first response must not roll the
// cache back even though it arrives last.
func TestTransportNextNonceOutOfOrder(t *testing.T) {
	username, password := "foo", "bar"
	chal := &Challenge{Realm: "test", Nonce: "nonce-1", Algorithm: "MD5", QOP: []string{"auth"}}

	arrivedA := make(chan struct{})
	releaseA := make(chan struct{})
	var arriveOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseA) }) }
	defer release()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := validCredentials(r, chal, username, password)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if cred.Nc == 1 {
			// request A: hold the response until B has committed
			arriveOnce.Do(func() { close(arrivedA) })
			<-releaseA
			w.Header().Set("Authentication-Info", (&AuthenticationInfo{NextNonce: "nonce-a"}).String())
			io.WriteString(w, "A")
			return
		}
		// request B: respond immediately
		w.Header().Set("Authentication-Info", (&AuthenticationInfo{NextNonce: "nonce-b"}).String())
		io.WriteString(w, "B")
	}))
	defer ts.Close()

	tr := &Transport{Username: username, Password: password}
	seedCache(tr, ts.URL, chal, 0)
	client := http.Client{Transport: tr}

	// request A goes out first and captures the current generation
	aErr := make(chan error, 1)
	go func() {
		res, err := client.Get(ts.URL)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		aErr <- err
	}()
	<-arrivedA

	// request B uses the same generation and commits its nextnonce first
	resB, err := client.Get(ts.URL)
	assert.NilError(t, err)
	_, _ = io.Copy(io.Discard, resB.Body)
	resB.Body.Close()

	// only now A's stale response arrives; it must not roll the cache back
	release()
	assert.NilError(t, <-aErr)

	cc := cachedChallenge(t, tr, ts.URL)
	assert.Equal(t, cc.c.Nonce, "nonce-b")
	assert.Equal(t, cc.n, 0)
}

// A 401 swaps the cached challenge while an older request is still in
// flight. The older response's nextnonce targets a superseded generation
// and must be discarded.
func TestTransportNextNonceChallengeSwap(t *testing.T) {
	username, password := "foo", "bar"
	chal1 := &Challenge{Realm: "test", Nonce: "nonce-1", Algorithm: "MD5", QOP: []string{"auth"}}
	chal2 := &Challenge{Realm: "test", Nonce: "nonce-2", Algorithm: "MD5", QOP: []string{"auth"}}

	arrivedA := make(chan struct{})
	releaseA := make(chan struct{})
	var arriveOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseA) }) }
	defer release()

	var rec recorder
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			rec.add("bare")
			w.Header().Set("WWW-Authenticate", chal1.String())
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		cred, err := ParseCredentials(auth)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.add(fmt.Sprintf("%s/%08x", cred.Nonce, cred.Nc))
		switch {
		case cred.Nonce == "nonce-1" && cred.Nc == 1:
			// request A: hold until the challenge has been swapped
			arriveOnce.Do(func() { close(arrivedA) })
			<-releaseA
			w.Header().Set("Authentication-Info", (&AuthenticationInfo{NextNonce: "nonce-a"}).String())
			io.WriteString(w, "A")
		case cred.Nonce == "nonce-1":
			// request B: the old nonce expired, issue a new challenge
			w.Header().Set("WWW-Authenticate", chal2.String())
			w.WriteHeader(http.StatusUnauthorized)
		case cred.Nonce == "nonce-2":
			if _, ok := validCredentials(r, chal2, username, password); !ok {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			io.WriteString(w, "ok")
		default:
			http.Error(w, "unexpected nonce", http.StatusBadRequest)
		}
	}))
	defer ts.Close()

	tr := &Transport{Username: username, Password: password}
	seedCache(tr, ts.URL, chal1, 0)
	client := http.Client{Transport: tr}

	// request A captures the nonce-1 generation and stays in flight
	aErr := make(chan error, 1)
	go func() {
		res, err := client.Get(ts.URL)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		aErr <- err
	}()
	<-arrivedA

	// request B is rejected with a new challenge, replacing the generation
	resB, err := client.Get(ts.URL)
	assert.NilError(t, err)
	_, _ = io.Copy(io.Discard, resB.Body)
	resB.Body.Close()
	assert.Equal(t, resB.StatusCode, http.StatusOK)

	// A's nextnonce update targets a superseded generation and is dropped
	release()
	assert.NilError(t, <-aErr)

	cc := cachedChallenge(t, tr, ts.URL)
	assert.Equal(t, cc.c.Nonce, "nonce-2")
	assert.Equal(t, cc.n, 1)

	// the next request keeps counting on the swapped-in challenge
	res, err := client.Get(ts.URL)
	assert.NilError(t, err)
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.DeepEqual(t, rec.get(), []string{
		"nonce-1/00000001", // A captures the original generation
		"nonce-1/00000002", // B's first attempt on the same generation
		"nonce-2/00000001", // B's retry on the swapped challenge
		"nonce-2/00000002", // C continues counting on the new generation
	})
}

// Concurrent requests must each be allocated a distinct nonce count.
func TestTransportConcurrentNonceCount(t *testing.T) {
	const N = 50
	username, password := "foo", "bar"
	chal := &Challenge{Realm: "test", Nonce: "nonce-1", Algorithm: "MD5", QOP: []string{"auth"}}

	var mu sync.Mutex
	ncs := make(map[int]int)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := validCredentials(r, chal, username, password)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		ncs[cred.Nc]++
		mu.Unlock()
		io.WriteString(w, "ok")
	}))
	defer ts.Close()

	tr := &Transport{Username: username, Password: password}
	seedCache(tr, ts.URL, chal, 0)
	client := http.Client{Transport: tr}

	start := make(chan struct{})
	errs := make(chan error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := client.Get(ts.URL)
			if err != nil {
				errs <- err
				return
			}
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("unexpected status: %s", res.Status)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NilError(t, err)
	}
	// every nonce count in [1, N] was allocated exactly once
	assert.Equal(t, len(ncs), N)
	for nc := 1; nc <= N; nc++ {
		assert.Equal(t, ncs[nc], 1)
	}
	assert.Equal(t, cachedChallenge(t, tr, ts.URL).n, N)
}

// Once every nonce count is allocated, the transport reports exhaustion and
// drops the challenge instead of wrapping the counter.
func TestChallengeNonceCountExhausted(t *testing.T) {
	tr := &Transport{}
	tr.cache = map[string]*cchal{
		"example.com": {c: &Challenge{Nonce: "nonce-1"}, n: maxNonceCount},
	}
	req, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	assert.NilError(t, err)
	_, _, err = tr.challenge(req)
	assert.ErrorIs(t, err, ErrNonceCountExhausted)
	// the exhausted challenge is dropped while waiting for a new one
	_, ok := tr.cache["example.com"]
	assert.Assert(t, !ok)
}

// The last nonce count (ffffffff) is still usable. The following request
// finds the challenge exhausted, goes out bare, and picks up the fresh
// challenge from the 401 instead of re-using wrapped nonce counts.
func TestTransportNonceCountExhaustionBoundary(t *testing.T) {
	username, password := "foo", "bar"
	chal1 := &Challenge{Realm: "test", Nonce: "nonce-1", Algorithm: "MD5", QOP: []string{"auth"}}
	chal2 := &Challenge{Realm: "test", Nonce: "nonce-2", Algorithm: "MD5", QOP: []string{"auth"}}

	var rec recorder
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			rec.add("bare")
			w.Header().Set("WWW-Authenticate", chal2.String())
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		cred, err := ParseCredentials(auth)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.add(fmt.Sprintf("%s/%08x", cred.Nonce, cred.Nc))
		chal := chal1
		if cred.Nonce == chal2.Nonce {
			chal = chal2
		}
		if _, ok := validCredentials(r, chal, username, password); !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer ts.Close()

	tr := &Transport{Username: username, Password: password}
	// one nonce count left before the 8-hex-digit limit
	seedCache(tr, ts.URL, chal1, maxNonceCount-1)
	client := http.Client{Transport: tr}

	for i := 0; i < 2; i++ {
		res, err := client.Get(ts.URL)
		assert.NilError(t, err)
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		assert.Equal(t, res.StatusCode, http.StatusOK)
	}
	assert.DeepEqual(t, rec.get(), []string{
		"nonce-1/ffffffff", // last valid nonce count, no wrap-around
		"bare",             // exhausted: wait for a new challenge
		"nonce-2/00000001", // counting restarts on the new challenge
	})
}

// With NoReuse the generation is consumed when the request is authorized,
// so a nextnonce update can never be committed.
func TestTransportNextNonceNoReuse(t *testing.T) {
	username, password := "foo", "bar"
	chal1 := &Challenge{Realm: "test", Nonce: "nonce-1", Algorithm: "MD5", QOP: []string{"auth"}}
	chal3 := &Challenge{Realm: "test", Nonce: "nonce-3", Algorithm: "MD5", QOP: []string{"auth"}}

	var rec recorder
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			rec.add("bare")
			w.Header().Set("WWW-Authenticate", chal3.String())
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		cred, err := ParseCredentials(auth)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.add(fmt.Sprintf("%s/%08x", cred.Nonce, cred.Nc))
		var chal *Challenge
		switch cred.Nonce {
		case chal1.Nonce:
			chal = chal1
		case chal3.Nonce:
			chal = chal3
		default:
			http.Error(w, "unexpected nonce", http.StatusBadRequest)
			return
		}
		if _, ok := validCredentials(r, chal, username, password); !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if cred.Nonce == chal1.Nonce {
			w.Header().Set("Authentication-Info", (&AuthenticationInfo{NextNonce: "nonce-2"}).String())
		}
		io.WriteString(w, "ok")
	}))
	defer ts.Close()

	tr := &Transport{Username: username, Password: password, NoReuse: true}
	seedCache(tr, ts.URL, chal1, 0)
	client := http.Client{Transport: tr}

	for i := 0; i < 2; i++ {
		res, err := client.Get(ts.URL)
		assert.NilError(t, err)
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		assert.Equal(t, res.StatusCode, http.StatusOK)
	}
	assert.DeepEqual(t, rec.get(), []string{
		"nonce-1/00000001",
		"bare", // the nextnonce was not committed: full challenge dance
		"nonce-3/00000001",
	})
}

// rspauth computes the expected response digest for an Authentication-Info
// header with qop=auth.
func rspauth(chal *Challenge, cred *Credentials, username, password string) string {
	h, err := hashalg(chal.Algorithm)
	if err != nil {
		panic(err)
	}
	a1 := hashjoin(h, username, chal.Realm, password)
	a2 := hashjoin(h, "", cred.URI)
	return hashjoin(h, a1, chal.Nonce, fmt.Sprintf("%08x", cred.Nc), cred.Cnonce, "auth", a2)
}

// nextNonceServer returns a server which authorizes requests against chal
// and decorates every successful response with an Authentication-Info
// header built by makeInfo.
func nextNonceServer(chal *Challenge, username, password string, makeInfo func(*Credentials) *AuthenticationInfo) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := validCredentials(r, chal, username, password)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if info := makeInfo(cred); info != nil {
			w.Header().Set("Authentication-Info", info.String())
		}
		io.WriteString(w, "ok")
	}))
}

func TestTransportNextNonceVerifyResponse(t *testing.T) {
	username, password := "foo", "bar"
	chal := &Challenge{Realm: "test", Nonce: "nonce-1", Algorithm: "MD5", QOP: []string{"auth"}}

	do := func(t *testing.T, tr *Transport, ts *httptest.Server) {
		t.Helper()
		seedCache(tr, ts.URL, chal, 0)
		client := http.Client{Transport: tr}
		res, err := client.Get(ts.URL)
		assert.NilError(t, err)
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		assert.Equal(t, res.StatusCode, http.StatusOK)
	}

	t.Run("valid rspauth commits", func(t *testing.T) {
		ts := nextNonceServer(chal, username, password, func(cred *Credentials) *AuthenticationInfo {
			return &AuthenticationInfo{
				NextNonce: "nonce-2",
				QOP:       "auth",
				Cnonce:    cred.Cnonce,
				Nc:        cred.Nc,
				Rspauth:   rspauth(chal, cred, username, password),
			}
		})
		defer ts.Close()
		tr := &Transport{Username: username, Password: password}
		do(t, tr, ts)
		assert.Equal(t, cachedChallenge(t, tr, ts.URL).c.Nonce, "nonce-2")
	})

	t.Run("invalid rspauth rejected", func(t *testing.T) {
		ts := nextNonceServer(chal, username, password, func(cred *Credentials) *AuthenticationInfo {
			return &AuthenticationInfo{
				NextNonce: "nonce-2",
				QOP:       "auth",
				Cnonce:    cred.Cnonce,
				Nc:        cred.Nc,
				Rspauth:   "bogus",
			}
		})
		defer ts.Close()
		tr := &Transport{Username: username, Password: password}
		do(t, tr, ts)
		assert.Equal(t, cachedChallenge(t, tr, ts.URL).c.Nonce, "nonce-1")
	})

	t.Run("custom verifier accepts", func(t *testing.T) {
		ts := nextNonceServer(chal, username, password, func(cred *Credentials) *AuthenticationInfo {
			return &AuthenticationInfo{NextNonce: "nonce-2", Rspauth: "bogus"}
		})
		defer ts.Close()
		var got *ResponseAuth
		tr := &Transport{
			Username: username,
			Password: password,
			VerifyResponse: func(ra *ResponseAuth) error {
				got = ra
				return nil
			},
		}
		do(t, tr, ts)
		assert.Equal(t, cachedChallenge(t, tr, ts.URL).c.Nonce, "nonce-2")
		assert.Assert(t, got != nil)
		assert.Equal(t, got.Info.NextNonce, "nonce-2")
		assert.Equal(t, got.Challenge.Nonce, "nonce-1")
		assert.Equal(t, got.Credentials.Nonce, "nonce-1")
	})

	t.Run("custom verifier rejects", func(t *testing.T) {
		ts := nextNonceServer(chal, username, password, func(cred *Credentials) *AuthenticationInfo {
			return &AuthenticationInfo{NextNonce: "nonce-2"}
		})
		defer ts.Close()
		tr := &Transport{
			Username: username,
			Password: password,
			VerifyResponse: func(ra *ResponseAuth) error {
				return ErrRspauth
			},
		}
		do(t, tr, ts)
		assert.Equal(t, cachedChallenge(t, tr, ts.URL).c.Nonce, "nonce-1")
	})
}
