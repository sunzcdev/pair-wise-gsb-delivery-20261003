# DPoP proof verification

This example shows a resource server accepting access tokens bound to a DPoP
key with the
[`oidc/dpop`](https://pkg.go.dev/github.com/coreos/go-oidc/v3/oidc/dpop)
package, per [RFC 9449](https://www.rfc-editor.org/rfc/rfc9449).

It runs an in-process authorization server and resource server and walks
through five scenarios:

1. A valid request: the client mints a DPoP proof over the HTTP method,
   target URI, access-token hash, and its public key; the server accepts it.
2. Replay of the exact same proof: the shared `ReplayStore` rejects the
   already-used `jti`.
3. The same proof sent to a different path: the `htu` claim does not match.
4. The same proof presented with a different access token: the `ath` hash
   does not match.
5. A server nonce challenge: the first request to a protected area gets
   `401 use_dpop_nonce` with a `DPoP-Nonce` header, and the client retries
   with a fresh proof carrying that nonce.

Run it with:

```
go run ./example/dpop
```

## Shared replay store

`dpop.MemoryStore` is used in the example for simplicity. The
`dpop.ReplayStore` interface is the only thing the verifier depends on, so a
deployment with multiple resource-server instances backs it with a shared
system whose conditional write is atomic across instances, for example:

```go
type RedisStore struct {
	Client *redis.Client // github.com/redis/go-redis/v9
}

func (s *RedisStore) Add(ctx context.Context, jti string, ttl time.Duration) (bool, error) {
	// SET NX performs check-and-set atomically across every instance.
	ok, err := s.Client.SetNX(ctx, "dpop:jti:"+jti, "1", ttl).Result()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, dpop.ErrReplay
	}
	return true, nil
}
```

The important property is that check-and-set is a single atomic operation;
a read-then-write sequence would let two instances accept the same proof.
Key the record by the proof `jti` and give it the verifier's replay
lifetime (twice the configured time window by default).

## Trusted token binding

The verifier never reads a `cnf` claim itself. The resource server validates
the access token by its own means and passes in the binding it verified via
`dpop.TokenBinding`. For a JWT access token, extract it after signature
verification:

```go
idToken, err := idTokenVerifier.Verify(ctx, rawAccessToken)
// ...
binding, err := dpop.BindingFromIDToken(idToken)

proof, err := dpopVerifier.VerifyRequest(ctx, req, rawAccessToken, binding)
```

For an opaque token, obtain `cnf.jkt` from an introspection response over an
authenticated channel and use `dpop.BindingFromClaims`. Passing a `cnf` from
an unverified token would let an attacker choose the thumbprint their own
proof is compared against.
