# FailoverProxy

A forward proxy that chains two upstream proxy lines. Every request starts on
a **preferred** line and, while the request is still safely replayable, fails
over to the **backup** line. It covers both ordinary HTTP forwarding and
`CONNECT` tunneling, and each line authenticates with its own credential.

```
client --> failover proxy --+--> primary (own credentials)
                            +--> backup  (own credentials)
```

## Configuration

See `config.json`. Each entry in `lines` has:

- `name`: stable identifier used in selection and in error messages.
- `addr`: upstream endpoint as `host:port` (plain HTTP proxy).
- `username` / `password`: HTTP Basic credentials sent **only** to this line.

Other fields:

- `listen`: local address for this proxy.
- `max_buffered_body_bytes`: bodies up to this size are fully buffered, so a
  preferred line that fails before consuming the body can be retried. Larger
  bodies are streamed and are never replayed on another line.
- `dial_timeout`: per-line TCP connect timeout.
- `auth_probe_timeout`: how long, after sending a body-bearing request with
  `Expect: 100-continue`, to wait for an upstream challenge (e.g. `407`)
  before transmitting the body.

## Selecting the preferred line

By default, lines are tried in the order listed in `lines`. A single request
can override that by sending:

```
X-Upstream-Preferred: backup,primary
```

The header is a control header for this proxy and is stripped before the
request leaves for any upstream.

## When failover happens

Failover is allowed only while the request can safely be re-sent:

- **Connection refused / reset during dial or while sending the request
  head** — no request processing has happened, so another line is tried.
- **Upstream `407 Proxy Authentication Required` before the request body**
  (via the `Expect: 100-continue` gate for body-bearing requests) — the
  upstream demonstrably did not consume the body, so the backup is tried with
  the backup's own credential.

Failover is **not** performed:

- **After any request-body byte has been committed** to a line. The origin may
  already be processing the request; it is answered with `502` and the backup
  is never contacted.
- **For a body that cannot be replayed** (a streamed body larger than
  `max_buffered_body_bytes`), even on a failed dial. It is not sent a second
  time.
- **For `CONNECT` after the upstream replied `200 Connection established`.**
  The tunnel already exists; a break afterward is surfaced to the client with
  no second tunnel attempt.
- **For a `407` that arrives only after the request body was sent.** That is
  terminal and reported as `502`.

## Failure reporting

When every attempt fails the client receives `502 Bad Gateway` whose body
lists, in order, the lines actually tried and the phase reached, for example:

```
upstream failover exhausted; tried lines:
  - line "primary": could not establish connection (dial phase)
  - line "backup": upstream proxy authentication failed (status 407) after request was sent (response phase)
```

Credentials, realms and upstream response bodies are never included.

## Credential scoping

Each line's `Proxy-Authorization` is generated per attempt for that line only.
Any `Proxy-Authorization` sent by the downstream client is stripped, so a
credential for one line can never be replayed against the other line or leak
to the destination server. Hop-by-hop proxy headers (`Proxy-Connection`,
`Proxy-Authenticate`, `Connection`, ...) are also removed at each hop.

## Run

```sh
go run . -config config.json
```

Then point an HTTP client at it as a standard forward proxy.
