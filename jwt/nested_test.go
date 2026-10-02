package jwt

import (
	"errors"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
)

var (
	nestedSignKey = []byte("0123456789abcdef0123456789abcdef")
	nestedEncKey  = []byte("fedcba9876543210")
)

func mustSigner(t *testing.T, key interface{}, typ string) jose.Signer {
	t.Helper()
	opts := &jose.SignerOptions{}
	if typ != "" {
		opts = opts.WithType(jose.ContentType(typ))
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: key}, opts)
	if err != nil {
		t.Fatalf("failed to create signer: %s", err)
	}
	return signer
}

func mustEncrypter(t *testing.T, key interface{}, typ, cty string) jose.Encrypter {
	t.Helper()
	opts := &jose.EncrypterOptions{}
	if typ != "" {
		opts = opts.WithType(jose.ContentType(typ))
	}
	if cty != "" {
		opts = opts.WithContentType(jose.ContentType(cty))
	}
	enc, err := jose.NewEncrypter(
		jose.A128GCM,
		jose.Recipient{Algorithm: jose.DIRECT, Key: key},
		opts,
	)
	if err != nil {
		t.Fatalf("failed to create encrypter: %s", err)
	}
	return enc
}

func signCompact(t *testing.T, key interface{}, typ, payload string) string {
	t.Helper()
	jws, err := mustSigner(t, key, typ).Sign([]byte(payload))
	if err != nil {
		t.Fatalf("failed to sign: %s", err)
	}
	out, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("failed to serialize JWS: %s", err)
	}
	return out
}

func encryptCompact(t *testing.T, key interface{}, typ, cty, payload string) string {
	t.Helper()
	jwe, err := mustEncrypter(t, key, typ, cty).Encrypt([]byte(payload))
	if err != nil {
		t.Fatalf("failed to encrypt: %s", err)
	}
	out, err := jwe.CompactSerialize()
	if err != nil {
		t.Fatalf("failed to serialize JWE: %s", err)
	}
	return out
}

// nestedPolicy declares the chain expected by the interface under test:
// outer JWE (cty JWT), inner JWS (typ JWT).
func nestedPolicy() NestedPolicy {
	return NestedPolicy{
		Layers: []LayerPolicy{
			{
				Kind:              LayerJWE,
				KeyAlgorithms:     []jose.KeyAlgorithm{jose.DIRECT},
				ContentEncryption: []jose.ContentEncryption{jose.A128GCM},
				Key:               nestedEncKey,
				ContentType:       "JWT",
			},
			{
				Kind:                LayerJWS,
				SignatureAlgorithms: []jose.SignatureAlgorithm{jose.HS256},
				Key:                 nestedSignKey,
				Type:                "JWT",
			},
		},
		MaxDepth: 2,
	}
}

const nestedClaims = `{"sub":"subject","iss":"issuer","secret":"s3cr3t-claim-value"}`

func layerErr(t *testing.T, err error) *LayerError {
	t.Helper()
	var le *LayerError
	if !errors.As(err, &le) {
		t.Fatalf("expected *LayerError, got %T (%v)", err, err)
	}
	return le
}

func TestParseNestedValid(t *testing.T) {
	inner := signCompact(t, nestedSignKey, "JWT", nestedClaims)
	token := encryptCompact(t, nestedEncKey, "", "JWT", inner)

	tok, err := ParseNested(token, nestedPolicy())
	if err != nil {
		t.Fatalf("ParseNested failed on valid token: %s", err)
	}
	if len(tok.Headers) != 2 {
		t.Fatalf("expected 2 layer headers, got %d", len(tok.Headers))
	}

	cl := Claims{}
	private := map[string]string{}
	if err := tok.Claims(&cl, &private); err != nil {
		t.Fatalf("Claims failed: %s", err)
	}
	if cl.Subject != "subject" || cl.Issuer != "issuer" {
		t.Errorf("unexpected claims: %+v", cl)
	}
	if private["secret"] != "s3cr3t-claim-value" {
		t.Errorf("unexpected private claims: %v", private)
	}
}

