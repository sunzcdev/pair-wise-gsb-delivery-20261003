package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

type jarmTest struct {
	name string

	// If not provided defaults to "https://foo".
	issuer string

	// JWT payload (just the claims).
	claims string

	// Key to sign the response with. Defaults to a generated RSA key.
	signKey *signingKey
	// If not provided defaults to signKey. Only useful when testing invalid
	// signatures.
	verificationKey *signingKey

	config JARMConfig
	// Expected state passed to Verify. Defaults to "some-state".
	state string

	wantErr       bool
	wantErrExpiry bool
	// Expected authorization code or OAuth 2.0 error code on success.
	wantCode      string
	wantErrorCode string
}

func (j jarmTest) run(t *testing.T) {
	t.Helper()

	signKey := j.signKey
	if signKey == nil {
		signKey = newRSAKey(t)
	}
	verificationKey := j.verificationKey
	if verificationKey == nil {
		verificationKey = signKey
	}

	issuer := "https://foo"
	if j.issuer != "" {
		issuer = j.issuer
	}
	state := "some-state"
	if j.state != "" {
		state = j.state
	}

	ks := &StaticKeySet{PublicKeys: []crypto.PublicKey{verificationKey.pub}}
	verifier := NewJARMVerifier(issuer, ks, &j.config)

	token := signKey.sign(t, []byte(j.claims))
	resp, err := verifier.Verify(t.Context(), token, state)
	if err != nil {
		if j.wantErrExpiry {
			var errExp *TokenExpiredError
			if !errors.As(err, &errExp) {
				t.Fatalf("expected *TokenExpiredError but got %v", err)
			}
			return
		}
		if !j.wantErr {
			t.Fatalf("Verify() unexpected error: %v", err)
		}
		return
	}
	if j.wantErr || j.wantErrExpiry {
		t.Fatalf("Verify() expected error, got response: %+v", resp)
	}

	if j.wantErrorCode != "" {
		if resp.Error == nil {
			t.Fatalf("expected error response %q, got code response", j.wantErrorCode)
		}
		if resp.Error.Code != j.wantErrorCode {
			t.Errorf("Error.Code = %q, want %q", resp.Error.Code, j.wantErrorCode)
		}
		if resp.Code != "" {
			t.Errorf("error response unexpectedly carried code %q", resp.Code)
		}
		return
	}
	if resp.Error != nil {
		t.Fatalf("expected code response, got error: %v", resp.Error)
	}
	if resp.Code != j.wantCode {
		t.Errorf("Code = %q, want %q", resp.Code, j.wantCode)
	}
	if resp.State != state {
		t.Errorf("State = %q, want %q", resp.State, state)
	}
}

