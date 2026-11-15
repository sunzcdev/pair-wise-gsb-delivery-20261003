HTTPX is careful to enforce timeouts everywhere by default.

The default behavior is to raise a `TimeoutException` after 5 seconds of
network inactivity.

## Setting and disabling timeouts

You can set timeouts for an individual request:

```python
# Using the top-level API:
httpx.get('http://example.com/api/v1/example', timeout=10.0)

# Using a client instance:
with httpx.Client() as client:
    client.get("http://example.com/api/v1/example", timeout=10.0)
```

Or disable timeouts for an individual request:

```python
# Using the top-level API:
httpx.get('http://example.com/api/v1/example', timeout=None)

# Using a client instance:
with httpx.Client() as client:
    client.get("http://example.com/api/v1/example", timeout=None)
```

## Setting a default timeout on a client

You can set a timeout on a client instance, which results in the given
`timeout` being used as the default for requests made with this client:

```python
client = httpx.Client()              # Use a default 5s timeout everywhere.
client = httpx.Client(timeout=10.0)  # Use a default 10s timeout everywhere.
client = httpx.Client(timeout=None)  # Disable all timeouts by default.
```

## Fine tuning the configuration

HTTPX also allows you to specify the timeout behavior in more fine grained detail.

There are four different types of timeouts that may occur. These are **connect**,
**read**, **write**, and **pool** timeouts.

* The **connect** timeout specifies the maximum amount of time to wait until
a socket connection to the requested host is established. If HTTPX is unable to connect
within this time frame, a `ConnectTimeout` exception is raised.
* The **read** timeout specifies the maximum duration to wait for a chunk of
data to be received (for example, a chunk of the response body). If HTTPX is
unable to receive data within this time frame, a `ReadTimeout` exception is raised.
* The **write** timeout specifies the maximum duration to wait for a chunk of
data to be sent (for example, a chunk of the request body). If HTTPX is unable
to send data within this time frame, a `WriteTimeout` exception is raised.
* The **pool** timeout specifies the maximum duration to wait for acquiring
a connection from the connection pool. If HTTPX is unable to acquire a connection
within this time frame, a `PoolTimeout` exception is raised. A related
configuration here is the maximum number of allowable connections in the
connection pool, which is configured by the `limits` argument.

You can configure the timeout behavior for any of these values...

```python
# A client with a 60s timeout for connecting, and a 10s timeout elsewhere.
timeout = httpx.Timeout(10.0, connect=60.0)
client = httpx.Client(timeout=timeout)

response = client.get('http://example.com/')
```

## Setting an overall deadline

The `connect`, `read`, `write` and `pool` timeouts each constrain a single
operation. They are measured independently on every request, so the wait is
renewed for each hop when following redirects or answering authentication
challenges.

In addition to those per-stage timeouts you can set an overall `total`
deadline. It starts ticking when the first request is sent and keeps running
until the response body has been fully read. The same absolute deadline is
shared by every request in the exchange, so a redirect or an authentication
challenge never resets the remaining budget.

```python
# 5 seconds for each individual stage, but 10 seconds for the whole exchange.
timeout = httpx.Timeout(5.0, total=10.0)
client = httpx.Client(timeout=timeout)
```

When both are configured the remaining overall budget is used to cap each
per-stage timeout, so whichever limit is reached first fires. If the overall
deadline expires the current response is closed and an `httpx.TotalTimeout`
exception is raised. The exception exposes both the original request
(`exc.request`) and the request that was actually in flight when the deadline
was reached (`exc.last_request`); the two differ across redirect/auth chains.

Because the deadline is an absolute point in time it still applies when no
single operation is slow: a chain of quick redirects can individually stay
within every stage timeout while the cumulative exchange outlives the total
deadline.

The deadline is enforced at the client level. A custom clock can be supplied
to the client through the `clock` argument (any object exposing a `now()`
method returning monotonic seconds) to make deadline behaviour deterministic
in tests.
