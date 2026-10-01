package delegation

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Authorizer is the source of truth for directly held permissions. It is the
// subset of the casbin Enforcer API used to verify that a root delegator really
// holds the scope it delegates, and it is re-checked on every delegated
// decision so that removing a direct policy immediately invalidates every
// chain rooted at it.
//
// *casbin.Enforcer satisfies this interface directly.
type Authorizer interface {
	Enforce(rvals ...interface{}) (bool, error)
}

// Option customizes a Manager at construction time.
type Option func(*Manager)

// WithResourceMatcher sets the matcher used to compare (and contain) resource
// scopes. The default is exact equality.
func WithResourceMatcher(m Matcher) Option {
	return func(mgr *Manager) { mgr.resourceMatcher = m }
}

// WithActionMatcher sets the matcher used to compare (and contain) action
// scopes. The default is exact equality.
func WithActionMatcher(m Matcher) Option {
	return func(mgr *Manager) { mgr.actionMatcher = m }
}

// WithClock overrides the time source. It is mainly useful in tests for
// exercising validity windows.
func WithClock(now func() time.Time) Option {
	return func(mgr *Manager) { mgr.now = now }
}

// Manager creates, stores and evaluates delegation chains.
//
// It never modifies the underlying casbin policy: root authority is only read
// through the Authorizer, so direct enforcement results are unaffected by any
// delegation operation.
type Manager struct {
	authorizer Authorizer
	adapter    Adapter

	resourceMatcher Matcher
	actionMatcher   Matcher
	now             func() time.Time

	mu          sync.RWMutex
	byID        map[string]*Grant
	byDelegatee map[string][]*Grant
}

// NewManager builds a delegation manager and loads existing grants from the
// adapter.
func NewManager(authorizer Authorizer, adapter Adapter, opts ...Option) (*Manager, error) {
	if authorizer == nil {
		return nil, ErrNilAuthorizer
	}
	if adapter == nil {
		return nil, ErrNilAdapter
	}

	mgr := &Manager{
		authorizer:      authorizer,
		adapter:         adapter,
		resourceMatcher: EqualMatcher,
		actionMatcher:   EqualMatcher,
		now:             time.Now,
		byID:            map[string]*Grant{},
		byDelegatee:     map[string][]*Grant{},
	}
	for _, opt := range opts {
		opt(mgr)
	}

	if err := mgr.reload(); err != nil {
		return nil, err
	}
	return mgr, nil
}

// reload (re)reads all grants from the adapter into the in-memory indexes.
func (mgr *Manager) reload() error {
	grants, err := mgr.adapter.LoadGrants()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAdapterOperation, err)
	}

	mgr.byID = make(map[string]*Grant, len(grants))
	mgr.byDelegatee = map[string][]*Grant{}
	for i := range grants {
		grant := grants[i]
		pointer := &grant
		mgr.byID[grant.ID] = pointer
		mgr.byDelegatee[grant.Delegatee] = append(mgr.byDelegatee[grant.Delegatee], pointer)
	}
	return nil
}

// Grant creates a root delegation: delegator temporarily hands scope to
// delegatee. The delegator must hold the scope directly, as decided by the
// underlying enforcer. canSubGrant decides whether the delegatee may pass the
// grant on. The id of the new grant is returned.
//
// extra values are appended to the enforcer request (after sub, obj, act)
// when the root delegator's direct authority is verified.
func (mgr *Manager) Grant(delegator, delegatee string, scope Scope, canSubGrant bool, extra ...interface{}) (string, error) {
	if delegator == "" {
		return "", ErrEmptyDelegator
	}
	if delegatee == "" {
		return "", ErrEmptyDelegatee
	}
	if err := validateWindow(scope); err != nil {
		return "", err
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	allowed, err := mgr.authorizer.Enforce(buildRvals(delegator, scope, extra)...)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", ErrNoDirectAuthority
	}

	grant := Grant{
		ID:          uuid.NewString(),
		Delegator:   delegator,
		Delegatee:   delegatee,
		Scope:       scope,
		CanSubGrant: canSubGrant,
		CreatedAt:   mgr.now(),
	}
	return mgr.persistNew(grant)
}

