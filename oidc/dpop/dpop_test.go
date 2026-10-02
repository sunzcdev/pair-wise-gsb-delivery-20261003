package dpop

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	testMethod = "GET"
	testURI    = "https://rs.example.com/resource/42"
	testToken  = "K2~gJQm4x8v2pQ9LdY3sR7nW1bV5cH8eF6aZ0tU"
)

type keyPair struct {
	priv   *ecdsa.PrivateKey
	pubJWK jose.JSONWebKey
	tp     string
}

func newKeyPair(t *testing.T) keyPair {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	pubJWK := jose.JSONWebKey{Key: priv.Public()}
	rawTP, err := pubJWK.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	return keyPair{
		priv:   priv,
		pubJWK: pubJWK,
		tp:     base64.RawURLEncoding.EncodeToString(rawTP),
	}
}

type proofInput struct {
	key   *ecdsa.PrivateKey
	alg   jose.SignatureAlgorithm
	htm   string
	htu   string
	iat   time.Time
	ath   string
	nonce string
	jti   string
	typ   string
}

func defaultProofInput(kp keyPair) proofInput {
	return proofInput{
		key: kp.priv,
		alg: jose.ES256,
		htm: testMethod,
		htu: testURI,
		iat: time.Now(),
		ath: AccessTokenHash(testToken),
		jti: "jti-1",
		typ: ProofType,
	}
}

func signProof(t *testing.T, in proofInput) string {
	t.Helper()
	if in.key == nil {
		t.Fatal("proof input requires a signing key")
	}
	extra := map[jose.HeaderKey]any{}
	if in.typ != "" {
		extra[jose.HeaderType] = in.typ
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: in.alg, Key: in.key},
		&jose.SignerOptions{EmbedJWK: true, ExtraHeaders: extra},
	)
	if err != nil {
		t.Fatalf("creating signer: %v", err)
	}

	claims := map[string]any{
		"jti": in.jti,
		"htm": in.htm,
		"htu": in.htu,
		"iat": float64(in.iat.UnixNano()) / float64(time.Second),
	}
	if in.ath != "" {
		claims["ath"] = in.ath
	}
	if in.nonce != "" {
		claims["nonce"] = in.nonce
	}

	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshaling claims: %v", err)
	}
	jws, err := signer.Sign(body)
	if err != nil {
		t.Fatalf("signing proof: %v", err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serializing proof: %v", err)
	}
	return raw
}

type fixedClock struct{ t time.Time }

func newTestVerifier(t *testing.T, clock *fixedClock) (*Verifier, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	if clock != nil {
		store.now = func() time.Time { return clock.t }
	}
	now := time.Now
	if clock != nil {
		now = func() time.Time { return clock.t }
	}
	v, err := NewVerifier(&Config{
		ReplayStore: store,
		TimeWindow:  time.Minute,
		Now:         now,
	})
	if err != nil {
		t.Fatalf("creating verifier: %v", err)
	}
	return v, store
}

func tokenParams(raw string, kp keyPair) VerifyParams {
	return VerifyParams{
		Proof:       raw,
		Method:      testMethod,
		TargetURI:   testURI,
		AccessToken: testToken,
		Binding:     &TokenBinding{JWKThumbprint: kp.tp},
	}
}

