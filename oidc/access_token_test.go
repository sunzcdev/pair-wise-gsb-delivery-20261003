package oidc

import (
	"crypto"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// signWithType creates a JWS with the given "typ" header using the private key.
func signWithType(t testing.TB, key *signingKey, typ string, payload []byte) string {
	opts := new(jose.SignerOptions)
	if typ != "" {
		opts.WithType(jose.ContentType(typ))
	}
	privKey := &jose.JSONWebKey{Key: key.priv, Algorithm: string(key.alg), KeyID: key.keyID}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: key.alg, Key: privKey}, opts)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	data, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type verifyAccessTokenTest struct {
	// Name of the subtest.
	name string

	// If not provided defaults to "https://foo".
	issuer string

	// JWT payload (just the claims).
	payload string

	// "typ" header of the JWT. Defaults to "at+jwt".
	typ string

	// Key to sign the access token with.
	signKey *signingKey
	// If not provided defaults to signKey. Only useful when
	// testing invalid signatures.
	verificationKey *signingKey

	config        AccessTokenConfig
	wantErr       bool
	wantErrExpiry bool
}

func (v verifyAccessTokenTest) runGetToken(t *testing.T) (*AccessToken, error) {
	typ := v.typ
	if typ == "" {
		typ = AccessTokenTypeJWT
	}
	token := signWithType(t, v.signKey, typ, []byte(v.payload))

	issuer := "https://foo"
	if v.issuer != "" {
		issuer = v.issuer
	}
	var ks KeySet
	if v.verificationKey != nil {
		ks = &StaticKeySet{PublicKeys: []crypto.PublicKey{v.verificationKey.pub}}
	} else {
		ks = &StaticKeySet{PublicKeys: []crypto.PublicKey{v.signKey.pub}}
	}
	verifier := NewAccessTokenVerifier(issuer, ks, &v.config)

	return verifier.Verify(t.Context(), token)
}

func (v verifyAccessTokenTest) run(t *testing.T) {
	_, err := v.runGetToken(t)
	if err != nil && !v.wantErr && !v.wantErrExpiry {
		t.Errorf("%v", err)
	}
	if err == nil && (v.wantErr || v.wantErrExpiry) {
		t.Errorf("expected error")
	}
	if v.wantErrExpiry {
		var errExp *TokenExpiredError
		if !errors.As(err, &errExp) {
			t.Errorf("expected *TokenExpiryError but got %q", err)
		}
	}
}

func validAccessTokenClaims() string {
	return `{"iss":"https://foo","sub":"user1","aud":"https://api.example.com",` +
		`"client_id":"client1","scope":"read write","jti":"token-1",` +
		`"iat":` + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) +
		`,"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`
}

func validAccessTokenConfig() AccessTokenConfig {
	return AccessTokenConfig{ExpectedAudience: "https://api.example.com"}
}

func TestVerifyAccessToken(t *testing.T) {
	tests := []verifyAccessTokenTest{
		{
			name:    "good token",
			payload: validAccessTokenClaims(),
			config:  validAccessTokenConfig(),
			signKey: newRSAKey(t),
		},
		{
			name:    "good token, media type typ header",
			payload: validAccessTokenClaims(),
			typ:     "application/at+jwt",
			config:  validAccessTokenConfig(),
			signKey: newRSAKey(t),
		},
		{
			name:    "unrecognized typ header",
			payload: validAccessTokenClaims(),
			typ:     "none",
			config:  validAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:    "wrong typ header",
			payload: validAccessTokenClaims(),
			typ:     "JWT",
			config:  validAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:    "invalid issuer",
			issuer:  "https://bar",
			payload: validAccessTokenClaims(),
			config:  validAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:    "mismatched audience",
			payload: validAccessTokenClaims(),
			config:  AccessTokenConfig{ExpectedAudience: "https://other.example.com"},
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name: "multiple audiences, one matches",
			payload: `{"iss":"https://foo","sub":"user1","aud":["https://api.example.com","https://other.example.com"],` +
				`"client_id":"client1","jti":"token-1",` +
				`"iat":` + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) +
				`,"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`,
			config:  validAccessTokenConfig(),
			signKey: newRSAKey(t),
		},
		{
			name:    "missing expected audience config",
			payload: validAccessTokenClaims(),
			config:  AccessTokenConfig{},
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:            "invalid sig",
			payload:         validAccessTokenClaims(),
			config:          validAccessTokenConfig(),
			signKey:         newRSAKey(t),
			verificationKey: newRSAKey(t),
			wantErr:         true,
		},
		{
			name: "expired token",
			payload: `{"iss":"https://foo","sub":"user1","aud":"https://api.example.com",` +
				`"client_id":"client1","jti":"token-1",` +
				`"iat":` + strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10) +
				`,"exp":` + strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10) + `}`,
			config:        validAccessTokenConfig(),
			signKey:       newRSAKey(t),
			wantErrExpiry: true,
		},
		{
			name: "nbf in future",
			payload: `{"iss":"https://foo","sub":"user1","aud":"https://api.example.com",` +
				`"client_id":"client1","jti":"token-1",` +
				`"iat":` + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) +
				`,"nbf":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
				`,"exp":` + strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10) + `}`,
			config:  validAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:    "bad signing alg",
			payload: validAccessTokenClaims(),
			config:  validAccessTokenConfig(),
			signKey: newECDSAKey(t),
			wantErr: true,
		},
		{
			name:    "ecdsa signing",
			payload: validAccessTokenClaims(),
			config: AccessTokenConfig{
				ExpectedAudience:     "https://api.example.com",
				SupportedSigningAlgs: []string{ES256},
			},
			signKey: newECDSAKey(t),
		},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func TestVerifyAccessTokenRequiredClaims(t *testing.T) {
	claims := map[string]string{
		"iss":       `"iss":"https://foo"`,
		"sub":       `"sub":"user1"`,
		"aud":       `"aud":"https://api.example.com"`,
		"client_id": `"client_id":"client1"`,
		"iat":       `"iat":` + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10),
		"exp":       `"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
		"jti":       `"jti":"token-1"`,
	}
	for missing := range claims {
		t.Run("missing "+missing, func(t *testing.T) {
			payload := "{"
			first := true
			for name, claim := range claims {
				if name == missing {
					continue
				}
				if !first {
					payload += ","
				}
				payload += claim
				first = false
			}
			payload += "}"
			verifyAccessTokenTest{
				name:    "missing " + missing,
				payload: payload,
				config:  validAccessTokenConfig(),
				signKey: newRSAKey(t),
				wantErr: true,
			}.run(t)
		})
	}
}

