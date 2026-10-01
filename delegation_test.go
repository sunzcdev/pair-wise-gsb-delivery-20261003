package casbin

import (
	"errors"
	"testing"
	"time"

	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/util"
)

func newDelegationTestEnforcer(t *testing.T) *Enforcer {
	t.Helper()

	e, err := NewEnforcer("examples/keymatch_model.conf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddPolicies([][]string{
		{"alice", "/docs/*", "read|write"},
		{"owner", "/data/*", "read"},
	}); err != nil {
		t.Fatal(err)
	}
	e.SetDelegationActionMatchingFunc(func(value, pattern string) bool {
		return value == pattern || (pattern == "read|write" && (value == "read" || value == "write"))
	})
	return e
}

func TestDelegationNarrowsScopeAndValidity(t *testing.T) {
	e := newDelegationTestEnforcer(t)
	now := time.Now()

	root, err := e.GrantDelegation(DelegationRequest{
		Delegator:   "alice",
		Delegatee:   "bob",
		Object:      "/docs/*",
		Action:      "read|write",
		StartsAt:    now.Add(-time.Hour),
		ExpiresAt:   now.Add(time.Hour),
		CanDelegate: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	child, err := e.GrantDelegation(DelegationRequest{
		ParentID:  root.ID,
		Delegator: "bob",
		Delegatee: "carol",
		Object:    "/docs/report",
		Action:    "read",
		StartsAt:  now.Add(-time.Minute),
		ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	ok, err := e.EnforceDelegation("carol", "/docs/report", "read")
	if err != nil || !ok {
		t.Fatalf("expected narrowed delegation to allow, got %v, %v", ok, err)
	}

	ok, err = e.Enforce("carol", "/docs/report", "read")
	if err != nil || ok {
		t.Fatalf("direct enforcement must ignore delegations, got %v, %v", ok, err)
	}

	for _, request := range []DelegationRequest{
		{ParentID: root.ID, Delegator: "bob", Delegatee: "carol", Object: "/other", Action: "read"},
		{ParentID: root.ID, Delegator: "bob", Delegatee: "carol", Object: "/docs/report", Action: "write"},
		{ParentID: root.ID, Delegator: "bob", Delegatee: "carol", Object: "/docs/report", Action: "read", StartsAt: now.Add(-2 * time.Hour), ExpiresAt: now},
		{ParentID: root.ID, Delegator: "bob", Delegatee: "carol", Object: "/docs/report", Action: "read", StartsAt: now, ExpiresAt: now.Add(2 * time.Hour)},
	} {
		if _, err := e.GrantDelegation(request); !errors.Is(err, ErrDelegationPrivilegeUp) {
			t.Fatalf("expected widening request to be rejected, got %v", err)
		}
	}

	if ok, err := e.EnforceDelegation("carol", "/docs/admin", "read", now); err != nil || ok {
		t.Fatalf("expected object outside child scope to deny, got %v, %v", ok, err)
	}
	if ok, err := e.EnforceDelegation("carol", "/docs/report", "write", now); err != nil || ok {
		t.Fatalf("expected action outside child scope to deny, got %v, %v", ok, err)
	}

	ok, err = e.EnforceDelegation("carol", "/docs/report", "read", child.StartsAt.Add(-time.Second))
	if err != nil || ok {
		t.Fatalf("expected not-yet-valid child to deny, got %v, %v", ok, err)
	}
	ok, err = e.EnforceDelegation("carol", "/docs/report", "read", child.ExpiresAt.Add(time.Second))
	if err != nil || ok {
		t.Fatalf("expected expired child to deny, got %v, %v", ok, err)
	}
}

func TestDelegationCanDelegateIsControlledByParent(t *testing.T) {
	e := newDelegationTestEnforcer(t)

	root, err := e.GrantDelegation(DelegationRequest{
		Delegator: "alice",
		Delegatee: "bob",
		Object:    "/docs/*",
		Action:    "read",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.GrantDelegation(DelegationRequest{
		ParentID:  root.ID,
		Delegator: "bob",
		Delegatee: "carol",
		Object:    "/docs/a",
		Action:    "read",
	})
	if !errors.Is(err, ErrDelegationNotPermitted) {
		t.Fatalf("expected non-delegatable parent to reject child, got %v", err)
	}

	root.CanDelegate = true
	stored, ok := e.GetDelegation(root.ID)
	if !ok || stored.CanDelegate {
		t.Fatal("GetDelegation must return a copy and the first grant must remain non-delegatable")
	}
}

func TestDelegationRevokesAnyAncestor(t *testing.T) {
	e := newDelegationTestEnforcer(t)

	root, err := e.GrantDelegation(DelegationRequest{
		Delegator:   "alice",
		Delegatee:   "bob",
		Object:      "/docs/*",
		Action:      "read",
		CanDelegate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := e.GrantDelegation(DelegationRequest{
		ParentID:    root.ID,
		Delegator:   "bob",
		Delegatee:   "carol",
		Object:      "/docs/a",
		Action:      "read",
		CanDelegate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := e.GrantDelegation(DelegationRequest{
		ParentID:    child.ID,
		Delegator:   "carol",
		Delegatee:   "dave",
		Object:      "/docs/a",
		Action:      "read",
		CanDelegate: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := e.EnforceDelegation("dave", "/docs/a", "read"); err != nil || !ok {
		t.Fatalf("expected chain to allow before revocation, got %v, %v", ok, err)
	}

	if _, err := e.RevokeDelegation(root.ID); err != nil {
		t.Fatal(err)
	}

	if ok, err := e.EnforceDelegation("dave", "/docs/a", "read"); err != nil || ok {
		t.Fatalf("expected revoked ancestor to deny descendants, got %v, %v", ok, err)
	}

	if _, err := e.GrantDelegation(DelegationRequest{
		ParentID:  grandchild.ID,
		Delegator: "dave",
		Delegatee: "erin",
		Object:    "/docs/a",
		Action:    "read",
	}); !errors.Is(err, ErrDelegationParentInvalid) {
		t.Fatalf("expected revoked chain to reject extension, got %v", err)
	}
}

func TestDelegationRechecksRootDirectAuthority(t *testing.T) {
	e := newDelegationTestEnforcer(t)

	root, err := e.GrantDelegation(DelegationRequest{
		Delegator:   "owner",
		Delegatee:   "bob",
		Object:      "/data/*",
		Action:      "read",
		CanDelegate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.GrantDelegation(DelegationRequest{
		ParentID:  root.ID,
		Delegator: "bob",
		Delegatee: "carol",
		Object:    "/data/x",
		Action:    "read",
	})
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := e.EnforceDelegation("carol", "/data/x", "read"); err != nil || !ok {
		t.Fatalf("expected valid root authority to allow, got %v, %v", ok, err)
	}

	if _, err := e.RemovePolicy("owner", "/data/*", "read|write"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RemovePolicy("owner", "/data/*", "read"); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.EnforceDelegation("carol", "/data/x", "read"); err != nil || ok {
		t.Fatalf("expected removed root authority to deny descendant, got %v, %v", ok, err)
	}

	if _, err := e.AddPolicy("owner", "/data/*", "read"); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.EnforceDelegation("carol", "/data/x", "read"); err != nil || !ok {
		t.Fatalf("expected restored root authority to allow existing chain, got %v, %v", ok, err)
	}
}

func TestDelegationRejectsBrokenChainAndCycle(t *testing.T) {
	e := newDelegationTestEnforcer(t)

	if _, err := e.GrantDelegation(DelegationRequest{
		ParentID:  "missing",
		Delegator: "bob",
		Delegatee: "carol",
		Object:    "/docs/a",
		Action:    "read",
	}); !errors.Is(err, ErrDelegationBrokenChain) {
		t.Fatalf("expected missing parent to fail, got %v", err)
	}

	root, err := e.GrantDelegation(DelegationRequest{
		Delegator:   "alice",
		Delegatee:   "bob",
		Object:      "/docs/*",
		Action:      "read",
		CanDelegate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := e.GrantDelegation(DelegationRequest{
		ParentID:    root.ID,
		Delegator:   "bob",
		Delegatee:   "carol",
		Object:      "/docs/a",
		Action:      "read",
		CanDelegate: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.GrantDelegation(DelegationRequest{
		ParentID:  child.ID,
		Delegator: "alice",
		Delegatee: "bob",
		Object:    "/docs/a",
		Action:    "read",
	}); !errors.Is(err, ErrDelegationBrokenChain) {
		t.Fatalf("expected delegator mismatch to break chain, got %v", err)
	}

	_, err = e.GrantDelegation(DelegationRequest{
		ParentID:  child.ID,
		Delegator: "carol",
		Delegatee: "alice",
		Object:    "/docs/a",
		Action:    "read",
	})
	if !errors.Is(err, ErrDelegationCycle) {
		t.Fatalf("expected repeated subject in chain to be rejected, got %v", err)
	}

	e.SetDelegationMatchingFunc("", util.KeyMatch)
}

func TestDelegationNarrowsDomainsAndUsesModelMatchers(t *testing.T) {
	domainMatcherModel := `
[request_definition]
r = sub, dom, obj, act
[policy_definition]
p = sub, dom, obj, act
[role_definition]
g = _, _, _
[policy_effect]
e = some(where (p.eft == allow))
[matchers]
m = g(r.sub, p.sub, r.dom) && keyMatch(r.dom, p.dom) && keyMatch(r.obj, p.obj) && r.act == p.act
`
	m, err := model.NewModelFromString(domainMatcherModel)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEnforcer(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddPolicy("alice", "domain*", "/docs/*", "read"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddGroupingPolicy("alice", "alice", "domain1"); err != nil {
		t.Fatal(err)
	}

	root, err := e.GrantDelegation(DelegationRequest{
		Delegator:   "alice",
		Delegatee:   "bob",
		Domain:      "domain*",
		Object:      "/docs/*",
		Action:      "read",
		CanDelegate: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.GrantDelegation(DelegationRequest{
		ParentID:  root.ID,
		Delegator: "bob",
		Delegatee: "carol",
		Domain:    "other",
		Object:    "/docs/a",
		Action:    "read",
	})
	if !errors.Is(err, ErrDelegationPrivilegeUp) {
		t.Fatalf("expected domain widening to be rejected, got %v", err)
	}

	expiry := time.Now().Add(time.Minute).Truncate(time.Second)
	_, err = e.GrantDelegation(DelegationRequest{
		ParentID:  root.ID,
		Delegator: "bob",
		Delegatee: "carol",
		Domain:    "domain1",
		Object:    "/docs/a",
		Action:    "read",
		ExpiresAt: expiry,
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := e.EnforceDelegationInDomain("domain1", "carol", "/docs/a", "read", expiry)
	if err != nil || !ok {
		t.Fatalf("expected request at exact expiry to allow, got %v, %v", ok, err)
	}
	ok, err = e.EnforceDelegationInDomain("domain1", "carol", "/docs/a", "read", expiry.Add(time.Second))
	if err != nil || ok {
		t.Fatalf("expected request after expiry to deny, got %v, %v", ok, err)
	}
	ok, err = e.EnforceDelegationInDomain("other", "carol", "/docs/a", "read")
	if err != nil || ok {
		t.Fatalf("expected narrowed domain to deny another request, got %v, %v", ok, err)
	}
}
