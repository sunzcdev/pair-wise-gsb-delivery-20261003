package oidc

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	tests := []verificationTest{
		{
			name:    "good token",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SkipClientIDCheck: true,
				SkipExpiryCheck:   true,
			},
			signKey: newRSAKey(t),
		},
		{
			name:    "good eddsa token",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SkipClientIDCheck:    true,
				SkipExpiryCheck:      true,
				SupportedSigningAlgs: []string{EdDSA},
			},
			signKey: newEdDSAKey(t),
		},
		{
			name:    "invalid issuer",
			issuer:  "https://bar",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SkipClientIDCheck: true,
				SkipExpiryCheck:   true,
			},
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:    "skip issuer check",
			issuer:  "https://bar",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SkipIssuerCheck:   true,
				SkipClientIDCheck: true,
				SkipExpiryCheck:   true,
			},
			signKey: newRSAKey(t),
		},
		{
			name:    "invalid sig",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SkipClientIDCheck: true,
				SkipExpiryCheck:   true,
			},
			signKey:         newRSAKey(t),
			verificationKey: newRSAKey(t),
			wantErr:         true,
		},
		{
			name:    "google accounts without scheme",
			issuer:  "https://accounts.google.com",
			idToken: `{"iss":"accounts.google.com"}`,
			config: Config{
				SkipClientIDCheck: true,
				SkipExpiryCheck:   true,
			},
			signKey: newRSAKey(t),
		},
		{
			name:    "expired token",
			idToken: `{"iss":"https://foo","exp":` + strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10) + `}`,
			config: Config{
				SkipClientIDCheck: true,
			},
			signKey:       newRSAKey(t),
			wantErrExpiry: true,
		},
		{
			name:    "unexpired token",
			idToken: `{"iss":"https://foo","exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`,
			config: Config{
				SkipClientIDCheck: true,
			},
			signKey: newRSAKey(t),
		},
		{
			name: "expiry as float",
			idToken: `{"iss":"https://foo","exp":` +
				strconv.FormatFloat(float64(time.Now().Add(time.Hour).Unix()), 'E', -1, 64) +
				`}`,
			config: Config{
				SkipClientIDCheck: true,
			},
			signKey: newRSAKey(t),
		},
		{
			name: "nbf in future",
			idToken: `{"iss":"https://foo","nbf":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
				`,"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`,
			config: Config{
				SkipClientIDCheck: true,
			},
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name: "nbf in past",
			idToken: `{"iss":"https://foo","nbf":` + strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10) +
				`,"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`,
			config: Config{
				SkipClientIDCheck: true,
			},
			signKey: newRSAKey(t),
		},
		{
			name: "nbf in future within clock skew tolerance",
			idToken: `{"iss":"https://foo","nbf":` + strconv.FormatInt(time.Now().Add(30*time.Second).Unix(), 10) +
				`,"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`,
			config: Config{
				SkipClientIDCheck: true,
			},
			signKey: newRSAKey(t),
		},
		{
			name:    "unsigned token",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SkipClientIDCheck: true,
				SkipExpiryCheck:   true,
			},
			wantErr: true,
		},
		{
			name:    "unsigned token InsecureSkipSignatureCheck",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SkipClientIDCheck:          true,
				SkipExpiryCheck:            true,
				InsecureSkipSignatureCheck: true,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func TestVerifyAudience(t *testing.T) {
	tests := []verificationTest{
		{
			name:    "good audience",
			idToken: `{"iss":"https://foo","aud":"client1"}`,
			config: Config{
				ClientID:        "client1",
				SkipExpiryCheck: true,
			},
			signKey: newRSAKey(t),
		},
		{
			name:    "mismatched audience",
			idToken: `{"iss":"https://foo","aud":"client2"}`,
			config: Config{
				ClientID:        "client1",
				SkipExpiryCheck: true,
			},
			signKey: newRSAKey(t),
			wantErr: true,
		},
		{
			name:    "multiple audiences, one matches",
			idToken: `{"iss":"https://foo","aud":["client1","client2"]}`,
			config: Config{
				ClientID:        "client2",
				SkipExpiryCheck: true,
			},
			signKey: newRSAKey(t),
		},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func TestVerifySigningAlg(t *testing.T) {
	tests := []verificationTest{
		{
			name:    "default signing alg",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SkipClientIDCheck: true,
				SkipExpiryCheck:   true,
			},
			signKey: newRSAKey(t),
		},
		{
			name:    "bad signing alg",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SkipClientIDCheck: true,
				SkipExpiryCheck:   true,
			},
			signKey: newECDSAKey(t),
			wantErr: true,
		},
		{
			name:    "ecdsa signing",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SupportedSigningAlgs: []string{ES256},
				SkipClientIDCheck:    true,
				SkipExpiryCheck:      true,
			},
			signKey: newECDSAKey(t),
		},
		{
			name:    "eddsa signing",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SkipClientIDCheck:    true,
				SkipExpiryCheck:      true,
				SupportedSigningAlgs: []string{EdDSA},
			},
			signKey: newEdDSAKey(t),
		},
		{
			name:    "one of many supported",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SkipClientIDCheck:    true,
				SkipExpiryCheck:      true,
				SupportedSigningAlgs: []string{RS256, ES256},
			},
			signKey: newECDSAKey(t),
		},
		{
			name:    "not in requiredAlgs",
			idToken: `{"iss":"https://foo"}`,
			config: Config{
				SupportedSigningAlgs: []string{RS256, ES512},
				SkipClientIDCheck:    true,
				SkipExpiryCheck:      true,
			},
			signKey: newECDSAKey(t),
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func TestAccessTokenHash(t *testing.T) {
	atHash := "piwt8oCH-K2D9pXlaS1Y-w"
	vt := verificationTest{
		name:    "preserves token hash and sig algo",
		idToken: `{"iss":"https://foo","aud":"client1", "at_hash": "` + atHash + `"}`,
		config: Config{
			ClientID:        "client1",
			SkipExpiryCheck: true,
		},
		signKey: newRSAKey(t),
	}
	t.Run("at_hash", func(t *testing.T) {
		tok, err := vt.runGetToken(t)
		if err != nil {
			t.Errorf("parsing token: %v", err)
			return
		}
		if tok.AccessTokenHash != atHash {
			t.Errorf("access token hash not preserved correctly, want %q got %q", atHash, tok.AccessTokenHash)
		}
		if tok.sigAlgorithm != RS256 {
			t.Errorf("invalid signature algo, want %q got %q", RS256, tok.sigAlgorithm)
		}
	})
}

func TestDistributedClaims(t *testing.T) {
	tests := []struct {
		test    verificationTest
		want    map[string]claimSource
		wantErr bool
	}{
		{
			test: verificationTest{
				name:    "NoDistClaims",
				idToken: `{"iss":"https://foo","aud":"client1"}`,
				config: Config{
					ClientID:        "client1",
					SkipExpiryCheck: true,
				},
				signKey: newRSAKey(t),
			},
			want: map[string]claimSource{},
		},
		{
			test: verificationTest{
				name: "1DistClaim",
				idToken: `{
							"iss":"https://foo","aud":"client1",
							"_claim_names": {
							    "address": "src1"
						 	},
						 	"_claim_sources": {
							    "src1": {"endpoint": "123", "access_token":"1234"}
							}
						  }`,
				config: Config{
					ClientID:        "client1",
					SkipExpiryCheck: true,
				},
				signKey: newRSAKey(t),
			},
			want: map[string]claimSource{
				"address": claimSource{Endpoint: "123", AccessToken: "1234"},
			},
		},
		{
			test: verificationTest{
				name: "2DistClaims1Src",
				idToken: `{
							"iss":"https://foo","aud":"client1",
							"_claim_names": {
							    "address": "src1",
							    "phone_number": "src1"
						 	},
						 	"_claim_sources": {
								"src1": {"endpoint": "123", "access_token":"1234"}
							}
						  }`,
				config: Config{
					ClientID:        "client1",
					SkipExpiryCheck: true,
				},
				signKey: newRSAKey(t),
			},
			want: map[string]claimSource{
				"address":      claimSource{Endpoint: "123", AccessToken: "1234"},
				"phone_number": claimSource{Endpoint: "123", AccessToken: "1234"},
			},
		},
		{
			test: verificationTest{
				name: "1Name0Src",
				idToken: `{
							"iss":"https://foo","aud":"client1",
							"_claim_names": {
								"address": "src1"
						 	},
							"_claim_sources": {
							}
						  }`,
				config: Config{
					ClientID:        "client1",
					SkipExpiryCheck: true,
				},
				signKey: newRSAKey(t),
			},
			wantErr: true,
		},
		{
			test: verificationTest{
				name: "NoNames1Src",
				idToken: `{
							"iss":"https://foo","aud":"client1",
							"_claim_names": {
						 	},
						 	"_claim_sources": {
								"src1": {"endpoint": "https://foo", "access_token":"1234"}
							}
						  }`,
				config: Config{
					ClientID:        "client1",
					SkipExpiryCheck: true,
				},
				signKey: newRSAKey(t),
			},
			want: map[string]claimSource{},
		},
	}
	for _, test := range tests {
		t.Run(test.test.name, func(t *testing.T) {
			idToken, err := test.test.runGetToken(t)
			if err != nil {
				if !test.wantErr {
					t.Errorf("parsing token: %v", err)
				}
				return
			}
			if test.wantErr {
				t.Errorf("expected error parsing token")
				return
			}
			if !reflect.DeepEqual(idToken.distributedClaims, test.want) {
				t.Errorf("expected distributed claim: %#v, got: %#v", test.want, idToken.distributedClaims)
			}
		})
	}
}

func TestDistClaimResolver(t *testing.T) {
	tests := []resolverTest{
		{
			name: "noAccessToken",
			payload: `{"iss":"https://foo","aud":"client1",
				"email":"janedoe@email.com",
				"shipping_address": {
					"street_address": "1234 Hollywood Blvd.",
    				"locality": "Los Angeles",
    				"region": "CA",
    				"postal_code": "90210",
    				"country": "US"}
				}`,
			config: Config{
				ClientID:        "client1",
				SkipExpiryCheck: true,
			},
			signKey: newRSAKey(t),
			issuer:  "https://foo",

			want: map[string]claimSource{},
		},
		{
			name: "rightAccessToken",
			payload: `{"iss":"https://foo","aud":"client1",
				"email":"janedoe@email.com",
				"shipping_address": {
					"street_address": "1234 Hollywood Blvd.",
    				"locality": "Los Angeles",
    				"region": "CA",
    				"postal_code": "90210",
    				"country": "US"}
				}`,
			config: Config{
				ClientID:        "client1",
				SkipExpiryCheck: true,
			},
			signKey:     newRSAKey(t),
			accessToken: "1234",
			issuer:      "https://foo",

			want: map[string]claimSource{},
		},
		{
			name: "wrongAccessToken",
			payload: `{"iss":"https://foo","aud":"client1",
				"email":"janedoe@email.com",
				"shipping_address": {
					"street_address": "1234 Hollywood Blvd.",
    				"locality": "Los Angeles",
    				"region": "CA",
    				"postal_code": "90210",
    				"country": "US"}
				}`,
			config: Config{
				ClientID:        "client1",
				SkipExpiryCheck: true,
			},
			signKey:     newRSAKey(t),
			accessToken: "12345",
			issuer:      "https://foo",
			wantErr:     true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims, err := test.testEndpoint(t)
			if err != nil {
				if !test.wantErr {
					t.Errorf("%v", err)
				}
				return
			}
			if test.wantErr {
				t.Errorf("expected error receiving response")
				return
			}
			if !reflect.DeepEqual(string(claims), test.payload) {
				t.Errorf("expected dist claim: %#v, got: %v", test.payload, string(claims))
			}
		})
	}

}

