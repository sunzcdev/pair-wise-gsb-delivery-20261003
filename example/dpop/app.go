// This example simulates a complete DPoP deployment (RFC 9449) inside one
// process:

//   - an authorization server that mints a DPoP-bound access token carrying
//     cnf.jkt,
//   - a resource server that independently verifies the access token and then
//     checks the DPoP proof with the dpop package, backed by a shared replay
//     store and a server-nonce challenge,
//   - a legitimate client that proves possession of its private key,
//   - an attacker that replays the same proof, changes the target path, and
//     swaps the access token.
//
// Run with: go run ./example/dpop
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/dpop"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	resourceURL = "https://rs.example.com/api/resource"
	method      = http.MethodPost
)

func main() {
	// --- Client: generate the DPoP key that never leaves the client. -------
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	jkt, err := dpop.PublicKeyThumbprint(&clientKey.PublicKey)
	if err != nil {
		log.Fatal(err)
	}

	// --- Authorization server: bind the token to the client's JWK. ---------
	asSigningKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	accessToken := issueAccessToken(asSigningKey, jkt)

	// --- Resource server: one verifier, but imagine several replicas all ---
	// sharing the same backend replay store.
	replays := &redisLikeStore{entries: map[string]time.Time{}}
	verifier := dpop.NewVerifier(&dpop.Config{
		ReplayStore: replays,
		Nonces:      dpop.NewNonceManager(10 * time.Minute),
	})

	rs := dpop.Middleware(&dpop.MiddlewareConfig{
		Verifier: verifier,
		Authenticate: func(r *http.Request) (*dpop.AccessTokenBinding, error) {
			// The resource server verifies the access token itself and only
			// then trusts its cnf claim. It never derives binding from the
			// proof header.
			token := bearerToken(r)
			binding, err := verifyAccessToken(asSigningKey, token)
			if err != nil {
				return nil, fmt.Errorf("access token rejected: %w", err)
			}
			return &dpop.AccessTokenBinding{Token: token, Binding: binding}, nil
		},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proof := dpop.ProofFromContext(r.Context())
		fmt.Fprintf(w, "protected resource served; proof jti=%s jkt=%s\n", proof.JTI, proof.Thumbprint)
	}))
	server := httptest.NewServer(rs)
	defer server.Close()

	sign := func(jti, nonce string) string {
		proof, err := dpop.SignProof(clientKey, dpop.ProofClaims{
			HTM:   method,
			HTU:   resourceURL,
			ATH:   dpop.AccessTokenHash(accessToken),
			IAT:   time.Now(),
			JTI:   jti,
			Nonce: nonce,
		})
		if err != nil {
			log.Fatal(err)
		}
		return proof
	}

	// 1) Legitimate request (before nonce enforcement, the first request gets
	// challenged; we fold that into the dedicated section below and seed the
	// nonce here).
	fmt.Println("== nonce challenge flow ==")
	jti1 := "proof-1"
	resp := call(server, accessToken, sign(jti1, ""))
	fmt.Printf("client sends proof without nonce -> %s\n", summarize(resp))
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get(dpop.NonceHeaderName) == "" {
		log.Fatalf("expected a nonce challenge, got %d", resp.StatusCode)
	}
	nonce := resp.Header.Get(dpop.NonceHeaderName)
	fmt.Printf("server challenges with DPoP-Nonce: %s...\n", nonce[:12])

	resp = call(server, accessToken, sign(jti1, nonce))
	fmt.Printf("client retries same jti with nonce -> %s: %s", summarize(resp), resp.BodyString())
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("nonce retry failed: %d %s", resp.StatusCode, resp.BodyString())
	}

	// 2) Replay the exact same proof a second time.
	fmt.Println("== replay of the same proof ==")
	resp = call(server, accessToken, sign(jti1, nonce))
	fmt.Printf("same proof replayed -> %s (%s)\n", summarize(resp), strings.TrimSpace(resp.BodyString()))

	// 3) Same key/token, but a proof for a different path.
	fmt.Println("== proof bound to a different path ==")
	otherPathProof, err := dpop.SignProof(clientKey, dpop.ProofClaims{
		HTM:   method,
		HTU:   "https://rs.example.com/api/admin",
		ATH:   dpop.AccessTokenHash(accessToken),
		IAT:   time.Now(),
		JTI:   "proof-other-path",
		Nonce: nonce,
	})
	if err != nil {
		log.Fatal(err)
	}
	resp = call(server, accessToken, otherPathProof)
	fmt.Printf("htu mismatch -> %s (%s)\n", summarize(resp), strings.TrimSpace(resp.BodyString()))

	// 4) A different access token presented with a valid proof for the real
	// token: the "ath" digests differ.
	fmt.Println("== access token swapped ==")
	attackerToken := issueAccessToken(asSigningKey, "some-other-thumbprint")
	resp = call(server, attackerToken, sign("proof-swapped-token", nonce))
	fmt.Printf("proof ath vs foreign token -> %s (%s)\n", summarize(resp), strings.TrimSpace(resp.BodyString()))

	// 5) A forged token signed by an unknown key is rejected before DPoP even
	// runs, and a token whose cnf.jkt does not match the proof is rejected.
	fmt.Println("== trust anchor checks ==")
	forgedKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	forgedToken := issueAccessToken(forgedKey, jkt)
	resp = call(server, forgedToken, sign("proof-forged-token", nonce))
	fmt.Printf("token with untrusted signature -> %s\n", summarize(resp))

	mismatchedToken := issueAccessToken(asSigningKey, "not-the-client-jkt")
	mismatchedProof, err := dpop.SignProof(clientKey, dpop.ProofClaims{
		HTM:   method,
		HTU:   resourceURL,
		ATH:   dpop.AccessTokenHash(mismatchedToken),
		IAT:   time.Now(),
		JTI:   "proof-wrong-binding",
		Nonce: nonce,
	})
	if err != nil {
		log.Fatal(err)
	}
	resp = call(server, mismatchedToken, mismatchedProof)
	fmt.Printf("verified token with foreign cnf.jkt -> %s (%s)\n", summarize(resp), strings.TrimSpace(resp.BodyString()))

	// 6) A fresh, legitimately bound proof is still accepted afterwards.
	resp = call(server, accessToken, sign("proof-2", nonce))
	fmt.Printf("fresh legitimate proof -> %s: %s", summarize(resp), resp.BodyString())

	fmt.Printf("\nreplay store retained %d jti(s)\n", replays.len())
}

