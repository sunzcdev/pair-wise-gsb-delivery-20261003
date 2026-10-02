package digest

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// fakeRoundTripper is a scripted http.RoundTripper that never touches the
// network. Every request, along with its Authorization header, is recorded.
type fakeRoundTripper struct {
	requests []recordedRequest
	round    func(*http.Request) (*http.Response, error)
}

type recordedRequest struct {
	req  *http.Request
	auth string
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, recordedRequest{
		req:  req,
		auth: req.Header.Get("Authorization"),
	})
	return f.round(req)
}

func newResponse(req *http.Request, status int, header http.Header) *http.Response {
	return &http.Response{
		Status:     http.StatusText(status),
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
}

func challengeResponse(req *http.Request, chal *Challenge) *http.Response {
	h := http.Header{}
	h.Add("WWW-Authenticate", chal.String())
	return newResponse(req, http.StatusUnauthorized, h)
}

func okResponse(req *http.Request) *http.Response {
	return newResponse(req, http.StatusOK, nil)
}

func newChallenge(realm, nonce string, domains ...string) *Challenge {
	return &Challenge{
		Realm:     realm,
		Nonce:     nonce,
		Algorithm: "MD5",
		QOP:       []string{"auth"},
		Domain:    domains,
	}
}

// validAuth reports whether auth is a well-formed Digest response for chal.
func validAuth(auth string, chal *Challenge, req *http.Request) bool {
	if auth == "" {
		return false
	}
	cred, err := ParseCredentials(auth)
	if err != nil {
		return false
	}
	if cred.Nonce != chal.Nonce || cred.Realm != chal.Realm {
		return false
	}
	want, err := Digest(chal, Options{
		Method:   req.Method,
		URI:      req.URL.RequestURI(),
		Cnonce:   cred.Cnonce,
		Count:    cred.Nc,
		Username: "foo",
		Password: "bar",
	})
	if err != nil {
		return false
	}
	return cred.Response == want.Response
}

// newTransport wires a digest.Transport to a fake RoundTripper.
func newTransport(round func(*http.Request) (*http.Response, error)) (*Transport, *fakeRoundTripper) {
	fake := &fakeRoundTripper{round: round}
	return &Transport{
		Username:  "foo",
		Password:  "bar",
		Transport: fake,
	}, fake
}

func clientGet(t *testing.T, tr *Transport, rawurl string) (*http.Response, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawurl, nil)
	assert.NilError(t, err)
	n := 0
	if fake, ok := tr.Transport.(*fakeRoundTripper); ok {
		n = len(fake.requests)
	}
	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	return res, n
}

func fakeAuths(fake *fakeRoundTripper, from int) []string {
	var auths []string
	for _, r := range fake.requests[from:] {
		auths = append(auths, r.auth)
	}
	return auths
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	assert.NilError(t, err)
	return u
}

// checkOrChallenge answers 200 for valid credentials for chal, otherwise a
// 401 carrying chal.
func checkOrChallenge(req *http.Request, chal *Challenge) *http.Response {
	if validAuth(req.Header.Get("Authorization"), chal, req) {
		return okResponse(req)
	}
	return challengeResponse(req, chal)
}

// TestProtectionSpaceDifferentPorts verifies that challenges learned for the
// same hostname on different ports never share a nonce.
func TestProtectionSpaceDifferentPorts(t *testing.T) {
	chal8080 := newChallenge("realm-8080", "nonce-8080")
	chal9090 := newChallenge("realm-9090", "nonce-9090")
	tr, fake := newTransport(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Port() {
		case "8080":
			return checkOrChallenge(req, chal8080), nil
		case "9090":
			return checkOrChallenge(req, chal9090), nil
		default:
			return newResponse(req, http.StatusBadGateway, nil), nil
		}
	})

	// learn the :8080 space
	res, n := clientGet(t, tr, "http://example.com:8080/")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 2)
	assert.Equal(t, fake.requests[n].auth, "")
	assert.Equal(t, mustCred(t, fake.requests[n+1].auth).Nonce, "nonce-8080")

	// :8080 now pre-sends; :9090 must not reuse the :8080 nonce
	res, n = clientGet(t, tr, "http://example.com:8080/anything")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 1)
	assert.Assert(t, strings.HasPrefix(fake.requests[n].auth, "Digest "))
	cred, err := ParseCredentials(fake.requests[n].auth)
	assert.NilError(t, err)
	assert.Equal(t, cred.Nonce, "nonce-8080")

	res, n = clientGet(t, tr, "http://example.com:9090/")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 2)
	assert.Equal(t, fake.requests[n].auth, "")
	cred, err = ParseCredentials(fake.requests[n+1].auth)
	assert.NilError(t, err)
	assert.Equal(t, cred.Nonce, "nonce-9090")

	// subsequent :9090 request pre-sends its own nonce only
	res, n = clientGet(t, tr, "http://example.com:9090/x")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 1)
	cred, err = ParseCredentials(fake.requests[n].auth)
	assert.NilError(t, err)
	assert.Equal(t, cred.Nonce, "nonce-9090")
}

