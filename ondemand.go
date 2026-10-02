package nosurf

import (
	"errors"
	"net/http"
)

// ErrResponseAlreadyCommitted is returned by EnsureToken when the token is
// requested after the response headers have already been sent.
//
// In on-demand cookie mode a CSRF cookie cannot be scheduled once the response
// has been committed, so nosurf refuses to pretend that the browser received a
// cookie it will never see.
var ErrResponseAlreadyCommitted = errors.New("nosurf: the response has already been committed; the CSRF cookie cannot be set")

// errNoContext is returned by EnsureToken when it is called with a request
// that never passed through a CSRFHandler.
var errNoContext = errors.New("nosurf: the request is not associated with a CSRFHandler")

// committedWriter wraps an http.ResponseWriter so that nosurf can tell
// whether the response headers have already been committed.
//
// http.ResponseWriter.WriteHeader and the first Write both commit the
// response. After that point SetCookie has no effect, since the Set-Cookie
// header is a response header.
type committedWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *committedWriter) WriteHeader(code int) {
	w.committed = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *committedWriter) Write(p []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(p)
}

// Unwrap exposes the underlying writer to http.ResponseController so that
// wrapping does not hide Flush/Hijack/ServerPush capabilities.
func (w *committedWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Flush implements http.Flusher. Flushing, like Write, commits the response,
// so it is tracked to prevent issuing a token after the headers are gone.
func (w *committedWriter) Flush() {
	w.committed = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// EnsureToken returns the masked, base64 encoded CSRF token for the request
// and schedules its cookie when on-demand mode is enabled.
//
// It is the explicit, error-aware counterpart of Token. The two return the
// exact same token for a given request, so mixing them is safe:
//
//   - In the default ("eager") mode the token and its cookie are always
//     issued up front by the middleware, so EnsureToken simply returns the
//     existing token.
//   - In on-demand mode (enabled with SetCookieOnDemand) the token is only
//     generated or reused and its cookie scheduled when a handler actually
//     requests it through Token or EnsureToken. Repeated calls within a single
//     request always yield the same token.
//
// EnsureToken returns ErrResponseAlreadyCommitted if the response headers have
// already been sent: in that case the browser cannot receive the Set-Cookie
// header, so no token is issued. A failure handler can use this to decide
// explicitly whether and how to expose the token.
//
// The writer argument is the ResponseWriter passed to the handler; nosurf
// tracks commit state on the writer it installed for the request, so an
// additionally wrapped writer may be passed as well.
func EnsureToken(w http.ResponseWriter, r *http.Request) (string, error) {
	ctx := requestContext(r)
	if ctx == nil {
		return "", errNoContext
	}

	if !ctx.onDemand {
		// Eager mode: the token is always resolved by the middleware.
		return ctx.token, nil
	}

	// On-demand mode. If the token has already been requested during this
	// request, keep returning the same (masked) value and never touch the
	// response again.
	if ctx.issued {
		return ctx.token, nil
	}

	cw := ctx.writer
	if cw == nil || cw.committed {
		return "", ErrResponseAlreadyCommitted
	}

	// Either reuse the token the client already presented in its cookie or
	// mint a brand new one, but only now that a handler actually needs it.
	token := ctx.realToken
	if len(token) != tokenLength {
		token = generateToken()
	}

	ctx.handler.setTokenCookieOn(cw, ctx, token)
	addVaryCookie(cw)

	return ctx.token, nil
}

// addVaryCookie appends "Cookie" to the Vary header, matching the behavior of
// the default eager mode, but only once the response actually varies on the
// CSRF cookie (i.e. a token has been issued).
func addVaryCookie(w http.ResponseWriter) {
	headers := w.Header()
	for _, v := range headers["Vary"] {
		if v == "Cookie" {
			return
		}
	}
	headers.Add("Vary", "Cookie")
}

// resolveOrGenerateToken ensures that a real token exists without scheduling
// a cookie. It is used on the unsafe-method failure path in on-demand mode so
// that the failure reason can be recorded without eagerly issuing a cookie:
// the failure handler remains free to request a token itself.
func resolveOrGenerateToken(ctx *csrfContext) []byte {
	if len(ctx.realToken) == tokenLength {
		return ctx.realToken
	}

	token := generateToken()
	ctx.realToken = token
	return token
}
