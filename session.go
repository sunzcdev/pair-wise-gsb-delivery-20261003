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
	"sort"
	"sync"

	"github.com/casbin/casbin/v3/errors"
)

// roleExclusionSet stores pairs of roles that must never be active in the
// same authorization session at the same time (dynamic separation of duty).
// The zero value is ready to use.
type roleExclusionSet struct {
	mu    sync.RWMutex
	pairs map[string]map[string]struct{}
}

func (s *roleExclusionSet) add(role1, role2 string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pairs == nil {
		s.pairs = make(map[string]map[string]struct{})
	}
	if s.pairs[role1] == nil {
		s.pairs[role1] = make(map[string]struct{})
	}
	if s.pairs[role2] == nil {
		s.pairs[role2] = make(map[string]struct{})
	}
	s.pairs[role1][role2] = struct{}{}
	s.pairs[role2][role1] = struct{}{}
}

func (s *roleExclusionSet) remove(role1, role2 string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pairs == nil {
		return
	}
	delete(s.pairs[role1], role2)
	delete(s.pairs[role2], role1)
}

func (s *roleExclusionSet) isExclusive(role1, role2 string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.pairs[role1][role2]
	return ok
}

// AddMutuallyExclusiveRoles declares that role1 and role2 are mutually
// exclusive: they (and any role inheriting either of them) can never be
// activated together within a single authorization Session.
func (e *Enforcer) AddMutuallyExclusiveRoles(role1, role2 string) {
	e.exclusions.add(role1, role2)
}

// RemoveMutuallyExclusiveRoles removes a mutual exclusion declaration
// previously added by AddMutuallyExclusiveRoles.
func (e *Enforcer) RemoveMutuallyExclusiveRoles(role1, role2 string) {
	e.exclusions.remove(role1, role2)
}

// AreRolesMutuallyExclusive reports whether role1 and role2 have been
// declared mutually exclusive via AddMutuallyExclusiveRoles.
func (e *Enforcer) AreRolesMutuallyExclusive(role1, role2 string) bool {
	return e.exclusions.isExclusive(role1, role2)
}

// Session is an explicit authorization session for a single user.
//
// A user may be assigned several roles at the same time (for example both
// "applicant" and "approver"), but within one session only roles that are
// not mutually exclusive may be activated. The session enforces requests on
// behalf of its activated roles only, so static separation-of-duty
// constraints on the grouping policy are not needed for this restriction.
//
// A Session does not cache role grants: every Enforce call re-validates the
// activated roles against the live role manager, so a role revoked by an
// administrator stops working in existing sessions immediately.
//
// A Session is safe for concurrent use: concurrent ActivateRole calls are
// serialized, so two racing activations of mutually exclusive roles cannot
// both succeed.
type Session struct {
	e      *Enforcer
	user   string
	domain []string

	mu     sync.Mutex
	active map[string]struct{}
}

// NewSession creates an explicit authorization session for a user.
// The returned session has no active role yet; use ActivateRole first.
func (e *Enforcer) NewSession(user string, domain ...string) *Session {
	return &Session{
		e:      e,
		user:   user,
		domain: domain,
		active: make(map[string]struct{}),
	}
}

// User returns the user the session belongs to.
func (s *Session) User() string {
	return s.user
}

// implicitRoles returns the roles directly or indirectly granted to name.
func (s *Session) implicitRoles(name string) (map[string]struct{}, error) {
	roles, err := s.e.GetImplicitRolesForUser(name, s.domain...)
	if err != nil {
		return nil, err
	}
	res := make(map[string]struct{}, len(roles)+1)
	res[name] = struct{}{}
	for _, role := range roles {
		res[role] = struct{}{}
	}
	return res, nil
}

// ActivateRole activates a role for the session.
//
// The role must be granted to the session's user (directly or inherited).
// Activation fails with ErrRoleMutuallyExclusive if the role, or any role it
// inherits, is mutually exclusive with an already active role or any role
// that one inherits. The check and the activation are performed atomically,
// so concurrent activations cannot both succeed.
func (s *Session) ActivateRole(role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.active[role]; ok {
		return nil
	}

	granted, err := s.implicitRoles(s.user)
	if err != nil {
		return err
	}
	if _, ok := granted[role]; !ok {
		return fmt.Errorf("%w: %s is not granted to %s", errors.ErrRoleNotGranted, role, s.user)
	}

	newClosure, err := s.implicitRoles(role)
	if err != nil {
		return err
	}

	for activeRole := range s.active {
		activeClosure, err := s.implicitRoles(activeRole)
		if err != nil {
			return err
		}
		for newRole := range newClosure {
			for existingRole := range activeClosure {
				if s.e.exclusions.isExclusive(newRole, existingRole) {
					return fmt.Errorf("%w: %s conflicts with active role %s (%s and %s are mutually exclusive)",
						errors.ErrRoleMutuallyExclusive, role, activeRole, newRole, existingRole)
				}
			}
		}
	}

	s.active[role] = struct{}{}
	return nil
}

// DeactivateRole deactivates a role previously activated in the session.
func (s *Session) DeactivateRole(role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, role)
}

// ActiveRoles returns the currently active roles of the session.
func (s *Session) ActiveRoles() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	roles := make([]string, 0, len(s.active))
	for role := range s.active {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// Enforce enforces a request on behalf of the session's active roles.
// rvals are the request values that follow the subject (e.g. obj, act).
// The request is allowed if any still-valid active role allows it.
//
// Active roles are re-validated against the current role manager on every
// call; roles revoked since their activation are dropped and can no longer
// be used. If no valid active role remains, Enforce fails closed with
// ErrSessionNoActiveRole.
func (s *Session) Enforce(rvals ...interface{}) (bool, error) {
	s.mu.Lock()
	granted, err := s.implicitRoles(s.user)
	if err != nil {
		s.mu.Unlock()
		return false, err
	}
	roles := make([]string, 0, len(s.active))
	for role := range s.active {
		if _, ok := granted[role]; ok {
			roles = append(roles, role)
		} else {
			delete(s.active, role)
		}
	}
	sort.Strings(roles)
	s.mu.Unlock()

	if len(roles) == 0 {
		return false, errors.ErrSessionNoActiveRole
	}

	for _, role := range roles {
		args := make([]interface{}, 0, len(rvals)+1)
		args = append(args, role)
		args = append(args, rvals...)
		ok, err := s.e.Enforce(args...)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
