package secure

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type key int

const cspNonceKey key = iota

// ErrInvalidCSPNonce is returned when a CSP nonce carried by the request
// context does not match the expected format and length.
var ErrInvalidCSPNonce = errors.New("secure: invalid CSP nonce")

// cspNonceValue is the value stored under cspNonceKey in request contexts.
// Only values flagged as internal (i.e. generated or validated by this
// package) are eligible for reuse by the ReuseRequestNonce option.
type cspNonceValue struct {
	nonce    string
	internal bool
}

// CSPNonce returns the nonce value associated with the present request.
// If no nonce has been generated it returns an empty string.
func CSPNonce(c context.Context) string {
	switch val := c.Value(cspNonceKey).(type) {
	case cspNonceValue:
		return val.nonce
	case string:
		return val
	}

	return ""
}

// WithCSPNonce returns a context derived from ctx containing the given nonce as a value.
//
// This is intended for testing or more advanced use-cases;
// For ordinary HTTP handlers, clients can rely on this package's middleware to populate the CSP nonce in the context.
func WithCSPNonce(ctx context.Context, nonce string) context.Context {
	return context.WithValue(ctx, cspNonceKey, nonce)
}

func withCSPNonce(r *http.Request, nonce string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), cspNonceKey, cspNonceValue{nonce: nonce, internal: true}))
}

// cspNonceForRequest returns the nonce to use for the given request.
// When the ReuseRequestNonce option is enabled and the request context
// already carries an internal nonce set by this package, that nonce is
// validated and reused. Otherwise a new random nonce is generated.
// Client-provided values (headers or externally set context values) are
// never reused.
func (s *Secure) cspNonceForRequest(r *http.Request) (string, error) {
	if s.opt.ReuseRequestNonce {
		if val, ok := r.Context().Value(cspNonceKey).(cspNonceValue); ok && val.internal {
			if err := validateCSPNonce(val.nonce); err != nil {
				return "", err
			}

			return val.nonce, nil
		}
	}

	return cspRandNonce(), nil
}

// validateCSPNonce checks that the nonce is a base64 (RawStdEncoding) encoded
// value of exactly cspNonceSize bytes.
func validateCSPNonce(nonce string) error {
	decoded, err := base64.RawStdEncoding.DecodeString(nonce)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidCSPNonce, nonce)
	}

	if len(decoded) != cspNonceSize {
		return fmt.Errorf("%w: %q", ErrInvalidCSPNonce, nonce)
	}

	return nil
}

func cspRandNonce() string {
	var buf [cspNonceSize]byte

	_, err := io.ReadFull(rand.Reader, buf[:])
	if err != nil {
		panic("CSP Nonce rand.Reader failed" + err.Error())
	}

	return base64.RawStdEncoding.EncodeToString(buf[:])
}
