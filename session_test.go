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
	"sync"
	"testing"

	"github.com/casbin/casbin/v3/errors"
	"github.com/casbin/casbin/v3/model"
	stringadapter "github.com/casbin/casbin/v3/persist/string-adapter"
)

// newSessionTestManager builds an enforcer where alice is assigned both the
// applicant and the approver role, and approver inherits auditor:
//
//	alice --> applicant
//	alice --> approver --> auditor
//
// A static SOD constraint on the grouping policy would forbid assigning both
// applicant and approver to alice at all; sessions restrict only which duty
// is exercised per operation session.
func newSessionTestManager(t *testing.T) *SessionManager {
	t.Helper()

	modelText := `
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
`

	policyText := `
p, applicant, application, create
p, approver, application, approve
p, auditor, application, read
g, alice, applicant
g, alice, approver
g, approver, auditor
`

	m, err := model.NewModelFromString(modelText)
	if err != nil {
		t.Fatalf("Failed to create model: %v", err)
	}

	e, err := NewEnforcer(m, stringadapter.NewAdapter(policyText))
	if err != nil {
		t.Fatalf("Failed to create enforcer: %v", err)
	}

	sm := NewSessionManager(e)
	if err = sm.AddDynamicMutexGroup("duty", "applicant", "approver"); err != nil {
		t.Fatalf("Failed to add mutex group: %v", err)
	}
	if err = sm.AddDynamicMutexGroup("audit", "applicant", "auditor"); err != nil {
		t.Fatalf("Failed to add mutex group: %v", err)
	}

	return sm
}

// TestAuthorizationSessionExample is the complete example: one user (alice)
// holding two conflicting duties runs two sessions, one per duty, over an
// inheritance chain (approver inherits auditor).
func TestAuthorizationSessionExample(t *testing.T) {
	sm := newSessionTestManager(t)

	// Two sessions for the same user.
	applySession := sm.CreateSession("alice")
	approveSession := sm.CreateSession("alice")

	// Each session activates one of the mutually exclusive roles.
	if err := sm.ActivateRole(applySession.ID, "applicant"); err != nil {
		t.Fatalf("Failed to activate applicant: %v", err)
	}
	if err := sm.ActivateRole(approveSession.ID, "approver"); err != nil {
		t.Fatalf("Failed to activate approver: %v", err)
	}

	// The same session cannot activate the conflicting role.
	if err := sm.ActivateRole(applySession.ID, "approver"); err == nil {
		t.Fatal("Expected dynamic mutex violation, got nil")
	}

	// Inherited roles participate in the check: approver inherits auditor,
	// and auditor is mutually exclusive with applicant.
	if err := sm.ActivateRole(applySession.ID, "auditor"); err == nil {
		t.Fatal("Expected dynamic mutex violation via inherited role, got nil")
	}

	// Enforcement goes through the session's active role only.
	testEnforce := func(sessionID string, obj, act string, expected bool) {
		t.Helper()
		allowed, err := sm.Enforce(sessionID, obj, act)
		if err != nil {
			t.Fatalf("Enforce failed: %v", err)
		}
		if allowed != expected {
			t.Fatalf("session %s Enforce(%s, %s) = %v, expected %v", sessionID, obj, act, allowed, expected)
		}
	}

	testEnforce(applySession.ID, "application", "create", true)
	testEnforce(applySession.ID, "application", "approve", false)
	testEnforce(approveSession.ID, "application", "approve", true)
	// approver inherits auditor, so the approve session can also read.
	testEnforce(approveSession.ID, "application", "read", true)
	testEnforce(approveSession.ID, "application", "create", false)

	// Deactivating a role immediately removes its permissions.
	if err := sm.DeactivateRole(approveSession.ID, "approver"); err != nil {
		t.Fatalf("Failed to deactivate approver: %v", err)
	}
	testEnforce(approveSession.ID, "application", "approve", false)

	// After deactivation the conflicting role may be activated instead.
	if err := sm.ActivateRole(approveSession.ID, "applicant"); err != nil {
		t.Fatalf("Failed to activate applicant after deactivation: %v", err)
	}
	testEnforce(approveSession.ID, "application", "create", true)
}

// TestAuthorizationSessionConcurrentActivation ensures two concurrent
// activation requests for conflicting roles in the same session cannot both
// succeed.
func TestAuthorizationSessionConcurrentActivation(t *testing.T) {
	sm := newSessionTestManager(t)
	session := sm.CreateSession("alice")

	var wg sync.WaitGroup
	results := make(chan error, 2)

	for _, role := range []string{"applicant", "approver"} {
		wg.Add(1)
		go func(role string) {
			defer wg.Done()
			results <- sm.ActivateRole(session.ID, role)
		}(role)
	}
	wg.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("Expected exactly 1 concurrent activation to succeed, got %d", succeeded)
	}

	active, err := sm.GetActiveRoles(session.ID)
	if err != nil {
		t.Fatalf("Failed to get active roles: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("Expected exactly 1 active role, got %v", active)
	}
}

// TestAuthorizationSessionRevocation ensures that once an administrator
// revokes a role, an old session can no longer enforce with the cached role.
func TestAuthorizationSessionRevocation(t *testing.T) {
	sm := newSessionTestManager(t)
	session := sm.CreateSession("alice")

	if err := sm.ActivateRole(session.ID, "approver"); err != nil {
		t.Fatalf("Failed to activate approver: %v", err)
	}

	allowed, err := sm.Enforce(session.ID, "application", "approve")
	if err != nil || !allowed {
		t.Fatalf("Expected approve to be allowed, allowed=%v err=%v", allowed, err)
	}

	// The administrator revokes the role from the user.
	if _, err = sm.e.DeleteRoleForUser("alice", "approver"); err != nil {
		t.Fatalf("Failed to revoke approver: %v", err)
	}

	// The old session must not keep using the cached role.
	allowed, err = sm.Enforce(session.ID, "application", "approve")
	if err != nil {
		t.Fatalf("Enforce failed: %v", err)
	}
	if allowed {
		t.Fatal("Expected approve to be denied after revocation")
	}

	active, err := sm.GetActiveRoles(session.ID)
	if err != nil {
		t.Fatalf("Failed to get active roles: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("Expected revoked role to be dropped from session, got %v", active)
	}

	// The revoked role can no longer be activated either.
	if err = sm.ActivateRole(session.ID, "approver"); err != errors.ErrRoleNotGranted {
		t.Fatalf("Expected ErrRoleNotGranted, got %v", err)
	}
}

// TestAuthorizationSessionNotGranted ensures a session cannot activate a
// role the user was never assigned.
func TestAuthorizationSessionNotGranted(t *testing.T) {
	sm := newSessionTestManager(t)
	session := sm.CreateSession("alice")

	if err := sm.ActivateRole(session.ID, "root"); err != errors.ErrRoleNotGranted {
		t.Fatalf("Expected ErrRoleNotGranted, got %v", err)
	}

	if err := sm.ActivateRole("no-such-session", "applicant"); err != errors.ErrSessionNotFound {
		t.Fatalf("Expected ErrSessionNotFound, got %v", err)
	}
}
