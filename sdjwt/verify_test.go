package sdjwt

import (
	"crypto"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/json"
)

// TestRFC9901FixedVectors checks the SHA-256 digest calculation against the
// fixed Disclosure objects from RFC 9901 Appendix A.2 (array element) and
// Appendix A.3 (object property). The expected digests were independently
// computed from the base64url-encoded JSON with a second SHA-256
// implementation (.NET) and agree here.
func TestRFC9901FixedVectors(t *testing.T) {
	vector := func(name, rawDisclosure, expectedDigest string) {
		t.Helper()
		d, err := parseDisclosure(rawDisclosure)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		got, err := digestOf(d.rawJSON, crypto.SHA256, 32)
		if err != nil {
			t.Fatalf("%s: digest: %v", name, err)
		}
		if got != expectedDigest {
			t.Fatalf("%s: digest mismatch:\n got %s\nwant %s", name, got, expectedDigest)
		}
	}

	// RFC 9901 Appendix A.2:
	// ["FklWxmutFMLH_s_n_E9kg","given_name","Maximilian"]
	vector("A.2 object property",
		"WyJGS2xXeG11dFVNTEhfc19uX0U5a2ciLCAiZ2l2ZW5fbmFtZSIsICJNYXhpbWlsaWFuIl0",
		"68yknB0vTjpEK9dwUrsPkFIQWfnAGhbxjWnmExOTYTc")

	// RFC 9901 Appendix A.3:
	// ["2sIj_HEIKNTe_QiXpaIW4VcIE","Patch Reference.party_name","Quayton"]
	vector("A.3 object property",
		"WyIyc0lqX0hFSUtOVGVfUWlYcGFJVzRWY0lFIiwiUGF0Y2ggUmVmZXJlbmNlLnBhcnR5X25hbWUiLCJRdWF5dG9uIl0",
		"_1v652POdcC7Vo4f4_T4zaAxyq4xd2L1CHcu6iOrDVg")
}

func TestVerifyPartialDisclosure(t *testing.T) {
	keys := generateTestKeys(t)
	cred := buildCredential(t, keys)

	// Selective presentation: given_name, nationality DE, and only the
	// locality inside the nested address object.
	labels := []string{"given_name", "nat-DE", "locality"}
	combined := cred.presentation(t, labels, "")

	out := parseAndVerify(t, combined, keys.issuerKey, nil)

	if out.KeyBindingVerified {
		t.Fatalf("unexpected key binding")
	}

	if got := out.Claims["given_name"]; got != "Maximilian" {
		t.Fatalf("given_name = %v, want Maximilian", got)
	}
	if _, present := out.Claims["family_name"]; present {
		t.Fatalf("family_name must not be disclosed")
	}
	if _, present := out.Claims["birthdate"]; present {
		t.Fatalf("birthdate must not be disclosed")
	}

	addr, ok := out.Claims["address"].(map[string]interface{})
	if !ok {
		t.Fatalf("address missing or wrong type: %T", out.Claims["address"])
	}
	if got := addr["country"]; got != "DE" {
		t.Fatalf("address.country = %v, want DE", got)
	}
	if got := addr["locality"]; got != "Köln" {
		t.Fatalf("address.locality = %v, want Köln", got)
	}
	if _, present := addr["street_address"]; present {
		t.Fatalf("street_address must not be disclosed")
	}

	nats, ok := out.Claims["nationalities"].([]interface{})
	if !ok || len(nats) != 2 {
		t.Fatalf("nationalities missing or wrong type: %#v", out.Claims["nationalities"])
	}
	if got := nats[0]; got != "DE" {
		t.Fatalf("nationalities[0] = %v, want DE", got)
	}
	if _, ok := nats[1].(UndisclosedArrayElement); !ok {
		t.Fatalf("nationalities[1] = %#v, want UndisclosedArrayElement", nats[1])
	}

	// The undisclosed digests must be reported with JSON pointers so the
	// caller can distinguish "not disclosed" from "never asserted".
	pointers := map[string]int{}
	for _, u := range out.Undisclosed {
		pointers[u.Pointer]++
	}
	wantPointers := map[string]int{
		"":                 2, // family_name, birthdate
		"/address":         1, // street_address
		"/nationalities/1": 1, // nat-US
	}
	if len(pointers) != len(wantPointers) {
		t.Fatalf("undisclosed pointers = %v, want %v", pointers, wantPointers)
	}
	for p, n := range wantPointers {
		if pointers[p] != n {
			t.Fatalf("undisclosed at %q = %d, want %d (all: %v)", p, pointers[p], n, pointers)
		}
	}

	if len(out.Disclosed) != 3 {
		t.Fatalf("disclosed count = %d, want 3", len(out.Disclosed))
	}

	// No SD-JWT structural markers may survive into the output claims.
	encoded, err := json.Marshal(out.Claims)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, marker := range []string{"_sd", "_sd_alg"} {
		if strings.Contains(string(encoded), marker) {
			t.Fatalf("output claims still contain %q: %s", marker, encoded)
		}
	}

	// The elided array element renders as the ["..."] marker.
	elEncoded, err := json.Marshal(UndisclosedArrayElement{})
	if err != nil || string(elEncoded) != `["..."]` {
		t.Fatalf("UndisclosedArrayElement marshals to %s, %v", elEncoded, err)
	}
}

