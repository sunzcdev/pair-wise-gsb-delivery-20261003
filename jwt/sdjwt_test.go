package jwt

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/json"
	"github.com/go-jose/go-jose/v4/testutils/assert"
	"github.com/go-jose/go-jose/v4/testutils/require"
)

func sdTestDisclosure(t *testing.T, elements ...interface{}) string {
	t.Helper()
	raw, err := json.Marshal(elements)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func sdTestDigest(disclosure string) string {
	sum := sha256.Sum256([]byte(disclosure))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func sdTestPresentation(t *testing.T, claims map[string]interface{}, disclosures ...string) string {
	t.Helper()
	token, err := Signed(hmacSigner).Claims(claims).Serialize()
	require.NoError(t, err)
	out := token
	for _, d := range disclosures {
		out += "~" + d
	}
	return out + "~"
}

// TestSDJWTDisclosureDigestVectors checks the digest computation against the
// fixed test vectors from RFC 9901.
func TestSDJWTDisclosureDigestVectors(t *testing.T) {
	vectors := []struct {
		disclosure string
		digest     string
		elements   []interface{}
	}{
		{
			// RFC 9901: object property disclosure for "family_name".
			disclosure: "WyI2cU1RdlJMNWhhaiIsICJmYW1pbHlfbmFtZSIsICJNw7ZiaXVzIl0",
			digest:     "uutlBuYeMDyjLLTpf6Jxi7yNkEF35jdyWMn9U7b_RYY",
			elements:   []interface{}{"6qMQvRL5haj", "family_name", "Möbius"},
		},
		{
			// RFC 9901: array element disclosure for "FR".
			disclosure: "WyJsa2x4RjVqTVlsR1RQVW92TU5JdkNBIiwgIkZSIl0",
			digest:     "w0I8EKcdCtUPkGCNUrfwVp2xEgNjtoIDlOxc9-PlOhs",
			elements:   []interface{}{"lklxF5jMYlGTPUovMNIvCA", "FR"},
		},
	}

	for _, v := range vectors {
		assert.Equal(t, v.digest, sdTestDigest(v.disclosure))

		decoded, err := base64.RawURLEncoding.DecodeString(v.disclosure)
		require.NoError(t, err)
		var elements []interface{}
		require.NoError(t, json.Unmarshal(decoded, &elements))
		require.Equal(t, len(elements), len(v.elements))
		for i := range v.elements {
			assert.Equal(t, v.elements[i], elements[i])
		}

		// The library parser must compute the same digest.
		disc, err := parseSDDisclosure(v.disclosure, crypto.SHA256)
		require.NoError(t, err)
		assert.Equal(t, v.digest, disc.digest)
	}
}

// TestSDJWTVerifyObjectAndArrayDisclosures verifies disclosures in objects,
// nested objects and arrays, and checks that concealed claims are reported
// separately from plainly missing ones.
func TestSDJWTVerifyObjectAndArrayDisclosures(t *testing.T) {
	familyName := sdTestDisclosure(t, "salt-fam", "family_name", "Möbius")
	street := sdTestDisclosure(t, "salt-street", "street_address", "Main St")
	nationalityFR := sdTestDisclosure(t, "salt-fr", "FR")
	hidden := sdTestDisclosure(t, "salt-hidden", "hidden_claim", 42)

	issuerClaims := map[string]interface{}{
		"iss":        "https://issuer.example",
		"given_name": "John",
		"_sd_alg":    "sha-256",
		"_sd":        []interface{}{sdTestDigest(familyName)},
		"address": map[string]interface{}{
			"country": "DE",
			"_sd":     []interface{}{sdTestDigest(street)},
		},
		"nationalities": []interface{}{
			map[string]interface{}{"...": sdTestDigest(nationalityFR)},
			"DE",
			map[string]interface{}{"...": sdTestDigest(hidden)},
		},
	}

	// The holder discloses everything except "hidden".
	serialized := sdTestPresentation(t, issuerClaims, familyName, street, nationalityFR)

	sd, err := ParseSDJWT(serialized, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)

	result, err := sd.Verify(&SDJWTVerificationOptions{IssuerSigningKey: sharedKey})
	require.NoError(t, err)

	claims := result.Claims
	assert.Equal(t, "Möbius", claims["family_name"])
	assert.Equal(t, "John", claims["given_name"])

	address, ok := claims["address"].(map[string]interface{})
	require.Equal(t, true, ok)
	assert.Equal(t, "Main St", address["street_address"])
	assert.Equal(t, "DE", address["country"])
	_, ok = address["_sd"]
	assert.Equal(t, false, ok)

	nationalities, ok := claims["nationalities"].([]interface{})
	require.Equal(t, true, ok)
	require.Equal(t, len(nationalities), 2)
	assert.Equal(t, "FR", nationalities[0])
	assert.Equal(t, "DE", nationalities[1])

	_, ok = claims["_sd"]
	assert.Equal(t, false, ok)
	_, ok = claims["_sd_alg"]
	assert.Equal(t, false, ok)

	// "hidden_claim" was not disclosed: it must be distinguishable from a
	// claim that simply does not exist, like "email".
	_, ok = claims["hidden_claim"]
	assert.Equal(t, false, ok)
	_, ok = claims["email"]
	assert.Equal(t, false, ok)

	require.Equal(t, len(result.Concealed), 1)
	assert.Equal(t, "nationalities[2]", result.Concealed[0].Path)
	assert.Equal(t, sdTestDigest(hidden), result.Concealed[0].Digest)
}

// TestSDJWTDuplicateDisclosure ensures that presenting the same disclosure
// twice is rejected.
func TestSDJWTDuplicateDisclosure(t *testing.T) {
	familyName := sdTestDisclosure(t, "salt-fam", "family_name", "Möbius")
	issuerClaims := map[string]interface{}{
		"_sd": []interface{}{sdTestDigest(familyName)},
	}
	serialized := sdTestPresentation(t, issuerClaims, familyName, familyName)

	sd, err := ParseSDJWT(serialized, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)

	_, err = sd.Verify(&SDJWTVerificationOptions{IssuerSigningKey: sharedKey})
	assert.ErrorIs(t, err, ErrSDJWTDuplicateDisclosure)
}

// TestSDJWTDigestReused ensures that a digest referenced twice in the SD-JWT
// is rejected.
func TestSDJWTDigestReused(t *testing.T) {
	familyName := sdTestDisclosure(t, "salt-fam", "family_name", "Möbius")
	digest := sdTestDigest(familyName)
	issuerClaims := map[string]interface{}{
		"_sd": []interface{}{digest, digest},
	}
	serialized := sdTestPresentation(t, issuerClaims, familyName)

	sd, err := ParseSDJWT(serialized, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)

	_, err = sd.Verify(&SDJWTVerificationOptions{IssuerSigningKey: sharedKey})
	assert.ErrorIs(t, err, ErrSDJWTDigestReused)
}

// TestSDJWTUnreferencedDisclosure ensures that a disclosure the attacker
// injected without a matching digest in the signed JWT is rejected and never
// lands in the resulting claims.
func TestSDJWTUnreferencedDisclosure(t *testing.T) {
	familyName := sdTestDisclosure(t, "salt-fam", "family_name", "Möbius")
	forged := sdTestDisclosure(t, "salt-evil", "role", "admin")
	issuerClaims := map[string]interface{}{
		"_sd": []interface{}{sdTestDigest(familyName)},
	}
	serialized := sdTestPresentation(t, issuerClaims, familyName, forged)

	sd, err := ParseSDJWT(serialized, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)

	_, err = sd.Verify(&SDJWTVerificationOptions{IssuerSigningKey: sharedKey})
	assert.ErrorIs(t, err, ErrSDJWTUnreferencedDisclosure)
}

// TestSDJWTClaimCollision ensures a disclosure cannot overwrite a claim that
// already exists in the issuer-signed object.
func TestSDJWTClaimCollision(t *testing.T) {
	collision := sdTestDisclosure(t, "salt-evil", "given_name", "Eve")
	issuerClaims := map[string]interface{}{
		"given_name": "John",
		"_sd":        []interface{}{sdTestDigest(collision)},
	}
	serialized := sdTestPresentation(t, issuerClaims, collision)

	sd, err := ParseSDJWT(serialized, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)

	_, err = sd.Verify(&SDJWTVerificationOptions{IssuerSigningKey: sharedKey})
	assert.ErrorIs(t, err, ErrSDJWTClaimCollision)
}

// TestSDJWTDisclosureTypeMismatch ensures disclosures cannot be referenced
// from a forged nesting level: array-element disclosures in object "_sd"
// arrays and object-property disclosures in array placeholders are rejected.
func TestSDJWTDisclosureTypeMismatch(t *testing.T) {
	arrayElem := sdTestDisclosure(t, "salt-arr", "FR")
	objectProp := sdTestDisclosure(t, "salt-obj", "family_name", "Möbius")

	// Array-element disclosure referenced from an object "_sd" array.
	issuerClaims := map[string]interface{}{
		"_sd": []interface{}{sdTestDigest(arrayElem)},
	}
	serialized := sdTestPresentation(t, issuerClaims, arrayElem)
	sd, err := ParseSDJWT(serialized, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)
	_, err = sd.Verify(&SDJWTVerificationOptions{IssuerSigningKey: sharedKey})
	assert.ErrorIs(t, err, ErrSDJWTDisclosureType)

	// Object-property disclosure referenced from an array placeholder.
	issuerClaims = map[string]interface{}{
		"nationalities": []interface{}{
			map[string]interface{}{"...": sdTestDigest(objectProp)},
		},
	}
	serialized = sdTestPresentation(t, issuerClaims, objectProp)
	sd, err = ParseSDJWT(serialized, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)
	_, err = sd.Verify(&SDJWTVerificationOptions{IssuerSigningKey: sharedKey})
	assert.ErrorIs(t, err, ErrSDJWTDisclosureType)
}

func sdTestKeyBindingJWT(t *testing.T, holderKey ed25519.PrivateKey, signedPart, nonce, audience string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(signedPart))
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.EdDSA, Key: holderKey},
		(&jose.SignerOptions{}).WithType("kb+jwt"),
	)
	require.NoError(t, err)
	token, err := Signed(signer).Claims(map[string]interface{}{
		"iat":     time.Now().Unix(),
		"aud":     audience,
		"nonce":   nonce,
		"sd_hash": base64.RawURLEncoding.EncodeToString(sum[:]),
	}).Serialize()
	require.NoError(t, err)
	return token
}

