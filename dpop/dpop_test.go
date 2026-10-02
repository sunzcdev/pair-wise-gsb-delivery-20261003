package dpop

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testMethod = "POST"
	testURL    = "https://rs.example.com/api/resource"
)

type testEnv struct {
	t        *testing.T
	verifier *Verifier
	key      *ecdsa.PrivateKey
	token    string
	binding  Binding
	now      time.Time
}

func newTestEnv(t *testing.T, opts ...func(*Config)) *testEnv {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating DPoP key: %v", err)
	}
	jkt, err := PublicKeyThumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("computing jkt: %v", err)
	}

	now := time.Now().UTC()
	token := "access-token-" + randomSuffix(t)

	config := &Config{Now: func() time.Time { return now }}
	for _, opt := range opts {
		opt(config)
	}

	return &testEnv{
		t:        t,
		verifier: NewVerifier(config),
		key:      key,
		token:    token,
		binding:  Binding{JKT: jkt},
		now:      now,
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return fmt.Sprintf("%x", buf)
}

func (e *testEnv) signProof(claims ProofClaims) string {
	e.t.Helper()
	if claims.IAT.IsZero() {
		claims.IAT = e.now
	}
	if claims.JTI == "" {
		claims.JTI = randomSuffix(e.t)
	}
	if claims.HTM == "" {
		claims.HTM = testMethod
	}
	if claims.HTU == "" {
		claims.HTU = testURL
	}
	if claims.ATH == "" && e.token != "" {
		claims.ATH = AccessTokenHash(e.token)
	}
	raw, err := SignProof(e.key, claims)
	if err != nil {
		e.t.Fatalf("signing proof: %v", err)
	}
	return raw
}

func (e *testEnv) verify(rawProof string) (*Proof, error) {
	return e.verifier.Verify(context.Background(), rawProof, Request{
		Method:    testMethod,
		TargetURI: testURL,
	}, e.token, e.binding)
}

func TestValidProof(t *testing.T) {
	env := newTestEnv(t)

	proof, err := env.verify(env.signProof(ProofClaims{}))
	if err != nil {
		t.Fatalf("expected proof to verify: %v", err)
	}
	if proof.Thumbprint != env.binding.JKT {
		t.Errorf("thumbprint = %q, want %q", proof.Thumbprint, env.binding.JKT)
	}
	if proof.JTI == "" {
		t.Error("jti should be returned")
	}
}

func TestReplay(t *testing.T) {
	env := newTestEnv(t)
	rawProof := env.signProof(ProofClaims{JTI: "fixed-jti"})

	if _, err := env.verify(rawProof); err != nil {
		t.Fatalf("first use should succeed: %v", err)
	}
	_, err := env.verify(rawProof)
	if !errors.Is(err, ErrReplayed) {
		t.Fatalf("second use should be ErrReplayed, got %v", err)
	}
}

// TestReplayConcurrency proves the atomicity contract of the store: of N
// concurrent presentations of one jti exactly one wins.
func TestReplayConcurrency(t *testing.T) {
	env := newTestEnv(t)
	rawProof := env.signProof(ProofClaims{JTI: "concurrent-jti"})

	const n = 32
	var wg sync.WaitGroup
	var winners int64
	var mu sync.Mutex
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := env.verify(rawProof)
			if err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			} else if !errors.Is(err, ErrReplayed) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Fatalf("expected exactly 1 accepted proof, got %d", winners)
	}
}

func TestHTMMismatch(t *testing.T) {
	env := newTestEnv(t)
	proof := env.signProof(ProofClaims{HTM: "GET"})

	_, err := env.verify(proof)
	if !errors.Is(err, ErrInvalidClaims) {
		t.Fatalf("expected ErrInvalidClaims for method change, got %v", err)
	}
}

func TestHTUMismatch(t *testing.T) {
	env := newTestEnv(t)

	// A proof minted for another path must not verify, even with the same
	// key and token digest.
	rawProof := env.signProof(ProofClaims{HTU: "https://rs.example.com/api/other"})
	_, err := env.verifier.Verify(context.Background(), rawProof, Request{
		Method:    testMethod,
		TargetURI: testURL,
	}, env.token, env.binding)
	if !errors.Is(err, ErrInvalidClaims) {
		t.Fatalf("expected ErrInvalidClaims for changed path, got %v", err)
	}
}

func TestHTUCanonicalization(t *testing.T) {
	env := newTestEnv(t)
	cases := map[string]string{
		"https://rs.example.com:443/api/resource":  testURL,
		"https://RS.Example.com/api/resource":      testURL,
		"https://rs.example.com/api/resource?x=1":  testURL,
		"https://rs.example.com/api/resource#frag": testURL,
		"https://rs.example.com/api/resource/":     "https://rs.example.com/api/resource/",
		"http://rs.example.com:80/api/resource":    "http://rs.example.com/api/resource",
	}
	for signed, target := range cases {
		rawProof := env.signProof(ProofClaims{HTU: signed, JTI: randomSuffix(t)})
		if _, err := env.verifier.Verify(context.Background(), rawProof, Request{
			Method:    testMethod,
			TargetURI: target,
		}, env.token, env.binding); err != nil {
			t.Errorf("htu canonicalization mismatch for %q vs %q: %v", signed, target, err)
		}
	}
}

