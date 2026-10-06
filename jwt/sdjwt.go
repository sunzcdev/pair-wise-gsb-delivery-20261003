package jwt

import (
	"crypto"
	_ "crypto/sha256"
	_ "crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/json"
)

// Hash algorithm identifiers for the SD-JWT "_sd_alg" claim (RFC 9901).
const (
	SDHashAlgorithmSHA256 = "sha-256"
	SDHashAlgorithmSHA384 = "sha-384"
	SDHashAlgorithmSHA512 = "sha-512"
)

// SDKeyBindingJWTType is the required "typ" header value of a Key Binding JWT.
const SDKeyBindingJWTType = "kb+jwt"

var (
	// ErrSDJWTFormat indicates a malformed SD-JWT serialization.
	ErrSDJWTFormat = errors.New("go-jose/go-jose/jwt: malformed SD-JWT serialization")

	// ErrSDJWTUnsupportedHashAlgorithm indicates an "_sd_alg" value that is not
	// supported or not allowed by the verification options.
	ErrSDJWTUnsupportedHashAlgorithm = errors.New("go-jose/go-jose/jwt: unsupported SD-JWT hash algorithm")

	// ErrSDJWTInvalidDisclosure indicates a malformed disclosure.
	ErrSDJWTInvalidDisclosure = errors.New("go-jose/go-jose/jwt: invalid SD-JWT disclosure")

	// ErrSDJWTDuplicateDisclosure indicates the same disclosure was presented twice.
	ErrSDJWTDuplicateDisclosure = errors.New("go-jose/go-jose/jwt: duplicate SD-JWT disclosure")

	// ErrSDJWTUnreferencedDisclosure indicates a disclosure whose digest is not
	// referenced anywhere in the issuer-signed JWT.
	ErrSDJWTUnreferencedDisclosure = errors.New("go-jose/go-jose/jwt: disclosure not referenced by SD-JWT")

	// ErrSDJWTDigestReused indicates a digest value referenced more than once
	// in the SD-JWT.
	ErrSDJWTDigestReused = errors.New("go-jose/go-jose/jwt: disclosure digest referenced more than once")

	// ErrSDJWTDisclosureType indicates an array-element disclosure referenced
	// from an object "_sd" array or an object-property disclosure referenced
	// from an array "..." placeholder.
	ErrSDJWTDisclosureType = errors.New("go-jose/go-jose/jwt: disclosure type does not match referencing element")

	// ErrSDJWTClaimCollision indicates a disclosure whose claim name already
	// exists in the containing object.
	ErrSDJWTClaimCollision = errors.New("go-jose/go-jose/jwt: disclosed claim name already exists")

	// ErrSDJWTInvalidSDClaim indicates a malformed "_sd" or "..." element.
	ErrSDJWTInvalidSDClaim = errors.New("go-jose/go-jose/jwt: invalid _sd claim or array placeholder")

	// ErrSDJWTKeyBindingRequired indicates a missing Key Binding JWT.
	ErrSDJWTKeyBindingRequired = errors.New("go-jose/go-jose/jwt: key binding JWT required but not present")

	// ErrSDJWTInvalidKeyBindingType indicates a Key Binding JWT with a wrong "typ" header.
	ErrSDJWTInvalidKeyBindingType = errors.New("go-jose/go-jose/jwt: key binding JWT must have typ kb+jwt")

	// ErrSDJWTInvalidKeyBindingClaims indicates missing claims in the Key Binding JWT.
	ErrSDJWTInvalidKeyBindingClaims = errors.New("go-jose/go-jose/jwt: key binding JWT misses required claims")

	// ErrSDJWTMissingHolderKey indicates that no holder key could be determined.
	ErrSDJWTMissingHolderKey = errors.New("go-jose/go-jose/jwt: no holder key available for key binding verification")

	// ErrSDJWTSDHashMismatch indicates an "sd_hash" claim that does not match
	// the SD-JWT it is bound to.
	ErrSDJWTSDHashMismatch = errors.New("go-jose/go-jose/jwt: key binding sd_hash does not match SD-JWT")

	// ErrSDJWTNonceMismatch indicates a "nonce" claim that does not match the
	// expected nonce.
	ErrSDJWTNonceMismatch = errors.New("go-jose/go-jose/jwt: key binding nonce mismatch")

	// ErrSDJWTAudienceMismatch indicates an "aud" claim that does not contain
	// the expected audience.
	ErrSDJWTAudienceMismatch = errors.New("go-jose/go-jose/jwt: key binding audience mismatch")
)

