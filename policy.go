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
	"errors"
	"fmt"
	"strings"
)

// ErrPolicyNotSatisfied is returned by VerifyPolicy and DetachedVerifyPolicy
// when the verified signatures on a JWS object do not satisfy the given
// SignaturePolicy. The PolicyReport returned alongside the error describes
// exactly which principals voted and which signatures were rejected.
var ErrPolicyNotSatisfied = errors.New("go-jose/go-jose: signature policy not satisfied")

// TrustedPrincipal associates a caller-defined approver identity with the
// verification keys that belong to it. Votes are counted per principal, never
// per signature and never per "kid" header value: a principal casts at most
// one vote no matter how many signatures verify against its keys, and a
// signature is only attributed to a principal after it cryptographically
// verifies against one of that principal's keys.
type TrustedPrincipal struct {
	// ID is the caller-defined identity of the principal, e.g.
	// "security-officer". IDs must be unique within a policy check.
	ID string

	// Keys are the verification keys belonging to this principal. Each key
	// must have one of the types accepted by JSONWebSignature.Verify.
	Keys []interface{}
}

// SignaturePolicy describes a quorum rule over a set of trusted principals,
// e.g. "at least 2 of the 3 release approvers, and the security officer must
// be among them".
type SignaturePolicy struct {
	// MinApprovals is the minimum number of distinct trusted principals that
	// must have a verified signature. Must be at least 1.
	MinApprovals int

	// RequiredPrincipals lists IDs of principals that must have a verified
	// signature regardless of the quorum count. Every ID must appear in the
	// trusted principal list passed to the verification call.
	RequiredPrincipals []string
}

// RejectionReason explains why a signature did not count toward the policy.
type RejectionReason string

const (
	// ReasonUnverified means the signature did not verify against any key of
	// any trusted principal. This covers both signatures from unknown
	// (untrusted) signers and invalid/forged extra signatures; the two are
	// cryptographically indistinguishable without trusting unauthenticated
	// header data, so they are handled identically: they never count toward
	// the quorum and never satisfy a required principal, but they do not by
	// themselves fail an otherwise satisfied policy. They are surfaced in
	// PolicyReport.Rejected so callers can audit or alert on them.
	ReasonUnverified RejectionReason = "unverified"

	// ReasonDuplicate means the signature verified, but against keys of a
	// principal that had already cast its vote. Re-signing with the same key
	// under a different "kid", or the same principal signing twice, is
	// reported here and counts only once.
	ReasonDuplicate RejectionReason = "duplicate"
)

// RejectedSignature describes a single signature that did not count toward
// the policy. Only verified information is reported: Principal is set (for
// ReasonDuplicate only) when the signature cryptographically verified against
// that principal's keys.
type RejectedSignature struct {
	// Index is the position of the signature in JSONWebSignature.Signatures.
	Index int

	// Reason explains why the signature did not count.
	Reason RejectionReason

	// Principal is the verified identity of the signer, set only when Reason
	// is ReasonDuplicate. Empty for ReasonUnverified, because an unverified
	// signature has no trustworthy identity.
	Principal string
}

// PrincipalDecision reports the verified outcome for one trusted principal.
type PrincipalDecision struct {
	// ID is the caller-defined principal identity.
	ID string

	// Required indicates whether the policy mandates this principal's vote.
	Required bool

	// Approved is true only if at least one signature cryptographically
	// verified against one of this principal's keys.
	Approved bool

	// SignatureIndex is the index of the signature that verified for this
	// principal, or -1 when Approved is false.
	SignatureIndex int
}

// PolicyReport details the outcome of a policy verification. Every field is
// derived exclusively from cryptographically verified information; header
// values such as "kid" are never used to attribute a signature.
type PolicyReport struct {
	// Approvals is the number of distinct principals with a verified vote.
	Approvals int

	// Decisions holds one entry per trusted principal, in the order the
	// principals were passed to the verification call.
	Decisions []PrincipalDecision

	// Rejected lists the signatures that did not count, with the reason.
	Rejected []RejectedSignature
}