type resolverTest struct {
	// Name of the subtest.
	name string

	// issuer will be the endpoint server url
	issuer string

	// just the payload
	payload string

	// Key to sign the ID Token with.
	signKey *signingKey

	// If not provided defaults to signKey. Only useful when
	// testing invalid signatures.
	verificationKey *signingKey

	config  Config
	wantErr bool
	want    map[string]claimSource

	//this is the access token that the testEndpoint will accept
	accessToken string
}

func (v resolverTest) testEndpoint(t *testing.T) ([]byte, error) {
	token := v.signKey.sign(t, []byte(v.payload))

	ctx := t.Context()

	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if got != "" && got != "Bearer 1234" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, token)
	}))
	defer s.Close()

	issuer := v.issuer
	var ks KeySet
	if v.verificationKey == nil {
		ks = &StaticKeySet{PublicKeys: []crypto.PublicKey{v.signKey.pub}}
	} else {
		ks = &StaticKeySet{PublicKeys: []crypto.PublicKey{v.verificationKey.pub}}
	}
	verifier := NewVerifier(issuer, ks, &v.config)

	ctx = ClientContext(ctx, s.Client())

	src := claimSource{
		Endpoint:    s.URL + "/",
		AccessToken: v.accessToken,
	}
	return resolveDistributedClaim(ctx, verifier, src)
}

