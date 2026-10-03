package digest

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"gotest.tools/v3/assert"
)

// recordedRequest captures what the underlying RoundTripper received.
type recordedRequest struct {
	method        string
	requestURI    string
	body          string
	authorization string
	proxyAuth     string
	customHeader  string
}

// scriptedRoundTripper replays canned responses in order and records requests.
type scriptedRoundTripper struct {
	mu        sync.Mutex
	requests  []recordedRequest
	responses []*http.Response
}

func (s *scriptedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
	}
	rec := recordedRequest{
		method:        req.Method,
		requestURI:    req.URL.RequestURI(),
		body:          string(body),
		authorization: req.Header.Get("Authorization"),
		proxyAuth:     req.Header.Get("Proxy-Authorization"),
		customHeader:  req.Header.Get("X-Custom"),
	}
	s.mu.Lock()
	s.requests = append(s.requests, rec)
	if len(s.responses) == 0 {
		s.mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{},
			Body:       http.NoBody,
			Request:    req,
		}, nil
	}
	res := s.responses[0]
	s.responses = s.responses[1:]
	s.mu.Unlock()
	res.Request = req
	return res, nil
}

func challengeResponse(status int, header string) *http.Response {
	h := http.Header{}
	if status == http.StatusProxyAuthRequired {
		h.Set("Proxy-Authenticate", header)
	} else {
		h.Set("WWW-Authenticate", header)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       http.NoBody,
	}
}

func plainResponse(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("Hello World")),
	}
}

func checkCred(t *testing.T, headerValue string, chal *Challenge, method, uri, username, password string) {
	t.Helper()
	assert.Assert(t, headerValue != "", "missing credentials header")
	cred, err := ParseCredentials(headerValue)
	assert.NilError(t, err)
	want, err := Digest(chal, Options{
		Method:   method,
		URI:      uri,
		Cnonce:   cred.Cnonce,
		Count:    cred.Nc,
		Username: username,
		Password: password,
	})
	assert.NilError(t, err)
	assert.Equal(t, cred.Response, want.Response)
	assert.Equal(t, cred.URI, uri)
}

// TestTransportProxyThenOrigin replays 407 -> 401 -> 200 and verifies that each
// hop carries exactly the correct authentication header.
func TestTransportProxyThenOrigin(t *testing.T) {
	proxyChal := &Challenge{Realm: "proxy-realm", Nonce: "proxy-nonce", Algorithm: "MD5", QOP: []string{"auth"}}
	originChal := &Challenge{Realm: "origin-realm", Nonce: "origin-nonce", Algorithm: "MD5", QOP: []string{"auth"}}

	rt := &scriptedRoundTripper{
		responses: []*http.Response{
			challengeResponse(http.StatusProxyAuthRequired, proxyChal.String()),
			challengeResponse(http.StatusUnauthorized, originChal.String()),
			plainResponse(http.StatusOK),
			plainResponse(http.StatusOK),
		},
	}
	tr := &Transport{
		Username:      "ouser",
		Password:      "opass",
		ProxyUsername: "puser",
		ProxyPassword: "ppass",
		Transport:     rt,
	}

	req, err := http.NewRequest(http.MethodPost, "http://example.com/path?q=1", strings.NewReader("The Body"))
	assert.NilError(t, err)
	req.Header.Set("X-Custom", "kept")

	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusOK)
	body, err := io.ReadAll(res.Body)
	assert.NilError(t, err)
	assert.Equal(t, string(body), "Hello World")

	assertFirstThreeHops := func() {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		assert.Equal(t, len(rt.requests), 3)

		// hop 1: no authentication headers at all
		assert.Equal(t, rt.requests[0].authorization, "")
		assert.Equal(t, rt.requests[0].proxyAuth, "")
		assert.Equal(t, rt.requests[0].body, "The Body")

		// hop 2: Proxy-Authorization only, computed against the absolute-form target
		assert.Equal(t, rt.requests[1].authorization, "")
		checkCred(t, rt.requests[1].proxyAuth, proxyChal,
			http.MethodPost, "http://example.com/path?q=1", "puser", "ppass")
		assert.Equal(t, rt.requests[1].body, "The Body")

		// hop 3: both headers; Authorization uses the origin-form URI
		checkCred(t, rt.requests[2].proxyAuth, proxyChal,
			http.MethodPost, "http://example.com/path?q=1", "puser", "ppass")
		checkCred(t, rt.requests[2].authorization, originChal,
			http.MethodPost, "/path?q=1", "ouser", "opass")
		assert.Equal(t, rt.requests[2].body, "The Body")

		// custom headers and method survive every hop
		for i, rec := range rt.requests {
			assert.Equal(t, rec.customHeader, "kept", "hop %d", i+1)
			assert.Equal(t, rec.method, http.MethodPost, "hop %d", i+1)
		}

		// the caller's request and headers were never mutated
		assert.Equal(t, req.Header.Get("Authorization"), "")
		assert.Equal(t, req.Header.Get("Proxy-Authorization"), "")
	}
	assertFirstThreeHops()

	// a subsequent logical request goes out fully authenticated on the first try
	res2, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res2.StatusCode, http.StatusOK)
	rt.mu.Lock()
	assert.Equal(t, len(rt.requests), 4)
	rec := rt.requests[3]
	checkCred(t, rec.proxyAuth, proxyChal, http.MethodPost, "http://example.com/path?q=1", "puser", "ppass")
	checkCred(t, rec.authorization, originChal, http.MethodPost, "/path?q=1", "ouser", "opass")
	rt.mu.Unlock()
}

