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
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/go-jose/go-jose/v4/testutils/assert"
	"github.com/go-jose/go-jose/v4/testutils/require"
)

var policyPayload = []byte(`{"release":"v2.4.0","artifact":"app.tar.gz"}`)

// policyTestKey derives a deterministic Ed25519 key from a one-byte seed.
func policyTestKey(seed byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed([]byte{
		seed, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
		16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31,
	})
}

var (
	policyOpsKey      = policyTestKey(1)
	policyReleaseKey  = policyTestKey(2)
	policySecurityKey = policyTestKey(3)
	policyAttackerKey = policyTestKey(4)
	policyOutsiderKey = policyTestKey(5)

	policySignAlgs = []SignatureAlgorithm{EdDSA}

	policyTestPrincipals = []TrustedPrincipal{
		{ID: "ops-lead", Key: JSONWebKey{Key: policyOpsKey.Public(), KeyID: "ops-key-1"}},
		{ID: "release-mgr", Key: JSONWebKey{Key: policyReleaseKey.Public(), KeyID: "release-key-1"}},
		{ID: "security-officer", Key: JSONWebKey{Key: policySecurityKey.Public(), KeyID: "sec-key-1"}},
	}

	policyReleaseManifest = SignaturePolicy{
		MinApprovals: 2,
		Required:     []string{"security-officer"},
	}
)

func policySigningKey(priv ed25519.PrivateKey, kid string) SigningKey {
	return SigningKey{
		Algorithm: EdDSA,
		Key:       JSONWebKey{Key: priv, KeyID: kid},
	}
}

func policySign(t *testing.T, keys ...SigningKey) *JSONWebSignature {
	signer, err := NewMultiSigner(keys, nil)
	require.NoError(t, err)
	obj, err := signer.Sign(policyPayload)
	require.NoError(t, err)
	return obj
}

// policyRoundTrip serializes and re-parses the object so the test exercises
// the same code path as a verifier receiving the JWS over the wire.
func policyRoundTrip(t *testing.T, objs ...*JSONWebSignature) *JSONWebSignature {
	combined := &JSONWebSignature{payload: policyPayload}
	for _, obj := range objs {
		combined.Signatures = append(combined.Signatures, obj.Signatures...)
	}
	parsed, err := ParseSignedJSON(combined.FullSerialize(), policySignAlgs)
	require.NoError(t, err)
	return parsed
}

func policyDispositions(report PolicyReport) []SignatureDisposition {
	out := make([]SignatureDisposition, len(report.Details))
	for i, d := range report.Details {
		out[i] = d.Disposition
	}
	return out
}

// Normal case: two of three approvers sign, including the mandatory
// security officer.
func TestVerifyPolicyTwoApprovals(t *testing.T) {
	obj := policyRoundTrip(t, policySign(t,
		policySigningKey(policyOpsKey, "ops-key-1"),
		policySigningKey(policySecurityKey, "sec-key-1"),
	))

	payload, report, err := obj.VerifyPolicy(policyReleaseManifest, policyTestPrincipals)
	require.NoError(t, err)
	assert.Equal(t, string(policyPayload), string(payload))
	assert.Equal(t, true, report.Approved)
	assert.EqualSlice(t, []string{"ops-lead", "security-officer"}, report.Approvals)
	assert.Equal(t, 0, len(report.Missing))
	assert.EqualSlice(t,
		[]SignatureDisposition{DispositionCounted, DispositionCounted},
		policyDispositions(report))
	assert.Equal(t, "ops-lead", report.Details[0].Principal)
	assert.Equal(t, "security-officer", report.Details[1].Principal)
}

// Forged duplicates: the same key is presented under two different "kid"
// values, and the attacker adds a signature that claims the ops lead's "kid"
// but is made with the attacker's own key. Only one real vote exists, so the
// quorum is not met.
func TestVerifyPolicyForgedDuplicateVotes(t *testing.T) {
	obj := policyRoundTrip(t,
		// The security officer's key signs twice, each time with a
		// different kid, trying to look like two distinct approvers.
		policySign(t,
			policySigningKey(policySecurityKey, "sec-key-1"),
			policySigningKey(policySecurityKey, "sec-key-2"),
		),
		// A forgery that claims the ops lead's kid but is signed with the
		// attacker's key.
		policySign(t, policySigningKey(policyAttackerKey, "ops-key-1")),
	)

	payload, report, err := obj.VerifyPolicy(policyReleaseManifest, policyTestPrincipals)
	assert.ErrorIs(t, err, ErrPolicyNotSatisfied)
	assert.Equal(t, true, payload == nil)
	assert.Equal(t, false, report.Approved)
	assert.EqualSlice(t, []string{"security-officer"}, report.Approvals)
	assert.EqualSlice(t,
		[]SignatureDisposition{DispositionCounted, DispositionDuplicate, DispositionInvalid},
		policyDispositions(report))
	// The invalid forgery must not be attributed to anyone.
	assert.Equal(t, "", report.Details[2].Principal)
}

