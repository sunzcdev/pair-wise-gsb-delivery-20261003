# Exceptions

While you should use a proper crash reporter like [Sentry](https://sentry.io) in production, *structlog* has helpers for formatting exceptions for humans and machines.

All *structlog*'s exception features center around passing an `exc_info` key-value pair in the event dict.
There are three possible behaviors depending on its value:

1. If the value is a tuple, render it as if it was returned by {func}`sys.exc_info`.
2. If the value is an Exception, render it.
3. If the value is true but no tuple, call {func}`sys.exc_info` and render that.

If there is no `exc_info` key or false, the event dict is not touched.
This behavior is analog to the one of the stdlib's logging.


## Transformations

*structlog* comes with {class}`structlog.processors.ExceptionRenderer` that deduces and removes the `exc_info` key as outlined above, calls a user-supplied function with the synthesized `exc_info`, and stores its return value in the `exception` key.
The most common use-cases are already covered by the following processors:

{func}`structlog.processors.format_exc_info`

: Formats it to a flat string like the standard library would on the console.

{obj}`structlog.processors.dict_tracebacks`

: Uses {class}`structlog.tracebacks.ExceptionDictTransformer` to give you a structured and JSON-serializable `exception` key.


### Customizing Dict Tracebacks

{class}`structlog.tracebacks.ExceptionDictTransformer` can be customized with a number of parameters.
If these prove insufficient, see {ref}`Custom Filtering of Dict Tracebacks <custom-dict-tracebacks>`.

#### Protecting Sensitive and Expensive Locals

Rendering local variables calls `repr()` on their values, which can both leak secrets and trigger expensive or dangerous side effects (for example a database connection whose `__repr__()` runs a query).
Therefore you should be deliberate about which locals are rendered:

- **Variable names and types**: pass *locals_hide_value* with variable names (exact match) and/or types.
  Matching variables remain visible by name, but their value is replaced with the `structlog.tracebacks.HIDDEN_LOCAL` placeholder (`"<hidden>"`) **without ever calling `repr()`** on the value (type matching only uses `isinstance()`).
- **Arbitrary predicates**: pass *locals_filter*, a callable receiving the variable name and value and returning `True` to hide it.
  If the callable itself raises, the variable is hidden as well.
- Independently of the configured *locals_max_string*, a rendered value can never exceed `structlog.tracebacks.REPR_MAX_STRING`, even when truncation is disabled.
  If rendering a value fails, the failure is marked inline as `"<repr-error ...>"` instead of aborting the whole traceback.

Hidden variables are decided before any value formatting, so the exclusion cannot be bypassed by formatting the value first and redacting the resulting string.

```python
from structlog.tracebacks import ExceptionDictTransformer

transformer = ExceptionDictTransformer(
    show_locals=True,
    locals_hide_value=("password", MyConnection),
    locals_filter=lambda name, value: name.startswith("secret_"),
)
```

If *show_locals* is `False` (or locals are suppressed for a frame), the frame's locals are never accessed at all.


## Console rendering

Our {doc}`console-output`'s {class}`structlog.dev.ConsoleRenderer` takes an *exception_formatter* argument that allows for customizing the output of exceptions.

{func}`structlog.dev.plain_traceback`

: Is the default if [Rich] is not installed.
  As the name suggests, it renders a plain traceback.

{class}`structlog.dev.RichTracebackFormatter`

: Uses [Rich] to render a colorful traceback.
  It's a class because it allows for customizing the output by passing arguments to Rich.
: It's the default if Rich is installed.

:::{seealso}
{doc}`console-output` for more information on *structlog*'s console features.
:::

:::{deprecated} 26.1.0
Support for [*better-exceptions*] is deprecated and will be removed in a future release.
Use [Rich] instead.
:::

[*better-exceptions*]: https://github.com/qix-/better-exceptions
[Rich]: https://github.com/Textualize/rich
