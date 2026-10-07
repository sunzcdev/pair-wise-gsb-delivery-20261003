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
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
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
)

// jwsUse is one recorded observation of which public key authenticated
// a single JWS request received by the fake CA.
type jwsUse struct {
	endpoint   string
	keyLabel   string // JWK thumbprint of the outer-signature key
	innerLabel string // JWK thumbprint of the inner-JWS key (keyChange only)
}

// fakeCA is a minimal ACME server that verifies every JWS it receives and
// records the public key (by JWK thumbprint) used on each request. It
// implements just enough of RFC 8555 (directory, newNonce, newAccount,
// keyChange, newOrder and account POST-as-GET) to exercise coordinated
// key rollovers. It doubles as an http.RoundTripper so tests can serve
// requests in process and selectively drop responses.
type fakeCA struct {
	t *testing.T

	mu       sync.Mutex
	nonces   map[string]bool
	accounts map[string]crypto.PublicKey // account path -> current key
	nonceN   int

	uses       []jwsUse
	keyChanges int // keyChange requests actually processed

	srv *httptest.Server
}

func newFakeCA(t *testing.T) *fakeCA {
	ca := &fakeCA{
		t:        t,
		nonces:   make(map[string]bool),
		accounts: make(map[string]crypto.PublicKey),
	}
	ca.srv = httptest.NewServer(http.HandlerFunc(ca.handle))
	t.Cleanup(ca.srv.Close)
	return ca
}

func (ca *fakeCA) directoryURL() string { return ca.srv.URL + "/directory" }

func (ca *fakeCA) client(transport http.RoundTripper) *Client {
	if transport == nil {
		transport = ca
	}
	return &Client{
		Directory:  ca.directoryURL(),
		HTTPClient: &http.Client{Transport: transport},
	}
}

// RoundTrip implements http.RoundTripper by serving in process.
func (ca *fakeCA) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	ca.handle(rec, req)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func (ca *fakeCA) handle(w http.ResponseWriter, r *http.Request) {
	// §6.5: a fresh nonce on every POST response (including errors) keeps
	// the client's anti-replay pool healthy for the next request.
	if r.Method == http.MethodPost {
		w.Header().Set(replayNonce, ca.issueNonce())
	}
	switch {
	case r.URL.Path == "/directory":
		writeJSON(w, http.StatusOK, Directory{
			NewNonce:   ca.srv.URL + "/new-nonce",
			NewAccount: ca.srv.URL + "/new-account",
			NewOrder:   ca.srv.URL + "/new-order",
			KeyChange:  ca.srv.URL + "/key-change",
		})
	case r.URL.Path == "/new-nonce":
		w.Header().Set(replayNonce, ca.issueNonce())
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/new-account":
		ca.handleNewAccount(w, r)
	case r.URL.Path == "/key-change":
		ca.handleKeyChange(w, r)
	case r.URL.Path == "/new-order":
		ca.handleNewOrder(w, r)
	case strings.HasPrefix(r.URL.Path, "/account/"):
		ca.handleAccount(w, r)
	default:
		writeProblem(w, http.StatusNotFound, ProblemTypeMalformed, "unknown endpoint: "+r.URL.Path)
	}
}

func (ca *fakeCA) issueNonce() string {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.nonceN++
	nonce := fmt.Sprintf("nonce-%d", ca.nonceN)
	ca.nonces[nonce] = true
	return nonce
}

func (ca *fakeCA) consumeNonce(nonce string) bool {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if !ca.nonces[nonce] {
		return false
	}
	delete(ca.nonces, nonce)
	return true
}

