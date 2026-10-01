package casbin

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/casbin/casbin/v3/rbac"
	"github.com/casbin/casbin/v3/util"
	"github.com/google/uuid"
)

var delegationMatcherCallPattern = regexp.MustCompile(`(?i)(keyMatch[2-5]?|regexMatch|ipMatch|globMatch)\s*\(\s*r[._](dom|domain|obj|object|act|action)\s*,\s*p[._](dom|domain|obj|object|act|action)\s*\)`)

var (
	ErrDelegationNotFound      = errors.New("delegation not found")
	ErrInvalidDelegation       = errors.New("invalid delegation")
	ErrDelegationCycle         = errors.New("delegation cycle")
	ErrDelegationBrokenChain   = errors.New("delegation chain is broken")
	ErrDelegationPrivilegeUp   = errors.New("delegation attempts to widen authority")
	ErrDelegationNotPermitted  = errors.New("delegation is not permitted")
	ErrDelegationRevoked       = errors.New("delegation has been revoked")
	ErrDelegationInvalidTime   = errors.New("delegation is outside its validity period")
	ErrDelegationParentInvalid = errors.New("parent delegation is invalid")
)

// DelegationRequest describes authority that one subject gives to another.
//
// Empty ParentID creates a root delegation. The delegator must directly hold the
// delegated scope at that time. A non-empty ParentID creates a child delegation;
// its scope, validity period and delegation right are constrained by the parent.
type DelegationRequest struct {
	ParentID    string
	Delegator   string
	Delegatee   string
	Domain      string
	Object      string
	Action      string
	StartsAt    time.Time
	ExpiresAt   time.Time
	CanDelegate bool
}

// Delegation is one edge in a delegation chain.
type Delegation struct {
	ID          string
	ParentID    string
	Delegator   string
	Delegatee   string
	Domain      string
	Object      string
	Action      string
	StartsAt    time.Time
	ExpiresAt   time.Time
	CanDelegate bool
	Revoked     bool
	CreatedAt   time.Time
}

// DelegatedPermission is kept as an alias for callers that model delegation as
// a transfer of a scoped permission.
type DelegatedPermission = Delegation

// SetDelegationMatchingFunc sets the matcher used for delegated objects and actions.
func (e *Enforcer) SetDelegationMatchingFunc(_ string, fn rbac.MatchingFunc) {
	e.SetDelegationObjectMatchingFunc(fn)
	e.SetDelegationActionMatchingFunc(fn)
}

// SetDelegationObjectMatchingFunc sets the matcher used for delegated objects.
func (e *Enforcer) SetDelegationObjectMatchingFunc(fn rbac.MatchingFunc) {
	e.delegationMu.Lock()
	defer e.delegationMu.Unlock()
	e.delegationObjFunc = fn
}

// SetDelegationActionMatchingFunc sets the matcher used for delegated actions.
func (e *Enforcer) SetDelegationActionMatchingFunc(fn rbac.MatchingFunc) {
	e.delegationMu.Lock()
	defer e.delegationMu.Unlock()
	e.delegationActFunc = fn
}

// SetDelegationDomainMatchingFunc sets the matcher used for delegated domains.
func (e *Enforcer) SetDelegationDomainMatchingFunc(fn rbac.MatchingFunc) {
	e.delegationMu.Lock()
	defer e.delegationMu.Unlock()
	e.delegationDomFunc = fn
}

// GrantDelegation validates and stores a delegation edge.
func (e *Enforcer) GrantDelegation(req DelegationRequest) (*Delegation, error) {
	req.StartsAt = normalizeDelegationTime(req.StartsAt)
	req.ExpiresAt = normalizeDelegationTime(req.ExpiresAt)
	if err := validateDelegationRequest(req); err != nil {
		return nil, err
	}

	e.delegationMu.Lock()
	if e.delegations == nil {
		e.delegations = make(map[string]*Delegation)
	}

	var ancestors []*Delegation
	if req.ParentID != "" {
		parent, ok := e.delegations[req.ParentID]
		if !ok {
			e.delegationMu.Unlock()
			return nil, ErrDelegationBrokenChain
		}

		chain, err := e.chainLocked(req.ParentID)
		if err != nil {
			e.delegationMu.Unlock()
			return nil, err
		}
		ancestors = chain

		if err := validateChildDelegation(req, parent, ancestors, time.Now(), e.delegationDomFunc, e.delegationObjFunc, e.delegationActFunc); err != nil {
			e.delegationMu.Unlock()
			return nil, err
		}
	}

	delegation := &Delegation{
		ID:          uuid.NewString(),
		ParentID:    req.ParentID,
		Delegator:   req.Delegator,
		Delegatee:   req.Delegatee,
		Domain:      req.Domain,
		Object:      req.Object,
		Action:      req.Action,
		StartsAt:    req.StartsAt,
		ExpiresAt:   req.ExpiresAt,
		CanDelegate: req.CanDelegate,
		CreatedAt:   time.Now(),
	}
	e.delegations[delegation.ID] = delegation
	e.delegationMu.Unlock()

	if req.ParentID == "" {
		if err := e.assertDirectAuthority(req); err != nil {
			e.delegationMu.Lock()
			delete(e.delegations, delegation.ID)
			e.delegationMu.Unlock()
			return nil, err
		}
	}

	return copyDelegation(delegation), nil
}

