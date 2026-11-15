You can control the connection pool size using the `limits` keyword
argument on the client. It takes instances of `httpx.Limits` which define:

- `max_keepalive_connections`, number of allowable keep-alive connections, or `None` to always
allow. (Defaults 20)
- `max_connections`, maximum number of allowable connections, or `None` for no limits.
(Default 100)
- `keepalive_expiry`, time limit on idle keep-alive connections in seconds, or `None` for no limits. (Default 5)
- `max_connections_per_origin`, maximum number of in-flight requests to any
single origin, or `None` for no per-origin limit. (Default `None`)

```python
limits = httpx.Limits(
    max_keepalive_connections=5,
    max_connections=10,
    max_connections_per_origin=2,
)
client = httpx.Client(limits=limits)
```

The per-origin limit is also available directly on `Client` and
`AsyncClient`. A permit is acquired immediately before a request is sent, and
is held until the response body has been read to completion or closed. This
means streaming responses continue to occupy their origin's permit while the
caller consumes them.

Waiting requests are admitted in the order they were queued for each origin.
Queues for different origins are independent, so a busy origin does not block
requests to other sites. Cancelling a queued request, a transport failure, or
an event hook raising all release the permit. When following a redirect to a
different origin, the previous response is closed before the next request
acquires its permit, so a single request never holds permits for two origins.
Closing the client wakes queued requests and makes them fail with a
`RuntimeError` rather than letting them proceed against a closed client.

```python
client = httpx.Client(max_connections_per_origin=2)
```