// TestParseNestedReversedOrder feeds a token that is encrypted-then-signed
// (JWS outside, JWE inside) to a policy that requires signed-then-encrypted.
// It must be rejected at layer 0, before any decryption is attempted.
func TestParseNestedReversedOrder(t *testing.T) {
	inner := encryptCompact(t, nestedEncKey, "", "", nestedClaims)
	token := signCompact(t, nestedSignKey, "JWT", inner)

	_, err := ParseNested(token, nestedPolicy())
	le := layerErr(t, err)
	if le.Layer != 0 {
		t.Errorf("expected rejection at layer 0, got layer %d (%v)", le.Layer, err)
	}
	if strings.Contains(err.Error(), "s3cr3t-claim-value") {
		t.Errorf("error leaks plaintext: %s", err)
	}
}

// TestParseNestedInnerReplaced replaces the inner JWS with one signed by an
// attacker-controlled key. The outer JWE still decrypts fine, so the
// rejection must happen at layer 1, and the claims must never be delivered.
func TestParseNestedInnerReplaced(t *testing.T) {
	attackerKey := []byte("attacker-controlled-signing-key!")
	inner := signCompact(t, attackerKey, "JWT", nestedClaims)
	token := encryptCompact(t, nestedEncKey, "", "JWT", inner)

	_, err := ParseNested(token, nestedPolicy())
	le := layerErr(t, err)
	if le.Layer != 1 {
		t.Errorf("expected rejection at layer 1, got layer %d (%v)", le.Layer, err)
	}
	if strings.Contains(err.Error(), "s3cr3t-claim-value") {
		t.Errorf("error leaks plaintext: %s", err)
	}
}

// TestParseNestedInnerReplacedByJWE swaps the declared inner JWS for a JWE.
// The layer kind mismatch must be reported at layer 1.
func TestParseNestedInnerReplacedByJWE(t *testing.T) {
	inner := encryptCompact(t, nestedEncKey, "", "", nestedClaims)
	token := encryptCompact(t, nestedEncKey, "", "JWT", inner)

	_, err := ParseNested(token, nestedPolicy())
	le := layerErr(t, err)
	if le.Layer != 1 {
		t.Errorf("expected rejection at layer 1, got layer %d (%v)", le.Layer, err)
	}
	if strings.Contains(err.Error(), "s3cr3t-claim-value") {
		t.Errorf("error leaks plaintext: %s", err)
	}
}

// TestParseNestedExtraLayer wraps the declared chain in one more JWS layer.
// The undeclared nesting must be rejected as exceeding the declared depth.
func TestParseNestedExtraLayer(t *testing.T) {
	innermost := signCompact(t, nestedSignKey, "JWT", nestedClaims)
	extra := signCompact(t, nestedSignKey, "JWT", innermost)
	token := encryptCompact(t, nestedEncKey, "", "JWT", extra)

	_, err := ParseNested(token, nestedPolicy())
	le := layerErr(t, err)
	if !errors.Is(err, ErrNestedTooDeep) {
		t.Errorf("expected ErrNestedTooDeep, got %v", err)
	}
	if le.Layer != 2 {
		t.Errorf("expected rejection at layer 2, got layer %d (%v)", le.Layer, err)
	}
	if strings.Contains(err.Error(), "s3cr3t-claim-value") {
		t.Errorf("error leaks plaintext: %s", err)
	}
}

// TestParseNestedWrongOuterKey: decryption fails at layer 0 and the error
// must not contain the (unrecoverable) plaintext.
func TestParseNestedWrongOuterKey(t *testing.T) {
	inner := signCompact(t, nestedSignKey, "JWT", nestedClaims)
	token := encryptCompact(t, nestedEncKey, "", "JWT", inner)

	policy := nestedPolicy()
	policy.Layers[0].Key = []byte("0000000000000000")

	_, err := ParseNested(token, policy)
	le := layerErr(t, err)
	if le.Layer != 0 {
		t.Errorf("expected rejection at layer 0, got layer %d (%v)", le.Layer, err)
	}
	if strings.Contains(err.Error(), "s3cr3t-claim-value") {
		t.Errorf("error leaks plaintext: %s", err)
	}
}

