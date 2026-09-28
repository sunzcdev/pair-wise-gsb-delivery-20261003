package oidc

import (
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
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

// TestAggregatedClaims verifies support for "_claim_sources" entries that
// carry an aggregated claims JWT. Two independent issuers back two sources:
//
//   - "https://university.example" issues the "degree" claim.
//   - "https://employer.example" issues the "job_title" and "department"
//     claims, proving a source referenced by multiple claims is verified
//     once while failures still name every affected claim.
func TestAggregatedClaims(t *testing.T) {
	ctx := t.Context()

	idpKey := newRSAKey(t)
	uniKey := newRSAKey(t)
	employerKey := newRSAKey(t)

	const (
		idpIssuer      = "https://foo"
		uniIssuer      = "https://university.example"
		employerIssuer = "https://employer.example"
	)

	uniVerifier := NewVerifier(uniIssuer, &StaticKeySet{PublicKeys: []crypto.PublicKey{uniKey.pub}}, &Config{
		ClientID:        "client1",
		SkipExpiryCheck: true,
	})
	employerVerifier := NewVerifier(employerIssuer, &StaticKeySet{PublicKeys: []crypto.PublicKey{employerKey.pub}}, &Config{
		ClientID:        "client1",
		SkipExpiryCheck: true,
	})

	// University JWT: only "degree" is referenced. The extra "gpa", "iss",
	// and "sub" members must not be merged into the main token.
	uniToken := uniKey.sign(t, []byte(`{
		"iss": "https://university.example",
		"aud": "client1",
		"sub": "student-123",
		"degree": {"type": "BachelorDegree", "name": "B.Sc. Computer Science"},
		"gpa": 3.9
	}`))

	// Employer JWT: both "job_title" and "department" reference this source.
	employerToken := employerKey.sign(t, []byte(`{
		"iss": "https://employer.example",
		"aud": "client1",
		"sub": "employee-456",
		"job_title": "Software Engineer",
		"department": "Platform"
	}`))

	idTokenPayload := fmt.Sprintf(`{
		"iss": "https://foo",
		"aud": "client1",
		"sub": "user-789",
		"_claim_names": {
			"degree": "uni",
			"job_title": "employer",
			"department": "employer"
		},
		"_claim_sources": {
			"uni": {"JWT": %q},
			"employer": {"JWT": %q}
		}
	}`, uniToken, employerToken)

	rawIDToken := idpKey.sign(t, []byte(idTokenPayload))

	config := &Config{
		ClientID:        "client1",
		SkipExpiryCheck: true,
		ClaimSourceVerifiers: map[string]*IDTokenVerifier{
			"uni":      uniVerifier,
			"employer": employerVerifier,
		},
	}
	verifier := NewVerifier(idpIssuer, &StaticKeySet{PublicKeys: []crypto.PublicKey{idpKey.pub}}, config)

	token, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		t.Fatalf("verifying token with aggregated claims: %v", err)
	}

	// Both issuers' referenced claims are merged into the main token.
	var merged struct {
		Subject string `json:"sub"`
		Degree  struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"degree"`
		JobTitle   string   `json:"job_title"`
		Department string   `json:"department"`
		GPA        *float64 `json:"gpa"`
	}
	if err := token.Claims(&merged); err != nil {
		t.Fatalf("unmarshaling merged claims: %v", err)
	}
	if merged.Subject != "user-789" {
		t.Errorf("main token subject not preserved, got %q", merged.Subject)
	}
	if merged.Degree.Type != "BachelorDegree" || merged.Degree.Name != "B.Sc. Computer Science" {
		t.Errorf("degree claim not merged, got %#v", merged.Degree)
	}
	if merged.JobTitle != "Software Engineer" {
		t.Errorf("job_title claim not merged, got %q", merged.JobTitle)
	}
	if merged.Department != "Platform" {
		t.Errorf("department claim not merged, got %q", merged.Department)
	}
	if merged.GPA != nil {
		t.Errorf("unreferenced claim from aggregated JWT must not be merged, got gpa=%v", *merged.GPA)
	}

	var rawMerged map[string]json.RawMessage
	if err := json.Unmarshal(token.claims, &rawMerged); err != nil {
		t.Fatalf("unmarshaling raw merged claims: %v", err)
	}
	if _, ok := rawMerged["iss"]; !ok {
		t.Errorf("main token issuer claim missing")
	}
	if got := string(rawMerged["iss"]); got != `"https://foo"` {
		t.Errorf("main token issuer must not be overwritten by an aggregated JWT, got %s", got)
	}

	// The endpoint-based source metadata is still exposed for lazy resolution.
	if !reflect.DeepEqual(token.distributedClaims, map[string]claimSource{
		"degree":     {JWT: uniToken},
		"job_title":  {JWT: employerToken},
		"department": {JWT: employerToken},
	}) {
		t.Errorf("unexpected distributed claim sources: %#v", token.distributedClaims)
	}
}

// countingKeySet records how many times VerifySignature is invoked.
type countingKeySet struct {
	inner KeySet
	calls int
}

func (c *countingKeySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	c.calls++
	return c.inner.VerifySignature(ctx, jwt)
}

func TestAggregatedClaimsVerifiedOncePerSource(t *testing.T) {
	ctx := t.Context()

	idpKey := newRSAKey(t)
	employerKey := newRSAKey(t)

	const employerIssuer = "https://employer.example"

	employerKeySet := &countingKeySet{inner: &StaticKeySet{PublicKeys: []crypto.PublicKey{employerKey.pub}}}
	employerVerifier := NewVerifier(employerIssuer, employerKeySet, &Config{
		ClientID:        "client1",
		SkipExpiryCheck: true,
	})

	employerToken := employerKey.sign(t, []byte(`{
		"iss": "https://employer.example",
		"aud": "client1",
		"sub": "employee-456",
		"job_title": "Software Engineer",
		"department": "Platform"
	}`))

	// Three claim names reference the same employer source.
	idTokenPayload := fmt.Sprintf(`{
		"iss": "https://foo",
		"aud": "client1",
		"_claim_names": {
			"job_title": "employer",
			"department": "employer",
			"team": "employer"
		},
		"_claim_sources": {
			"employer": {"JWT": %q}
		}
	}`, employerToken)

	// "team" is intentionally absent from the JWT: verification itself runs
	// once, then the missing referenced claim fails the merge.
	verifier := NewVerifier("https://foo", &StaticKeySet{PublicKeys: []crypto.PublicKey{idpKey.pub}}, &Config{
		ClientID:        "client1",
		SkipExpiryCheck: true,
		ClaimSourceVerifiers: map[string]*IDTokenVerifier{
			"employer": employerVerifier,
		},
	})

	_, err := verifier.Verify(ctx, idpKey.sign(t, []byte(idTokenPayload)))
	if err == nil {
		t.Fatalf("expected error for missing referenced claim")
	}
	if !strings.Contains(err.Error(), `aggregated claims source "employer" does not contain claim "team"`) {
		t.Fatalf("unexpected error: %v", err)
	}
	if employerKeySet.calls != 1 {
		t.Fatalf("expected source to be verified once despite 3 references, got %d verifications", employerKeySet.calls)
	}
}

func TestAggregatedClaimsErrors(t *testing.T) {
	ctx := t.Context()

	idpKey := newRSAKey(t)
	uniKey := newRSAKey(t)
	otherKey := newRSAKey(t)

	const (
		idpIssuer = "https://foo"
		uniIssuer = "https://university.example"
	)

	uniVerifier := NewVerifier(uniIssuer, &StaticKeySet{PublicKeys: []crypto.PublicKey{uniKey.pub}}, &Config{
		ClientID:        "client1",
		SkipExpiryCheck: true,
	})

	uniToken := func(t *testing.T, payload string) string {
		return uniKey.sign(t, []byte(payload))
	}

	tests := []struct {
		name      string
		payload   func(t *testing.T) string
		verifiers map[string]*IDTokenVerifier
		wantErr   string
	}{
		{
			name: "noVerifier",
			payload: func(t *testing.T) string {
				agg := uniToken(t, `{
					"iss": "https://university.example",
					"aud": "client1",
					"degree": {"name": "B.Sc."}
				}`)
				return fmt.Sprintf(`{
					"iss": "https://foo",
					"aud": "client1",
					"_claim_names": {"degree": "uni"},
					"_claim_sources": {"uni": {"JWT": %q}}
				}`, agg)
			},
			verifiers: map[string]*IDTokenVerifier(nil),
			wantErr:   `no verifier configured for aggregated claims source "uni" (claims degree)`,
		},
		{
			name: "untrustedIssuer",
			payload: func(t *testing.T) string {
				agg := uniToken(t, `{
					"iss": "https://evil.example",
					"aud": "client1",
					"degree": {"name": "B.Sc."}
				}`)
				return fmt.Sprintf(`{
					"iss": "https://foo",
					"aud": "client1",
					"_claim_names": {"degree": "uni"},
					"_claim_sources": {"uni": {"JWT": %q}}
				}`, agg)
			},
			verifiers: map[string]*IDTokenVerifier{"uni": uniVerifier},
			wantErr:   `failed to verify aggregated claims source "uni" (claims degree)`,
		},
		{
			name: "badSignatureAffectsMultipleClaims",
			payload: func(t *testing.T) string {
				agg := otherKey.sign(t, []byte(`{
					"iss": "https://university.example",
					"aud": "client1",
					"job_title": "Engineer",
					"department": "Platform"
				}`))
				return fmt.Sprintf(`{
					"iss": "https://foo",
					"aud": "client1",
					"_claim_names": {"job_title": "employer", "department": "employer"},
					"_claim_sources": {"employer": {"JWT": %q}}
				}`, agg)
			},
			verifiers: map[string]*IDTokenVerifier{"employer": uniVerifier},
			wantErr:   `failed to verify aggregated claims source "employer" (claims department, job_title)`,
		},
		{
			name: "referencedClaimMissing",
			payload: func(t *testing.T) string {
				agg := uniToken(t, `{
					"iss": "https://university.example",
					"aud": "client1",
					"gpa": 3.9
				}`)
				return fmt.Sprintf(`{
					"iss": "https://foo",
					"aud": "client1",
					"_claim_names": {"degree": "uni"},
					"_claim_sources": {"uni": {"JWT": %q}}
				}`, agg)
			},
			verifiers: map[string]*IDTokenVerifier{"uni": uniVerifier},
			wantErr:   `aggregated claims source "uni" does not contain claim "degree"`,
		},
		{
			name: "unreferencedSourceIgnored",
			payload: func(t *testing.T) string {
				// "unused" has no verifier, but nothing in "_claim_names"
				// points at it, so it must be ignored.
				agg := uniToken(t, `{
					"iss": "https://university.example",
					"aud": "client1",
					"degree": {"name": "B.Sc."}
				}`)
				return fmt.Sprintf(`{
					"iss": "https://foo",
					"aud": "client1",
					"_claim_names": {"degree": "uni"},
					"_claim_sources": {
						"uni": {"JWT": %q},
						"unused": {"JWT": "not-a-jwt"}
					}
				}`, agg)
			},
			verifiers: map[string]*IDTokenVerifier{"uni": uniVerifier},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rawIDToken := idpKey.sign(t, []byte(test.payload(t)))
			verifier := NewVerifier(idpIssuer, &StaticKeySet{PublicKeys: []crypto.PublicKey{idpKey.pub}}, &Config{
				ClientID:             "client1",
				SkipExpiryCheck:      true,
				ClaimSourceVerifiers: test.verifiers,
			})
			_, err := verifier.Verify(ctx, rawIDToken)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got none", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected error containing %q, got %q", test.wantErr, err.Error())
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