func (ca *fakeCA) handleNewAccount(w http.ResponseWriter, r *http.Request) {
	_, outer, ok := ca.verifyOuter(w, r)
	if !ok {
		return
	}

	var req struct {
		OnlyReturnExisting bool `json:"onlyReturnExisting"`
	}
	if err := json.Unmarshal(outer.payload, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, err.Error())
		return
	}

	path := "/account/" + thumb(ca.t, outer.headerJWK)
	kid := ca.srv.URL + path

	ca.mu.Lock()
	_, exists := ca.accounts[path]
	if !exists && req.OnlyReturnExisting {
		ca.mu.Unlock()
		writeProblem(w, http.StatusBadRequest, ProblemTypeAccountDoesNotExist, "unknown account")
		return
	}
	ca.accounts[path] = outer.headerJWK
	ca.mu.Unlock()

	w.Header().Set("Location", kid)
	writeJSON(w, http.StatusCreated, Account{Status: StatusValid, Location: kid})
}

func (ca *fakeCA) handleAccount(w http.ResponseWriter, r *http.Request) {
	_, outer, ok := ca.verifyOuter(w, r)
	if !ok {
		return
	}
	kid := outer.protected.KID
	if kid == "" {
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "account request requires a kid")
		return
	}

	ca.mu.Lock()
	current, exists := ca.accounts[r.URL.Path]
	ca.mu.Unlock()

	if !exists || kid != ca.srv.URL+r.URL.Path {
		writeProblem(w, http.StatusUnauthorized, ProblemTypeUnauthorized, "unknown account")
		return
	}
	if !publicKeyEqual(current, outer.signKey) {
		writeProblem(w, http.StatusUnauthorized, ProblemTypeUnauthorized, "key does not match account key")
		return
	}
	writeJSON(w, http.StatusOK, Account{Status: StatusValid, Location: kid})
}

func (ca *fakeCA) handleKeyChange(w http.ResponseWriter, r *http.Request) {
	_, outer, ok := ca.verifyOuter(w, r)
	if !ok {
		return
	}

	inner, ok := verifyInnerJWS(w, outer.payload, r.URL.String())
	if !ok {
		return
	}

	var changeReq keyChangeRequest
	if err := json.Unmarshal(inner.payload, &changeReq); err != nil {
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, err.Error())
		return
	}

	oldKey, err := parseJWK(changeReq.OldKey)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, err.Error())
		return
	}

	path := strings.TrimPrefix(changeReq.Account, ca.srv.URL)
	newPub := inner.headerJWK

	ca.mu.Lock()
	defer ca.mu.Unlock()

	current, exists := ca.accounts[path]
	switch {
	case !exists:
		writeProblem(w, http.StatusUnauthorized, ProblemTypeAccountDoesNotExist, "unknown account")
	case outer.protected.KID != changeReq.Account:
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "outer kid does not match account")
	case !publicKeyEqual(current, outer.signKey):
		writeProblem(w, http.StatusUnauthorized, ProblemTypeUnauthorized, "outer key is not the current account key")
	case !publicKeyEqual(oldKey, current):
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "oldKey does not match current account key")
	case publicKeyEqual(newPub, current):
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "new key already in use")
	default:
		ca.accounts[path] = newPub
		ca.keyChanges++
		ca.uses[len(ca.uses)-1].innerLabel = thumb(ca.t, newPub)
		writeJSON(w, http.StatusOK, Account{Status: StatusValid, Location: changeReq.Account})
	}
}

func (ca *fakeCA) handleNewOrder(w http.ResponseWriter, r *http.Request) {
	_, _, ok := ca.verifyOuter(w, r)
	if !ok {
		return
	}
	orderURL := ca.srv.URL + fmt.Sprintf("/order/%d", len(ca.uses))
	w.Header().Set("Location", orderURL)
	writeJSON(w, http.StatusCreated, Order{
		Status:   StatusPending,
		Finalize: ca.srv.URL + "/finalize/1",
		Location: orderURL,
	})
}

// verifiedJWS is a flattened JWS whose signature checked out.
type verifiedJWS struct {
	protected jwsProtected
	headerJWK crypto.PublicKey // non-nil for jwk-authenticated requests
	signKey   crypto.PublicKey // the key the server authenticated against
	payload   []byte
}

type jwsProtected struct {
	Alg   string `json:"alg"`
	KID   string `json:"kid"`
	Nonce string `json:"nonce"`
	URL   string `json:"url"`
}