// TestTransportProxyRepeatedChallenge verifies that a proxy that keeps
// challenging does not cause an unbounded loop: the layer retries once and the
// second 407 is returned to the caller.
func TestTransportProxyRepeatedChallenge(t *testing.T) {
	proxyChal := &Challenge{Realm: "proxy-realm", Nonce: "proxy-nonce", Algorithm: "MD5", QOP: []string{"auth"}}
	rt := &scriptedRoundTripper{
		responses: []*http.Response{
			challengeResponse(http.StatusProxyAuthRequired, proxyChal.String()),
			challengeResponse(http.StatusProxyAuthRequired, proxyChal.String()),
		},
	}
	tr := &Transport{ProxyUsername: "puser", ProxyPassword: "ppass", Transport: rt}

	req, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	assert.NilError(t, err)
	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusProxyAuthRequired)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, len(rt.requests), 2)
	assert.Equal(t, rt.requests[0].proxyAuth, "")
	assert.Assert(t, rt.requests[1].proxyAuth != "")
	assert.Equal(t, rt.requests[1].authorization, "")
}

// TestTransportOriginRepeatedChallenge bounds the origin layer independently.
func TestTransportOriginRepeatedChallenge(t *testing.T) {
	originChal := &Challenge{Realm: "origin-realm", Nonce: "origin-nonce", Algorithm: "MD5", QOP: []string{"auth"}}
	rt := &scriptedRoundTripper{
		responses: []*http.Response{
			challengeResponse(http.StatusUnauthorized, originChal.String()),
			challengeResponse(http.StatusUnauthorized, originChal.String()),
		},
	}
	tr := &Transport{
		Username:      "ouser",
		Password:      "opass",
		ProxyUsername: "puser",
		ProxyPassword: "ppass",
		Transport:     rt,
	}

	req, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	assert.NilError(t, err)
	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusUnauthorized)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, len(rt.requests), 2)
	// the proxy never challenged, so Proxy-Authorization must never appear
	assert.Equal(t, rt.requests[0].proxyAuth, "")
	assert.Equal(t, rt.requests[1].proxyAuth, "")
	assert.Assert(t, rt.requests[1].authorization != "")
}

// failingGetBody produces the body once, then refuses to replay it.
type failingGetBody struct {
	used bool
}

func (f *failingGetBody) GetBody() (io.ReadCloser, error) {
	if !f.used {
		f.used = true
		return io.NopCloser(strings.NewReader("The Body")), nil
	}
	return nil, errors.New("body already consumed")
}

// TestTransportBodyNotReplayable verifies that a non-replayable body produces
// an explicit error after the challenge: the request is never resent empty.
func TestTransportBodyNotReplayable(t *testing.T) {
	proxyChal := &Challenge{Realm: "proxy-realm", Nonce: "proxy-nonce", Algorithm: "MD5", QOP: []string{"auth"}}
	rt := &scriptedRoundTripper{
		responses: []*http.Response{
			challengeResponse(http.StatusProxyAuthRequired, proxyChal.String()),
		},
	}
	tr := &Transport{ProxyUsername: "puser", ProxyPassword: "ppass", Transport: rt}

	gb := &failingGetBody{}
	req, err := http.NewRequest(http.MethodPost, "http://example.com/",
		io.NopCloser(strings.NewReader("The Body")))
	assert.NilError(t, err)
	req.GetBody = gb.GetBody

	res, err := tr.RoundTrip(req)
	assert.Assert(t, res == nil)
	assert.Assert(t, errors.Is(err, ErrBodyNotReplayable), "got %v", err)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, len(rt.requests), 1)
	assert.Equal(t, rt.requests[0].body, "The Body")
	assert.Equal(t, rt.requests[0].proxyAuth, "")
}