func sdTestHolderCNF(t *testing.T, holderKey ed25519.PublicKey) map[string]interface{} {
	t.Helper()
	encoded, err := json.Marshal(jose.JSONWebKey{Key: holderKey})
	require.NoError(t, err)
	var jwk map[string]interface{}
	require.NoError(t, json.Unmarshal(encoded, &jwk))
	return map[string]interface{}{"jwk": jwk}
}

// TestSDJWTKeyBinding covers the optional holder key-binding JWT: a valid
// presentation verifies, a presentation bound to another nonce is rejected,
// a key binding JWT from another presentation is rejected, and a missing key
// binding JWT fails when required.
func TestSDJWTKeyBinding(t *testing.T) {
	holderPub, holderPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	familyName := sdTestDisclosure(t, "salt-fam", "family_name", "Möbius")
	issuerClaims := map[string]interface{}{
		"iss": "https://issuer.example",
		"_sd": []interface{}{sdTestDigest(familyName)},
		"cnf": sdTestHolderCNF(t, holderPub),
	}
	sdWithoutKB := sdTestPresentation(t, issuerClaims, familyName)

	kb := sdTestKeyBindingJWT(t, holderPriv, sdWithoutKB, "nonce-1", "https://verifier.example")
	presentation := sdWithoutKB + kb

	verifyOpts := func(nonce string) *SDJWTVerificationOptions {
		return &SDJWTVerificationOptions{
			IssuerSigningKey:              sharedKey,
			KeyBindingSignatureAlgorithms: []jose.SignatureAlgorithm{jose.EdDSA},
			ExpectedNonce:                 nonce,
			ExpectedAudience:              "https://verifier.example",
			RequireKeyBinding:             true,
		}
	}

	// Valid presentation.
	sd, err := ParseSDJWT(presentation, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)
	result, err := sd.Verify(verifyOpts("nonce-1"))
	require.NoError(t, err)
	assert.Equal(t, "Möbius", result.Claims["family_name"])

	// The same presentation replayed against another nonce must fail.
	sd, err = ParseSDJWT(presentation, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)
	_, err = sd.Verify(verifyOpts("nonce-2"))
	assert.ErrorIs(t, err, ErrSDJWTNonceMismatch)

	// A key binding JWT made for a different presentation must fail sd_hash
	// validation.
	otherSD := sdTestPresentation(t, issuerClaims)
	otherKB := sdTestKeyBindingJWT(t, holderPriv, otherSD, "nonce-1", "https://verifier.example")
	sd, err = ParseSDJWT(sdWithoutKB+otherKB, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)
	_, err = sd.Verify(verifyOpts("nonce-1"))
	assert.ErrorIs(t, err, ErrSDJWTSDHashMismatch)

	// Key binding required but missing.
	sd, err = ParseSDJWT(sdWithoutKB, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)
	_, err = sd.Verify(verifyOpts("nonce-1"))
	assert.ErrorIs(t, err, ErrSDJWTKeyBindingRequired)

	// Without any key binding requirement the SD-JWT alone verifies.
	sd, err = ParseSDJWT(sdWithoutKB, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)
	result, err = sd.Verify(&SDJWTVerificationOptions{IssuerSigningKey: sharedKey})
	require.NoError(t, err)
	assert.Equal(t, "Möbius", result.Claims["family_name"])
}