type verificationTest struct {
	// Name of the subtest.
	name string

	// If not provided defaults to "https://foo"
	issuer string

	// JWT payload (just the claims).
	idToken string

	// Key to sign the ID Token with.
	signKey *signingKey
	// If not provided defaults to signKey. Only useful when
	// testing invalid signatures.
	verificationKey *signingKey

	config        Config
	wantErr       bool
	wantErrExpiry bool
}

func (v verificationTest) runGetToken(t *testing.T) (*IDToken, error) {
	var token string
	if v.signKey != nil {
		token = v.signKey.sign(t, []byte(v.idToken))
	} else {
		// "none" still uses a second "." character, but "...MUST use the empty octet
		// sequence as its JWS Signature value."
		//
		// https://datatracker.ietf.org/doc/html/rfc7518#section-3.6
		token = base64.RawURLEncoding.EncodeToString([]byte(`{"alg": "none"}`))
		token += "."
		token += base64.RawURLEncoding.EncodeToString([]byte(v.idToken))
		token += "."
	}

	ctx := t.Context()

	issuer := "https://foo"
	if v.issuer != "" {
		issuer = v.issuer
	}
	var ks KeySet
	if v.verificationKey != nil {
		ks = &StaticKeySet{PublicKeys: []crypto.PublicKey{v.verificationKey.pub}}
	} else if v.signKey != nil {
		ks = &StaticKeySet{PublicKeys: []crypto.PublicKey{v.signKey.pub}}
	}
	verifier := NewVerifier(issuer, ks, &v.config)

	return verifier.Verify(ctx, token)
}