func TestDisclosedClaimDoesNotOverrideVisibleClaim(t *testing.T) {
	keys := generateTestKeys(t)

	evil := encodeDisclosure(t, "evil-salt", "sub", "attacker-controlled")
	claims := map[string]interface{}{
		"iss": "https://example.com/issuer",
		"sub": "legitimate-user",
		"cnf": map[string]interface{}{"jwk": publicJWKMap(t, keys.holderKey)},
		"_sd": []string{disclosureDigest(t, evil)},
	}
	compact := signCompact(t, keys.issuerKey, claims, "")
	combined := compact + "~" + evil + "~"

	parseExpectVerifyError(t, combined, keys.issuerKey, nil, ErrDuplicateClaimName)

	// The originally visible claim must remain untouched on a rejected
	// presentation; verify a clean presentation still returns it.
	clean := compact + "~"
	out := parseAndVerify(t, clean, keys.issuerKey, nil)
	if out.Claims["sub"] != "legitimate-user" {
		t.Fatalf("visible sub claim = %v", out.Claims["sub"])
	}
}

func TestDuplicateDisclosureRejected(t *testing.T) {
	keys := generateTestKeys(t)
	cred := buildCredential(t, keys)

	// Same raw disclosure transmitted twice: rejected at parse time.
	t.Run("identical raw disclosure twice", func(t *testing.T) {
		combined := cred.issuerCompact + "~" + cred.disclosures["given_name"] +
			"~" + cred.disclosures["given_name"] + "~"
		if _, err := Parse(combined, []jose.SignatureAlgorithm{jose.ES256}); err == nil {
			t.Fatalf("expected parse error for duplicate disclosure")
		}
	})

	// Same digest referenced by two "_sd" entries in the issuer claims.
	t.Run("digest referenced twice in claims", func(t *testing.T) {
		d := encodeDisclosure(t, "twice-salt", "given_name", "Max")
		digest := disclosureDigest(t, d)
		claims := map[string]interface{}{
			"iss": "i",
			"_sd": []string{digest, digest},
		}
		combined := signCompact(t, keys.issuerKey, claims, "") + "~" + d + "~"
		parseExpectVerifyError(t, combined, keys.issuerKey, nil, ErrDuplicateDigest)
	})

	// Same digest referenced once at the top level and once inside a nested
	// object; the second use during processing must be rejected.
	t.Run("digest shared across objects", func(t *testing.T) {
		d := encodeDisclosure(t, "shared-salt", "nickname", "Max")
		digest := disclosureDigest(t, d)
		claims := map[string]interface{}{
			"iss": "i",
			"_sd": []string{digest},
			"nested": map[string]interface{}{
				"_sd": []string{digest},
			},
		}
		combined := signCompact(t, keys.issuerKey, claims, "") + "~" + d + "~"
		parseExpectVerifyError(t, combined, keys.issuerKey, nil, ErrDuplicateDigest)
	})

}

