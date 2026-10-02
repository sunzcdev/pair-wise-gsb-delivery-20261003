package federation

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

var testNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// testEntity is a local federation entity that can sign statements.
type testEntity struct {
	id  string
	key *ecdsa.PrivateKey
}

func newEntity(id string) (*testEntity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &testEntity{id: id, key: key}, nil
}

func mustEntity(t *testing.T, id string) *testEntity {
	t.Helper()
	e, err := newEntity(id)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return e
}

func (e *testEntity) publicJWKS() jose.JSONWebKeySet {
	return jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: e.key.Public()}}}
}

func (e *testEntity) jwksJSON() (map[string]any, error) {
	raw, err := json.Marshal(e.publicJWKS())
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// issue signs a statement about sub. Default validity is one hour around
// testNow; extra claims may override iat/exp.
func (e *testEntity) issue(sub string, extra map[string]any) (string, error) {
	claims := map[string]any{
		"iss": e.id,
		"sub": sub,
		"iat": testNow.Add(-time.Hour).Unix(),
		"exp": testNow.Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: e.key},
		(&jose.SignerOptions{}).WithType("entity-statement+jwt"),
	)
	if err != nil {
		return "", err
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return jws.CompactSerialize()
}

func (e *testEntity) mustIssue(t *testing.T, sub string, extra map[string]any) string {
	t.Helper()
	s, err := e.issue(sub, extra)
	if err != nil {
		t.Fatalf("signing statement: %v", err)
	}
	return s
}

// configure signs the entity's self-signed configuration.
func (e *testEntity) configure(extra map[string]any) (string, error) {
	jwks, err := e.jwksJSON()
	if err != nil {
		return "", err
	}
	claims := map[string]any{"jwks": jwks}
	for k, v := range extra {
		claims[k] = v
	}
	return e.issue(e.id, claims)
}

func (e *testEntity) mustConfigure(t *testing.T, extra map[string]any) string {
	t.Helper()
	s, err := e.configure(extra)
	if err != nil {
		t.Fatalf("signing configuration: %v", err)
	}
	return s
}

// fakeFetcher serves entity statements from memory. It stands in for the
// network so tests control the whole federation locally.
type fakeFetcher struct {
	configs map[string]string
	// statements maps fetch endpoint -> subordinate entity ID -> statement.
	statements map[string]map[string]string
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		configs:    map[string]string{},
		statements: map[string]map[string]string{},
	}
}

func (f *fakeFetcher) FetchEntityConfiguration(_ context.Context, entityID string) ([]byte, error) {
	if c, ok := f.configs[entityID]; ok {
		return []byte(c), nil
	}
	return nil, fmt.Errorf("no configuration for %s", entityID)
}

func (f *fakeFetcher) FetchSubordinateStatement(_ context.Context, fetchEndpoint, subordinate string) ([]byte, error) {
	if subs, ok := f.statements[fetchEndpoint]; ok {
		if s, ok := subs[subordinate]; ok {
			return []byte(s), nil
		}
	}
	return nil, fmt.Errorf("no statement about %s at %s", subordinate, fetchEndpoint)
}

func (f *fakeFetcher) publishConfig(entityID, jwt string) {
	f.configs[entityID] = jwt
}

func (f *fakeFetcher) publishStatement(fetchEndpoint, subordinate, jwt string) {
	if f.statements[fetchEndpoint] == nil {
		f.statements[fetchEndpoint] = map[string]string{}
	}
	f.statements[fetchEndpoint][subordinate] = jwt
}

const (
	taID = "https://ta.example.com"
	imID = "https://im.example.com"
	opID = "https://op.example.com"

	taEndpoint = "https://ta.example.com/fetch"
	imEndpoint = "https://im.example.com/fetch"
)