// TestProtectionSpaceTwoRealms verifies that two realms on one origin are
// cached independently and that each challenge's domain directive decides
// where its nonce is pre-sent.
func TestProtectionSpaceTwoRealms(t *testing.T) {
	users := newChallenge("users", "nonce-users", "/users/")
	admin := newChallenge("admin", "nonce-admin", "/admin/")
	tr, fake := newTransport(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(req.URL.Path, "/users/"):
			return checkOrChallenge(req, users), nil
		case strings.HasPrefix(req.URL.Path, "/admin/"):
			return checkOrChallenge(req, admin), nil
		default:
			return newResponse(req, http.StatusNotFound, nil), nil
		}
	})

	// learn both realms
	res, _ := clientGet(t, tr, "http://example.com/users/a")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	res, _ = clientGet(t, tr, "http://example.com/admin/b")
	assert.Equal(t, res.StatusCode, http.StatusOK)

	// each path pre-sends its own realm's nonce, in a single hop
	res, n := clientGet(t, tr, "http://example.com/users/c")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 1)
	cred := mustCred(t, fake.requests[n].auth)
	assert.Equal(t, cred.Nonce, "nonce-users")
	assert.Equal(t, cred.Realm, "users")

	res, n = clientGet(t, tr, "http://example.com/admin/d")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 1)
	cred = mustCred(t, fake.requests[n].auth)
	assert.Equal(t, cred.Nonce, "nonce-admin")
	assert.Equal(t, cred.Realm, "admin")

	// outside either domain: nothing is pre-sent
	res, n = clientGet(t, tr, "http://example.com/public")
	assert.Equal(t, res.StatusCode, http.StatusNotFound)
	assert.DeepEqual(t, fakeAuths(fake, n), []string{""})
}

func mustCred(t *testing.T, auth string) *Credentials {
	t.Helper()
	assert.Assert(t, auth != "", "expected an Authorization header")
	cred, err := ParseCredentials(auth)
	assert.NilError(t, err)
	return cred
}

// TestProtectionSpaceSegmentBoundaries verifies that domain matching happens
// on path-segment boundaries: /api covers /api and /api/x but not /apix.
func TestProtectionSpaceSegmentBoundaries(t *testing.T) {
	api := newChallenge("api", "nonce-api", "/api")
	tr, fake := newTransport(func(req *http.Request) (*http.Response, error) {
		return checkOrChallenge(req, api), nil
	})

	res, _ := clientGet(t, tr, "http://example.com/api")
	assert.Equal(t, res.StatusCode, http.StatusOK)

	cases := []struct {
		path string
		auth bool
		hops int
	}{
		{"/api", true, 1},
		{"/api/users", true, 1},
		{"/api/v2/widgets", true, 1},
		{"/apix", false, 2}, // adjacent path must not reuse the challenge
		{"/apixyz", false, 2},
		{"/other", false, 2},
	}
	for _, tc := range cases {
		res, n := clientGet(t, tr, "http://example.com"+tc.path)
		assert.Equal(t, res.StatusCode, http.StatusOK, tc.path)
		assert.Equal(t, len(fakeAuths(fake, n)), tc.hops, tc.path)
		if tc.auth {
			assert.Equal(t, mustCred(t, fake.requests[n].auth).Nonce, "nonce-api", tc.path)
		} else {
			assert.Equal(t, fake.requests[n].auth, "", tc.path)
		}
	}
}

