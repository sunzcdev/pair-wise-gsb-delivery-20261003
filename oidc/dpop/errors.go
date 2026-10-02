package dpop

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrorCode is a machine-readable error code for a failed DPoP proof
// validation. The values match the "error" parameter values defined by
// RFC 9449 sections 4.3 and 5.2.
type ErrorCode string

const (
	// ErrorInvalidToken means the DPoP proof is either malformed, missing
	// required claims or headers, or failed signature, method, URI, token
	// hash, or public-key binding validation.
	ErrorInvalidToken ErrorCode = "invalid_dpop_proof"

	// ErrorUseDPoPNonce means the request lacks the server-issued nonce,
	// carries an invalid nonce, or uses a nonce associated with a different
	// DPoP key. The required nonce is returned in the DPoP-Nonce response
	// header.
	ErrorUseDPoPNonce ErrorCode = "use_dpop_nonce"
)

// Error is returned by [Verifier.Verify] and [Verifier.VerifyRequest] when a
// DPoP proof cannot be accepted.
type Error struct {
	// Code is the RFC 9449 error code.
	Code ErrorCode

	// Reason describes why the proof was rejected. It is safe to log but is
	// not intended to be shown to end users.
	Reason string

	// nonce is the current server nonce carried by a use_dpop_nonce
	// challenge.
	nonce string

	err error
}

func proofError(format string, args ...any) *Error {
	return &Error{Code: ErrorInvalidToken, Reason: fmt.Sprintf(format, args...)}
}

func nonceError(nonce string, cause error, format string, args ...any) *Error {
	return &Error{
		Code:   ErrorUseDPoPNonce,
		Reason: fmt.Sprintf(format, args...),
		nonce:  nonce,
		err:    cause,
	}
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.err != nil {
		return fmt.Sprintf("dpop: %s: %s: %v", e.Code, e.Reason, e.err)
	}
	return fmt.Sprintf("dpop: %s: %s", e.Code, e.Reason)
}

// Unwrap returns the underlying error, if any.
func (e *Error) Unwrap() error { return e.err }

func (e *Error) nonceValue() string {
	if e == nil {
		return ""
	}
	return e.nonce
}

// NonceChallenge reports whether the error is a use_dpop_nonce challenge and,
// if so, returns the nonce value the client must echo in a retried proof.
func (e *Error) NonceChallenge() (nonce string, ok bool) {
	if e == nil || e.Code != ErrorUseDPoPNonce {
		return "", false
	}
	return e.nonceValue(), true
}

// Write responds to req with an error response that conforms to RFC 9449
// sections 4.3 and 5.2. A use_dpop_nonce failure is reported as 401 with the
// current nonce in the DPoP-Nonce header; every other failure is reported as
// 400. The "realm" and "error_description" parameters described by the RFC
// are added to the WWW-Authenticate challenge.
func (e *Error) Write(w http.ResponseWriter, realm string) {
	if e == nil {
		return
	}
	if realm == "" {
		realm = "resource"
	}
	w.Header().Set("Content-Type", "application/json")
	if e.Code == ErrorUseDPoPNonce {
		if nonce, ok := e.NonceChallenge(); ok {
			w.Header().Set("DPoP-Nonce", nonce)
		}
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`DPoP realm="%s", error="%s", error_description="%s"`,
				realm, e.Code, httpErrorDescription(e.Reason)))
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":%q}`, e.Code)
		return
	}
	w.Header().Set("WWW-Authenticate",
		fmt.Sprintf(`DPoP realm="%s", error="%s", error_description="%s"`,
			realm, e.Code, httpErrorDescription(e.Reason)))
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `{"error":%q}`, e.Code)
}

// httpErrorDescription makes a reason safe to embed in a quoted-string by
// collapsing characters not allowed there into spaces.
func httpErrorDescription(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			r = ' '
		}
		b = append(b, r)
	}
	return string(b)
}

// ErrReplay is returned by implementations of [ReplayStore.Add] when the
// proof identifier has already been recorded.
var ErrReplay = errors.New("dpop: proof jti already used")