// DelegatePermission is an alias for GrantDelegation.
func (e *Enforcer) DelegatePermission(req DelegationRequest) (*Delegation, error) {
	return e.GrantDelegation(req)
}

// RevokeDelegation revokes one delegation. Descendant delegations stop being
// effective because every enforcement traverses and checks the complete chain.
func (e *Enforcer) RevokeDelegation(id string) (bool, error) {
	e.delegationMu.Lock()
	defer e.delegationMu.Unlock()

	delegation, ok := e.delegations[id]
	if !ok {
		return false, ErrDelegationNotFound
	}
	if delegation.Revoked {
		return false, nil
	}

	delegation.Revoked = true
	return true, nil
}

// GetDelegation returns one delegation edge.
func (e *Enforcer) GetDelegation(id string) (*Delegation, bool) {
	e.delegationMu.RLock()
	defer e.delegationMu.RUnlock()

	delegation, ok := e.delegations[id]
	if !ok {
		return nil, false
	}
	return copyDelegation(delegation), true
}

// GetDelegations returns all delegation edges.
func (e *Enforcer) GetDelegations() []*Delegation {
	e.delegationMu.RLock()
	defer e.delegationMu.RUnlock()

	delegations := make([]*Delegation, 0, len(e.delegations))
	for _, delegation := range e.delegations {
		delegations = append(delegations, copyDelegation(delegation))
	}
	return delegations
}

// EnforceDelegation checks an access request using either direct policy or a
// currently valid delegation chain.
func (e *Enforcer) EnforceDelegation(subject, object, action string, at ...time.Time) (bool, error) {
	return e.EnforceDelegationInDomain("", subject, object, action, at...)
}

// EnforceDelegatedPermission is an alias for EnforceDelegation.
func (e *Enforcer) EnforceDelegatedPermission(subject, object, action string, at ...time.Time) (bool, error) {
	return e.EnforceDelegation(subject, object, action, at...)
}

// EnforceDelegationInDomain checks a domain-scoped direct or delegated request.
func (e *Enforcer) EnforceDelegationInDomain(domain, subject, object, action string, at ...time.Time) (bool, error) {
	now := time.Now()
	if len(at) > 0 {
		now = at[0]
	}

	if ok, err := e.enforceDirect(domain, subject, object, action); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}

	e.delegationMu.RLock()
	objectMatcher := e.delegationObjFunc
	actionMatcher := e.delegationActFunc
	domainMatcher := e.delegationDomFunc
	var candidates []*Delegation
	for _, delegation := range e.delegations {
		if delegation.Delegatee == subject {
			candidates = append(candidates, copyDelegation(delegation))
		}
	}
	e.delegationMu.RUnlock()

	for _, leaf := range candidates {
		e.delegationMu.RLock()
		chain, err := e.chainLocked(leaf.ID)
		e.delegationMu.RUnlock()
		if err != nil {
			continue
		}

		if err := validateEffectiveChain(chain, domain, object, action, now, domainMatcher, objectMatcher, actionMatcher); err != nil {
			continue
		}
		if ok, err := e.enforceDirect(domain, chain[len(chain)-1].Delegator, object, action); err != nil {
			return false, err
		} else if ok {
			return true, nil
		}
	}

	return false, nil
}

func validateDelegationRequest(req DelegationRequest) error {
	if strings.TrimSpace(req.Delegator) == "" || strings.TrimSpace(req.Delegatee) == "" ||
		strings.TrimSpace(req.Object) == "" || strings.TrimSpace(req.Action) == "" {
		return ErrInvalidDelegation
	}
	if !req.StartsAt.IsZero() && !req.ExpiresAt.IsZero() && req.ExpiresAt.Before(req.StartsAt) {
		return ErrInvalidDelegation
	}
	if !req.ExpiresAt.IsZero() && req.ExpiresAt.Before(normalizeDelegationTime(time.Now())) {
		return ErrInvalidDelegation
	}
	return nil
}

func validateChildDelegation(req DelegationRequest, parent *Delegation, ancestors []*Delegation, now time.Time, domainMatcher, objectMatcher, actionMatcher rbac.MatchingFunc) error {
	if parent.Delegatee != req.Delegator {
		return ErrDelegationBrokenChain
	}

	now = normalizeDelegationTime(now)
	for _, ancestor := range ancestors {
		if ancestor.Revoked {
			return ErrDelegationParentInvalid
		}
		if !ancestor.ExpiresAt.IsZero() && now.After(ancestor.ExpiresAt) {
			return ErrDelegationParentInvalid
		}
	}

	if !parent.CanDelegate {
		return ErrDelegationNotPermitted
	}
	if !domainMatcher(req.Domain, parent.Domain) {
		return ErrDelegationPrivilegeUp
	}
	if !objectMatcher(req.Object, parent.Object) || !actionMatcher(req.Action, parent.Action) {
		return ErrDelegationPrivilegeUp
	}
	if !containsTime(req.StartsAt, parent.StartsAt, req.ExpiresAt, parent.ExpiresAt) {
		return ErrDelegationPrivilegeUp
	}

	if req.Delegator == req.Delegatee {
		return ErrDelegationCycle
	}

	delegators := make(map[string]struct{})
	for i := len(ancestors) - 1; i >= 0; i-- {
		ancestor := ancestors[i]
		delegators[ancestor.Delegator] = struct{}{}
	}
	if _, ok := delegators[req.Delegatee]; ok {
		return ErrDelegationCycle
	}

	return nil
}

