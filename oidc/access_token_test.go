package oidc

import (
	"crypto"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// signWithType signs the payload like signingKey.sign, but sets the "typ"
// header. An empty typ omits the header.
func signWithType(t *testing.T, key *signingKey, typ string, payload []byte) string {
	t.Helper()
	privKey := &jose.JSONWebKey{Key: key.priv, Algorithm: string(key.alg), KeyID: key.keyID}
	opts := new(jose.SignerOptions)
	if typ != "" {
		opts.WithType(jose.ContentType(typ))
	}
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

// validAccessTokenClaims returns a claim set satisfying RFC 9068.
func validAccessTokenClaims() string {
	return `{"iss":"https://foo","sub":"user123","aud":"https://api.example.com",` +
		`"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
		`,"iat":` + strconv.FormatInt(time.Now().Unix(), 10) +
		`,"client_id":"client123","jti":"token-id-1","scope":"read write"}`
}

type accessTokenVerificationTest struct {
	// Name of the subtest.
	name string

	// If not provided defaults to "https://foo".
	issuer string

	// JWT payload (just the claims). Defaults to validAccessTokenClaims().
	payload string

	// Value of the "typ" header. Defaults to "at+jwt". The literal value
	// "none" omits the header entirely.
	typ string

	// Key to sign the token with.
	signKey *signingKey
	// If not provided defaults to signKey. Only useful when testing invalid
	// signatures.
	verificationKey *signingKey

	config        AccessTokenConfig
	wantErr       bool
	wantErrExpiry bool
}

func (v accessTokenVerificationTest) runGetToken(t *testing.T) (*AccessToken, error) {
	payload := v.payload
	if payload == "" {
		payload = validAccessTokenClaims()
	}
	typ := v.typ
	switch typ {
	case "":
		typ = "at+jwt"
	case "none":
		typ = ""
	}
	token := signWithType(t, v.signKey, typ, []byte(payload))

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

func (v accessTokenVerificationTest) run(t *testing.T) {
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
			t.Errorf("expected *TokenExpiredError but got %q", err)
		}
	}
}

func defaultAccessTokenConfig() AccessTokenConfig {
	return AccessTokenConfig{ExpectedAudience: []string{"https://api.example.com"}}
}

func TestAccessTokenVerify(t *testing.T) {
	tests := []accessTokenVerificationTest{
		{
			name:    "valid token",
			config:  defaultAccessTokenConfig(),
			signKey: newRSAKey(t),
		},
		{
			name:    "media type typ header",
			typ:     "application/at+jwt",
			config:  defaultAccessTokenConfig(),
			signKey: newRSAKey(t),
		},
		{
			name:    "missing typ header",
			typ:     "none",
			config:  defaultAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:    "wrong typ header",
			typ:     "JWT",
			config:  defaultAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:    "missing audience in config",
			config:  AccessTokenConfig{},
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:    "wrong audience",
			config:  AccessTokenConfig{ExpectedAudience: []string{"https://other-api.example.com"}},
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:    "one of many expected audiences",
			config:  AccessTokenConfig{ExpectedAudience: []string{"https://other-api.example.com", "https://api.example.com"}},
			signKey: newRSAKey(t),
		},
		{
			name: "multiple token audiences, one matches",
			payload: `{"iss":"https://foo","sub":"user123","aud":["https://api.example.com","https://api2.example.com"],` +
				`"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
				`,"iat":` + strconv.FormatInt(time.Now().Unix(), 10) +
				`,"client_id":"client123","jti":"token-id-1"}`,
			config:  defaultAccessTokenConfig(),
			signKey: newRSAKey(t),
		},
		{
			name:    "invalid issuer",
			issuer:  "https://bar",
			config:  defaultAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:    "skip issuer check",
			issuer:  "https://bar",
			config:  AccessTokenConfig{ExpectedAudience: []string{"https://api.example.com"}, SkipIssuerCheck: true},
			signKey: newRSAKey(t),
		},
		{
			name: "expired token",
			payload: `{"iss":"https://foo","sub":"user123","aud":"https://api.example.com",` +
				`"exp":` + strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10) +
				`,"iat":` + strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10) +
				`,"client_id":"client123","jti":"token-id-1"}`,
			config:        defaultAccessTokenConfig(),
			signKey:       newRSAKey(t),
			wantErrExpiry: true,
		},
		{
			name: "nbf in future",
			payload: `{"iss":"https://foo","sub":"user123","aud":"https://api.example.com",` +
				`"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
				`,"iat":` + strconv.FormatInt(time.Now().Unix(), 10) +
				`,"nbf":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
				`,"client_id":"client123","jti":"token-id-1"}`,
			config:  defaultAccessTokenConfig(),
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:            "invalid signature",
			config:          defaultAccessTokenConfig(),
			signKey:         newRSAKey(t),
			verificationKey: newRSAKey(t),
			wantErr:         true,
		},
		{
			name:    "signing alg not allowed",
			config:  AccessTokenConfig{ExpectedAudience: []string{"https://api.example.com"}, SupportedSigningAlgs: []string{RS256}},
			signKey: newECDSAKey(t),
			wantErr: true,
		},
		{
			name:    "ecdsa signing alg allowed",
			config:  AccessTokenConfig{ExpectedAudience: []string{"https://api.example.com"}, SupportedSigningAlgs: []string{ES256}},
			signKey: newECDSAKey(t),
		},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func TestAccessTokenRequiredClaims(t *testing.T) {
	required := []string{"iss", "exp", "aud", "sub", "client_id", "iat", "jti"}
	for _, missing := range required {
		t.Run("missing "+missing, func(t *testing.T) {
			claims := map[string]any{
				"iss":       "https://foo",
				"sub":       "user123",
				"aud":       "https://api.example.com",
				"exp":       time.Now().Add(time.Hour).Unix(),
				"iat":       time.Now().Unix(),
				"client_id": "client123",
				"jti":       "token-id-1",
			}
			delete(claims, missing)
			payload, err := json.Marshal(claims)
			if err != nil {
				t.Fatal(err)
			}
			test := accessTokenVerificationTest{
				payload: string(payload),
				config:  defaultAccessTokenConfig(),
				signKey: newRSAKey(t),
				wantErr: true,
			}
			test.run(t)
		})
	}
}