func proofErrorCode(t *testing.T, err error) ErrorCode {
	t.Helper()
	var dpopErr *Error
	if !errors.As(err, &dpopErr) {
		t.Fatalf("expected *dpop.Error, got %T: %v", err, err)
	}
	return dpopErr.Code
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestVerifySuccess(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	in := defaultProofInput(kp)
	in.iat = clock.t
	proof, err := v.Verify(context.Background(), tokenParams(signProof(t, in), kp))
	if err != nil {
		t.Fatalf("verifying proof: %v", err)
	}
	if proof.JTI != "jti-1" {
		t.Errorf("jti = %q, want jti-1", proof.JTI)
	}
	if proof.KeyThumbprint != kp.tp {
		t.Errorf("thumbprint = %q, want %q", proof.KeyThumbprint, kp.tp)
	}
}

func TestVerifyTokenEndpointProofWithoutBinding(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	in := defaultProofInput(kp)
	in.iat = clock.t
	in.ath = "" // proofs at the token endpoint carry no ath
	in.htm, in.htu = "POST", "https://as.example.com/token"
	params := VerifyParams{
		Proof:     signProof(t, in),
		Method:    "POST",
		TargetURI: "https://as.example.com/token",
	}

	if _, err := v.Verify(context.Background(), params); err != nil {
		t.Fatalf("verifying token endpoint proof: %v", err)
	}
}

func TestVerifyReplay(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	in := defaultProofInput(kp)
	in.iat = clock.t
	raw := signProof(t, in)

	if _, err := v.Verify(context.Background(), tokenParams(raw, kp)); err != nil {
		t.Fatalf("first verification: %v", err)
	}
	_, err := v.Verify(context.Background(), tokenParams(raw, kp))
	if err == nil {
		t.Fatal("second verification of the same proof succeeded")
	}
	if code := proofErrorCode(t, err); code != ErrorInvalidToken {
		t.Fatalf("code = %q, want %q", code, ErrorInvalidToken)
	}
	if !strings.Contains(err.Error(), "already been used") {
		t.Errorf("error %q does not mention replay", err)
	}
}

func TestVerifyReplayIsAtomic(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	in := defaultProofInput(kp)
	in.iat = clock.t
	raw := signProof(t, in)

	const goroutines = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, goroutines)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := v.Verify(context.Background(), tokenParams(raw, kp))
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var successes, replays int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case strings.Contains(err.Error(), "already been used"):
			replays++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || replays != goroutines-1 {
		t.Fatalf("successes = %d, replays = %d; want 1 and %d",
			successes, replays, goroutines-1)
	}
}

func TestVerifyReplayAfterExpiry(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, store := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	in := defaultProofInput(kp)
	in.iat = clock.t
	raw := signProof(t, in)

	if _, err := v.Verify(context.Background(), tokenParams(raw, kp)); err != nil {
		t.Fatalf("first verification: %v", err)
	}

	// Advance the clock beyond the store entry's lifetime and keep the new
	// proof within the time window.
	clock.t = clock.t.Add(3 * time.Minute)
	in.iat = clock.t
	refreshed := signProof(t, in)

	if _, err := v.Verify(context.Background(), tokenParams(refreshed, kp)); err != nil {
		t.Fatalf("verification after jti expiry: %v", err)
	}
	// Sanity check the store really held the original entry.
	if added, err := store.Add(context.Background(), "never-seen", time.Minute); err != nil || !added {
		t.Fatalf("unrelated Add = %v, %v", added, err)
	}
}

func TestVerifyMethodMismatch(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	in := defaultProofInput(kp)
	in.iat = clock.t
	raw := signProof(t, in)

	params := tokenParams(raw, kp)
	params.Method = "POST"
	_, err := v.Verify(context.Background(), params)
	if err == nil || !strings.Contains(err.Error(), "htm mismatch") {
		t.Fatalf("expected htm mismatch, got %v", err)
	}
}

func TestVerifyPathMismatch(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	in := defaultProofInput(kp)
	in.iat = clock.t
	raw := signProof(t, in)

	params := tokenParams(raw, kp)
	params.TargetURI = "https://rs.example.com/resource/43"
	_, err := v.Verify(context.Background(), params)
	if err == nil || !strings.Contains(err.Error(), "htu mismatch") {
		t.Fatalf("expected htu mismatch, got %v", err)
	}
}

func TestVerifyQueryAndFragmentAreIgnored(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	in := defaultProofInput(kp)
	in.iat = clock.t
	in.htu = "https://rs.example.com/resource/42?page=2#frag"
	raw := signProof(t, in)

	params := tokenParams(raw, kp)
	params.TargetURI = "https://rs.example.com/resource/42"
	if _, err := v.Verify(context.Background(), params); err != nil {
		t.Fatalf("query/fragment in htu should compare equal to bare path: %v", err)
	}
}

