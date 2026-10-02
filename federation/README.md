# federation — OpenID Federation 1.0 metadata resolver

`federation` is a standalone resolver for the leaf metadata part of
[OpenID Federation 1.0](https://openid.net/specs/openid-federation-1_0.html).
Given a leaf entity identifier and a pre-configured set of trust anchors, it
builds a verifiable trust chain, composes the metadata policy published along
that chain and returns the leaf's trusted `openid_provider` metadata.

The package is intentionally scoped to **metadata resolution**: trust marks,
trust-mark delegation, explicit trust, federation listing and resolving
metadata types other than `openid_provider` are out of scope.

## What is verified on every statement

For each entity statement on a candidate chain, independently:

1. **Signature** — an entity configuration is verified against the keys in its
   own `jwks` member; a subordinate statement is verified against the
   superior entity configuration's `jwks`. The JWS `typ` header must be
   `entity-statement+jwt`; `alg=none`, symmetric algorithms and unrecognized
   `crit` headers are rejected. Only asymmetric algorithms
   (`RS*`/`PS*`/`ES*`/`EdDSA`) are accepted.
2. **Subject relationship** — `iss == sub` for an entity configuration; for a
   subordinate statement `iss` must be the superior reached through
   `authority_hints` and `sub` the neighboring entity. A mismatch breaks the
   edge.
3. **Validity** — `iat`, `nbf` and `exp` are checked against the resolver's
   clock. `exp` is mandatory; statements at or past `exp` are rejected.

The discovery URL is **never assumed to be a trust anchor**. A chain only
succeeds by traversing at least one issued subordinate statement and ending
at an explicitly configured trust anchor. Cycles are rejected per path, and
when multiple paths exist the resolver enumerates all valid chains and uses
the shortest one (deterministic tie-breaking).

## Supported metadata policy operators

The supported operator set is fixed and exported as
`SupportedPolicyOperators`:

| Operator | Purpose |
| --- | --- |
| `value` | Replace the parameter with a fixed value |
| `default` | Provide a value when the leaf published none |
| `add` | Append elements to a string array |
| `merge` | Merge a JSON object |
| `subset_of` | Restrict a string array to an allowed set |
| `superset_of` | Require elements to be present |
| `one_of` | Restrict a value to a set of candidates |

Operators are composed trust-anchor-to-leaf using the spec's rules of
combination (e.g. a downstream `value` must still satisfy an earlier
`subset_of`; intersecting `subset_of`/`superset_of` collapses to `one_of`).

**Any other operator — including draft-era operators such as `less_than` and
`greater_than` — is a hard error.** It is never ignored and never lets the
chain validate as if the operator were absent. Conflicting operators (for
example two incompatible `value` rules, or a leaf value outside
`subset_of`) are likewise hard errors rather than silent wins.

## Pluggable network access

All fetching goes through the `Client` interface:

```go
type Client interface {
    FetchEntityConfiguration(ctx context.Context, entityID string) (string, error)
    FetchEntityStatement(ctx context.Context, issuer, subject string) (string, error)
}
```

`HTTPClient` implements the interface over `net/http`, fetching
`{entityID}/.well-known/openid-federation` and the issuer's
`federation_fetch_endpoint?sub=...`. `LocalClient` is an in-memory
implementation used for deterministic tests and for the local example.

## Local example

The example builds signed graphs entirely in memory and runs five scenarios:

1. a three-tier chain (trust anchor → intermediate → OP) with merged policy
   and provenance;
2. a conflicting policy that must be rejected;
3. an unsupported critical operator (`less_than`) that must be rejected;
4. a broken chain (missing subordinate statement);
5. a graph containing a cycle and multiple paths, showing shortest-path
   selection and the provenance of the final metadata.

```sh
go run ./federation/example_local
```

The output shows the chosen chain, each statement's policy contribution, the
final trusted metadata and the origin of every parameter (the leaf itself, or
the entity and operator that fixed it).