func TestAccessTokenClaimTypes(t *testing.T) {
	// Claims with unexpected JSON types must be rejected, not coerced.
	tests := []struct {
		name    string
		payload string
	}{
		{
			name: "client_id as number",
			payload: `{"iss":"https://foo","sub":"user123","aud":"https://api.example.com",` +
				`"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
				`,"iat":` + strconv.FormatInt(time.Now().Unix(), 10) +
				`,"client_id":123,"jti":"token-id-1"}`,
		},
		{
			name: "sub as number",
			payload: `{"iss":"https://foo","sub":123,"aud":"https://api.example.com",` +
				`"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
				`,"iat":` + strconv.FormatInt(time.Now().Unix(), 10) +
				`,"client_id":"client123","jti":"token-id-1"}`,
		},
		{
			name: "aud as number",
			payload: `{"iss":"https://foo","sub":"user123","aud":123,` +
				`"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
				`,"iat":` + strconv.FormatInt(time.Now().Unix(), 10) +
				`,"client_id":"client123","jti":"token-id-1"}`,
		},
		{
			name: "exp as string",
			payload: `{"iss":"https://foo","sub":"user123","aud":"https://api.example.com",` +
				`"exp":"` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `"` +
				`,"iat":` + strconv.FormatInt(time.Now().Unix(), 10) +
				`,"client_id":"client123","jti":"token-id-1"}`,
		},
		{
			name: "scope as array",
			payload: `{"iss":"https://foo","sub":"user123","aud":"https://api.example.com",` +
				`"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
				`,"iat":` + strconv.FormatInt(time.Now().Unix(), 10) +
				`,"client_id":"client123","jti":"token-id-1","scope":["read","write"]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			vt := accessTokenVerificationTest{
				payload: test.payload,
				config:  defaultAccessTokenConfig(),
				signKey: newRSAKey(t),
				wantErr: true,
			}
			vt.run(t)
		})
	}
}

func TestAccessTokenClaims(t *testing.T) {
	vt := accessTokenVerificationTest{
		config:  defaultAccessTokenConfig(),
		signKey: newRSAKey(t),
	}
	token, err := vt.runGetToken(t)
	if err != nil {
		t.Fatalf("verifying token: %v", err)
	}
	if token.Issuer != "https://foo" {
		t.Errorf("unexpected issuer %q", token.Issuer)
	}
	if token.Subject != "user123" {
		t.Errorf("unexpected subject %q", token.Subject)
	}
	if token.ClientID != "client123" {
		t.Errorf("unexpected client_id %q", token.ClientID)
	}
	if token.JWTID != "token-id-1" {
		t.Errorf("unexpected jti %q", token.JWTID)
	}
	if !reflect.DeepEqual(token.Scopes, []string{"read", "write"}) {
		t.Errorf("unexpected scopes %q", token.Scopes)
	}
	if !reflect.DeepEqual(token.Audience, []string{"https://api.example.com"}) {
		t.Errorf("unexpected audience %q", token.Audience)
	}
	var claims struct {
		Scope string `json:"scope"`
	}
	if err := token.Claims(&claims); err != nil {
		t.Fatalf("unmarshaling claims: %v", err)
	}
	if claims.Scope != "read write" {
		t.Errorf("unexpected raw scope claim %q", claims.Scope)
	}
}

// TestAccessTokenCrossValidation ensures tokens issued by the same provider
// for different purposes are not interchangeable: an ID Token that is valid
// for login must not be accepted by the resource server, and an access token
// must not be usable as a login credential.
func TestAccessTokenCrossValidation(t *testing.T) {
	key := newRSAKey(t)
	ks := &StaticKeySet{PublicKeys: []crypto.PublicKey{key.pub}}
	ctx := t.Context()

	idTokenVerifier := NewVerifier("https://foo", ks, &Config{ClientID: "client123"})
	accessTokenVerifier := NewAccessTokenVerifier("https://foo", ks, &AccessTokenConfig{
		ExpectedAudience: []string{"https://api.example.com"},
	})

	// An ID Token valid for login carries no "typ" header and is rejected by
	// the resource server.
	rawIDToken := key.sign(t, []byte(`{"iss":"https://foo","sub":"user123","aud":"client123",`+
		`"exp":`+strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)+
		`,"iat":`+strconv.FormatInt(time.Now().Unix(), 10)+`}`))
	if _, err := idTokenVerifier.Verify(ctx, rawIDToken); err != nil {
		t.Fatalf("id token should be valid for login: %v", err)
	}
	if _, err := accessTokenVerifier.Verify(ctx, rawIDToken); err == nil {
		t.Errorf("id token must not be accepted as an access token")
	}

	// A valid access token is accepted by the resource server, but is not a
	// valid login credential for the client.
	rawAccessToken := signWithType(t, key, "at+jwt", []byte(validAccessTokenClaims()))
	if _, err := accessTokenVerifier.Verify(ctx, rawAccessToken); err != nil {
		t.Fatalf("access token should be valid for the resource server: %v", err)
	}
	if _, err := idTokenVerifier.Verify(ctx, rawAccessToken); err == nil {
		t.Errorf("access token must not be accepted as an id token")
	}
}
