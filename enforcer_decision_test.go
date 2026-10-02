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
	"testing"

	"github.com/casbin/casbin/v3/model"
)

type decisionSubject struct {
	Age int
}

func newDecisionEnforcer(t *testing.T, effect string, matcher string, policies [][]string) *Enforcer {
	t.Helper()

	modelText := `
[request_definition]
r = sub, obj, act

[policy_definition]
p = obj, act, eft

[policy_effect]
e = ` + effect + `

[matchers]
m = ` + matcher + `
`
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		t.Fatalf("failed to create model: %v", err)
	}

	e, err := NewEnforcer(m)
	if err != nil {
		t.Fatalf("failed to create enforcer: %v", err)
	}

	for _, rule := range policies {
		if _, err = e.AddPolicy(rule[0], rule[1], rule[2]); err != nil {
			t.Fatalf("failed to add policy %v: %v", rule, err)
		}
	}
	return e
}

func testDecide(t *testing.T, e *Enforcer, sub interface{}, obj string, act string, wantDecision Decision, wantNeeded []string) {
	t.Helper()

	decision, needed, err := e.Decide(sub, obj, act)
	if err != nil {
		t.Fatalf("Decide(%v, %s, %s) returned error: %v", sub, obj, act, err)
	}
	if decision != wantDecision {
		t.Errorf("Decide(%v, %s, %s) = %s, want %s", sub, obj, act, decision, wantDecision)
	}
	if len(needed) != len(wantNeeded) {
		t.Errorf("Decide(%v, %s, %s) needed = %v, want %v", sub, obj, act, needed, wantNeeded)
		return
	}
	for i := range needed {
		if needed[i] != wantNeeded[i] {
			t.Errorf("Decide(%v, %s, %s) needed = %v, want %v", sub, obj, act, needed, wantNeeded)
			return
		}
	}
}

// testDecideAlignsWithEnforce asserts that with complete attributes Decide
// always agrees with Enforce.
func testDecideAlignsWithEnforce(t *testing.T, e *Enforcer, sub interface{}, obj string, act string) {
	t.Helper()

	decision, needed, err := e.Decide(sub, obj, act)
	if err != nil {
		t.Fatalf("Decide(%v, %s, %s) returned error: %v", sub, obj, act, err)
	}
	if decision == DecisionIndeterminate {
		t.Fatalf("Decide(%v, %s, %s) = indeterminate with complete attributes, needed %v", sub, obj, act, needed)
	}
	if len(needed) != 0 {
		t.Fatalf("Decide(%v, %s, %s) reported unneeded attributes %v", sub, obj, act, needed)
	}

	enforced, err := e.Enforce(sub, obj, act)
	if err != nil {
		t.Fatalf("Enforce(%v, %s, %s) returned error: %v", sub, obj, act, err)
	}
	if (decision == DecisionAllow) != enforced {
		t.Errorf("Decide(%v, %s, %s) = %s, but Enforce = %v", sub, obj, act, decision, enforced)
	}
}

func TestDecideAllowOverride(t *testing.T) {
	e := newDecisionEnforcer(t,
		"some(where (p_eft == allow))",
		"r_obj == p_obj && r_act == p_act && r_sub.Age >= 18",
		[][]string{{"data1", "read", "allow"}})

	// r_sub is missing and the matcher result depends on it.
	testDecide(t, e, nil, "data1", "read", DecisionIndeterminate, []string{"r_sub"})
	// r_act mismatches, so && short-circuits before r_sub is ever touched:
	// the missing attribute is irrelevant to the conclusion.
	testDecide(t, e, nil, "data1", "write", DecisionDeny, nil)
	testDecide(t, e, nil, "data2", "read", DecisionDeny, nil)

	testDecide(t, e, decisionSubject{Age: 20}, "data1", "read", DecisionAllow, nil)
	testDecide(t, e, decisionSubject{Age: 10}, "data1", "read", DecisionDeny, nil)

	// filling in the missing attribute aligns with Enforce.
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 20}, "data1", "read")
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 10}, "data1", "read")
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 20}, "data2", "read")
}

