// This program demonstrates RFC 9449 DPoP proof verification on a resource
// server using the oidc/dpop package. It simulates an authorization server
// that issues a DPoP-bound access token, a resource server that verifies
// proofs, and a client that presents them. The same proof is then replayed,
// presented to a different path, presented with a different token, and a
// server nonce challenge is walked end to end.
//
// Run it with:
//
//	go run ./example/dpop
package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/dpop"
	jose "github.com/go-jose/go-jose/v4"
)

func main() {
	// One shared replay store stands in for a Redis/SQL backend shared by
	// every resource-server instance; check-and-set is atomic across them.
	store := dpop.NewMemoryStore()

	// The nonce secret would be shared configuration across instances.
	nonceSecret := make([]byte, 32)
	if _, err := rand.Read(nonceSecret); err != nil {
		log.Fatalf("nonce secret: %v", err)
	}
	nonceIssuer, err := dpop.NewNonceIssuer(nonceSecret, 10*time.Minute)
	if err != nil {
		log.Fatalf("nonce issuer: %v", err)
	}

	verifier, err := dpop.NewVerifier(&dpop.Config{
		ReplayStore: store,
		Nonce:       nonceIssuer,
		TimeWindow:  time.Minute,
	})
	if err != nil {
		log.Fatalf("verifier: %v", err)
	}

	// The client generates its DPoP key once and keeps it private.
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("client key: %v", err)
	}
	pubJWK := jose.JSONWebKey{Key: clientKey.Public()}
	rawTP, err := pubJWK.Thumbprint(crypto.SHA256)
	if err != nil {
		log.Fatalf("thumbprint: %v", err)
	}
	jkt := base64.RawURLEncoding.EncodeToString(rawTP)

	// Trusted token bindings. A real resource server populates this from a
	// signed JWT access token it verified, or from an introspection response
	// over an authenticated channel. It must never trust a cnf claim from an
	// unverified token.
	bindings := map[string]*dpop.TokenBinding{}

	// Simulated authorization server: mints a token bound to the client key.
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		tokenJKT := strings.TrimSpace(string(body))
		token := mintToken(tokenJKT)
		bindings[token] = &dpop.TokenBinding{JWKThumbprint: tokenJKT}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"token_type":"DPoP","expires_in":3600}`, token)
	}))
	defer as.Close()

	// Resource server.
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := parseDPoPAuthorization(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `DPoP realm="api"`)
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}
		binding, known := bindings[token]
		if !known {
			w.Header().Set("WWW-Authenticate", `DPoP realm="api", error="invalid_token"`)
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}

		proof, verr := verifier.VerifyRequest(r.Context(), r, token, binding)
		if verr != nil {
			var dpopErr *dpop.Error
			if errors.As(verr, &dpopErr) {
				dpopErr.Write(w, "api")
				return
			}
			http.Error(w, verr.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q,"jti":%q,"jkt":%q}`,
			r.URL.Path, proof.JTI, proof.KeyThumbprint)
	}))
	defer rs.Close()

	// Client obtains a token from the AS.
	resp, err := http.Post(as.URL+"/token", "text/plain", strings.NewReader(jkt))
	if err != nil {
		log.Fatalf("token request: %v", err)
	}
	var issued struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issued); err != nil {
		log.Fatalf("token response: %v", err)
	}
	resp.Body.Close()

	client := &demoClient{key: clientKey}
	resourceURL := rs.URL + "/accounts/123"

	fmt.Println("== 1. First, valid DPoP request ==")
	// The client transparently handles any DPoP-Nonce challenge, then holds
	// onto the accepted proof to demonstrate replay.
	var firstProof string
	firstProof, resp = client.sendWithNonce(resourceURL, issued.AccessToken)
	printResponse(resp)

	fmt.Println("== 2. Replay the exact same proof ==")
	resp = client.send(resourceURL, issued.AccessToken, firstProof, client.currentNonce())
	printResponse(resp)

	fmt.Println("== 3. Same proof against a different path ==")
	resp = client.send(rs.URL+"/accounts/999", issued.AccessToken, firstProof, client.currentNonce())
	printResponse(resp)

	fmt.Println("== 4. Same proof presented with a different access token ==")
	// The other token is bound to the same key, but the proof's "ath" was
	// computed from the first token, so it must be rejected.
	otherResp, err := http.Post(as.URL+"/token", "text/plain", strings.NewReader(jkt))
	if err != nil {
		log.Fatalf("second token request: %v", err)
	}
	var other struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(otherResp.Body).Decode(&other); err != nil {
		log.Fatalf("second token response: %v", err)
	}
	otherResp.Body.Close()
	resp = client.send(resourceURL, other.AccessToken, firstProof, client.currentNonce())
	printResponse(resp)

	fmt.Println("== 5. Server nonce challenge ==")
	nonceURL := rs.URL + "/admin"

	// Force an attempt without a nonce claim: the RS challenges with
	// DPoP-Nonce and HTTP 401 use_dpop_nonce.
	noNonceProof := client.proof(http.MethodGet, nonceURL, issued.AccessToken, "")
	resp = client.send(nonceURL, issued.AccessToken, noNonceProof, "")
	serverNonce := resp.Header.Get(dpop.NonceHeader)
	fmt.Print("initial  ")
	printResponse(resp)

	// The client mints a fresh proof that echoes the server nonce.
	nonceProof := client.proof(http.MethodGet, nonceURL, issued.AccessToken, serverNonce)
	resp = client.send(nonceURL, issued.AccessToken, nonceProof, serverNonce)
	fmt.Print("retry    ")
	printResponse(resp)

	// Replaying the nonce-bound proof is still rejected.
	resp = client.send(nonceURL, issued.AccessToken, nonceProof, serverNonce)
	fmt.Print("replay   ")
	printResponse(resp)
}

