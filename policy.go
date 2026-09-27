/*-
 * Copyright 2014 Square Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package jose

import (
	"crypto"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrPolicyNotSatisfied is returned by VerifyPolicy and DetachedVerifyPolicy
// when the signatures on a JWS object do not satisfy the given policy. The
// returned PolicyReport still describes every signature that was evaluated.
var ErrPolicyNotSatisfied = errors.New("go-jose/go-jose: signature policy not satisfied")

// TrustedPrincipal binds a stable identity (ID) to one verification key, as
// declared by the caller. Policy decisions are made in terms of these
// identities only: a signature is attributed to a principal exclusively by
// cryptographically verifying it against the principal's key. The "kid"
// header of a signature is never used to attribute or count votes.
//
// Key accepts the same types as the verificationKey argument of
// JSONWebSignature.Verify().
type TrustedPrincipal struct {
	ID  string
	Key interface{}
}

// SignaturePolicy describes how many distinct principals must have verifiably
// signed a JWS object, and which of them are mandatory.
type SignaturePolicy struct {
	// MinApprovals is the minimum number of distinct trusted principals that
	// must have a verified signature. Must be at least 1.
	MinApprovals int

	// Required lists principal IDs that must be among the verified signers,
	// regardless of how many other approvals are present.
	Required []string
}

// SignatureDisposition describes the outcome of evaluating a single
// signature of a JWS object against a policy.
type SignatureDisposition string

const (
	// DispositionCounted means the signature was cryptographically verified
	// against a trusted principal's key and contributed that principal's vote.
	DispositionCounted SignatureDisposition = "counted"

	// DispositionDuplicate means the signature was cryptographically verified
	// against a trusted key, but the same principal or the same key material
	// already contributed a vote. It does not count again. This covers a key
	// being re-presented under a different "kid" and a principal signing twice.
	DispositionDuplicate SignatureDisposition = "duplicate"

	// DispositionUnknown means the signature could not be verified against any
	// trusted principal's key and does not claim (via an unverified "kid"
	// header) any known key ID. It contributes no vote.
	DispositionUnknown SignatureDisposition = "unknown"

	// DispositionInvalid means the signature claims (via an unverified "kid"
	// header) the key ID of a trusted principal but failed cryptographic
	// verification against every trusted key. It contributes no vote.
	DispositionInvalid SignatureDisposition = "invalid"
)

// SignatureDetail describes the verified outcome for one signature of the
// evaluated JWS object. Only information established by cryptographic
// verification is recorded: Principal is set exclusively when the signature
// was verified against that principal's key. Unverified header values such as
// "kid" are never echoed back here.
type SignatureDetail struct {
	// Index is the position of the signature in JSONWebSignature.Signatures.
	Index int `json:"index"`

	// Disposition is the verified outcome of evaluating this signature.
	Disposition SignatureDisposition `json:"disposition"`

	// Principal is the ID of the trusted principal whose key verified this
	// signature. Empty for unknown and invalid signatures.
	Principal string `json:"principal,omitempty"`
}

// PolicyReport is the outcome of evaluating a JWS object against a
// SignaturePolicy. Every field reflects only cryptographically verified
// information.
type PolicyReport struct {
	// Approved is true when the policy is satisfied.
	Approved bool `json:"approved"`

	// Approvals lists the IDs of the distinct trusted principals whose
	// signatures were verified and counted, in signature order.
	Approvals []string `json:"approvals"`

	// Missing lists required principals that have no verified signature.
	Missing []string `json:"missing,omitempty"`

	// Details has one entry per signature of the JWS object, in order.
	Details []SignatureDetail `json:"details"`
}

// VerifyPolicy verifies the signatures on the object against the given policy
// and trusted principals, and returns the payload if and only if the policy
// is satisfied. Use DetachedVerifyPolicy for detached payloads.
//
// Votes are counted per principal, not per signature: multiple signatures
// from the same principal, or signatures made with key material that already
// contributed a vote (even under a different "kid"), count only once.
// Signatures that do not verify against any trusted key are reported as
// unknown or invalid and never count. See PolicyReport for the per-signature
// breakdown.
func (obj JSONWebSignature) VerifyPolicy(policy SignaturePolicy, principals []TrustedPrincipal) ([]byte, PolicyReport, error) {
	report, err := obj.DetachedVerifyPolicy(obj.payload, policy, principals)
	if err != nil {
		return nil, report, err
	}
	return obj.payload, report, nil
}

// DetachedVerifyPolicy verifies the signatures on the object against the
// given payload, policy and trusted principals. It returns a report
// describing every signature; the error is ErrPolicyNotSatisfied when the
// policy is not met, or a configuration error when the policy or the
// principal list itself is invalid.
func (obj JSONWebSignature) DetachedVerifyPolicy(payload []byte, policy SignaturePolicy, principals []TrustedPrincipal) (PolicyReport, error) {
	if err := validatePolicyConfig(policy, principals); err != nil {
		return PolicyReport{}, err
	}

	verifiers := make([]payloadVerifier, len(principals))
	fingerprints := make([]string, len(principals))
	knownKeyIDs := map[string]struct{}{}
	for i, p := range principals {
		verifier, err := newVerifier(p.Key)
		if err != nil {
			return PolicyReport{}, fmt.Errorf("go-jose/go-jose: invalid key for principal %q: %w", p.ID, err)
		}
		verifiers[i] = verifier
		fingerprints[i] = principalKeyFingerprint(p, i)
		if kid := principalKeyID(p.Key); kid != "" {
			knownKeyIDs[kid] = struct{}{}
		}
	}

	report := PolicyReport{
		Approvals: []string{},
		Details:   make([]SignatureDetail, 0, len(obj.Signatures)),
	}

	countedPrincipals := map[string]struct{}{}
	countedKeys := map[string]struct{}{}

	for i := range obj.Signatures {
		signature := &obj.Signatures[i]
		detail := SignatureDetail{Index: i}

		matched := -1
		for j := range principals {
			if obj.verifySignatureWithVerifier(payload, signature, verifiers[j]) == nil {
				matched = j
				break
			}
		}

		switch {
		case matched < 0:
			// The signature verified against no trusted key. The "kid" header
			// is unverified and only used as a hint to distinguish a forgery
			// or corruption aimed at a known key ID from a signature by an
			// entirely unknown key. Neither counts as a vote.
			if _, ok := knownKeyIDs[signature.Header.KeyID]; ok {
				detail.Disposition = DispositionInvalid
			} else {
				detail.Disposition = DispositionUnknown
			}
		case alreadyCounted(countedPrincipals, countedKeys, principals[matched].ID, fingerprints[matched]):
			detail.Disposition = DispositionDuplicate
			detail.Principal = principals[matched].ID
		default:
			detail.Disposition = DispositionCounted
			detail.Principal = principals[matched].ID
			countedPrincipals[principals[matched].ID] = struct{}{}
			countedKeys[fingerprints[matched]] = struct{}{}
			report.Approvals = append(report.Approvals, principals[matched].ID)
		}

		report.Details = append(report.Details, detail)
	}

	for _, required := range policy.Required {
		if _, ok := countedPrincipals[required]; !ok {
			report.Missing = append(report.Missing, required)
		}
	}

	report.Approved = len(report.Approvals) >= policy.MinApprovals && len(report.Missing) == 0
	if !report.Approved {
		return report, ErrPolicyNotSatisfied
	}
	return report, nil
}

func validatePolicyConfig(policy SignaturePolicy, principals []TrustedPrincipal) error {
	if policy.MinApprovals < 1 {
		return errors.New("go-jose/go-jose: policy MinApprovals must be at least 1")
	}
	if len(principals) == 0 {
		return errors.New("go-jose/go-jose: policy requires at least one trusted principal")
	}

	seen := make(map[string]struct{}, len(principals))
	for _, p := range principals {
		if p.ID == "" {
			return errors.New("go-jose/go-jose: trusted principal with empty ID")
		}
		if _, ok := seen[p.ID]; ok {
			return fmt.Errorf("go-jose/go-jose: duplicate trusted principal ID %q", p.ID)
		}
		seen[p.ID] = struct{}{}
	}

	for _, required := range policy.Required {
		if _, ok := seen[required]; !ok {
			return fmt.Errorf("go-jose/go-jose: required principal %q is not a trusted principal", required)
		}
	}
	return nil
}

func alreadyCounted(countedPrincipals, countedKeys map[string]struct{}, id, fingerprint string) bool {
	if _, ok := countedPrincipals[id]; ok {
		return true
	}
	_, ok := countedKeys[fingerprint]
	return ok
}

// verifySignatureWithVerifier verifies a single signature of the object
// against a single, already-constructed verifier.
func (obj JSONWebSignature) verifySignatureWithVerifier(payload []byte, signature *Signature, verifier payloadVerifier) error {
	if signature.header != nil {
		// Per RFC 7515 Section 4.1.11, "crit" must be integrity protected and
		// therefore must not appear in the unprotected header.
		if err := signature.header.checkNoCritical(); err != nil {
			return err
		}
	}

	if signature.protected != nil {
		if err := signature.protected.checkSupportedCritical(supportedCritical); err != nil {
			return err
		}
	}

	input, err := obj.computeAuthData(payload, signature)
	if err != nil {
		return err
	}

	alg := signature.mergedHeaders().getSignatureAlgorithm()
	return verifier.verifyPayload(input, signature.Signature, alg)
}

// principalKeyFingerprint derives a stable fingerprint of a principal's key
// material, used to detect the same key being presented more than once (for
// example under two different "kid" values). For asymmetric keys this is the
// JWK thumbprint (RFC 7638); for HMAC keys it is the SHA-256 of the raw key.
// Opaque verifiers expose no key material, so they fall back to a
// per-principal token: they can still be deduplicated per principal, but not
// across principals.
func principalKeyFingerprint(p TrustedPrincipal, index int) string {
	fingerprint, err := keyFingerprint(p.Key)
	if err != nil {
		return fmt.Sprintf("principal:%d:%s", index, p.ID)
	}
	return fingerprint
}

func keyFingerprint(key interface{}) (string, error) {
	if hmacKey, ok := key.([]byte); ok {
		sum := sha256.Sum256(hmacKey)
		return "hmac:" + base64.RawURLEncoding.EncodeToString(sum[:]), nil
	}

	var jwk JSONWebKey
	switch k := key.(type) {
	case JSONWebKey:
		jwk = k
	case *JSONWebKey:
		if k == nil {
			return "", errInvalidVerificationKey
		}
		jwk = *k
	default:
		jwk = JSONWebKey{Key: key}
	}

	thumbprint, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", err
	}
	return "jwk:" + base64.RawURLEncoding.EncodeToString(thumbprint), nil
}

// principalKeyID extracts the key ID of a principal's key when it is given as
// a JSONWebKey. It is only used to classify non-verifying signatures, never
// to attribute votes.
func principalKeyID(key interface{}) string {
	switch k := key.(type) {
	case JSONWebKey:
		return k.KeyID
	case *JSONWebKey:
		if k != nil {
			return k.KeyID
		}
	}
	return ""
}
