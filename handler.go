// Package nosurf implements an HTTP handler that
// mitigates Cross-Site Request Forgery Attacks.
package nosurf

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
)

const (
	// the name of CSRF cookie
	CookieName = "csrf_token"
	// the name of the form field
	FormFieldName = "csrf_token"
	// the name of CSRF header
	HeaderName = "X-CSRF-Token"
	// the HTTP status code for the default failure handler
	FailureCode = 400

	// Max-Age in seconds for the default base cookie. 365 days.
	MaxAge = 365 * 24 * 60 * 60
)

var safeMethods = []string{"GET", "HEAD", "OPTIONS", "TRACE"}

// reasons for CSRF check failures
var (
	ErrNoReferer  = errors.New("A secure request contained no Referer or its value was malformed")
	ErrBadReferer = errors.New("A secure request's Referer comes from a different origin" +
		" from the request's URL")
	ErrBadOrigin = errors.New("Request was made with a disallowed origin specified in the Origin header")
	ErrBadToken  = errors.New("The CSRF token in the cookie doesn't match the one" +
		" received in a form/header.")

	// Internal error. When this is raised, and the request is secure, we additionally check for Referer.
	errNoOrigin = errors.New("Origin header was not present")

	// ErrResponseAlreadyCommitted is returned by GetToken() in on-demand mode
	// when the CSRF token is requested after the response headers have already
	// been written (e.g. via ResponseWriter.WriteHeader()).
	// In that case it is no longer possible to deliver a Set-Cookie header
	// to the browser, so nosurf refuses to hand out a token that could not
	// be paired with a cookie.
	ErrResponseAlreadyCommitted = errors.New("CSRF token requested after the response headers were written")

	// ErrContextNotAvailable is returned by GetToken() when it is called
	// outside of a request handled by CSRFHandler.
	ErrContextNotAvailable = errors.New("The request is not handled by CSRFHandler")
)

type CSRFHandler struct {
	// Handlers that CSRFHandler wraps.
	successHandler http.Handler
	failureHandler http.Handler

	// The base cookie that CSRF cookies will be built upon.
	// This should be a better solution of customizing the options
	// than a bunch of methods SetCookieExpiration(), etc.
	baseCookie http.Cookie

	// Slices of paths that are exempt from CSRF checks.
	// All of those will be matched against Request.URL.Path,
	// So they should take the leading slash into account
	// Paths can be specified by...
	// ...an exact path,
	exemptPaths []string
	// ...a regexp,
	exemptRegexps []*regexp.Regexp
	// ...or a glob (as used by path.Match()).
	exemptGlobs []string
	// ...or a custom matcher function
	exemptFunc func(r *http.Request) bool

	// When true, the CSRF cookie (and the Vary: Cookie header) is only issued
	// once a handler actually obtains a token via GetToken().
	onDemandCookie bool

	isTLS           func(r *http.Request) bool
	isAllowedOrigin func(r *url.URL) bool
}

func defaultFailureHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, http.StatusText(FailureCode), FailureCode)
}

// Extracts the "sent" token from the request
// and returns an unmasked version of it
func extractToken(r *http.Request) []byte {
	// Prefer the header over form value
	sentToken := r.Header.Get(HeaderName)

	// Then POST values
	if len(sentToken) == 0 {
		sentToken = r.PostFormValue(FormFieldName)
	}

	// If all else fails, try a multipart value.
	// PostFormValue() will already have called ParseMultipartForm()
	if len(sentToken) == 0 && r.MultipartForm != nil {
		vals := r.MultipartForm.Value[FormFieldName]
		if len(vals) != 0 {
			sentToken = vals[0]
		}
	}

	return b64decode(sentToken)
}

// Constructs a new CSRFHandler that calls
// the specified handler if the CSRF check succeeds.
func New(handler http.Handler) *CSRFHandler {
	baseCookie := http.Cookie{}
	baseCookie.MaxAge = MaxAge

	csrf := &CSRFHandler{successHandler: handler,
		failureHandler: http.HandlerFunc(defaultFailureHandler),
		baseCookie:     baseCookie,
		isTLS:          func(r *http.Request) bool { return true },
	}

	return csrf
}

