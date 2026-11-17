(contextvars)=

# Context Variables

```{testsetup}
import structlog
```

```{testcleanup}
import structlog
structlog.reset_defaults()
```

The {mod}`contextvars` module in the Python standard library allows having a global *structlog* context that is local to the current execution context.
The execution context can be thread-local if using threads, stored in the {mod}`asyncio` event loop, or [*greenlet*](https://greenlet.readthedocs.io/) respectively.

For example, you may want to bind certain values like a request ID or the peer's IP address at the beginning of a web request and have them logged out along with the local contexts you build within our views.

For that *structlog* provides the {mod}`structlog.contextvars` module with a set of functions to bind variables to a context-local context.
This context is safe to be used both in threaded as well as asynchronous code.

:::{warning}
Since the storage mechanics of your context variables is different for each concurrency method, they are _isolated_ from each other.

This can be a problem in hybrid applications like those based on [Starlette](https://www.starlette.io) (this [includes FastAPI](https://github.com/fastapi/fastapi/discussions/5999)) where context variables set in a synchronous context don't appear in logs from an async context and vice versa.
:::

The general flow is:

- Use {func}`structlog.configure` with {func}`structlog.contextvars.merge_contextvars` as your first processor (part of default configuration).
- Call {func}`structlog.contextvars.clear_contextvars` at the beginning of your request handler (or whenever you want to reset the context-local context).
- Call {func}`structlog.contextvars.bind_contextvars` and {func}`structlog.contextvars.unbind_contextvars` instead of your bound logger's `bind()` and `unbind()` when you want to bind and unbind key-value pairs to the context-local context.
  You can also use the {func}`structlog.contextvars.bound_contextvars` context manager / decorator.
- Use *structlog* as normal.
  Loggers act as they always do, but the {func}`structlog.contextvars.merge_contextvars` processor ensures that any context-local binds get included in all of your log messages.
- If you want to access the context-local storage, you use {func}`structlog.contextvars.get_contextvars` and {func}`structlog.contextvars.get_merged_contextvars`.

We're sorry the word *context* means three different things in this itemization depending on ... context.

```{doctest}
>>> from structlog.contextvars import (
...     bind_contextvars,
...     bound_contextvars,
...     clear_contextvars,
...     merge_contextvars,
...     unbind_contextvars,
... )
>>> from structlog import configure
>>> configure(
...     processors=[
...         merge_contextvars,
...         structlog.processors.KeyValueRenderer(key_order=["event", "a"]),
...     ]
... )
>>> log = structlog.get_logger()
>>> # At the top of your request handler (or, ideally, some general
>>> # middleware), clear the contextvars-local context and bind some common
>>> # values:
>>> clear_contextvars()
>>> bind_contextvars(a=1, b=2)
{'a': <Token var=<ContextVar name='structlog_a' default=Ellipsis at ...> at ...>, 'b': <Token var=<ContextVar name='structlog_b' default=Ellipsis at ...> at ...>}
>>> # Then use loggers as per normal
>>> # (perhaps by using structlog.get_logger() to create them).
>>> log.info("hello")
event='hello' a=1 b=2
>>> # Use unbind_contextvars to remove a variable from the context.
>>> unbind_contextvars("b")
>>> log.info("world")
event='world' a=1
>>> # You can also bind key-value pairs temporarily.
>>> with bound_contextvars(b=2):
...    log.info("hi")
event='hi' a=1 b=2
>>> # Now it's gone again.
>>> log.info("hi")
event='hi' a=1
>>> # And when we clear the contextvars state again, it goes away.
>>> # a=None is printed due to the key_order argument passed to
>>> # KeyValueRenderer, but it is NOT present anymore.
>>> clear_contextvars()
>>> log.info("hi there")
event='hi there' a=None
```


## Support for `contextvars.Token`

If, for example, your request handler calls a helper function that needs to temporarily override some contextvars before restoring them back to their original values, you can use the {class}`~contextvars.Token`s returned by {func}`~structlog.contextvars.bind_contextvars` along with {func}`~structlog.contextvars.reset_contextvars` to accomplish this (much like how {meth}`contextvars.ContextVar.reset` works):

```python
def foo():
    bind_contextvars(a=1)
    _helper()
    log.info("a is restored!")  # a=1


def _helper():
    tokens = bind_contextvars(a=2)
    log.info("a is overridden")  # a=2
    reset_contextvars(**tokens)
```

(importing-trace-context)=

## Importing Trace Context

Inbound requests usually carry [W3C Trace Context](https://www.w3.org/TR/trace-context/) in the `traceparent` and `tracestate` headers.
To correlate your logs with your traces, *structlog* can validate those headers and bind the result to the context-local context.

The parsing is separate from the binding, so you can inspect -- or re-validate -- a header without touching any context.
It takes either the raw header values or a mapping of headers:

```{doctest}
>>> from structlog.contextvars import parse_trace_context
>>> ctx = parse_trace_context(
...     "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
...     "congo=t61rcWkgMzE",
... )
>>> ctx.status
<TraceContextStatus.SAMPLED: 'sampled'>
>>> ctx.trace_id
'4bf92f3577b34da6a3ce929d0e0e4736'
>>> ctx.span_id
'00f067aa0ba902b7'
>>> ctx.tracestate
(('congo', 't61rcWkgMzE'),)
>>> ctx.as_event_dict()
{'trace_context_status': 'sampled', 'trace_id': '4bf92f3577b34da6a3ce929d0e0e4736', 'span_id': '00f067aa0ba902b7', 'trace_flags': 1, 'trace_sampled': True, 'tracestate': 'congo=t61rcWkgMzE'}
>>> parse_trace_context({"TraceParent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}).trace_id
'4bf92f3577b34da6a3ce929d0e0e4736'
```

{func}`~structlog.contextvars.parse_trace_context` never raises for bad input.
Instead, it reports the outcome in `TraceContext.status` -- one of `missing`, `invalid`, `not_sampled`, or `sampled` -- and in `TraceContext.error`.
**Invalid values are never echoed back as trusted IDs**:

```{doctest}
>>> from structlog.contextvars import parse_trace_context
>>> broken = parse_trace_context("00-00000000000000000000000000000000-00f067aa0ba902b7-01")
>>> broken.status
<TraceContextStatus.INVALID: 'invalid'>
>>> broken.error
'zero_trace_id'
>>> broken.trace_id is None
True
>>> parse_trace_context(None).status
<TraceContextStatus.MISSING: 'missing'>
>>> parse_trace_context("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00").status
<TraceContextStatus.NOT_SAMPLED: 'not_sampled'>
```

To put the correlation fields into the context-local context, use {func}`~structlog.contextvars.bind_trace_context` -- typically in your trace middleware.
It accepts the header values, an already parsed {class}`~structlog.contextvars.TraceContext`, or -- most conveniently -- the request's header mapping, which is looked up case-insensitively:

```python
from structlog.contextvars import (
    bind_contextvars,
    bind_trace_context,
    clear_contextvars,
)


def trace_middleware(request):
    clear_contextvars()
    bind_contextvars(path=request.path)
    bind_trace_context(request.headers)

    return handle(request)
```

The fields are ordinary context-local values -- `trace_id`, `span_id`, `trace_flags`, `trace_sampled`, `tracestate`, `trace_context_status`, and `trace_context_error` -- merged by {func}`~structlog.contextvars.merge_contextvars` like any other.
Nothing special is enforced: {func}`~structlog.contextvars.bind_contextvars` keeps binding arbitrary business fields, and *structlog* never validates or rewrites values that you bind yourself.

For scoped entry points, {func}`~structlog.contextvars.bound_trace_context` restores the previous trace context -- including the absence of one -- on exit, and is isolated between {mod}`asyncio` tasks like any other context-local data:

```python
from structlog.contextvars import bound_trace_context


def handle_request(headers):
    with bound_trace_context(headers):
        # Logs in here use this request's trace context...
        run_handlers()


# ... and here, the previous -- probably empty -- context is active again.
```

:::{note}
Traceparent versions other than `00` are accepted as long as they follow the same field layout, but `ff` is forbidden by the specification.
A malformed `tracestate` only drops the trace state; a valid `traceparent` is still used.
:::

(flask-example)=

## Example: Flask and thread-local data

Let's assume you want to bind a unique request ID, the URL path, and the peer's IP to every log entry by storing it in thread-local storage that is managed by context variables:

```python
import logging
import sys
import uuid

import flask

from .some_module import some_function

import structlog

logger = structlog.get_logger()
app = flask.Flask(__name__)


@app.route("/login", methods=["POST", "GET"])
def some_route():
    # You would put this into some kind of middleware or processor so it's set
    # automatically for all requests in all views.
    structlog.contextvars.clear_contextvars()
    structlog.contextvars.bind_contextvars(
        view=flask.request.path,
        request_id=str(uuid.uuid4()),
        peer=flask.request.access_route[0],
    )
    # End of belongs-to-middleware.

    log = logger.bind()
    # do something
    # ...
    log.info("user logged in", user="test-user")
    # ...
    some_function()
    # ...
    return "logged in!"


if __name__ == "__main__":
    logging.basicConfig(
        format="%(message)s", stream=sys.stdout, level=logging.INFO
    )
    structlog.configure(
        processors=[
            structlog.contextvars.merge_contextvars,  # <--!!!
            structlog.processors.KeyValueRenderer(
                key_order=["event", "view", "peer"]
            ),
        ],
        logger_factory=structlog.stdlib.LoggerFactory(),
    )
    app.run()
```

`some_module.py`:

```python
from structlog import get_logger

logger = get_logger()


def some_function():
    # ...
    logger.error("user did something", something="shot_in_foot")
    # ...
```

This would result among other the following lines to be printed:

```text
event='user logged in' view='/login' peer='127.0.0.1' user='test-user' request_id='e08ddf0d-23a5-47ce-b20e-73ab8877d736'
event='user did something' view='/login' peer='127.0.0.1' something='shot_in_foot' request_id='e08ddf0d-23a5-47ce-b20e-73ab8877d736'
```

As you can see, `view`, `peer`, and `request_id` are present in **both** log entries.
