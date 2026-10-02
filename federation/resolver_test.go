package federation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	tAnchor = "https://anchor.test"
	tInter  = "https://inter.test"
	tLeaf   = "https://op.test"
)

var testNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func testClock() func() time.Time {
	t := testNow
	return func() time.Time { return t }
}

func buildChainGraph(t *testing.T, client *LocalClient, now time.Time, leafMeta []byte, hints []string, anchorPolicy, interPolicy []byte, interExpiry time.Time) map[string]*Entity {
	t.Helper()
	if leafMeta == nil {
		leafMeta = []byte(`{
			"issuer": "https://op.test",
			"scopes_supported": ["openid"],
			"grant_types_supported": ["authorization_code"]
		}`)
	}
	if hints == nil {
		hints = []string{tInter}
	}
	interInput := &EntityInput{ID: tInter, AuthorityHints: []string{tAnchor}, FederationMetadata: []byte(`{}`)}
	if !interExpiry.IsZero() {
		interInput.Expiry = interExpiry
	}
	entities, err := AddEntities(client, now,
		&EntityInput{ID: tAnchor, FederationMetadata: []byte(`{}`)},
		interInput,
		&EntityInput{ID: tLeaf, AuthorityHints: hints, ProviderMetadata: leafMeta},
	)
	if err != nil {
		t.Fatal(err)
	}
	if anchorPolicy == nil {
		anchorPolicy = []byte(`{"scopes_supported":{"subset_of":["openid","profile"]}}`)
	}
	if interPolicy == nil {
		interPolicy = []byte(`{"scopes_supported":{"add":["profile"]}}`)
	}
	if _, err := AddStatement(client, entities[tAnchor], &StatementInput{Subject: tInter, ProviderPolicy: anchorPolicy}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := AddStatement(client, entities[tInter], &StatementInput{Subject: tLeaf, ProviderPolicy: interPolicy}, now); err != nil {
		t.Fatal(err)
	}
	return entities
}

func TestResolveProviderHappyPath(t *testing.T) {
	client := NewLocalClient()
	buildChainGraph(t, client, testNow, nil, nil, nil, nil, time.Time{})

	result, err := NewResolver(client, tAnchor).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if err != nil {
		t.Fatal(err)
	}

	got := strings.TrimSpace(string(result.ProviderMetadata))
	if !strings.Contains(got, `"openid","profile"`) {
		t.Fatalf("policy not applied: %s", got)
	}
	entities := result.Chain.Entities()
	if len(entities) != 3 || entities[0] != tLeaf || entities[2] != tAnchor {
		t.Fatalf("chain = %v", entities)
	}
	if len(result.Trace.ValidPaths) != 1 {
		t.Fatalf("valid paths = %v", result.Trace.ValidPaths)
	}
}

func TestLeafURLIsNotATrustAnchor(t *testing.T) {
	client := NewLocalClient()
	buildChainGraph(t, client, testNow, nil, nil, nil, nil, time.Time{})

	// Configuring the leaf itself as an anchor must not produce a
	// zero-length chain bypass; leaf configs carry no policy anchor and the
	// point of federation is to reach an externally configured anchor.
	if _, err := NewResolver(client, tLeaf).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf); err == nil {
		t.Fatal("resolution succeeded with leaf configured as sole trust anchor")
	}

	// An anchor that appears nowhere on any path cannot be reached either.
	_, err := NewResolver(client, "https://unrelated.test").WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if !errors.Is(err, ErrNoTrustPath) {
		t.Fatalf("err = %v, want ErrNoTrustPath", err)
	}
}

func TestExpiredIntermediateConfigurationRejected(t *testing.T) {
	client := NewLocalClient()
	buildChainGraph(t, client, testNow, nil, nil, nil, nil, testNow.Add(-time.Minute))

	_, err := NewResolver(client, tAnchor).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want expiry rejection", err)
	}
}