// TestParseNestedHeaderMismatch checks typ/cty enforcement per layer.
func TestParseNestedHeaderMismatch(t *testing.T) {
	// Outer JWE without cty: JWT.
	inner := signCompact(t, nestedSignKey, "JWT", nestedClaims)
	token := encryptCompact(t, nestedEncKey, "", "", inner)

	_, err := ParseNested(token, nestedPolicy())
	le := layerErr(t, err)
	if le.Layer != 0 || !errors.Is(err, ErrInvalidContentType) {
		t.Errorf("expected ErrInvalidContentType at layer 0, got %v", err)
	}

	// Inner JWS without typ: JWT.
	inner = signCompact(t, nestedSignKey, "", nestedClaims)
	token = encryptCompact(t, nestedEncKey, "", "JWT", inner)

	_, err = ParseNested(token, nestedPolicy())
	le = layerErr(t, err)
	if le.Layer != 1 || !errors.Is(err, ErrInvalidType) {
		t.Errorf("expected ErrInvalidType at layer 1, got %v", err)
	}
}

// TestParseNestedAlgorithmMismatch ensures a layer using a non-declared
// algorithm is rejected at that layer.
func TestParseNestedAlgorithmMismatch(t *testing.T) {
	otherKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS512, Key: otherKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("failed to create signer: %s", err)
	}
	jws, err := signer.Sign([]byte(nestedClaims))
	if err != nil {
		t.Fatalf("failed to sign: %s", err)
	}
	inner, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("failed to serialize: %s", err)
	}
	token := encryptCompact(t, nestedEncKey, "", "JWT", inner)

	_, err = ParseNested(token, nestedPolicy())
	le := layerErr(t, err)
	if le.Layer != 1 {
		t.Errorf("expected rejection at layer 1, got layer %d (%v)", le.Layer, err)
	}
}

func TestParseNestedInvalidPolicy(t *testing.T) {
	if _, err := ParseNested("whatever", NestedPolicy{}); !errors.Is(err, ErrInvalidNestedPolicy) {
		t.Errorf("expected ErrInvalidNestedPolicy for empty layers, got %v", err)
	}

	policy := nestedPolicy()
	policy.MaxDepth = 1
	if _, err := ParseNested("whatever", policy); !errors.Is(err, ErrInvalidNestedPolicy) {
		t.Errorf("expected ErrInvalidNestedPolicy for depth overflow, got %v", err)
	}

	policy = nestedPolicy()
	policy.Layers[0].ContentEncryption = nil
	if _, err := ParseNested("whatever", policy); !errors.Is(err, ErrInvalidNestedPolicy) {
		t.Errorf("expected ErrInvalidNestedPolicy for missing enc algorithms, got %v", err)
	}

	policy = nestedPolicy()
	policy.Layers[1].SignatureAlgorithms = nil
	if _, err := ParseNested("whatever", policy); !errors.Is(err, ErrInvalidNestedPolicy) {
		t.Errorf("expected ErrInvalidNestedPolicy for missing sig algorithms, got %v", err)
	}
}

// TestParseNestedSingleLayer ensures a one-layer chain works and that a
// two-layer token is rejected as too deep.
func TestParseNestedSingleLayer(t *testing.T) {
	policy := NestedPolicy{
		Layers: []LayerPolicy{{
			Kind:                LayerJWS,
			SignatureAlgorithms: []jose.SignatureAlgorithm{jose.HS256},
			Key:                 nestedSignKey,
			Type:                "JWT",
		}},
	}

	token := signCompact(t, nestedSignKey, "JWT", nestedClaims)
	tok, err := ParseNested(token, policy)
	if err != nil {
		t.Fatalf("ParseNested failed: %s", err)
	}
	cl := Claims{}
	if err := tok.Claims(&cl); err != nil {
		t.Fatalf("Claims failed: %s", err)
	}
	if cl.Subject != "subject" {
		t.Errorf("unexpected claims: %+v", cl)
	}

	// JWS wrapping another JWS: one layer deeper than declared.
	wrapped := signCompact(t, nestedSignKey, "JWT", token)
	if _, err := ParseNested(wrapped, policy); !errors.Is(err, ErrNestedTooDeep) {
		t.Errorf("expected ErrNestedTooDeep, got %v", err)
	}
}
