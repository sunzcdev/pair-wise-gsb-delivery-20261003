You can control the connection pool size using the `limits` keyword
argument on the client. It takes instances of `httpx.Limits` which define:

- `max_keepalive_connections`, number of allowable keep-alive connections, or `None` to always
allow. (Defaults 20)
- `max_connections`, maximum number of allowable connections, or `None` for no limits.
(Default 100)
- `keepalive_expiry`, time limit on idle keep-alive connections in seconds, or `None` for no limits. (Default 5)
- `max_connections_per_origin`, maximum number of in-flight requests to any single origin,
or `None` for no limit. (Default `None`)

```python
limits = httpx.Limits(max_keepalive_connections=5, max_connections=10)
client = httpx.Client(limits=limits)
```

## Per-origin concurrency limits

`max_connections` limits the client as a whole, but a single busy site can still
consume the entire connection pool. When a client is used to collect from many
sites at once, you may want to cap the number of in-flight requests separately
for each origin:

```python
limits = httpx.Limits(max_connections=100, max_connections_per_origin=4)
client = httpx.Client(limits=limits)
```

An origin is the `(scheme, host, port)` triple of the request URL. A permit is
acquired just before the request is about to be sent, and it is held until the
response body has been fully consumed, or the response has been closed. This
means `client.get(...)` holds a permit for its whole request/response cycle,
while a `client.stream(...)` response holds its permit until the caller closes
it or exhausts the body.

The limit behaves as follows:

* Requests to the same origin are admitted in arrival order, while requests to
  other origins are never held up by a busy site.
* The permit is returned if a queued request is cancelled, if the transport
  fails, or if a request or response event hook raises an exception.
* When following a cross-origin redirect, the previous response is always
  consumed before the new request is admitted, so a single request never holds
  permits for two origins at the same time.
* Closing the client wakes up any request still waiting for a permit, and causes
  it to raise `httpx.ClientClosed`. Pending requests are not admitted after the
  client is closed.

The per-origin limit is opt-in, and defaults to `None`, meaning that only the
client-wide connection limit applies.