func (v verificationTest) run(t *testing.T) {
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

// Fixed aggregated-claims JWTs signed by two independent claims providers.
const aggregatedClaimsTokenA = "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJodHRwczovL2lzc3Vlci1hLmV4YW1wbGUiLCJzdWIiOiJhdHRhY2tlci1jb250cm9sbGVkLXN1YiIsImVkdWNhdGlvbiI6IlRzaW5naHVhIFVuaXZlcnNpdHkiLCJkZWdyZWUiOiJNU2MgQ29tcHV0ZXIgU2NpZW5jZSIsImFkbWluIjp0cnVlfQ.PckSqLFvhbBE9qWlsRgyBuQ0d9AFNOJjq06ID8ZJ5hLl4f7UWq90Fr5ZkWiT-cPVthHFWX-bcwCqSHqdCWOewasDYqeenVrTj5MR_X5rouejJoLynNDtsSa3KY0R5dxZ3MPYWMPhzmWXPBXK0Go2nYXl_m5x6Z7gbwmNZC3AamLIIqa0_DMdzZi6bFTkQuwKp4iV8nkCr8qH6YYPiQWw9zcBRIsx_7dh_S9K6Oz5_LhQaJ8Y2BOJCgVk2i-3KIVuWcvdGWZQyy_DmLECk4mihBN3wbmHCvGAp8gJCCWtblZshAWU8g4DpZVhoXIt5d19eMmaWxvlWCtmCeFsB35atA"

const aggregatedClaimsTokenB = "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJodHRwczovL2lzc3Vlci1iLmV4YW1wbGUiLCJzdWIiOiJhbm90aGVyLXN1YiIsImVtcGxveWVyIjoiQWNtZSBDb3JwIn0.HOiQV8NHwRtnOVCh9Jgv_fVMb_ExHuZA7en17tO033L_qdishkG4ohJ9rPob_J1_b8sjjJfUNmYNu1Z5uDehy3ZXPj0AYAfmnrwiW1EwSfHmGQZ_ObUdMHkFcWwCN_b3PzWsQbMX58aId0KaagC1LuYNv75MwGvM3sXVI0diwuQ7qm4m45DqrHbmi45KE4x_zoNefqYTsdMz9HYwcMDeb9LBaSf9OW0LA_xe0EQFk5kt1gUTYpW_LqjlMYBD_DweufwKA8KTxaX5LSVhAa0Y7MCPm98K_UfkQsauQvQ5xWxn1hLyhuXK0-SK-lLmqpd-QCi6u_ieyzrhy00VUJtqOg"

const aggregatedClaimsPubKeyA = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAtzLKjf2VJOsVbVjsoWf6
kDpPyRRMCxsNuzm3+/mv5JJjBCtNaB8iI+xZqxYtMdAjxXR0JTUU/TeJ06nk1ufz
odoxvFp4QvHFWH2SGGMH1x/inOyHSSg1SJWXqlKYt/AOflCOXVAsC+tY6zSP+1nn
tsG/uL1Q7tHqsE7NAsE01xhsPTwKDsFJWI0WktKvbAOKaTQh+/GRbk5+osEA8QbY
n2Aziofpf19Kv1RNeRVeM+COEg0PwdbpoyroZ8GZV0QAXt7Y2DJeuHAx1HgydC83
HZZ72eqvUSl4NbLHMv+zAugLqtvF0VAPFld3vH3C1VR5iPtpC1RamHU6w/y1RMvo
2QIDAQAB
-----END PUBLIC KEY-----
`

const aggregatedClaimsPubKeyB = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA6wVspRuK95ZLNSksOr/W
l3bQe/VIj5aNFGFBevZ9bYrx41rBF9rbMr8QPHlgFUFU+rSXefjb4rpaPpPfFWQX
6a2OG7R2rbsKGC6wsW+7P1ksc+l5+BFcG8soQFdC+12NbHdBaHa7cRxFsXoh8vrP
/QPF9skUT1fHBi/pb2qwmjJRL3hyF18reh4sZ5nJmvD5JJldPr9FB1yKRSeNEQfF
/jszPj2hNA+T1a2hVbYBANaOqj943fcyRBSczuB/ee7k+EuV8o06XFBxHPLvJgIa
GXsfY4MFBcmq/bZVC/PVItzcayTp6ebhkdYLDvUE3Y7lxKpZIqZ3F0Jj5qu2QzWJ
cQIDAQAB
-----END PUBLIC KEY-----
`

func parsePublicKey(t *testing.T, pemKey string) crypto.PublicKey {
	t.Helper()
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		t.Fatalf("failed to decode PEM block")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse public key: %v", err)
	}
	return key
}

