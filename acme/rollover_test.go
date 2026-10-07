// Copyright 2026 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCA is a minimal in-memory ACME server for key rollover tests. It
// verifies the JWS of every POST it receives and records the thumbprint
// of the public key that signed each one, so tests can assert exactly
// which key was used for which request, and in which order.
type fakeCA struct {
	mu       sync.Mutex
	accounts map[string]*fakeCAAccount   // by account URL
	keys     map[string]*ecdsa.PublicKey // every key ever registered, by thumbprint
	nonces   map[string]bool
	requests []fakeCARequest
	nextID   int
}

type fakeCAAccount struct {
	url string
	key *ecdsa.PublicKey // the account's current key
}

type fakeCARequest struct {
	path string
	key  string // JWK thumbprint of the key that signed the JWS ("" if unverifiable)
}

func newFakeCA(t *testing.T) (*fakeCA, *httptest.Server) {
	t.Helper()
	ca := &fakeCA{
		accounts: make(map[string]*fakeCAAccount),
		keys:     make(map[string]*ecdsa.PublicKey),
		nonces:   make(map[string]bool),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /directory", ca.handleDirectory)
	mux.HandleFunc("HEAD /new-nonce", ca.handleNewNonce)
	mux.HandleFunc("POST /new-account", ca.handleNewAccount)
	mux.HandleFunc("POST /new-order", ca.handleNewOrder)
	mux.HandleFunc("POST /key-change", ca.handleKeyChange)
	mux.HandleFunc("POST /acct/", ca.handleAccount)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return ca, srv
}

func (ca *fakeCA) handleDirectory(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	writeJSON(w, http.StatusOK, map[string]any{
		"newNonce":   base + "/new-nonce",
		"newAccount": base + "/new-account",
		"newOrder":   base + "/new-order",
		"revokeCert": base + "/revoke-cert",
		"keyChange":  base + "/key-change",
	})
}

func (ca *fakeCA) handleNewNonce(w http.ResponseWriter, r *http.Request) {
	ca.issueNonce(w)
	w.WriteHeader(http.StatusOK)
}

func (ca *fakeCA) handleNewAccount(w http.ResponseWriter, r *http.Request) {
	ca.issueNonce(w)
	j := ca.parseAndCheckNonce(w, r)
	if j == nil {
		return
	}
	if j.jwk == nil {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "newAccount requires a jwk")
		return
	}
	if !j.verify(j.jwk) {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "invalid JWS signature")
		return
	}
	thumb, err := jwkThumbprint(j.jwk)
	if err != nil {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeBadPublicKey, "unsupported key")
		return
	}
	ca.mu.Lock()
	ca.keys[thumb] = j.jwk
	ca.nextID++
	acctURL := fmt.Sprintf("http://%s/acct/%d", r.Host, ca.nextID)
	ca.accounts[acctURL] = &fakeCAAccount{url: acctURL, key: j.jwk}
	ca.requests = append(ca.requests, fakeCARequest{path: r.URL.Path, key: thumb})
	ca.mu.Unlock()
	w.Header().Set("Location", acctURL)
	writeJSON(w, http.StatusCreated, map[string]any{"status": StatusValid})
}

func (ca *fakeCA) handleNewOrder(w http.ResponseWriter, r *http.Request) {
	ca.issueNonce(w)
	_, acct := ca.authorize(w, r)
	if acct == nil {
		return
	}
	ca.mu.Lock()
	ca.nextID++
	orderURL := fmt.Sprintf("http://%s/order/%d", r.Host, ca.nextID)
	ca.mu.Unlock()
	w.Header().Set("Location", orderURL)
	writeJSON(w, http.StatusCreated, map[string]any{
		"status":   StatusPending,
		"finalize": orderURL + "/finalize",
	})
}

// handleAccount serves POST-as-GET requests to the account URL. This is
// what the rollover verification probes use: the request only succeeds
// if it is signed with the account's current key.
func (ca *fakeCA) handleAccount(w http.ResponseWriter, r *http.Request) {
	ca.issueNonce(w)
	_, acct := ca.authorize(w, r)
	if acct == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": StatusValid})
}

