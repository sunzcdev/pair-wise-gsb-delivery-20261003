package sdjwt

import (
	"crypto"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/json"
)

// KeyBindingExpected carries the parameters a verifier requires of the holder
// Key-Binding JWT (RFC 9901, Section 4.3).
type KeyBindingExpected struct {
	// Nonce is the expected "nonce" claim. A non-empty value must be
	// supplied: without an expected nonce the presentation cannot be bound
	// to the verifier's challenge.
	Nonce string
	// Audience is the expected "aud" claim. It must be non-empty.
	Audience string
	// If Now is non-zero, "iat" must not be later than Now. If MaxAge is
	// greater than zero, "iat" must also be no older than MaxAge relative to
	// the reference time (time.Now is used when Now is zero).
	Now    time.Time
	MaxAge time.Duration

	// SignatureAlgorithms restricts the KB-JWS "alg" header. When nil, the
	// algorithms accepted by defaultKeyBindingAlgorithms are allowed.
	SignatureAlgorithms []jose.SignatureAlgorithm
}

// Verify validates the SD-JWT and returns the disclosed claims.
//
// It verifies the Issuer-Signed JWT against issuerKey, then processes the
// Disclosures: every provided Disclosure must match exactly one digest, every
// digest must reference at most one Disclosure, and a disclosed object claim
// must not already be visible under the same name in the same object.
//
// If the input carries a Key-Binding JWT, expected must be non-nil and the
// holder binding is verified against the "cnf.jwk" claim. When expected is
// non-nil, the input must carry a Key-Binding JWT.
func (s *SDJWT) Verify(issuerKey interface{}, expected *KeyBindingExpected) (*Claims, error) {
	payload, err := s.IssuerJWT.Verify(issuerKey)
	if err != nil {
		return nil, fmt.Errorf("sdjwt: issuer signature verification failed: %w", err)
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("%w: claims are not a JSON object: %v", ErrMalformedClaims, err)
	}
	if claims == nil {
		return nil, fmt.Errorf("%w: claims are not a JSON object", ErrMalformedClaims)
	}

	sdAlg := SHA256
	if rawAlg, ok := claims[sdAlgClaimKey]; ok {
		alg, ok := rawAlg.(string)
		if !ok {
			return nil, fmt.Errorf("%w: _sd_alg must be a string", ErrMalformedClaims)
		}
		sdAlg = alg
	}
	hash, hashSize, err := hashForAlg(sdAlg)
	if err != nil {
		return nil, err
	}

	// Index disclosures by digest. Two disclosures hashing to the same
	// digest would be ambiguous and are rejected.
	byDigest := make(map[string]*Disclosure, len(s.Disclosures))
	for _, d := range s.Disclosures {
		digest, err := digestOf(d.rawJSON, hash, hashSize)
		if err != nil {
			return nil, err
		}
		if _, exists := byDigest[digest]; exists {
			return nil, fmt.Errorf("%w: two disclosures share digest %s", ErrDuplicateDigest, digest)
		}
		byDigest[digest] = d
	}

	p := &processor{
		hash:     hash,
		hashSize: hashSize,
		byDigest: byDigest,
		used:     make(map[string]struct{}),
		out: &Claims{
			Claims: make(map[string]interface{}, len(claims)),
		},
	}

	processed, err := p.processObject(claims, "")
	if err != nil {
		return nil, err
	}
	p.out.Claims = processed

	if len(p.used) != len(s.Disclosures) {
		for _, d := range s.Disclosures {
			if _, used := p.used[d.raw]; !used {
				digest, _ := digestOf(d.rawJSON, hash, hashSize)
				return nil, fmt.Errorf("%w: digest %s", ErrUnusedDisclosure, digest)
			}
		}
	}

	if s.KeyBindingJWT != "" {
		if expected == nil {
			return nil, fmt.Errorf("%w: presentation contains a key binding JWT but no expected holder parameters were provided", ErrKeyBindingRequired)
		}
		if err := s.verifyKeyBinding(claims, expected, hash); err != nil {
			return nil, err
		}
		p.out.KeyBindingVerified = true
	} else if expected != nil {
		return nil, fmt.Errorf("%w: presentation does not contain a key binding JWT", ErrKeyBindingRequired)
	}

	return p.out, nil
}

type processor struct {
	hash     crypto.Hash
	hashSize int
	byDigest map[string]*Disclosure
	used     map[string]struct{}
	out      *Claims
}

func (p *processor) processValue(v interface{}, pointer string) (interface{}, error) {
	switch tv := v.(type) {
	case map[string]interface{}:
		return p.processObject(tv, pointer)
	case []interface{}:
		return p.processArray(tv, pointer)
	default:
		return v, nil
	}
}

