package dpop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNonceChallengeFlow(t *testing.T) {
	env := newTestEnv(t, func(c *Config) {
		c.Nonces = NewNonceManager(time.Minute)
	})

	// First attempt without a nonce is rejected with a fresh challenge.
	rawProof := env.signProof(ProofClaims{JTI: "nonce-flow-jti"})
	_, err := env.verify(rawProof)
	var challenge *NonceError
	if !errors.As(err, &challenge) || !errors.Is(err, ErrNonceRequired) {
		t.Fatalf("expected *NonceError/ErrNonceRequired, got %v", err)
	}
	if challenge.Nonce == "" {
		t.Fatal("challenge must carry a nonce")
	}

	// The challenge must not have consumed the jti: the client retries the
	// same request with the same proof, only adding the nonce. Here the
	// proof is re-signed carrying the nonce but keeps the same jti.
	retryProof := env.signProof(ProofClaims{JTI: "nonce-flow-jti", Nonce: challenge.Nonce})
	proof, err := env.verify(retryProof)
	if err != nil {
		t.Fatalf("retry with nonce should succeed: %v", err)
	}
	if proof.Nonce != challenge.Nonce {
		t.Errorf("nonce = %q, want %q", proof.Nonce, challenge.Nonce)
	}

	// A nonce may be reused by distinct proofs until it expires; each proof's
	// jti is still single use.
	again := env.signProof(ProofClaims{JTI: "nonce-flow-jti-2", Nonce: challenge.Nonce})
	if _, err := env.verify(again); err != nil {
		t.Fatalf("a different proof reusing a valid nonce should verify: %v", err)
	}

	// An unknown nonce is rejected with a fresh challenge.
	bogus := env.signProof(ProofClaims{JTI: "nonce-flow-jti-3", Nonce: "not-a-real-nonce"})
	_, err = env.verify(bogus)
	var second *NonceError
	if !errors.As(err, &second) || !errors.Is(err, ErrNonceInvalid) {
		t.Fatalf("expected ErrNonceInvalid for bogus nonce, got %v", err)
	}
	if second.Nonce == "" {
		t.Error("a fresh nonce should be issued on the challenge")
	}
}

func TestNonceExpiry(t *testing.T) {
	now := time.Now()
	manager := NewNonceManager(time.Minute)
	manager.now = func() time.Time { return now }

	nonce := manager.NewNonce()
	manager.now = func() time.Time { return now.Add(2 * time.Minute) }
	if manager.Verify(nonce) {
		t.Fatal("expired nonce should be rejected")
	}
}

func TestBindingFromClaims(t *testing.T) {
	payload := `{"iss":"https://as.example.com","cnf":{"jkt":"abc123"}}`
	var claims map[string]any
	if err := json.Unmarshal([]byte(payload), &claims); err != nil {
		t.Fatal(err)
	}
	binding, err := BindingFromClaims(claims)
	if err != nil {
		t.Fatalf("BindingFromClaims: %v", err)
	}
	if binding.JKT != "abc123" || !binding.IsDPoP() {
		t.Fatalf("unexpected binding: %+v", binding)
	}

	// Token without cnf: empty binding, no error (a plain bearer token).
	var bearer map[string]any
	if err := json.Unmarshal([]byte(`{"iss":"https://as.example.com"}`), &bearer); err != nil {
		t.Fatal(err)
	}
	b, err := BindingFromClaims(bearer)
	if err != nil || b.IsDPoP() {
		t.Fatalf("expected empty binding, got %+v %v", b, err)
	}
}

// sharedStore is a stand-in for a Redis-backed store shared by multiple
// service instances: each instance has its own Verifier but all use one store.
type sharedStore struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func (s *sharedStore) CheckAndStore(_ context.Context, jti string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if expiresAt, ok := s.entries[jti]; ok && expiresAt.After(now) {
		return true, nil
	}
	s.entries[jti] = now.Add(ttl)
	return false, nil
}

