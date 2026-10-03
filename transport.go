package digest

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync"
)

// maxNonceCount is the largest nonce count representable in the 8-digit
// hexadecimal nc field.
const maxNonceCount = 0xffffffff

// ErrNonceCountExhausted is reported when every nonce count for the cached
// challenge has been allocated. The transport drops the exhausted challenge
// and waits for the server to issue a new one rather than wrapping the
// counter and re-using nonce count values.
var ErrNonceCountExhausted = errors.New("digest: nonce count exhausted")

// cchal is a cached challenge and the number of nonce counts allocated for
// it. The pointer identity of the entry acts as the challenge generation:
// installing a new entry (new challenge, nextnonce commit, exhaustion)
// starts a new generation and invalidates the state captured by in-flight
// requests.
type cchal struct {
	c *Challenge
	n int
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

	// VerifyResponse verifies the Authentication-Info header of a successful
	// response before its nextnonce is committed to the challenge cache.
	// If nil, the VerifyResponseAuth function is used.
	VerifyResponse func(*ResponseAuth) error

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

	// cache of challenges indexed by host
	cache   map[string]*cchal
	cacheMu sync.Mutex
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
		t.cache = map[string]*cchal{}
	}
	// TODO: if the challenge contains a domain, we should be using that
	//       to match against outgoing requests. We're currently ignoring
	//       it and just matching the hostname. That being said, none of
	//       the major browsers respect the domain either.
	host := res.Request.URL.Hostname()
	if err != nil {
		// if save is being invoked, the existing cached challenge didn't work
		delete(t.cache, host)
		return err
	}
	t.cache[host] = &cchal{c: chal}
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

// challenge allocates the next nonce count for the cached challenge of the
// request's host. The allocation and the generation capture are atomic: the
// returned entry identifies the challenge generation and stays valid only
// while it remains installed in the cache.
func (t *Transport) challenge(req *http.Request) (*cchal, int, error) {
	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()
	host := req.URL.Hostname()
	cc, ok := t.cache[host]
	if !ok {
		return nil, 0, nil
	}
	if t.NoReuse {
		delete(t.cache, host)
	}
	if cc.n >= maxNonceCount {
		// All nonce count values for this challenge have been allocated.
		// Drop it and wait for the server to issue a fresh challenge
		// instead of wrapping the counter.
		delete(t.cache, host)
		return nil, 0, ErrNonceCountExhausted
	}
	cc.n++
	return cc, cc.n, nil
}

// authState captures the challenge generation and the credentials used to
// authorize a request. It is used to commit Authentication-Info updates
// carried by the corresponding response.
type authState struct {
	entry *cchal
	chal  *Challenge
	cred  *Credentials
}

// prepare sets the Authorization header from a cached challenge if one
// exists. It returns the captured authorization state (nil when no cached
// challenge was used) and reports whether the caller should send a bodyless
// probe first (Probe set, request has a body, no challenge cached yet).
func (t *Transport) prepare(req *http.Request) (*authState, bool, error) {
	// add cookies
	if t.Jar != nil {
		for _, cookie := range t.Jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
	}
	// add auth
	entry, count, err := t.challenge(req)
	if err != nil {
		// The cached challenge is exhausted: send the request bare and
		// wait for the server to respond with a fresh challenge.
		if !errors.Is(err, ErrNonceCountExhausted) {
			return nil, false, err
		}
	}
	if entry == nil {
		// no cached challenge yet: probe bodyless first when configured
		bodied := req.Body != nil && req.Body != http.NoBody
		return nil, t.Probe && bodied, nil
	}
	cred, err := t.digest(req, entry.c, count)
	if err != nil {
		return nil, false, err
	}
	if cred != nil {
		req.Header.Set("Authorization", cred.String())
	}
	return &authState{entry: entry, chal: entry.c, cred: cred}, false, nil
}

// commit applies a verified Authentication-Info nextnonce update from the
// response to the cached challenge. The update is only committed when the
// challenge generation captured while authorizing the request is still the
// current generation; responses from superseded generations are discarded,
// even when they arrive last. A committed nextnonce starts a new generation
// and the nonce count restarts from one.
func (t *Transport) commit(req *http.Request, state *authState, res *http.Response) {
	if state == nil || state.cred == nil {
		return
	}
	info, err := FindAuthenticationInfo(res.Header)
	if err != nil || info == nil || info.NextNonce == "" {
		return
	}
	verify := t.VerifyResponse
	if verify == nil {
		verify = VerifyResponseAuth
	}
	if err := verify(&ResponseAuth{
		Request:     req,
		Credentials: state.cred,
		Challenge:   state.chal,
		Info:        info,
		Username:    t.Username,
		Password:    t.Password,
	}); err != nil {
		return
	}
	host := req.URL.Hostname()
	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()
	if cur := t.cache[host]; cur != state.entry {
		// The generation was superseded (new challenge, nextnonce commit,
		// exhaustion, or NoReuse consumption): a stale response must not
		// roll the cache back.
		return
	}
	if info.NextNonce == state.entry.c.Nonce {
		// The nonce is unchanged: keep counting on this generation.
		return
	}
	// Install the nextnonce as a new generation; counting restarts at one.
	chal := *state.entry.c
	chal.Nonce = info.NextNonce
	t.cache[host] = &cchal{c: &chal}
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
	state, probe, err := t.prepare(first)
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
			t.commit(first, state, res)
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
	state2, _, err := t.prepare(second)
	if err != nil {
		return nil, err
	}
	res, err = tr.RoundTrip(second)
	if err == nil && res.StatusCode != http.StatusUnauthorized {
		t.commit(second, state2, res)
	}
	return res, err
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