// TestTransportSameRealmTwoLayers verifies that a proxy and an origin using the
// same realm and nonce keep independent credentials, caches and nonce counters.
func TestTransportSameRealmTwoLayers(t *testing.T) {
	chal := &Challenge{Realm: "shared-realm", Nonce: "shared-nonce", Algorithm: "MD5", QOP: []string{"auth"}}
	rt := &scriptedRoundTripper{
		responses: []*http.Response{
			challengeResponse(http.StatusProxyAuthRequired, chal.String()),
			challengeResponse(http.StatusUnauthorized, chal.String()),
			plainResponse(http.StatusOK),
		},
	}
	tr := &Transport{
		Username:      "ouser",
		Password:      "opass",
		ProxyUsername: "puser",
		ProxyPassword: "ppass",
		Transport:     rt,
	}

	req, err := http.NewRequest(http.MethodGet, "http://example.com/path", nil)
	assert.NilError(t, err)
	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusOK)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, len(rt.requests), 3)

	// hop 2: proxy credentials with absolute URI
	checkCred(t, rt.requests[1].proxyAuth, chal,
		http.MethodGet, "http://example.com/path", "puser", "ppass")
	assert.Equal(t, rt.requests[1].authorization, "")

	// hop 3: origin credentials with origin-form URI; both counters start at 1
	checkCred(t, rt.requests[2].authorization, chal,
		http.MethodGet, "/path", "ouser", "opass")
	checkCred(t, rt.requests[2].proxyAuth, chal,
		http.MethodGet, "http://example.com/path", "puser", "ppass")
	proxyCred, err := ParseCredentials(rt.requests[2].proxyAuth)
	assert.NilError(t, err)
	originCred, err := ParseCredentials(rt.requests[2].authorization)
	assert.NilError(t, err)
	assert.Equal(t, originCred.Nc, 1)
	// proxy nc is 2: the cached proxy challenge was used on hops 2 and 3
	assert.Equal(t, proxyCred.Nc, 2)
	// distinct usernames/passwords/URIs must yield distinct responses
	assert.Assert(t, proxyCred.Response != originCred.Response)
}

// TestTransportNoProxyConfigRegression verifies that with no proxy
// configuration, a 407 is returned verbatim: no retry, no Proxy-Authorization
// injected, and origin auth is unaffected.
func TestTransportNoProxyConfigRegression(t *testing.T) {
	t.Run("407 passes through", func(t *testing.T) {
		rt := &scriptedRoundTripper{
			responses: []*http.Response{
				challengeResponse(http.StatusProxyAuthRequired,
					(&Challenge{Realm: "proxy", Nonce: "n"}).String()),
			},
		}
		tr := &Transport{Username: "ouser", Password: "opass", Transport: rt}

		req, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
		assert.NilError(t, err)
		res, err := tr.RoundTrip(req)
		assert.NilError(t, err)
		assert.Equal(t, res.StatusCode, http.StatusProxyAuthRequired)

		rt.mu.Lock()
		defer rt.mu.Unlock()
		assert.Equal(t, len(rt.requests), 1)
		assert.Equal(t, rt.requests[0].proxyAuth, "")
		assert.Equal(t, rt.requests[0].authorization, "")
	})

	t.Run("origin 401 still handled", func(t *testing.T) {
		originChal := &Challenge{Realm: "origin", Nonce: "n", Algorithm: "MD5", QOP: []string{"auth"}}
		rt := &scriptedRoundTripper{
			responses: []*http.Response{
				challengeResponse(http.StatusUnauthorized, originChal.String()),
				plainResponse(http.StatusOK),
			},
		}
		tr := &Transport{Username: "ouser", Password: "opass", Transport: rt}

		req, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
		assert.NilError(t, err)
		res, err := tr.RoundTrip(req)
		assert.NilError(t, err)
		assert.Equal(t, res.StatusCode, http.StatusOK)

		rt.mu.Lock()
		defer rt.mu.Unlock()
		assert.Equal(t, len(rt.requests), 2)
		assert.Equal(t, rt.requests[0].authorization, "")
		assert.Assert(t, rt.requests[1].authorization != "")
		assert.Equal(t, rt.requests[1].proxyAuth, "")
	})
}