func TestJARMVerify(t *testing.T) {
	validClaims := `{
		"iss": "https://foo",
		"aud": "client1",
		"exp": ` + fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()) + `,
		"state": "some-state",
		"code": "some-code"
	}`

	tests := []jarmTest{
		{
			name:     "valid code response",
			claims:   validClaims,
			config:   JARMConfig{ClientID: "client1"},
			wantCode: "some-code",
		},
		{
			name: "valid error response",
			claims: `{
				"iss": "https://foo",
				"aud": "client1",
				"exp": ` + fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()) + `,
				"state": "some-state",
				"error": "access_denied",
				"error_description": "user declined"
			}`,
			config:        JARMConfig{ClientID: "client1"},
			wantErrorCode: "access_denied",
		},
		{
			name: "audience as list",
			claims: `{
				"iss": "https://foo",
				"aud": ["other", "client1"],
				"exp": ` + fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()) + `,
				"state": "some-state",
				"code": "some-code"
			}`,
			config:   JARMConfig{ClientID: "client1"},
			wantCode: "some-code",
		},
		{
			name:    "contradictory code and error",
			claims:  `{"iss":"https://foo","aud":"client1","exp":9999999999,"state":"some-state","code":"c","error":"access_denied"}`,
			config:  JARMConfig{ClientID: "client1"},
			wantErr: true,
		},
		{
			name:    "neither code nor error",
			claims:  `{"iss":"https://foo","aud":"client1","exp":9999999999,"state":"some-state"}`,
			config:  JARMConfig{ClientID: "client1"},
			wantErr: true,
		},
		{
			name:            "invalid signature",
			claims:          validClaims,
			verificationKey: newRSAKey(t),
			config:          JARMConfig{ClientID: "client1"},
			wantErr:         true,
		},
		{
			name:    "disallowed algorithm",
			claims:  validClaims,
			signKey: newECDSAKey(t),
			config: JARMConfig{
				ClientID:             "client1",
				SupportedSigningAlgs: []string{RS256},
			},
			wantErr: true,
		},
		{
			name:    "explicitly allowed algorithm",
			claims:  validClaims,
			signKey: newECDSAKey(t),
			config: JARMConfig{
				ClientID:             "client1",
				SupportedSigningAlgs: []string{ES256},
			},
			wantCode: "some-code",
		},
		{
			name:    "invalid issuer",
			claims:  validClaims,
			issuer:  "https://bar",
			config:  JARMConfig{ClientID: "client1"},
			wantErr: true,
		},
		{
			name:    "invalid audience",
			claims:  validClaims,
			config:  JARMConfig{ClientID: "client2"},
			wantErr: true,
		},
		{
			name:    "missing client id config",
			claims:  validClaims,
			config:  JARMConfig{},
			wantErr: true,
		},
		{
			name:          "expired response",
			claims:        `{"iss":"https://foo","aud":"client1","exp":1,"state":"some-state","code":"some-code"}`,
			config:        JARMConfig{ClientID: "client1"},
			wantErrExpiry: true,
		},
		{
			name:    "missing expiry",
			claims:  `{"iss":"https://foo","aud":"client1","state":"some-state","code":"some-code"}`,
			config:  JARMConfig{ClientID: "client1"},
			wantErr: true,
		},
		{
			name:    "state mismatch",
			claims:  validClaims,
			config:  JARMConfig{ClientID: "client1"},
			state:   "other-state",
			wantErr: true,
		},
		{
			name:    "missing state",
			claims:  `{"iss":"https://foo","aud":"client1","exp":9999999999,"code":"some-code"}`,
			config:  JARMConfig{ClientID: "client1"},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

// ExampleJARMVerifier demonstrates a login callback handler that verifies a
// JARM response. It runs against a test identity provider backed by
// httptest.
func ExampleJARMVerifier() {
	// Key used by the test identity provider to sign JARM responses.
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                srv.URL,
			"authorization_endpoint":                srv.URL + "/auth",
			"token_endpoint":                        srv.URL + "/token",
			"jwks_uri":                              srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{
			Keys: []jose.JSONWebKey{{
				Key:       priv.Public(),
				Use:       "sig",
				Algorithm: string(jose.RS256),
			}},
		})
	})

	ctx := context.Background()
	provider, err := NewProvider(ctx, srv.URL)
	if err != nil {
		log.Fatal(err)
	}
	verifier := provider.JARMVerifier(&JARMConfig{ClientID: "my-client-id"})

	// State saved when the authorization request was initiated, e.g. in a
	// cookie. The callback only accepts responses bound to this value.
	const expectedState = "af0ifjsldkj"

	// Login callback handler for an authorization server using JARM. The
	// authorization response is delivered as a signed JWT in the "response"
	// query parameter.
	callback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := verifier.Verify(r.Context(), r.URL.Query().Get("response"), expectedState)
		if err != nil {
			http.Error(w, "Invalid response: "+err.Error(), http.StatusBadRequest)
			return
		}
		if resp.Error != nil {
			http.Error(w, "Authorization failed: "+resp.Error.Error(), http.StatusForbidden)
			return
		}
		// The signature, issuer, audience, expiry, and state have all been
		// validated. It's now safe to exchange the authorization code.
		fmt.Fprintf(w, "exchanging code: %s", resp.Code)
	})

	// The identity provider issues a signed JARM response.
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: priv}, nil)
	if err != nil {
		log.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"iss":   srv.URL,
		"aud":   "my-client-id",
		"exp":   time.Now().Add(time.Minute).Unix(),
		"state": expectedState,
		"code":  "splxlOBeZQQYbYS6WxSbIA",
	})
	if err != nil {
		log.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		log.Fatal(err)
	}
	rawResponse, err := jws.CompactSerialize()
	if err != nil {
		log.Fatal(err)
	}

	// Simulate the redirect back to the callback.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/callback?response="+rawResponse, nil)
	callback.ServeHTTP(rec, req)

	fmt.Println(rec.Body.String())
	// Output: exchanging code: splxlOBeZQQYbYS6WxSbIA
}
