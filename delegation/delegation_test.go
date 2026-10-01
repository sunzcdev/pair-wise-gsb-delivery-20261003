package delegation

import (
	"errors"
	"testing"
	"time"

	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/util"
)

const basicModelText = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = r.sub == p.sub && r.obj == p.obj && r.act == p.act
`

func newBasicEnforcer(t *testing.T) *fakeEnforcer {
	t.Helper()
	m, err := model.NewModelFromString(basicModelText)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	return newFakeEnforcer(t, m)
}

func TestRootGrantRequiresDirectAuthority(t *testing.T) {
	auth := newBasicEnforcer(t)
	mgr := newManager(t, auth)

	scope := Scope{Resource: "docs/1", Action: "read"}

	// Alice has no direct policy: she cannot delegate authority she lacks.
	if _, err := mgr.Grant("alice", "bob", scope, true); !errors.Is(err, ErrNoDirectAuthority) {
		t.Fatalf("expected ErrNoDirectAuthority, got %v", err)
	}

	auth.allow("alice", "docs/1", "read")
	id := mustGrant(t, mgr, "alice", "bob", scope, true)
	if id == "" {
		t.Fatal("expected non-empty grant id")
	}

	// The delegatee is authorized through the chain while the delegator is
	// unaffected, and out-of-scope requests stay denied.
	wantAllowed(t, mgr, "bob", "docs/1", "read", true)
	wantAllowed(t, mgr, "bob", "docs/1", "write", false)
	wantAllowed(t, mgr, "bob", "docs/2", "read", false)
	wantAllowed(t, mgr, "carol", "docs/1", "read", false)
}

func TestDelegationDoesNotChangeDirectEnforcement(t *testing.T) {
	auth := newBasicEnforcer(t)
	auth.allow("alice", "docs/1", "read")
	mgr := newManager(t, auth)

	mustGrant(t, mgr, "alice", "bob", Scope{Resource: "docs/1", Action: "read"}, true)

	// Direct casbin decisions are untouched by the delegation interface.
	if ok, _ := auth.Enforce("alice", "docs/1", "read"); !ok {
		t.Fatal("alice direct read should still be allowed")
	}
	if ok, _ := auth.Enforce("bob", "docs/1", "read"); ok {
		t.Fatal("bob must not gain a direct policy from a delegation grant")
	}
	if ok, _ := auth.Enforce("alice", "docs/1", "write"); ok {
		t.Fatal("delegation must not widen alice's direct authority")
	}
}

func TestSubGrantNarrowsResourceActionAndTime(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	auth := newBasicEnforcer(t)
	auth.allow("alice", "docs/1", "read")
	mgr, clock := newManagerAt(t, auth, now)

	root := mustGrant(t, mgr, "alice", "bob", Scope{
		Resource:  "docs/1",
		Action:    "read",
		NotBefore: now.Add(-time.Hour),
		ExpiresAt: now.Add(time.Hour),
	}, true)

	// Widening a resource must be rejected (exact matcher).
	if _, err := mgr.GrantDelegation("bob", "carol", root,
		Scope{Resource: "docs/2", Action: "read"}, true); !errors.Is(err, ErrScopeWidened) {
		t.Fatalf("widen resource: expected ErrScopeWidened, got %v", err)
	}

	// Widening an action must be rejected.
	if _, err := mgr.GrantDelegation("bob", "carol", root,
		Scope{Resource: "docs/1", Action: "write"}, true); !errors.Is(err, ErrScopeWidened) {
		t.Fatalf("widen action: expected ErrScopeWidened, got %v", err)
	}

	// Extending the validity window must be rejected.
	if _, err := mgr.GrantDelegation("bob", "carol", root, Scope{
		Resource:  "docs/1",
		Action:    "read",
		NotBefore: now.Add(-2 * time.Hour),
		ExpiresAt: now,
	}, true); !errors.Is(err, ErrScopeWidened) {
		t.Fatalf("earlier NotBefore: expected ErrScopeWidened, got %v", err)
	}
	if _, err := mgr.GrantDelegation("bob", "carol", root, Scope{
		Resource:  "docs/1",
		Action:    "read",
		NotBefore: now,
		ExpiresAt: now.Add(2 * time.Hour),
	}, true); !errors.Is(err, ErrScopeWidened) {
		t.Fatalf("later ExpiresAt: expected ErrScopeWidened, got %v", err)
	}

	// A strictly narrower window is accepted.
	leaf := mustSubGrant(t, mgr, "bob", "carol", root, Scope{
		Resource:  "docs/1",
		Action:    "read",
		NotBefore: now,
		ExpiresAt: now.Add(30 * time.Minute),
	}, true)

	chain, err := mgr.Chain(leaf)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(chain) != 2 || chain[0].ID != root || chain[1].ID != leaf {
		t.Fatalf("unexpected chain order: %+v", chain)
	}

	wantAllowed(t, mgr, "carol", "docs/1", "read", true)

	// Outside the child window the leaf is ineffective even though the parent
	// window still covers the time.
	advance(clock, now.Add(45*time.Minute))
	ok, reason, _, err := mgr.EnforceEx("carol", "docs/1", "read")
	if err != nil {
		t.Fatalf("EnforceEx: %v", err)
	}
	if ok || reason != DeniedExpired {
		t.Fatalf("expected DeniedExpired, got ok=%v reason=%v", ok, reason)
	}
}

func TestCanSubGrantDecidedByParent(t *testing.T) {
	auth := newBasicEnforcer(t)
	auth.allow("alice", "docs/1", "read")
	mgr := newManager(t, auth)

	// Alice does not allow Bob to pass the grant on.
	root := mustGrant(t, mgr, "alice", "bob", Scope{Resource: "docs/1", Action: "read"}, false)

	if _, err := mgr.GrantDelegation("bob", "carol", root,
		Scope{Resource: "docs/1", Action: "read"}, true); !errors.Is(err, ErrCannotSubGrant) {
		t.Fatalf("expected ErrCannotSubGrant, got %v", err)
	}

	// Only the current holder may delegate from the parent.
	if _, err := mgr.GrantDelegation("mallory", "carol", root,
		Scope{Resource: "docs/1", Action: "read"}, true); !errors.Is(err, ErrNotTheDelegatee) {
		t.Fatalf("expected ErrNotTheDelegatee, got %v", err)
	}

	wantAllowed(t, mgr, "bob", "docs/1", "read", true)
	wantAllowed(t, mgr, "carol", "docs/1", "read", false)
}

func TestBrokenChainAndMissingParentRejected(t *testing.T) {
	auth := newBasicEnforcer(t)
	auth.allow("alice", "docs/1", "read")
	mgr := newManager(t, auth)

	if _, err := mgr.GrantDelegation("bob", "carol", "missing-id",
		Scope{Resource: "docs/1", Action: "read"}, true); !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("expected ErrParentNotFound, got %v", err)
	}

	root := mustGrant(t, mgr, "alice", "bob", Scope{Resource: "docs/1", Action: "read"}, true)

	// Simulate a broken chain: the parent grant is gone from the store while a
	// child still references it.
	child := mustSubGrant(t, mgr, "bob", "carol", root,
		Scope{Resource: "docs/1", Action: "read"}, true)
	mgr.mu.Lock()
	delete(mgr.byID, root)
	mgr.mu.Unlock()

	ok, reason, _, err := mgr.EnforceEx("carol", "docs/1", "read")
	if err != nil {
		t.Fatalf("EnforceEx: %v", err)
	}
	if ok || reason != DeniedBrokenChain {
		t.Fatalf("expected DeniedBrokenChain, got ok=%v reason=%v", ok, reason)
	}
	if _, err := mgr.Chain(child); !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("Chain expected ErrParentNotFound, got %v", err)
	}
}

func TestCyclesRejected(t *testing.T) {
	auth := newBasicEnforcer(t)
	auth.allow("alice", "docs/1", "read")
	mgr := newManager(t, auth)

	root := mustGrant(t, mgr, "alice", "bob", Scope{Resource: "docs/1", Action: "read"}, true)
	first := mustSubGrant(t, mgr, "bob", "carol", root,
		Scope{Resource: "docs/1", Action: "read"}, true)

	// Handing the grant back to an earlier delegator would close a cycle.
	if _, err := mgr.GrantDelegation("carol", "bob", first,
		Scope{Resource: "docs/1", Action: "read"}, true); !errors.Is(err, ErrCycle) {
		t.Fatalf("expected ErrCycle, got %v", err)
	}
	if _, err := mgr.GrantDelegation("carol", "alice", first,
		Scope{Resource: "docs/1", Action: "read"}, true); !errors.Is(err, ErrCycle) {
		t.Fatalf("expected ErrCycle for root delegator, got %v", err)
	}
	// Self-delegation is also a cycle.
	if _, err := mgr.GrantDelegation("carol", "carol", first,
		Scope{Resource: "docs/1", Action: "read"}, true); !errors.Is(err, ErrCycle) {
		t.Fatalf("expected ErrCycle for self grant, got %v", err)
	}
}

func TestRevocationCascadesDownTheChain(t *testing.T) {
	auth := newBasicEnforcer(t)
	auth.allow("alice", "docs/1", "read")
	mgr := newManager(t, auth)

	root := mustGrant(t, mgr, "alice", "bob", Scope{Resource: "docs/1", Action: "read"}, true)
	mid := mustSubGrant(t, mgr, "bob", "carol", root,
		Scope{Resource: "docs/1", Action: "read"}, true)
	mustSubGrant(t, mgr, "carol", "dave", mid,
		Scope{Resource: "docs/1", Action: "read"}, true)

	wantAllowed(t, mgr, "dave", "docs/1", "read", true)

	// Bob revokes the middle grant: the leaf can no longer stand on its own.
	if err := mgr.Revoke(mid); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	ok, reason, _, err := mgr.EnforceEx("dave", "docs/1", "read")
	if err != nil {
		t.Fatalf("EnforceEx: %v", err)
	}
	if ok || reason != DeniedRevoked {
		t.Fatalf("expected DeniedRevoked, got ok=%v reason=%v", ok, reason)
	}

	// Bob's own grant is unaffected by revoking a descendant, but revoking the
	// root also takes Bob down.
	wantAllowed(t, mgr, "bob", "docs/1", "read", true)
	if err := mgr.Revoke(root); err != nil {
		t.Fatalf("Revoke root: %v", err)
	}
	if ok, _ := mgr.Enforce("bob", "docs/1", "read"); ok {
		t.Fatal("bob must be denied after root revocation")
	}

	if err := mgr.Revoke("does-not-exist"); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("expected ErrGrantNotFound, got %v", err)
	}
}

func TestRootAuthorityRemovalInvalidatesChain(t *testing.T) {
	auth := newBasicEnforcer(t)
	auth.allow("alice", "docs/1", "read")
	mgr := newManager(t, auth)

	root := mustGrant(t, mgr, "alice", "bob", Scope{Resource: "docs/1", Action: "read"}, true)
	mustSubGrant(t, mgr, "bob", "carol", root, Scope{Resource: "docs/1", Action: "read"}, true)

	wantAllowed(t, mgr, "carol", "docs/1", "read", true)

	// The direct policy backing the chain is removed: the whole chain is dead
	// even though no delegation grant was revoked.
	auth.remove("alice", "docs/1", "read")
	ok, reason, _, err := mgr.EnforceEx("carol", "docs/1", "read")
	if err != nil {
		t.Fatalf("EnforceEx: %v", err)
	}
	if ok || reason != DeniedRootAuthority {
		t.Fatalf("expected DeniedRootAuthority, got ok=%v reason=%v", ok, reason)
	}
}

func TestGrantExpiration(t *testing.T) {
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	auth := newBasicEnforcer(t)
	auth.allow("alice", "docs/1", "read")
	mgr, clock := newManagerAt(t, auth, start)

	mustGrant(t, mgr, "alice", "bob", Scope{
		Resource:  "docs/1",
		Action:    "read",
		NotBefore: start.Add(time.Hour),
		ExpiresAt: start.Add(2 * time.Hour),
	}, true)

	// Not yet valid.
	if ok, _ := mgr.Enforce("bob", "docs/1", "read"); ok {
		t.Fatal("grant should not be valid before NotBefore")
	}

	advance(clock, start.Add(90*time.Minute))
	wantAllowed(t, mgr, "bob", "docs/1", "read", true)

	// ExpiresAt is exclusive.
	advance(clock, start.Add(2*time.Hour))
	ok, reason, _, err := mgr.EnforceEx("bob", "docs/1", "read")
	if err != nil {
		t.Fatalf("EnforceEx: %v", err)
	}
	if ok || reason != DeniedExpired {
		t.Fatalf("expected DeniedExpired after ExpiresAt, got ok=%v reason=%v", ok, reason)
	}

	// Invalid window must be rejected up front.
	bad := Scope{
		Resource:  "docs/1",
		Action:    "read",
		NotBefore: start.Add(2 * time.Hour),
		ExpiresAt: start.Add(time.Hour),
	}
	if _, err := mgr.Grant("alice", "bob", bad, true); !errors.Is(err, ErrInvalidTimeWindow) {
		t.Fatalf("expected ErrInvalidTimeWindow, got %v", err)
	}
}

func TestSubGrantUnderInactiveAncestorRejected(t *testing.T) {
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	auth := newBasicEnforcer(t)
	auth.allow("alice", "docs/1", "read")
	mgr, clock := newManagerAt(t, auth, start)

	root := mustGrant(t, mgr, "alice", "bob", Scope{
		Resource:  "docs/1",
		Action:    "read",
		ExpiresAt: start.Add(time.Hour),
	}, true)

	advance(clock, start.Add(2*time.Hour))
	if _, err := mgr.GrantDelegation("bob", "carol", root,
		Scope{Resource: "docs/1", Action: "read"}, true); !errors.Is(err, ErrParentExpired) {
		t.Fatalf("expected ErrParentExpired, got %v", err)
	}

	advance(clock, start)
	mid := mustSubGrant(t, mgr, "bob", "carol", root,
		Scope{Resource: "docs/1", Action: "read"}, true)
	if err := mgr.Revoke(root); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := mgr.GrantDelegation("carol", "dave", mid,
		Scope{Resource: "docs/1", Action: "read"}, true); !errors.Is(err, ErrParentRevoked) {
		t.Fatalf("expected ErrParentRevoked, got %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	auth := newBasicEnforcer(t)
	auth.allow("alice", "docs/1", "read")

	if _, err := NewManager(nil, NewMemoryAdapter()); !errors.Is(err, ErrNilAuthorizer) {
		t.Fatalf("expected ErrNilAuthorizer, got %v", err)
	}
	if _, err := NewManager(auth, nil); !errors.Is(err, ErrNilAdapter) {
		t.Fatalf("expected ErrNilAdapter, got %v", err)
	}

	mgr := newManager(t, auth)
	scope := Scope{Resource: "docs/1", Action: "read"}
	if _, err := mgr.Grant("", "bob", scope, true); !errors.Is(err, ErrEmptyDelegator) {
		t.Fatalf("expected ErrEmptyDelegator, got %v", err)
	}
	if _, err := mgr.Grant("alice", "", scope, true); !errors.Is(err, ErrEmptyDelegatee) {
		t.Fatalf("expected ErrEmptyDelegatee, got %v", err)
	}
}

const patternModelText = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = r.sub == p.sub && keyMatch2(r.obj, p.obj) && regexMatch(r.act, p.act)
`

