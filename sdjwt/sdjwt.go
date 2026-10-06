// Package sdjwt implements parsing and verification of Selective Disclosure
// for JWTs (SD-JWT) as specified in RFC 9901.
//
// An SD-JWT is an issuer-signed JWS (the "Issuer-Signed JWT") followed by
// base64url-encoded Disclosures, each prefixed with a '~'. An SD-JWT
// Presentation optionally appends a Key-Binding JWT after a final '~':
//
//	<Issuer-Signed JWT>~<Disclosure 1>~...~<Disclosure N>~[<KB-JWT>]
//
// The verifier checks the issuer signature, matches every Disclosure against
// the SD digests embedded in the issuer claims, and optionally authenticates
// the holder through a Key-Binding JWT. See Parse and SDJWT.Verify for
// details.
package sdjwt

import (
	"crypto"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/json"
)

// Algorithm names for the "_sd_alg" claim (RFC 9901, Section 4.2.1).
const (
	SHA256 = "sha-256"
	SHA384 = "sha-384"
	SHA512 = "sha-512"
)

var (
	// ErrMalformedCombinedFormat indicates that the input is not a valid
	// combined SD-JWT / SD-JWT Presentation serialization.
	ErrMalformedCombinedFormat = errors.New("sdjwt: malformed SD-JWT combined format")

	// ErrUnsupportedHashAlgorithm indicates an "_sd_alg" value for which no
	// hash function is available.
	ErrUnsupportedHashAlgorithm = errors.New("sdjwt: unsupported _sd_alg hash algorithm")

	// ErrMalformedDisclosure indicates an invalid Disclosure encoding or
	// JSON array content.
	ErrMalformedDisclosure = errors.New("sdjwt: malformed disclosure")

	// ErrMalformedClaims indicates the issuer claims are not a valid JSON
	// object or contain malformed SD-JWT structure.
	ErrMalformedClaims = errors.New("sdjwt: malformed issuer claims")

	// ErrDuplicateDigest indicates that one Disclosure is referenced by more
	// than one "_sd" entry, or that multiple disclosures hash to the same
	// digest.
	ErrDuplicateDigest = errors.New("sdjwt: duplicate SD digest")

	// ErrUnusedDisclosure indicates that a Disclosure has no matching digest
	// anywhere in the issuer claims (RFC 9901, Section 6.1).
	ErrUnusedDisclosure = errors.New("sdjwt: disclosure not referenced by any SD digest")

	// ErrDuplicateClaimName indicates that a disclosed claim collides with an
	// already visible claim name or another disclosed claim in the same object
	// (RFC 9901, Sections 4.4 and 6.1).
	ErrDuplicateClaimName = errors.New("sdjwt: duplicate claim name")

	// ErrKeyBindingRequired indicates a Presentation carrying a Key-Binding
	// JWT that was handed to Verify without expected holder parameters, or
	// an expected holder binding that is missing.
	ErrKeyBindingRequired = errors.New("sdjwt: holder key binding required")

	// ErrKeyBinding indicates a malformed or invalid Key-Binding JWT.
	ErrKeyBinding = errors.New("sdjwt: invalid key binding JWT")
)

const (
	sdClaimKey       = "_sd"
	sdAlgClaimKey    = "_sd_alg"
	arrayElisionMark = "..."
)

// Disclosure is a decoded Disclosure object (RFC 9901, Section 4.1).
//
// For an object property disclosure, Name is the claim name and Value is the
// disclosed claim value. For an array element disclosure, Name is empty and
// Value holds the disclosed array element.
type Disclosure struct {
	// Salt is the random salt from the disclosure array.
	Salt string
	// Name is the disclosed claim name for object properties; empty for
	// array element disclosures.
	Name string
	// Value is the disclosed claim value or array element.
	Value interface{}

	// raw is the base64url-encoded disclosure as transmitted.
	raw string
	// rawJSON is the decoded disclosure JSON, used for hashing.
	rawJSON []byte
}

// SDJWT is a parsed SD-JWT (without verification).
type SDJWT struct {
	// IssuerJWT is the Issuer-Signed JWT parsed from the first component.
	IssuerJWT *jose.JSONWebSignature
	// Disclosures are the decoded Disclosure objects in presentation order.
	Disclosures []*Disclosure
	// KeyBindingJWT is the raw Key-Binding JWT, if the input was a
	// Presentation with a holder binding appended.
	KeyBindingJWT string

	// issuerCompact is the raw compact JWS, needed to compute sd_hash.
	issuerCompact string
}

// UndisclosedClaim marks a digest that was present in the issuer claims but
// whose Disclosure was not included in the presentation.
type UndisclosedClaim struct {
	// Pointer is a JSON Pointer (RFC 6901) to the object or array that
	// contains the digest. The empty string points at the top-level claims
	// object. For an undisclosed array element the pointer addresses the
	// containing array; individual element positions are not exposed.
	Pointer string
	// Digest is the base64url-encoded digest from the "_sd" array.
	Digest string
}

// Claims is the result of a successful verification.
type Claims struct {
	// Claims holds the Issuer-Signed JWT claims with all successfully
	// processed Disclosures merged in. "_sd", "_sd_alg" and array elision
	// markers are removed. Undisclosed array elements are represented by
	// UndisclosedArrayElement values.
	Claims map[string]interface{}

	// Disclosed is the set of Disclosures that matched a digest and were
	// merged into Claims, in presentation order.
	Disclosed []*Disclosure

	// Undisclosed lists every digest in the issuer claims for which no
	// Disclosure was provided. This lets the caller tell selectively
	// undisclosed claims apart from claims the issuer never asserted.
	Undisclosed []UndisclosedClaim

	// KeyBindingVerified is true when a Key-Binding JWT was present and
	// verified successfully.
	KeyBindingVerified bool
}