func TestDecideDenyOverride(t *testing.T) {
	e := newDecisionEnforcer(t,
		"!some(where (p_eft == deny))",
		"r_obj == p_obj && r_act == p_act && r_sub.Age >= 18",
		[][]string{{"data1", "read", "deny"}, {"data2", "read", "allow"}})

	// the deny rule cannot be evaluated without r_sub.
	testDecide(t, e, nil, "data1", "read", DecisionIndeterminate, []string{"r_sub"})
	// a definite deny is conclusive: the second rule is never evaluated and
	// no attribute is reported.
	testDecide(t, e, decisionSubject{Age: 20}, "data1", "read", DecisionDeny, nil)
	// the deny rule does not match, so the request is allowed by default.
	testDecide(t, e, decisionSubject{Age: 10}, "data1", "read", DecisionAllow, nil)
	// no deny matched: allow by default. The unknown allow rule cannot turn
	// the outcome into a deny, so it reports nothing.
	testDecide(t, e, nil, "data2", "read", DecisionAllow, nil)
	testDecide(t, e, decisionSubject{Age: 20}, "data2", "read", DecisionAllow, nil)

	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 20}, "data1", "read")
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 10}, "data1", "read")
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 20}, "data2", "read")
}

func TestDecideAllowAndDeny(t *testing.T) {
	e := newDecisionEnforcer(t,
		"some(where (p_eft == allow)) && !some(where (p_eft == deny))",
		"r_obj == p_obj && r_act == p_act && r_sub.Age >= 18",
		[][]string{{"data1", "read", "deny"}, {"data1", "read", "allow"}, {"data2", "read", "allow"}})

	// a definite deny short-circuits: later rules are never evaluated.
	testDecide(t, e, decisionSubject{Age: 20}, "data1", "read", DecisionDeny, nil)
	// both the deny and the allow rule depend on the missing r_sub.
	testDecide(t, e, nil, "data1", "read", DecisionIndeterminate, []string{"r_sub"})
	// only an allow rule is undecided: the outcome is still indeterminate
	// because that rule could grant access.
	testDecide(t, e, nil, "data2", "read", DecisionIndeterminate, []string{"r_sub"})
	testDecide(t, e, decisionSubject{Age: 20}, "data2", "read", DecisionAllow, nil)
	testDecide(t, e, decisionSubject{Age: 10}, "data2", "read", DecisionDeny, nil)

	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 20}, "data1", "read")
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 10}, "data1", "read")
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 20}, "data2", "read")
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 10}, "data2", "read")
}

func TestDecideShortCircuitMatcher(t *testing.T) {
	e := newDecisionEnforcer(t,
		"some(where (p_eft == allow))",
		`r_obj == p_obj && (r_act == "read" || r_sub.Age >= 18)`,
		[][]string{{"data1", "read", "allow"}})

	// r_act == "read" satisfies the || before r_sub is evaluated, so the
	// missing attribute is not needed.
	testDecide(t, e, nil, "data1", "read", DecisionAllow, nil)
	// here the || falls through to r_sub.Age, which is missing.
	testDecide(t, e, nil, "data1", "write", DecisionIndeterminate, []string{"r_sub"})

	testDecide(t, e, decisionSubject{Age: 20}, "data1", "write", DecisionAllow, nil)
	testDecide(t, e, decisionSubject{Age: 10}, "data1", "write", DecisionDeny, nil)

	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 20}, "data1", "write")
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 10}, "data1", "write")
}

func TestDecideDefiniteDenySkipsMissingAttributes(t *testing.T) {
	e := newDecisionEnforcer(t,
		"!some(where (p_eft == deny))",
		`r_act == "delete" || (r_obj == p_obj && r_act == p_act && r_sub.Age >= 18)`,
		[][]string{{"data1", "read", "deny"}})

	// the request is conclusively denied without ever touching r_sub, so the
	// missing attribute must not be reported.
	testDecide(t, e, nil, "data1", "delete", DecisionDeny, nil)
	// the same rule is undecidable for other actions.
	testDecide(t, e, nil, "data1", "read", DecisionIndeterminate, []string{"r_sub"})

	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 20}, "data1", "delete")
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 20}, "data1", "read")
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 10}, "data1", "read")
}

