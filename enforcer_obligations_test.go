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
	"reflect"
	"strings"
	"testing"
)

func testEnforceWithObligations(t *testing.T, e *Enforcer, rvals []interface{}, wantAllowed bool, wantObligations Obligations) {
	t.Helper()

	allowed, obligations, err := e.EnforceWithObligations(rvals...)
	if err != nil {
		t.Fatalf("EnforceWithObligations(%v) returned error: %v", rvals, err)
	}
	if allowed != wantAllowed {
		t.Errorf("EnforceWithObligations(%v) allowed = %v, want %v", rvals, allowed, wantAllowed)
	}
	if !reflect.DeepEqual(obligations, wantObligations) {
		t.Errorf("EnforceWithObligations(%v) obligations = %v, want %v", rvals, obligations, wantObligations)
	}
}

// Two allow rules matched by the same request both take effect under
// allow-override, so their obligations are merged into one result.
func TestObligationsMergedFromAllowRules(t *testing.T) {
	e, err := NewEnforcer("examples/obligations_model.conf", "examples/obligations_policy.csv")
	if err != nil {
		t.Fatal(err)
	}

	testEnforceWithObligations(t, e, []interface{}{"alice", "data1", "export"}, true, Obligations{
		"mask":      {"ssn"},
		"watermark": {"owner"},
	})

	// A rule that declares no obligations contributes an empty set.
	testEnforceWithObligations(t, e, []interface{}{"alice", "data1", "read"}, true, Obligations{})

	// Bob only matches a deny rule: not allowed, no obligations.
	testEnforceWithObligations(t, e, []interface{}{"bob", "data1", "export"}, false, nil)
}

// Under the priority effect a higher-priority deny shadows the matching allow
// rule, so the request is denied and the shadowed rule's obligation ("mask")
// is never returned.
func TestObligationsShadowedByHigherPriorityDeny(t *testing.T) {
	e, err := NewEnforcer("examples/obligations_priority_model.conf", "examples/obligations_priority_policy.csv")
	if err != nil {
		t.Fatal(err)
	}

	testEnforceWithObligations(t, e, []interface{}{"alice", "data1", "export"}, false, nil)

	// Bob matches a single allow rule: its obligation is returned.
	testEnforceWithObligations(t, e, []interface{}{"bob", "data2", "read"}, true, Obligations{
		"mask": {"watermark"},
	})
}

// Two effective allow rules impose mutually exclusive values for "mask".
// By default the call fails explicitly; with ObligationsConflictUnion the
// values are merged instead of silently overwritten.
func TestObligationsConflict(t *testing.T) {
	e, err := NewEnforcer("examples/obligations_model.conf", "examples/obligations_conflict_policy.csv")
	if err != nil {
		t.Fatal(err)
	}

	allowed, obligations, err := e.EnforceWithObligations("alice", "data1", "export")
	if err == nil {
		t.Fatalf("expected a conflict error, got allowed=%v obligations=%v", allowed, obligations)
	}
	if !strings.Contains(err.Error(), `conflicting obligations for "mask"`) {
		t.Fatalf("unexpected conflict error: %v", err)
	}

	e.SetObligationsConflictStrategy(ObligationsConflictUnion)
	testEnforceWithObligations(t, e, []interface{}{"alice", "data1", "export"}, true, Obligations{
		"mask": {"ssn", "none"},
	})
}
