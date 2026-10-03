package digest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// cchal is a cached challenge and the number of times it's been used.
type cchal struct {
	c *Challenge
	n int
}

// authLayer identifies one of the two authentication boundaries handled
// by the transport: the origin server (401) or a forward proxy (407).
type authLayer int

const (
	originLayer authLayer = iota
	proxyLayer
)

func (l authLayer) challengeHeader() string {
	if l == proxyLayer {
		return "Proxy-Authenticate"
	}
	return "WWW-Authenticate"
}

func (l authLayer) credentialsHeader() string {
	if l == proxyLayer {
		return "Proxy-Authorization"
	}
	return "Authorization"
}

// Transport implements http.RoundTripper
type Transport struct {
	Username string
	Password string

	// ProxyUsername and ProxyPassword are used to answer Digest challenges
	// from a forward proxy (Proxy-Authenticate / 407). They are independent
	// of the origin credentials (Username / Password). When both are empty,
	// proxy challenges are not answered and the 407 response is returned as-is.
	//
	// Only ordinary (non-tunneled) proxy requests are handled: the request is
	// authenticated against its absolute-form target. CONNECT tunnels are not
	// supported.
	ProxyUsername string
	ProxyPassword string

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

	// challenges cached per layer, indexed by target host
	originCache map[string]*cchal
	proxyCache  map[string]*cchal
	cacheMu     sync.Mutex
}

// ErrBodyNotReplayable is returned when an authentication retry needs to
// resend the request body but it cannot be reproduced (GetBody fails).
// The request is never retried with an empty body.
var ErrBodyNotReplayable = errors.New("digest: request body cannot be replayed for authentication retry")

// save parses the digest challenge from the response and caches it per layer.
func (t *Transport) save(layer authLayer, res *http.Response) error {
	// save cookies
	if t.Jar != nil {
		t.Jar.SetCookies(res.Request.URL, res.Cookies())
	}
	// find and save digest challenge
	chal, err := t.findChallenge(layer)(res.Header)
	host := res.Request.URL.Hostname()
	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()
	cache := t.layerCache(layer)
	if *cache == nil {
		*cache = map[string]*cchal{}
	}
	// TODO: if the challenge contains a domain, we should be using that
	//       to match against outgoing requests. We're currently ignoring
	//       it and just matching the hostname. That being said, none of
	//       the major browsers respect the domain either.
	if err != nil {
		// if save is being invoked, the existing cached challenge didn't work
		delete(*cache, host)
		return err
	}
	(*cache)[host] = &cchal{c: chal}
	return nil
}

// findChallenge returns the challenge locator for the given layer.
// Origin challenges honor the configurable FindChallenge hook; proxy
// challenges always come from the Proxy-Authenticate header.
func (t *Transport) findChallenge(layer authLayer) func(http.Header) (*Challenge, error) {
	if layer == originLayer && t.FindChallenge != nil {
		return t.FindChallenge
	}
	header := layer.challengeHeader()
	return func(h http.Header) (*Challenge, error) {
		return findChallengeInHeader(h, header)
	}
}

func (t *Transport) layerCache(layer authLayer) *map[string]*cchal {
	if layer == proxyLayer {
		return &t.proxyCache
	}
	return &t.originCache
}

// proxyEnabled reports whether proxy credentials are configured.
func (t *Transport) proxyEnabled() bool {
	return t.ProxyUsername != "" || t.ProxyPassword != ""
}

// digest creates credentials from the cached challenge.
func (t *Transport) digest(layer authLayer, req *http.Request, chal *Challenge, count int) (*Credentials, error) {
	uri := req.URL.RequestURI()
	username := t.Username
	password := t.Password
	if layer == proxyLayer {
		// proxy auth covers the absolute-form request target that is forwarded
		uri = absoluteRequestURI(req.URL)
		username = t.ProxyUsername
		password = t.ProxyPassword
	}
	opt := Options{
		Method:   req.Method,
		URI:      uri,
		GetBody:  req.GetBody,
		Count:    count,
		Username: username,
		Password: password,
	}
	if t.Digest != nil {
		return t.Digest(req, chal, opt)
	}
	return Digest(chal, opt)
}

// challenge returns a cached challenge and count for the provided request.
func (t *Transport) challenge(layer authLayer, req *http.Request) (*Challenge, int, bool) {
	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()
	cache := *t.layerCache(layer)
	host := req.URL.Hostname()
	cc, ok := cache[host]
	if !ok {
		return nil, 0, false
	}
	if t.NoReuse {
		delete(cache, host)
	}
	cc.n++
	return cc.c, cc.n, true
}

// hasChallenge reports whether a challenge is cached for the layer/host.
func (t *Transport) hasChallenge(layer authLayer, req *http.Request) bool {
	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()
	cache := *t.layerCache(layer)
	_, ok := cache[req.URL.Hostname()]
	return ok
}