// The same as New(), but has an interface return type.
func NewPure(handler http.Handler) http.Handler {
	return New(handler)
}

func (h CSRFHandler) getCookieName() string {
	if h.baseCookie.Name != "" {
		return h.baseCookie.Name
	}

	return CookieName
}

func (h *CSRFHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = addNosurfContext(r)
	defer ctxClear(r)

	var realToken []byte

	tokenCookie, err := r.Cookie(h.getCookieName())
	if err == nil {
		realToken = b64decode(tokenCookie.Value)
	}

	if h.onDemandCookie {
		ctxSetState(r, &requestState{
			handler:   h,
			writer:    newOnDemandResponseWriter(w),
			realToken: realToken,
			hasCookie: err == nil && len(realToken) == tokenLength,
		})

		if sContains(safeMethods, r.Method) || h.IsExempt(r) {
			// short-circuit with a success for safe methods
			h.handleSuccess(ctxWriter(r), r)
			return
		}

		if err := h.ensureSameOrigin(r); err != nil {
			ctxSetReason(r, err)
			h.handleFailure(ctxWriter(r), r)
			return
		}

		// Finally, we check the token itself.
		sentToken := extractToken(r)

		if !verifyToken(realToken, sentToken) {
			ctxSetReason(r, ErrBadToken)
			h.handleFailure(ctxWriter(r), r)
			return
		}

		// Everything else passed, handle the success.
		h.handleSuccess(ctxWriter(r), r)
		return
	}

	addVaryCookie(w)

	// If the length of the real token isn't what it should be,
	// it has either been tampered with,
	// or we're migrating onto a new algorithm for generating tokens,
	// or it hasn't ever been set so far.
	// In any case of those, we should regenerate it.
	//
	// As a consequence, CSRF check will fail when comparing the tokens later on,
	// so we don't have to fail it just yet.
	if len(realToken) != tokenLength {
		h.RegenerateToken(w, r)
	} else {
		ctxSetToken(r, realToken)
	}

	if sContains(safeMethods, r.Method) || h.IsExempt(r) {
		// short-circuit with a success for safe methods
		h.handleSuccess(w, r)
		return
	}

	if err := h.ensureSameOrigin(r); err != nil {
		ctxSetReason(r, err)
		h.handleFailure(w, r)
		return
	}

	// Finally, we check the token itself.
	sentToken := extractToken(r)

	if !verifyToken(realToken, sentToken) {
		ctxSetReason(r, ErrBadToken)
		h.handleFailure(w, r)
		return
	}

	// Everything else passed, handle the success.
	h.handleSuccess(w, r)
}

// handleSuccess simply calls the successHandler.
// Everything else, like setting a token in the context
// is taken care of by h.ServeHTTP()
func (h *CSRFHandler) handleSuccess(w http.ResponseWriter, r *http.Request) {
	h.successHandler.ServeHTTP(w, r)
}

// Same applies here: h.ServeHTTP() sets the failure reason, the token,
// and only then calls handleFailure()
func (h *CSRFHandler) handleFailure(w http.ResponseWriter, r *http.Request) {
	h.failureHandler.ServeHTTP(w, r)
}

func (h *CSRFHandler) ensureSameOrigin(r *http.Request) error {
	selfOrigin := &url.URL{
		Scheme: "http",
		Host:   r.Host,
	}
	isTLS := h.isTLS(r)
	if isTLS {
		selfOrigin.Scheme = "https"
	}

	secFetchSite := r.Header.Get("Sec-Fetch-Site")
	if secFetchSite == "same-origin" {
		return nil
	}

	// If no `Sec-Fetch-Site: same-origin` is present, fallback to Origin or Referer,
	// including considering custom allowed origins.
	err := h.checkOrigin(selfOrigin, r)
	if err == nil {
		return nil
	} else if !errors.Is(err, errNoOrigin) {
		return err
	}

	// If Origin header was not present, fall back on Referer check for both secure and insecure requests.
	// This is opposite of Django's behavior, but should be fine, as neither of the three headers existing is an edge case.
	// https://github.com/django/django/blob/8be0c0d6901669661fca578f474cd51cd284d35a/django/middleware/csrf.py#L460
	return h.checkReferer(selfOrigin, r)
}

