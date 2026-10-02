package federation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

func mustMarshalClaims(t *testing.T, claims map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestEntityConfigurationRequiresEntityStatementType(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	jwks := &jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: key.Public(), KeyID: "k1"}}}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: key},
		// Deliberately wrong typ (a plain JWT instead of an entity statement).
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), "k1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	payload := mustMarshalClaims(t, map[string]any{
		"iss": "https://e.test", "sub": "https://e.test",
		"iat": testNow.Add(-time.Minute).Unix(), "exp": testNow.Add(time.Hour).Unix(),
		"jwks": jwks,
	})
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := obj.CompactSerialize()

	if _, err := verifyConfiguration(raw, "https://e.test", testNow); err == nil ||
		!strings.Contains(err.Error(), "typ") {
		t.Fatalf("err = %v, want typ rejection", err)
	}
}

func TestTamperedPayloadRejected(t *testing.T) {
	client := NewLocalClient()
	entities := buildChainGraph(t, client, testNow, nil, nil, []byte(`{}`), []byte(`{}`), time.Time{})

	// Flip a byte in the signed subordinate statement payload segment.
	raw := client.statements[statementKey(tInter, tLeaf)]
	parts := strings.Split(raw, ".")
	payload := []byte(parts[1])
	if payload[2] == 'A' {
		payload[2] = 'B'
	} else {
		payload[2] = 'A'
	}
	parts[1] = string(payload)
	client.PutEntityStatement(tInter, tLeaf, strings.Join(parts, "."))

	_, err := NewResolver(client, tAnchor).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("err = %v, want signature failure after tampering", err)
	}

	_ = entities
}

func TestNotBeforeInFutureRejected(t *testing.T) {
	client := NewLocalClient()
	buildChainGraph(t, client, testNow, nil, nil, nil, nil, time.Time{})

	// Re-sign the leaf configuration with a future nbf.
	leaf, err := SignEntityConfiguration(&EntityInput{
		ID:             tLeaf,
		AuthorityHints: []string{tInter},
		ProviderMetadata: []byte(`{
			"issuer":"https://op.test",
			"scopes_supported":["openid"]
		}`),
		NBF: testNow.Add(time.Hour),
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	client.PutEntityConfiguration(tLeaf, leaf.ConfigJWT)

	_, err = NewResolver(client, tAnchor).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if err == nil || !strings.Contains(err.Error(), "not yet valid") {
		t.Fatalf("err = %v, want nbf rejection", err)
	}
}
