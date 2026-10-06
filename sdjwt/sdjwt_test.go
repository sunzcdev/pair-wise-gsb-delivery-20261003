package sdjwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/json"
)

type testKeys struct {
	issuerKey *ecdsa.PrivateKey
	holderKey *ecdsa.PrivateKey
}

func generateTestKeys(t *testing.T) testKeys {
	t.Helper()
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate issuer key: %v", err)
	}
	holderKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate holder key: %v", err)
	}
	return testKeys{issuerKey: issuerKey, holderKey: holderKey}
}

func signCompact(t *testing.T, key *ecdsa.PrivateKey, claims interface{}, typ string) string {
	t.Helper()
	opts := &jose.SignerOptions{}
	if typ != "" {
		opts = opts.WithHeader(jose.HeaderType, jose.ContentType(typ))
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, opts)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	serialized, err := signJWT(signer, claims)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return serialized
}

func signJWT(signer jose.Signer, claims interface{}) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

func publicJWKMap(t *testing.T, key *ecdsa.PrivateKey) map[string]interface{} {
	t.Helper()
	jwk := jose.JSONWebKey{Key: key.Public()}
	raw, err := json.Marshal(&jwk)
	if err != nil {
		t.Fatalf("marshal jwk: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal jwk: %v", err)
	}
	return m
}

// encodeDisclosure builds a property disclosure [salt, name, value] or array
// element disclosure [salt, value], returning its raw base64url form and the
// decoded Disclosure.
func encodeDisclosure(t *testing.T, salt string, args ...interface{}) string {
	t.Helper()
	arr := append([]interface{}{salt}, args...)
	rawJSON, err := json.Marshal(arr)
	if err != nil {
		t.Fatalf("marshal disclosure: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(rawJSON)
}

func disclosureDigest(t *testing.T, raw string) string {
	t.Helper()
	d, err := parseDisclosure(raw)
	if err != nil {
		t.Fatalf("parse disclosure: %v", err)
	}
	digest, err := digestOf(d.rawJSON, crypto.SHA256, sha256.Size)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return digest
}

type credential struct {
	issuerCompact string
	disclosures   map[string]string // label -> raw disclosure
}

// buildCredential creates an issuer JWT resembling the RFC 9901 Appendix A
// example, with object and array digests and a holder cnf.
func buildCredential(t *testing.T, keys testKeys) credential {
	t.Helper()

	givenName := encodeDisclosure(t, "FklWxmutFMLH_s_n_E9kg", "given_name", "Maximilian")
	familyName := encodeDisclosure(t, "nPuoQnkRFq3AIwSF4rJfNQ", "family_name", "Müller")
	birthdate := encodeDisclosure(t, "birth-salt-3", "birthdate", "1928-03-22")
	streetAddr := encodeDisclosure(t, "addr-salt-1", "street_address", "Weidenpescher Straße 9")
	locality := encodeDisclosure(t, "addr-salt-2", "locality", "Köln")
	nationalityDE := encodeDisclosure(t, "nat-salt-de", "DE")
	nationalityUS := encodeDisclosure(t, "nat-salt-us", "US")

	claims := map[string]interface{}{
		"iss":     "https://example.com/issuer",
		"sub":     "user-42",
		"exp":     1883000000,
		"vct":     "ID-Card",
		"cnf":     map[string]interface{}{"jwk": publicJWKMap(t, keys.holderKey)},
		"_sd_alg": SHA256,
		"_sd":     []string{disclosureDigest(t, givenName), disclosureDigest(t, familyName), disclosureDigest(t, birthdate)},
		"address": map[string]interface{}{
			"country": "DE",
			"_sd":     []string{disclosureDigest(t, streetAddr), disclosureDigest(t, locality)},
		},
		"nationalities": []interface{}{
			map[string]interface{}{"...": disclosureDigest(t, nationalityDE)},
			map[string]interface{}{"...": disclosureDigest(t, nationalityUS)},
		},
	}

	compact := signCompact(t, keys.issuerKey, claims, "")

	return credential{
		issuerCompact: compact,
		disclosures: map[string]string{
			"given_name":     givenName,
			"family_name":    familyName,
			"birthdate":      birthdate,
			"street_address": streetAddr,
			"locality":       locality,
			"nat-DE":         nationalityDE,
			"nat-US":         nationalityUS,
		},
	}
}

func (c credential) presentation(t *testing.T, labels []string, kb string) string {
	t.Helper()
	out := c.issuerCompact + "~"
	for _, label := range labels {
		raw, ok := c.disclosures[label]
		if !ok {
			t.Fatalf("unknown disclosure label %q", label)
		}
		out += raw + "~"
	}
	return out + kb
}

func (c credential) kbJWT(t *testing.T, keys testKeys, pres string, nonce, aud string, iat time.Time) string {
	t.Helper()

	// sd_hash is computed over the presentation up to and including the
	// final "~" before the KB-JWT.
	sdHash := sha256Hash(pres)
	payload := map[string]interface{}{
		"iat":     iat.Unix(),
		"aud":     aud,
		"nonce":   nonce,
		"sd_hash": sdHash,
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: keys.holderKey},
		(&jose.SignerOptions{}).WithHeader(jose.HeaderType, jose.ContentType("kb+jwt")),
	)
	if err != nil {
		t.Fatalf("new kb signer: %v", err)
	}
	kb, err := signJWT(signer, payload)
	if err != nil {
		t.Fatalf("sign kb jwt: %v", err)
	}
	return kb
}

func (c credential) kbJWTWithOptions(t *testing.T, keys testKeys, pres string, payload map[string]interface{}, typ string) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: keys.holderKey},
		(&jose.SignerOptions{}).WithHeader(jose.HeaderType, jose.ContentType(typ)),
	)
	if err != nil {
		t.Fatalf("new kb signer: %v", err)
	}
	kb, err := signJWT(signer, payload)
	if err != nil {
		t.Fatalf("sign kb jwt: %v", err)
	}
	return kb
}

func sha256Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func parseAndVerify(t *testing.T, combined string, key *ecdsa.PrivateKey, expected *KeyBindingExpected) *Claims {
	t.Helper()
	s, err := Parse(combined, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := s.Verify(key.Public(), expected)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return out
}

func mustParse(t *testing.T, combined string) *SDJWT {
	t.Helper()
	s, err := Parse(combined, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return s
}

func parseExpectVerifyError(t *testing.T, combined string, key *ecdsa.PrivateKey, expected *KeyBindingExpected, target error) {
	t.Helper()
	s := mustParse(t, combined)
	_, err := s.Verify(key.Public(), expected)
	if err == nil {
		t.Fatalf("expected error, got success")
	}
	if !errorIs(err, target) {
		t.Fatalf("expected error wrapping %v, got %v", target, err)
	}
}

func errorIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