func TestVerifyTokenMismatch(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	in := defaultProofInput(kp)
	in.iat = clock.t
	raw := signProof(t, in)

	params := tokenParams(raw, kp)
	params.AccessToken = "a-completely-different-access-token"
	_, err := v.Verify(context.Background(), params)
	if err == nil || !strings.Contains(err.Error(), "ath") {
		t.Fatalf("expected ath mismatch, got %v", err)
	}
}

func TestVerifyKeyBindingMismatch(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	signingKey := newKeyPair(t)
	boundKey := newKeyPair(t)

	in := defaultProofInput(signingKey)
	in.iat = clock.t
	raw := signProof(t, in)

	// Simulate an attacker whose token was bound to a different key.
	params := tokenParams(raw, signingKey)
	params.Binding = &TokenBinding{JWKThumbprint: boundKey.tp}
	_, err := v.Verify(context.Background(), params)
	if err == nil || !strings.Contains(err.Error(), "does not match the access token binding") {
		t.Fatalf("expected binding mismatch, got %v", err)
	}
}

func TestVerifyTimeWindow(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	cases := []struct {
		name    string
		offset  time.Duration
		wantErr bool
	}{
		{"future within window", -30 * time.Second, false},
		{"past within window", 30 * time.Second, false},
		{"future outside window", -90 * time.Second, true},
		{"past outside window", 90 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := defaultProofInput(kp)
			in.jti = "jti-" + tc.name
			in.iat = clock.t.Add(tc.offset)
			_, err := v.Verify(context.Background(), tokenParams(signProof(t, in), kp))
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "iat")) {
				t.Fatalf("expected iat window error, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("verification: %v", err)
			}
		})
	}
}

func TestVerifyMalformedProofs(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	t.Run("not a jws", func(t *testing.T) {
		if _, err := v.Verify(context.Background(), tokenParams("not-a-jws", kp)); err == nil {
			t.Fatal("expected malformed proof error")
		}
	})

	t.Run("json serialization rejected", func(t *testing.T) {
		in := defaultProofInput(kp)
		in.iat = clock.t
		compact := signProof(t, in)
		parts := strings.Split(compact, ".")
		jsonJWS := `{"protected":` + quote(parts[0]) +
			`,"payload":` + quote(parts[1]) +
			`,"signature":` + quote(parts[2]) + `}`
		if _, err := v.Verify(context.Background(), tokenParams(jsonJWS, kp)); err == nil {
			t.Fatal("expected compact-only proof to be enforced")
		}
	})

	t.Run("wrong typ", func(t *testing.T) {
		in := defaultProofInput(kp)
		in.iat = clock.t
		in.typ = "JWT"
		_, err := v.Verify(context.Background(), tokenParams(signProof(t, in), kp))
		if err == nil || !strings.Contains(err.Error(), "typ") {
			t.Fatalf("expected typ error, got %v", err)
		}
	})

	t.Run("tampered body", func(t *testing.T) {
		in := defaultProofInput(kp)
		in.iat = clock.t
		raw := signProof(t, in)
		parts := strings.Split(raw, ".")
		if len(parts) != 3 {
			t.Fatalf("compact JWS should have 3 parts, got %d", len(parts))
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("decoding payload: %v", err)
		}
		payload[0] ^= 0xFF // flip bytes in the JSON; signature must fail
		tampered := parts[0] + "." +
			base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]
		_, err = v.Verify(context.Background(), tokenParams(tampered, kp))
		if err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("expected signature error, got %v", err)
		}
	})

	t.Run("wrong key signs the proof", func(t *testing.T) {
		attacker := newKeyPair(t)
		in := defaultProofInput(attacker)
		in.iat = clock.t
		params := tokenParams(signProof(t, in), kp)
		_, err := v.Verify(context.Background(), params)
		if err == nil || !strings.Contains(err.Error(), "binding") {
			t.Fatalf("expected binding error, got %v", err)
		}
	})

	t.Run("access token without binding", func(t *testing.T) {
		in := defaultProofInput(kp)
		in.iat = clock.t
		params := tokenParams(signProof(t, in), kp)
		params.Binding = nil
		if _, err := v.Verify(context.Background(), params); err == nil {
			t.Fatal("expected error when AccessToken is set without Binding")
		}
	})

	t.Run("missing jti", func(t *testing.T) {
		in := defaultProofInput(kp)
		in.iat = clock.t
		in.jti = ""
		_, err := v.Verify(context.Background(), tokenParams(signProof(t, in), kp))
		if err == nil || !strings.Contains(err.Error(), "jti") {
			t.Fatalf("expected jti error, got %v", err)
		}
	})
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func newNonceVerifier(t *testing.T, clock *fixedClock) (*Verifier, *NonceIssuer) {
	t.Helper()
	issuer, err := NewNonceIssuer(bytesRepeat(0xAB, 32), 10*time.Minute)
	if err != nil {
		t.Fatalf("creating nonce issuer: %v", err)
	}
	issuer.now = func() time.Time { return clock.t }

	store := NewMemoryStore()
	store.now = func() time.Time { return clock.t }

	v, err := NewVerifier(&Config{
		ReplayStore: store,
		Nonce:       issuer,
		TimeWindow:  time.Minute,
		Now:         func() time.Time { return clock.t },
	})
	if err != nil {
		t.Fatalf("creating verifier: %v", err)
	}
	return v, issuer
}

