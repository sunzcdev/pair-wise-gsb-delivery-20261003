package dpop_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/dpop"
	jose "github.com/go-jose/go-jose/v4"
)

// TestBindingFromVerifiedIDToken proves the trust boundary: the thumbprint is
// taken from a token the oidc package verified against the issuer's key, not
// from an unverified token string.
func TestBindingFromVerifiedIDToken(t *testing.T) {
	ctx := context.Background()

	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating issuer key: %v", err)
	}

	keySet := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{issuerKey.Public()}}
	verifier := oidc.NewVerifier("https://as.example.com", keySet, &oidc.Config{
		ClientID:             "resource-server",
		SupportedSigningAlgs: []string{oidc.ES256},
	})

	// This is the thumbprint the AS binds the token to; a proof key the
	// client presents must thumbprint to the same value.
	proofKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating proof key: %v", err)
	}
	proofJWK := jose.JSONWebKey{Key: proofKey.Public()}
	rawTP, err := proofJWK.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	expectedJKT := base64.RawURLEncoding.EncodeToString(rawTP)

	now := time.Now()
	claims := map[string]any{
		"iss": "https://as.example.com",
		"aud": "resource-server",
		"sub": "user-1",
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
		"cnf": map[string]any{"jkt": expectedJKT},
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: issuerKey},
		nil,
	)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	jws, err := signer.Sign(body)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	rawToken, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serializing token: %v", err)
	}

	idToken, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		t.Fatalf("verifying token: %v", err)
	}

	binding, err := dpop.BindingFromIDToken(idToken)
	if err != nil {
		t.Fatalf("extracting binding: %v", err)
	}
	if binding.JWKThumbprint != expectedJKT {
		t.Errorf("jkt = %q, want %q", binding.JWKThumbprint, expectedJKT)
	}
}