// countingKeySet records how often VerifySignature was called.
type countingKeySet struct {
	KeySet
	calls int
}

func (k *countingKeySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	k.calls++
	return k.KeySet.VerifySignature(ctx, jwt)
}

func TestAggregatedClaims(t *testing.T) {
	// Two JWT claim sources from two issuers. src1 is referenced by both
	// "education" and "degree", src2 by "employer". Both source tokens carry
	// their own "sub" (and src1 an extra "admin" claim) which must not leak
	// into the ID token.
	idToken := `{
		"iss": "https://foo", "aud": "client1", "sub": "user-123",
		"_claim_names": {
			"education": "src1",
			"degree": "src1",
			"employer": "src2"
		},
		"_claim_sources": {
			"src1": {"JWT": "` + aggregatedClaimsTokenA + `"},
			"src2": {"JWT": "` + aggregatedClaimsTokenB + `"}
		}
	}`

	keysetA := &countingKeySet{KeySet: &StaticKeySet{PublicKeys: []crypto.PublicKey{
		parsePublicKey(t, aggregatedClaimsPubKeyA),
	}}}
	keysetB := &countingKeySet{KeySet: &StaticKeySet{PublicKeys: []crypto.PublicKey{
		parsePublicKey(t, aggregatedClaimsPubKeyB),
	}}}

	vt := verificationTest{
		name:    "twoJWTClaimSources",
		idToken: idToken,
		config: Config{
			ClientID:        "client1",
			SkipExpiryCheck: true,
			ClaimSourceVerifiers: map[string]*IDTokenVerifier{
				"src1": NewVerifier("https://issuer-a.example", keysetA, &Config{
					SkipClientIDCheck: true,
					SkipExpiryCheck:   true,
				}),
				"src2": NewVerifier("https://issuer-b.example", keysetB, &Config{
					SkipClientIDCheck: true,
					SkipExpiryCheck:   true,
				}),
			},
		},
		signKey: newRSAKey(t),
	}

	tok, err := vt.runGetToken(t)
	if err != nil {
		t.Fatalf("verifying token: %v", err)
	}

	// src1 is referenced by two claim names but must be verified only once.
	if keysetA.calls != 1 {
		t.Errorf("expected src1 to be verified once, got %d verifications", keysetA.calls)
	}
	if keysetB.calls != 1 {
		t.Errorf("expected src2 to be verified once, got %d verifications", keysetB.calls)
	}

	// Merged claims contain the explicitly named fields from both issuers,
	// while the ID token keeps its own iss/sub and never gets the sources'
	// extra claims.
	var claims map[string]json.RawMessage
	if err := tok.Claims(&claims); err != nil {
		t.Fatalf("unmarshaling merged claims: %v", err)
	}
	wantClaims := map[string]string{
		"education": `"Tsinghua University"`,
		"degree":    `"MSc Computer Science"`,
		"employer":  `"Acme Corp"`,
		"sub":       `"user-123"`,
		"iss":       `"https://foo"`,
	}
	for name, want := range wantClaims {
		if got := string(claims[name]); got != want {
			t.Errorf("claim %q = %q, want %s", name, got, want)
		}
	}
	if _, ok := claims["admin"]; ok {
		t.Errorf("non-referenced claim %q from src1 leaked into the ID token", "admin")
	}

	// Structured claims unmarshal into user structs as well.
	var merged struct {
		Subject   string `json:"sub"`
		Issuer    string `json:"iss"`
		Education string `json:"education"`
		Degree    string `json:"degree"`
		Employer  string `json:"employer"`
		Admin     bool   `json:"admin"`
	}
	if err := tok.Claims(&merged); err != nil {
		t.Fatalf("unmarshaling merged claims into struct: %v", err)
	}
	if merged.Subject != "user-123" || merged.Issuer != "https://foo" {
		t.Errorf("ID token iss/sub overridden by source claims: %+v", merged)
	}
	if merged.Admin {
		t.Errorf("non-referenced %q claim merged into the ID token", "admin")
	}
	if merged.Education != "Tsinghua University" ||
		merged.Degree != "MSc Computer Science" ||
		merged.Employer != "Acme Corp" {
		t.Errorf("unexpected merged claims: %+v", merged)
	}
}