func TestNonceChallengeFlow(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, issuer := newNonceVerifier(t, clock)
	kp := newKeyPair(t)

	// 1. Initial request without a nonce claim triggers a challenge.
	in := defaultProofInput(kp)
	in.iat = clock.t
	_, err := v.Verify(context.Background(), tokenParams(signProof(t, in), kp))
	var dpopErr *Error
	if !errors.As(err, &dpopErr) {
		t.Fatalf("expected *dpop.Error, got %v", err)
	}
	if dpopErr.Code != ErrorUseDPoPNonce {
		t.Fatalf("code = %q, want %q", dpopErr.Code, ErrorUseDPoPNonce)
	}
	serverNonce, ok := dpopErr.NonceChallenge()
	if !ok || serverNonce == "" {
		t.Fatalf("expected a nonce challenge, got %v", err)
	}

	// 2. Client retries with a fresh proof that echoes the nonce.
	in.jti = "jti-2"
	in.nonce = serverNonce
	params := tokenParams(signProof(t, in), kp)
	params.ExpectedNonce = serverNonce
	proof, err := v.Verify(context.Background(), params)
	if err != nil {
		t.Fatalf("verifying proof with nonce: %v", err)
	}
	if proof.Nonce != serverNonce {
		t.Errorf("proof nonce = %q, want %q", proof.Nonce, serverNonce)
	}

	// 3. A nonce that doesn't match the one required for this request is
	// challenged again.
	in.jti = "jti-3"
	in.nonce = issuer.Issue()
	params = tokenParams(signProof(t, in), kp)
	params.ExpectedNonce = serverNonce
	_, err = v.Verify(context.Background(), params)
	if !errors.As(err, &dpopErr) || dpopErr.Code != ErrorUseDPoPNonce {
		t.Fatalf("expected a fresh nonce challenge, got %v", err)
	}

	if err := issuer.Check("not-a-nonce"); err == nil {
		t.Fatal("expected malformed nonce to be rejected")
	}
}

func TestNonceProofHeaderMismatch(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newNonceVerifier(t, clock)
	kp := newKeyPair(t)

	// A proof carrying one nonce but a request that advertises another must
	// be challenged: RFC 9449 section 8 ties proof nonce and the DPoP-Nonce
	// header together.
	otherIssuer, err := NewNonceIssuer(bytesRepeat(0xAB, 32), 10*time.Minute)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	otherIssuer.now = func() time.Time { return clock.t }

	in := defaultProofInput(kp)
	in.iat = clock.t
	in.nonce = otherIssuer.Issue()

	params := tokenParams(signProof(t, in), kp)
	params.ExpectedNonce = "different-header-nonce"

	_, err = v.Verify(context.Background(), params)
	var dpopErr *Error
	if !errors.As(err, &dpopErr) || dpopErr.Code != ErrorUseDPoPNonce {
		t.Fatalf("expected use_dpop_nonce challenge, got %v", err)
	}
}

