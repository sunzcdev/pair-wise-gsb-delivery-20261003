package dpop

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBindingFromClaims(t *testing.T) {
	raw := []byte(`{"iss":"https://as.example.com","cnf":{"jkt":"0ZcOC2Ok31"}}`)

	binding, err := BindingFromClaims(raw)
	if err != nil {
		t.Fatalf("extracting binding: %v", err)
	}
	if binding.JWKThumbprint != "0ZcOC2Ok31" {
		t.Errorf("jkt = %q", binding.JWKThumbprint)
	}
}

func TestBindingFromClaimsMissing(t *testing.T) {
	cases := map[string][]byte{
		"no cnf":    []byte(`{"iss":"https://as.example.com"}`),
		"empty jkt": []byte(`{"cnf":{}}`),
		"malformed": []byte(`{not json`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := BindingFromClaims(raw); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// sharedStore models an external, multi-instance store (for example Redis
// behind SET NX): every verifier instance points at the same backend, and
// Add must remain atomic across them.
type sharedStore struct {
	mu      sync.Mutex
	entries map[string]time.Time
	calls   int
}

func (s *sharedStore) Add(_ context.Context, jti string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if _, exists := s.entries[jti]; exists {
		return false, ErrReplay
	}
	s.entries[jti] = time.Now().Add(ttl)
	return true, nil
}

func TestMultipleVerifierInstancesShareStore(t *testing.T) {
	store := &sharedStore{entries: map[string]time.Time{}}

	verifiers := make([]*Verifier, 3)
	for i := range verifiers {
		v, err := NewVerifier(&Config{ReplayStore: store})
		if err != nil {
			t.Fatalf("creating verifier %d: %v", i, err)
		}
		verifiers[i] = v
	}

	clock := time.Now()
	kp := newKeyPair(t)
	in := defaultProofInput(kp)
	in.iat = clock
	raw := signProof(t, in)

	// Instance 0 accepts the proof.
	if _, err := verifiers[0].Verify(context.Background(), tokenParams(raw, kp)); err != nil {
		t.Fatalf("first instance: %v", err)
	}

	// Instances 1 and 2 reject the identical proof via the shared backend.
	for i := 1; i < len(verifiers); i++ {
		_, err := verifiers[i].Verify(context.Background(), tokenParams(raw, kp))
		if err == nil || !strings.Contains(err.Error(), "already been used") {
			t.Fatalf("instance %d accepted a replayed proof: %v", i, err)
		}
	}
}

func TestStoreErrorPropagates(t *testing.T) {
	v, err := NewVerifier(&Config{ReplayStore: failingStore{}})
	if err != nil {
		t.Fatalf("creating verifier: %v", err)
	}
	kp := newKeyPair(t)
	in := defaultProofInput(kp)
	raw := signProof(t, in)

	_, err = v.Verify(context.Background(), tokenParams(raw, kp))
	if err == nil || !errors.Is(err, errStoreDown) {
		t.Fatalf("expected store error to propagate, got %v", err)
	}
}

var errStoreDown = errors.New("dpop test: store is down")

type failingStore struct{}

func (failingStore) Add(context.Context, string, time.Duration) (bool, error) {
	return false, errStoreDown
}
