// Package federation implements a standalone resolver for OpenID Federation
// 1.0 (https://openid.net/specs/openid-federation-1_0.html).
//
// The current scope is deliberately narrow: resolve the openid_provider
// metadata of a leaf entity, starting from one or more pre-configured trust
// anchors. The leaf's own URL is never assumed to be a trust anchor.
//
// For every entity statement on a candidate trust chain the resolver
// verifies, independently:
//
//   - the JWS signature against the issuer's published keys (the leaf or
//     intermediate's entity configuration for self-signed statements, the
//     superior's entity configuration for subordinate statements);
//   - the subject relationship (iss == sub for an entity configuration;
//     iss/sub link the two neighboring entity configurations for a
//     subordinate statement);
//   - the validity window (iat, nbf and exp) against the resolver's clock.
//
// Metadata policies found on the chain are composed trust-anchor-to-leaf
// using only the operator set declared by [SupportedPolicyOperators]. Any
// other operator encountered on the openid_provider metadata policy is a
// hard error: the chain is never treated as valid by silently ignoring an
// unrecognized operator.
//
// All network access goes through the [Client] interface. [HTTPClient]
// implements it over net/http; [LocalClient] is an in-memory client used by
// the local examples and tests to deterministically demonstrate chains,
// conflicting policies, broken links, cycles and multiple paths.
package federation
