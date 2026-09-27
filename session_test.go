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
	"sync"
	"testing"

	"github.com/casbin/casbin/v3/model"

	casbinerrors "github.com/casbin/casbin/v3/errors"
)

func newSessionEnforcer(t *testing.T) *Enforcer {
	t.Helper()

	m, err := model.NewModelFromString(`
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && r.obj == p.obj && r.act == p.act
`)
	if err != nil {
		t.Fatalf("failed to create model: %v", err)
	}

	e, err := NewEnforcer(m)
	if err != nil {
		t.Fatalf("failed to create enforcer: %v", err)
	}

	// Permissions.
	if _, err = e.AddPolicy("employee", "data_public", "read"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.AddPolicy("applicant", "application", "submit"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.AddPolicy("approver", "application", "approve"); err != nil {
		t.Fatal(err)
	}

	// Inheritance chain: applicant -> employee, approver -> employee.
	if _, err = e.AddGroupingPolicy("applicant", "employee"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.AddGroupingPolicy("approver", "employee"); err != nil {
		t.Fatal(err)
	}

	// Alice is assigned both the applicant and the approver role.
	if _, err = e.AddRoleForUser("alice", "applicant"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.AddRoleForUser("alice", "approver"); err != nil {
		t.Fatal(err)
	}

	// Applicant and approver must never be active in the same session.
	e.AddMutuallyExclusiveRoles("applicant", "approver")

	return e
}

// TestAuthorizationSessionExample is a complete example: one user (alice)
// holds two mutually exclusive roles and uses two separate sessions, while
// permissions are inherited through the applicant/approver -> employee chain.
func TestAuthorizationSessionExample(t *testing.T) {
	e := newSessionEnforcer(t)

	// Session 1: alice works as an applicant.
	s1 := e.NewSession("alice")
	if err := s1.ActivateRole("applicant"); err != nil {
		t.Fatalf("activate applicant: %v", err)
	}

	// The mutually exclusive role cannot be activated in the same session.
	if err := s1.ActivateRole("approver"); !errors.Is(err, casbinerrors.ErrRoleMutuallyExclusive) {
		t.Fatalf("expected ErrRoleMutuallyExclusive, got: %v", err)
	}

	// Session 1 enforces as the applicant: submit allowed, approve denied,
	// and the inherited employee permission (read data_public) works.
	testSessionEnforce(t, s1, "application", "submit", true)
	testSessionEnforce(t, s1, "application", "approve", false)
	testSessionEnforce(t, s1, "data_public", "read", true)

	// Session 2: the same user opens a second session as an approver.
	s2 := e.NewSession("alice")
	if err := s2.ActivateRole("approver"); err != nil {
		t.Fatalf("activate approver: %v", err)
	}
	testSessionEnforce(t, s2, "application", "approve", true)
	testSessionEnforce(t, s2, "application", "submit", false)
	testSessionEnforce(t, s2, "data_public", "read", true)

	// Deactivating frees the session to activate the other role.
	s1.DeactivateRole("applicant")
	if err := s1.ActivateRole("approver"); err != nil {
		t.Fatalf("activate approver after deactivating applicant: %v", err)
	}
	testSessionEnforce(t, s1, "application", "approve", true)
}

// TestSessionInheritedRoleExclusion verifies that roles inherited through the
// role hierarchy participate in the dynamic mutual exclusion check:
// lead_approver inherits approver, so it conflicts with applicant.
func TestSessionInheritedRoleExclusion(t *testing.T) {
	e := newSessionEnforcer(t)

	// lead_approver -> approver -> employee, and alice gets lead_approver.
	if _, err := e.AddGroupingPolicy("lead_approver", "approver"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddRoleForUser("alice", "lead_approver"); err != nil {
		t.Fatal(err)
	}

	s := e.NewSession("alice")
	if err := s.ActivateRole("lead_approver"); err != nil {
		t.Fatalf("activate lead_approver: %v", err)
	}

	// applicant conflicts with the inherited approver role.
	if err := s.ActivateRole("applicant"); !errors.Is(err, casbinerrors.ErrRoleMutuallyExclusive) {
		t.Fatalf("expected ErrRoleMutuallyExclusive, got: %v", err)
	}

	// The reverse order conflicts as well.
	s2 := e.NewSession("alice")
	if err := s2.ActivateRole("applicant"); err != nil {
		t.Fatalf("activate applicant: %v", err)
	}
	if err := s2.ActivateRole("lead_approver"); !errors.Is(err, casbinerrors.ErrRoleMutuallyExclusive) {
		t.Fatalf("expected ErrRoleMutuallyExclusive, got: %v", err)
	}

	// lead_approver enforces inherited approver and employee permissions.
	testSessionEnforce(t, s, "application", "approve", true)
	testSessionEnforce(t, s, "data_public", "read", true)
}

// TestSessionConcurrentActivation verifies that two concurrent activations of
// mutually exclusive roles in the same session cannot both succeed.
func TestSessionConcurrentActivation(t *testing.T) {
	e := newSessionEnforcer(t)

	for i := 0; i < 100; i++ {
		s := e.NewSession("alice")

		var wg sync.WaitGroup
		results := make([]error, 2)
		roles := []string{"applicant", "approver"}
		for j, role := range roles {
			wg.Add(1)
			go func(j int, role string) {
				defer wg.Done()
				results[j] = s.ActivateRole(role)
			}(j, role)
		}
		wg.Wait()

		succeeded := 0
		for _, err := range results {
			if err == nil {
				succeeded++
			} else if !errors.Is(err, casbinerrors.ErrRoleMutuallyExclusive) {
				t.Fatalf("unexpected activation error: %v", err)
			}
		}
		if succeeded != 1 {
			t.Fatalf("iteration %d: expected exactly 1 successful activation, got %d", i, succeeded)
		}
		if got := len(s.ActiveRoles()); got != 1 {
			t.Fatalf("iteration %d: expected exactly 1 active role, got %d", i, got)
		}
	}
}

// TestSessionRoleRevocation verifies that a session cannot keep using a role
// after an administrator revoked it: there is no cached grant to rely on.
func TestSessionRoleRevocation(t *testing.T) {
	e := newSessionEnforcer(t)

	s := e.NewSession("alice")
	if err := s.ActivateRole("approver"); err != nil {
		t.Fatalf("activate approver: %v", err)
	}
	testSessionEnforce(t, s, "application", "approve", true)

	// The administrator revokes the role.
	if _, err := e.DeleteRoleForUser("alice", "approver"); err != nil {
		t.Fatal(err)
	}

	// The old session fails closed instead of using a cached grant.
	if ok, err := s.Enforce("application", "approve"); !errors.Is(err, casbinerrors.ErrSessionNoActiveRole) || ok {
		t.Fatalf("expected ErrSessionNoActiveRole and deny, got ok=%t err=%v", ok, err)
	}
	if got := s.ActiveRoles(); len(got) != 0 {
		t.Fatalf("expected revoked role to be dropped, active roles: %v", got)
	}

	// Activating the revoked role again is rejected.
	if err := s.ActivateRole("approver"); !errors.Is(err, casbinerrors.ErrRoleNotGranted) {
		t.Fatalf("expected ErrRoleNotGranted, got: %v", err)
	}
}

func TestSessionRoleNotGranted(t *testing.T) {
	e := newSessionEnforcer(t)

	s := e.NewSession("alice")
	if err := s.ActivateRole("admin"); !errors.Is(err, casbinerrors.ErrRoleNotGranted) {
		t.Fatalf("expected ErrRoleNotGranted, got: %v", err)
	}

	if _, err := s.Enforce("application", "submit"); !errors.Is(err, casbinerrors.ErrSessionNoActiveRole) {
		t.Fatalf("expected ErrSessionNoActiveRole, got: %v", err)
	}
}

func TestMutuallyExclusiveRolesAPI(t *testing.T) {
	e := newSessionEnforcer(t)

	if !e.AreRolesMutuallyExclusive("applicant", "approver") {
		t.Fatal("expected applicant and approver to be mutually exclusive")
	}
	if !e.AreRolesMutuallyExclusive("approver", "applicant") {
		t.Fatal("mutual exclusion should be symmetric")
	}
	if e.AreRolesMutuallyExclusive("applicant", "employee") {
		t.Fatal("applicant and employee should not be mutually exclusive")
	}

	e.RemoveMutuallyExclusiveRoles("applicant", "approver")
	if e.AreRolesMutuallyExclusive("applicant", "approver") {
		t.Fatal("expected exclusion to be removed")
	}

	// After removal both roles can be active in one session.
	s := e.NewSession("alice")
	if err := s.ActivateRole("applicant"); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateRole("approver"); err != nil {
		t.Fatal(err)
	}
	testSessionEnforce(t, s, "application", "submit", true)
	testSessionEnforce(t, s, "application", "approve", true)
}

func testSessionEnforce(t *testing.T, s *Session, obj string, act string, res bool) {
	t.Helper()
	ok, err := s.Enforce(obj, act)
	if err != nil {
		t.Errorf("Enforce(%s, %s, %s) error: %v", s.User(), obj, act, err)
		return
	}
	if ok != res {
		t.Errorf("%s, %s, %s: %t, supposed to be %t", s.User(), obj, act, ok, res)
	}
}