// SDJWT represents a parsed SD-JWT (RFC 9901): an issuer-signed JWT followed
// by disclosures and, optionally, a Key Binding JWT, separated by "~".
type SDJWT struct {
	// IssuerJWT is the issuer-signed JWT containing the "_sd" digests.
	IssuerJWT *JSONWebToken

	// Disclosures contains the raw, base64url-encoded disclosures.
	Disclosures []string

	// KeyBindingJWT is the raw Key Binding JWT, or "" if none was presented.
	KeyBindingJWT string

	// signedPart is the serialization up to and including the "~" preceding
	// the Key Binding JWT. It is the input of the "sd_hash" computation.
	signedPart string

	signatureAlgorithms []jose.SignatureAlgorithm
}

// SDJWTVerificationOptions controls SD-JWT verification.
type SDJWTVerificationOptions struct {
	// IssuerSigningKey verifies the issuer JWT signature. Required.
	IssuerSigningKey interface{}

	// ExpectedNonce, when non-empty, requires a Key Binding JWT whose "nonce"
	// claim matches. Use it to bind a presentation to a verifier-chosen nonce.
	ExpectedNonce string

	// ExpectedAudience, when non-empty, requires a Key Binding JWT whose "aud"
	// claim contains this value.
	ExpectedAudience string

	// RequireKeyBinding makes verification fail when no Key Binding JWT is
	// present.
	RequireKeyBinding bool

	// HolderKey verifies the Key Binding JWT signature. When nil, the key is
	// taken from the "cnf" ("jwk") claim of the issuer JWT.
	HolderKey interface{}

	// KeyBindingSignatureAlgorithms lists the accepted "alg" values of the Key
	// Binding JWT. Defaults to the algorithms passed to ParseSDJWT.
	KeyBindingSignatureAlgorithms []jose.SignatureAlgorithm

	// AllowedHashAlgorithms lists the accepted "_sd_alg" values. Defaults to
	// sha-256, sha-384 and sha-512.
	AllowedHashAlgorithms []string
}

// SDJWTConcealedClaim identifies a claim whose digest appears in the SD-JWT
// but for which no disclosure was presented. It lets callers distinguish
// "deliberately not disclosed" from "claim absent".
type SDJWTConcealedClaim struct {
	// Path is the location of the concealed claim, e.g. "address" for
	// concealed members of the "address" object, or "nationalities[2]" for a
	// concealed array element. The root object is "".
	Path string

	// Digest is the base64url-encoded digest of the undisclosed claim.
	Digest string
}

// SDJWTClaims is the result of a successful SD-JWT verification.
type SDJWTClaims struct {
	// Claims contains the issuer JWT claims with all presented disclosures
	// applied. The "_sd" and "_sd_alg" claims are removed.
	Claims map[string]interface{}

	// Concealed lists claims that remain undisclosed.
	Concealed []SDJWTConcealedClaim
}

// ParseSDJWT parses an SD-JWT serialization of the form
//
//	<issuer JWT>~<disclosure>~...~[<Key Binding JWT>]
//
// The signatureAlgorithms parameter restricts the accepted "alg" header of
// the issuer JWT (and, by default, of the Key Binding JWT).
func ParseSDJWT(serialized string, signatureAlgorithms []jose.SignatureAlgorithm) (*SDJWT, error) {
	parts := strings.Split(serialized, "~")
	if len(parts) < 2 || parts[0] == "" {
		return nil, ErrSDJWTFormat
	}

	issuerJWT, err := ParseSigned(parts[0], signatureAlgorithms)
	if err != nil {
		return nil, err
	}

	out := &SDJWT{
		IssuerJWT:           issuerJWT,
		signatureAlgorithms: signatureAlgorithms,
	}

	disclosures := parts[1:]
	if last := parts[len(parts)-1]; last != "" {
		out.KeyBindingJWT = last
		out.signedPart = serialized[:len(serialized)-len(last)]
	}
	// Drop the last segment: it is either the Key Binding JWT or the empty
	// segment after the trailing "~".
	disclosures = disclosures[:len(disclosures)-1]

	for _, d := range disclosures {
		if d == "" {
			return nil, ErrSDJWTFormat
		}
	}
	out.Disclosures = disclosures

	return out, nil
}