func (p *processor) processObject(obj map[string]interface{}, pointer string) (map[string]interface{}, error) {
	result := make(map[string]interface{}, len(obj))

	if _, nestedAlg := obj[sdAlgClaimKey]; pointer != "" && nestedAlg {
		return nil, fmt.Errorf("%w: %q is only allowed at the top level", ErrMalformedClaims, sdAlgClaimKey)
	}

	sdRaw, hasSD := obj[sdClaimKey]

	for k, v := range obj {
		if k == sdClaimKey || k == sdAlgClaimKey {
			continue
		}
		processed, err := p.processValue(v, pointer+"/"+escapePointer(k))
		if err != nil {
			return nil, err
		}
		result[k] = processed
	}

	if hasSD {
		sdArray, ok := sdRaw.([]interface{})
		if !ok {
			return nil, fmt.Errorf("%w: %q must be an array of strings", ErrMalformedClaims, sdClaimKey)
		}
		for _, e := range sdArray {
			digest, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%w: %q entries must be strings", ErrMalformedClaims, sdClaimKey)
			}
			if err := checkDigestEncoding(digest, p.hashSize, p.hash); err != nil {
				return nil, err
			}

			d, found := p.byDigest[digest]
			if !found {
				// No Disclosure supplied for this digest: the claim stays
				// hidden but is recorded so the caller can distinguish it
				// from an absent claim.
				p.out.Undisclosed = append(p.out.Undisclosed, UndisclosedClaim{
					Pointer: pointer,
					Digest:  digest,
				})
				continue
			}

			if _, duplicate := p.used[d.raw]; duplicate {
				return nil, fmt.Errorf("%w: digest %s referenced more than once", ErrDuplicateDigest, digest)
			}

			// A two-element disclosure discloses an array element and may
			// only resolve an array marker digest.
			if d.Name == "" {
				return nil, fmt.Errorf("%w: array-element disclosure matched an object digest %s", ErrMalformedClaims, digest)
			}
			if _, exists := result[d.Name]; exists {
				return nil, fmt.Errorf("%w: disclosed claim %q collides with an existing claim", ErrDuplicateClaimName, d.Name)
			}

			processed, err := p.processValue(d.Value, pointer+"/"+escapePointer(d.Name))
			if err != nil {
				return nil, err
			}
			result[d.Name] = processed
			p.used[d.raw] = struct{}{}
			p.out.Disclosed = append(p.out.Disclosed, d)
		}
	}

	return result, nil
}

func (p *processor) processArray(arr []interface{}, pointer string) ([]interface{}, error) {
	result := make([]interface{}, 0, len(arr))

	for i, e := range arr {
		// The single-element array ["..."] is the output-side marker for an
		// undisclosed element; it is not a valid issuer-signed claim, where
		// element digests are always the object {"...": "<digest>"}.
		if marker, ok := e.([]interface{}); ok && len(marker) == 1 {
			if s, isString := marker[0].(string); isString && s == arrayElisionMark {
				return nil, fmt.Errorf("%w: %s marker is not valid in issuer claims", ErrMalformedClaims, `["..."]`)
			}
		}

		// Issuer-side array element digests are single-key objects
		// {"...": "<digest>"} (RFC 9901, Section 4.5).
		if marker, ok := e.(map[string]interface{}); ok && len(marker) == 1 {
			if digest, isString := marker[arrayElisionMark].(string); isString {
				if err := checkDigestEncoding(digest, p.hashSize, p.hash); err != nil {
					return nil, err
				}

				d, found := p.byDigest[digest]
				if !found {
					result = append(result, UndisclosedArrayElement{})
					p.out.Undisclosed = append(p.out.Undisclosed, UndisclosedClaim{
						Pointer: pointer + "/" + strconv.Itoa(i),
						Digest:  digest,
					})
					continue
				}

				if _, duplicate := p.used[d.raw]; duplicate {
					return nil, fmt.Errorf("%w: digest %s referenced more than once", ErrDuplicateDigest, digest)
				}
				if d.Name != "" {
					return nil, fmt.Errorf("%w: object-property disclosure matched an array element digest %s", ErrMalformedClaims, digest)
				}

				processed, err := p.processValue(d.Value, pointer+"/"+strconv.Itoa(i))
				if err != nil {
					return nil, err
				}
				result = append(result, processed)
				p.used[d.raw] = struct{}{}
				p.out.Disclosed = append(p.out.Disclosed, d)
				continue
			}
		}

		if s, isString := e.(string); isString && s == arrayElisionMark {
			return nil, fmt.Errorf("%w: bare %q array element marker", ErrMalformedClaims, arrayElisionMark)
		}

		processed, err := p.processValue(e, pointer+"/"+strconv.Itoa(i))
		if err != nil {
			return nil, err
		}
		result = append(result, processed)
	}

	return result, nil
}

func checkDigestEncoding(digest string, hashSize int, hash crypto.Hash) error {
	b, err := base64.RawURLEncoding.DecodeString(digest)
	if err != nil {
		return fmt.Errorf("%w: invalid base64url digest: %v", ErrMalformedClaims, err)
	}
	if len(b) != hashSize {
		return fmt.Errorf("%w: digest length %d does not match %s output of %d bytes", ErrMalformedClaims, len(b), hash, hashSize)
	}
	return nil
}

func escapePointer(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")
	return token
}