func validatePolicy(policy SignaturePolicy, principals []TrustedPrincipal) error {
	if policy.MinApprovals < 1 {
		return errors.New("go-jose/go-jose: MinApprovals must be at least 1")
	}
	seen := make(map[string]bool, len(principals))
	for _, p := range principals {
		if p.ID == "" {
			return errors.New("go-jose/go-jose: trusted principal with empty ID")
		}
		if seen[p.ID] {
			return fmt.Errorf("go-jose/go-jose: duplicate trusted principal ID %q", p.ID)
		}
		seen[p.ID] = true
		if len(p.Keys) == 0 {
			return fmt.Errorf("go-jose/go-jose: trusted principal %q has no keys", p.ID)
		}
	}
	for _, id := range policy.RequiredPrincipals {
		if !seen[id] {
			return fmt.Errorf("go-jose/go-jose: required principal %q is not in the trusted principal list", id)
		}
	}
	return nil
}

// VerifyPolicy verifies the signatures on the object against a quorum policy
// over caller-supplied trusted principals and returns the payload together
// with a report of the verified outcome.
//
// Votes are counted per principal based solely on successful cryptographic
// verification against that principal's keys: the number of signatures and
// the "kid" header values play no role in counting. Unknown or invalid extra
// signatures never count and never fail an otherwise satisfied policy; they
// are reported in PolicyReport.Rejected. If the policy is not satisfied, the
// returned error wraps ErrPolicyNotSatisfied and the report is still
// returned for auditing.
func (obj JSONWebSignature) VerifyPolicy(policy SignaturePolicy, principals []TrustedPrincipal) ([]byte, *PolicyReport, error) {
	return obj.DetachedVerifyPolicy(obj.payload, policy, principals)
}

// DetachedVerifyPolicy is like VerifyPolicy but verifies a detached payload.
// In most cases you will want to use VerifyPolicy instead.
func (obj JSONWebSignature) DetachedVerifyPolicy(payload []byte, policy SignaturePolicy, principals []TrustedPrincipal) ([]byte, *PolicyReport, error) {
	if err := validatePolicy(policy, principals); err != nil {
		return nil, nil, err
	}

	required := make(map[string]bool, len(policy.RequiredPrincipals))
	for _, id := range policy.RequiredPrincipals {
		required[id] = true
	}

	report := &PolicyReport{
		Decisions: make([]PrincipalDecision, len(principals)),
	}
	for i, p := range principals {
		report.Decisions[i] = PrincipalDecision{
			ID:             p.ID,
			Required:       required[p.ID],
			SignatureIndex: -1,
		}
	}

	voted := make([]bool, len(principals))

	for sigIdx := range obj.Signatures {
		signature := &obj.Signatures[sigIdx]
		matched := false
		for pIdx := range principals {
			for _, key := range principals[pIdx].Keys {
				if err := obj.verifySignature(payload, signature, key); err != nil {
					continue
				}
				// A signature counts for at most one principal, and a
				// principal casts at most one vote.
				matched = true
				if voted[pIdx] {
					report.Rejected = append(report.Rejected, RejectedSignature{
						Index:     sigIdx,
						Reason:    ReasonDuplicate,
						Principal: principals[pIdx].ID,
					})
				} else {
					voted[pIdx] = true
					report.Decisions[pIdx].Approved = true
					report.Decisions[pIdx].SignatureIndex = sigIdx
					report.Approvals++
				}
				break
			}
			if matched {
				break
			}
		}
		if !matched {
			report.Rejected = append(report.Rejected, RejectedSignature{
				Index:  sigIdx,
				Reason: ReasonUnverified,
			})
		}
	}

	var missing []string
	for _, decision := range report.Decisions {
		if decision.Required && !decision.Approved {
			missing = append(missing, decision.ID)
		}
	}

	if report.Approvals < policy.MinApprovals || len(missing) > 0 {
		var reasons []string
		if report.Approvals < policy.MinApprovals {
			reasons = append(reasons, fmt.Sprintf("only %d of %d required distinct approvals verified", report.Approvals, policy.MinApprovals))
		}
		if len(missing) > 0 {
			reasons = append(reasons, fmt.Sprintf("missing required principal(s): %s", strings.Join(missing, ", ")))
		}
		return nil, report, fmt.Errorf("%w: %s", ErrPolicyNotSatisfied, strings.Join(reasons, "; "))
	}

	return payload, report, nil
}
