package digest

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// scriptedTransport replays a fixed sequence of responses and records every
// request it sees, allowing per-hop header assertions.
type scriptedTransport struct {
	t    *testing.T
	hops []func(*http.Request) (*http.Response, error)
	got  []*http.Request
}

func (s *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	i := len(s.got)
	s.got = append(s.got, req)
	if i >= len(s.hops) {
		s.t.Fatalf("unexpected request #%d to %s", i+1, req.URL)
	}
	return s.hops[i](req)
}

func challengeResponse(req *http.Request, status int, header string, chal *Challenge) *http.Response {
	h := make(http.Header)
	h.Add(header, chal.String())
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
}

func okResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func readRequestBody(t *testing.T, r *http.Request) string {
	t.Helper()
	if r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(r.Body)
	assert.NilError(t, err)
	return string(body)
}

// expectCred parses a credentials header and verifies the digest response
// against the expected challenge, credentials and request URI.
func expectCred(t *testing.T, value string, chal *Challenge, user, pass, method, uri string) *Credentials {
	t.Helper()
	cred, err := ParseCredentials(value)
	assert.NilError(t, err)
	assert.Equal(t, cred.URI, uri)
	want, err := Digest(chal, Options{
		Method:   method,
		URI:      uri,
		Cnonce:   cred.Cnonce,
		Count:    cred.Nc,
		Username: user,
		Password: pass,
	})
	assert.NilError(t, err)
	assert.Equal(t, cred.Response, want.Response)
	assert.Equal(t, cred.Realm, chal.Realm)
	assert.Equal(t, cred.Nonce, chal.Nonce)
	return cred
}

func TestTransportProxyThenOrigin(t *testing.T) {
	const target = "http://origin.example.com/path?q=1"
	const absURI = "http://origin.example.com/path?q=1"
	const originURI = "/path?q=1"

	proxyChal := &Challenge{Realm: "proxy-realm", Nonce: "proxy-nonce", Algorithm: "MD5", QOP: []string{"auth"}}
	originChal := &Challenge{Realm: "origin-realm", Nonce: "origin-nonce", Algorithm: "MD5", QOP: []string{"auth"}}

	var proxyCredHop1, proxyCredHop2 *Credentials

	mt := &scriptedTransport{t: t, hops: []func(*http.Request) (*http.Response, error){
		// hop 1: no credentials of either kind
		func(r *http.Request) (*http.Response, error) {
			assert.Equal(t, r.Header.Get("Proxy-Authorization"), "")
			assert.Equal(t, r.Header.Get("Authorization"), "")
			return challengeResponse(r, http.StatusProxyAuthRequired, "Proxy-Authenticate", proxyChal), nil
		},
		// hop 2: only proxy credentials, absolute-form URI
		func(r *http.Request) (*http.Response, error) {
			assert.Equal(t, r.Header.Get("Authorization"), "")
			proxy := r.Header.Get("Proxy-Authorization")
			assert.Assert(t, proxy != "")
			proxyCredHop1 = expectCred(t, proxy, proxyChal, "puser", "ppass", http.MethodGet, absURI)
			return challengeResponse(r, http.StatusUnauthorized, "WWW-Authenticate", originChal), nil
		},
		// hop 3: both layers authenticated
		func(r *http.Request) (*http.Response, error) {
			proxy := r.Header.Get("Proxy-Authorization")
			origin := r.Header.Get("Authorization")
			assert.Assert(t, proxy != "")
			assert.Assert(t, origin != "")
			proxyCredHop2 = expectCred(t, proxy, proxyChal, "puser", "ppass", http.MethodGet, absURI)
			expectCred(t, origin, originChal, "ouser", "opass", http.MethodGet, originURI)
			return okResponse(r, "Hello World"), nil
		},
	}}

	tr := &Transport{
		Username:      "ouser",
		Password:      "opass",
		ProxyUsername: "puser",
		ProxyPassword: "ppass",
		Transport:     mt,
	}

	req, err := http.NewRequest(http.MethodGet, target, nil)
	assert.NilError(t, err)
	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusOK)
	body, err := io.ReadAll(res.Body)
	assert.NilError(t, err)
	assert.Equal(t, string(body), "Hello World")
	assert.Equal(t, len(mt.got), 3)

	// proxy nonce counter advanced across the retries of one logical request
	assert.Equal(t, proxyCredHop1.Nc, 1)
	assert.Equal(t, proxyCredHop2.Nc, 2)

	// the caller request and its headers were never mutated
	assert.Equal(t, req.Header.Get("Authorization"), "")
	assert.Equal(t, req.Header.Get("Proxy-Authorization"), "")
}

