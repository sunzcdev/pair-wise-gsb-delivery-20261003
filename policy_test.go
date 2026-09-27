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
	"crypto/rand"
	"errors"
	"testing"
)

var releaseManifestPayload = []byte(`{"release":"v2.4.0","artifact":"app.tar.gz"}`)

// releaseManifestPolicy requires at least 2 of the 3 approvers, and the
// security officer's signature must be present.
var releaseManifestPolicy = SignaturePolicy{
	MinApprovals:       2,
	RequiredPrincipals: []string{"security-officer"},
}

type approver struct {
	id  string
	pub ed25519.PublicKey
	key ed25519.PrivateKey
}

func newApprover(t *testing.T, id string) approver {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key for %s: %v", id, err)
	}
	return approver{id: id, pub: pub, key: key}
}

func trustedPrincipals(approvers ...approver) []TrustedPrincipal {
	principals := make([]TrustedPrincipal, len(approvers))
	for i, a := range approvers {
		principals[i] = TrustedPrincipal{ID: a.id, Keys: []interface{}{a.pub}}
	}
	return principals
}

func signManifest(t *testing.T, keys ...SigningKey) *JSONWebSignature {
	t.Helper()
	signer, err := NewMultiSigner(keys, nil)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}
	obj, err := signer.Sign(releaseManifestPayload)
	if err != nil {
		t.Fatalf("failed to sign: %v", err)
	}
	parsed, err := ParseSignedJSON(obj.FullSerialize(), []SignatureAlgorithm{EdDSA})
	if err != nil {
		t.Fatalf("failed to re-parse: %v", err)
	}
	return parsed
}

func logReport(t *testing.T, report *PolicyReport) {
	t.Helper()
	t.Logf("approvals (distinct verified principals): %d", report.Approvals)
	for _, d := range report.Decisions {
		t.Logf("  principal=%-18s required=%-5v approved=%-5v signatureIndex=%d", d.ID, d.Required, d.Approved, d.SignatureIndex)
	}
	for _, r := range report.Rejected {
		t.Logf("  rejected signature[%d]: reason=%s principal=%q", r.Index, r.Reason, r.Principal)
	}
}

// Normal case: security officer plus one other approver sign the manifest.
func TestVerifyPolicyNormalTwoVotes(t *testing.T) {
	releaseManager := newApprover(t, "release-manager")
	qaLead := newApprover(t, "qa-lead")
	securityOfficer := newApprover(t, "security-officer")

	obj := signManifest(t,
		SigningKey{Algorithm: EdDSA, Key: securityOfficer.key},
		SigningKey{Algorithm: EdDSA, Key: qaLead.key},
	)

	payload, report, err := obj.VerifyPolicy(releaseManifestPolicy, trustedPrincipals(releaseManager, qaLead, securityOfficer))
	if err != nil {
		t.Fatalf("expected policy to be satisfied, got: %v", err)
	}
	logReport(t, report)

	if string(payload) != string(releaseManifestPayload) {
		t.Errorf("unexpected payload: %s", payload)
	}
	if report.Approvals != 2 {
		t.Errorf("expected 2 approvals, got %d", report.Approvals)
	}
	if len(report.Rejected) != 0 {
		t.Errorf("expected no rejected signatures, got %+v", report.Rejected)
	}
	for _, d := range report.Decisions {
		switch d.ID {
		case "security-officer", "qa-lead":
			if !d.Approved {
				t.Errorf("expected %s to be approved", d.ID)
			}
		case "release-manager":
			if d.Approved {
				t.Errorf("expected release-manager to not be approved")
			}
		}
	}
}

// Forgery attempt: the security officer's key signs twice under two different
// "kid" values, and an attacker appends a signature from an untrusted key.
// None of that may inflate the vote count.
func TestVerifyPolicyForgedDuplicateVotes(t *testing.T) {
	releaseManager := newApprover(t, "release-manager")
	qaLead := newApprover(t, "qa-lead")
	securityOfficer := newApprover(t, "security-officer")
	attacker := newApprover(t, "attacker")

	obj := signManifest(t,
		// Same key, two different kids: must count as one vote.
		SigningKey{Algorithm: EdDSA, Key: JSONWebKey{Key: securityOfficer.key, KeyID: "sec-officer-key-1"}},
		SigningKey{Algorithm: EdDSA, Key: JSONWebKey{Key: securityOfficer.key, KeyID: "sec-officer-key-2"}},
		// Forged extra signature from an untrusted key: must not count.
		SigningKey{Algorithm: EdDSA, Key: JSONWebKey{Key: attacker.key, KeyID: "qa-lead"}},
	)

	payload, report, err := obj.VerifyPolicy(releaseManifestPolicy, trustedPrincipals(releaseManager, qaLead, securityOfficer))
	logReport(t, report)

	if !errors.Is(err, ErrPolicyNotSatisfied) {
		t.Fatalf("expected ErrPolicyNotSatisfied, got: %v", err)
	}
	if payload != nil {
		t.Errorf("expected nil payload on policy failure")
	}
	if report == nil {
		t.Fatalf("expected report even on policy failure")
	}
	if report.Approvals != 1 {
		t.Errorf("expected exactly 1 approval (duplicates/forgeries must not count), got %d", report.Approvals)
	}

	var duplicates, unverified int
	for _, r := range report.Rejected {
		switch r.Reason {
		case ReasonDuplicate:
			duplicates++
			if r.Principal != "security-officer" {
				t.Errorf("duplicate attributed to wrong principal: %q", r.Principal)
			}
		case ReasonUnverified:
			unverified++
			if r.Principal != "" {
				t.Errorf("unverified signature must not carry a principal, got %q", r.Principal)
			}
		}
	}
	if duplicates != 1 {
		t.Errorf("expected 1 duplicate rejection, got %d", duplicates)
	}
	if unverified != 1 {
		t.Errorf("expected 1 unverified rejection, got %d", unverified)
	}
}

// Quorum is met numerically, but the mandatory security officer is missing.
func TestVerifyPolicyMissingRequiredSigner(t *testing.T) {
	releaseManager := newApprover(t, "release-manager")
	qaLead := newApprover(t, "qa-lead")
	securityOfficer := newApprover(t, "security-officer")

	obj := signManifest(t,
		SigningKey{Algorithm: EdDSA, Key: releaseManager.key},
		SigningKey{Algorithm: EdDSA, Key: qaLead.key},
	)

	_, report, err := obj.VerifyPolicy(releaseManifestPolicy, trustedPrincipals(releaseManager, qaLead, securityOfficer))
	logReport(t, report)

	if !errors.Is(err, ErrPolicyNotSatisfied) {
		t.Fatalf("expected ErrPolicyNotSatisfied, got: %v", err)
	}
	if report.Approvals != 2 {
		t.Errorf("expected 2 approvals, got %d", report.Approvals)
	}
	for _, d := range report.Decisions {
		if d.ID == "security-officer" && (!d.Required || d.Approved) {
			t.Errorf("expected security-officer to be required and not approved, got %+v", d)
		}
	}
}
