// +build !go1.7

package nosurf

import (
	"net/http"
	"sync"
)

// This file implements a context similar to one found
// in gorilla/context, but tailored specifically for our use case
// and not using gorilla's package just because.

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

var (
	contextMap = make(map[*http.Request]*csrfContext)
	cmMutex    = new(sync.RWMutex)
)

// Token() takes an HTTP request and returns
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
	cmMutex.RLock()
	ctx, ok := contextMap[req]
	if !ok {
		cmMutex.RUnlock()
		return ""
	}

	onDemand := ctx.onDemand
	issued := ctx.issued
	writer := ctx.writer
	token := ctx.token
	cmMutex.RUnlock()

	if onDemand && !issued {
		if writer == nil || writer.committed {
			return ""
		}
		issuedToken, err := EnsureToken(writer, req)
		if err != nil {
			return ""
		}
		return issuedToken
	}

	return token
}

// Reason() takes an HTTP request and returns
// the reason of failure of the CSRF check for that request
//
// Note that the same availability restrictions apply for Reason() as for Token().
func Reason(req *http.Request) error {
	cmMutex.RLock()
	defer cmMutex.RUnlock()

	ctx, ok := contextMap[req]

	if !ok {
		return nil
	}

	return ctx.reason
}

// Takes a raw token, masks it with a per-request key,
// encodes in base64 and makes it available to the wrapped handler
func ctxSetToken(req *http.Request, token []byte) *http.Request {
	cmMutex.Lock()
	defer cmMutex.Unlock()

	ctx, ok := contextMap[req]
	if !ok {
		ctx = new(csrfContext)
		contextMap[req] = ctx
	}

	ctx.token = b64encode(maskToken(token))

	return req
}

func ctxSetReason(req *http.Request, reason error) *http.Request {
	cmMutex.Lock()
	defer cmMutex.Unlock()

	ctx, ok := contextMap[req]
	if !ok {
		panic("Reason should never be set when there's no token" +
			" (context) yet.")
	}

	ctx.reason = reason
	return req
}

func ctxClear(req *http.Request) {
	cmMutex.Lock()
	defer cmMutex.Unlock()

	delete(contextMap, req)
}

func requestContext(req *http.Request) *csrfContext {
	cmMutex.RLock()
	defer cmMutex.RUnlock()

	return contextMap[req]
}

func ctxConfigure(req *http.Request, onDemand bool, w *committedWriter) {
	cmMutex.Lock()
	defer cmMutex.Unlock()

	ctx, ok := contextMap[req]
	if !ok {
		ctx = new(csrfContext)
		contextMap[req] = ctx
	}
	ctx.onDemand = onDemand
	ctx.writer = w
}