func TestTransportProxyRepeatedChallengeBounded(t *testing.T) {
	proxyChal := &Challenge{Realm: "proxy-realm", Nonce: "proxy-nonce", Algorithm: "MD5", QOP: []string{"auth"}}

	mt := &scriptedTransport{t: t, hops: []func(*http.Request) (*http.Response, error){
		func(r *http.Request) (*http.Response, error) {
			assert.Equal(t, r.Header.Get("Proxy-Authorization"), "")
			return challengeResponse(r, http.StatusProxyAuthRequired, "Proxy-Authenticate", proxyChal), nil
		},
		func(r *http.Request) (*http.Response, error) {
			assert.Assert(t, r.Header.Get("Proxy-Authorization") != "")
			assert.Equal(t, r.Header.Get("Authorization"), "")
			return challengeResponse(r, http.StatusProxyAuthRequired, "Proxy-Authenticate", proxyChal), nil
		},
	}}

	tr := &Transport{
		ProxyUsername: "puser",
		ProxyPassword: "ppass",
		Transport:     mt,
	}

	req, err := http.NewRequest(http.MethodGet, "http://origin.example.com/", nil)
	assert.NilError(t, err)
	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusProxyAuthRequired)
	// exactly one unauthenticated + one authenticated attempt, never unbounded
	assert.Equal(t, len(mt.got), 2)
}

func TestTransportProxyBodyNotReplayable(t *testing.T) {
	proxyChal := &Challenge{Realm: "proxy-realm", Nonce: "proxy-nonce", Algorithm: "MD5", QOP: []string{"auth"}}

	mt := &scriptedTransport{t: t, hops: []func(*http.Request) (*http.Response, error){
		func(r *http.Request) (*http.Response, error) {
			// the original body is streamed on the first attempt
			body, err := io.ReadAll(r.Body)
			assert.NilError(t, err)
			assert.Equal(t, string(body), "payload")
			return challengeResponse(r, http.StatusProxyAuthRequired, "Proxy-Authenticate", proxyChal), nil
		},
	}}

	tr := &Transport{
		ProxyUsername: "puser",
		ProxyPassword: "ppass",
		Transport:     mt,
	}

	req, err := http.NewRequest(http.MethodPost, "http://origin.example.com/upload",
		io.NopCloser(strings.NewReader("payload")))
	assert.NilError(t, err)
	req.GetBody = func() (io.ReadCloser, error) {
		return nil, errors.New("stream already consumed")
	}

	_, err = tr.RoundTrip(req)
	assert.Assert(t, err != nil)
	assert.ErrorIs(t, err, ErrBodyNotReplayable)
	// no empty-body retry was ever sent
	assert.Equal(t, len(mt.got), 1)
}

func TestTransportProxyAndOriginIndependentCaches(t *testing.T) {
	// both layers advertise the same realm name; caches and counters stay separate
	proxyChal := &Challenge{Realm: "shared", Nonce: "proxy-nonce", Algorithm: "MD5", QOP: []string{"auth"}}
	originChal := &Challenge{Realm: "shared", Nonce: "origin-nonce", Algorithm: "MD5", QOP: []string{"auth"}}

	mt := &scriptedTransport{t: t, hops: []func(*http.Request) (*http.Response, error){
		func(r *http.Request) (*http.Response, error) {
			return challengeResponse(r, http.StatusProxyAuthRequired, "Proxy-Authenticate", proxyChal), nil
		},
		func(r *http.Request) (*http.Response, error) {
			assert.Assert(t, r.Header.Get("Proxy-Authorization") != "")
			return challengeResponse(r, http.StatusUnauthorized, "WWW-Authenticate", originChal), nil
		},
		func(r *http.Request) (*http.Response, error) {
			return okResponse(r, "first"), nil
		},
		// second logical request: both challenges cached, single hop
		func(r *http.Request) (*http.Response, error) {
			proxy := expectCred(t, r.Header.Get("Proxy-Authorization"), proxyChal,
				"puser", "ppass", http.MethodGet, "http://origin.example.com/")
			origin := expectCred(t, r.Header.Get("Authorization"), originChal,
				"ouser", "opass", http.MethodGet, "/")
			// proxy counter: used on hops 2,3 then again here => 3
			assert.Equal(t, proxy.Nc, 3)
			// origin counter: used on hop 3 then here => 2
			assert.Equal(t, origin.Nc, 2)
			return okResponse(r, "second"), nil
		},
	}}

	tr := &Transport{
		Username:      "ouser",
		Password:      "opass",
		ProxyUsername: "puser",
		ProxyPassword: "ppass",
		Transport:     mt,
	}

	for i, want := range []string{"first", "second"} {
		req, err := http.NewRequest(http.MethodGet, "http://origin.example.com/", nil)
		assert.NilError(t, err)
		res, err := tr.RoundTrip(req)
		assert.NilError(t, err)
		body, err := io.ReadAll(res.Body)
		assert.NilError(t, err)
		assert.Equal(t, string(body), want, "request %d", i)
	}
	assert.Equal(t, len(mt.got), 4)
}

