package digest

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
)

// ErrBodyNotReplayable is returned when a challenge requires the request to be
// replayed but its body cannot be produced again. The request is never sent
// with an empty body in this case.
var ErrBodyNotReplayable = errors.New("digest: request body is not replayable")

// cchal is a cached challenge and the number of times it's been used.
type cchal struct {
	c *Challenge
	n int
}

// Transport implements http.RoundTripper
type Transport struct {
	Username string
	Password string

	// ProxyUsername and ProxyPassword are the credentials used in response to
	// a proxy's 407 Proxy Authentication Required challenge. They are kept
	// separate from the origin credentials and cached independently.
	//
	// Proxy, if non-nil, reports the proxy used for a request. It mirrors
	// http.Transport.Proxy: proxy authentication is only attempted when a
	// proxy is configured for the request, and the proxy challenge cache is
	// keyed by the proxy host. When Proxy is nil, proxy authentication is
	// attempted for every request and the cache is keyed by the target host.
	ProxyUsername string
	ProxyPassword string
	Proxy         func(*http.Request) (*url.URL, error)

	// Digest computes the digest credentials.
	// If nil, the Digest function is used.
	Digest func(*http.Request, *Challenge, Options) (*Credentials, error)

	// ProxyDigest computes the digest credentials for a proxy challenge.
	// If nil, Digest is used; if that is also nil, the Digest function is used.
	ProxyDigest func(*http.Request, *Challenge, Options) (*Credentials, error)

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

	// origin challenges (WWW-Authenticate / Authorization), indexed by origin host
	// proxy challenges (Proxy-Authenticate / Proxy-Authorization), indexed by
	// proxy host (or target host when no Proxy selector is configured)
	cache      map[string]*cchal
	proxyCache map[string]*cchal
	cacheMu    sync.Mutex
}

// proxyConfigured reports whether proxy authentication should be attempted
// for the request and returns the cache key for the proxy challenge.
//
// Proxy authentication is opt-in: it is active when proxy credentials are
// explicitly configured and/or when a Proxy selector is present. With neither
// configured, 407 responses pass through untouched, preserving the
// non-proxy behavior.
func (t *Transport) proxyConfigured(req *http.Request) (string, bool, error) {
	credsSet := t.ProxyUsername != "" || t.ProxyPassword != ""
	if t.Proxy == nil {
		if !credsSet {
			return "", false, nil
		}
		// No selector but explicit credentials: attempt proxy auth for every
		// request and key the proxy challenge cache by the target host.
		return req.URL.Hostname(), true, nil
	}
	proxyURL, err := t.Proxy(req)
	if err != nil {
		return "", false, err
	}
	if proxyURL == nil {
		return "", false, nil
	}
	if !credsSet {
		// A proxy is selected but no digest credentials were configured:
		// leave the handshake to the underlying transport.
		return "", false, nil
	}
	return proxyURL.Hostname(), true, nil
}

// layerCache returns the challenge cache for the specified authentication layer.
func (t *Transport) layerCache(proxy bool) map[string]*cchal {
	if t.cache == nil {
		t.cache = map[string]*cchal{}
	}
	if !proxy {
		return t.cache
	}
	if t.proxyCache == nil {
		t.proxyCache = map[string]*cchal{}
	}
	return t.proxyCache
}