func (h *CSRFHandler) checkReferer(selfOrigin *url.URL, r *http.Request) error {
	referer, err := url.Parse(r.Referer())
	if err != nil || referer.String() == "" {
		return ErrNoReferer
	}

	if sameOrigin(selfOrigin, referer) {
		return nil
	}

	if h.isAllowedOrigin != nil && h.isAllowedOrigin(referer) {
		return nil
	}

	return ErrBadReferer
}

func (h *CSRFHandler) checkOrigin(selfOrigin *url.URL, r *http.Request) error {
	originStr := r.Header.Get("Origin")
	if originStr == "" || originStr == "null" {
		return errNoOrigin
	}

	origin, err := url.Parse(originStr)
	if err != nil {
		return err
	}

	if sameOrigin(selfOrigin, origin) {
		return nil
	}

	if h.isAllowedOrigin != nil && h.isAllowedOrigin(origin) {
		return nil
	}

	return ErrBadOrigin
}

// Generates a new token, sets it on the given request and returns it
func (h *CSRFHandler) RegenerateToken(w http.ResponseWriter, r *http.Request) string {
	token := generateToken()
	h.setTokenCookie(w, r, token)

	return Token(r)
}

func (h *CSRFHandler) setTokenCookie(w http.ResponseWriter, r *http.Request, token []byte) {
	ctxSetToken(r, token)

	cookie := h.baseCookie
	cookie.Name = h.getCookieName()
	cookie.Value = b64encode(token)

	http.SetCookie(w, &cookie)
	addVaryCookie(w)

	if state := ctxGetState(r); state != nil {
		state.realToken = token
		state.hasCookie = true
	}
}

// addVaryCookie adds `Vary: Cookie` without adding duplicates.
func addVaryCookie(w http.ResponseWriter) {
	if !sContains(w.Header().Values("Vary"), "Cookie") {
		w.Header().Add("Vary", "Cookie")
	}
}

// SetCookieOnDemand enables on-demand CSRF cookie issuance.
//
// By default, nosurf sets the CSRF cookie (and a `Vary: Cookie` response
// header) on every request, including plain GET requests that never render a
// form. When on-demand mode is enabled, nosurf only generates or reuses a
// token and schedules the corresponding cookie once a downstream handler
// actually obtains the token with GetToken().
//
// CSRF checks for unsafe (non-GET/HEAD/OPTIONS/TRACE) methods remain
// unaffected: the request token is still fully validated against the cookie.
//
// Callers must inspect the error returned by GetToken(): a token can not be
// delivered after the response headers have been written, in which case
// ErrResponseAlreadyCommitted is returned.
func (h *CSRFHandler) SetCookieOnDemand(onDemand bool) {
	h.onDemandCookie = onDemand
}

// GetToken returns the masked, base64-encoded CSRF token for the request,
// suitable for embedding in forms or headers.
//
// In on-demand mode (see SetCookieOnDemand), the first call to GetToken()
// either reuses the valid token from the incoming CSRF cookie or generates a
// new one, and schedules the Set-Cookie header on the response. Repeated
// calls within the same request return the exact same token.
//
// It returns ErrResponseAlreadyCommitted if the response headers have already
// been written (e.g. the handler called WriteHeader() before GetToken()),
// because the browser could no longer receive the accompanying cookie.
// It returns ErrContextNotAvailable when called outside of a request served
// by CSRFHandler.
func GetToken(r *http.Request) (string, error) {
	state := ctxGetState(r)
	if state == nil || state.handler == nil || state.writer == nil {
		return "", ErrContextNotAvailable
	}

	if token := ctxTokenRaw(r); token != "" {
		return token, nil
	}

	if state.writer.Committed() {
		return "", ErrResponseAlreadyCommitted
	}

	token := state.realToken
	if !state.hasCookie || len(token) != tokenLength {
		token = generateToken()
	}

	state.handler.setTokenCookie(state.writer, r, token)

	return ctxTokenRaw(r), nil
}

