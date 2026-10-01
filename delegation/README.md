# Delegation

Temporary, chained delegation of authority for casbin.

A user on duty can hand part of their permissions to a colleague, who may in
turn hand them on. Every level can only narrow the permission, the previous
level decides whether passing it on is allowed, and an authorization decision
verifies the whole chain.

## Concepts

- `Scope`: the delegated permission, expressed as a `Resource`, an `Action` and
  a validity window (`NotBefore`, `ExpiresAt`). Zero time means the bound is
  open. `ExpiresAt` is exclusive.
- `Grant`: one delegation edge from `Delegator` to `Delegatee`. A grant with an
  empty `ParentID` is a root delegation backed by the delegator's direct
  authority; other grants reference their parent.
- `CanSubGrant`: whether the delegatee may pass this grant on. This is decided
  by the grant's delegator.

## Rules enforced

- A root grant is accepted only when the delegator's direct enforcer allows the
  scope.
- A sub-grant is accepted only from the parent's delegatee, when the parent
  allows sub-delegation, every ancestor is active, and the new scope is
  contained in the parent scope (resource, action and time window).
- Cycles and references to missing grants are rejected.
- At decision time every grant on the chain must be unrevoked, within its
  validity window and cover the concrete request, and the root delegator's
  direct authority is re-checked. Revoking or letting any ancestor expire
  disables every grant below it.
- The delegation API never writes casbin policy, so direct `Enforce` results
  are unchanged.

Resource and action containment reuse request matching: a concrete value is
matched against the stored scope value. Plug in the same matcher the model
uses, for example `util.KeyMatch2` or `util.RegexMatch`. Arbitrary
  pattern-to-pattern set inclusion is not required.

## Example

```go
e, _ := casbin.NewEnforcer(model)
_, _ = e.AddPolicy("alice", "/docs/1", "read")

mgr, _ := delegation.NewManager(e, delegation.NewMemoryAdapter(),
    delegation.WithResourceMatcher(util.KeyMatch2))

// Alice delegates read access to Bob until tomorrow and allows passing it on.
root, _ := mgr.Grant("alice", "bob",
    delegation.Scope{Resource: "/docs/1", Action: "read", ExpiresAt: tomorrow},
    true)

// Bob narrows the window further and hands it to Carol.
_, _ = mgr.GrantDelegation("bob", "carol", root,
    delegation.Scope{Resource: "/docs/1", Action: "read", ExpiresAt: tonight},
    true)

ok, _ := mgr.Enforce("carol", "/docs/1", "read") // true

// Revoking an ancestor immediately disables everything below it.
_ = mgr.Revoke(root)
ok, _ = mgr.Enforce("carol", "/docs/1", "read") // false
```

Implement the `Adapter` interface to persist grants in the same store as the
rest of the policy.