// ---- authorization server side -------------------------------------------------

func issueAccessToken(signingKey *ecdsa.PrivateKey, jkt string) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: signingKey}, nil)
	if err != nil {
		log.Fatal(err)
	}
	claims := map[string]interface{}{
		"iss": "https://as.example.com",
		"sub": "client-123",
		"exp": jwt.NewNumericDate(time.Now().Add(time.Hour)),
		"cnf": map[string]string{"jkt": jkt},
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		log.Fatal(err)
	}
	return raw
}

// verifyAccessToken is what the resource server trusts: signature check with
// the authorization server's key, then extraction of cnf.
func verifyAccessToken(publicKey *ecdsa.PrivateKey, raw string) (dpop.Binding, error) {
	token, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return dpop.Binding{}, err
	}
	var claims map[string]any
	if err := token.Claims(&publicKey.PublicKey, &claims); err != nil {
		return dpop.Binding{}, err
	}
	if exp, ok := claims["exp"].(float64); ok && time.Now().Unix() > int64(exp) {
		return dpop.Binding{}, errors.New("token expired")
	}
	return dpop.BindingFromClaims(claims)
}

// ---- request plumbing ----------------------------------------------------------

type recordedResponse struct {
	StatusCode int
	Header     http.Header
	body       string
}

func (r recordedResponse) BodyString() string { return r.body }
func (r recordedResponse) String() string     { return http.StatusText(r.StatusCode) }
func summarize(r recordedResponse) string {
	return fmt.Sprintf("%d %s", r.StatusCode, http.StatusText(r.StatusCode))
}

func call(server *httptest.Server, token, proof string) recordedResponse {
	req, err := http.NewRequest(method, server.URL+"/api/resource", nil)
	if err != nil {
		log.Fatal(err)
	}
	req.Host = "rs.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Authorization", "DPoP "+token)
	req.Header.Set(dpop.HeaderName, proof)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return recordedResponse{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), body: string(buf[:n])}
}

func bearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "DPoP ")
}

// redisLikeStore is a toy stand-in for "SET dpop:jti:<jti> 1 NX PX <ttl>".
// Every resource-server replica would point at the same Redis cluster, so the
// check-and-set is atomic even across instances.
type redisLikeStore struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func (s *redisLikeStore) CheckAndStore(_ context.Context, jti string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, expiry := range s.entries {
		if !expiry.After(now) {
			delete(s.entries, id)
		}
	}
	if expiry, ok := s.entries[jti]; ok && expiry.After(now) {
		return true, nil
	}
	s.entries[jti] = now.Add(ttl)
	return false, nil
}

func (s *redisLikeStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
