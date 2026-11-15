HTTPX allows you to register "event hooks" with the client, that are called
every time a particular type of event takes place.

There are currently two event hooks:

* `request` - Called after a request is fully prepared, but before it is sent to the network. Passed the `request` instance.
* `response` - Called after the response has been fetched from the network, but before it is returned to the caller. Passed the `response` instance.
* `response_complete` - Called once when the response body reaches a terminal state. Passed the `response` instance, a `reason`, the number of bytes received as `num_bytes`, and any `exception` that occurred.

These allow you to install client-wide functionality such as logging, monitoring or tracing.

```python
def log_request(request):
    print(f"Request event hook: {request.method} {request.url} - Waiting for response")

def log_response(response):
    request = response.request
    print(f"Response event hook: {request.method} {request.url} - Status {response.status_code}")

client = httpx.Client(event_hooks={'request': [log_request], 'response': [log_response]})
```

You can also use these hooks to install response processing code, such as this
example, which creates a client instance that always raises `httpx.HTTPStatusError`
on 4xx and 5xx responses.

```python
def raise_on_4xx_5xx(response):
    response.raise_for_status()

client = httpx.Client(event_hooks={'response': [raise_on_4xx_5xx]})
```

!!! note
    Response event hooks are called before determining if the response body
    should be read or not.

    If you need access to the response body inside an event hook, you'll
    need to call `response.read()`, or for AsyncClients, `response.aread()`.

## Monitoring when the body completes

The `response` hook fires as soon as the response headers are available, which
is usually before the response body has been read. The `response_complete` hook
instead fires exactly once, when the body reaches a terminal state. This makes
it suitable for monitoring and instrumentation that needs to know when the
response is genuinely finished.

The `reason` keyword argument is one of the constants on
`httpx.ResponseTerminationReason`:

* `httpx.ResponseTerminationReason.COMPLETE` - The body was read to completion.
* `httpx.ResponseTerminationReason.CLOSED_EARLY` - The response was closed before the body was fully read.
* `httpx.ResponseTerminationReason.FAILED` - Reading or decoding the body raised an exception, provided as `exception`.

```python
def log_response_complete(response, reason, num_bytes, exception):
    print(f"{response.request.method} {response.request.url} -> {reason} ({num_bytes} bytes)")

client = httpx.Client(event_hooks={"response_complete": [log_response_complete]})
```

The hook is bound to a specific `Response`, so every response in a redirect
chain settles independently. Empty bodies and `HEAD` requests also reach the
`complete` state, with `num_bytes` set to `0`. Reading a response and then
closing it only notifies once. If a hook itself raises an exception, the
exception propagates to the caller.

The hooks are also allowed to modify `request` and `response` objects.

```python
def add_timestamp(request):
    request.headers['x-request-timestamp'] = datetime.now(tz=datetime.utc).isoformat()

client = httpx.Client(event_hooks={'request': [add_timestamp]})
```

Event hooks must always be set as a **list of callables**, and you may register
multiple event hooks for each type of event.

As well as being able to set event hooks on instantiating the client, there
is also an `.event_hooks` property, that allows you to inspect and modify
the installed hooks.

```python
client = httpx.Client()
client.event_hooks['request'] = [log_request]
client.event_hooks['response'] = [log_response, raise_on_4xx_5xx]
```

!!! note
    If you are using HTTPX's async support, then you need to be aware that
    hooks registered with `httpx.AsyncClient` MUST be async functions,
    rather than plain functions.
