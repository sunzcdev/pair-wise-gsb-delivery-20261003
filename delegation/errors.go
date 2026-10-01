package delegation

import "errors"

// Errors returned when a delegation request is malformed or violates the
// delegation rules. They are returned by Grant / GrantDelegation at creation
// time and describe why a request must be rejected.
var (
	// ErrEmptyDelegator is returned when a grant does not name a delegator.
	ErrEmptyDelegator = errors.New("delegation: delegator must not be empty")
	// ErrEmptyDelegatee is returned when a grant does not name a delegatee.
	ErrEmptyDelegatee = errors.New("delegation: delegatee must not be empty")
	// ErrInvalidTimeWindow is returned when NotBefore is not before ExpiresAt.
	ErrInvalidTimeWindow = errors.New("delegation: NotBefore must be before ExpiresAt")
	// ErrNilAuthorizer is returned by NewManager when no source of authority
	// (usually a casbin enforcer) is supplied.
	ErrNilAuthorizer = errors.New("delegation: authorizer must not be nil")
	// ErrNilAdapter is returned by NewManager when no adapter is supplied.
	ErrNilAdapter = errors.New("delegation: adapter must not be nil")

	// ErrParentNotFound rejects a sub-delegation that refers to a grant that
	// does not exist: this is a broken chain.
	ErrParentNotFound = errors.New("delegation: parent grant not found (broken chain)")
	// ErrParentRevoked rejects a sub-delegation created under a revoked grant.
	ErrParentRevoked = errors.New("delegation: parent grant has been revoked")
	// ErrParentExpired rejects a sub-delegation created under an expired or
	// not-yet-valid grant.
	ErrParentExpired = errors.New("delegation: parent grant is not currently active")
	// ErrNotTheDelegatee rejects a sub-delegation issued by a subject other
	// than the parent grant's delegatee. Only the holder of a grant may pass
	// it on.
	ErrNotTheDelegatee = errors.New("delegation: delegator is not the delegatee of the parent grant")
	// ErrCannotSubGrant rejects a sub-delegation when the parent grant's
	// delegator did not allow the permission to be passed further down.
	ErrCannotSubGrant = errors.New("delegation: further sub-delegation is not permitted by the parent grant")
	// ErrNoDirectAuthority rejects a root delegation when the delegator does
	// not itself hold the scope it attempts to hand over, as decided by the
	// underlying casbin enforcer.
	ErrNoDirectAuthority = errors.New("delegation: delegator lacks direct authority for the requested scope")
	// ErrScopeWidened rejects a sub-delegation whose resource, action or time
	// window is not contained in the parent grant. Each level may only narrow.
	ErrScopeWidened = errors.New("delegation: child grant must not widen the scope of its parent")
	// ErrCycle rejects a grant that would close a delegation cycle.
	ErrCycle = errors.New("delegation: grant would create a cycle in the delegation chain")
	// ErrGrantNotFound is returned when revoking or inspecting an unknown id.
	ErrGrantNotFound = errors.New("delegation: grant not found")
	// ErrAdapterOperation wraps a persistence failure from the adapter.
	ErrAdapterOperation = errors.New("delegation: adapter operation failed")
)

// DenyReason explains why a delegated enforcement decision was denied.
type DenyReason int

const (
	// DeniedUnknown means no active delegation chain reached the request at all.
	DeniedUnknown DenyReason = iota
	// DeniedBrokenChain means a grant along the best chain disappeared.
	DeniedBrokenChain
	// DeniedRevoked means an ancestor (or the leaf) of the best chain was revoked.
	DeniedRevoked
	// DeniedExpired means an ancestor (or the leaf) of the best chain was outside
	// its validity window at decision time.
	DeniedExpired
	// DeniedScope means the request was not covered by an ancestor's scope.
	DeniedScope
	// DeniedRootAuthority means the root delegator's direct authorization no
	// longer covers the request.
	DeniedRootAuthority
)

// String returns a short human-readable description of the deny reason.
func (r DenyReason) String() string {
	switch r {
	case DeniedBrokenChain:
		return "broken chain"
	case DeniedRevoked:
		return "ancestor revoked"
	case DeniedExpired:
		return "ancestor not within validity window"
	case DeniedScope:
		return "request outside delegated scope"
	case DeniedRootAuthority:
		return "root delegator lacks direct authority"
	default:
		return "no delegated grant applies"
	}
}
