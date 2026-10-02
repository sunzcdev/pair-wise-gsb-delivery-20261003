package dpop

import (
	"encoding/json"
	"errors"

	"github.com/coreos/go-oidc/v3/oidc"
)

// TokenBinding is the confirmation key (the "cnf" claim) that an access token
// has independently been verified to carry.
//
// JWKThumbprint is the base64url-encoded SHA-256 JWK Thumbprint of the DPoP
// public key (RFC 7638), i.e. the "cnf.jkt" member. The thumbprint is the
// value the DPoP proof's embedded key is compared against.
type TokenBinding struct {
	JWKThumbprint string
}

// cnfClaim is the subset of a token's confirmation claim relevant to DPoP.
type cnfClaim struct {
	JWKThumbprint string `json:"jkt"`
}

// BindingFromClaims extracts the DPoP binding from the raw JSON claims of an
// already-verified access token, such as the payload of a JWT access token or
// an OAuth 2.0 token introspection response:
//
//	{ "cnf": { "jkt": "0ZcOC2Ok31..." } }
//
// The bytes MUST come from a token whose signature and issuer the caller has
// already validated, or from an introspection response obtained over a
// trusted, authenticated channel. Passing in claims parsed from an
// unverified token would let an attacker choose the thumbprint that their own
// proof is checked against.
func BindingFromClaims(rawClaims []byte) (*TokenBinding, error) {
	var claims struct {
		Cnf cnfClaim `json:"cnf"`
	}
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		return nil, err
	}
	if claims.Cnf.JWKThumbprint == "" {
		return nil, errors.New("dpop: access token claims contain no cnf.jkt binding")
	}
	return &TokenBinding{JWKThumbprint: claims.Cnf.JWKThumbprint}, nil
}

// BindingFromIDToken extracts the DPoP binding from an [oidc.IDToken] that has
// already been verified by [oidc.IDTokenVerifier.Verify]. The verifier is
// what establishes trust in the embedded "cnf.jkt"; this helper never parses
// an unverified token string.
func BindingFromIDToken(token *oidc.IDToken) (*TokenBinding, error) {
	if token == nil {
		return nil, errors.New("dpop: nil ID token")
	}
	var claims struct {
		Cnf cnfClaim `json:"cnf"`
	}
	if err := token.Claims(&claims); err != nil {
		return nil, err
	}
	if claims.Cnf.JWKThumbprint == "" {
		return nil, errors.New("dpop: access token contains no cnf.jkt binding")
	}
	return &TokenBinding{JWKThumbprint: claims.Cnf.JWKThumbprint}, nil
}