// cachedChallenge returns a cached challenge and its use count for the provided host.
func (t *Transport) cachedChallenge(host string, proxy bool) (*Challenge, int, bool) {
	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()
	cache := t.layerCache(proxy)
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

// storeChallenge saves a challenge for the provided host.
func (t *Transport) storeChallenge(host string, chal *Challenge, proxy bool) {
	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()
	t.layerCache(proxy)[host] = &cchal{c: chal}
}

// dropChallenge removes the cached challenge for the provided host. It is used
// when a cached challenge fails to authenticate again.
func (t *Transport) dropChallenge(host string, proxy bool) {
	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()
	delete(t.layerCache(proxy), host)
}

// saveChallenge parses the challenge from the response and caches it for the
// corresponding authentication layer.
func (t *Transport) saveChallenge(header http.Header, proxy bool, key string) error {
	find := t.findChallengeFunc(proxy)
	chal, err := find(header)
	if err != nil {
		// if save is being invoked, the existing cached challenge didn't work
		t.dropChallenge(key, proxy)
		return err
	}
	t.storeChallenge(key, chal, proxy)
	return nil
}

// findChallengeFunc returns the configured challenge finder for the layer.
func (t *Transport) findChallengeFunc(proxy bool) func(http.Header) (*Challenge, error) {
	if proxy {
		return FindProxyChallenge
	}
	if t.FindChallenge != nil {
		return t.FindChallenge
	}
	return FindChallenge
}

// credentials creates digest credentials from a cached challenge.
// Origin authentication uses the origin-form URI; proxy authentication uses
// the absolute-form of the target that is actually forwarded.
func (t *Transport) credentials(req *http.Request, chal *Challenge, count int, proxy bool) (*Credentials, error) {
	opt := Options{
		Method:   req.Method,
		URI:      req.URL.RequestURI(),
		GetBody:  req.GetBody,
		Count:    count,
		Username: t.Username,
		Password: t.Password,
	}
	digestfn := t.Digest
	if proxy {
		opt.Username = t.ProxyUsername
		opt.Password = t.ProxyPassword
		opt.URI = absoluteURI(req.URL)
		digestfn = t.ProxyDigest
		if digestfn == nil {
			digestfn = t.Digest
		}
	}
	if digestfn != nil {
		return digestfn(req, chal, opt)
	}
	return Digest(chal, opt)
}

// prepare adds cookies and the cached Authorization/Proxy-Authorization headers.
// It reports whether the caller should send a bodyless probe first (Probe set,
// request has a body, no challenge cached for at least one active layer).
func (t *Transport) prepare(req *http.Request, proxyAuth bool, proxyKey string) (bool, error) {
	// add cookies
	if t.Jar != nil {
		for _, cookie := range t.Jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
	}
	uncached := false
	// add origin auth
	if chal, count, ok := t.cachedChallenge(req.URL.Hostname(), false); ok {
		if err := t.setAuthHeader(req, chal, count, false); err != nil {
			return false, err
		}
	} else {
		uncached = true
	}
	// add proxy auth
	if proxyAuth {
		if chal, count, ok := t.cachedChallenge(proxyKey, true); ok {
			if err := t.setAuthHeader(req, chal, count, true); err != nil {
				return false, err
			}
		} else {
			uncached = true
		}
	}
	bodied := req.Body != nil && req.Body != http.NoBody
	return t.Probe && bodied && uncached, nil
}

func (t *Transport) setAuthHeader(req *http.Request, chal *Challenge, count int, proxy bool) error {
	cred, err := t.credentials(req, chal, count, proxy)
	if err != nil {
		return err
	}
	if cred != nil {
		if proxy {
			req.Header.Set("Proxy-Authorization", cred.String())
		} else {
			req.Header.Set("Authorization", cred.String())
		}
	}
	return nil
}

// absoluteURI returns the absolute-form request target used when forwarding
// a request to a proxy: scheme://host + origin-form path?query.
func absoluteURI(u *url.URL) string {
	var b bytes.Buffer
	b.WriteString(u.Scheme)
	b.WriteString("://")
	b.WriteString(u.Host)
	if r := u.RequestURI(); r != "" && r[0] != '/' {
		b.WriteByte('/')
	}
	b.WriteString(u.RequestURI())
	return b.String()
}

// RoundTrip sends the request using cached challenges where available. A 407
// (Proxy Authentication Required) is answered with Proxy-Authorization and a
// 401 (Unauthorized) with Authorization. The two layers have independent
// challenges, caches, nonce counters and retry budgets: a single logical
// request can be challenged by the proxy and then by the origin, but each
// layer is retried at most once, so the handling always terminates.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// use the configured transport if there is one
	tr := t.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	// determine whether proxy authentication is in play for this request
	proxyKey, proxyAuth, err := t.proxyConfigured(req)
	if err != nil {
		return nil, err
	}
	// don't modify the original request or its headers. The cloner also
	// guarantees the body can be replayed; if it cannot, fail explicitly
	// instead of silently sending an empty request.
	clone, err := cloner(req)
	if err != nil {
		return nil, errors.Join(ErrBodyNotReplayable, err)
	}
	// each authentication layer gets at most one challenge-driven retry
	retried := map[authLayer]bool{}
	var probe bool
	for {
		cur, err := clone()
		if err != nil {
			return nil, errors.Join(ErrBodyNotReplayable, err)
		}
		probe, err = t.prepare(cur, proxyAuth, proxyKey)
		if err != nil {
			return nil, err
		}
		// probe bodyless so the real body is only streamed on a request that
		// already carries credentials for every layer that challenged us
		if probe {
			stripBody(cur)
		}
		res, err := tr.RoundTrip(cur)
		if err != nil {
			return res, err
		}
		layer, challenged := challengeLayer(res.StatusCode, proxyAuth)
		// a bodyless probe is not a valid caller response: if it was not
		// challenged, discard it and send the real request exactly as prepared
		if probe && !challenged {
			drainBody(res)
			return t.sendAsPrepared(tr, clone, proxyAuth, proxyKey)
		}
		if !challenged || retried[layer] {
			// success, or a repeated challenge from a layer whose budget is
			// spent: return the response unchanged
			return res, nil
		}
		// drain and close the challenge response body
		drainBody(res)
		// save cookies from origin challenge responses
		if layer == originLayer && t.Jar != nil {
			t.Jar.SetCookies(cur.URL, res.Cookies())
		}
		key := req.URL.Hostname()
		if layer == proxyLayer {
			key = proxyKey
		}
		if err := t.saveChallenge(res.Header, layer == proxyLayer, key); err != nil {
			if err == ErrNoChallenge {
				return res, nil
			}
			return nil, err
		}
		retried[layer] = true
	}
}

// sendAsPrepared sends a fresh clone with cookies and cached credentials but
// with the real body intact, used after a bodyless probe was not challenged.
func (t *Transport) sendAsPrepared(
	tr http.RoundTripper,
	clone func() (*http.Request, error),
	proxyAuth bool,
	proxyKey string,
) (*http.Response, error) {
	cur, err := clone()
	if err != nil {
		return nil, errors.Join(ErrBodyNotReplayable, err)
	}
	if _, err := t.prepare(cur, proxyAuth, proxyKey); err != nil {
		return nil, err
	}
	return tr.RoundTrip(cur)
}

type authLayer int

const (
	originLayer authLayer = iota
	proxyLayer
)

// challengeLayer maps a response status to an authentication layer. It reports
// false for statuses that are not challenges (including 407 when proxy
// authentication is not configured).
func challengeLayer(status int, proxyAuth bool) (authLayer, bool) {
	switch status {
	case http.StatusProxyAuthRequired:
		if !proxyAuth {
			return originLayer, false
		}
		return proxyLayer, true
	case http.StatusUnauthorized:
		return originLayer, true
	default:
		return originLayer, false
	}
}

// stripBody removes the body from a probe request.
func stripBody(req *http.Request) {
	req.Body = nil
	req.ContentLength = 0
	req.GetBody = nil
}

// drainBody reads and discards the response body so the connection can be reused.
func drainBody(res *http.Response) {
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
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
