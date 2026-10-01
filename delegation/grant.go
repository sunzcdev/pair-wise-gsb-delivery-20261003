package delegation

import "time"

// Scope is the set of permissions carried by a delegation.
//
// Resource and Action are request-shaped values exactly like the obj and act
// parameters of a casbin enforce call. They may be concrete values or patterns
// (for example "/docs/*" or "read"); containment between a parent and a child
// scope is decided by the configured Matcher, in the same direction as request
// matching: the concrete request value matches the stored pattern.
//
// The time window bounds the period during which the delegation is effective.
// A zero Time means the corresponding bound is unbounded.
type Scope struct {
	Resource  string
	Action    string
	NotBefore time.Time
	ExpiresAt time.Time
}

// activeAt reports whether the scope's time window contains t.
// NotBefore is inclusive, ExpiresAt is exclusive.
func (s Scope) activeAt(t time.Time) bool {
	if !s.NotBefore.IsZero() && t.Before(s.NotBefore) {
		return false
	}
	if !s.ExpiresAt.IsZero() && !t.Before(s.ExpiresAt) {
		return false
	}
	return true
}

// Grant is a single delegation edge: a delegator hands a scope to a delegatee,
// optionally allowing the delegatee to pass it on.
//
// Root delegations have an empty ParentID and carry permissions that the
// delegator already holds directly. Non-root delegations reference the parent
// grant through ParentID and must stay within its scope.
type Grant struct {
	// ID is the unique identifier of the grant, assigned on creation.
	ID string
	// ParentID links a sub-delegation to its parent; empty for a root grant.
	ParentID string
	// Delegator is the subject giving away part of its authority.
	Delegator string
	// Delegatee is the subject receiving the authority.
	Delegatee string
	// Scope is the (possibly narrowed) delegated permission.
	Scope Scope
	// CanSubGrant decides, from the delegator's point of view, whether the
	// delegatee may hand this grant on to yet another subject.
	CanSubGrant bool
	// Revoked records whether the grant has been withdrawn by its delegator.
	Revoked bool
	// CreatedAt records when the grant was created.
	CreatedAt time.Time
}

// clone returns a copy of the grant that is safe to hand to callers.
func (g *Grant) clone() Grant {
	return *g
}