// TestForgedNestingRejected covers attacks where the presentation tries to
// attach disclosures that the issuer structure does not authorize.
func TestForgedNestingRejected(t *testing.T) {
	keys := generateTestKeys(t)

	// An orphan disclosure that has no matching digest anywhere: a holder
	// cannot smuggle an arbitrary claim into the verified claims.
	t.Run("orphan disclosure has no digest", func(t *testing.T) {
		cred := buildCredential(t, keys)
		extra := encodeDisclosure(t, "forged-salt", "is_admin", true)
		combined := cred.presentation(t, []string{"given_name"}, "") +
			extra + "~"
		parseExpectVerifyError(t, combined, keys.issuerKey, nil, ErrUnusedDisclosure)
	})

	// A 2-element (array element) disclosure placed behind a 3-element
	// object digest, or vice versa: disclosure kind cannot be forged to
	// match a slot it was not issued for.
	t.Run("array disclosure matched against object digest", func(t *testing.T) {
		arrDisc := encodeDisclosure(t, "arr-salt", "injected-value")
		claims := map[string]interface{}{
			"iss": "i",
			"_sd": []string{disclosureDigest(t, arrDisc)},
		}
		combined := signCompact(t, keys.issuerKey, claims, "") + "~" + arrDisc + "~"
		parseExpectVerifyError(t, combined, keys.issuerKey, nil, ErrMalformedClaims)
	})

	t.Run("object disclosure matched against array digest", func(t *testing.T) {
		objDisc := encodeDisclosure(t, "obj-salt", "injected", "value")
		claims := map[string]interface{}{
			"iss": "i",
			"items": []interface{}{
				map[string]interface{}{"...": disclosureDigest(t, objDisc)},
			},
		}
		combined := signCompact(t, keys.issuerKey, claims, "") + "~" + objDisc + "~"
		parseExpectVerifyError(t, combined, keys.issuerKey, nil, ErrMalformedClaims)
	})

	// The holder cannot add a top-level "_sd_alg" to a nested object.
	t.Run("nested _sd_alg rejected", func(t *testing.T) {
		claims := map[string]interface{}{
			"iss": "i",
			"nested": map[string]interface{}{
				"_sd_alg": SHA256,
			},
		}
		combined := signCompact(t, keys.issuerKey, claims, "") + "~"
		parseExpectVerifyError(t, combined, keys.issuerKey, nil, ErrMalformedClaims)
	})

	// A malformed array marker (bare "..." string) cannot be used to hide
	// arbitrary issuer content from the verifier.
	t.Run("bare array marker rejected", func(t *testing.T) {
		claims := map[string]interface{}{
			"iss":   "i",
			"items": []interface{}{"..."},
		}
		combined := signCompact(t, keys.issuerKey, claims, "") + "~"
		parseExpectVerifyError(t, combined, keys.issuerKey, nil, ErrMalformedClaims)
	})

	// The ["..."] output marker is not valid inside issuer-signed claims;
	// issuer claims use {"...": "<digest>"} objects.
	t.Run("output-side array marker in issuer claims rejected", func(t *testing.T) {
		claims := map[string]interface{}{
			"iss":   "i",
			"items": []interface{}{[]interface{}{"..."}},
		}
		combined := signCompact(t, keys.issuerKey, claims, "") + "~"
		parseExpectVerifyError(t, combined, keys.issuerKey, nil, ErrMalformedClaims)
	})
}