// TestProtectionSpaceMostSpecific verifies that when several spaces cover a
// request, the one with the longest matching domain path wins, and that the
// origin-wide (domain-less) space is the fallback.
func TestProtectionSpaceMostSpecific(t *testing.T) {
	root := newChallenge("root", "nonce-root")
	v1 := newChallenge("v1", "nonce-v1", "/api/v1")
	v1Users := newChallenge("v1users", "nonce-v1users", "/api/v1/users")
	tr, fake := newTransport(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(req.URL.Path, "/api/v1/users"):
			return checkOrChallenge(req, v1Users), nil
		case strings.HasPrefix(req.URL.Path, "/api/v1"):
			return checkOrChallenge(req, v1), nil
		default:
			return checkOrChallenge(req, root), nil
		}
	})

	// learn each space from a path only it serves
	for _, c := range []struct {
		rawurl string
		chal   *Challenge
	}{
		{"http://example.com/health", root},
		{"http://example.com/api/v1/things", v1},
		{"http://example.com/api/v1/users/me", v1Users},
	} {
		res, n := clientGet(t, tr, c.rawurl)
		assert.Equal(t, res.StatusCode, http.StatusOK)
		assert.Equal(t, len(fakeAuths(fake, n)), 2, c.rawurl)
		assert.Assert(t, validAuth(fake.requests[n+1].auth, c.chal, fake.requests[n+1].req), c.rawurl)
	}

	cases := []struct {
		path  string
		nonce string
	}{
		{"/api/v1/users/me/posts", "nonce-v1users"}, // longest path wins
		{"/api/v1/users", "nonce-v1users"},
		{"/api/v1/things", "nonce-v1"},
		{"/api/v1", "nonce-v1"},
		{"/health", "nonce-root"}, // origin-wide fallback
		{"/", "nonce-root"},
	}
	for _, tc := range cases {
		res, n := clientGet(t, tr, "http://example.com"+tc.path)
		assert.Equal(t, res.StatusCode, http.StatusOK, tc.path)
		assert.Equal(t, len(fakeAuths(fake, n)), 1, tc.path)
		assert.Equal(t, mustCred(t, fake.requests[n].auth).Nonce, tc.nonce, tc.path)
	}
}

// TestProtectionSpaceTieBreak verifies the fixed tie-break: realms that cover
// the same paths with equal specificity resolve to the lexicographically
// smaller realm.
func TestProtectionSpaceTieBreak(t *testing.T) {
	zebra := newChallenge("zebra", "nonce-zebra", "/x/y")
	alpha := newChallenge("alpha", "nonce-alpha", "/x/y")
	tr, fake := newTransport(func(req *http.Request) (*http.Response, error) {
		var wantRealm string
		if req.Header.Get("X-Learn") == "zebra" {
			wantRealm = "zebra"
		} else if req.Header.Get("X-Learn") == "alpha" {
			wantRealm = "alpha"
		}
		if a := req.Header.Get("Authorization"); a != "" {
			if wantRealm == "" {
				if validAuth(a, zebra, req) || validAuth(a, alpha, req) {
					return okResponse(req), nil
				}
			} else {
				chal := zebra
				if wantRealm == "alpha" {
					chal = alpha
				}
				if validAuth(a, chal, req) {
					return okResponse(req), nil
				}
			}
		}
		if req.URL.Path == "/x/y" {
			h := http.Header{}
			if wantRealm == "" || wantRealm == "zebra" {
				h.Add("WWW-Authenticate", zebra.String())
			}
			if wantRealm == "" || wantRealm == "alpha" {
				h.Add("WWW-Authenticate", alpha.String())
			}
			return newResponse(req, http.StatusUnauthorized, h), nil
		}
		return newResponse(req, http.StatusNotFound, nil), nil
	})
	// learn one realm at a time via a custom FindChallenge hook
	var wantRealm string
	tr.FindChallenge = func(h http.Header) (*Challenge, error) {
		for _, value := range h.Values("WWW-Authenticate") {
			chal, err := ParseChallenge(value)
			if err == nil && chal.Realm == wantRealm {
				return chal, nil
			}
		}
		return nil, ErrNoChallenge
	}

	learn := func(realm string) {
		wantRealm = realm
		req, err := http.NewRequest(http.MethodGet, "http://example.com/x/y", nil)
		assert.NilError(t, err)
		req.Header.Set("X-Learn", realm)
		res, err := tr.RoundTrip(req)
		assert.NilError(t, err)
		assert.Equal(t, res.StatusCode, http.StatusOK)
	}
	learn("zebra")
	learn("alpha")

	res, n := clientGet(t, tr, "http://example.com/x/y")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Assert(t, len(fake.requests) >= 4)

	// both spaces cover /x/y with one path segment; "alpha" < "zebra"
	res, n = clientGet(t, tr, "http://example.com/x/y")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 1)
	assert.Equal(t, mustCred(t, fake.requests[n].auth).Realm, "alpha")
}