func TestDecidePriority(t *testing.T) {
	e := newDecisionEnforcer(t,
		"priority(p_eft) || deny",
		`r_obj == p_obj && r_act == p_act && (p_eft == "deny" || r_sub.Age >= 18)`,
		[][]string{{"data1", "read", "allow"}, {"data1", "read", "deny"}})

	// the higher priority allow rule is undecidable, so the definite deny
	// behind it cannot be trusted either.
	testDecide(t, e, nil, "data1", "read", DecisionIndeterminate, []string{"r_sub"})
	// the first matched rule decides.
	testDecide(t, e, decisionSubject{Age: 20}, "data1", "read", DecisionAllow, nil)
	testDecide(t, e, decisionSubject{Age: 10}, "data1", "read", DecisionDeny, nil)
	// nothing matched: deny by default.
	testDecide(t, e, nil, "data2", "read", DecisionDeny, nil)

	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 20}, "data1", "read")
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 10}, "data1", "read")
}

func TestDecideWithMatcher(t *testing.T) {
	e := newDecisionEnforcer(t,
		"some(where (p_eft == allow))",
		"r_obj == p_obj && r_act == p_act",
		[][]string{{"data1", "read", "allow"}})

	matcher := "r_obj == p_obj && r_act == p_act && r_sub.Age >= 21"

	decision, needed, err := e.DecideWithMatcher(matcher, nil, "data1", "read")
	if err != nil {
		t.Fatalf("DecideWithMatcher returned error: %v", err)
	}
	if decision != DecisionIndeterminate || len(needed) != 1 || needed[0] != "r_sub" {
		t.Errorf("DecideWithMatcher = %s, %v, want indeterminate, [r_sub]", decision, needed)
	}

	decision, _, err = e.DecideWithMatcher(matcher, decisionSubject{Age: 20}, "data1", "read")
	if err != nil {
		t.Fatalf("DecideWithMatcher returned error: %v", err)
	}
	enforced, err := e.EnforceWithMatcher(matcher, decisionSubject{Age: 20}, "data1", "read")
	if err != nil {
		t.Fatalf("EnforceWithMatcher returned error: %v", err)
	}
	if (decision == DecisionAllow) != enforced {
		t.Errorf("DecideWithMatcher = %s, but EnforceWithMatcher = %v", decision, enforced)
	}
}

func TestDecidePolicyLessMatcher(t *testing.T) {
	modelText := `
[request_definition]
r = sub, obj, act

[policy_definition]
p = obj, act, eft

[policy_effect]
e = some(where (p_eft == allow))

[matchers]
m = r_sub.Age >= 18 && r_obj == "data1"
`
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		t.Fatalf("failed to create model: %v", err)
	}
	e, err := NewEnforcer(m)
	if err != nil {
		t.Fatalf("failed to create enforcer: %v", err)
	}

	testDecide(t, e, nil, "data1", "read", DecisionIndeterminate, []string{"r_sub"})
	testDecide(t, e, decisionSubject{Age: 20}, "data1", "read", DecisionAllow, nil)
	testDecide(t, e, decisionSubject{Age: 10}, "data1", "read", DecisionDeny, nil)
	testDecideAlignsWithEnforce(t, e, decisionSubject{Age: 20}, "data1", "read")
}

func TestDecideInvalidRequest(t *testing.T) {
	e := newDecisionEnforcer(t,
		"some(where (p_eft == allow))",
		"r_obj == p_obj && r_act == p_act",
		[][]string{{"data1", "read", "allow"}})

	if _, _, err := e.Decide("alice", "data1"); err == nil {
		t.Error("Decide with a wrong request size should return an error")
	}
}