// prepare sets the Authorization and Proxy-Authorization headers from the
// independently cached challenges, when present.
func (t *Transport) prepare(req *http.Request) error {
	// add cookies
	if t.Jar != nil {
		for _, cookie := range t.Jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
	}
	layers := []authLayer{originLayer}
	if t.proxyEnabled() {
		layers = append(layers, proxyLayer)
	}
	for _, layer := range layers {
		chal, count, ok := t.challenge(layer, req)
		if !ok {
			continue
		}
		cred, err := t.digest(layer, req, chal, count)
		if err != nil {
			return err
		}
		if cred != nil {
			req.Header.Set(layer.credentialsHeader(), cred.String())
		}
	}
	return nil
}

// RoundTrip authorizes the request using cached challenges for the origin
// server and, when proxy credentials are configured, the forward proxy.
//
// A single logical request may be challenged once by the proxy (407) and once
// by the origin (401); each layer keeps its own cache, nonce counter and a
// single retry, so neither layer can loop without bound.
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
	cur, err := clone()
	if err != nil {
		return nil, err
	}

	var retriedOrigin, retriedProxy bool
	// discover the origin challenge with a bodyless request when configured
	probe := t.Probe && cur.Body != nil && cur.Body != http.NoBody &&
		!t.hasChallenge(originLayer, cur)

	for {
		if err := t.prepare(cur); err != nil {
			return nil, err
		}
		if probe {
			cur.Body = nil
			cur.ContentLength = 0
			cur.GetBody = nil
		}

		res, err := tr.RoundTrip(cur)
		if err != nil {
			return res, err
		}

		layer := challengeLayer(res, t.proxyEnabled())
		if layer < 0 {
			if !probe {
				return res, nil
			}
			// the bodyless probe returned something we can't answer; discard it
			// and send the real request with the body
			drain(res)
			cur, err = clone()
			if err != nil {
				return nil, err
			}
			probe = false
			continue
		}

		// this layer already consumed its single retry: return the challenge
		// response instead of looping on it
		if (layer == originLayer && retriedOrigin) ||
			(layer == proxyLayer && retriedProxy) {
			return res, nil
		}

		// drain the challenge response before replaying the request
		drain(res)

		// learn and cache the challenge; without a usable challenge there is
		// nothing to retry with, so surface the response as-is
		if err := t.save(layer, res); err != nil {
			if errors.Is(err, ErrNoChallenge) {
				return res, nil
			}
			return nil, err
		}
		if layer == originLayer {
			retriedOrigin = true
			// the origin challenge is known: the real body can now be streamed
			probe = false
		} else {
			retriedProxy = true
		}

		// replay the request; a body that cannot be reproduced is a hard error,
		// never a silent empty-body retry
		cur, err = clone()
		if err != nil {
			return nil, err
		}
	}
}

// challengeLayer maps a response status to the authentication layer it
// challenges. Proxy challenges are only reported when proxy auth is enabled.
func challengeLayer(res *http.Response, proxyEnabled bool) authLayer {
	switch res.StatusCode {
	case http.StatusUnauthorized:
		return originLayer
	case http.StatusProxyAuthRequired:
		if proxyEnabled {
			return proxyLayer
		}
	}
	return -1
}

func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
}

// absoluteRequestURI renders the absolute-form request target used when
// forwarding through an ordinary (non-CONNECT) proxy.
func absoluteRequestURI(u *url.URL) string {
	var b strings.Builder
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	b.WriteString(scheme)
	b.WriteString("://")
	b.WriteString(u.Host)
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	b.WriteString(path)
	if u.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(u.RawQuery)
	}
	return b.String()
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

// cloner returns a function which makes clones of the provided request.
func cloner(req *http.Request) (func() (*http.Request, error), error) {
	getbody := req.GetBody
	// originalBody reports whether req.Body is still the untouched caller body
	// and can be streamed on the first attempt.
	originalBody := true
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
			originalBody = false
			getbody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		}
	}
	first := true
	return func() (*http.Request, error) {
		clone := req.Clone(req.Context())
		if first {
			// stream the caller's untouched body on the first attempt when we
			// didn't have to buffer it; otherwise replay the buffered copy
			first = false
			if !originalBody {
				body, err := getbody()
				if err != nil {
					return nil, fmt.Errorf("%w: %v", ErrBodyNotReplayable, err)
				}
				clone.Body = body
			}
		} else {
			// authentication replay: reproduce the body, failing explicitly if
			// it cannot be replayed instead of sending an empty request
			body, err := getbody()
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrBodyNotReplayable, err)
			}
			clone.Body = body
		}
		clone.GetBody = getbody
		return clone, nil
	}, nil
}
