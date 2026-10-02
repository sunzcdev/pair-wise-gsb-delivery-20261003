// +build go1.7

package nosurf

import "net/http"

type ctxKey int

const (
	nosurfKey ctxKey = iota
)

type csrfContext struct {
	// The masked, base64 encoded token
	// That's suitable for use in form fields, etc.
	token string
	// reason for the failure of CSRF check
	reason error

	// The unmasked real token, 32 bytes long if present.
	// It is decoded from the incoming cookie or freshly generated.
	realToken []byte
	// Whether the handler runs in on-demand cookie mode.
	onDemand bool
	// The response writer associated with the request, wrapped so that
	// on-demand issuance can detect an already committed response.
	writer *committedWriter
	// The handler serving the request, used to build the cookie on demand.
	handler *CSRFHandler
	// Whether a token has already been issued for this request,
	// guaranteeing that repeated Token()/EnsureToken() calls are consistent
	// and schedule the cookie at most once.
	issued bool
}

// Token takes an HTTP request and returns
// the CSRF token for that request
// or an empty string if the token does not exist.
//
// In on-demand cookie mode (see CSRFHandler.SetCookieOnDemand) calling Token
// requests the token for the current request and schedules its cookie, unless
// the response has already been committed, in which case Token returns "".
// Use EnsureToken to distinguish that failure from an unset token.
//
// Note that the token won't be available after
// CSRFHandler finishes
// (that is, in another handler that wraps it,
// or after the request has been served)
func Token(req *http.Request) string {
	ctx, ok := req.Context().Value(nosurfKey).(*csrfContext)
	if !ok {
		return ""
	}

	if ctx.onDemand && !ctx.issued {
		// Best-effort, error-free counterpart of EnsureToken: when the
		// response is already committed no cookie can be scheduled, so we
		// must not hand out a token the browser would never receive.
		if ctx.writer == nil || ctx.writer.committed {
			return ""
		}
		token, err := EnsureToken(ctx.writer, req)
		if err != nil {
			return ""
		}
		return token
	}

	return ctx.token
}

// Reason takes an HTTP request and returns
// the reason of failure of the CSRF check for that request
//
// Note that the same availability restrictions apply for Reason() as for Token().
func Reason(req *http.Request) error {
	ctx := req.Context().Value(nosurfKey).(*csrfContext)

	return ctx.reason
}

func ctxClear(_ *http.Request) {
}

func ctxSetToken(req *http.Request, token []byte) {
	ctx := req.Context().Value(nosurfKey).(*csrfContext)
	ctx.token = b64encode(maskToken(token))
}

func requestContext(req *http.Request) *csrfContext {
	ctx, _ := req.Context().Value(nosurfKey).(*csrfContext)
	return ctx
}

func ctxConfigure(req *http.Request, onDemand bool, w *committedWriter) {
	ctx := req.Context().Value(nosurfKey).(*csrfContext)
	ctx.onDemand = onDemand
	ctx.writer = w
}

func ctxSetReason(req *http.Request, reason error) {
	ctx := req.Context().Value(nosurfKey).(*csrfContext)
	ctx.reason = reason
}