func (ca *fakeCA) handleKeyChange(w http.ResponseWriter, r *http.Request) {
	ca.issueNonce(w)
	outer, acct := ca.authorize(w, r)
	if acct == nil {
		return
	}
	// the payload of the outer JWS is the inner JWS, signed by the new key
	inner, err := parseJWS(outer.payloadJSON)
	if err != nil || inner.jwk == nil {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "keyChange payload must be a JWS with a jwk")
		return
	}
	if inner.header.Nonce != "" {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "inner JWS must not have a nonce")
		return
	}
	if !inner.verify(inner.jwk) {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "invalid inner JWS signature")
		return
	}
	var keyChangeReq struct {
		Account string          `json:"account"`
		OldKey  json.RawMessage `json:"oldKey"`
	}
	if err := json.Unmarshal(inner.payloadJSON, &keyChangeReq); err != nil {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "invalid keyChange payload")
		return
	}
	if keyChangeReq.Account != acct.url {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "account URL mismatch")
		return
	}
	oldKey, err := decodeECJWK(keyChangeReq.OldKey)
	if err != nil {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "invalid oldKey")
		return
	}
	oldThumb, err := jwkThumbprint(oldKey)
	if err != nil {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "invalid oldKey")
		return
	}
	acctThumb, err := jwkThumbprint(acct.key)
	if err != nil {
		ca.writeProblem(w, http.StatusInternalServerError, ProblemTypeServerInternal, "broken account key")
		return
	}
	if oldThumb != acctThumb {
		ca.writeProblem(w, http.StatusConflict, ProblemTypeMalformed, "oldKey does not match the account's key")
		return
	}
	newThumb, err := jwkThumbprint(inner.jwk)
	if err != nil {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeBadPublicKey, "unsupported new key")
		return
	}
	ca.mu.Lock()
	acct.key = inner.jwk
	ca.keys[newThumb] = inner.jwk
	ca.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// parseAndCheckNonce reads and parses the JWS of r and consumes its
// nonce. On failure it writes the appropriate problem and returns nil.
func (ca *fakeCA) parseAndCheckNonce(w http.ResponseWriter, r *http.Request) *fakeJWS {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "reading request body")
		return nil
	}
	j, err := parseJWS(body)
	if err != nil {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "parsing JWS")
		return nil
	}
	ca.mu.Lock()
	valid := ca.nonces[j.header.Nonce]
	delete(ca.nonces, j.header.Nonce)
	ca.mu.Unlock()
	if !valid {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeBadNonce, "unknown or reused nonce")
		return nil
	}
	return j
}

// authorize verifies the JWS of an account-bound request: it consumes
// the nonce, records which known key signed the request, and checks
// that the signing key is the account's current key. On failure it
// writes the appropriate problem and returns nil.
func (ca *fakeCA) authorize(w http.ResponseWriter, r *http.Request) (*fakeJWS, *fakeCAAccount) {
	j := ca.parseAndCheckNonce(w, r)
	if j == nil {
		return nil, nil
	}
	ca.mu.Lock()
	acct := ca.accounts[j.header.KID]
	// record which known key signed this request, regardless of whether
	// it is the key the account currently uses
	thumb := ""
	for candidateThumb, candidate := range ca.keys {
		if j.verify(candidate) {
			thumb = candidateThumb
			break
		}
	}
	ca.requests = append(ca.requests, fakeCARequest{path: r.URL.Path, key: thumb})
	ca.mu.Unlock()
	if acct == nil {
		ca.writeProblem(w, http.StatusBadRequest, ProblemTypeAccountDoesNotExist, "unknown account")
		return nil, nil
	}
	acctThumb, err := jwkThumbprint(acct.key)
	if err != nil || thumb != acctThumb {
		ca.writeProblem(w, http.StatusUnauthorized, ProblemTypeUnauthorized, "JWS not signed with the account's key")
		return nil, nil
	}
	return j, acct
}

func (ca *fakeCA) issueNonce(w http.ResponseWriter) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.nextID++
	nonce := fmt.Sprintf("nonce-%d", ca.nextID)
	ca.nonces[nonce] = true
	w.Header().Set(replayNonce, nonce)
}

func (ca *fakeCA) writeProblem(w http.ResponseWriter, status int, problemType, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":%q,"detail":%q,"status":%d}`, problemType, detail, status)
}

func (ca *fakeCA) requestLog() []fakeCARequest {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return append([]fakeCARequest(nil), ca.requests...)
}