// Same key material registered under two different principals must still
// yield only one vote.
func TestVerifyPolicySameKeyTwoPrincipals(t *testing.T) {
	principals := []TrustedPrincipal{
		{ID: "security-officer", Key: JSONWebKey{Key: policySecurityKey.Public(), KeyID: "sec-key-1"}},
		{ID: "sec-alias", Key: JSONWebKey{Key: policySecurityKey.Public(), KeyID: "sec-key-2"}},
		{ID: "ops-lead", Key: JSONWebKey{Key: policyOpsKey.Public(), KeyID: "ops-key-1"}},
	}
	policy := SignaturePolicy{MinApprovals: 2, Required: []string{"security-officer"}}

	obj := policyRoundTrip(t, policySign(t,
		policySigningKey(policySecurityKey, "sec-key-1"),
		policySigningKey(policySecurityKey, "sec-key-2"),
	))

	_, report, err := obj.VerifyPolicy(policy, principals)
	assert.ErrorIs(t, err, ErrPolicyNotSatisfied)
	assert.Equal(t, false, report.Approved)
	assert.EqualSlice(t, []string{"security-officer"}, report.Approvals)
	assert.EqualSlice(t,
		[]SignatureDisposition{DispositionCounted, DispositionDuplicate},
		policyDispositions(report))
}

// Two approvals are present, but the mandatory security officer is missing.
func TestVerifyPolicyMissingRequiredSigner(t *testing.T) {
	obj := policyRoundTrip(t, policySign(t,
		policySigningKey(policyOpsKey, "ops-key-1"),
		policySigningKey(policyReleaseKey, "release-key-1"),
	))

	payload, report, err := obj.VerifyPolicy(policyReleaseManifest, policyTestPrincipals)
	assert.ErrorIs(t, err, ErrPolicyNotSatisfied)
	assert.Equal(t, true, payload == nil)
	assert.Equal(t, false, report.Approved)
	assert.EqualSlice(t, []string{"ops-lead", "release-mgr"}, report.Approvals)
	assert.EqualSlice(t, []string{"security-officer"}, report.Missing)
}

// A signature from an untrusted key is reported as unknown and neither
// counts nor fails the evaluation on its own.
func TestVerifyPolicyUnknownExtraSignature(t *testing.T) {
	obj := policyRoundTrip(t,
		policySign(t,
			policySigningKey(policyOpsKey, "ops-key-1"),
			policySigningKey(policySecurityKey, "sec-key-1"),
		),
		policySign(t, policySigningKey(policyOutsiderKey, "outsider-key")),
	)

	_, report, err := obj.VerifyPolicy(policyReleaseManifest, policyTestPrincipals)
	require.NoError(t, err)
	assert.Equal(t, true, report.Approved)
	assert.EqualSlice(t, []string{"ops-lead", "security-officer"}, report.Approvals)
	assert.EqualSlice(t,
		[]SignatureDisposition{DispositionCounted, DispositionCounted, DispositionUnknown},
		policyDispositions(report))
	assert.Equal(t, "", report.Details[2].Principal)
}

func TestVerifyPolicyConfigValidation(t *testing.T) {
	obj := policyRoundTrip(t, policySign(t, policySigningKey(policyOpsKey, "ops-key-1")))

	_, _, err := obj.VerifyPolicy(SignaturePolicy{MinApprovals: 0}, policyTestPrincipals)
	assert.Error(t, err)

	_, _, err = obj.VerifyPolicy(policyReleaseManifest, nil)
	assert.Error(t, err)

	_, _, err = obj.VerifyPolicy(SignaturePolicy{MinApprovals: 1, Required: []string{"ghost"}}, policyTestPrincipals)
	assert.Error(t, err)

	dup := append(append([]TrustedPrincipal{}, policyTestPrincipals...), policyTestPrincipals[0])
	_, _, err = obj.VerifyPolicy(policyReleaseManifest, dup)
	assert.Error(t, err)

	// Configuration errors must not be reported as policy failures.
	assert.Equal(t, false, errors.Is(err, ErrPolicyNotSatisfied))
}
