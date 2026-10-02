package digest

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// recordingTransport is a fake http.RoundTripper which records the URL and
// Authorization header of every request and answers via its handler.
// It never touches the network.
type recordingTransport struct {
	handler func(*http.Request) *http.Response
	auths   []string
	urls    []string
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.auths = append(rt.auths, req.Header.Get("Authorization"))
	rt.urls = append(rt.urls, req.URL.String())
	res := rt.handler(req)
	res.Request = req
	return res, nil
}

func respond(status int, chal *Challenge) *http.Response {
	header := http.Header{}
	if chal != nil {
		header.Set("WWW-Authenticate", chal.String())
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d", status),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

// challenging returns a handler which answers 200 to authorized requests and
// otherwise issues a 401 with the challenge of the first matching rule.
// Requests matching no rule are public and answered 200.
func challenging(rules ...struct {
	prefix string
	chal   *Challenge
},
) func(*http.Request) *http.Response {
	return func(req *http.Request) *http.Response {
		if req.Header.Get("Authorization") != "" {
			return respond(http.StatusOK, nil)
		}
		for _, rule := range rules {
			if matchPath(rule.prefix, req.URL.Path) {
				return respond(http.StatusUnauthorized, rule.chal)
			}
		}
		return respond(http.StatusOK, nil)
	}
}

func rule(prefix string, chal *Challenge) struct {
	prefix string
	chal   *Challenge
} {
	return struct {
		prefix string
		chal   *Challenge
	}{prefix, chal}
}

func do(t *testing.T, tr *Transport, rawurl string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawurl, nil)
	assert.NilError(t, err)
	res, err := tr.RoundTrip(req)
	assert.NilError(t, err)
	_ = res.Body.Close()
}

func authNonce(t *testing.T, header string) string {
	t.Helper()
	cred, err := ParseCredentials(header)
	assert.NilError(t, err)
	return cred.Nonce
}

// A challenge cached for one port must not be pre-emptively sent to another
// port on the same host: origins differ.
func TestTransportSpacePorts(t *testing.T) {
	rt := &recordingTransport{
		handler: func(req *http.Request) *http.Response {
			if req.Header.Get("Authorization") != "" {
				return respond(http.StatusOK, nil)
			}
			return respond(http.StatusUnauthorized, &Challenge{
				Realm:     "svc",
				Nonce:     "nonce-" + req.URL.Port(),
				Algorithm: "MD5",
				QOP:       []string{"auth"},
			})
		},
	}
	tr := &Transport{Username: "u", Password: "p", Transport: rt}

	// seed the cache for :8080 (no domain directive => whole origin)
	do(t, tr, "http://example.com:8080/api")
	// pre-emptive auth on the same origin, different path
	do(t, tr, "http://example.com:8080/other")
	// a different port is a different origin: no pre-emptive auth
	do(t, tr, "http://example.com:9090/api")

	assert.Equal(t, len(rt.auths), 5)
	assert.Equal(t, rt.auths[0], "") // :8080 discovery
	assert.Equal(t, authNonce(t, rt.auths[1]), "nonce-8080")
	assert.Equal(t, authNonce(t, rt.auths[2]), "nonce-8080") // pre-emptive, first try
	assert.Equal(t, rt.auths[3], "")                         // :9090 gets nothing cached
	assert.Equal(t, authNonce(t, rt.auths[4]), "nonce-9090")
}

// Two realms on the same origin keep separate challenges and are selected by
// the domain directive.
func TestTransportSpaceRealms(t *testing.T) {
	rt := &recordingTransport{
		handler: challenging(
			rule("/api", &Challenge{Realm: "api", Nonce: "nonce-api", Domain: []string{"/api"}, Algorithm: "MD5", QOP: []string{"auth"}}),
			rule("/admin", &Challenge{Realm: "admin", Nonce: "nonce-admin", Domain: []string{"/admin"}, Algorithm: "MD5", QOP: []string{"auth"}}),
		),
	}
	tr := &Transport{Username: "u", Password: "p", Transport: rt}

	// seed both protection spaces
	do(t, tr, "http://example.com/api/x")
	do(t, tr, "http://example.com/admin/x")
	// pre-emptive requests pick the right realm
	do(t, tr, "http://example.com/api/y")
	do(t, tr, "http://example.com/admin/y")
	// outside both domains: nothing cached
	do(t, tr, "http://example.com/public")

	assert.Equal(t, len(rt.auths), 7)
	assert.Equal(t, authNonce(t, rt.auths[4]), "nonce-api")
	assert.Equal(t, authNonce(t, rt.auths[5]), "nonce-admin")
	assert.Equal(t, rt.auths[6], "")
}

// Path matching is segment-aware: "/api" covers "/api" and "/api/x" but not
// the adjacent "/apix".
func TestTransportSpacePathBoundary(t *testing.T) {
	rt := &recordingTransport{
		handler: challenging(
			rule("/api", &Challenge{Realm: "r", Nonce: "n", Domain: []string{"/api"}, Algorithm: "MD5", QOP: []string{"auth"}}),
		),
	}
	tr := &Transport{Username: "u", Password: "p", Transport: rt}

	do(t, tr, "http://example.com/api/x") // seed: "", auth
	do(t, tr, "http://example.com/api")   // exact match: pre-emptive
	do(t, tr, "http://example.com/api/y") // descendant: pre-emptive
	do(t, tr, "http://example.com/apix")  // adjacent path: no auth

	assert.Equal(t, len(rt.auths), 5)
	assert.Equal(t, rt.auths[0], "")
	assert.Assert(t, rt.auths[1] != "")
	assert.Assert(t, rt.auths[2] != "")
	assert.Assert(t, rt.auths[3] != "")
	assert.Equal(t, rt.auths[4], "")
}

// Domain entries pointing outside the original origin are ignored, and a
// cross-origin redirect never carries the cached credentials.
func TestTransportSpaceCrossOriginDomain(t *testing.T) {
	rt := &recordingTransport{
		handler: func(req *http.Request) *http.Response {
			if req.Header.Get("Authorization") != "" {
				return respond(http.StatusOK, nil)
			}
			switch req.URL.Hostname() {
			case "a.test":
				if req.URL.Path == "/other" {
					return respond(http.StatusOK, nil) // public
				}
				return respond(http.StatusUnauthorized, &Challenge{
					Realm:     "r",
					Nonce:     "nonce-a",
					Domain:    []string{"http://evil.test/stolen", "/ok"},
					Algorithm: "MD5",
					QOP:       []string{"auth"},
				})
			default:
				return respond(http.StatusUnauthorized, &Challenge{
					Realm:     "r",
					Nonce:     "nonce-evil",
					Algorithm: "MD5",
					QOP:       []string{"auth"},
				})
			}
		},
	}
	tr := &Transport{Username: "u", Password: "p", Transport: rt}

	do(t, tr, "http://a.test/ok")        // seed: the evil.test domain entry is dropped
	do(t, tr, "http://a.test/ok/more")   // inside "/ok": pre-emptive
	do(t, tr, "http://a.test/other")     // outside the domain: no auth
	do(t, tr, "http://evil.test/stolen") // cross-origin: no cached credentials

	assert.Equal(t, len(rt.auths), 6)
	assert.Equal(t, authNonce(t, rt.auths[2]), "nonce-a")
	assert.Equal(t, rt.auths[3], "")
	assert.Equal(t, rt.auths[4], "") // nothing leaks to evil.test
	assert.Equal(t, authNonce(t, rt.auths[5]), "nonce-evil")
	for i, rawurl := range rt.urls {
		if strings.Contains(rawurl, "evil.test") {
			assert.Assert(t, !strings.Contains(rt.auths[i], "nonce-a"))
		}
	}
}

// A new challenge for the same origin and realm replaces the cached one and
// resets the nonce count.
func TestTransportSpaceChallengeUpdate(t *testing.T) {
	nonce := "n1"
	rt := &recordingTransport{
		handler: func(req *http.Request) *http.Response {
			chal := &Challenge{Realm: "r", Nonce: nonce, Algorithm: "MD5", QOP: []string{"auth"}}
			if auth := req.Header.Get("Authorization"); auth != "" {
				cred, err := ParseCredentials(auth)
				assert.NilError(t, err)
				if cred.Nonce == nonce {
					return respond(http.StatusOK, nil)
				}
				chal.Stale = true
			}
			return respond(http.StatusUnauthorized, chal)
		},
	}
	tr := &Transport{Username: "u", Password: "p", Transport: rt}

	do(t, tr, "http://example.com/x") // seed n1
	nonce = "n2"                      // server rotates the nonce
	do(t, tr, "http://example.com/x") // stale n1 -> update -> n2
	do(t, tr, "http://example.com/x") // pre-emptive n2

	assert.Equal(t, len(rt.auths), 5)
	cred1, err := ParseCredentials(rt.auths[1])
	assert.NilError(t, err)
	assert.Equal(t, cred1.Nonce, "n1")
	assert.Equal(t, cred1.Nc, 1)
	cred2, err := ParseCredentials(rt.auths[2])
	assert.NilError(t, err)
	assert.Equal(t, cred2.Nonce, "n1")
	assert.Equal(t, cred2.Nc, 2)
	cred3, err := ParseCredentials(rt.auths[3])
	assert.NilError(t, err)
	assert.Equal(t, cred3.Nonce, "n2")
	assert.Equal(t, cred3.Nc, 1) // count resets with the new challenge
	cred4, err := ParseCredentials(rt.auths[4])
	assert.NilError(t, err)
	assert.Equal(t, cred4.Nonce, "n2")
	assert.Equal(t, cred4.Nc, 2)
}

// The most specific matching path wins.
func TestTransportSpaceMostSpecific(t *testing.T) {
	rt := &recordingTransport{
		handler: challenging(
			rule("/api", &Challenge{Realm: "api", Nonce: "n-api", Domain: []string{"/api"}, Algorithm: "MD5", QOP: []string{"auth"}}),
			rule("/other", &Challenge{Realm: "root", Nonce: "n-root", Domain: []string{"/"}, Algorithm: "MD5", QOP: []string{"auth"}}),
		),
	}
	tr := &Transport{Username: "u", Password: "p", Transport: rt}

	do(t, tr, "http://example.com/api/x")    // seed "api"
	do(t, tr, "http://example.com/other")    // seed "root" (domain "/")
	do(t, tr, "http://example.com/api/more") // "/api" beats "/"
	do(t, tr, "http://example.com/else")     // only "/" matches

	assert.Equal(t, authNonce(t, rt.auths[len(rt.auths)-2]), "n-api")
	assert.Equal(t, authNonce(t, rt.auths[len(rt.auths)-1]), "n-root")
}

// Equal specificity is broken by lexical realm order so the choice is stable.
func TestTransportSpaceTieBreak(t *testing.T) {
	tr := &Transport{
		cache: map[spaceKey]*space{
			{scheme: "http", host: "example.com:80", realm: "bbb"}: {
				chal:  &Challenge{Realm: "bbb", Nonce: "n-bbb"},
				paths: []string{"/dup"},
			},
			{scheme: "http", host: "example.com:80", realm: "aaa"}: {
				chal:  &Challenge{Realm: "aaa", Nonce: "n-aaa"},
				paths: []string{"/dup"},
			},
		},
	}
	u, err := url.Parse("http://example.com/dup/x")
	assert.NilError(t, err)
	_, sp := tr.lookup(u)
	assert.Assert(t, sp != nil)
	assert.Equal(t, sp.chal.Nonce, "n-aaa")
}

// NoReuse consumes a cached challenge after a single use, so every request
// starts unauthenticated.
func TestTransportSpaceNoReuse(t *testing.T) {
	rt := &recordingTransport{
		handler: challenging(
			rule("/", &Challenge{Realm: "r", Nonce: "n", Algorithm: "MD5", QOP: []string{"auth"}}),
		),
	}
	tr := &Transport{Username: "u", Password: "p", Transport: rt, NoReuse: true}

	do(t, tr, "http://example.com/x")
	do(t, tr, "http://example.com/x")

	assert.Equal(t, len(rt.auths), 4)
	assert.Equal(t, rt.auths[0], "")
	assert.Assert(t, rt.auths[1] != "")
	assert.Equal(t, rt.auths[2], "")
	assert.Assert(t, rt.auths[3] != "")
}

// Custom FindChallenge and Digest hooks feed into the same protection-space
// cache: pre-emptive auth still follows the domain directive.
func TestTransportSpaceCustomHooks(t *testing.T) {
	rt := &recordingTransport{
		handler: func(req *http.Request) *http.Response {
			if req.Header.Get("Authorization") != "" {
				return respond(http.StatusOK, nil)
			}
			if !strings.HasPrefix(req.URL.Path, "/api") {
				return respond(http.StatusOK, nil)
			}
			chal := &Challenge{Realm: "r", Nonce: "n", Domain: []string{"/api"}, Algorithm: "MD5", QOP: []string{"auth"}}
			res := respond(http.StatusUnauthorized, nil)
			res.Header.Set("X-Custom-Auth", chal.String())
			return res
		},
	}
	var digests int
	tr := &Transport{
		Username:  "u",
		Password:  "p",
		Transport: rt,
		FindChallenge: func(h http.Header) (*Challenge, error) {
			value := h.Get("X-Custom-Auth")
			if value == "" {
				return nil, ErrNoChallenge
			}
			return ParseChallenge(value)
		},
		Digest: func(req *http.Request, chal *Challenge, opt Options) (*Credentials, error) {
			digests++
			return Digest(chal, opt)
		},
	}

	do(t, tr, "http://example.com/api/x") // discovery + retry: 1 digest
	do(t, tr, "http://example.com/api/y") // pre-emptive: 1 digest
	do(t, tr, "http://example.com/apix")  // no match: no digest

	assert.Equal(t, digests, 2)
	assert.Equal(t, rt.auths[0], "")
	assert.Assert(t, rt.auths[1] != "")
	assert.Assert(t, rt.auths[2] != "")
	assert.Equal(t, rt.auths[3], "")
}