// UndisclosedArrayElement represents one undisclosed (elided) array element in
// the verified claims. It serializes as the RFC 9901 array element marker
// ["..."].
type UndisclosedArrayElement struct{}

// MarshalJSON implements json.Marshaler.
func (UndisclosedArrayElement) MarshalJSON() ([]byte, error) {
	return []byte(`["` + arrayElisionMark + `"]`), nil
}

// Parse parses an SD-JWT or SD-JWT Presentation in combined format without
// verifying any signature or disclosure. Use SDJWT.Verify to validate it.
//
// The issuer JWS header "alg" must be present in signatureAlgorithms.
func Parse(combined string, signatureAlgorithms []jose.SignatureAlgorithm) (*SDJWT, error) {
	if len(combined) == 0 {
		return nil, ErrMalformedCombinedFormat
	}
	if strings.TrimSpace(combined) != combined {
		return nil, ErrMalformedCombinedFormat
	}

	parts := strings.Split(combined, "~")

	// The last component is the KB-JWT when the input ends with "~" and has
	// more than one component; otherwise it is an empty trailing element.
	var kb string
	if parts[len(parts)-1] != "" {
		if len(parts) == 1 {
			return nil, fmt.Errorf("%w: missing disclosure separators", ErrMalformedCombinedFormat)
		}
		kb = parts[len(parts)-1]
	}
	parts = parts[:len(parts)-1]

	if len(parts) == 0 || parts[0] == "" {
		return nil, fmt.Errorf("%w: empty issuer JWT", ErrMalformedCombinedFormat)
	}

	issuerJWT, err := jose.ParseSignedCompact(parts[0], signatureAlgorithms)
	if err != nil {
		return nil, fmt.Errorf("sdjwt: %w", err)
	}

	disclosures := make([]*Disclosure, 0, len(parts)-1)
	seenRaw := make(map[string]struct{}, len(parts)-1)
	for _, raw := range parts[1:] {
		if raw == "" {
			return nil, fmt.Errorf("%w: empty disclosure", ErrMalformedCombinedFormat)
		}
		if _, dup := seenRaw[raw]; dup {
			return nil, fmt.Errorf("%w: disclosure appears more than once", ErrDuplicateDigest)
		}
		seenRaw[raw] = struct{}{}

		d, err := parseDisclosure(raw)
		if err != nil {
			return nil, err
		}
		disclosures = append(disclosures, d)
	}

	return &SDJWT{
		IssuerJWT:     issuerJWT,
		Disclosures:   disclosures,
		KeyBindingJWT: kb,
		issuerCompact: parts[0],
	}, nil
}

func b64urlDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func parseDisclosure(raw string) (*Disclosure, error) {
	jsonBytes, err := b64urlDecode(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid base64url encoding: %v", ErrMalformedDisclosure, err)
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(jsonBytes, &arr); err != nil {
		return nil, fmt.Errorf("%w: not a JSON array: %v", ErrMalformedDisclosure, err)
	}
	if len(arr) != 2 && len(arr) != 3 {
		return nil, fmt.Errorf("%w: expected a JSON array of 2 or 3 elements", ErrMalformedDisclosure)
	}

	d := &Disclosure{raw: raw, rawJSON: jsonBytes}
	if err := json.Unmarshal(arr[0], &d.Salt); err != nil || d.Salt == "" {
		return nil, fmt.Errorf("%w: first element must be a non-empty salt string", ErrMalformedDisclosure)
	}

	if len(arr) == 3 {
		if err := json.Unmarshal(arr[1], &d.Name); err != nil {
			return nil, fmt.Errorf("%w: second element must be a claim name string", ErrMalformedDisclosure)
		}
		if isReservedClaimName(d.Name) {
			return nil, fmt.Errorf("%w: claim name %q is reserved", ErrMalformedDisclosure, d.Name)
		}
		if err := json.Unmarshal(arr[2], &d.Value); err != nil {
			return nil, fmt.Errorf("%w: invalid claim value", ErrMalformedDisclosure)
		}
	} else {
		if err := json.Unmarshal(arr[1], &d.Value); err != nil {
			return nil, fmt.Errorf("%w: invalid array element value", ErrMalformedDisclosure)
		}
	}

	return d, nil
}

func isReservedClaimName(name string) bool {
	return name == sdClaimKey || name == sdAlgClaimKey || name == arrayElisionMark
}

func hashForAlg(alg string) (crypto.Hash, int, error) {
	switch alg {
	case SHA256, "":
		return crypto.SHA256, sha256.Size, nil
	case SHA384:
		return crypto.SHA384, sha512.Size384, nil
	case SHA512:
		return crypto.SHA512, sha512.Size, nil
	default:
		return 0, 0, fmt.Errorf("%w: %q", ErrUnsupportedHashAlgorithm, alg)
	}
}

func digestOf(rawJSON []byte, hash crypto.Hash, size int) (string, error) {
	h := hash.New()
	h.Write(rawJSON)
	sum := h.Sum(nil)
	if len(sum) != size {
		return "", fmt.Errorf("sdjwt: internal error: unexpected digest size")
	}
	return base64URLEncode(sum), nil
}

func base64URLEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
