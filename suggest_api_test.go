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
	"fmt"
	"reflect"
	"testing"
)

func newSuggestEnforcer(t *testing.T) *Enforcer {
	t.Helper()
	e, err := NewEnforcer("examples/suggest_model.conf", "examples/suggest_policy.csv")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func requestTuple(sub, obj, act string) []interface{} {
	return []interface{}{sub, obj, act}
}

// applySuggestion grants the suggested roles on an independent copy of the
// policy and checks that every request is allowed there.
func applySuggestion(t *testing.T, subject string, requests [][]interface{}, roles []string) {
	t.Helper()
	copyEnforcer := newSuggestEnforcer(t)
	for _, role := range roles {
		if _, err := copyEnforcer.AddGroupingPolicy(subject, role); err != nil {
			t.Fatal(err)
		}
	}
	for _, req := range requests {
		ok, err := copyEnforcer.Enforce(req...)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("suggestion %v does not satisfy request %v on an independent copy", roles, req)
		}
	}
}

func TestSuggestRolesWithInheritance(t *testing.T) {
	e := newSuggestEnforcer(t)

	requests := [][]interface{}{
		requestTuple("alice", "data1", "read"),
		requestTuple("alice", "data1", "write"),
	}
	candidates := []RoleCandidate{
		{Role: "viewer", Cost: 1},
		{Role: "operator", Cost: 2},
		{Role: "admin", Cost: 10},
	}

	res, err := e.SuggestRoles("alice", requests, candidates, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestFound || !res.Complete {
		t.Fatalf("expected a complete found result, got %v (complete=%v)", res.Status, res.Complete)
	}
	// operator inherits viewer, so it alone covers both requests and beats
	// the more expensive admin.
	if res.Cost != 2 || len(res.Solutions) != 1 || !reflect.DeepEqual(res.Solutions[0], []string{"operator"}) {
		t.Fatalf("expected {[operator]} at cost 2, got %v at cost %d", res.Solutions, res.Cost)
	}

	applySuggestion(t, "alice", requests, res.Solutions[0])
}

func TestSuggestRolesMutualExclusion(t *testing.T) {
	e := newSuggestEnforcer(t)

	requests := [][]interface{}{
		requestTuple("alice", "data1", "write"),
		requestTuple("alice", "data1", "audit"),
	}
	candidates := []RoleCandidate{
		{Role: "operator", Cost: 1},
		{Role: "auditor", Cost: 1},
	}

	// Without the separation-of-duties constraint both roles can be granted.
	res, err := e.SuggestRoles("alice", requests, candidates, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestFound || res.Cost != 2 {
		t.Fatalf("expected a solution at cost 2, got %v at cost %d", res.Solutions, res.Cost)
	}
	applySuggestion(t, "alice", requests, res.Solutions[0])

	// With operator and auditor mutually exclusive the requests cannot be
	// satisfied from the candidate set.
	opts := &SuggestOptions{MutexPairs: [][2]string{{"operator", "auditor"}}}
	res, err = e.SuggestRoles("alice", requests, candidates, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestUnsatisfiable {
		t.Fatalf("expected unsatisfiable, got %v with %v", res.Status, res.Solutions)
	}

	// Mutual exclusion also applies to inherited roles: admin inherits
	// viewer, which conflicts with auditor.
	res, err = e.SuggestRoles("alice", requests, []RoleCandidate{
		{Role: "admin", Cost: 1},
		{Role: "auditor", Cost: 1},
	}, &SuggestOptions{MutexPairs: [][2]string{{"viewer", "auditor"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestUnsatisfiable {
		t.Fatalf("expected unsatisfiable via inherited mutex, got %v with %v", res.Status, res.Solutions)
	}
}

func TestSuggestRolesEquivalentSolutions(t *testing.T) {
	e := newSuggestEnforcer(t)

	requests := [][]interface{}{requestTuple("alice", "data2", "read")}
	candidates := []RoleCandidate{
		{Role: "analyst_a", Cost: 1},
		{Role: "analyst_b", Cost: 1},
		{Role: "viewer", Cost: 1},
	}

	res, err := e.SuggestRoles("alice", requests, candidates, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestFound || res.Cost != 1 {
		t.Fatalf("expected solutions at cost 1, got %v at cost %d", res.Solutions, res.Cost)
	}
	expected := [][]string{{"analyst_a"}, {"analyst_b"}}
	if !reflect.DeepEqual(res.Solutions, expected) {
		t.Fatalf("expected two equivalent solutions %v, got %v", expected, res.Solutions)
	}
	for _, solution := range res.Solutions {
		applySuggestion(t, "alice", requests, solution)
	}
}

func TestSuggestRolesDenyIsNotBypassed(t *testing.T) {
	e := newSuggestEnforcer(t)

	requests := [][]interface{}{requestTuple("alice", "data1", "read")}

	// The only candidate carries a deny rule for the request, so no valid
	// suggestion exists; the deny policy must not be removed to make one.
	res, err := e.SuggestRoles("alice", requests, []RoleCandidate{{Role: "blocked", Cost: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestUnsatisfiable {
		t.Fatalf("expected unsatisfiable, got %v with %v", res.Status, res.Solutions)
	}

	// Given a real alternative, the cheaper but denied role must be avoided.
	res, err = e.SuggestRoles("alice", requests, []RoleCandidate{
		{Role: "blocked", Cost: 1},
		{Role: "viewer", Cost: 2},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestFound || res.Cost != 2 || !reflect.DeepEqual(res.Solutions[0], []string{"viewer"}) {
		t.Fatalf("expected {[viewer]} at cost 2, got %v at cost %d", res.Solutions, res.Cost)
	}
	applySuggestion(t, "alice", requests, res.Solutions[0])
}

func TestSuggestRolesBudgetExhaustedVsUnsatisfiable(t *testing.T) {
	e := newSuggestEnforcer(t)

	requests := [][]interface{}{requestTuple("alice", "data1", "read")}

	// Many useless roles are explored before the one useful (most expensive)
	// candidate, so a small budget runs out before any solution is found.
	candidates := make([]RoleCandidate, 0, 9)
	for i := 0; i < 8; i++ {
		candidates = append(candidates, RoleCandidate{Role: fmt.Sprintf("useless_%d", i), Cost: 1})
	}
	candidates = append(candidates, RoleCandidate{Role: "viewer", Cost: 100})

	res, err := e.SuggestRoles("alice", requests, candidates, &SuggestOptions{MaxNodes: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestBudgetExhausted {
		t.Fatalf("expected budget_exhausted, got %v with %v", res.Status, res.Solutions)
	}
	if res.NodesExplored != 5 {
		t.Fatalf("expected exactly 5 explored nodes, got %d", res.NodesExplored)
	}

	// With an unlimited budget the same query is satisfiable.
	res, err = e.SuggestRoles("alice", requests, candidates, &SuggestOptions{MaxNodes: -1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestFound || !res.Complete || res.Cost != 100 {
		t.Fatalf("expected a complete solution at cost 100, got %v at cost %d (complete=%v)", res.Solutions, res.Cost, res.Complete)
	}

	// A request that no candidate can ever satisfy is reported as genuinely
	// unsatisfiable, not as a budget problem.
	res, err = e.SuggestRoles("alice", [][]interface{}{requestTuple("alice", "data9", "read")}, candidates, &SuggestOptions{MaxNodes: -1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestUnsatisfiable {
		t.Fatalf("expected unsatisfiable, got %v with %v", res.Status, res.Solutions)
	}
}

func TestSuggestRolesAlreadyAllowed(t *testing.T) {
	e := newSuggestEnforcer(t)
	if _, err := e.AddGroupingPolicy("alice", "viewer"); err != nil {
		t.Fatal(err)
	}

	res, err := e.SuggestRoles("alice", [][]interface{}{requestTuple("alice", "data1", "read")}, []RoleCandidate{{Role: "operator", Cost: 2}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestFound || res.Cost != 0 || len(res.Solutions) != 1 || len(res.Solutions[0]) != 0 {
		t.Fatalf("expected an empty solution at cost 0, got %v at cost %d", res.Solutions, res.Cost)
	}
}

func TestSuggestRolesDoesNotModifyEnforcer(t *testing.T) {
	e := newSuggestEnforcer(t)

	policyBefore, err := e.GetPolicy()
	if err != nil {
		t.Fatal(err)
	}
	groupingBefore, err := e.GetGroupingPolicy()
	if err != nil {
		t.Fatal(err)
	}

	requests := [][]interface{}{
		requestTuple("alice", "data1", "read"),
		requestTuple("alice", "data1", "write"),
	}
	allowedBefore, err := e.Enforce("alice", "data1", "write")
	if err != nil {
		t.Fatal(err)
	}

	res, err := e.SuggestRoles("alice", requests, []RoleCandidate{
		{Role: "viewer", Cost: 1},
		{Role: "operator", Cost: 2},
		{Role: "admin", Cost: 10},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SuggestFound {
		t.Fatalf("expected a solution, got %v", res.Status)
	}

	policyAfter, err := e.GetPolicy()
	if err != nil {
		t.Fatal(err)
	}
	groupingAfter, err := e.GetGroupingPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(policyBefore, policyAfter) {
		t.Errorf("policy changed by SuggestRoles: before %v, after %v", policyBefore, policyAfter)
	}
	if !reflect.DeepEqual(groupingBefore, groupingAfter) {
		t.Errorf("grouping policy changed by SuggestRoles: before %v, after %v", groupingBefore, groupingAfter)
	}

	// The suggestion must not have leaked into the original enforcer.
	allowedAfter, err := e.Enforce("alice", "data1", "write")
	if err != nil {
		t.Fatal(err)
	}
	if allowedBefore || allowedAfter {
		t.Errorf("enforcement result on the original enforcer changed: before=%v after=%v", allowedBefore, allowedAfter)
	}

	// But applied to an independent copy, the suggestion works.
	applySuggestion(t, "alice", requests, res.Solutions[0])
}

func TestSuggestRolesInvalidInput(t *testing.T) {
	e := newSuggestEnforcer(t)
	requests := [][]interface{}{requestTuple("alice", "data1", "read")}

	if _, err := e.SuggestRoles("", requests, nil, nil); err == nil {
		t.Error("expected an error for an empty subject")
	}
	if _, err := e.SuggestRoles("alice", requests, []RoleCandidate{{Role: "viewer", Cost: -1}}, nil); err == nil {
		t.Error("expected an error for a negative cost")
	}
	if _, err := e.SuggestRoles("alice", requests, []RoleCandidate{{Role: "viewer"}, {Role: "viewer"}}, nil); err == nil {
		t.Error("expected an error for duplicate candidates")
	}
}