func TestExpiredSubordinateStatementRejected(t *testing.T) {
	client := NewLocalClient()
	entities := buildChainGraph(t, client, testNow, nil, nil, []byte(`{}`), []byte(`{}`), time.Time{})

	// Replace the inter -> leaf statement with an expired one.
	expired, err := SignEntityStatement(&StatementInput{
		Issuer:  entities[tInter],
		Subject: tLeaf,
		Expiry:  testNow.Add(-time.Minute),
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	client.PutEntityStatement(tInter, tLeaf, expired)

	_, err = NewResolver(client, tAnchor).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want expired statement rejection", err)
	}
}

func TestWrongSignerRejected(t *testing.T) {
	client := NewLocalClient()
	entities := buildChainGraph(t, client, testNow, nil, nil, []byte(`{}`), []byte(`{}`), time.Time{})

	// Sign a statement with iss=inter, sub=leaf but using the anchor's key.
	// Verification against the intermediate's published jwks must fail.
	forged, err := signStatementsWithIssuer(entities[tAnchor], tInter, tLeaf, testNow)
	if err != nil {
		t.Fatal(err)
	}
	client.PutEntityStatement(tInter, tLeaf, forged)

	_, err = NewResolver(client, tAnchor).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if err == nil || (!strings.Contains(err.Error(), "signature") && !strings.Contains(err.Error(), "no issuer key")) {
		t.Fatalf("err = %v, want signature verification rejection", err)
	}
}

func TestSubjectMismatchRejected(t *testing.T) {
	client := NewLocalClient()
	entities := buildChainGraph(t, client, testNow, nil, nil, []byte(`{}`), []byte(`{}`), time.Time{})

	// The intermediate signs a statement whose sub does not match the
	// entity that listed it as an authority.
	wrong, err := SignEntityStatement(&StatementInput{
		Issuer:  entities[tInter],
		Subject: "https://someone-else.test",
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	client.PutEntityStatement(tInter, tLeaf, wrong)

	_, err = NewResolver(client, tAnchor).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if err == nil || !strings.Contains(err.Error(), "subject") {
		t.Fatalf("err = %v, want subject mismatch rejection", err)
	}
}

func TestIssuerMismatchRejected(t *testing.T) {
	client := NewLocalClient()
	entities := buildChainGraph(t, client, testNow, nil, nil, []byte(`{}`), []byte(`{}`), time.Time{})

	// Sign with the intermediate's key (so the signature is valid) but
	// claim the anchor is the issuer, breaking the iss relationship.
	var wrong string
	claimsWrongIssuer := map[string]any{
		"iss": tAnchor,
		"sub": tLeaf,
		"iat": testNow.Add(-time.Minute).Unix(),
		"exp": testNow.Add(time.Hour).Unix(),
	}
	var err error
	wrong, err = signStatement(entities[tInter].Key, entities[tInter].KeyID, claimsWrongIssuer)
	if err != nil {
		t.Fatal(err)
	}
	client.PutEntityStatement(tInter, tLeaf, wrong)

	_, err = NewResolver(client, tAnchor).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if err == nil || (!strings.Contains(err.Error(), "issuer") && !strings.Contains(err.Error(), "signature")) {
		t.Fatalf("err = %v, want issuer relationship rejection", err)
	}
}

func TestBrokenEdgeRejected(t *testing.T) {
	client := NewLocalClient()
	buildChainGraph(t, client, testNow, nil, nil, nil, nil, time.Time{})
	client.BreakStatement(tInter, tLeaf)

	_, err := NewResolver(client, tAnchor).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if !errors.Is(err, ErrNoTrustPath) {
		t.Fatalf("err = %v, want ErrNoTrustPath", err)
	}
}

func TestCycleAndMultiplePaths(t *testing.T) {
	client := NewLocalClient()

	other := "https://other.test"
	entities, err := AddEntities(client, testNow,
		&EntityInput{ID: tAnchor, FederationMetadata: []byte(`{}`)},
		&EntityInput{ID: other, AuthorityHints: []string{tAnchor}, FederationMetadata: []byte(`{}`)},
		&EntityInput{
			ID:                 tInter,
			AuthorityHints:     []string{tAnchor, other, tLeaf}, // tLeaf entry creates a cycle
			FederationMetadata: []byte(`{}`),
		},
		&EntityInput{
			ID:             tLeaf,
			AuthorityHints: []string{tInter},
			ProviderMetadata: []byte(`{
				"issuer": "https://op.test",
				"scopes_supported": ["openid"]
			}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	mustTestStatement(t, client, entities[tAnchor], tInter, `{"scopes_supported":{"subset_of":["openid","profile"]}}`)
	mustTestStatement(t, client, entities[tAnchor], other, `{}`)
	mustTestStatement(t, client, entities[other], tInter, `{}`)
	mustTestStatement(t, client, entities[tInter], tLeaf, `{"scopes_supported":{"add":["profile"]}}`)

	result, err := NewResolver(client, tAnchor).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if err != nil {
		t.Fatal(err)
	}

	// Shortest chain wins.
	if len(result.Chain.Entities()) != 3 {
		t.Fatalf("chosen chain = %v, want the direct 3-entity path", result.Chain.Entities())
	}
	if len(result.Trace.ValidPaths) != 2 {
		t.Fatalf("valid paths = %v, want 2", result.Trace.ValidPaths)
	}

	// The cycle edge must appear among the rejected attempts.
	sawCycle := false
	for _, attempt := range result.Trace.Attempts {
		if strings.Contains(attempt.Reason.Error(), "cycle") {
			sawCycle = true
		}
	}
	if !sawCycle {
		t.Fatalf("cycle not reported in trace: %+v", result.Trace.Attempts)
	}
}

func TestUntrustedAnchorPathNotAccepted(t *testing.T) {
	client := NewLocalClient()
	rogue := "https://rogue.test"
	entities, err := AddEntities(client, testNow,
		&EntityInput{ID: rogue, FederationMetadata: []byte(`{}`)},
		&EntityInput{
			ID:               tLeaf,
			AuthorityHints:   []string{rogue},
			ProviderMetadata: []byte(`{"issuer":"https://op.test"}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	mustTestStatement(t, client, entities[rogue], tLeaf, `{"issuer":{"value":"https://attacker.test"}}`)

	_, err = NewResolver(client, tAnchor).WithClock(testClock()).ResolveProvider(context.Background(), tLeaf)
	if !errors.Is(err, ErrNoTrustPath) {
		t.Fatalf("err = %v, want ErrNoTrustPath for chain ending at non-anchor", err)
	}
}

func mustTestStatement(t *testing.T, client *LocalClient, issuer *Entity, subject, policy string) {
	t.Helper()
	if _, err := AddStatement(client, issuer, &StatementInput{
		Subject:        subject,
		ProviderPolicy: []byte(policy),
	}, testNow); err != nil {
		t.Fatal(err)
	}
}

func signStatementsWithIssuer(anchorKey *Entity, issuer, subject string, now time.Time) (string, error) {
	// Build claims manually so the issuer string is issuer but the signature
	// comes from a different key.
	return signStatement(anchorKey.Key, anchorKey.KeyID, map[string]any{
		"iss": issuer,
		"sub": subject,
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
}