func TestDifferentAccessToken(t *testing.T) {
	env := newTestEnv(t)

	// Replay the proof with a different access token: its digest no longer
	// matches the proof's "ath".
	otherToken := "a-different-access-token"
	_, err := env.verifier.Verify(context.Background(), env.signProof(ProofClaims{}), Request{
		Method:    testMethod,
		TargetURI: testURL,
	}, otherToken, env.binding)
	if !errors.Is(err, ErrTokenDigestMismatch) {
		t.Fatalf("expected ErrTokenDigestMismatch, got %v", err)
	}

	// Conversely, a proof minted for the other token but presented with the
	// original bound token also fails.
	otherProof := env.signProof(ProofClaims{ATH: AccessTokenHash(otherToken), JTI: randomSuffix(t)})
	if _, err := env.verify(otherProof); !errors.Is(err, ErrTokenDigestMismatch) {
		t.Fatalf("expected ErrTokenDigestMismatch for foreign ath, got %v", err)
	}
}

func TestKeyBindingMismatch(t *testing.T) {
	env := newTestEnv(t)

	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	otherJKT, err := PublicKeyThumbprint(&otherKey.PublicKey)
	if err != nil {
		t.Fatalf("jkt: %v", err)
	}

	// Proof signed by a different key than the token is bound to. The
	// signature is valid over the proof, but cnf.jkt does not match.
	raw, err := SignProof(otherKey, ProofClaims{
		HTM: testMethod,
		HTU: testURL,
		ATH: AccessTokenHash(env.token),
		IAT: env.now,
		JTI: randomSuffix(t),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, err = env.verify(raw)
	if !errors.Is(err, ErrKeyBindingMismatch) {
		t.Fatalf("expected ErrKeyBindingMismatch, got %v", err)
	}

	// Sanity check the two thumbprints really differ.
	if otherJKT == env.binding.JKT {
		t.Fatal("distinct keys produced identical thumbprints")
	}
}

func TestTamperedSignature(t *testing.T) {
	env := newTestEnv(t)

	raw := env.signProof(ProofClaims{})
	// Flip a character in the signature segment.
	parts := strings.Split(raw, ".")
	if parts[2][0] == 'A' {
		parts[2] = "B" + parts[2][1:]
	} else {
		parts[2] = "A" + parts[2][1:]
	}
	if _, err := env.verify(strings.Join(parts, ".")); !errors.Is(err, ErrInvalidSignature) &&
		!errors.Is(err, ErrMalformedProof) {
		t.Fatalf("expected signature/parse error, got %v", err)
	}
}

func TestIATWindow(t *testing.T) {
	env := newTestEnv(t)

	old := env.signProof(ProofClaims{IAT: env.now.Add(-10 * time.Minute), JTI: randomSuffix(t)})
	if _, err := env.verify(old); !errors.Is(err, ErrProofExpired) {
		t.Fatalf("expected ErrProofExpired for old proof, got %v", err)
	}

	future := env.signProof(ProofClaims{IAT: env.now.Add(5 * time.Minute), JTI: randomSuffix(t)})
	if _, err := env.verify(future); !errors.Is(err, ErrProofExpired) {
		t.Fatalf("expected ErrProofExpired for future proof, got %v", err)
	}

	withinLeeway := env.signProof(ProofClaims{IAT: env.now.Add(30 * time.Second), JTI: randomSuffix(t)})
	if _, err := env.verify(withinLeeway); err != nil {
		t.Fatalf("proof within leeway should verify: %v", err)
	}
}

func TestBindingMustBeVerified(t *testing.T) {
	env := newTestEnv(t)

	// No cnf.jkt supplied by the caller: the verifier refuses rather than
	// trusting the JWK embedded in the proof.
	if _, err := env.verifier.Verify(context.Background(), env.signProof(ProofClaims{}), Request{
		Method:    testMethod,
		TargetURI: testURL,
	}, env.token, Binding{}); err == nil {
		t.Fatal("verification without verified binding should fail")
	}
}

func TestRSASupported(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	jkt, err := PublicKeyThumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("jkt: %v", err)
	}

	token := "rsa-token"
	verifier := NewVerifier(&Config{Now: func() time.Time { return time.Now() }})
	raw, err := SignProof(key, ProofClaims{
		HTM: testMethod,
		HTU: testURL,
		ATH: AccessTokenHash(token),
		IAT: time.Now(),
		JTI: randomSuffix(t),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), raw, Request{
		Method:    testMethod,
		TargetURI: testURL,
	}, token, Binding{JKT: jkt}); err != nil {
		t.Fatalf("RSA proof should verify: %v", err)
	}
}