func (ca *fakeCA) countRequests(path string) int {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	var n int
	for _, req := range ca.requests {
		if req.path == path {
			n++
		}
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// fakeJWS is a parsed JWS as received by the fake CA.
type fakeJWS struct {
	protected   string
	payload     string
	signature   string
	payloadJSON []byte
	jwk         *ecdsa.PublicKey // the key embedded in the header, if any
	header      struct {
		Alg   string          `json:"alg"`
		KID   string          `json:"kid"`
		JWK   json.RawMessage `json:"jwk"`
		Nonce string          `json:"nonce"`
		URL   string          `json:"url"`
	}
}

func parseJWS(body []byte) (*fakeJWS, error) {
	var raw struct {
		Protected string `json:"protected"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	j := &fakeJWS{protected: raw.Protected, payload: raw.Payload, signature: raw.Signature}
	headBytes, err := base64.RawURLEncoding.DecodeString(raw.Protected)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(headBytes, &j.header); err != nil {
		return nil, err
	}
	j.payloadJSON, err = base64.RawURLEncoding.DecodeString(raw.Payload)
	if err != nil {
		return nil, err
	}
	if len(j.header.JWK) > 0 {
		pub, err := decodeECJWK(j.header.JWK)
		if err != nil {
			return nil, err
		}
		j.jwk = pub
	}
	return j, nil
}

// verify reports whether the JWS signature is valid for pub (ES256 only).
func (j *fakeJWS) verify(pub *ecdsa.PublicKey) bool {
	sig, err := base64.RawURLEncoding.DecodeString(j.signature)
	if err != nil || len(sig) == 0 || len(sig)%2 != 0 {
		return false
	}
	r := new(big.Int).SetBytes(sig[:len(sig)/2])
	s := new(big.Int).SetBytes(sig[len(sig)/2:])
	digest := sha256.Sum256([]byte(j.protected + "." + j.payload))
	return ecdsa.Verify(pub, digest[:], r, s)
}

func decodeECJWK(raw json.RawMessage) (*ecdsa.PublicKey, error) {
	var jwk struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}
	if err := json.Unmarshal(raw, &jwk); err != nil {
		return nil, err
	}
	if jwk.Kty != "EC" || jwk.Crv != "P-256" {
		return nil, fmt.Errorf("fake CA only supports P-256 keys, got %s/%s", jwk.Kty, jwk.Crv)
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		return nil, err
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil {
		return nil, err
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}, nil
}

// keyChangeTransport simulates network failures around the keyChange
// request and the verification probes, on top of a real RoundTripper.
type keyChangeTransport struct {
	base http.RoundTripper

	mu                    sync.Mutex
	dropKeyChangeResponse bool // the server processes keyChange, but the client never sees the response
	failKeyChange         bool // keyChange never reaches the server
	failAccountProbes     bool // POST-as-GET requests to the account URL fail
}

func (t *keyChangeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	drop, fail, failProbes := t.dropKeyChangeResponse, t.failKeyChange, t.failAccountProbes
	t.mu.Unlock()

	if strings.HasSuffix(req.URL.Path, "/key-change") {
		if fail {
			return nil, fmt.Errorf("connection reset before request was sent")
		}
		if drop {
			resp, err := t.base.RoundTrip(req)
			if err != nil {
				return nil, err
			}
			resp.Body.Close() //nolint:errcheck
			return nil, fmt.Errorf("response lost in transit")
		}
	}
	if failProbes && strings.Contains(req.URL.Path, "/acct/") {
		return nil, fmt.Errorf("connection refused")
	}
	return t.base.RoundTrip(req)
}

func (t *keyChangeTransport) setFailAccountProbes(fail bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failAccountProbes = fail
}

func generateP256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func thumbprint(t *testing.T, key crypto.Signer) string {
	t.Helper()
	thumb, err := jwkThumbprint(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	return thumb
}

// newRolloverTestSetup starts a fake CA and registers an account.
func newRolloverTestSetup(t *testing.T, transport http.RoundTripper) (*fakeCA, *Client, Account) {
	t.Helper()
	ca, srv := newFakeCA(t)
	client := &Client{Directory: srv.URL + "/directory"}
	if transport != nil {
		client.HTTPClient = &http.Client{Transport: transport}
	}
	account, err := client.NewAccount(context.Background(), Account{
		Contact:    []string{"mailto:rollover-test@example.com"},
		PrivateKey: generateP256(t),
	})
	if err != nil {
		t.Fatalf("creating account: %v", err)
	}
	return ca, client, account
}

// issueOrder makes one new-order request for the account, signed with key.
func issueOrder(client *Client, account Account, key crypto.Signer) error {
	acct := account
	acct.PrivateKey = key
	_, err := client.NewOrder(context.Background(), acct, Order{
		Identifiers: []Identifier{{Type: "dns", Value: "example.com"}},
	})
	return err
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within deadline: %s", what)
}

// While issuance goroutines hammer the CA with orders, a rollover must
// draw a clean boundary: the CA observes old-key orders, then exactly
// one keyChange, then only new-key orders.
func TestKeyRolloverConcurrentIssuance(t *testing.T) {
	ca, client, account := newRolloverTestSetup(t, nil)
	oldKey := account.PrivateKey
	oldThumb := thumbprint(t, oldKey)
	kr := NewKeyRollover(oldKey)

	ctx := context.Background()
	const workers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				key, release, err := kr.AcquireKey(ctx)
				if err != nil {
					t.Errorf("AcquireKey: %v", err)
					return
				}
				err = issueOrder(client, account, key)
				release()
				if err != nil {
					t.Errorf("issuing order: %v", err)
					return
				}
			}
		}()
	}

	// make sure some old-key orders are in flight before rolling
	waitFor(t, "old-key orders", func() bool { return ca.countRequests("/new-order") >= 3 })

	newKey := generateP256(t)
	newThumb := thumbprint(t, newKey)
	if err := kr.Rollover(ctx, client, account, newKey); err != nil {
		t.Fatalf("Rollover: %v", err)
	}

	// make sure some new-key orders happen after the rollover
	for i := 0; i < 3; i++ {
		key, release, err := kr.AcquireKey(ctx)
		if err != nil {
			t.Fatalf("AcquireKey after rollover: %v", err)
		}
		if key != newKey {
			t.Errorf("AcquireKey after rollover returned the wrong key")
		}
		err = issueOrder(client, account, key)
		release()
		if err != nil {
			t.Fatalf("issuing order after rollover: %v", err)
		}
	}

	close(stop)
	wg.Wait()

	// the CA must have observed: old-key orders, then exactly one
	// keyChange, then only new-key orders
	var keyChanges int
	var sawOldOrder, sawNewOrder bool
	for _, req := range ca.requestLog() {
		switch req.path {
		case "/key-change":
			keyChanges++
		case "/new-order":
			if keyChanges == 0 {
				if req.key != oldThumb {
					t.Errorf("order before rollover signed with unexpected key %q", req.key)
				}
				sawOldOrder = true
			} else {
				if req.key != newThumb {
					t.Errorf("order after keyChange signed with old key %q", req.key)
				}
				sawNewOrder = true
			}
		}
	}
	if keyChanges != 1 {
		t.Errorf("CA saw %d keyChange requests, want exactly 1", keyChanges)
	}
	if !sawOldOrder || !sawNewOrder {
		t.Errorf("rollover did not happen mid-stream (old orders: %v, new orders: %v)", sawOldOrder, sawNewOrder)
	}
}

// If the CA commits the keyChange but the response is lost, the
// rollover must recover through verification probes instead of
// retrying the keyChange.
func TestKeyRolloverResponseLost(t *testing.T) {
	transport := &keyChangeTransport{base: http.DefaultTransport, dropKeyChangeResponse: true}
	ca, client, account := newRolloverTestSetup(t, transport)
	oldKey := account.PrivateKey
	oldThumb := thumbprint(t, oldKey)
	kr := NewKeyRollover(oldKey)

	ctx := context.Background()
	newKey := generateP256(t)
	newThumb := thumbprint(t, newKey)

	if err := kr.Rollover(ctx, client, account, newKey); err != nil {
		t.Fatalf("Rollover should recover from a lost response via verification, got: %v", err)
	}

	if n := ca.countRequests("/key-change"); n != 1 {
		t.Errorf("CA saw %d keyChange requests, want exactly 1 (no automatic retries)", n)
	}
	if key, certain := kr.CurrentKey(); !certain || key != newKey {
		t.Errorf("CurrentKey = %v, %v; want the new key, certain", key, certain)
	}

	// the verification probe must have asked the CA about the account
	// with the new key, after the keyChange
	var keyChangeSeen, probeSeen bool
	for _, req := range ca.requestLog() {
		if req.path == "/key-change" {
			if req.key != oldThumb {
				t.Errorf("keyChange signed with %q, want old key %q", req.key, oldThumb)
			}
			keyChangeSeen = true
		}
		if keyChangeSeen && strings.HasPrefix(req.path, "/acct/") && req.key == newThumb {
			probeSeen = true
		}
	}
	if !probeSeen {
		t.Error("no verification probe with the new key observed after the keyChange")
	}

	// the CA definitively rejects the old key now
	if definitive, accepted, err := probeAccountKey(ctx, client, account, oldKey); !definitive || accepted || err != nil {
		t.Errorf("old key probe = %v, %v, %v; want definitive rejection", definitive, accepted, err)
	}

	// and issuance continues with the new key
	key, release, err := kr.AcquireKey(ctx)
	if err != nil {
		t.Fatalf("AcquireKey: %v", err)
	}
	if err := issueOrder(client, account, key); err != nil {
		release()
		t.Fatalf("issuing order with new key: %v", err)
	}
	release()
	if got := ca.requestLog()[len(ca.requestLog())-1].key; got != newThumb {
		t.Errorf("last order signed with %q, want new key %q", got, newThumb)
	}
}

// If the keyChange never reaches the CA, verification must establish
// that the old key is still current, and issuance continues with it.
func TestKeyRolloverNeverReachedCA(t *testing.T) {
	transport := &keyChangeTransport{base: http.DefaultTransport, failKeyChange: true}
	ca, client, account := newRolloverTestSetup(t, transport)
	oldKey := account.PrivateKey
	oldThumb := thumbprint(t, oldKey)
	kr := NewKeyRollover(oldKey)

	ctx := context.Background()
	err := kr.Rollover(ctx, client, account, generateP256(t))
	if err == nil {
		t.Fatal("Rollover should fail when the keyChange cannot be sent")
	}
	if errors.Is(err, ErrKeyRolloverUncertain) {
		t.Errorf("error = %v; verification should have determined the rollover did not commit", err)
	}

	if n := ca.countRequests("/key-change"); n != 0 {
		t.Errorf("CA saw %d keyChange requests, want 0", n)
	}
	if key, certain := kr.CurrentKey(); !certain || key != oldKey {
		t.Errorf("CurrentKey = %v, %v; want the old key, certain", key, certain)
	}

	// issuance continues with the old key
	key, release, err := kr.AcquireKey(ctx)
	if err != nil {
		t.Fatalf("AcquireKey: %v", err)
	}
	if err := issueOrder(client, account, key); err != nil {
		release()
		t.Fatalf("issuing order with old key: %v", err)
	}
	release()
	if got := ca.requestLog()[len(ca.requestLog())-1].key; got != oldThumb {
		t.Errorf("last order signed with %q, want old key %q", got, oldThumb)
	}
}

// If the keyChange's outcome is ambiguous and the CA cannot be queried,
// the rollover must report an explicit uncertain state, refuse to roll
// again, and stay resolvable once the CA is reachable.
func TestKeyRolloverUncertainUntilResolved(t *testing.T) {
	transport := &keyChangeTransport{base: http.DefaultTransport, dropKeyChangeResponse: true, failAccountProbes: true}
	ca, client, account := newRolloverTestSetup(t, transport)
	kr := NewKeyRollover(account.PrivateKey)

	ctx := context.Background()
	newKey := generateP256(t)

	err := kr.Rollover(ctx, client, account, newKey)
	if !errors.Is(err, ErrKeyRolloverUncertain) {
		t.Fatalf("Rollover error = %v, want ErrKeyRolloverUncertain", err)
	}

	// while uncertain, no key is handed out
	if _, _, err := kr.AcquireKey(ctx); !errors.Is(err, ErrKeyRolloverUncertain) {
		t.Errorf("AcquireKey error = %v, want ErrKeyRolloverUncertain", err)
	}

	// and the coordinator refuses to roll again instead of stacking
	// another keyChange on top of the uncertain one
	if err := kr.Rollover(ctx, client, account, generateP256(t)); !errors.Is(err, ErrKeyRolloverUncertain) {
		t.Errorf("second Rollover error = %v, want ErrKeyRolloverUncertain", err)
	}
	if n := ca.countRequests("/key-change"); n != 1 {
		t.Errorf("CA saw %d keyChange requests, want exactly 1 (no automatic retries)", n)
	}

	// once the CA is reachable, the operator resolves the state
	transport.setFailAccountProbes(false)
	resolved, err := kr.Resolve(ctx, client, account)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved != newKey {
		t.Errorf("Resolve returned the wrong key; want the new key the CA committed")
	}
	if key, certain := kr.CurrentKey(); !certain || key != newKey {
		t.Errorf("CurrentKey = %v, %v; want the new key, certain", key, certain)
	}

	// issuance resumes with the resolved key
	key, release, err := kr.AcquireKey(ctx)
	if err != nil {
		t.Fatalf("AcquireKey after Resolve: %v", err)
	}
	if err := issueOrder(client, account, key); err != nil {
		release()
		t.Fatalf("issuing order after Resolve: %v", err)
	}
	release()
}
