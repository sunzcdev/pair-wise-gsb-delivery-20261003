package dpop

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"fmt"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ProofClaims are the client-side payload claims of a DPoP proof
// (RFC 9449, sections 4.2-4.3). They are provided for clients and for tests
// exercising a resource server; the verifier does not use this type.
type ProofClaims struct {
	// HTM is the HTTP method of the request (uppercase).
	HTM string
	// HTU is the absolute target URI without query or fragment.
	HTU string
	// ATH is the base64url SHA-256 hash of the access token. Compute it with
	// [AccessTokenHash]. Empty for proofs sent to the token endpoint without
	// an access token.
	ATH string
	// IAT is the issuance time.
	IAT time.Time
	// JTI is the proof's unique identifier.
	JTI string
	// Nonce is the server-issued nonce being answered, if any.
	Nonce string
}

// AccessTokenHash returns the base64url-encoded SHA-256 hash of an access
// token, the value of the proof "ath" claim (RFC 9449, section 4.3).
func AccessTokenHash(accessToken string) string {
	return accessTokenHash(accessToken)
}

// PublicKeyThumbprint returns the base64url-encoded SHA-256 JWK Thumbprint
// (RFC 7638) of an RSA, ECDSA or Ed25519 public key. This is the value an
// authorization server places in the access token "cnf.jkt" claim.
func PublicKeyThumbprint(publicKey crypto.PublicKey) (string, error) {
	jwk := jose.JSONWebKey{Key: publicKey}
	sum, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("dpop: computing public key thumbprint: %w", err)
	}
	return b64URLEncode(sum), nil
}

// SignProof creates a compact-serialized DPoP proof JWT signed with key, with
// the public key embedded in the "jwk" header and "typ":"dpop+jwt".
//
// The signature algorithm is chosen from the key type: RSA keys use RS256,
// Ed25519 keys use EdDSA, and ECDSA keys use ES256/ES384/ES512 according to
// the curve.
func SignProof(key crypto.Signer, claims ProofClaims) (string, error) {
	alg, err := algForKey(key)
	if err != nil {
		return "", err
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{
		EmbedJWK: true,
	}).WithType(ProofType))
	if err != nil {
		return "", fmt.Errorf("dpop: constructing proof signer: %w", err)
	}

	payload := map[string]interface{}{
		"htm": claims.HTM,
		"htu": claims.HTU,
		"iat": jwt.NewNumericDate(claims.IAT),
		"jti": claims.JTI,
	}
	if claims.ATH != "" {
		payload["ath"] = claims.ATH
	}
	if claims.Nonce != "" {
		payload["nonce"] = claims.Nonce
	}

	raw, err := jwt.Signed(signer).Claims(payload).Serialize()
	if err != nil {
		return "", fmt.Errorf("dpop: serializing proof: %w", err)
	}
	return raw, nil
}

func algForKey(key crypto.Signer) (jose.SignatureAlgorithm, error) {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return jose.RS256, nil
	case ed25519.PrivateKey:
		return jose.EdDSA, nil
	case *ecdsa.PrivateKey:
		switch k.Curve {
		case elliptic.P256():
			return jose.ES256, nil
		case elliptic.P384():
			return jose.ES384, nil
		case elliptic.P521():
			return jose.ES512, nil
		}
	}
	return "", fmt.Errorf("dpop: unsupported DPoP proof key type %T", key)
}