func TestAggregatedClaimsErrors(t *testing.T) {
	srcConfig := &Config{SkipClientIDCheck: true, SkipExpiryCheck: true}
	verifierA := NewVerifier("https://issuer-a.example", &StaticKeySet{
		PublicKeys: []crypto.PublicKey{parsePublicKey(t, aggregatedClaimsPubKeyA)},
	}, srcConfig)
	// src1 is signed by issuer A; verifying it with issuer B's keys fails.
	verifierWrongKey := NewVerifier("https://issuer-b.example", &StaticKeySet{
		PublicKeys: []crypto.PublicKey{parsePublicKey(t, aggregatedClaimsPubKeyB)},
	}, srcConfig)

	tests := []struct {
		name       string
		idToken    string
		verifiers  map[string]*IDTokenVerifier
		wantSource string
		wantClaims []string
	}{
		{
			name: "untrustedSource",
			idToken: `{
				"iss": "https://foo", "aud": "client1",
				"_claim_names": {"education": "src1"},
				"_claim_sources": {"src1": {"JWT": "` + aggregatedClaimsTokenA + `"}}
			}`,
			wantSource: "src1",
			wantClaims: []string{"education"},
		},
		{
			name: "verificationFailureListsAffectedClaims",
			idToken: `{
				"iss": "https://foo", "aud": "client1",
				"_claim_names": {
					"education": "src1",
					"degree": "src1"
				},
				"_claim_sources": {"src1": {"JWT": "` + aggregatedClaimsTokenA + `"}}
			}`,
			verifiers:  map[string]*IDTokenVerifier{"src1": verifierWrongKey},
			wantSource: "src1",
			wantClaims: []string{"degree", "education"},
		},
		{
			name: "missingNamedClaim",
			idToken: `{
				"iss": "https://foo", "aud": "client1",
				"_claim_names": {"email": "src1"},
				"_claim_sources": {"src1": {"JWT": "` + aggregatedClaimsTokenA + `"}}
			}`,
			verifiers:  map[string]*IDTokenVerifier{"src1": verifierA},
			wantSource: "src1",
			wantClaims: []string{"email"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			vt := verificationTest{
				name:    test.name,
				idToken: test.idToken,
				config: Config{
					ClientID:             "client1",
					SkipExpiryCheck:      true,
					ClaimSourceVerifiers: test.verifiers,
				},
				signKey: newRSAKey(t),
			}
			_, err := vt.runGetToken(t)
			if err == nil {
				t.Fatal("expected error resolving JWT claim source")
			}

			var srcErr *ClaimSourceError
			if !errors.As(err, &srcErr) {
				t.Fatalf("expected *ClaimSourceError, got %v", err)
			}
			if srcErr.Source != test.wantSource {
				t.Errorf("source = %q, want %q", srcErr.Source, test.wantSource)
			}
			if !reflect.DeepEqual(srcErr.Claims, test.wantClaims) {
				t.Errorf("affected claims = %q, want %q", srcErr.Claims, test.wantClaims)
			}
		})
	}
}