type demoClient struct {
	key   *ecdsa.PrivateKey
	jti   int
	nonce string
}

func (c *demoClient) currentNonce() string { return c.nonce }

// sendWithNonce performs a request and, on a use_dpop_nonce challenge,
// retries once with a fresh proof that carries the server-issued nonce. It
// returns the proof that the server ultimately accepted (or rejected).
func (c *demoClient) sendWithNonce(target, token string) (string, *http.Response) {
	proof := c.proof(http.MethodGet, target, token, c.nonce)
	resp := c.send(target, token, proof, c.nonce)
	if resp.StatusCode == http.StatusUnauthorized {
		if nonce := resp.Header.Get(dpop.NonceHeader); nonce != "" {
			resp.Body.Close()
			c.nonce = nonce
			proof = c.proof(http.MethodGet, target, token, c.nonce)
			resp = c.send(target, token, proof, c.nonce)
		}
	}
	return proof, resp
}

// proof builds and signs a DPoP proof JWT per RFC 9449 section 4.2.
func (c *demoClient) proof(method, target, token, nonce string) string {
	htu, err := url.Parse(target)
	if err != nil {
		log.Fatalf("parse target: %v", err)
	}
	htu.RawQuery = ""
	htu.Fragment = ""

	c.jti++
	claims := map[string]any{
		"jti": fmt.Sprintf("jti-%d", c.jti),
		"htm": method,
		"htu": htu.String(),
		"iat": float64(time.Now().UnixNano()) / float64(time.Second),
		"ath": dpop.AccessTokenHash(token),
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: c.key},
		&jose.SignerOptions{
			EmbedJWK:     true,
			ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: dpop.ProofType},
		},
	)
	if err != nil {
		log.Fatalf("signer: %v", err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		log.Fatalf("marshal claims: %v", err)
	}
	jws, err := signer.Sign(body)
	if err != nil {
		log.Fatalf("sign: %v", err)
	}
	compact, err := jws.CompactSerialize()
	if err != nil {
		log.Fatalf("serialize: %v", err)
	}
	return compact
}

func (c *demoClient) send(target, token, proof, nonce string) *http.Response {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		log.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "DPoP "+token)
	req.Header.Set(dpop.Header, proof)
	if nonce != "" {
		req.Header.Set(dpop.NonceHeader, nonce)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("round trip: %v", err)
	}
	return resp
}

func mintToken(jkt string) string {
	sum := crypto.SHA256.New()
	sum.Write([]byte("issued-access-token:"))
	sum.Write([]byte(jkt))
	sum.Write([]byte(fmt.Sprintf("-%d", time.Now().UnixNano())))
	return base64.RawURLEncoding.EncodeToString(sum.Sum(nil))
}

func parseDPoPAuthorization(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "DPoP") {
		return "", false
	}
	return token, true
}

func printResponse(resp *http.Response) {
	fmt.Printf("HTTP %d: %s\n", resp.StatusCode, readBody(resp))
	if challenge := resp.Header.Get("WWW-Authenticate"); challenge != "" {
		fmt.Printf("       WWW-Authenticate: %s\n", challenge)
	}
	if nonce := resp.Header.Get(dpop.NonceHeader); nonce != "" {
		fmt.Printf("       DPoP-Nonce: %s\n", nonce)
	}
	fmt.Println()
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	var out bytes.Buffer
	if err := json.Compact(&out, bytes.TrimSpace(b)); err != nil {
		return strings.TrimSpace(string(b))
	}
	return out.String()
}
