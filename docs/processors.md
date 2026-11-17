# Processors

The true power of *structlog* lies in its *combinable log processors*.
A log processor is a regular callable or in other words:
A function or an instance of a class with a `__call__()` method.

(chains)=

## Chains

The *processor chain* is a list of processors.
Each processors receives three positional arguments:

**logger**

: Your wrapped logger object that is called with the final processor return value.
  For example {class}`logging.Logger` or {class}`structlog.PrintLogger` (default).

**method_name**

: The name of the wrapped method.
  If you called `log.warning("foo")`, it will be `"warning"`.

**event_dict**

: Current context together with the current event.
  If the context was `{"a": 42}` and the event is `"foo"`, the initial `event_dict` will be `{"a":42, "event": "foo"}`.

The return value of each processor is passed on to the next one as `event_dict` until finally the return value of the last processor gets passed into the wrapped logging method.

:::{note}
*structlog* only looks at the return value of the **last** processor.
That means that as long as you control the next processor in the chain (the processor that will get your return value passed as an argument), you can return whatever you want.

Returning a modified event dictionary from your processors is just a convention to make processors composable.
:::


### Examples

If you set up your logger like:

```python
structlog.configure(processors=[f1, f2, f3])
log = structlog.get_logger().bind(x=42)
```

and call `log.info("some_event", y=23)`, it results in the following call chain:

```python
wrapped_logger.info(
    f3(
        wrapped_logger,
        "info",
        f2(
            wrapped_logger,
            "info",
            f1(
                wrapped_logger,
                "info",
                {"event": "some_event", "x": 42, "y": 23},
            ),
        ),
    )
)
```

In this case, `f3` has to make sure it returns something `wrapped_logger.info` can handle (see {ref}`adapting`).
For the example with `PrintLogger` above, this means `f3` must return a string.

The simplest modification a processor can make is adding new values to the `event_dict`.
Parsing human-readable timestamps is tedious, not so [UNIX timestamps](https://en.wikipedia.org/wiki/UNIX_time) -- let's add one to each log entry:

```python
import calendar
import time


def timestamper(logger, log_method, event_dict):
    event_dict["timestamp"] = calendar.timegm(time.gmtime())
    return event_dict
```

:::{important}
You're explicitly allowed to modify the `event_dict` parameter, because a copy has been created before calling the first processor.
:::

Please note that *structlog* comes with such a processor built in: {class}`~structlog.processors.TimeStamper`.


## Filtering

If a processor raises {class}`structlog.DropEvent`, the event is silently dropped.

Therefore, the following processor drops every entry:

```python
from structlog import DropEvent


def dropper(logger, method_name, event_dict):
    raise DropEvent
```

But we can do better than that!

(cond-drop)=

How about dropping only log entries that are marked as coming from a certain peer (for example, monitoring)?

```python
class ConditionalDropper:
    def __init__(self, peer_to_ignore):
        self._peer_to_ignore = peer_to_ignore

    def __call__(self, logger, method_name, event_dict):
        """
        >>> cd = ConditionalDropper("127.0.0.1")
        >>> cd(None, "", {"event": "foo", "peer": "10.0.0.1"})
        {'peer': '10.0.0.1', 'event': 'foo'}
        >>> cd(None, "", {"event": "foo", "peer": "127.0.0.1"})
        Traceback (most recent call last):
        ...
        DropEvent
        """
        if event_dict.get("peer") == self._peer_to_ignore:
            raise DropEvent

        return event_dict
```

Since it's so common to filter by the log level, *structlog* comes with {func}`structlog.make_filtering_bound_logger` that filters log entries before they even enter the processor chain.
It does **not** use the standard library, but it does use its names and order of log levels.

(duplicate-suppression)=

### Suppressing duplicate events

Transient failures -- a flaky network call, a full disk -- often produce the
same warning many times in quick succession.
{class}`structlog.processors.DuplicateEventSuppressor` keeps a bounded cache of
events and only lets a limited number of identical events through per time
window:

```python
import structlog

from structlog.processors import DuplicateEventSuppressor

suppressor = DuplicateEventSuppressor(window=60.0)

structlog.configure(processors=[structlog.processors.add_log_level, suppressor])
```

Two events are considered identical if the values of the processor's
*signature_keys* (just `event` by default) are equal.
The first *max_passed* events of every signature are passed through within
each *window* seconds; further duplicates raise {class}`structlog.DropEvent`.

An event with a **higher** log level is never swallowed by an older window of
lower-severity events.
Instead, the old window is settled first, and the escalated event opens a
fresh one.

When a window expires, when the bounded cache evicts its least recently used
entry, or when you call `DuplicateEventSuppressor.flush()` -- for example at
process shutdown -- every signature that actually had events suppressed yields
a {class}`structlog.processors.SuppressionSummary` containing the number of
suppressed events and the clock values of the first and last occurrence.
`flush()` returns all pending summaries and clears the processor state:

```python
for summary in suppressor.flush():
    print(
        summary.signature,
        "suppressed",
        summary.suppressed_count,
        "events between",
        summary.first_seen,
        "and",
        summary.last_seen,
    )
```

For deterministic tests, the clock can be injected through the *clock*
parameter instead of the default {func}`time.monotonic`.
If you want to be notified immediately when a window settles, pass an
*on_settle* callback.
Calls made from within that callback are guarded, so logging from it cannot
re-enter the same suppressor.

(adapting)=

## Adapting and rendering

An important role is played by the *last* processor because its duty is to adapt the `event_dict` into something the logging methods of the *wrapped logger* understand.
With that, it's also the *only* processor that needs to know anything about the underlying system.

It can return one of three types:

- An Unicode string ({any}`str`), a bytes string ({any}`bytes`), or a {any}`bytearray` that is passed as the first (and only) positional argument to the underlying logger.
- A tuple of `(args, kwargs)` that are passed as `log_method(*args, **kwargs)`.
- A dictionary which is passed as `log_method(**kwargs)`.

Therefore `return "hello world"` is a shortcut for `return (("hello world",), {})` (the example in {ref}`chains` assumes this shortcut has been taken).

This should give you enough power to use *structlog* with any logging system while writing agnostic processors that operate on dictionaries.

:::{versionchanged} 14.0.0 Allow final processor to return a {any}`dict`.
:::

:::{versionchanged} 20.2.0 Allow final processor to return a {any}`bytes`.
:::

:::{versionchanged} 21.2.0 Allow final processor to return a {any}`bytearray`.
:::

### Examples

The probably most useful formatter for string based loggers is {class}`structlog.processors.JSONRenderer`.
Advanced log aggregation and analysis tools like [Logstash](https://www.elastic.co/logstash) offer features like telling them "this is JSON, deal with it" instead of fiddling with regular expressions.

For a list of shipped processors, check out the {ref}`API documentation <procs>`.


## Third-Party packages

*structlog* was specifically designed to be as composable and reusable as possible, so whatever you're missing:
chances are, you can solve it with a processor!
Since processors are self-contained callables, it's easy to write your own and to share them in separate packages.

We collect those packages in our [GitHub Wiki](https://github.com/hynek/structlog/wiki/Third-Party-Extensions) and encourage you to add your package too!