func TestKeyBinding(t *testing.T) {
	keys := generateTestKeys(t)
	cred := buildCredential(t, keys)

	now := time.Now().Add(-time.Second)
	expected := &KeyBindingExpected{
		Nonce:    "verifier-nonce",
		Audience: "https://verifier.example",
		Now:      now.Add(time.Second),
		MaxAge:   time.Hour,
	}

	pres := cred.presentation(t, []string{"given_name", "nat-DE"}, "")
	kb := cred.kbJWT(t, keys, pres, expected.Nonce, expected.Audience, now)

	t.Run("valid presentation", func(t *testing.T) {
		out := parseAndVerify(t, pres+kb, keys.issuerKey, expected)
		if !out.KeyBindingVerified {
			t.Fatalf("key binding not verified")
		}
	})

	t.Run("replayed to another nonce", func(t *testing.T) {
		otherExpected := *expected
		otherExpected.Nonce = "different-verifier-nonce"
		parseExpectVerifyError(t, pres+kb, keys.issuerKey, &otherExpected, ErrKeyBinding)
	})

	t.Run("presentation rebound onto a different disclosure set", func(t *testing.T) {
		// The same KB-JWT attached to a different SD-JWT presentation must
		// fail the sd_hash comparison.
		otherPres := cred.presentation(t, []string{"given_name", "family_name"}, "")
		parseExpectVerifyError(t, otherPres+kb, keys.issuerKey, expected, ErrKeyBinding)
	})

	t.Run("wrong audience", func(t *testing.T) {
		otherExpected := *expected
		otherExpected.Audience = "https://other.example"
		parseExpectVerifyError(t, pres+kb, keys.issuerKey, &otherExpected, ErrKeyBinding)
	})

	t.Run("tampered sd_hash", func(t *testing.T) {
		payload := map[string]interface{}{
			"iat":     now.Unix(),
			"aud":     expected.Audience,
			"nonce":   expected.Nonce,
			"sd_hash": strings.Repeat("A", 43),
		}
		forgedKB := cred.kbJWTWithOptions(t, keys, pres, payload, "kb+jwt")
		parseExpectVerifyError(t, pres+forgedKB, keys.issuerKey, expected, ErrKeyBinding)
	})

	t.Run("wrong typ header", func(t *testing.T) {
		payload := map[string]interface{}{
			"iat":     now.Unix(),
			"aud":     expected.Audience,
			"nonce":   expected.Nonce,
			"sd_hash": sha256Hash(pres),
		}
		wrongTyp := cred.kbJWTWithOptions(t, keys, pres, payload, "jwt")
		parseExpectVerifyError(t, pres+wrongTyp, keys.issuerKey, expected, ErrKeyBinding)
	})

	t.Run("stale presentation", func(t *testing.T) {
		otherExpected := *expected
		otherExpected.MaxAge = time.Second
		otherExpected.Now = time.Now().Add(2 * time.Hour)
		parseExpectVerifyError(t, pres+kb, keys.issuerKey, &otherExpected, ErrKeyBinding)
	})

	t.Run("key binding signed by someone else", func(t *testing.T) {
		attacker := generateTestKeys(t)
		forgedKB := cred.kbJWT(t, attacker, pres, expected.Nonce, expected.Audience, now)
		parseExpectVerifyError(t, pres+forgedKB, keys.issuerKey, expected, ErrKeyBinding)
	})

	t.Run("expected key binding missing", func(t *testing.T) {
		parseExpectVerifyError(t, pres, keys.issuerKey, expected, ErrKeyBindingRequired)
	})

	t.Run("unexpected key binding present", func(t *testing.T) {
		parseExpectVerifyError(t, pres+kb, keys.issuerKey, nil, ErrKeyBindingRequired)
	})
}

func TestMalformedParsing(t *testing.T) {
	keys := generateTestKeys(t)
	cred := buildCredential(t, keys)

	for _, in := range []string{
		"",
		"~",
		cred.issuerCompact, // missing trailing "~"
		cred.issuerCompact + "~~",
		" " + cred.issuerCompact + "~",
		cred.issuerCompact + "~" + "not-base64-!!!" + "~",
	} {
		if _, err := Parse(in, []jose.SignatureAlgorithm{jose.ES256}); err == nil {
			t.Fatalf("Parse(%q) expected error", in)
		}
	}

	// A disclosure that is not a 2- or 3-element JSON array.
	bad := base64.RawURLEncoding.EncodeToString([]byte(`["salt"]`))
	if _, err := Parse(cred.issuerCompact+"~"+bad+"~", []jose.SignatureAlgorithm{jose.ES256}); err == nil {
		t.Fatalf("expected error for one-element disclosure")
	}

	// Reserved claim names are not allowed in disclosures.
	reserved := base64.RawURLEncoding.EncodeToString([]byte(`["salt","_sd",[]]`))
	if _, err := Parse(cred.issuerCompact+"~"+reserved+"~", []jose.SignatureAlgorithm{jose.ES256}); err == nil {
		t.Fatalf("expected error for reserved claim name")
	}
}

func TestUnsupportedHashAlgorithm(t *testing.T) {
	keys := generateTestKeys(t)
	claims := map[string]interface{}{
		"iss":     "i",
		"_sd_alg": "sha-1024",
	}
	combined := signCompact(t, keys.issuerKey, claims, "") + "~"
	parseExpectVerifyError(t, combined, keys.issuerKey, nil, ErrUnsupportedHashAlgorithm)
}
