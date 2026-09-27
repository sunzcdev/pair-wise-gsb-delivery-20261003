package oidc_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

// This example runs a fake OpenID Connect provider with httptest and shows a
// client callback handler verifying a JARM (JWT Secured Authorization
// Response Mode) response before using the authorization code.
func ExampleJARMVerifier() {
	// The provider's signing key. In a real deployment the client only sees
	// the public half, advertised through the provider's JWKS endpoint.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	// A fake provider serving discovery metadata and its JWKS.
	var provider *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"issuer": %q,
			"authorization_endpoint": "%[1]s/auth",
			"token_endpoint": "%[1]s/token",
			"jwks_uri": "%[1]s/keys",
			"id_token_signing_alg_values_supported": ["RS256"]
		}`, provider.URL)
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		jwk := jose.JSONWebKey{Key: key.Public(), Use: "sig", Algorithm: string(jose.RS256)}
		fmt.Fprintf(w, `{"keys": [%s]}`, mustJSON(jwk))
	})
	provider = httptest.NewServer(mux)
	defer provider.Close()

	ctx := context.Background()
	p, err := oidc.NewProvider(ctx, provider.URL)
	if err != nil {
		panic(err)
	}

	const clientID = "my-client"
	verifier := p.JARMVerifier(&oidc.JARMConfig{ClientID: clientID})

	// State generated and stored when the authorization request was created,
	// e.g. in the user's session.
	const storedState = "e3b0c44298fc1c14"

	// The client application's redirect URI handler.
	callback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawResponse := r.URL.Query().Get("response")
		resp, err := verifier.Verify(r.Context(), rawResponse, storedState)
		var authErr *oidc.AuthorizationError
		if errors.As(err, &authErr) {
			// The provider denied the request with a verified OAuth error.
			http.Error(w, "authorization failed: "+authErr.Code, http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(w, "invalid authorization response", http.StatusBadRequest)
			return
		}
		// The signature, issuer, audience, expiry, and state have all been
		// verified. Only now is it safe to redeem the code at the token
		// endpoint.
		fmt.Fprintf(w, "exchanging code: %s", resp.Code)
	})

	// The provider signs the authorization response parameters into a JARM
	// JWT and redirects the user agent back to the client's redirect URI.
	jarm := signJARM(key, map[string]any{
		"iss":   provider.URL,
		"aud":   clientID,
		"exp":   time.Now().Add(time.Minute).Unix(),
		"code":  "SplxlOBeZQQYbYS6WxSbIA",
		"state": storedState,
	})
	redirectURL := "/callback?response=" + url.QueryEscape(jarm)

	recorder := httptest.NewRecorder()
	callback.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, redirectURL, nil))
	fmt.Println(recorder.Body.String())

	// Output: exchanging code: SplxlOBeZQQYbYS6WxSbIA
}

func signJARM(key *rsa.PrivateKey, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		panic(err)
	}
	jws, err := signer.Sign([]byte(mustJSON(claims)))
	if err != nil {
		panic(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		panic(err)
	}
	return raw
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