func validateEffectiveChain(chain []*Delegation, domain, object, action string, at time.Time, domainMatcher, objectMatcher, actionMatcher rbac.MatchingFunc) error {
	at = normalizeDelegationTime(at)
	for _, delegation := range chain {
		if delegation.Revoked {
			return ErrDelegationRevoked
		}
		if !domainMatcher(domain, delegation.Domain) || !objectMatcher(object, delegation.Object) || !actionMatcher(action, delegation.Action) {
			return ErrDelegationPrivilegeUp
		}
		if !delegation.StartsAt.IsZero() && at.Before(delegation.StartsAt) {
			return ErrDelegationInvalidTime
		}
		if !delegation.ExpiresAt.IsZero() && at.After(delegation.ExpiresAt) {
			return ErrDelegationInvalidTime
		}
	}
	return nil
}

func normalizeDelegationTime(value time.Time) time.Time {
	if value.IsZero() {
		return value
	}
	return value.Round(0)
}

func containsTime(childStart, parentStart, childEnd, parentEnd time.Time) bool {
	if !parentStart.IsZero() && (childStart.IsZero() || childStart.Before(parentStart)) {
		return false
	}
	if !parentEnd.IsZero() && (childEnd.IsZero() || childEnd.After(parentEnd)) {
		return false
	}
	return true
}

func (e *Enforcer) chainLocked(leafID string) ([]*Delegation, error) {
	var chain []*Delegation
	visited := make(map[string]struct{})
	currentID := leafID

	for currentID != "" {
		if _, ok := visited[currentID]; ok {
			return nil, ErrDelegationCycle
		}
		visited[currentID] = struct{}{}

		delegation, ok := e.delegations[currentID]
		if !ok {
			return nil, ErrDelegationBrokenChain
		}
		chain = append(chain, copyDelegation(delegation))

		if delegation.ParentID == "" {
			break
		}

		parent, ok := e.delegations[delegation.ParentID]
		if !ok || parent.Delegatee != delegation.Delegator {
			return nil, ErrDelegationBrokenChain
		}
		currentID = delegation.ParentID
	}

	return chain, nil
}

func (e *Enforcer) assertDirectAuthority(req DelegationRequest) error {
	ok, err := e.enforceDirect(req.Domain, req.Delegator, req.Object, req.Action)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDelegationPrivilegeUp
	}
	return nil
}

func (e *Enforcer) enforceDirect(domain, subject, object, action string) (bool, error) {
	if domain == "" {
		return e.Enforce(subject, object, action)
	}
	return e.Enforce(subject, domain, object, action)
}

func copyDelegation(delegation *Delegation) *Delegation {
	copied := *delegation
	return &copied
}

func exactMatch(value, pattern string) bool {
	return value == pattern
}

func globMatch(value, pattern string) bool {
	matched, err := util.GlobMatch(value, pattern)
	return err == nil && matched
}

func regexMatch(value, pattern string) bool {
	return util.RegexMatch(value, pattern)
}

func ipMatch(value, pattern string) bool {
	return util.IPMatch(value, pattern)
}

func (e *Enforcer) configureDelegationMatchersFromModel() {
	if e.model == nil || e.model["m"] == nil || e.model["m"]["m"] == nil {
		return
	}

	matcher := e.model["m"]["m"].Value
	for _, match := range delegationMatcherCallPattern.FindAllStringSubmatch(matcher, -1) {
		if !strings.EqualFold(match[2], match[3]) {
			continue
		}
		fn := delegationMatcherFunc(match[1])
		if fn == nil {
			continue
		}
		switch strings.ToLower(match[2]) {
		case "obj", "object":
			e.delegationObjFunc = fn
		case "act", "action":
			e.delegationActFunc = fn
		case "dom", "domain":
			e.delegationDomFunc = fn
		}
	}
}

func delegationMatcherFunc(name string) rbac.MatchingFunc {
	switch strings.ToLower(name) {
	case "keymatch":
		return util.KeyMatch
	case "keymatch2":
		return util.KeyMatch2
	case "keymatch3":
		return util.KeyMatch3
	case "keymatch4":
		return util.KeyMatch4
	case "keymatch5":
		return util.KeyMatch5
	case "regexmatch":
		return regexMatch
	case "ipmatch":
		return ipMatch
	case "globmatch":
		return globMatch
	}
	return nil
}