// onDemandIssue implements the lazy behaviour of Token(): it triggers
// on-demand issuance only when the handler is configured for it, returning nil
// in eager mode (where the token is always set beforehand).
func onDemandIssue(r *http.Request) error {
	state := ctxGetState(r)
	if state == nil || state.handler == nil || state.writer == nil {
		return nil
	}

	_, err := GetToken(r)
	return err
}

// Sets the handler to call in case the CSRF check
// fails. By default it's defaultFailureHandler.
func (h *CSRFHandler) SetFailureHandler(handler http.Handler) {
	h.failureHandler = handler
}

// Sets the base cookie to use when building a CSRF token cookie
// This way you can specify the Domain, Path, HttpOnly, Secure, etc.
func (h *CSRFHandler) SetBaseCookie(cookie http.Cookie) {
	h.baseCookie = cookie
}

// SetIsTLSFunc sets a delegate function which determines, on a per-request basis, whether the request is made over a secure connection.
// This should return `true` iff the URL that the user uses to access the application begins with https://.
// For example, if the Go web application is served via plain-text HTTP,
// but the user is accessing it through HTTPS via a TLS-terminating reverse-proxy, this should return `true`.
//
// Examples:
//
// 1. If you're using the Go TLS stack (no TLS-terminating proxies in between the user and the app), you may use:
//
//	h.SetIsTLSFunc(func(r *http.Request) bool { return r.TLS != nil })
//
// 2. If your application is behind a reverse proxy that terminates TLS, you should configure the reverse proxy
// to report the protocol that the request was made over via an HTTP header,
// e.g. `X-Forwarded-Proto`.
// You should also validate that the request is coming in from an IP of a trusted reverse proxy
// to ensure that this header has not been spoofed by an attacker. For example:
//
//	var trustedProxies = []string{"198.51.100.1", "198.51.100.2"}
//	h.SetIsTLSFunc(func(r *http.Request) bool {
//		ip, _, _ := strings.Cut(r.RemoteAddr, ":")
//		proto := r.Header.Get("X-Forwarded-Proto")
//		return slices.Contains(trustedProxies, ip) && proto == "https"
//	})
func (h *CSRFHandler) SetIsTLSFunc(f func(*http.Request) bool) {
	h.isTLS = f
}

// SetAllowedOrigins defines a function that checks whether the request comes from an allowed origin.
// This function will be invoked when the request is not considered a same-origin request.
// If this function returns `false`, request will be disallowed.
//
// In most cases, this will be used with [StaticOrigins].
func (h *CSRFHandler) SetIsAllowedOriginFunc(f func(*url.URL) bool) {
	h.isAllowedOrigin = f
}

// StaticOrigins returns a delegate, suitable for passing to [CSRFHandler.SetIsAllowedOriginFunc],
// that validates the request origin against a static list of allowed origins.
// This function expects each element to be of form `scheme://host`, e.g.: `https://example.com`, `http://example.org`.
// If any element of the slice is an invalid URL, this function will return an error.
// If an element includes additional URL parts (e.g. a path), these parts will be ignored,
// as origin checks only take the scheme and host into account.
//
// Example:
//
//	h := nosurf.New()
//	origins, err := nosurf.StaticOrigins("https://api.example.com", "http://insecure.example.com")
//	if err != nil {
//		panic(err)
//	}
//	h.SetIsAllowedOriginFunc(origins)
func StaticOrigins(origins ...string) (func(r *url.URL) bool, error) {
	var allowedOrigins []*url.URL
	for _, o := range origins {
		url, err := url.Parse(o)
		if err != nil {
			return nil, err
		}
		allowedOrigins = append(allowedOrigins, url)
	}
	return func(u *url.URL) bool {
		for _, candidate := range allowedOrigins {
			if sameOrigin(candidate, u) {
				return true
			}
		}
		return false
	}, nil
}
