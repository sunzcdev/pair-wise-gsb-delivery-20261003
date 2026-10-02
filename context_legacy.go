//go:build !go1.7
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
	// Per-request bookkeeping (present on every request served by CSRFHandler).
	state *requestState
}

var (
	contextMap = make(map[*http.Request]*csrfContext)
	cmMutex    = new(sync.RWMutex)
)

// Token() takes an HTTP request and returns
// the CSRF token for that request
// or an empty string if the token does not exist.
//
// In on-demand cookie mode (see CSRFHandler.SetCookieOnDemand), calling Token
// triggers the on-demand issuance of the token (and its cookie) on the first
// call; use GetToken() instead if you need to detect the failure case where
// the response headers have already been written.
//
// Note that the token won't be available after
// CSRFHandler finishes
// (that is, in another handler that wraps it,
// or after the request has been served)
func Token(req *http.Request) string {
	token, _ := getToken(req)
	return token
}

// ctxTokenRaw returns the token already stored in the context
// without triggering on-demand issuance.
func ctxTokenRaw(req *http.Request) string {
	cmMutex.RLock()
	defer cmMutex.RUnlock()

	ctx, ok := contextMap[req]
	if !ok {
		return ""
	}

	return ctx.token
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

func ctxGetState(req *http.Request) *requestState {
	cmMutex.RLock()
	defer cmMutex.RUnlock()

	ctx, ok := contextMap[req]
	if !ok {
		return nil
	}

	return ctx.state
}

func ctxSetState(req *http.Request, state *requestState) {
	cmMutex.Lock()
	defer cmMutex.Unlock()

	ctx, ok := contextMap[req]
	if !ok {
		ctx = new(csrfContext)
		contextMap[req] = ctx
	}

	ctx.state = state
}

// ctxWriter returns the response writer associated with the request,
// or nil when the request is not served by CSRFHandler.
func ctxWriter(req *http.Request) http.ResponseWriter {
	state := ctxGetState(req)
	if state == nil {
		return nil
	}

	return state.writer
}

// getToken returns the token for the request, triggering on-demand issuance
// when configured and necessary.
func getToken(req *http.Request) (string, error) {
	cmMutex.RLock()
	ctx, ok := contextMap[req]
	cmMutex.RUnlock()

	if !ok {
		return "", ErrContextNotAvailable
	}

	return ctx.token, onDemandIssue(req)
}

func ctxClear(req *http.Request) {
	cmMutex.Lock()
	defer cmMutex.Unlock()

	delete(contextMap, req)
}