// verifyOuter validates the outer request JWS: nonce consumption, URL
// binding, signer resolution (jwk header or the account behind kid) and
// the signature. It records the signer key before returning.
func (ca *fakeCA) verifyOuter(w http.ResponseWriter, r *http.Request) ([]byte, verifiedJWS, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, err.Error())
		return nil, verifiedJWS{}, false
	}
	parsed, err := parseFlatJWS(raw)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, err.Error())
		return nil, verifiedJWS{}, false
	}

	if parsed.protected.URL != r.URL.String() {
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "jws url mismatch")
		return nil, verifiedJWS{}, false
	}
	if !ca.consumeNonce(parsed.protected.Nonce) {
		w.Header().Set(replayNonce, ca.issueNonce())
		writeProblem(w, http.StatusBadRequest, ProblemTypeBadNonce, "bad nonce")
		return nil, verifiedJWS{}, false
	}

	signKey := parsed.headerJWK
	if signKey == nil {
		kid := parsed.protected.KID
		if kid == "" {
			writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "jws must have kid or jwk")
			return nil, verifiedJWS{}, false
		}
		ca.mu.Lock()
		current, exists := ca.accounts[strings.TrimPrefix(kid, ca.srv.URL)]
		ca.mu.Unlock()
		if !exists {
			writeProblem(w, http.StatusUnauthorized, ProblemTypeAccountDoesNotExist, "unknown account")
			return nil, verifiedJWS{}, false
		}
		signKey = current
	}

	if !verifyJWSSignature(parsed, signKey) {
		writeProblem(w, http.StatusUnauthorized, ProblemTypeUnauthorized, "invalid signature")
		return nil, verifiedJWS{}, false
	}

	payload, err := base64.RawURLEncoding.DecodeString(parsed.payloadB64)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, err.Error())
		return nil, verifiedJWS{}, false
	}

	verified := verifiedJWS{
		protected: parsed.protected,
		headerJWK: parsed.headerJWK,
		signKey:   signKey,
		payload:   payload,
	}

	ca.mu.Lock()
	ca.uses = append(ca.uses, jwsUse{endpoint: r.URL.Path, keyLabel: thumb(ca.t, signKey)})
	ca.mu.Unlock()

	return payload, verified, true
}

// flatJWS is an unverified flattened JWS.
type flatJWS struct {
	protected    jwsProtected
	headerJWK    crypto.PublicKey
	payloadB64   string
	signature    []byte
	signingInput string
}

