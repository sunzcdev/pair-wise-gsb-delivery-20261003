package digest

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// cchal is a cached challenge and the number of times it's been used.
type cchal struct {
	c       *Challenge
	n       int
	origin  string
	domains []string
}

// spaceKey identifies a Digest protection space.
// A protection space is defined by the origin (scheme + host + port) the
// challenge was received from and its realm, as specified by RFC 7616 3.2.
type spaceKey struct {
	origin string
	realm  string
}

// originOf returns the scheme + host + port that defines the origin for url.
// Default ports are omitted so that http://example.com and
// http://example.com:80 are the same origin; different ports are not.
func originOf(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port == "" {
		return scheme + "://" + host
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// newCchal caches a challenge received in response to req.
//
// Per RFC 7616 section 3.2, each URI in the challenge's domain directive
// names a protection space on the server where the credentials may be used.
// Relative URIs are resolved against the request that received the challenge.
// Entries that resolve to a different origin are ignored: pre-sending
// credentials to them would leak credentials to another protection space
// (e.g. across a cross-origin redirect).
//
// A challenge without a domain directive applies to the entire origin.
// RFC 7616 3.2: "If this directive is not present ... the protection space
// consists of all URIs on the responding server." A directive whose entries
// are all relative-to-nowhere or cross-origin, on the other hand, matches
// nothing on this origin.
func newCchal(req *http.Request, chal *Challenge) *cchal {
	origin := originOf(req.URL)
	cc := &cchal{c: chal, origin: origin}
	if len(chal.Domain) > 0 {
		// distinguish "directive present but nothing usable" from
		// "directive absent" (origin-wide).
		cc.domains = []string{}
	}
	seen := map[string]bool{}
	for _, d := range chal.Domain {
		ref, err := url.Parse(d)
		if err != nil {
			continue
		}
		u := req.URL.ResolveReference(ref)
		// ignore domain entries that escape the original origin
		if originOf(u) != origin {
			continue
		}
		p := u.Path
		if p == "" {
			p = "/"
		}
		if !seen[p] {
			seen[p] = true
			cc.domains = append(cc.domains, p)
		}
	}
	slices.Sort(cc.domains)
	return cc
}

// matchesPath reports whether the domain path p covers the request path.
// Matching is performed on path-segment boundaries: p = "/api" matches
// "/api" and "/api/users", but not "/apix".
func matchesPath(p, target string) bool {
	if p == "/" || p == target || strings.HasPrefix(target, p+"/") {
		return true
	}
	if strings.HasSuffix(p, "/") {
		return strings.HasPrefix(target, p)
	}
	return false
}

// specificity ranks how specifically a cached challenge covers a request:
// the longest matching domain path wins, measured by the number of its
// non-empty path segments. An origin-wide space (no domain directive) has
// specificity zero and is only selected when nothing more specific matches.
func (cc *cchal) specificity(target string) (int, bool) {
	if len(cc.domains) == 0 {
		return 0, true
	}
	best := -1
	for _, p := range cc.domains {
		if !matchesPath(p, target) {
			continue
		}
		if n := len(strings.FieldsFunc(strings.TrimPrefix(p, "/"), func(r rune) bool {
			return r == '/'
		})); n > best {
			best = n
		}
	}
	if best < 0 {
		return 0, false
	}
	return best, true
}

// betterSpace reports whether candidate wins over current for a request.
// The most specific matching path is chosen. Ties (same specificity and
// matched-path length) are broken by realm, then origin, in ascending order
// so selection is deterministic.
func betterSpace(candSpec, curSpec int, candPathLen, curPathLen int, candKey, curKey spaceKey) bool {
	if candSpec != curSpec {
		return candSpec > curSpec
	}
	if candPathLen != curPathLen {
		return candPathLen > curPathLen
	}
	if candKey.realm != curKey.realm {
		return candKey.realm < curKey.realm
	}
	return candKey.origin < curKey.origin
}

// Transport implements http.RoundTripper
type Transport struct {
	Username string
	Password string

	// Digest computes the digest credentials.
	// If nil, the Digest function is used.
	Digest func(*http.Request, *Challenge, Options) (*Credentials, error)

	// FindChallenge extracts the challenge from the request headers.
	// If nil, the FindChallenge function is used.
	FindChallenge func(http.Header) (*Challenge, error)

	// Transport specifies the mechanism by which individual
	// HTTP requests are made.
	// If nil, DefaultTransport is used.
	Transport http.RoundTripper

	// Jar specifies the cookie jar.
	//
	// The Jar is used to insert relevant cookies into every
	// outbound Request and is updated with the cookie values
	// of every inbound Response. The Jar is consulted for every
	// redirect that the Client follows.
	//
	// If Jar is nil, cookies are only sent if they are explicitly
	// set on the Request.
	Jar http.CookieJar

	// NoReuse prevents the transport from reusing challenges.
	NoReuse bool

	// Probe discovers the challenge with a bodyless request before streaming the
	// body, so it's only sent once authenticated — for servers that reject the
	// unauthenticated request without draining the body. Applies only when uncached.
	Probe bool

	// cache of challenges indexed by protection space (origin + realm)
	cache   map[spaceKey]*cchal
	cacheMu sync.Mutex
}

// save parses the digest challenge from the response and stores it as a new
// protection space. hadChallenge/failedKey identify the protection space
// pre-sent on the failed request; it is invalidated when the response carries
// no usable new challenge. The returned challenge is used for the in-flight
// retry, even when the request path falls outside the challenge's domain
// scope: that retry is a direct response to the challenge, not a pre-send.
func (t *Transport) save(res *http.Response, failedKey spaceKey, hadChallenge bool) (*cchal, error) {
	// save cookies
	if t.Jar != nil {
		t.Jar.SetCookies(res.Request.URL, res.Cookies())
	}
	// find and save digest challenge
	find := t.FindChallenge
	if find == nil {
		find = FindChallenge
	}
	chal, err := find(res.Header)
	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()
	if t.cache == nil {
		t.cache = map[spaceKey]*cchal{}
	}
	if err != nil {
		// the pre-sent challenge didn't work; drop its protection space.
		if hadChallenge {
			delete(t.cache, failedKey)
		}
		return nil, err
	}
	cc := newCchal(res.Request, chal)
	t.cache[spaceKey{origin: cc.origin, realm: chal.Realm}] = cc
	return cc, nil
}

// digest creates credentials from the cached challenge
func (t *Transport) digest(req *http.Request, chal *Challenge, count int) (*Credentials, error) {
	opt := Options{
		Method:   req.Method,
		URI:      req.URL.RequestURI(),
		GetBody:  req.GetBody,
		Count:    count,
		Username: t.Username,
		Password: t.Password,
	}
	if t.Digest != nil {
		return t.Digest(req, chal, opt)
	}
	return Digest(chal, opt)
}

// lockedChallenge selects the most specific cached protection space covering
// req. If NoReuse is set, the selected entry is consumed (deleted from the
// cache) so it cannot be pre-sent again.
func (t *Transport) lockedChallenge(req *http.Request) (*Challenge, int, spaceKey, bool) {
	if t.cache == nil {
		return nil, 0, spaceKey{}, false
	}
	origin := originOf(req.URL)
	target := req.URL.Path
	if target == "" {
		target = "/"
	}
	type candidate struct {
		key     spaceKey
		cc      *cchal
		spec    int
		pathLen int
	}
	var best candidate
	found := false
	for key, cc := range t.cache {
		// challenges are never pre-sent to another origin
		if key.origin != origin {
			continue
		}
		spec, ok := cc.specificity(target)
		if !ok {
			continue
		}
		pathLen := 0
		for _, p := range cc.domains {
			if matchesPath(p, target) && len(p) > pathLen {
				pathLen = len(p)
			}
		}
		if !found || betterSpace(spec, best.spec, pathLen, best.pathLen, key, best.key) {
			best = candidate{key: key, cc: cc, spec: spec, pathLen: pathLen}
			found = true
		}
	}
	if !found {
		return nil, 0, spaceKey{}, false
	}
	if t.NoReuse {
		delete(t.cache, best.key)
	}
	best.cc.n++
	return best.cc.c, best.cc.n, best.key, true
}

// lockedConsume returns the specific cached challenge for the provided
// protection-space key, consuming it when NoReuse is set. It's used for the
// in-flight retry directly against the just-received challenge.
func (t *Transport) lockedConsume(key spaceKey) (*Challenge, int, bool) {
	cc, ok := t.cache[key]
	if !ok {
		return nil, 0, false
	}
	if t.NoReuse {
		delete(t.cache, key)
	}
	cc.n++
	return cc.c, cc.n, true
}

// authorize builds credentials from cc and attaches them to req.
func (t *Transport) authorize(req *http.Request, chal *Challenge, count int) error {
	cred, err := t.digest(req, chal, count)
	if err != nil {
		return err
	}
	if cred != nil {
		req.Header.Set("Authorization", cred.String())
	}
	return nil
}

// prepare sets the Authorization header from the best matching cached
// protection space. It reports whether the caller should send a bodyless
// probe first (Probe set, request with a body, no challenge cached yet), the
// key of the selected protection space, and whether one was selected.
func (t *Transport) prepare(req *http.Request) (probe bool, key spaceKey, ok bool, err error) {
	// add cookies
	if t.Jar != nil {
		for _, cookie := range t.Jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
	}
	// pre-send auth from the best matching cached protection space
	t.cacheMu.Lock()
	chal, count, key, ok := t.lockedChallenge(req)
	t.cacheMu.Unlock()
	if !ok {
		// no cached challenge yet: probe bodyless first when configured
		bodied := req.Body != nil && req.Body != http.NoBody
		return t.Probe && bodied, spaceKey{}, false, nil
	}
	return false, key, true, t.authorize(req, chal, count)
}

// addCookies attaches jar cookies for the request URL.
func (t *Transport) addCookies(req *http.Request) {
	if t.Jar != nil {
		for _, cookie := range t.Jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
	}
}

// RoundTrip will try to authorize the request using a cached challenge.
// If that doesn't work and we receive a 401, we'll try again using that challenge.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// use the configured transport if there is one
	tr := t.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	// don't modify the original request
	clone, err := cloner(req)
	if err != nil {
		return nil, err
	}
	// make a copy of the request
	first, err := clone()
	if err != nil {
		return nil, err
	}
	// prepare the first request using a cached challenge
	probe, failedKey, hadChallenge, err := t.prepare(first)
	if err != nil {
		return nil, err
	}
	// probe bodyless so the body is only streamed on the authenticated retry
	if probe {
		first.Body = nil
		first.ContentLength = 0
		first.GetBody = nil
	}
	// the first request will either succeed or return a 401
	res, err := tr.RoundTrip(first)
	if err != nil {
		return res, err
	}
	if res.StatusCode != http.StatusUnauthorized {
		if !probe {
			return res, nil
		}
		// a probe response isn't valid for the caller since the body was
		// stripped; discard it and send the real request as-is
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		second, err := clone()
		if err != nil {
			return nil, err
		}
		return tr.RoundTrip(second)
	}
	// drain and close the first message body
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	// save the challenge for future use
	cc, err := t.save(res, failedKey, hadChallenge)
	if err != nil {
		if err == ErrNoChallenge {
			return res, nil
		}
		return nil, err
	}
	// make a second copy of the request
	second, err := clone()
	if err != nil {
		return nil, err
	}
	t.addCookies(second)
	// authenticate the retry directly against the challenge just received.
	// It may be for a path outside the challenge's domain scope, but this
	// request is what the challenge was issued in response to, so using it
	// does not leak credentials anywhere new.
	t.cacheMu.Lock()
	retry, retryCount, found := t.lockedConsume(spaceKey{origin: cc.origin, realm: cc.c.Realm})
	t.cacheMu.Unlock()
	if !found {
		// NoReuse consumed it during prepare, or another goroutine did:
		// use the freshly learned challenge directly.
		retry = cc.c
		retryCount = 1
	}
	if err := t.authorize(second, retry, retryCount); err != nil {
		return nil, err
	}
	return tr.RoundTrip(second)
}

// CloseIdleConnections delegates the call to the underlying transport.
func (t *Transport) CloseIdleConnections() {
	tr := t.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	type closeIdler interface {
		CloseIdleConnections()
	}
	if tr, ok := tr.(closeIdler); ok {
		tr.CloseIdleConnections()
	}
}

// cloner returns a function which makes clones of the provided request
func cloner(req *http.Request) (func() (*http.Request, error), error) {
	getbody := req.GetBody
	// if there's no GetBody function set we have to copy the body
	// into memory to use for future clones
	if getbody == nil {
		if req.Body == nil || req.Body == http.NoBody {
			getbody = func() (io.ReadCloser, error) {
				return http.NoBody, nil
			}
		} else {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			if err := req.Body.Close(); err != nil {
				return nil, err
			}
			getbody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		}
	}
	return func() (*http.Request, error) {
		clone := req.Clone(req.Context())
		body, err := getbody()
		if err != nil {
			return nil, err
		}
		clone.Body = body
		clone.GetBody = getbody
		return clone, nil
	}, nil
}