// buildThreeLevelFederation builds a local trust anchor -> intermediate ->
// OP federation. The OP's discovery URL is not a trust anchor; the chain is
// only anchored at ta.
func buildThreeLevelFederation() (*fakeFetcher, *testEntity, *testEntity, *testEntity, error) {
	ta, err := newEntity(taID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	im, err := newEntity(imID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	op, err := newEntity(opID)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	f := newFakeFetcher()
	taConfig, err := ta.configure(map[string]any{
		"metadata": map[string]any{
			"federation_entity": map[string]any{"federation_fetch_endpoint": taEndpoint},
		},
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	imConfig, err := im.configure(map[string]any{
		"authority_hints": []string{taID},
		"metadata": map[string]any{
			"federation_entity": map[string]any{"federation_fetch_endpoint": imEndpoint},
		},
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	opConfig, err := op.configure(map[string]any{
		"authority_hints": []string{imID},
		"metadata": map[string]any{
			"openid_provider": map[string]any{
				"issuer":                                opID,
				"scopes_supported":                      []string{"openid", "profile"},
				"subject_types_supported":               []string{"pairwise"},
				"response_types_supported":              []string{"code"},
				"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
			},
		},
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	f.publishConfig(taID, taConfig)
	f.publishConfig(imID, imConfig)
	f.publishConfig(opID, opConfig)

	// The trust anchor constrains scopes and fixes subject types.
	taStmt, err := ta.issue(imID, map[string]any{
		"metadata_policy": map[string]any{
			"openid_provider": map[string]any{
				"scopes_supported":        map[string]any{"subset_of": []string{"openid", "profile", "email"}},
				"subject_types_supported": map[string]any{"value": []string{"public"}},
			},
		},
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// The intermediate adds a scope and requires a token endpoint auth
	// method.
	imStmt, err := im.issue(opID, map[string]any{
		"metadata_policy": map[string]any{
			"openid_provider": map[string]any{
				"scopes_supported":                      map[string]any{"add": []string{"email"}},
				"token_endpoint_auth_methods_supported": map[string]any{"essential": true},
			},
		},
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	f.publishStatement(taEndpoint, imID, taStmt)
	f.publishStatement(imEndpoint, opID, imStmt)
	return f, ta, im, op, nil
}

func newThreeLevelFederation(t *testing.T) (*fakeFetcher, *testEntity, *testEntity, *testEntity) {
	t.Helper()
	f, ta, im, op, err := buildThreeLevelFederation()
	if err != nil {
		t.Fatalf("building federation: %v", err)
	}
	return f, ta, im, op
}

func testResolver(t *testing.T, f Fetcher, anchors ...*testEntity) *Resolver {
	t.Helper()
	r := &Resolver{
		Fetcher: f,
		Now:     func() time.Time { return testNow },
	}
	for _, a := range anchors {
		r.TrustAnchors = append(r.TrustAnchors, TrustAnchor{EntityID: a.id, JWKS: a.publicJWKS()})
	}
	return r
}

func TestResolveProvider(t *testing.T) {
	f, ta, _, _ := newThreeLevelFederation(t)
	r := testResolver(t, f, ta)

	got, err := r.ResolveProvider(context.Background(), opID)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}

	wantChain := []string{taID, imID, opID}
	if !reflect.DeepEqual(got.Chain, wantChain) {
		t.Errorf("Chain = %v, want %v", got.Chain, wantChain)
	}
	if got.TrustAnchor != taID {
		t.Errorf("TrustAnchor = %q, want %q", got.TrustAnchor, taID)
	}
	wantMetadata := map[string]any{
		"issuer":                                opID,
		"scopes_supported":                      []any{"openid", "profile", "email"},
		"subject_types_supported":               []any{"public"},
		"response_types_supported":              []any{"code"},
		"token_endpoint_auth_methods_supported": []any{"client_secret_basic"},
	}
	if !reflect.DeepEqual(got.Metadata, wantMetadata) {
		t.Errorf("Metadata = %#v, want %#v", got.Metadata, wantMetadata)
	}
}

func TestUnsupportedPolicyOperatorRejected(t *testing.T) {
	f, ta, im, _ := newThreeLevelFederation(t)
	// A statement carrying an operator outside the supported set must be
	// rejected, not silently ignored.
	f.publishStatement(imEndpoint, opID, im.mustIssue(t, opID, map[string]any{
		"metadata_policy": map[string]any{
			"openid_provider": map[string]any{
				"claims_supported": map[string]any{"regexp": []string{".*"}},
			},
		},
	}))

	_, err := testResolver(t, f, ta).ResolveProvider(context.Background(), opID)
	if err == nil || !strings.Contains(err.Error(), `unsupported metadata policy operator "regexp"`) {
		t.Fatalf("ResolveProvider error = %v, want unsupported operator rejection", err)
	}
}

func TestConflictingPoliciesRejected(t *testing.T) {
	f, ta, im, _ := newThreeLevelFederation(t)
	// The intermediate narrows scopes to a set disjoint from what the OP
	// advertises: combined with the anchor's subset_of, the OP's "profile"
	// scope is no longer permitted.
	f.publishStatement(imEndpoint, opID, im.mustIssue(t, opID, map[string]any{
		"metadata_policy": map[string]any{
			"openid_provider": map[string]any{
				"scopes_supported": map[string]any{"subset_of": []string{"openid", "email"}},
			},
		},
	}))

	_, err := testResolver(t, f, ta).ResolveProvider(context.Background(), opID)
	if err == nil || !strings.Contains(err.Error(), "is not a subset of policy") {
		t.Fatalf("ResolveProvider error = %v, want policy conflict rejection", err)
	}
}

func TestBrokenChainFallsBackToValidPath(t *testing.T) {
	f, ta, _, op := newThreeLevelFederation(t)

	// A second, broken intermediate: its statement about the OP is signed
	// by a rogue key that does not match its published jwks.
	brokenID := "https://im-broken.example.com"
	brokenEndpoint := brokenID + "/fetch"
	broken := mustEntity(t, brokenID)
	rogue := mustEntity(t, brokenID)
	f.publishConfig(brokenID, broken.mustConfigure(t, map[string]any{
		"authority_hints": []string{taID},
		"metadata": map[string]any{
			"federation_entity": map[string]any{"federation_fetch_endpoint": brokenEndpoint},
		},
	}))
	f.publishStatement(brokenEndpoint, opID, rogue.mustIssue(t, opID, nil))

	// The OP hints at the broken intermediate first.
	f.publishConfig(opID, op.mustConfigure(t, map[string]any{
		"authority_hints": []string{brokenID, imID},
		"metadata": map[string]any{
			"openid_provider": map[string]any{
				"issuer":                                opID,
				"scopes_supported":                      []string{"openid", "profile"},
				"subject_types_supported":               []string{"pairwise"},
				"response_types_supported":              []string{"code"},
				"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
			},
		},
	}))

	got, err := testResolver(t, f, ta).ResolveProvider(context.Background(), opID)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	wantChain := []string{taID, imID, opID}
	if !reflect.DeepEqual(got.Chain, wantChain) {
		t.Errorf("Chain = %v, want the valid path %v", got.Chain, wantChain)
	}
}

func TestBrokenChainFails(t *testing.T) {
	f, ta, im, _ := newThreeLevelFederation(t)
	// The only statement about the OP names a different subject.
	f.publishStatement(imEndpoint, opID, im.mustIssue(t, "https://other.example.com", nil))

	_, err := testResolver(t, f, ta).ResolveProvider(context.Background(), opID)
	if err == nil || !strings.Contains(err.Error(), "unexpected iss/sub") {
		t.Fatalf("ResolveProvider error = %v, want broken chain rejection", err)
	}
}

func TestExpiredStatementFails(t *testing.T) {
	f, ta, im, _ := newThreeLevelFederation(t)
	f.publishStatement(imEndpoint, opID, im.mustIssue(t, opID, map[string]any{
		"exp": testNow.Add(-time.Minute).Unix(),
	}))

	_, err := testResolver(t, f, ta).ResolveProvider(context.Background(), opID)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("ResolveProvider error = %v, want expiry rejection", err)
	}
}

func TestMaxPathLengthEnforced(t *testing.T) {
	f, ta, _, _ := newThreeLevelFederation(t)
	// The anchor tolerates no intermediates, but the chain has one.
	f.publishConfig(taID, ta.mustConfigure(t, map[string]any{
		"metadata": map[string]any{
			"federation_entity": map[string]any{"federation_fetch_endpoint": taEndpoint},
		},
		"constraints": map[string]any{"max_path_length": 0},
	}))

	_, err := testResolver(t, f, ta).ResolveProvider(context.Background(), opID)
	if err == nil || !strings.Contains(err.Error(), "at most 0 intermediates") {
		t.Fatalf("ResolveProvider error = %v, want max_path_length rejection", err)
	}
}

func TestAuthorityHintCycle(t *testing.T) {
	// op -> a -> b -> a: the hints loop and never reach an anchor.
	ta := mustEntity(t, taID)
	op := mustEntity(t, opID)
	a := mustEntity(t, "https://a.example.com")
	b := mustEntity(t, "https://b.example.com")

	f := newFakeFetcher()
	f.publishConfig(taID, ta.mustConfigure(t, nil))
	f.publishConfig(opID, op.mustConfigure(t, map[string]any{
		"authority_hints": []string{a.id},
	}))
	f.publishConfig(a.id, a.mustConfigure(t, map[string]any{
		"authority_hints": []string{b.id},
		"metadata": map[string]any{
			"federation_entity": map[string]any{"federation_fetch_endpoint": a.id + "/fetch"},
		},
	}))
	f.publishConfig(b.id, b.mustConfigure(t, map[string]any{
		"authority_hints": []string{a.id},
		"metadata": map[string]any{
			"federation_entity": map[string]any{"federation_fetch_endpoint": b.id + "/fetch"},
		},
	}))
	f.publishStatement(a.id+"/fetch", opID, a.mustIssue(t, opID, nil))
	f.publishStatement(b.id+"/fetch", a.id, b.mustIssue(t, a.id, nil))
	f.publishStatement(a.id+"/fetch", b.id, a.mustIssue(t, b.id, nil))

	_, err := testResolver(t, f, ta).ResolveProvider(context.Background(), opID)
	if err == nil || !strings.Contains(err.Error(), "no trust chain") {
		t.Fatalf("ResolveProvider error = %v, want no-trust-chain failure", err)
	}
}

func ExampleResolver_ResolveProvider() {
	f, ta, _, _, err := buildThreeLevelFederation()
	if err != nil {
		panic(err)
	}
	r := &Resolver{
		TrustAnchors: []TrustAnchor{{EntityID: ta.id, JWKS: ta.publicJWKS()}},
		Fetcher:      f,
		Now:          func() time.Time { return testNow },
	}
	resolved, err := r.ResolveProvider(context.Background(), opID)
	if err != nil {
		panic(err)
	}
	metadata, err := json.Marshal(resolved.Metadata)
	if err != nil {
		panic(err)
	}
	fmt.Println("issuer:", resolved.Issuer)
	fmt.Println("trust anchor:", resolved.TrustAnchor)
	fmt.Println("chain:", strings.Join(resolved.Chain, " -> "))
	fmt.Println("metadata:", string(metadata))
	// Output:
	// issuer: https://op.example.com
	// trust anchor: https://ta.example.com
	// chain: https://ta.example.com -> https://im.example.com -> https://op.example.com
	// metadata: {"issuer":"https://op.example.com","response_types_supported":["code"],"scopes_supported":["openid","profile","email"],"subject_types_supported":["public"],"token_endpoint_auth_methods_supported":["client_secret_basic"]}
}
