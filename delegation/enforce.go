package delegation

import "time"

// Enforce reports whether subject may perform action on resource purely
// through a delegation chain. It does not consult any direct policy for the
// subject: a decision is allowed only when some chain rooted in a direct
// authority grants the request and every grant on that chain is alive and
// covers the request.
//
// extra values are appended to the enforcer request (after sub, obj, act)
// when the root delegator's direct authority is re-checked.
func (mgr *Manager) Enforce(subject, resource, action string, extra ...interface{}) (bool, error) {
	allowed, _, _, err := mgr.enforce(subject, resource, action, extra)
	return allowed, err
}

// EnforceEx behaves like Enforce and additionally reports why the best
// applicable chain was denied (DeniedUnknown when allowed) together with the
// chain that allowed the decision (nil when denied).
func (mgr *Manager) EnforceEx(subject, resource, action string, extra ...interface{}) (bool, DenyReason, []Grant, error) {
	return mgr.enforce(subject, resource, action, extra)
}

func (mgr *Manager) enforce(subject, resource, action string, extra []interface{}) (bool, DenyReason, []Grant, error) {
	now := mgr.now()

	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	// rank orders failure reasons: a chain that fails later is more
	// informative than one that fails at an earlier grant.
	rank := func(reason DenyReason) int {
		switch reason {
		case DeniedBrokenChain:
			return 1
		case DeniedRevoked:
			return 2
		case DeniedExpired:
			return 3
		case DeniedScope:
			return 4
		case DeniedRootAuthority:
			return 5
		default:
			return 0
		}
	}

	bestDepth := -1
	bestRank := -1
	bestReason := DeniedUnknown

	candidates := mgr.byDelegatee[subject]
	for _, leaf := range candidates {
		allowed, reason, chain, depth, err := mgr.evaluateChain(leaf, resource, action, now, extra)
		if err != nil {
			return false, DeniedUnknown, nil, err
		}
		if allowed {
			return true, DeniedUnknown, chain, nil
		}

		// Depth is the number of grants verified successfully from the leaf
		// upward; the chain that reached furthest is the most relevant one.
		if depth > bestDepth || (depth == bestDepth && rank(reason) > bestRank) {
			bestDepth = depth
			bestRank = rank(reason)
			bestReason = reason
		}
	}

	return false, bestReason, nil, nil
}

// evaluateChain walks a candidate chain leaf-to-root. It returns the chain in
// root-to-leaf order when allowed. When denied it returns the examined grants
// (in root-to-leaf order, ending at the failing grant), the reason, and the
// number of grants verified successfully starting from the leaf.
func (mgr *Manager) evaluateChain(leaf *Grant, resource, action string, now time.Time, extra []interface{}) (bool, DenyReason, []Grant, int, error) {
	reversed := []*Grant{}
	visited := map[string]bool{}

	current := leaf
	depth := 0
	for {
		if current == nil {
			return false, DeniedBrokenChain, mgr.toChain(reversed), depth, nil
		}
		if visited[current.ID] {
			// A cycle must never authorize; treat it like a dead chain.
			return false, DeniedBrokenChain, mgr.toChain(reversed), depth, nil
		}
		visited[current.ID] = true
		reversed = append(reversed, current)

		if current.Revoked {
			return false, DeniedRevoked, mgr.toChain(reversed), depth, nil
		}
		if !current.Scope.activeAt(now) {
			return false, DeniedExpired, mgr.toChain(reversed), depth, nil
		}
		if !mgr.covers(current, resource, action) {
			return false, DeniedScope, mgr.toChain(reversed), depth, nil
		}
		depth++

		if current.ParentID == "" {
			// Root of the chain: the request must still be covered by the
			// delegator's direct authority.
			allowed, err := mgr.authorizer.Enforce(buildRvals(current.Delegator, current.Scope.withRequest(resource, action), extra)...)
			if err != nil {
				return false, DeniedUnknown, nil, depth, err
			}
			if !allowed {
				return false, DeniedRootAuthority, mgr.toChain(reversed), depth, nil
			}
			return true, DeniedUnknown, mgr.toChain(reversed), depth, nil
		}

		parent, ok := mgr.byID[current.ParentID]
		if !ok {
			return false, DeniedBrokenChain, mgr.toChain(reversed), depth, nil
		}
		current = parent
	}
}

// covers reports whether a grant's scope contains the concrete request.
func (mgr *Manager) covers(grant *Grant, resource, action string) bool {
	return mgr.resourceMatcher(resource, grant.Scope.Resource) &&
		mgr.actionMatcher(action, grant.Scope.Action)
}

// toChain converts a leaf-to-root grant pointer list into root-to-leaf copies.
func (mgr *Manager) toChain(reversed []*Grant) []Grant {
	chain := make([]Grant, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		chain = append(chain, reversed[i].clone())
	}
	return chain
}

// withRequest returns a scope identical to s but carrying the concrete request
// values, so the root authority check uses the request rather than the stored
// (possibly patterned) scope.
func (s Scope) withRequest(resource, action string) Scope {
	s.Resource = resource
	s.Action = action
	return s
}
