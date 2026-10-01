// Package delegation adds temporary, chained delegation of authority on top of
// a casbin enforcer.
//
// A subject on duty (the delegator) can hand part of its permissions to a
// colleague (the delegatee). The delegatee may hand the permission on again.
// Every level obeys three rules:
//
//   - Each grant may only narrow its parent: the resource, action and validity
//     window of a child grant must all be contained in the parent grant.
//   - Whether a permission may be passed on at all is decided by the previous
//     level through the grant's CanSubGrant flag.
//   - A decision checks the entire chain. If any ancestor is revoked, expired,
//     missing, out of scope, or if the root delegator's own direct policy no
//     longer covers the request, the delegation cannot take effect.
//
// Scopes reuse the casbin request-matching model: a stored scope value is
// matched against a concrete value in the same direction as a normal enforce
// request. Containment of two arbitrary patterns is therefore not required;
// matchers such as util.KeyMatch2 or util.RegexMatch can be plugged in.
//
// The delegation interface never writes to the enforcer's policy. Direct
// enforcement results are therefore unchanged by any delegation operation.
package delegation