// sdDisclosure is a parsed disclosure.
type sdDisclosure struct {
	digest    string
	name      string
	value     interface{}
	arrayElem bool
	used      bool
}

// sdDisclosureDigest computes the base64url-encoded digest of a disclosure
// over its raw base64url representation, as specified by RFC 9901.
func sdDisclosureDigest(hash crypto.Hash, disclosure string) string {
	h := hash.New()
	h.Write([]byte(disclosure))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// parseSDDisclosure decodes and validates a single disclosure.
func parseSDDisclosure(raw string, hash crypto.Hash) (*sdDisclosure, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: not valid base64url", ErrSDJWTInvalidDisclosure)
	}

	var elements []interface{}
	if err := json.Unmarshal(decoded, &elements); err != nil {
		return nil, fmt.Errorf("%w: not a JSON array", ErrSDJWTInvalidDisclosure)
	}

	d := &sdDisclosure{digest: sdDisclosureDigest(hash, raw)}

	saltOk := func(v interface{}) bool {
		salt, ok := v.(string)
		return ok && salt != ""
	}

	switch len(elements) {
	case 2:
		// Array element: [<salt>, <value>]
		if !saltOk(elements[0]) {
			return nil, fmt.Errorf("%w: missing salt", ErrSDJWTInvalidDisclosure)
		}
		d.arrayElem = true
		d.value = elements[1]
	case 3:
		// Object property: [<salt>, <claim name>, <claim value>]
		if !saltOk(elements[0]) {
			return nil, fmt.Errorf("%w: missing salt", ErrSDJWTInvalidDisclosure)
		}
		name, ok := elements[1].(string)
		if !ok || name == "" || name == "_sd" || name == "..." {
			return nil, fmt.Errorf("%w: invalid claim name", ErrSDJWTInvalidDisclosure)
		}
		d.name = name
		d.value = elements[2]
	default:
		return nil, fmt.Errorf("%w: expected array of 2 or 3 elements", ErrSDJWTInvalidDisclosure)
	}

	return d, nil
}

// sdProcessor applies disclosures to the issuer JWT claims.
type sdProcessor struct {
	disclosures map[string]*sdDisclosure
	referenced  map[string]bool
	concealed   []SDJWTConcealedClaim
}

func sdJoinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// lookup marks a digest as referenced and returns the matching disclosure,
// or nil if the holder did not present one.
func (p *sdProcessor) lookup(digest string) (*sdDisclosure, error) {
	if p.referenced[digest] {
		return nil, ErrSDJWTDigestReused
	}
	p.referenced[digest] = true
	return p.disclosures[digest], nil
}

func (p *sdProcessor) processValue(value interface{}, path string) (interface{}, error) {
	switch v := value.(type) {
	case map[string]interface{}:
		if _, ok := v["..."]; ok {
			// "..." is only valid as the sole key of an array element.
			return nil, ErrSDJWTInvalidSDClaim
		}
		if err := p.processObject(v, path); err != nil {
			return nil, err
		}
		return v, nil
	case []interface{}:
		return p.processArray(v, path)
	default:
		return value, nil
	}
}

func (p *sdProcessor) processObject(obj map[string]interface{}, path string) error {
	for key, value := range obj {
		if key == "_sd" || key == "_sd_alg" {
			continue
		}
		processed, err := p.processValue(value, sdJoinPath(path, key))
		if err != nil {
			return err
		}
		obj[key] = processed
	}

	rawSD, ok := obj["_sd"]
	if !ok {
		return nil
	}
	sdList, ok := rawSD.([]interface{})
	if !ok {
		return ErrSDJWTInvalidSDClaim
	}

	for _, rawDigest := range sdList {
		digest, ok := rawDigest.(string)
		if !ok {
			return ErrSDJWTInvalidSDClaim
		}
		disc, err := p.lookup(digest)
		if err != nil {
			return err
		}
		if disc == nil {
			p.concealed = append(p.concealed, SDJWTConcealedClaim{Path: path, Digest: digest})
			continue
		}
		if disc.arrayElem {
			return fmt.Errorf("%w: array element disclosure referenced from object _sd", ErrSDJWTDisclosureType)
		}
		if _, exists := obj[disc.name]; exists {
			// A disclosure must never overwrite a claim that is already
			// present in the issuer-signed object.
			return fmt.Errorf("%w: %q", ErrSDJWTClaimCollision, disc.name)
		}
		disc.used = true
		processed, err := p.processValue(disc.value, sdJoinPath(path, disc.name))
		if err != nil {
			return err
		}
		obj[disc.name] = processed
	}

	delete(obj, "_sd")
	return nil
}

