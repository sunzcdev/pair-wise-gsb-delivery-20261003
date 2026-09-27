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
	"sort"
	"sync"
	"time"

	"github.com/casbin/casbin/v3/errors"

	"github.com/google/uuid"
)

// AuthorizationSession is an explicit authorization session for a user.
// A user may hold several mutually exclusive roles at the same time, but a
// single session can only activate a compatible subset of them. Enforcement
// through a session only uses the roles activated in that session, which
// implements dynamic separation of duties: static grouping policy keeps all
// assignments, while the session decides which duty is exercised.
type AuthorizationSession struct {
	ID        string
	User      string
	CreatedAt time.Time

	activeRoles map[string]struct{}
}

// SessionManager manages explicit authorization sessions on top of an
// Enforcer. It provides role activation/deactivation with dynamic mutual
// exclusion checks and session-scoped enforcement. All methods are safe for
// concurrent use: activation is an atomic check-and-set, so concurrent
// activation requests cannot both succeed for conflicting roles.
type SessionManager struct {
	e *Enforcer

	mu          sync.Mutex
	sessions    map[string]*AuthorizationSession
	mutexGroups map[string][]string
}

// NewSessionManager creates a SessionManager bound to the given enforcer.
func NewSessionManager(e *Enforcer) *SessionManager {
	return &SessionManager{
		e:           e,
		sessions:    make(map[string]*AuthorizationSession),
		mutexGroups: make(map[string][]string),
	}
}

// AddDynamicMutexGroup declares a set of roles of which at most one may be
// active in a single authorization session. Inherited roles participate in
// the check: activating a role that inherits a group member counts as
// activating that member.
func (sm *SessionManager) AddDynamicMutexGroup(name string, roles ...string) error {
	if len(roles) < 2 {
		return errors.NewDynamicMutexViolationError(name, "", []string{"a mutex group requires at least 2 roles"})
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	copied := make([]string, len(roles))
	copy(copied, roles)
	sm.mutexGroups[name] = copied

	return nil
}

// RemoveDynamicMutexGroup removes a previously declared mutex group.
func (sm *SessionManager) RemoveDynamicMutexGroup(name string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.mutexGroups, name)
}

// CreateSession creates a new authorization session for the user.
func (sm *SessionManager) CreateSession(user string) *AuthorizationSession {
	session := &AuthorizationSession{
		ID:          uuid.NewString(),
		User:        user,
		CreatedAt:   time.Now(),
		activeRoles: make(map[string]struct{}),
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sessions[session.ID] = session

	return session
}

// CloseSession closes and removes an authorization session.
func (sm *SessionManager) CloseSession(sessionID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, ok := sm.sessions[sessionID]; !ok {
		return errors.ErrSessionNotFound
	}
	delete(sm.sessions, sessionID)

	return nil
}

// ActivateRole activates a role in the session. The role must currently be
// granted to the session user (directly or inherited), and its full
// inheritance closure must not conflict with the roles already active in the
// session under any dynamic mutex group. The check-and-set is atomic, so
// concurrent activations of conflicting roles cannot both succeed.
func (sm *SessionManager) ActivateRole(sessionID string, role string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[sessionID]
	if !ok {
		return errors.ErrSessionNotFound
	}

	if _, active := session.activeRoles[role]; active {
		return nil
	}

	granted, err := sm.implicitRoleSet(session.User)
	if err != nil {
		return err
	}
	if _, ok = granted[role]; !ok {
		return errors.ErrRoleNotGranted
	}

	candidateClosure, err := sm.roleClosure(role)
	if err != nil {
		return err
	}

	activeClosure := make(map[string]struct{})
	for activeRole := range session.activeRoles {
		closure, err := sm.roleClosure(activeRole)
		if err != nil {
			return err
		}
		for member := range closure {
			activeClosure[member] = struct{}{}
		}
	}

	for groupName, groupRoles := range sm.mutexGroups {
		var hits, conflicts []string
		for _, groupRole := range groupRoles {
			_, inActive := activeClosure[groupRole]
			_, inCandidate := candidateClosure[groupRole]
			if inActive || inCandidate {
				hits = append(hits, groupRole)
			}
			if inActive {
				conflicts = append(conflicts, groupRole)
			}
		}

		if len(hits) > 1 {
			if len(conflicts) == 0 {
				conflicts = hits
			}
			sort.Strings(conflicts)
			return errors.NewDynamicMutexViolationError(groupName, role, conflicts)
		}
	}

	session.activeRoles[role] = struct{}{}

	return nil
}

// DeactivateRole deactivates a role in the session.
func (sm *SessionManager) DeactivateRole(sessionID string, role string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[sessionID]
	if !ok {
		return errors.ErrSessionNotFound
	}
	delete(session.activeRoles, role)

	return nil
}

// GetActiveRoles returns the roles currently active in the session. Roles
// that have been revoked from the user since activation are dropped.
func (sm *SessionManager) GetActiveRoles(sessionID string) ([]string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[sessionID]
	if !ok {
		return nil, errors.ErrSessionNotFound
	}

	if err := sm.dropRevokedRoles(session); err != nil {
		return nil, err
	}

	roles := make([]string, 0, len(session.activeRoles))
	for role := range session.activeRoles {
		roles = append(roles, role)
	}
	sort.Strings(roles)

	return roles, nil
}

// Enforce executes enforcement on behalf of the session: the request is
// allowed if any still-valid active role of the session is allowed by the
// underlying enforcer. Active roles are re-validated against the current
// grouping policy on every call, so a session can never keep using a role
// that an administrator has revoked from the user.
func (sm *SessionManager) Enforce(sessionID string, rvals ...interface{}) (bool, error) {
	sm.mu.Lock()
	session, ok := sm.sessions[sessionID]
	if !ok {
		sm.mu.Unlock()
		return false, errors.ErrSessionNotFound
	}
	if err := sm.dropRevokedRoles(session); err != nil {
		sm.mu.Unlock()
		return false, err
	}
	roles := make([]string, 0, len(session.activeRoles))
	for role := range session.activeRoles {
		roles = append(roles, role)
	}
	sm.mu.Unlock()

	for _, role := range roles {
		allowed, err := sm.e.Enforce(append([]interface{}{role}, rvals...)...)
		if err != nil {
			return false, err
		}
		if allowed {
			return true, nil
		}
	}

	return false, nil
}

// dropRevokedRoles removes active roles that are no longer granted to the
// session user. The caller must hold sm.mu.
func (sm *SessionManager) dropRevokedRoles(session *AuthorizationSession) error {
	granted, err := sm.implicitRoleSet(session.User)
	if err != nil {
		return err
	}

	for role := range session.activeRoles {
		if _, stillGranted := granted[role]; !stillGranted {
			delete(session.activeRoles, role)
		}
	}

	return nil
}

// implicitRoleSet returns all roles a name inherits (directly or
// transitively) as a set. The caller must hold sm.mu.
func (sm *SessionManager) implicitRoleSet(name string) (map[string]struct{}, error) {
	roles, err := sm.e.GetImplicitRolesForUser(name)
	if err != nil {
		return nil, err
	}

	set := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		set[role] = struct{}{}
	}

	return set, nil
}

// roleClosure returns the role itself plus all roles it inherits. The caller
// must hold sm.mu.
func (sm *SessionManager) roleClosure(role string) (map[string]struct{}, error) {
	closure, err := sm.implicitRoleSet(role)
	if err != nil {
		return nil, err
	}
	closure[role] = struct{}{}

	return closure, nil
}