func TestReplayAcrossInstances(t *testing.T) {
	store := &sharedStore{entries: map[string]time.Time{}}

	// Two "instances" share only the replay store.
	v1 := NewVerifier(&Config{ReplayStore: store})
	v2 := NewVerifier(&Config{ReplayStore: store})

	env := newTestEnv(t)
	rawProof := env.signProof(ProofClaims{JTI: "shared-jti"})

	if _, err := v1.Verify(context.Background(), rawProof, Request{
		Method:    testMethod,
		TargetURI: testURL,
	}, env.token, env.binding); err != nil {
		t.Fatalf("instance 1 should accept: %v", err)
	}
	if _, err := v2.Verify(context.Background(), rawProof, Request{
		Method:    testMethod,
		TargetURI: testURL,
	}, env.token, env.binding); !errors.Is(err, ErrReplayed) {
		t.Fatalf("instance 2 should reject replay, got %v", err)
	}
}

func TestMemoryStoreExpiry(t *testing.T) {
	now := time.Now()
	store := NewMemoryReplayStore()
	store.now = func() time.Time { return now }

	seen, err := store.CheckAndStore(context.Background(), "j", time.Minute)
	if seen || err != nil {
		t.Fatalf("first store: seen=%v err=%v", seen, err)
	}
	seen, _ = store.CheckAndStore(context.Background(), "j", time.Minute)
	if !seen {
		t.Fatal("second store within ttl should report seen")
	}

	store.now = func() time.Time { return now.Add(2 * time.Minute) }
	seen, _ = store.CheckAndStore(context.Background(), "j", time.Minute)
	if seen {
		t.Fatal("expired jti should be stored again")
	}
}

func TestMiddleware(t *testing.T) {
	env := newTestEnv(t, func(c *Config) {
		c.Nonces = NewNonceManager(time.Minute)
	})

	called := false
	handler := Middleware(&MiddlewareConfig{
		Verifier: env.verifier,
		Authenticate: func(r *http.Request) (*AccessTokenBinding, error) {
			// In a real resource server this verifies the access token's
			// signature/issuer/expiry and extracts cnf.jkt from it.
			return &AccessTokenBinding{Token: env.token, Binding: env.binding}, nil
		},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if ProofFromContext(r.Context()) == nil {
			t.Error("verified proof missing from context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	newRequest := func(proof string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, testURL, nil)
		r.Header.Set("Authorization", "DPoP "+env.token)
		if proof != "" {
			r.Header.Set(HeaderName, proof)
		}
		return r
	}

	// Missing proof header.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newRequest(""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing proof: code=%d", rec.Code)
	}

	// Valid proof without nonce: challenge with DPoP-Nonce header.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, newRequest(env.signProof(ProofClaims{JTI: "mw-jti"})))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get(NonceHeaderName) == "" {
		t.Fatalf("expected nonce challenge, code=%d nonce=%q", rec.Code, rec.Header().Get(NonceHeaderName))
	}
	if called {
		t.Fatal("handler must not run on challenge")
	}

	// Retry including the nonce succeeds.
	nonce := rec.Header().Get(NonceHeaderName)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, newRequest(env.signProof(ProofClaims{JTI: "mw-jti", Nonce: nonce})))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("retry should succeed, code=%d body=%q", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatal("handler should run after successful verification")
	}

	// Replay of the same proof is a 400.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, newRequest(env.signProof(ProofClaims{JTI: "mw-jti", Nonce: nonce})))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("replay code=%d, want 400", rec.Code)
	}

	// Different path: 401 invalid claims.
	wrongPath := httptest.NewRequest(http.MethodPost, "https://rs.example.com/api/elsewhere", nil)
	wrongPath.Header.Set("Authorization", "DPoP "+env.token)
	wrongPath.Header.Set(HeaderName, env.signProof(ProofClaims{JTI: "path-jti", Nonce: nonce}))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, wrongPath)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid proof claims") {
		t.Fatalf("changed path code=%d body=%q", rec.Code, rec.Body.String())
	}
}