func (p *sdProcessor) processArray(arr []interface{}, path string) ([]interface{}, error) {
	out := make([]interface{}, 0, len(arr))
	for i, element := range arr {
		elementPath := fmt.Sprintf("%s[%d]", path, i)
		if obj, ok := element.(map[string]interface{}); ok {
			if rawDigest, isPlaceholder := obj["..."]; isPlaceholder {
				if len(obj) != 1 {
					return nil, ErrSDJWTInvalidSDClaim
				}
				digest, ok := rawDigest.(string)
				if !ok {
					return nil, ErrSDJWTInvalidSDClaim
				}
				disc, err := p.lookup(digest)
				if err != nil {
					return nil, err
				}
				if disc == nil {
					// Undisclosed array element: drop the placeholder.
					p.concealed = append(p.concealed, SDJWTConcealedClaim{Path: elementPath, Digest: digest})
					continue
				}
				if !disc.arrayElem {
					return nil, fmt.Errorf("%w: object property disclosure referenced from array", ErrSDJWTDisclosureType)
				}
				disc.used = true
				processed, err := p.processValue(disc.value, elementPath)
				if err != nil {
					return nil, err
				}
				out = append(out, processed)
				continue
			}
		}
		processed, err := p.processValue(element, elementPath)
		if err != nil {
			return nil, err
		}
		out = append(out, processed)
	}
	return out, nil
}

func sdHashForAlgorithm(name string) (crypto.Hash, error) {
	switch name {
	case SDHashAlgorithmSHA256:
		return crypto.SHA256, nil
	case SDHashAlgorithmSHA384:
		return crypto.SHA384, nil
	case SDHashAlgorithmSHA512:
		return crypto.SHA512, nil
	}
	return 0, fmt.Errorf("%w: %s", ErrSDJWTUnsupportedHashAlgorithm, name)
}

func sdHashAllowed(alg string, allowed []string) bool {
	if len(allowed) == 0 {
		allowed = []string{SDHashAlgorithmSHA256, SDHashAlgorithmSHA384, SDHashAlgorithmSHA512}
	}
	for _, a := range allowed {
		if a == alg {
			return true
		}
	}
	return false
}

// Verify checks the issuer JWT signature, applies the presented disclosures
// and, when present or required, verifies the Key Binding JWT. On success it
// returns the disclosed claims along with the locations of claims that remain
// concealed.
func (t *SDJWT) Verify(opts *SDJWTVerificationOptions) (*SDJWTClaims, error) {
	if opts == nil || opts.IssuerSigningKey == nil {
		return nil, errors.New("go-jose/go-jose/jwt: SD-JWT verification requires an issuer signing key")
	}

	claims := make(map[string]interface{})
	if err := t.IssuerJWT.Claims(opts.IssuerSigningKey, &claims); err != nil {
		return nil, err
	}

	hashAlg, _ := claims["_sd_alg"].(string)
	if hashAlg == "" {
		hashAlg = SDHashAlgorithmSHA256
	}
	if !sdHashAllowed(hashAlg, opts.AllowedHashAlgorithms) {
		return nil, fmt.Errorf("%w: %s", ErrSDJWTUnsupportedHashAlgorithm, hashAlg)
	}
	hash, err := sdHashForAlgorithm(hashAlg)
	if err != nil {
		return nil, err
	}

	processor := &sdProcessor{
		disclosures: make(map[string]*sdDisclosure, len(t.Disclosures)),
		referenced:  make(map[string]bool),
	}
	for _, raw := range t.Disclosures {
		disc, err := parseSDDisclosure(raw, hash)
		if err != nil {
			return nil, err
		}
		if _, dup := processor.disclosures[disc.digest]; dup {
			return nil, ErrSDJWTDuplicateDisclosure
		}
		processor.disclosures[disc.digest] = disc
	}

	if err := processor.processObject(claims, ""); err != nil {
		return nil, err
	}

	for _, disc := range processor.disclosures {
		if !disc.used {
			return nil, fmt.Errorf("%w: %s", ErrSDJWTUnreferencedDisclosure, disc.digest)
		}
	}

	delete(claims, "_sd_alg")

	if err := t.verifyKeyBinding(opts, claims, hash); err != nil {
		return nil, err
	}

	return &SDJWTClaims{Claims: claims, Concealed: processor.concealed}, nil
}