// TestProtectionSpaceCrossOriginDomain verifies that domain entries pointing
// at another origin are ignored: the challenge from :8080 mentioning :9090
// must never cause credentials to be pre-sent to :9090.
func TestProtectionSpaceCrossOriginDomain(t *testing.T) {
	chal8080 := newChallenge("realm-8080", "nonce-8080",
		"http://example.com:9090/leak", "/local")
	var seenOn9090 []string
	tr, fake := newTransport(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Port() {
		case "8080":
			if req.URL.Path == "/redirect" && req.Header.Get("Authorization") == "" {
				h := http.Header{}
				h.Set("Location", "http://example.com:9090/leak")
				return newResponse(req, http.StatusTemporaryRedirect, h), nil
			}
			return checkOrChallenge(req, chal8080), nil
		case "9090":
			seenOn9090 = append(seenOn9090, req.Header.Get("Authorization"))
			return okResponse(req), nil
		}
		return newResponse(req, http.StatusBadGateway, nil), nil
	})

	// learn the :8080 challenge from a path outside /local: the in-flight
	// retry still authenticates, but the cached space covers only /local
	res, n := clientGet(t, tr, "http://example.com:8080/discover")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 2)

	// the cross-origin domain entry must not leak credentials
	res, n = clientGet(t, tr, "http://example.com:9090/leak")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.DeepEqual(t, fakeAuths(fake, n), []string{""})
	assert.DeepEqual(t, seenOn9090, []string{""})

	// the same-origin domain entry still works
	res, n = clientGet(t, tr, "http://example.com:8080/local/thing")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 1)
	assert.Equal(t, mustCred(t, fake.requests[n].auth).Nonce, "nonce-8080")

	// a cross-origin redirect must not carry the old credentials either.
	// Simulate an http.Client following the redirect: the client issues a new
	// request to the Location URL, which matches no cached :8080 space.
	res, n = clientGet(t, tr, "http://example.com:8080/redirect")
	assert.Equal(t, res.StatusCode, http.StatusTemporaryRedirect)
	loc, err := res.Location()
	assert.NilError(t, err)
	assert.Equal(t, loc.Port(), "9090")
	assert.Equal(t, fake.requests[n].auth, "")

	// follow the redirect like http.Client does
	req, err := http.NewRequest(http.MethodGet, loc.String(), nil)
	assert.NilError(t, err)
	res, err = tr.RoundTrip(req)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, fake.requests[len(fake.requests)-1].auth, "")
}

// TestProtectionSpaceRelativeDomain verifies that relative domain URIs are
// resolved against the request that received the challenge.
func TestProtectionSpaceRelativeDomain(t *testing.T) {
	chal := newChallenge("rel", "nonce-rel", "./v1/")
	tr, fake := newTransport(func(req *http.Request) (*http.Response, error) {
		return checkOrChallenge(req, chal), nil
	})

	// challenge received at /api/challenge: ./v1/ resolves to /api/v1/
	res, n := clientGet(t, tr, "http://example.com/api/challenge")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 2)

	res, n = clientGet(t, tr, "http://example.com/api/v1/widgets")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 1)
	assert.Equal(t, mustCred(t, fake.requests[n].auth).Nonce, "nonce-rel")

	// /v1/ at the root is a different path prefix
	res, n = clientGet(t, tr, "http://example.com/v1/widgets")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 2)
	assert.Equal(t, fake.requests[n].auth, "")
}

// TestProtectionSpaceChallengeUpdate verifies that a new 401 replaces the
// stale nonce for the protection space and subsequent requests use it.
func TestProtectionSpaceChallengeUpdate(t *testing.T) {
	current := newChallenge("realm", "nonce-1")
	tr, fake := newTransport(func(req *http.Request) (*http.Response, error) {
		return checkOrChallenge(req, current), nil
	})

	res, n := clientGet(t, tr, "http://example.com/api")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, mustCred(t, fake.requests[n+1].auth).Nonce, "nonce-1")

	// pre-send works
	res, n = clientGet(t, tr, "http://example.com/api")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 1)

	// server rotates the nonce; the cached nonce fails once
	current = newChallenge("realm", "nonce-2")
	res, n = clientGet(t, tr, "http://example.com/api")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 2)
	assert.Equal(t, mustCred(t, fake.requests[n].auth).Nonce, "nonce-1")
	assert.Equal(t, mustCred(t, fake.requests[n+1].auth).Nonce, "nonce-2")

	// the new nonce is cached for the same protection space
	res, n = clientGet(t, tr, "http://example.com/api/x")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 1)
	assert.Equal(t, mustCred(t, fake.requests[n].auth).Nonce, "nonce-2")
}

