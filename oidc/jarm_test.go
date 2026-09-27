package oidc

import (
	"context"
	"crypto"
	"errors"
	"strconv"
	"testing"
	"time"
)

type jarmTest struct {
	name          string
	claims        string
	config        JARMConfig
	issuer        string
	expectedState string
	signKey       *signingKey
	// If set, use this key to verify the signature instead of signKey.
	verificationKey *signingKey
	wantCode        string
	wantOAuthError  string
	wantErr         bool
	wantErrExpiry   bool
}

func (j jarmTest) run(t *testing.T) {
	issuer := j.issuer
	if issuer == "" {
		issuer = "https://foo"
	}
	signKey := j.signKey
	if signKey == nil {
		signKey = newRSAKey(t)
	}
	verificationKey := j.verificationKey
	if verificationKey == nil {
		verificationKey = signKey
	}

	keySet := &StaticKeySet{PublicKeys: []crypto.PublicKey{verificationKey.pub}}
	verifier := NewJARMVerifier(issuer, keySet, &j.config)

	jwt := signKey.sign(t, []byte(j.claims))
	resp, err := verifier.Verify(context.Background(), jwt, j.expectedState)

	if j.wantErrExpiry {
		var expiredErr *TokenExpiredError
		if !errors.As(err, &expiredErr) {
			t.Fatalf("expected TokenExpiredError, got %v", err)
		}
		return
	}
	if j.wantOAuthError != "" {
		var authErr *AuthorizationError
		if !errors.As(err, &authErr) {
			t.Fatalf("expected AuthorizationError, got %v", err)
		}
		if authErr.Code != j.wantOAuthError {
			t.Fatalf("expected oauth error %q, got %q", j.wantOAuthError, authErr.Code)
		}
		return
	}
	if j.wantErr {
		if err == nil {
			t.Fatalf("expected error, got success")
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Code != j.wantCode {
		t.Errorf("expected code %q, got %q", j.wantCode, resp.Code)
	}
	if resp.State != j.expectedState {
		t.Errorf("expected state %q, got %q", j.expectedState, resp.State)
	}
}

func TestJARMVerify(t *testing.T) {
	exp := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	config := JARMConfig{ClientID: "client123"}
	tests := []jarmTest{
		{
			name:          "good response",
			claims:        `{"iss":"https://foo","aud":"client123","exp":` + exp + `,"code":"abc","state":"xyz"}`,
			config:        config,
			expectedState: "xyz",
			wantCode:      "abc",
		},
		{
			name:           "oauth error response",
			claims:         `{"iss":"https://foo","aud":"client123","exp":` + exp + `,"error":"access_denied","error_description":"nope","state":"xyz"}`,
			config:         config,
			expectedState:  "xyz",
			wantOAuthError: "access_denied",
		},
		{
			name:          "code and error",
			claims:        `{"iss":"https://foo","aud":"client123","exp":` + exp + `,"code":"abc","error":"access_denied","state":"xyz"}`,
			config:        config,
			expectedState: "xyz",
			wantErr:       true,
		},
		{
			name:          "neither code nor error",
			claims:        `{"iss":"https://foo","aud":"client123","exp":` + exp + `,"state":"xyz"}`,
			config:        config,
			expectedState: "xyz",
			wantErr:       true,
		},
		{
			name:          "state mismatch",
			claims:        `{"iss":"https://foo","aud":"client123","exp":` + exp + `,"code":"abc","state":"xyz"}`,
			config:        config,
			expectedState: "different",
			wantErr:       true,
		},
		{
			name:          "invalid issuer",
			claims:        `{"iss":"https://bar","aud":"client123","exp":` + exp + `,"code":"abc","state":"xyz"}`,
			config:        config,
			expectedState: "xyz",
			wantErr:       true,
		},
		{
			name:          "invalid audience",
			claims:        `{"iss":"https://foo","aud":"other-client","exp":` + exp + `,"code":"abc","state":"xyz"}`,
			config:        config,
			expectedState: "xyz",
			wantErr:       true,
		},
		{
			name:          "audience list",
			claims:        `{"iss":"https://foo","aud":["client123","other"],"exp":` + exp + `,"code":"abc","state":"xyz"}`,
			config:        config,
			expectedState: "xyz",
			wantCode:      "abc",
		},
		{
			name:          "expired response",
			claims:        `{"iss":"https://foo","aud":"client123","exp":` + strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10) + `,"code":"abc","state":"xyz"}`,
			config:        config,
			expectedState: "xyz",
			wantErrExpiry: true,
		},
		{
			name:          "missing exp",
			claims:        `{"iss":"https://foo","aud":"client123","code":"abc","state":"xyz"}`,
			config:        config,
			expectedState: "xyz",
			wantErr:       true,
		},
		{
			name:            "invalid signature",
			claims:          `{"iss":"https://foo","aud":"client123","exp":` + exp + `,"code":"abc","state":"xyz"}`,
			config:          config,
			expectedState:   "xyz",
			verificationKey: newRSAKey(t),
			wantErr:         true,
		},
		{
			name:          "algorithm not allowed",
			claims:        `{"iss":"https://foo","aud":"client123","exp":` + exp + `,"code":"abc","state":"xyz"}`,
			config:        JARMConfig{ClientID: "client123", SupportedSigningAlgs: []string{RS256}},
			expectedState: "xyz",
			signKey:       newEdDSAKey(t),
			wantErr:       true,
		},
		{
			name:          "explicit algorithm policy",
			claims:        `{"iss":"https://foo","aud":"client123","exp":` + exp + `,"code":"abc","state":"xyz"}`,
			config:        JARMConfig{ClientID: "client123", SupportedSigningAlgs: []string{EdDSA}},
			expectedState: "xyz",
			signKey:       newEdDSAKey(t),
			wantCode:      "abc",
		},
		{
			name:          "missing client id",
			claims:        `{"iss":"https://foo","aud":"client123","exp":` + exp + `,"code":"abc","state":"xyz"}`,
			config:        JARMConfig{},
			expectedState: "xyz",
			wantErr:       true,
		},
		{
			name: "nbf in future",
			claims: `{"iss":"https://foo","aud":"client123","exp":` + exp +
				`,"nbf":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
				`,"code":"abc","state":"xyz"}`,
			config:        config,
			expectedState: "xyz",
			wantErr:       true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}