func TestVerifyAccessTokenStrictClaimTypes(t *testing.T) {
	tests := []verifyAccessTokenTest{
		{
			name: "client_id as number",
			payload: `{"iss":"https://foo","sub":"user1","aud":"https://api.example.com",` +
				`"client_id":123,"jti":"token-1",` +
				`"iat":` + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) +
				`,"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`,
			config:  validAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name: "scope as array",
			payload: `{"iss":"https://foo","sub":"user1","aud":"https://api.example.com",` +
				`"client_id":"client1","scope":["read","write"],"jti":"token-1",` +
				`"iat":` + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) +
				`,"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`,
			config:  validAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name: "jti as number",
			payload: `{"iss":"https://foo","sub":"user1","aud":"https://api.example.com",` +
				`"client_id":"client1","jti":123,` +
				`"iat":` + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) +
				`,"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`,
			config:  validAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name: "exp as string",
			payload: `{"iss":"https://foo","sub":"user1","aud":"https://api.example.com",` +
				`"client_id":"client1","jti":"token-1",` +
				`"iat":` + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) +
				`,"exp":"` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `"}`,
			config:  validAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func TestVerifyAccessTokenScopes(t *testing.T) {
	test := verifyAccessTokenTest{
		name:    "scopes parsed",
		payload: validAccessTokenClaims(),
		config:  validAccessTokenConfig(),
		signKey: newRSAKey(t),
	}
	token, err := test.runGetToken(t)
	if err != nil {
		t.Fatalf("parsing token: %v", err)
	}
	if want := []string{"read", "write"}; !reflect.DeepEqual(token.Scopes, want) {
		t.Errorf("expected scopes %v, got %v", want, token.Scopes)
	}
	if token.ClientID != "client1" {
		t.Errorf("expected client_id %q, got %q", "client1", token.ClientID)
	}
	if token.Subject != "user1" {
		t.Errorf("expected sub %q, got %q", "user1", token.Subject)
	}
	if token.JWTID != "token-1" {
		t.Errorf("expected jti %q, got %q", "token-1", token.JWTID)
	}
}

// TestAccessTokenIDTokenCrossCheck signs both an ID token and an access token
// with the same provider key and verifies that a token valid for one purpose
// is rejected for the other.
func TestAccessTokenIDTokenCrossCheck(t *testing.T) {
	signKey := newRSAKey(t)
	ks := &StaticKeySet{PublicKeys: []crypto.PublicKey{signKey.pub}}

	idTokenVerifier := NewVerifier("https://foo", ks, &Config{
		ClientID:        "client1",
		SkipExpiryCheck: true,
	})
	accessTokenVerifier := NewAccessTokenVerifier("https://foo", ks, &AccessTokenConfig{
		ExpectedAudience: "https://api.example.com",
	})

	ctx := t.Context()

	// An ID token valid for login must not grant access to the resource API.
	idToken := signKey.sign(t, []byte(`{"iss":"https://foo","aud":"client1","sub":"user1"}`))
	if _, err := idTokenVerifier.Verify(ctx, idToken); err != nil {
		t.Fatalf("id token should verify for login: %v", err)
	}
	if _, err := accessTokenVerifier.Verify(ctx, idToken); err == nil {
		t.Errorf("id token must not be accepted as an access token")
	}

	// An access token valid for the resource API must not be usable for login.
	accessToken := signWithType(t, signKey, AccessTokenTypeJWT, []byte(validAccessTokenClaims()))
	if _, err := accessTokenVerifier.Verify(ctx, accessToken); err != nil {
		t.Fatalf("access token should verify for the resource API: %v", err)
	}
	if _, err := idTokenVerifier.Verify(ctx, accessToken); err == nil {
		t.Errorf("access token must not be accepted as an id token")
	}
}
