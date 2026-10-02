package digest

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// spaceKey identifies a digest protection space: an origin plus a realm.
// The host always carries an explicit port (defaulting to 80/443), so
// services on different ports of the same host never share a challenge.
type spaceKey struct {
	scheme string
	host   string
	realm  string
}

// space is a cached challenge, the number of times it's been used, and the
// request paths it applies to. A nil paths slice means the whole origin.
type space struct {
	chal  *Challenge
	n     int
	paths []string
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

	// cache of protection spaces indexed by origin and realm
	cache   map[spaceKey]*space
	cacheMu sync.Mutex
}

// originOf returns the canonical scheme and host:port pair for a URL.
func originOf(u *url.URL) (scheme, host string) {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()) + ":" + port
}

// resolveDomain resolves the challenge's domain directive against the URL of
// the request which received it. Entries which resolve outside the original
// origin are ignored so credentials can never follow them off-site. It
// returns nil when the directive is absent, empty, or contains no usable
// same-origin entries — per RFC 7616 §3.3 the protection space then covers
// every URI on the responding origin.
func resolveDomain(base *url.URL, domain []string) []string {
	if len(domain) == 0 {
		return nil
	}
	bscheme, bhost := originOf(base)
	var paths []string
	for _, d := range domain {
		ref, err := url.Parse(d)
		if err != nil {
			continue
		}
		u := base.ResolveReference(ref)
		scheme, host := originOf(u)
		if scheme != bscheme || host != bhost {
			continue
		}
		path := u.Path
		if path == "" {
			path = "/"
		}
		paths = append(paths, path)
	}
	return paths
}

// matchPath reports whether the request path p falls inside the
// protection-space path prefix. Matching is segment-aware: "/api" matches
// "/api" and "/api/x" but not "/apix".
func matchPath(prefix, p string) bool {
	if prefix == "" || prefix == "/" {
		return true
	}
	if p == prefix {
		return true
	}
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	return prefix[len(prefix)-1] == '/' || p[len(prefix)] == '/'
}

// specificity returns the length of the space's longest path prefix matching
// p, or -1 when the space does not cover p at all. A space covering the
// whole origin (nil paths) matches with specificity 0.
func (s *space) specificity(p string) int {
	if s.paths == nil {
		return 0
	}
	best := -1
	for _, prefix := range s.paths {
		if matchPath(prefix, p) && len(prefix) > best {
			best = len(prefix)
		}
	}
	return best
}

// save parses the digest challenge from the response
// and adds it to the cache
func (t *Transport) save(res *http.Response) error {
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
		t.cache = map[spaceKey]*space{}
	}
	scheme, host := originOf(res.Request.URL)
	if err != nil {
		// if save is being invoked, the existing cached challenges for
		// this origin didn't work
		for key := range t.cache {
			if key.scheme == scheme && key.host == host {
				delete(t.cache, key)
			}
		}
		return err
	}
	key := spaceKey{scheme: scheme, host: host, realm: chal.Realm}
	t.cache[key] = &space{
		chal:  chal,
		paths: resolveDomain(res.Request.URL, chal.Domain),
	}
	return nil
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

// challenge returns a cached challenge and count for the provided request
func (t *Transport) challenge(req *http.Request) (*Challenge, int, bool) {
	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()
	key, sp := t.lookup(req.URL)
	if sp == nil {
		return nil, 0, false
	}
	if t.NoReuse {
		delete(t.cache, key)
	}
	sp.n++
	return sp.chal, sp.n, true
}

// lookup finds the cached protection space covering u. When several spaces
// match, the one with the most specific (longest) matching path prefix wins;
// ties are broken by the lexical order of the realm so the choice is stable.
// Callers must hold cacheMu.
func (t *Transport) lookup(u *url.URL) (spaceKey, *space) {
	scheme, host := originOf(u)
	path := u.Path
	if path == "" {
		path = "/"
	}
	var best spaceKey
	var sp *space
	spec := -1
	for key, s := range t.cache {
		if key.scheme != scheme || key.host != host {
			continue
		}
		spec2 := s.specificity(path)
		if spec2 < 0 {
			continue
		}
		if sp == nil || spec2 > spec || (spec2 == spec && key.realm < best.realm) {
			best, sp, spec = key, s, spec2
		}
	}
	return best, sp
}

// prepare sets the Authorization header from a cached challenge if one exists.
// It reports whether the caller should send a bodyless probe first (Probe set,
// request has a body, no challenge cached yet).
func (t *Transport) prepare(req *http.Request) (bool, error) {
	// add cookies
	if t.Jar != nil {
		for _, cookie := range t.Jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
	}
	// add auth
	chal, count, ok := t.challenge(req)
	if !ok {
		// no cached challenge yet: probe bodyless first when configured
		bodied := req.Body != nil && req.Body != http.NoBody
		return t.Probe && bodied, nil
	}
	cred, err := t.digest(req, chal, count)
	if err != nil {
		return false, err
	}
	if cred != nil {
		req.Header.Set("Authorization", cred.String())
	}
	return false, nil
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
	probe, err := t.prepare(first)
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
	if err := t.save(res); err != nil {
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
	// prepare the second request based on the new challenge
	if _, err := t.prepare(second); err != nil {
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