// TestProtectionSpaceNoReuse verifies challenges are never pre-sent more
// than once when NoReuse is enabled, but the in-flight retry still works.
func TestProtectionSpaceNoReuse(t *testing.T) {
	chal := newChallenge("realm", "nonce-noreuse", "/api")
	tr, fake := newTransport(func(req *http.Request) (*http.Response, error) {
		return checkOrChallenge(req, chal), nil
	})
	tr.NoReuse = true

	for i := 0; i < 3; i++ {
		res, n := clientGet(t, tr, "http://example.com/api")
		assert.Equal(t, res.StatusCode, http.StatusOK)
		assert.Equal(t, len(fakeAuths(fake, n)), 2)
		assert.Equal(t, fake.requests[n].auth, "")
		assert.Equal(t, mustCred(t, fake.requests[n+1].auth).Nonce, "nonce-noreuse")
	}
}

// TestProtectionSpaceCustomHooks verifies custom FindChallenge and Digest
// hooks keep working with the new cache.
func TestProtectionSpaceCustomHooks(t *testing.T) {
	chal := newChallenge("custom-realm", "custom-nonce", "/api")
	tr, fake := newTransport(func(req *http.Request) (*http.Response, error) {
		if a := req.Header.Get("Authorization"); a != "" {
			cred, err := ParseCredentials(a)
			if err == nil && cred.Response == "custom-credentials" {
				return okResponse(req), nil
			}
		}
		h := http.Header{}
		h.Add("X-WWW-Authenticate", chal.String())
		return newResponse(req, http.StatusUnauthorized, h), nil
	})
	tr.FindChallenge = func(h http.Header) (*Challenge, error) {
		value := h.Get("X-WWW-Authenticate")
		if value == "" {
			return nil, ErrNoChallenge
		}
		return ParseChallenge(value)
	}
	tr.Digest = func(_ *http.Request, c *Challenge, o Options) (*Credentials, error) {
		return &Credentials{
			Username:  o.Username,
			Realm:     c.Realm,
			Nonce:     c.Nonce,
			URI:       o.URI,
			Algorithm: c.Algorithm,
			Opaque:    c.Opaque,
			QOP:       "auth",
			Cnonce:    "custom-cnonce",
			Nc:        o.Count,
			Response:  "custom-credentials",
		}, nil
	}

	res, n := clientGet(t, tr, "http://example.com/api")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 2)

	// custom Digest emits an Authorization header containing the marker
	res, n = clientGet(t, tr, "http://example.com/api/x")
	assert.Equal(t, res.StatusCode, http.StatusOK)
	assert.Equal(t, len(fakeAuths(fake, n)), 1)
	assert.Assert(t, strings.Contains(fake.requests[n].auth, "custom-credentials"))
}

func TestMatchesPath(t *testing.T) {
	cases := []struct {
		p, target string
		want      bool
	}{
		{"/api", "/api", true},
		{"/api", "/api/users", true},
		{"/api", "/apix", false},
		{"/api", "/apix/y", false},
		{"/api/", "/api/", true},
		{"/api/", "/api/x", true},
		{"/api/", "/apix", false},
		{"/", "/anything", true},
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/bb", false},
	}
	for _, tc := range cases {
		assert.Equal(t, matchesPath(tc.p, tc.target), tc.want, tc.p+" vs "+tc.target)
	}
}

func TestOriginOf(t *testing.T) {
	cases := []struct {
		rawurl string
		origin string
	}{
		{"http://example.com/", "http://example.com"},
		{"http://example.com:80/", "http://example.com"},
		{"https://example.com:443/", "https://example.com"},
		{"http://example.com:8080/", "http://example.com:8080"},
		{"http://Example.COM:8080/x", "http://example.com:8080"},
		{"http://[::1]:8080/", "http://[::1]:8080"},
	}
	for _, tc := range cases {
		assert.Equal(t, originOf(mustURL(t, tc.rawurl)), tc.origin, tc.rawurl)
	}
}