func parseFlatJWS(raw []byte) (*flatJWS, error) {
	var wire struct {
		Protected string          `json:"protected"`
		Payload   string          `json:"payload"`
		Signature string          `json:"signature"`
		Header    json.RawMessage `json:"header"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}

	headBytes, err := base64.RawURLEncoding.DecodeString(wire.Protected)
	if err != nil {
		return nil, err
	}
	var p flatJWS
	if err := json.Unmarshal(headBytes, &p.protected); err != nil {
		return nil, err
	}

	// The JWK may be in the protected header (this client always puts it
	// there) or in a separate unprotected header.
	var jwkRaw json.RawMessage
	var headAll struct {
		JWK json.RawMessage `json:"jwk"`
	}
	_ = json.Unmarshal(headBytes, &headAll)
	jwkRaw = headAll.JWK
	if len(jwkRaw) == 0 && len(wire.Header) > 0 {
		var unprot struct {
			JWK json.RawMessage `json:"jwk"`
		}
		_ = json.Unmarshal(wire.Header, &unprot)
		jwkRaw = unprot.JWK
	}
	if len(jwkRaw) > 0 {
		jwkPub, err := parseJWK(jwkRaw)
		if err != nil {
			return nil, err
		}
		p.headerJWK = jwkPub
	}

	p.payloadB64 = wire.Payload
	p.signature, err = base64.RawURLEncoding.DecodeString(wire.Signature)
	if err != nil {
		return nil, err
	}
	p.signingInput = wire.Protected + "." + wire.Payload
	return &p, nil
}

// verifyInnerJWS validates the inner JWS of a keyChange: jwk present, no
// kid/nonce, matching URL, good signature.
func verifyInnerJWS(w http.ResponseWriter, raw []byte, keyChangeURL string) (verifiedJWS, bool) {
	parsed, err := parseFlatJWS(raw)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, err.Error())
		return verifiedJWS{}, false
	}
	switch {
	case parsed.headerJWK == nil:
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "inner jws must carry a jwk")
		return verifiedJWS{}, false
	case parsed.protected.KID != "" || parsed.protected.Nonce != "":
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "inner jws must not carry kid or nonce")
		return verifiedJWS{}, false
	case parsed.protected.URL != keyChangeURL:
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, "inner jws url mismatch")
		return verifiedJWS{}, false
	case !verifyJWSSignature(parsed, parsed.headerJWK):
		writeProblem(w, http.StatusUnauthorized, ProblemTypeUnauthorized, "invalid inner signature")
		return verifiedJWS{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parsed.payloadB64)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, ProblemTypeMalformed, err.Error())
		return verifiedJWS{}, false
	}
	return verifiedJWS{
		protected: parsed.protected,
		headerJWK: parsed.headerJWK,
		signKey:   parsed.headerJWK,
		payload:   payload,
	}, true
}

// parseJWK parses a minimal RSA or EC public JWK.
func parseJWK(raw json.RawMessage) (crypto.PublicKey, error) {
	var jwk struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
		N   string `json:"n"`
		E   string `json:"e"`
	}
	if err := json.Unmarshal(raw, &jwk); err != nil {
		return nil, err
	}
	dec := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

	switch jwk.Kty {
	case "EC":
		var curve elliptic.Curve
		switch jwk.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported EC curve %q", jwk.Crv)
		}
		xBytes, err := dec(jwk.X)
		if err != nil {
			return nil, err
		}
		yBytes, err := dec(jwk.Y)
		if err != nil {
			return nil, err
		}
		return &ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(xBytes),
			Y:     new(big.Int).SetBytes(yBytes),
		}, nil
	case "RSA":
		nBytes, err := dec(jwk.N)
		if err != nil {
			return nil, err
		}
		eBytes, err := dec(jwk.E)
		if err != nil {
			return nil, err
		}
		eInt := new(big.Int).SetBytes(eBytes)
		if !eInt.IsInt64() || eInt.Int64() > int64(^uint32(0)) {
			return nil, errors.New("unsupported RSA exponent")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(eInt.Int64())}, nil
	default:
		return nil, fmt.Errorf("unsupported JWK kty %q", jwk.Kty)
	}
}

func verifyJWSSignature(p *flatJWS, key crypto.PublicKey) bool {
	sum := sha256.Sum256([]byte(p.signingInput))
	switch key := key.(type) {
	case *ecdsa.PublicKey:
		size := key.Params().BitSize / 8
		if key.Params().BitSize%8 > 0 {
			size++
		}
		if len(p.signature) != size*2 {
			return false
		}
		r := new(big.Int).SetBytes(p.signature[:size])
		s := new(big.Int).SetBytes(p.signature[size:])
		return ecdsa.Verify(key, sum[:], r, s)
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], p.signature) == nil
	}
	return false
}

func thumb(t *testing.T, pub crypto.PublicKey) string {
	t.Helper()
	label, err := jwkThumbprint(pub)
	if err != nil {
		t.Fatalf("computing JWK thumbprint: %v", err)
	}
	return label
}

func publicKeyEqual(a, b crypto.PublicKey) bool {
	ta, _ := jwkThumbprint(a)
	tb, _ := jwkThumbprint(b)
	return ta != "" && ta == tb
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func writeProblem(w http.ResponseWriter, status int, typ, detail string) {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(Problem{Type: typ, Status: status, Detail: detail})
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func newTestECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
