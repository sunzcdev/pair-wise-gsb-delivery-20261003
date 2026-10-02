package dpop

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// BearerScheme is the RFC 6750 authorization scheme for DPoP-bound tokens.
const BearerScheme = "DPoP"

// AccessTokenBinding is supplied by the caller for every request after it has
// authenticated the access token. It is the trust bridge between the access
// token and the DPoP proof: the verifier never decodes the token itself.
type AccessTokenBinding struct {
	// Token is the raw access token exactly as received in the
	// Authorization header.
	Token string

	// Binding is the "cnf" confirmation extracted from the access token
	// AFTER the caller verified its signature and claims.
	Binding Binding
}

// AccessTokenAuthenticator inspects an authenticated request and returns the
// raw token and its already-verified binding.
//
// Implementations MUST fully verify the access token (signature, issuer,
// audience, expiry) before returning its "cnf" claim. Returning an error
// rejects the request before any DPoP verification runs; a nil result with a
// nil error indicates the request has no access token and will be rejected as
// unauthenticated.
type AccessTokenAuthenticator func(r *http.Request) (*AccessTokenBinding, error)

// MiddlewareConfig configures [Middleware].
type MiddlewareConfig struct {
	// Verifier checks the DPoP proof. Required.
	Verifier *Verifier

	// Authenticate verifies the access token and supplies its binding.
	// Required; see [AccessTokenAuthenticator] for the trust contract.
	Authenticate AccessTokenAuthenticator

	// TargetURL derives the absolute target URI (without query/fragment)
	// for the "htu" comparison. Optional: when nil it is reconstructed from
	// the request (see [RequestURL]).
	TargetURL func(r *http.Request) string

	// ErrorHandler writes the response on failure. Optional; defaults to
	// [WriteError]. It is also called for authenticator errors, with the
	// error the authenticator returned.
	ErrorHandler func(w http.ResponseWriter, r *http.Request, err error)
}

// Middleware returns an http.Handler wrapper enforcing DPoP on every request.
// On success the verified [Proof] is available via [ProofFromContext].
//
// Nonce challenges are answered with 401 and a fresh DPoP-Nonce response
// header; the client retries the same request with that nonce.
func Middleware(config *MiddlewareConfig, next http.Handler) http.Handler {
	verifier := config.Verifier
	authenticate := config.Authenticate
	targetURL := config.TargetURL
	if targetURL == nil {
		targetURL = RequestURL
	}
	errorHandler := config.ErrorHandler
	if errorHandler == nil {
		errorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
			WriteError(w, err)
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawProof := strings.TrimSpace(r.Header.Get(HeaderName))
		if rawProof == "" {
			errorHandler(w, r, ErrMalformedProof)
			return
		}

		authenticated, err := authenticate(r)
		if err != nil {
			errorHandler(w, r, err)
			return
		}
		if authenticated == nil || authenticated.Token == "" {
			errorHandler(w, r, &bindingError{message: "dpop: missing access token"})
			return
		}

		proof, err := verifier.Verify(r.Context(), rawProof, Request{
			Method:    r.Method,
			TargetURI: targetURL(r),
		}, authenticated.Token, authenticated.Binding)
		if err != nil {
			errorHandler(w, r, err)
			return
		}

		ctx := context.WithValue(r.Context(), proofContextKey, proof)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestURL reconstructs the absolute target URI of r without query or
// fragment, using the Host header and the X-Forwarded-Proto hint when present
// (the first comma-separated value). Deployments behind proxies that set
// other forwarding headers should supply a custom TargetURL.
func RequestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(firstValue(r.Header.Get("X-Forwarded-Proto"))); forwarded != "" {
		scheme = strings.ToLower(forwarded)
	}
	return scheme + "://" + r.Host + r.URL.EscapedPath()
}

func firstValue(header string) string {
	if value, _, ok := strings.Cut(header, ","); ok {
		return value
	}
	return header
}

// WriteError responds to a DPoP verification failure per RFC 9449, section 7:
// a 401 with WWW-Authenticate: DPoP and, for nonce failures, a fresh
// DPoP-Nonce header. The error message is sent as the response body.
func WriteError(w http.ResponseWriter, err error) {
	var nonceErr *NonceError
	if errors.As(err, &nonceErr) {
		w.Header().Set(NonceHeaderName, nonceErr.Nonce)
	}
	w.Header().Set("WWW-Authenticate", BearerScheme)
	status := http.StatusUnauthorized
	if errors.Is(err, ErrReplayed) {
		// RFC 9449, section 7.2 prescribes 400 for a replayed proof.
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(err.Error()))
}
