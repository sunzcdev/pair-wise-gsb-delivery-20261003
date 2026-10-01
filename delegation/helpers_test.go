package delegation

import (
	"testing"
	"time"

	casbin "github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
)

// fakeEnforcer wraps a real casbin enforcer so the delegation package is
// exercised end-to-end against the genuine matcher and effector.
type fakeEnforcer struct {
	e *casbin.Enforcer
}

func newFakeEnforcer(t *testing.T, m model.Model) *fakeEnforcer {
	t.Helper()
	e, err := casbin.NewEnforcer(m)
	if err != nil {
		t.Fatalf("create enforcer: %v", err)
	}
	return &fakeEnforcer{e: e}
}

func (f *fakeEnforcer) Enforce(rvals ...interface{}) (bool, error) {
	return f.e.Enforce(rvals...)
}

func (f *fakeEnforcer) allow(sub, obj, act string) {
	if _, err := f.e.AddPolicy(sub, obj, act); err != nil {
		panic(err)
	}
}

func (f *fakeEnforcer) remove(sub, obj, act string) {
	if _, err := f.e.RemovePolicy(sub, obj, act); err != nil {
		panic(err)
	}
}

func newManager(t *testing.T, auth Authorizer) *Manager {
	t.Helper()
	mgr, err := NewManager(auth, NewMemoryAdapter())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return mgr
}

func newManagerAt(t *testing.T, auth Authorizer, now time.Time) (*Manager, *time.Time) {
	t.Helper()
	clock := &now
	mgr, err := NewManager(auth, NewMemoryAdapter(), WithClock(func() time.Time { return *clock }))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return mgr, clock
}

func advance(clock *time.Time, now time.Time) { *clock = now }

func mustGrant(t *testing.T, mgr *Manager, delegator, delegatee string, scope Scope, canSubGrant bool, extra ...interface{}) string {
	t.Helper()
	id, err := mgr.Grant(delegator, delegatee, scope, canSubGrant, extra...)
	if err != nil {
		t.Fatalf("Grant(%s -> %s, %+v): %v", delegator, delegatee, scope, err)
	}
	return id
}

func mustSubGrant(t *testing.T, mgr *Manager, delegator, delegatee, parent string, scope Scope, canSubGrant bool) string {
	t.Helper()
	id, err := mgr.GrantDelegation(delegator, delegatee, parent, scope, canSubGrant)
	if err != nil {
		t.Fatalf("GrantDelegation(%s -> %s under %s, %+v): %v", delegator, delegatee, parent, scope, err)
	}
	return id
}

func wantAllowed(t *testing.T, mgr *Manager, sub, obj, act string, want bool) {
	t.Helper()
	got, err := mgr.Enforce(sub, obj, act)
	if err != nil {
		t.Fatalf("Enforce(%s, %s, %s): %v", sub, obj, act, err)
	}
	if got != want {
		t.Fatalf("Enforce(%s, %s, %s) = %v, want %v", sub, obj, act, got, want)
	}
}
