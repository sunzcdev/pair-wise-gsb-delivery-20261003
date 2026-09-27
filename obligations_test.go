// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package casbin

import (
	"errors"
	"reflect"
	"testing"
)

func testEnforceWithObligations(t *testing.T, e *Enforcer, expectedOk bool, expectedObligations Obligations, rvals ...interface{}) {
	t.Helper()
	ok, obligations, err := e.EnforceWithObligations(rvals...)
	if err != nil {
		t.Fatalf("EnforceWithObligations(%v) returned error: %v", rvals, err)
	}
	if ok != expectedOk {
		t.Errorf("EnforceWithObligations(%v) ok = %v, expected %v", rvals, ok, expectedOk)
	}
	if !reflect.DeepEqual(obligations, expectedObligations) {
		t.Errorf("EnforceWithObligations(%v) obligations = %v, expected %v", rvals, obligations, expectedObligations)
	}
}

// Two allow rules matched by the same request merge their obligations.
func TestEnforceWithObligationsMerge(t *testing.T) {
	e, err := NewEnforcer("examples/obligations_model.conf", "examples/obligations_policy.csv")
	if err != nil {
		t.Fatal(err)
	}

	// p, alice, data1, read, allow, mask=ssn
	// p, alice, data1, read, allow, watermark=owner
	testEnforceWithObligations(t, e, true, Obligations{"mask": "ssn", "watermark": "owner"}, "alice", "data1", "read")

	// Identical values for the same field are deduplicated, not a conflict.
	testEnforceWithObligations(t, e, true, Obligations{"mask": "ssn"}, "carol", "data3", "read")

	// No rule matched: deny without obligations.
	testEnforceWithObligations(t, e, false, Obligations{}, "alice", "data2", "write")
}

// A higher-priority deny overrides allow rules: the decision is deny and the
// obligations of the overridden allow rules are discarded. A higher-priority
// allow wins alone: lower-priority rules do not contribute obligations.
func TestEnforceWithObligationsPriority(t *testing.T) {
	e, err := NewEnforcer("examples/obligations_priority_model.conf", "examples/obligations_priority_policy.csv")
	if err != nil {
		t.Fatal(err)
	}

	// p, 1, alice, data1, read, deny, -        (wins over priority 2)
	// p, 2, alice, data1, read, allow, mask=ssn
	testEnforceWithObligations(t, e, false, Obligations{}, "alice", "data1", "read")

	// p, 1, bob, data2, read, allow, watermark=owner  (wins over priority 2)
	// p, 2, bob, data2, read, allow, mask=ssn
	testEnforceWithObligations(t, e, true, Obligations{"watermark": "owner"}, "bob", "data2", "read")
}

// Mutually exclusive obligations on the same field fail explicitly by default,
// or are reconciled by a configured ObligationsMerger.
func TestEnforceWithObligationsConflict(t *testing.T) {
	e, err := NewEnforcer("examples/obligations_model.conf", "examples/obligations_policy.csv")
	if err != nil {
		t.Fatal(err)
	}

	// p, bob, data2, read, allow, mask=ssn
	// p, bob, data2, read, allow, mask=none
	ok, obligations, err := e.EnforceWithObligations("bob", "data2", "read")
	if err == nil {
		t.Fatal("expected an obligation conflict error, got nil")
	}
	var conflictErr *ObligationConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected *ObligationConflictError, got %T: %v", err, err)
	}
	if conflictErr.Field != "mask" || !reflect.DeepEqual(conflictErr.Values, []string{"ssn", "none"}) {
		t.Errorf("unexpected conflict details: %+v", conflictErr)
	}
	if ok || obligations != nil {
		t.Errorf("conflict should fail the request, got ok=%v obligations=%v", ok, obligations)
	}

	// With a configured merge rule the conflict is reconciled deterministically.
	e.SetObligationsMerger(JoinObligationsMerger("|"))
	testEnforceWithObligations(t, e, true, Obligations{"mask": "ssn|none"}, "bob", "data2", "read")
}
