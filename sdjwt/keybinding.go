package sdjwt

import (
	"crypto"
	"fmt"
	"math"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/json"
)

// defaultKeyBindingAlgorithms are the asymmetric algorithms accepted by
// default on the Key-Binding JWS (RFC 9901, Section 4.3: the holder proves
// possession of the private key for cnf.jwk, so symmetric algorithms do not
// apply).
var defaultKeyBindingAlgorithms = []jose.SignatureAlgorithm{
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
}

const keyBindingType = "kb+jwt"

type keyBindingClaims struct {
	IAT    *numericDate `json:"iat"`
	Aud    audience     `json:"aud"`
	Nonce  string       `json:"nonce"`
	SDHash string       `json:"sd_hash"`
}

// numericDate is a small RFC 7519 NumericDate type kept local to avoid a
// dependency on the jwt package. It requires an integer JSON number: the KB
// "iat" claim is generated from a Unix timestamp.
type numericDate int64

func (n *numericDate) UnmarshalJSON(b []byte) error {
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	if f > math.MaxInt64 || f < math.MinInt64 {
		return fmt.Errorf("kb-jwt iat out of range")
	}
	if f != float64(int64(f)) {
		return fmt.Errorf("kb-jwt iat must be an integer number of seconds")
	}
	*n = numericDate(f)
	return nil
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var single string
	if err := json.Unmarshal(b, &single); err == nil {
		*a = audience{single}
		return nil
	}
	var multi []string
	if err := json.Unmarshal(b, &multi); err != nil {
		return err
	}
	*a = audience(multi)
	return nil
}

func (a audience) contains(v string) bool {
	for _, e := range a {
		if e == v {
			return true
		}
	}
	return false
}

func (s *SDJWT) verifyKeyBinding(
	issuerClaims map[string]interface{},
	expected *KeyBindingExpected,
	hash crypto.Hash,
) error {
	if expected.Nonce == "" || expected.Audience == "" {
		return fmt.Errorf("%w: expected nonce and audience must both be non-empty", ErrKeyBinding)
	}

	holderJWK, err := extractConfirmationJWK(issuerClaims)
	if err != nil {
		return err
	}

	algs := expected.SignatureAlgorithms
	if algs == nil {
		algs = defaultKeyBindingAlgorithms
	}

	kbJWS, err := jose.ParseSignedCompact(s.KeyBindingJWT, algs)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKeyBinding, err)
	}
	if len(kbJWS.Signatures) != 1 {
		return fmt.Errorf("%w: expected exactly one signature, got %d", ErrKeyBinding, len(kbJWS.Signatures))
	}
	if typ, _ := kbJWS.Signatures[0].Header.ExtraHeaders[jose.HeaderType].(string); typ != keyBindingType {
		return fmt.Errorf("%w: typ header must be %q, got %q", ErrKeyBinding, keyBindingType, typ)
	}

	kbPayload, err := kbJWS.Verify(holderJWK.Key)
	if err != nil {
		return fmt.Errorf("%w: signature does not verify against cnf.jwk: %v", ErrKeyBinding, err)
	}

	var kbClaims keyBindingClaims
	if err := json.Unmarshal(kbPayload, &kbClaims); err != nil {
		return fmt.Errorf("%w: malformed payload: %v", ErrKeyBinding, err)
	}

	expectedHash := s.sdHash(hash)
	if kbClaims.SDHash == "" {
		return fmt.Errorf("%w: missing sd_hash claim", ErrKeyBinding)
	}
	if kbClaims.SDHash != expectedHash {
		return fmt.Errorf("%w: sd_hash mismatch", ErrKeyBinding)
	}

	if kbClaims.Nonce != expected.Nonce {
		return fmt.Errorf("%w: nonce mismatch", ErrKeyBinding)
	}

	if len(kbClaims.Aud) == 0 || !kbClaims.Aud.contains(expected.Audience) {
		return fmt.Errorf("%w: audience mismatch", ErrKeyBinding)
	}

	if kbClaims.IAT == nil {
		return fmt.Errorf("%w: missing iat claim", ErrKeyBinding)
	}

	now := expected.Now
	if now.IsZero() {
		now = time.Now()
	}
	iat := time.Unix(int64(*kbClaims.IAT), 0)
	// Allow a minute of clock skew for the not-in-the-future check.
	if iat.After(now.Add(time.Minute)) {
		return fmt.Errorf("%w: iat claim is in the future", ErrKeyBinding)
	}
	if expected.MaxAge > 0 && now.Sub(iat) > expected.MaxAge {
		return fmt.Errorf("%w: presentation is older than the allowed MaxAge", ErrKeyBinding)
	}

	return nil
}

// sdHash computes the hash over the SD-JWT, including all "~" separators but
// excluding the Key-Binding JWT (RFC 9901, Section 4.3, step 2).
func (s *SDJWT) sdHash(hash crypto.Hash) string {
	var sb strings.Builder
	sb.WriteString(s.issuerCompact)
	sb.WriteByte('~')
	for i, d := range s.Disclosures {
		if i > 0 {
			sb.WriteByte('~')
		}
		sb.WriteString(d.raw)
	}
	sb.WriteByte('~')

	h := hash.New()
	h.Write([]byte(sb.String()))
	return base64URLEncode(h.Sum(nil))
}

func extractConfirmationJWK(claims map[string]interface{}) (*jose.JSONWebKey, error) {
	cnf, ok := claims["cnf"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%w: issuer claims do not contain a cnf claim", ErrKeyBinding)
	}
	rawJWK, ok := cnf["jwk"]
	if !ok {
		return nil, fmt.Errorf("%w: cnf claim does not contain a jwk", ErrKeyBinding)
	}
	jwkJSON, err := json.Marshal(rawJWK)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid cnf.jwk: %v", ErrKeyBinding, err)
	}

	var jwk jose.JSONWebKey
	if err := json.Unmarshal(jwkJSON, &jwk); err != nil {
		return nil, fmt.Errorf("%w: invalid cnf.jwk: %v", ErrKeyBinding, err)
	}
	if !jwk.IsPublic() {
		return nil, fmt.Errorf("%w: cnf.jwk must be a public key", ErrKeyBinding)
	}
	return &jwk, nil
}
