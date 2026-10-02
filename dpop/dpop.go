// Package dpop implements resource-server side verification of DPoP proofs
// as defined by RFC 9449 ("OAuth 2.0 Demonstrating Proof of Possession").
//
// A DPoP proof is a short-lived, asymmetrically signed JWT that the client
// sends alongside a DPoP-bound access token. The proof demonstrates that the
// caller holds the private key whose public key is bound to the access token
// through its "cnf.jkt" confirmation claim.
//
// This package intentionally does not verify access tokens. The caller is
// expected to validate the access token itself (signature, issuer, expiry,
// audience) and then pass the confirmation claim it obtained from that
// verified token to [Verifier.Verify]. The proof's embedded JWK is never
// treated as a source of binding information: an attacker can mint a proof
// with any key, so binding must come solely from the already-verified access
// token.
package dpop

import (
	"context"
	"crypto"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ProofType is the required value of the JWT "typ" header for DPoP proofs
// (RFC 9449, section 4.2).
const ProofType = "dpop+jwt"

// HeaderName is the HTTP request header that carries the DPoP proof.
const HeaderName = "DPoP"

// NonceHeaderName is the HTTP response/request header used for server-issued
// nonce challenges (RFC 9449, section 8).
const NonceHeaderName = "DPoP-Nonce"

// Binding holds the key confirmation information that the resource server
// extracted from an already-verified access token.
//
// The field names and semantics match the token "cnf" claim defined by
// RFC 7800 and RFC 9449, section 6.1. Exactly one of the fields should be
// populated; [Verifier] requires JKT for DPoP-bound tokens.
type Binding struct {
	// JKT is the base64url-encoded SHA-256 JWK Thumbprint (RFC 7638) of the
	// public key the access token is bound to. It comes from the "cnf.jkt"
	// claim of a verified access token.
	JKT string `json:"jkt,omitempty"`

	// X5tS256 optionally carries the "cnf.x5t#S256" confirmation. It is not
	// checked against the proof by this package, which only supports the
	// JWK-thumbprint binding, but is accepted when decoding a "cnf" claim so
	// that [BindingFromClaims] round-trips the full claim.
	X5tS256 string `json:"x5t#S256,omitempty"`
}

// IsDPoP reports whether the binding carries a JWK thumbprint and therefore
// represents a DPoP-bound token.
func (b Binding) IsDPoP() bool { return b.JKT != "" }

// BindingFromClaims extracts the "cnf" confirmation claim from an arbitrary
// JSON claim value. The argument is typically obtained with
// [github.com/coreos/go-oidc/v3/oidc.IDToken.Claims] or
// [encoding/json.Unmarshal] on an access token that the caller has already
// verified out of band.
//
// It returns an empty [Binding] (no error) when there is no "cnf" claim, so
// that callers can treat bearer tokens uniformly. It must never be called on
// an unverified token: the result is only as trustworthy as the input.
func BindingFromClaims(claims any) (Binding, error) {
	raw, err := json.Marshal(claims)
	if err != nil {
		return Binding{}, fmt.Errorf("dpop: marshaling access token claims: %w", err)
	}
	var wrapper struct {
		CNF *Binding `json:"cnf"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return Binding{}, fmt.Errorf("dpop: decoding access token cnf claim: %w", err)
	}
	if wrapper.CNF == nil {
		return Binding{}, nil
	}
	return *wrapper.CNF, nil
}

// Request describes the HTTP request a proof must be bound to.
type Request struct {
	// Method is the HTTP request method, compared case-sensitively against
	// the proof's "htm" claim. Use the on-the-wire uppercase form (GET,
	// POST, ...).
	Method string

	// TargetURI is the absolute request URI without query and fragment,
	// compared against the proof's "htu" claim. It must use the http or
	// https scheme.
	TargetURI string
}

// Proof is a successfully verified DPoP proof.
type Proof struct {
	// JTI is the proof's unique identifier ("jti" claim). It has already
	// been recorded by the replay store when returned.
	JTI string

	// Thumbprint is the base64url-encoded SHA-256 JWK Thumbprint (RFC 7638)
	// of the public key from the proof header, computed after the signature
	// was verified against that key.
	Thumbprint string

	// Nonce is the server-issued nonce presented in the proof, if any.
	Nonce string

	// IssuedAt is the proof's "iat" claim.
	IssuedAt time.Time
}

// Sentinel errors returned by [Verifier.Verify]. All wrapped errors returned
// by the verifier satisfy errors.Is against one of these.
var (
	// ErrMalformedProof means the DPoP header did not contain a single,
	// syntactically valid compact JWT.
	ErrMalformedProof = errors.New("dpop: malformed DPoP proof")

	// ErrInvalidSignature means the proof signature did not validate against
	// the embedded public JWK, the "alg" header was missing/unsupported, or
	// the embedded JWK was not a valid asymmetric public key.
	ErrInvalidSignature = errors.New("dpop: invalid proof signature")

	// ErrInvalidHeader means a protected header parameter (typ, jwk, alg)
	// did not satisfy RFC 9449, section 4.2.
	ErrInvalidHeader = errors.New("dpop: invalid proof header")

	// ErrInvalidClaims means a required payload claim (htm, htu, iat, jti,
	// ath) was missing or did not bind the proof to the current request.
	ErrInvalidClaims = errors.New("dpop: invalid proof claims")

	// ErrTokenDigestMismatch means the proof's "ath" claim did not equal
	// the SHA-256 hash of the access token presented with the request.
	ErrTokenDigestMismatch = errors.New("dpop: proof access token hash mismatch")

	// ErrKeyBindingMismatch means the thumbprint of the proof's JWK did not
	// equal the "jkt" confirmation of the verified access token.
	ErrKeyBindingMismatch = errors.New("dpop: proof key does not match token binding")

	// ErrProofExpired means the proof's "iat" claim was too far in the past
	// or in the future.
	ErrProofExpired = errors.New("dpop: proof issued-at outside acceptable window")

	// ErrReplayed means a proof with the same "jti" was already presented.
	ErrReplayed = errors.New("dpop: proof jti has already been used")

	// ErrNonceRequired means the verifier is enforcing nonces and the proof
	// did not contain one. The returned error is a [*NonceError].
	ErrNonceRequired = errors.New("dpop: server nonce required")

	// ErrNonceInvalid means the proof carried an unknown or expired nonce.
	// The returned error is a [*NonceError].
	ErrNonceInvalid = errors.New("dpop: invalid server nonce")
)

// NonceError carries the fresh nonce that the client must echo in a retried
// request. Resource servers write it to the DPoP-Nonce response header.
type NonceError struct {
	// Err is ErrNonceRequired or ErrNonceInvalid.
	Err error
	// Nonce is a fresh server nonce to use on the retry.
	Nonce string
}

func (e *NonceError) Error() string { return e.Err.Error() }
func (e *NonceError) Unwrap() error { return e.Err }

// proofClaims is the set of DPoP proof payload claims from RFC 9449,
// section 4.2 plus the "ath" claim from section 4.3.
type proofClaims struct {
	HTM   string           `json:"htm"`
	HTU   string           `json:"htu"`
	ATH   string           `json:"ath,omitempty"`
	IAT   *jwt.NumericDate `json:"iat"`
	JTI   string           `json:"jti"`
	Nonce string           `json:"nonce,omitempty"`
}

func parseProof(raw string, allowedAlgs []jose.SignatureAlgorithm) (*jwt.JSONWebToken, *proofClaims, error) {
	if strings.TrimSpace(raw) != raw || strings.Count(raw, ".") != 2 {
		return nil, nil, fmt.Errorf("%w: proof must be a single compact JWS", ErrMalformedProof)
	}
	token, err := jwt.ParseSigned(raw, allowedAlgs)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrMalformedProof, err)
	}
	if len(token.Headers) != 1 {
		return nil, nil, fmt.Errorf("%w: proof must use a single signature", ErrInvalidHeader)
	}

	claims := &proofClaims{}
	if err := token.UnsafeClaimsWithoutVerification(claims); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidClaims, err)
	}
	return token, claims, nil
}

func verifyHeader(header jose.Header) (*jose.JSONWebKey, error) {
	if typ, _ := header.ExtraHeaders[jose.HeaderType].(string); typ != ProofType {
		return nil, fmt.Errorf("%w: typ must be %q, got %q", ErrInvalidHeader, ProofType, typ)
	}
	if header.Algorithm == "" || header.Algorithm == "none" {
		return nil, fmt.Errorf("%w: missing or unsupported alg %q", ErrInvalidHeader, header.Algorithm)
	}
	jwk := header.JSONWebKey
	if jwk == nil || jwk.Key == nil {
		return nil, fmt.Errorf("%w: proof header must contain a public JWK", ErrInvalidHeader)
	}
	if !isPublicKey(jwk.Key) {
		return nil, fmt.Errorf("%w: embedded JWK must be a public key", ErrInvalidHeader)
	}
	return jwk, nil
}

// accessTokenHash returns the base64url-encoded SHA-256 hash of the access
// token, i.e. the expected "ath" value (RFC 9449, section 4.3).
func accessTokenHash(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return b64URLEncode(sum[:])
}

func b64URLEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func jwkThumbprint(jwk *jose.JSONWebKey) (string, error) {
	sum, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("%w: computing JWK thumbprint: %v", ErrInvalidHeader, err)
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

func (v *Verifier) validateClaims(claims *proofClaims, req Request, accessToken string, now time.Time) error {
	if claims.HTM == "" || claims.HTU == "" || claims.IAT == nil || claims.JTI == "" {
		return fmt.Errorf("%w: htm, htu, iat and jti are required", ErrInvalidClaims)
	}
	if claims.HTM != req.Method {
		return fmt.Errorf("%w: htm=%q does not match method %q", ErrInvalidClaims, claims.HTM, req.Method)
	}
	htu, err := canonicalURI(claims.HTU)
	if err != nil {
		return fmt.Errorf("%w: htu: %v", ErrInvalidClaims, err)
	}
	target, err := canonicalURI(req.TargetURI)
	if err != nil {
		return fmt.Errorf("%w: target uri: %v", ErrInvalidClaims, err)
	}
	if htu != target {
		return fmt.Errorf("%w: htu=%q does not match %q", ErrInvalidClaims, claims.HTU, req.TargetURI)
	}

	iat := claims.IAT.Time()
	if age := now.Sub(iat); age > v.maxAge+v.leeway {
		return fmt.Errorf("%w: proof is %s old", ErrProofExpired, age)
	} else if age < -v.leeway {
		return fmt.Errorf("%w: proof iat is %s in the future", ErrProofExpired, -age)
	}

	if claims.ATH == "" {
		return fmt.Errorf("%w: ath is required when an access token is presented", ErrInvalidClaims)
	}
	expectedATH := accessTokenHash(accessToken)
	if !secureEqual(claims.ATH, expectedATH) {
		return ErrTokenDigestMismatch
	}
	return nil
}

// contextKey is an unexported type for context keys provided by the package.
type contextKey int

const proofContextKey contextKey = iota

// ProofFromContext returns the verified [Proof] stored on a request context
// by [Middleware], or nil if the request was not handled by the middleware.
func ProofFromContext(ctx context.Context) *Proof {
	p, _ := ctx.Value(proofContextKey).(*Proof)
	return p
}
