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

package advisor

import (
	"reflect"
	"sort"
	"testing"

	casbin "github.com/casbin/casbin/v3"
)

const (
	modelPath  = "../examples/rbac_advisor_model.conf"
	policyPath = "../examples/rbac_advisor_policy.csv"
)

var allRequests = [][]interface{}{
	{"report", "read"},
	{"report", "write"},
	{"report", "approve"},
}

func newTestEnforcer(t *testing.T) *casbin.Enforcer {
	t.Helper()
	e, err := casbin.NewEnforcer(modelPath, policyPath)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	return e
}

func snapshot(t *testing.T, e *casbin.Enforcer) (policy [][]string, grouping [][]string) {
	t.Helper()
	policy, err := e.GetPolicy()
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	grouping, err = e.GetGroupingPolicy()
	if err != nil {
		t.Fatalf("GetGroupingPolicy: %v", err)
	}
	sortRules(policy)
	sortRules(grouping)
	return policy, grouping
}

func sortRules(rules [][]string) {
	sort.Slice(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
}

func roleSets(result *Result) [][]string {
	sets := make([][]string, len(result.Advice))
	for i, advice := range result.Advice {
		sets[i] = append([]string(nil), advice.Roles...)
	}
	return sets
}

// TestEquivalentSolutions checks two distinct minimum-cost combinations:
// {finance_requester, auditor} and {finance_requester, junior_auditor},
// where junior_auditor reaches the auditor permissions only via
// inheritance. Both cost 6.
func TestEquivalentSolutions(t *testing.T) {
	e := newTestEnforcer(t)
	beforeP, beforeG := snapshot(t, e)

	result, err := Suggest(e, Options{
		Subject:        "bob",
		CandidateRoles: []string{"finance_requester", "finance_approver", "auditor", "junior_auditor", "employee"},
		RoleCost: map[string]int{
			"finance_requester": 2,
			"finance_approver":  8,
			"auditor":           4,
			"junior_auditor":    4,
			"employee":          1,
		},
		Requests: allRequests,
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if result.Status != StatusFound {
		t.Fatalf("expected StatusFound, got %s", result.Status)
	}

	got := roleSets(result)
	want := [][]string{
		{"auditor", "finance_requester"},
		{"finance_requester", "junior_auditor"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for _, advice := range result.Advice {
		if advice.Cost != 6 {
			t.Fatalf("expected cost 6, got %d for %v", advice.Cost, advice.Roles)
		}
	}

	afterP, afterG := snapshot(t, e)
	if !reflect.DeepEqual(beforeP, afterP) || !reflect.DeepEqual(beforeG, afterG) {
		t.Fatal("the queried enforcer was mutated by Suggest")
	}
}

// TestMutuallyExclusiveRoles checks the SOD constraint: granting both
// finance_requester and finance_approver directly is forbidden, so the
// search falls through to the higher-priced finance_director role which
// holds all permissions on its own.
func TestMutuallyExclusiveRoles(t *testing.T) {
	e := newTestEnforcer(t)

	result, err := Suggest(e, Options{
		Subject:        "dave",
		CandidateRoles: []string{"finance_requester", "finance_approver", "finance_director"},
		RoleCost: map[string]int{
			"finance_requester": 2,
			"finance_approver":  2,
			"finance_director":  7,
		},
		Requests: allRequests,
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if result.Status != StatusFound {
		t.Fatalf("expected StatusFound, got %s", result.Status)
	}
	got := roleSets(result)
	want := [][]string{{"finance_director"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	if result.Advice[0].Cost != 7 {
		t.Fatalf("expected cost 7, got %d", result.Advice[0].Cost)
	}
}

// TestRolePreIsHandled checks that a rolePre constraint forces the
// prerequisite role into the combination even though it grants no needed
// permission by itself.
func TestRolePreIsHandled(t *testing.T) {
	e := newTestEnforcer(t)

	result, err := Suggest(e, Options{
		Subject:        "erin",
		CandidateRoles: []string{"finance_approver", "employee"},
		RoleCost: map[string]int{
			"finance_approver": 5,
			"employee":         1,
		},
		Requests: [][]interface{}{{"report", "approve"}},
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if result.Status != StatusFound {
		t.Fatalf("expected StatusFound, got %s", result.Status)
	}
	got := roleSets(result)
	want := [][]string{{"employee", "finance_approver"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	if result.Advice[0].Cost != 6 {
		t.Fatalf("expected cost 6, got %d", result.Advice[0].Cost)
	}
}

// TestExplicitDenyIsUnsatisfiable proves a deny policy cannot be worked
// around by granting more roles: carol has a direct deny on report
// approve, and the advisor never removes deny policies.
func TestExplicitDenyIsUnsatisfiable(t *testing.T) {
	e := newTestEnforcer(t)
	beforeP, beforeG := snapshot(t, e)

	result, err := Suggest(e, Options{
		Subject:        "carol",
		CandidateRoles: []string{"finance_approver", "finance_director", "auditor", "junior_auditor"},
		RoleCost: map[string]int{
			"finance_approver": 1,
			"finance_director": 1,
			"auditor":          1,
			"junior_auditor":   1,
		},
		Requests: [][]interface{}{{"report", "approve"}},
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if result.Status != StatusUnsatisfiable {
		t.Fatalf("expected StatusUnsatisfiable, got %s (%v)", result.Status, result.Advice)
	}
	if len(result.Advice) != 0 {
		t.Fatalf("expected no advice, got %v", result.Advice)
	}

	afterP, afterG := snapshot(t, e)
	if !reflect.DeepEqual(beforeP, afterP) || !reflect.DeepEqual(beforeG, afterG) {
		t.Fatal("the queried enforcer was mutated by Suggest")
	}
}

// TestBudgetExhausted verifies budget exhaustion is reported separately
// from proven unsatisfiability. The denied request is genuinely
// unsolvable, but a two-evaluation budget cannot finish the proof.
func TestBudgetExhausted(t *testing.T) {
	e := newTestEnforcer(t)

	result, err := Suggest(e, Options{
		Subject: "carol",
		CandidateRoles: []string{
			"finance_requester", "finance_approver", "auditor",
			"junior_auditor", "finance_director",
		},
		RoleCost: map[string]int{
			"finance_requester": 1,
			"finance_approver":  1,
			"auditor":           1,
			"junior_auditor":    1,
			"finance_director":  1,
		},
		Requests: [][]interface{}{{"report", "approve"}},
		Budget:   2,
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if result.Status != StatusBudgetExhausted {
		t.Fatalf("expected StatusBudgetExhausted, got %s", result.Status)
	}
	if result.Evaluations != 3 {
		t.Fatalf("expected 3 evaluations, got %d", result.Evaluations)
	}
}

// TestAlreadyHeldRolesAreFree checks that roles the subject already holds,
// including through inheritance, are not paid for or re-granted.
func TestAlreadyHeldRolesAreFree(t *testing.T) {
	e := newTestEnforcer(t)

	result, err := Suggest(e, Options{
		Subject:        "dave",
		CandidateRoles: []string{"employee", "auditor"},
		RoleCost: map[string]int{
			"employee": 1,
			"auditor":  5,
		},
		Requests: [][]interface{}{{"profile", "read"}},
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if result.Status != StatusFound {
		t.Fatalf("expected StatusFound, got %s", result.Status)
	}
	if len(result.Advice) != 1 || len(result.Advice[0].Roles) != 0 || result.Advice[0].Cost != 0 {
		t.Fatalf("expected single empty zero-cost advice, got %+v", result.Advice)
	}
}

// TestApplyAdviceOnCopy applies each suggested combination to a detached
// enforcer copy and confirms the requests then pass, while the original
// enforcer stays unchanged.
func TestApplyAdviceOnCopy(t *testing.T) {
	e := newTestEnforcer(t)
	beforeP, beforeG := snapshot(t, e)

	result, err := Suggest(e, Options{
		Subject:        "bob",
		CandidateRoles: []string{"finance_requester", "auditor", "junior_auditor"},
		RoleCost: map[string]int{
			"finance_requester": 2,
			"auditor":           4,
			"junior_auditor":    4,
		},
		Requests: allRequests,
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if len(result.Advice) != 2 {
		t.Fatalf("expected 2 equivalent solutions, got %d", len(result.Advice))
	}

	for _, advice := range result.Advice {
		applied, err := ApplyOnCopy(e, advice)
		if err != nil {
			t.Fatalf("ApplyOnCopy(%v): %v", advice.Roles, err)
		}
		for _, request := range []struct{ obj, act string }{
			{"report", "read"},
			{"report", "write"},
			{"report", "approve"},
		} {
			ok, err := applied.Enforce("bob", request.obj, request.act)
			if err != nil {
				t.Fatalf("Enforce after applying %v: %v", advice.Roles, err)
			}
			if !ok {
				t.Fatalf("%s/%s still denied after applying %v", request.obj, request.act, advice.Roles)
			}
		}

		afterP, afterG := snapshot(t, e)
		if !reflect.DeepEqual(beforeP, afterP) || !reflect.DeepEqual(beforeG, afterG) {
			t.Fatalf("original enforcer mutated while applying %v", advice.Roles)
		}
	}
}

// TestMissingRoleCostIsRejected validates option validation.
func TestMissingRoleCostIsRejected(t *testing.T) {
	e := newTestEnforcer(t)
	_, err := Suggest(e, Options{
		Subject:        "carol",
		CandidateRoles: []string{"auditor"},
		RoleCost:       map[string]int{},
		Requests:       [][]interface{}{{"report", "read"}},
	})
	if err == nil {
		t.Fatal("expected an error for a candidate role without a cost")
	}
}

// TestZeroCostEquivalentSolutions checks zero-cost roles: every optimum
// must be enumerated, and supersets that only add redundant free roles
// must be dropped.
func TestZeroCostEquivalentSolutions(t *testing.T) {
	e := newTestEnforcer(t)

	result, err := Suggest(e, Options{
		Subject:        "bob",
		CandidateRoles: []string{"auditor", "junior_auditor", "employee"},
		RoleCost: map[string]int{
			"auditor":        0,
			"junior_auditor": 0,
			"employee":       0,
		},
		Requests: [][]interface{}{
			{"report", "read"},
			{"report", "approve"},
		},
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if result.Status != StatusFound {
		t.Fatalf("expected StatusFound, got %s", result.Status)
	}
	got := roleSets(result)
	want := [][]string{
		{"auditor"},
		{"junior_auditor"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v (cost %d)", want, got, result.Advice[0].Cost)
	}
}

// TestRolesOutsideCandidatesAreNeverGranted proves the search cannot lean
// on roles outside the candidate set even when such a role alone would
// satisfy the request.
func TestRolesOutsideCandidatesAreNeverGranted(t *testing.T) {
	e := newTestEnforcer(t)

	// finance_director alone satisfies every request, but it is not in
	// the candidate set; the only offered candidate (employee) cannot.
	result, err := Suggest(e, Options{
		Subject:        "bob",
		CandidateRoles: []string{"employee"},
		RoleCost:       map[string]int{"employee": 1},
		Requests:       allRequests,
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if result.Status != StatusUnsatisfiable {
		t.Fatalf("expected StatusUnsatisfiable, got %s (%v)", result.Status, result.Advice)
	}

	// Sanity check: the excluded role really would solve it when offered.
	result, err = Suggest(e, Options{
		Subject:        "bob",
		CandidateRoles: []string{"finance_director"},
		RoleCost:       map[string]int{"finance_director": 1},
		Requests:       allRequests,
	})
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}
	if result.Status != StatusFound {
		t.Fatalf("expected StatusFound once the role is offered, got %s", result.Status)
	}
}