func TestScopesUseRequestMatchers(t *testing.T) {
	m, err := model.NewModelFromString(patternModelText)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	auth := newFakeEnforcer(t, m)
	auth.allow("alice", "/docs/*", "(read)|(write)")

	mgr, err := NewManager(auth, NewMemoryAdapter(),
		WithResourceMatcher(func(value, pattern string) bool { return util.KeyMatch2(value, pattern) }),
		WithActionMatcher(func(value, pattern string) bool { return util.RegexMatch(value, pattern) }),
	)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	// Alice's direct scope is expressed with patterns. The root check uses the
	// concrete request value against the enforcer.
	rootScope := Scope{Resource: "/docs/*", Action: "(read)|(write)"}
	root := mustGrant(t, mgr, "alice", "bob", rootScope, true)

	// A narrower pattern and a narrower action regex are accepted.
	narrow := Scope{Resource: "/docs/:id", Action: "read"}
	leaf := mustSubGrant(t, mgr, "bob", "carol", root, narrow, true)

	wantAllowed(t, mgr, "carol", "/docs/42", "read", true)
	wantAllowed(t, mgr, "carol", "/docs/42", "write", false)
	wantAllowed(t, mgr, "carol", "/other/42", "read", false)

	// Containment follows the request-matching direction: concrete child
	// values must match the parent's pattern. Values escaping it are rejected.
	if _, err := mgr.GrantDelegation("carol", "dave", leaf,
		Scope{Resource: "/other/9", Action: "read"}, true); !errors.Is(err, ErrScopeWidened) {
		t.Fatalf("expected ErrScopeWidened for escaped resource, got %v", err)
	}
	if _, err := mgr.GrantDelegation("carol", "dave", leaf,
		Scope{Resource: "/docs/9", Action: "write"}, true); !errors.Is(err, ErrScopeWidened) {
		t.Fatalf("expected ErrScopeWidened for widened action, got %v", err)
	}
}

func TestGrantsReloadedFromAdapter(t *testing.T) {
	auth := newBasicEnforcer(t)
	auth.allow("alice", "docs/1", "read")

	adapter := NewMemoryAdapter()
	mgr, err := NewManager(auth, adapter)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	root := mustGrant(t, mgr, "alice", "bob", Scope{Resource: "docs/1", Action: "read"}, true)
	mustSubGrant(t, mgr, "bob", "carol", root, Scope{Resource: "docs/1", Action: "read"}, true)

	// A fresh manager backed by the same adapter sees the chain and the
	// revocation state.
	reloaded, err := NewManager(auth, adapter)
	if err != nil {
		t.Fatalf("reload manager: %v", err)
	}
	wantAllowed(t, reloaded, "carol", "docs/1", "read", true)

	if err := mgr.Revoke(root); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	reloaded2, err := NewManager(auth, adapter)
	if err != nil {
		t.Fatalf("reload manager 2: %v", err)
	}
	wantAllowed(t, reloaded2, "carol", "docs/1", "read", false)

	grant, err := reloaded2.GetGrant(root)
	if err != nil {
		t.Fatalf("GetGrant: %v", err)
	}
	if !grant.Revoked {
		t.Fatal("reloaded grant should be marked revoked")
	}
}