// GrantDelegation creates a sub-delegation under parentID: the current holder
// (the parent's delegatee) narrows scope and hands it to delegatee.
//
// It rejects broken chains, inactive or revoked ancestors, delegators other
// than the parent's delegatee, parents that forbid sub-delegation, cycles and
// any scope that is not contained in the parent scope.
func (mgr *Manager) GrantDelegation(delegator, delegatee, parentID string, scope Scope, canSubGrant bool) (string, error) {
	if delegator == "" {
		return "", ErrEmptyDelegator
	}
	if delegatee == "" {
		return "", ErrEmptyDelegatee
	}
	if parentID == "" {
		return "", ErrParentNotFound
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	parent, ok := mgr.byID[parentID]
	if !ok {
		return "", ErrParentNotFound
	}
	if delegator != parent.Delegatee {
		return "", ErrNotTheDelegatee
	}
	if !parent.CanSubGrant {
		return "", ErrCannotSubGrant
	}

	// The whole ancestor chain must currently be alive, otherwise authority
	// cannot flow down to the new grant.
	ancestors, err := mgr.activeAncestors(parent)
	if err != nil {
		return "", err
	}

	// Reject cycles: the delegatee must not already be a delegator on the
	// chain (this also covers handing the grant straight back).
	if delegatee == delegator {
		return "", ErrCycle
	}
	for _, ancestor := range ancestors {
		if ancestor.Delegator == delegatee {
			return "", ErrCycle
		}
	}

	narrowed, err := mgr.narrowScope(parent.Scope, scope)
	if err != nil {
		return "", err
	}

	grant := Grant{
		ID:          uuid.NewString(),
		ParentID:    parentID,
		Delegator:   delegator,
		Delegatee:   delegatee,
		Scope:       narrowed,
		CanSubGrant: canSubGrant,
		CreatedAt:   mgr.now(),
	}
	return mgr.persistNew(grant)
}

// activeAncestors walks parent and all of its ancestors and returns the full
// chain including parent in leaf-to-root order, rejecting any revoked,
// inactive or missing grant.
func (mgr *Manager) activeAncestors(parent *Grant) ([]*Grant, error) {
	chain := []*Grant{}
	visited := map[string]bool{}
	current := parent
	for current != nil {
		if visited[current.ID] {
			return nil, ErrCycle
		}
		visited[current.ID] = true

		if current.Revoked {
			return nil, ErrParentRevoked
		}
		if !current.Scope.activeAt(mgr.now()) {
			return nil, ErrParentExpired
		}
		chain = append(chain, current)

		if current.ParentID == "" {
			break
		}
		next, ok := mgr.byID[current.ParentID]
		if !ok {
			return nil, ErrParentNotFound
		}
		current = next
	}
	return chain, nil
}

// narrowScope verifies that child only narrows parent and returns the child
// scope with any unbounded time bound inherited from the parent.
func (mgr *Manager) narrowScope(parent, child Scope) (Scope, error) {
	if !mgr.resourceMatcher(child.Resource, parent.Resource) {
		return Scope{}, fmt.Errorf("%w: resource %q not covered by %q", ErrScopeWidened, child.Resource, parent.Resource)
	}
	if !mgr.actionMatcher(child.Action, parent.Action) {
		return Scope{}, fmt.Errorf("%w: action %q not covered by %q", ErrScopeWidened, child.Action, parent.Action)
	}

	// Inherit an unbounded bound from the parent.
	if child.NotBefore.IsZero() {
		child.NotBefore = parent.NotBefore
	}
	if child.ExpiresAt.IsZero() {
		child.ExpiresAt = parent.ExpiresAt
	}

	// Containment of the time window within the parent window.
	if !parent.NotBefore.IsZero() && child.NotBefore.Before(parent.NotBefore) {
		return Scope{}, fmt.Errorf("%w: NotBefore %v earlier than parent %v", ErrScopeWidened, child.NotBefore, parent.NotBefore)
	}
	if !parent.ExpiresAt.IsZero() && (child.ExpiresAt.IsZero() || child.ExpiresAt.After(parent.ExpiresAt)) {
		return Scope{}, fmt.Errorf("%w: ExpiresAt %v later than parent %v", ErrScopeWidened, child.ExpiresAt, parent.ExpiresAt)
	}
	if err := validateWindow(child); err != nil {
		return Scope{}, err
	}
	return child, nil
}

// validateWindow ensures NotBefore strictly precedes ExpiresAt when both set.
func validateWindow(scope Scope) error {
	if !scope.NotBefore.IsZero() && !scope.ExpiresAt.IsZero() && !scope.NotBefore.Before(scope.ExpiresAt) {
		return ErrInvalidTimeWindow
	}
	return nil
}

// persistNew saves a validated grant through the adapter and indexes it.
func (mgr *Manager) persistNew(grant Grant) (string, error) {
	if err := mgr.adapter.SaveGrant(grant); err != nil {
		return "", fmt.Errorf("%w: %v", ErrAdapterOperation, err)
	}
	stored := grant
	mgr.byID[grant.ID] = &stored
	mgr.byDelegatee[grant.Delegatee] = append(mgr.byDelegatee[grant.Delegatee], &stored)
	return grant.ID, nil
}

// Revoke withdraws the grant. Every grant below it in the chain immediately
// loses effect because decisions verify the whole chain.
func (mgr *Manager) Revoke(id string) error {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	grant, ok := mgr.byID[id]
	if !ok {
		return ErrGrantNotFound
	}
	if err := mgr.adapter.RevokeGrant(id); err != nil {
		return fmt.Errorf("%w: %v", ErrAdapterOperation, err)
	}
	grant.Revoked = true
	return nil
}

// GetGrant returns a copy of the grant with the given id.
func (mgr *Manager) GetGrant(id string) (Grant, error) {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	grant, ok := mgr.byID[id]
	if !ok {
		return Grant{}, ErrGrantNotFound
	}
	return grant.clone(), nil
}

// Chain returns the full delegation chain of grantID from the root grant down
// to grantID itself.
func (mgr *Manager) Chain(grantID string) ([]Grant, error) {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	leaf, ok := mgr.byID[grantID]
	if !ok {
		return nil, ErrGrantNotFound
	}

	reversed := []Grant{}
	visited := map[string]bool{}
	current := leaf
	for current != nil {
		if visited[current.ID] {
			return nil, ErrCycle
		}
		visited[current.ID] = true
		reversed = append(reversed, current.clone())

		if current.ParentID == "" {
			break
		}
		next, exists := mgr.byID[current.ParentID]
		if !exists {
			return nil, ErrParentNotFound
		}
		current = next
	}

	chain := make([]Grant, len(reversed))
	for i := range reversed {
		chain[i] = reversed[len(reversed)-1-i]
	}
	return chain, nil
}

// buildRvals assembles the enforcer request values for a root authority check.
func buildRvals(delegator string, scope Scope, extra []interface{}) []interface{} {
	rvals := []interface{}{delegator, scope.Resource, scope.Action}
	return append(rvals, extra...)
}
