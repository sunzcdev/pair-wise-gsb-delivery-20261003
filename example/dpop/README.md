# DPoP resource server example

This example runs a full [RFC 9449][rfc9449] DPoP flow in one process:

1. a client generates a DPoP key and an authorization server mints an access
   token bound to the key (`cnf.jkt`);
2. the resource server verifies the access token itself, then checks the DPoP
   proof with the `dpop` package;
3. a server-issued nonce challenge is requested and answered;
4. replaying the same proof, changing the target path (`htu`), and swapping the
   access token (`ath`) are all rejected.

Run it with:

```shell
go run ./example/dpop
```

## Trust model

The `dpop.Verifier` never parses the access token. The resource server
authenticates the access token (signature, issuer, expiry, audience) first and
hands the already-verified `cnf` confirmation to the verifier. The JWK embedded
in the proof header is used only to check that specific proof's signature; the
key that the request is allowed to use comes exclusively from the verified
access token.

## Shared replay store

`MemoryReplayStore` is correct for a single instance only. In a multi-instance
deployment every replica must use one shared backend with an atomic
check-and-set, for example Redis:

```go
type RedisReplayStore struct {
    Client *redis.Client
}

func (s *RedisReplayStore) CheckAndStore(ctx context.Context, jti string, ttl time.Duration) (bool, error) {
    // SET NX is atomic: exactly one caller wins across all replicas.
    ok, err := s.Client.SetNX(ctx, "dpop:jti:"+jti, "1", ttl).Result()
    if err != nil {
        return false, err
    }
    return !ok, nil // !ok means the key (and therefore the jti) already existed
}
```

The same pattern (short-lived keys) backs a shared `NonceProvider`.

[rfc9449]: https://www.rfc-editor.org/rfc/rfc9449