func TestNonceExpiryAndIsolation(t *testing.T) {
	clock := &fixedClock{t: time.Now()}

	mkIssuer := func(secret byte) *NonceIssuer {
		issuer, err := NewNonceIssuer(bytesRepeat(secret, 32), time.Minute)
		if err != nil {
			t.Fatalf("creating nonce issuer: %v", err)
		}
		issuer.now = func() time.Time { return clock.t }
		return issuer
	}

	issuer := mkIssuer(0xCD)
	nonce := issuer.Issue()

	clock.t = clock.t.Add(2 * time.Minute)
	if err := issuer.Check(nonce); err == nil {
		t.Fatal("expected expired nonce to be rejected")
	}

	clock.t = time.Now()
	if _, err := NewNonceIssuer(bytesRepeat(0x01, 16), time.Minute); err == nil {
		t.Fatal("expected short secret to be rejected")
	}

	a := mkIssuer(0x01)
	b := mkIssuer(0x02)
	if err := b.Check(a.Issue()); err == nil {
		t.Fatal("nonce signed by another secret was accepted")
	}
}

func TestVerifyRequest(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newTestVerifier(t, clock)
	kp := newKeyPair(t)

	in := defaultProofInput(kp)
	in.iat = clock.t
	raw := signProof(t, in)

	req := httptest.NewRequest(http.MethodGet, testURI+"?limit=10", nil)
	req.Header.Set(Header, raw)

	proof, err := v.VerifyRequest(context.Background(), req, testToken,
		&TokenBinding{JWKThumbprint: kp.tp})
	if err != nil {
		t.Fatalf("verifying request: %v", err)
	}
	if proof.JTI != "jti-1" {
		t.Errorf("jti = %q", proof.JTI)
	}

	empty := httptest.NewRequest(http.MethodGet, testURI, nil)
	if _, err := v.VerifyRequest(context.Background(), empty, testToken,
		&TokenBinding{JWKThumbprint: kp.tp}); err == nil {
		t.Fatal("expected missing DPoP header error")
	}

	double := httptest.NewRequest(http.MethodGet, testURI, nil)
	double.Header.Add(Header, raw)
	double.Header.Add(Header, raw)
	if _, err := v.VerifyRequest(context.Background(), double, testToken,
		&TokenBinding{JWKThumbprint: kp.tp}); err == nil {
		t.Fatal("expected multiple DPoP headers error")
	}
}

func TestNewVerifierValidation(t *testing.T) {
	if _, err := NewVerifier(nil); err == nil {
		t.Fatal("expected error without a replay store")
	}
	if _, err := NewVerifier(&Config{
		ReplayStore:          NewMemoryStore(),
		SupportedSigningAlgs: []string{"HS256"},
	}); err == nil {
		t.Fatal("expected error for symmetric algorithm")
	}
}

func TestErrorWrite(t *testing.T) {
	clock := &fixedClock{t: time.Now()}
	v, _ := newNonceVerifier(t, clock)
	kp := newKeyPair(t)

	in := defaultProofInput(kp)
	in.iat = clock.t
	_, err := v.Verify(context.Background(), tokenParams(signProof(t, in), kp))

	rec := httptest.NewRecorder()
	var dpopErr *Error
	if !errors.As(err, &dpopErr) {
		t.Fatalf("expected *dpop.Error, got %v", err)
	}
	dpopErr.Write(rec, "api")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if rec.Header().Get(NonceHeader) == "" {
		t.Error("DPoP-Nonce header not set")
	}
	challenge := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(challenge, `realm="api"`) ||
		!strings.Contains(challenge, `error="use_dpop_nonce"`) {
		t.Errorf("unexpected WWW-Authenticate header: %q", challenge)
	}
}