// TestTransportProxySelector exercises the explicit Proxy selector: requests
// it routes direct skip proxy auth entirely, and the proxy cache is keyed by
// the proxy host so challenges for two different origins share it.
func TestTransportProxySelector(t *testing.T) {
	proxyChal := &Challenge{Realm: "proxy-realm", Nonce: "proxy-nonce", Algorithm: "MD5", QOP: []string{"auth"}}
	proxyURL, err := url.Parse("http://proxy.local:3128")
	assert.NilError(t, err)

	rt := &scriptedRoundTripper{
		responses: []*http.Response{
			challengeResponse(http.StatusProxyAuthRequired, proxyChal.String()),
			plainResponse(http.StatusOK),
			plainResponse(http.StatusOK), // second origin: cache hit, no challenge
			plainResponse(http.StatusOK), // direct request: no proxy header
		},
	}
	tr := &Transport{
		ProxyUsername: "puser",
		ProxyPassword: "ppass",
		Proxy: func(r *http.Request) (*url.URL, error) {
			if r.URL.Hostname() == "direct.example" {
				return nil, nil
			}
			return proxyURL, nil
		},
		Transport: rt,
	}

	req1, err := http.NewRequest(http.MethodGet, "http://a.example/", nil)
	assert.NilError(t, err)
	res, err := tr.RoundTrip(req1)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusOK)

	// different target host, same proxy: the proxy challenge must be reused
	req2, err := http.NewRequest(http.MethodGet, "http://b.example/", nil)
	assert.NilError(t, err)
	res, err = tr.RoundTrip(req2)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusOK)

	// direct connection: proxy auth is not attempted even with credentials set
	req3, err := http.NewRequest(http.MethodGet, "http://direct.example/", nil)
	assert.NilError(t, err)
	res, err = tr.RoundTrip(req3)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusOK)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, len(rt.requests), 4)
	assert.Equal(t, rt.requests[0].proxyAuth, "")
	checkCred(t, rt.requests[1].proxyAuth, proxyChal,
		http.MethodGet, "http://a.example/", "puser", "ppass")
	// cached proxy creds used against the second target without a new challenge
	checkCred(t, rt.requests[2].proxyAuth, proxyChal,
		http.MethodGet, "http://b.example/", "puser", "ppass")
	assert.Equal(t, rt.requests[3].proxyAuth, "")
}

// TestTransportProxyProbe verifies that a bodyless probe that surfaces the 407
// does not stream the body, while the authenticated replay carries it.
func TestTransportProxyProbe(t *testing.T) {
	proxyChal := &Challenge{Realm: "proxy-realm", Nonce: "proxy-nonce", Algorithm: "MD5", QOP: []string{"auth"}}
	rt := &scriptedRoundTripper{
		responses: []*http.Response{
			challengeResponse(http.StatusProxyAuthRequired, proxyChal.String()),
			plainResponse(http.StatusOK),
			plainResponse(http.StatusOK),
		},
	}
	tr := &Transport{
		ProxyUsername: "puser",
		ProxyPassword: "ppass",
		Probe:         true,
		Transport:     rt,
	}

	req, err := http.NewRequest(http.MethodPost, "http://example.com/upload", strings.NewReader("The Body"))
	assert.NilError(t, err)
	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusOK)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, len(rt.requests), 3)
	assert.Equal(t, rt.requests[0].body, "") // probe: body not streamed
	assert.Equal(t, rt.requests[0].proxyAuth, "")
	assert.Equal(t, rt.requests[1].body, "") // origin still uncached: probe again
	checkCred(t, rt.requests[1].proxyAuth, proxyChal,
		http.MethodPost, "http://example.com/upload", "puser", "ppass")
	assert.Equal(t, rt.requests[2].body, "The Body") // real upload, authenticated
	checkCred(t, rt.requests[2].proxyAuth, proxyChal,
		http.MethodPost, "http://example.com/upload", "puser", "ppass")
}
