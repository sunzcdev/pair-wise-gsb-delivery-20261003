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
	"fmt"

	"github.com/go-jose/go-jose/v4/json"
)

// Demonstrates the release-manifest policy: at least two of the three
// approvers must sign, and the security officer's signature is mandatory.
func ExampleJSONWebSignature_VerifyPolicy() {
	principals := []TrustedPrincipal{
		{ID: "ops-lead", Key: JSONWebKey{Key: policyOpsKey.Public(), KeyID: "ops-key-1"}},
		{ID: "release-mgr", Key: JSONWebKey{Key: policyReleaseKey.Public(), KeyID: "release-key-1"}},
		{ID: "security-officer", Key: JSONWebKey{Key: policySecurityKey.Public(), KeyID: "sec-key-1"}},
	}
	policy := SignaturePolicy{MinApprovals: 2, Required: []string{"security-officer"}}

	sign := func(keys ...SigningKey) *JSONWebSignature {
		signer, err := NewMultiSigner(keys, nil)
		if err != nil {
			panic(err)
		}
		obj, err := signer.Sign(policyPayload)
		if err != nil {
			panic(err)
		}
		return obj
	}
	combine := func(objs ...*JSONWebSignature) *JSONWebSignature {
		combined := &JSONWebSignature{payload: policyPayload}
		for _, obj := range objs {
			combined.Signatures = append(combined.Signatures, obj.Signatures...)
		}
		parsed, err := ParseSignedJSON(combined.FullSerialize(), []SignatureAlgorithm{EdDSA})
		if err != nil {
			panic(err)
		}
		return parsed
	}
	show := func(label string, obj *JSONWebSignature) {
		_, report, err := obj.VerifyPolicy(policy, principals)
		out, _ := json.Marshal(report)
		fmt.Printf("%s: err=%v report=%s\n", label, err, out)
	}

	// 1. Normal: ops lead and security officer sign.
	show("normal two votes", combine(sign(
		policySigningKey(policyOpsKey, "ops-key-1"),
		policySigningKey(policySecurityKey, "sec-key-1"),
	)))

	// 2. Forged duplicates: the security officer's key is presented twice
	// under two different kids, plus a forgery claiming the ops lead's kid.
	show("forged duplicate votes", combine(
		sign(
			policySigningKey(policySecurityKey, "sec-key-1"),
			policySigningKey(policySecurityKey, "sec-key-2"),
		),
		sign(policySigningKey(policyAttackerKey, "ops-key-1")),
	))

	// 3. Missing mandatory signer: ops lead and release manager sign, but
	// the security officer did not.
	show("missing required signer", combine(sign(
		policySigningKey(policyOpsKey, "ops-key-1"),
		policySigningKey(policyReleaseKey, "release-key-1"),
	)))

	// Output:
	// normal two votes: err=<nil> report={"approved":true,"approvals":["ops-lead","security-officer"],"details":[{"index":0,"disposition":"counted","principal":"ops-lead"},{"index":1,"disposition":"counted","principal":"security-officer"}]}
	// forged duplicate votes: err=go-jose/go-jose: signature policy not satisfied report={"approved":false,"approvals":["security-officer"],"details":[{"index":0,"disposition":"counted","principal":"security-officer"},{"index":1,"disposition":"duplicate","principal":"security-officer"},{"index":2,"disposition":"invalid"}]}
	// missing required signer: err=go-jose/go-jose: signature policy not satisfied report={"approved":false,"approvals":["ops-lead","release-mgr"],"missing":["security-officer"],"details":[{"index":0,"disposition":"counted","principal":"ops-lead"},{"index":1,"disposition":"counted","principal":"release-mgr"}]}
}