// sdConfirmationKey extracts the holder public key from the "cnf" claim.
func sdConfirmationKey(claims map[string]interface{}) (interface{}, error) {
	cnf, ok := claims["cnf"].(map[string]interface{})
	if !ok {
		return nil, ErrSDJWTMissingHolderKey
	}
	rawJWK, ok := cnf["jwk"]
	if !ok {
		return nil, ErrSDJWTMissingHolderKey
	}
	encoded, err := json.Marshal(rawJWK)
	if err != nil {
		return nil, err
	}
	jwk := jose.JSONWebKey{}
	if err := jwk.UnmarshalJSON(encoded); err != nil {
		return nil, err
	}
	if !jwk.Valid() || !jwk.IsPublic() {
		return nil, ErrSDJWTMissingHolderKey
	}
	return jwk.Key, nil
}

func (t *SDJWT) verifyKeyBinding(opts *SDJWTVerificationOptions, issuerClaims map[string]interface{}, hash crypto.Hash) error {
	if t.KeyBindingJWT == "" {
		if opts.RequireKeyBinding || opts.ExpectedNonce != "" || opts.ExpectedAudience != "" {
			return ErrSDJWTKeyBindingRequired
		}
		return nil
	}

	algs := opts.KeyBindingSignatureAlgorithms
	if len(algs) == 0 {
		algs = t.signatureAlgorithms
	}
	kb, err := ParseSigned(t.KeyBindingJWT, algs)
	if err != nil {
		return err
	}

	if len(kb.Headers) == 0 {
		return ErrSDJWTInvalidKeyBindingType
	}
	typ, _ := kb.Headers[0].ExtraHeaders[jose.HeaderType].(string)
	if strings.TrimPrefix(typ, "application/") != SDKeyBindingJWTType {
		return fmt.Errorf("%w: got %q", ErrSDJWTInvalidKeyBindingType, typ)
	}

	holderKey := opts.HolderKey
	if holderKey == nil {
		holderKey, err = sdConfirmationKey(issuerClaims)
		if err != nil {
			return err
		}
	}

	kbClaims := make(map[string]interface{})
	if err := kb.Claims(holderKey, &kbClaims); err != nil {
		return err
	}

	sdHash, ok := kbClaims["sd_hash"].(string)
	if !ok || sdHash == "" {
		return fmt.Errorf("%w: missing sd_hash", ErrSDJWTInvalidKeyBindingClaims)
	}
	h := hash.New()
	h.Write([]byte(t.signedPart))
	if sdHash != base64.RawURLEncoding.EncodeToString(h.Sum(nil)) {
		return ErrSDJWTSDHashMismatch
	}

	if _, ok := kbClaims["iat"]; !ok {
		return fmt.Errorf("%w: missing iat", ErrSDJWTInvalidKeyBindingClaims)
	}

	if opts.ExpectedNonce != "" {
		nonce, _ := kbClaims["nonce"].(string)
		if nonce != opts.ExpectedNonce {
			return ErrSDJWTNonceMismatch
		}
	}

	if opts.ExpectedAudience != "" {
		encoded, err := json.Marshal(kbClaims["aud"])
		if err != nil {
			return ErrSDJWTAudienceMismatch
		}
		var aud Audience
		if err := json.Unmarshal(encoded, &aud); err != nil {
			return ErrSDJWTAudienceMismatch
		}
		if !aud.Contains(opts.ExpectedAudience) {
			return ErrSDJWTAudienceMismatch
		}
	}

	return nil
}