func TestTransportNoProxyConfigKeeps407(t *testing.T) {
	proxyChal := &Challenge{Realm: "proxy-realm", Nonce: "proxy-nonce", Algorithm: "MD5", QOP: []string{"auth"}}

	mt := &scriptedTransport{t: t, hops: []func(*http.Request) (*http.Response, error){
		func(r *http.Request) (*http.Response, error) {
			assert.Equal(t, r.Header.Get("Proxy-Authorization"), "")
			return challengeResponse(r, http.StatusProxyAuthRequired, "Proxy-Authenticate", proxyChal), nil
		},
	}}

	// origin credentials configured, but no proxy credentials: behavior is unchanged
	tr := &Transport{
		Username:  "ouser",
		Password:  "opass",
		Transport: mt,
	}

	req, err := http.NewRequest(http.MethodGet, "http://origin.example.com/", nil)
	assert.NilError(t, err)
	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusProxyAuthRequired)
	assert.Equal(t, len(mt.got), 1)
}

func TestTransportProxyProbeBodyOnceAuthenticated(t *testing.T) {
	proxyChal := &Challenge{Realm: "proxy-realm", Nonce: "proxy-nonce", Algorithm: "MD5", QOP: []string{"auth"}}
	originChal := &Challenge{Realm: "origin-realm", Nonce: "origin-nonce", Algorithm: "MD5", QOP: []string{"auth"}}

	var bodyLens []int
	mt := &scriptedTransport{t: t, hops: []func(*http.Request) (*http.Response, error){
		// discovery request: bodyless, no credentials
		func(r *http.Request) (*http.Response, error) {
			body := readRequestBody(t, r)
			bodyLens = append(bodyLens, len(body))
			assert.Equal(t, r.Header.Get("Proxy-Authorization"), "")
			return challengeResponse(r, http.StatusProxyAuthRequired, "Proxy-Authenticate", proxyChal), nil
		},
		// still discovering the origin challenge: probe stays bodyless
		func(r *http.Request) (*http.Response, error) {
			body := readRequestBody(t, r)
			bodyLens = append(bodyLens, len(body))
			assert.Assert(t, r.Header.Get("Proxy-Authorization") != "")
			assert.Equal(t, r.Header.Get("Authorization"), "")
			return challengeResponse(r, http.StatusUnauthorized, "WWW-Authenticate", originChal), nil
		},
		// both layers authenticated: real body streams once
		func(r *http.Request) (*http.Response, error) {
			body := readRequestBody(t, r)
			bodyLens = append(bodyLens, len(body))
			assert.Equal(t, string(body), "payload")
			assert.Assert(t, r.Header.Get("Proxy-Authorization") != "")
			assert.Assert(t, r.Header.Get("Authorization") != "")
			return okResponse(r, "done"), nil
		},
	}}

	tr := &Transport{
		Username:      "ouser",
		Password:      "opass",
		ProxyUsername: "puser",
		ProxyPassword: "ppass",
		Probe:         true,
		Transport:     mt,
	}

	req, err := http.NewRequest(http.MethodPost, "http://origin.example.com/upload", strings.NewReader("payload"))
	assert.NilError(t, err)
	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.DeepEqual(t, bodyLens, []int{0, 0, len("payload")})
	assert.Equal(t, len(mt.got), 3)
}

func TestTransportProxy407WithoutChallengeHeaderPassedThrough(t *testing.T) {
	mt := &scriptedTransport{t: t, hops: []func(*http.Request) (*http.Response, error){
		func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusProxyAuthRequired,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("nope")),
				Request:    r,
			}, nil
		},
	}}

	tr := &Transport{
		ProxyUsername: "puser",
		ProxyPassword: "ppass",
		Transport:     mt,
	}

	req, err := http.NewRequest(http.MethodGet, "http://origin.example.com/", nil)
	assert.NilError(t, err)
	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusProxyAuthRequired)
	assert.Equal(t, len(mt.got), 1)
}
