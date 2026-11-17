# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

"""
Primitives to deal with a concurrency supporting context, as introduced in
Python 3.7 as :mod:`contextvars`.

.. versionadded:: 20.1.0
.. versionchanged:: 21.1.0
   Reimplemented without using a single dict as context carrier for improved
   isolation. Every key-value pair is a separate `contextvars.ContextVar` now.
.. versionchanged:: 23.3.0
   Callsite parameters are now also collected under asyncio.
.. versionadded:: 26.2.0
   Added validation and import of inbound `W3C Trace Context
   <https://www.w3.org/TR/trace-context/>`_ headers: `TraceContext`,
   `TraceContextStatus`, `parse_trace_context`, `bind_trace_context`, and
   `bound_trace_context`.

See :doc:`contextvars`.
"""

from __future__ import annotations

import contextlib
import contextvars
import enum

from collections.abc import Generator, Mapping
from dataclasses import dataclass
from types import FrameType
from typing import Any, Final

import structlog

from .typing import BindableLogger, EventDict, WrappedLogger


STRUCTLOG_KEY_PREFIX: Final = "structlog_"
STRUCTLOG_KEY_PREFIX_LEN: Final = len(STRUCTLOG_KEY_PREFIX)

_ASYNC_CALLING_STACK: contextvars.ContextVar[FrameType] = (
    contextvars.ContextVar("_ASYNC_CALLING_STACK")
)

# Stores thread info captured at async call time.
# Value is a tuple of (thread_id: int, thread_name: str)
_ASYNC_CALLING_THREAD: contextvars.ContextVar[tuple[int, str]] = (
    contextvars.ContextVar("_ASYNC_CALLING_THREAD")
)

# For proper isolation, we have to use a dict of ContextVars instead of a
# single ContextVar with a dict.
# See https://github.com/hynek/structlog/pull/302 for details.
_CONTEXT_VARS: dict[str, contextvars.ContextVar[Any]] = {}


def get_contextvars() -> dict[str, Any]:
    """
    Return a copy of the *structlog*-specific context-local context.

    .. versionadded:: 21.2.0
    """
    rv = {}
    ctx = contextvars.copy_context()

    for k in ctx:
        if k.name.startswith(STRUCTLOG_KEY_PREFIX) and ctx[k] is not Ellipsis:
            rv[k.name[STRUCTLOG_KEY_PREFIX_LEN:]] = ctx[k]

    return rv


def get_merged_contextvars(bound_logger: BindableLogger) -> dict[str, Any]:
    """
    Return a copy of the current context-local context merged with the context
    from *bound_logger*.

    .. versionadded:: 21.2.0
    """
    ctx = get_contextvars()
    ctx.update(structlog.get_context(bound_logger))

    return ctx


def merge_contextvars(
    logger: WrappedLogger, method_name: str, event_dict: EventDict
) -> EventDict:
    """
    A processor that merges in a global (context-local) context.

    Use this as your first processor in :func:`structlog.configure` to ensure
    context-local context is included in all log calls.

    .. versionadded:: 20.1.0
    .. versionchanged:: 21.1.0 See toplevel note.
    """
    ctx = contextvars.copy_context()

    for k in ctx:
        if k.name.startswith(STRUCTLOG_KEY_PREFIX) and ctx[k] is not Ellipsis:
            event_dict.setdefault(k.name[STRUCTLOG_KEY_PREFIX_LEN:], ctx[k])

    return event_dict


def clear_contextvars() -> None:
    """
    Clear the context-local context.

    The typical use-case for this function is to invoke it early in request-
    handling code.

    .. versionadded:: 20.1.0
    .. versionchanged:: 21.1.0 See toplevel note.
    """
    ctx = contextvars.copy_context()
    for k in ctx:
        if k.name.startswith(STRUCTLOG_KEY_PREFIX):
            k.set(Ellipsis)


def bind_contextvars(**kw: Any) -> Mapping[str, contextvars.Token[Any]]:
    r"""
    Put keys and values into the context-local context.

    Use this instead of :func:`~structlog.BoundLogger.bind` when you want some
    context to be global (context-local).

    Return the mapping of `contextvars.Token`\s resulting
    from setting the backing :class:`~contextvars.ContextVar`\s.
    Suitable for passing to :func:`reset_contextvars`.

    .. versionadded:: 20.1.0
    .. versionchanged:: 21.1.0 Return the `contextvars.Token` mapping
        rather than None. See also the toplevel note.
    """
    rv = {}
    for k, v in kw.items():
        structlog_k = f"{STRUCTLOG_KEY_PREFIX}{k}"
        try:
            var = _CONTEXT_VARS[structlog_k]
        except KeyError:
            var = contextvars.ContextVar(structlog_k, default=Ellipsis)
            _CONTEXT_VARS[structlog_k] = var

        rv[k] = var.set(v)

    return rv


def reset_contextvars(**kw: contextvars.Token[Any]) -> None:
    r"""
    Reset contextvars corresponding to the given Tokens.

    .. versionadded:: 21.1.0
    """
    for k, v in kw.items():
        structlog_k = f"{STRUCTLOG_KEY_PREFIX}{k}"
        var = _CONTEXT_VARS[structlog_k]
        var.reset(v)


def unbind_contextvars(*keys: str) -> None:
    """
    Remove *keys* from the context-local context if they are present.

    Use this instead of :func:`~structlog.BoundLogger.unbind` when you want to
    remove keys from a global (context-local) context.

    .. versionadded:: 20.1.0
    .. versionchanged:: 21.1.0 See toplevel note.
    """
    for k in keys:
        structlog_k = f"{STRUCTLOG_KEY_PREFIX}{k}"
        if structlog_k in _CONTEXT_VARS:
            _CONTEXT_VARS[structlog_k].set(Ellipsis)


@contextlib.contextmanager
def bound_contextvars(**kw: Any) -> Generator[None, None, None]:
    """
    Bind *kw* to the current context-local context. Unbind or restore *kw*
    afterwards. Do **not** affect other keys.

    Can be used as a context manager or decorator.

    .. versionadded:: 21.4.0
    """
    context = get_contextvars()
    saved = {k: context[k] for k in context.keys() & kw.keys()}

    bind_contextvars(**kw)
    try:
        yield
    finally:
        unbind_contextvars(*kw.keys())
        bind_contextvars(**saved)


class TraceContextStatus(enum.Enum):
    """
    Outcome of parsing inbound `W3C Trace Context
    <https://www.w3.org/TR/trace-context/>`_ headers.

    Every outcome -- including the two failure modes -- is observable in the
    ``trace_context_status`` log field, so that a missing header can be told
    apart from a malformed one and from a request that was explicitly not
    sampled.

    .. versionadded:: 26.2.0
    """

    #: No (or a blank) ``traceparent`` header was supplied.
    MISSING = "missing"
    #: A ``traceparent`` header was supplied but failed validation.
    INVALID = "invalid"
    #: The trace context is valid, but its sampled flag is not set.
    NOT_SAMPLED = "not_sampled"
    #: The trace context is valid and its sampled flag is set.
    SAMPLED = "sampled"


@dataclass(frozen=True)
class TraceContext:
    """
    A validated inbound `W3C Trace Context
    <https://www.w3.org/TR/trace-context/>`_.

    Instances are created by `parse_trace_context` and bound to the
    context-local context by `bind_trace_context` / `bound_trace_context`.

    Invalid input never ends up in `trace_id` or `span_id`: if `status` is
    `TraceContextStatus.INVALID`, both are `None` and `error` explains why.
    The same is true for `TraceContextStatus.MISSING`.

    Args:
        status: The outcome of the validation.

        trace_id:
            The validated 32 characters lowercase hex trace ID -- or `None`
            if there is no trustworthy one.

        span_id:
            The validated 16 characters lowercase hex ID of the caller's span
            (the ``parent-id`` field of the header) -- or `None` if there is
            no trustworthy one.

        trace_flags:
            The parsed trace flags as an integer.

        tracestate:
            The validated trace state as key-value pairs in their original
            order. Empty if there is no (valid) trace state.

        error:
            A short machine-readable reason why validation failed or why the
            trace state had to be dropped. `None` if there was no problem.

    .. versionadded:: 26.2.0
    """

    status: TraceContextStatus
    trace_id: str | None = None
    span_id: str | None = None
    trace_flags: int = 0
    tracestate: tuple[tuple[str, str], ...] = ()
    error: str | None = None

    @property
    def sampled(self) -> bool:
        """
        Whether the caller asked to sample this trace.

        `False` both for unsampled *and* invalid / missing trace contexts --
        use `status` to tell those cases apart.
        """
        return self.status is TraceContextStatus.SAMPLED

    def as_event_dict(self) -> dict[str, Any]:
        """
        Return the log correlation fields for this trace context.

        Useful if you want to add them to an event dictionary yourself --
        for example with `bind_contextvars` -- instead of using
        `bind_trace_context`.

        Returns:
            A dictionary that contains at least ``trace_context_status``.
            The IDs and the trace state are only included if they passed
            validation; ``trace_context_error`` is only included if there was
            a problem.
        """
        rv: dict[str, Any] = {"trace_context_status": self.status.value}

        if self.status in (
            TraceContextStatus.SAMPLED,
            TraceContextStatus.NOT_SAMPLED,
        ):
            rv["trace_id"] = self.trace_id
            rv["span_id"] = self.span_id
            rv["trace_flags"] = self.trace_flags
            rv["trace_sampled"] = self.sampled
            if self.tracestate:
                rv["tracestate"] = _format_tracestate(self.tracestate)

        if self.error is not None:
            rv["trace_context_error"] = self.error

        return rv


TRACE_CONTEXT_KEYS: Final = (
    "trace_id",
    "span_id",
    "trace_flags",
    "trace_sampled",
    "tracestate",
    "trace_context_status",
    "trace_context_error",
)
"""
The names -- before an optional prefix -- of the context-local fields that
are managed by `bind_trace_context`, `bound_trace_context`, and
`unbind_trace_context`.

.. versionadded:: 26.2.0
"""


_HEX_DIGITS: Final = frozenset("0123456789abcdef")
_LOWER_ALPHA: Final = frozenset("abcdefghijklmnopqrstuvwxyz")
_TRACESTATE_NAME_START: Final = _LOWER_ALPHA | _HEX_DIGITS
_TRACESTATE_KEY_CHARS: Final = frozenset(
    "abcdefghijklmnopqrstuvwxyz0123456789-*/_"
)
_TRACESTATE_VALUE_CHARS: Final = frozenset(
    chr(c) for c in range(0x20, 0x7F) if chr(c) not in ",="
)
_MAX_TRACESTATE_LENGTH: Final = 512
_MAX_TRACESTATE_MEMBERS: Final = 32
_MAX_TRACESTATE_TENANT_LENGTH: Final = 241
_MAX_TRACESTATE_SYSTEM_LENGTH: Final = 14
_MAX_TRACESTATE_VALUE_LENGTH: Final = 256
_ZERO_TRACE_ID: Final = "0" * 32
_ZERO_SPAN_ID: Final = "0" * 16
_INVALID_TRACESTATE: Final = "invalid_tracestate"


def _is_lower_hex(value: str, length: int) -> bool:
    return len(value) == length and all(c in _HEX_DIGITS for c in value)


def _is_tracestate_name(value: str, *, tenant: bool) -> bool:
    """
    Check a simple key, or -- if *tenant* -- a multi-tenant ``tenant-id``.

    Both are 1-255 characters of ``lcalpha``, digits, and ``-*/_``, but only a
    ``tenant-id`` may start with a digit.
    """
    start = _TRACESTATE_NAME_START if tenant else _LOWER_ALPHA

    return (
        bool(value)
        and value[0] in start
        and all(c in _TRACESTATE_KEY_CHARS for c in value[1:])
    )


def _is_valid_tracestate_key(key: str) -> bool:
    tenant, has_system, system = key.partition("@")

    if len(tenant) > _MAX_TRACESTATE_TENANT_LENGTH or not _is_tracestate_name(
        tenant, tenant=has_system
    ):
        return False

    return not has_system or (
        0 < len(system) <= _MAX_TRACESTATE_SYSTEM_LENGTH
        and _is_tracestate_name(system, tenant=False)
    )


def _is_valid_tracestate_value(value: str) -> bool:
    return (
        bool(value)
        and len(value) <= _MAX_TRACESTATE_VALUE_LENGTH
        and value[0] != " "
        and value[-1] != " "
        and all(c in _TRACESTATE_VALUE_CHARS for c in value)
    )


def _split_tracestate(value: str) -> tuple[tuple[str, str], ...] | None:
    """
    Split and validate tracestate members, or return `None` if they're broken.
    """
    if len(value) > _MAX_TRACESTATE_LENGTH:
        return None

    seen: set[str] = set()
    rv: list[tuple[str, str]] = []

    for member in value.split(","):
        key, sep, val = member.strip(" \t").partition("=")

        if (
            sep != "="
            or "=" in val
            or not _is_valid_tracestate_key(key)
            or not _is_valid_tracestate_value(val)
            or key in seen
        ):
            return None

        seen.add(key)
        rv.append((key, val))

    if len(rv) > _MAX_TRACESTATE_MEMBERS:
        return None

    return tuple(rv)


def _parse_tracestate(
    tracestate: str | None,
) -> tuple[tuple[tuple[str, str], ...], str | None]:
    """
    Return the parsed tracestate and an error if it had to be dropped.
    """
    if tracestate is not None and not isinstance(tracestate, str):
        return (), _INVALID_TRACESTATE

    value = "" if tracestate is None else tracestate.strip(" \t")
    if not value:
        return (), None

    members = _split_tracestate(value)
    if members is None:
        return (), _INVALID_TRACESTATE

    return members, None


def _format_tracestate(tracestate: tuple[tuple[str, str], ...]) -> str:
    return ",".join(f"{key}={value}" for key, value in tracestate)


def _invalid(error: str) -> TraceContext:
    return TraceContext(status=TraceContextStatus.INVALID, error=error)


def _traceparent_error(parts: list[str]) -> str | None:
    """
    Return the reason why the fields of a traceparent are invalid, or `None`.
    """
    version, trace_id, span_id, trace_flags = parts[:4]

    checks = (
        (not _is_lower_hex(version, 2) or version == "ff", "invalid_version"),
        (version == "00" and len(parts) != 4, "trailing_data"),
        (not _is_lower_hex(trace_id, 32), "invalid_trace_id"),
        (trace_id == _ZERO_TRACE_ID, "zero_trace_id"),
        (not _is_lower_hex(span_id, 16), "invalid_span_id"),
        (span_id == _ZERO_SPAN_ID, "zero_span_id"),
        (not _is_lower_hex(trace_flags, 2), "invalid_trace_flags"),
    )

    return next((error for failed, error in checks if failed), None)


def parse_trace_context(
    traceparent: str | Mapping[str, Any] | None = None,
    tracestate: str | None = None,
) -> TraceContext:
    """
    Validate inbound `W3C Trace Context
    <https://www.w3.org/TR/trace-context/>`_ headers and wrap them into a
    `TraceContext`.

    This function never raises for bad input: it reports what it found through
    `TraceContext.status` and `TraceContext.error` instead. In particular, an
    invalid header never leaks into `TraceContext.trace_id` -- values that
    don't pass validation are **not** echoed as trusted IDs.

    Validated are the version (two lowercase hex characters, ``ff`` is
    forbidden), the trace and span IDs (lowercase hex of the correct length,
    all-zero IDs are forbidden), the trace flags, and -- if given -- the
    trace state members. An invalid trace state only drops the trace state; a
    valid traceparent is still used.

    Args:
        traceparent:
            The value of the ``traceparent`` header, or `None` if there is
            none. Empty and whitespace-only values count as missing.

            Alternatively, a mapping of headers -- for example a request's
            headers -- which is searched case-insensitively for both
            ``traceparent`` and ``tracestate``. *tracestate* must then be
            `None`.

        tracestate:
            The value of the ``tracestate`` header, or `None`.

    Returns:
        A `TraceContext` that describes the outcome.

    Raises:
        ValueError: If *traceparent* is a mapping and *tracestate* is not
            `None`.

    .. versionadded:: 26.2.0
    """
    if isinstance(traceparent, Mapping):
        if tracestate is not None:
            msg = "tracestate must not be passed together with a mapping"
            raise ValueError(msg)

        traceparent, tracestate = _trace_headers(traceparent)

    if traceparent is None:
        return TraceContext(status=TraceContextStatus.MISSING)

    if not isinstance(traceparent, str):
        return _invalid("invalid_traceparent")

    value = traceparent.strip(" \t")
    if not value:
        return TraceContext(status=TraceContextStatus.MISSING)

    parts = value.split("-")
    if len(parts) < 4 or any(not part for part in parts):
        return _invalid("malformed")

    error = _traceparent_error(parts)
    if error is not None:
        return _invalid(error)

    _, trace_id, span_id, trace_flags = parts[:4]

    flags = int(trace_flags, 16)
    status = (
        TraceContextStatus.SAMPLED
        if flags & 0x01
        else TraceContextStatus.NOT_SAMPLED
    )
    parsed_tracestate, tracestate_error = _parse_tracestate(tracestate)

    return TraceContext(
        status=status,
        trace_id=trace_id,
        span_id=span_id,
        trace_flags=flags,
        tracestate=parsed_tracestate,
        error=tracestate_error,
    )


_TRACE_HEADER_NAMES: Final = ("traceparent", "tracestate")


def _header_values(value: Any) -> list[Any]:
    """
    Normalize a header value that may be repeated into a list.
    """
    if isinstance(value, str) or value is None:
        return [value]

    if isinstance(value, (list, tuple)):
        return list(value)

    return [value]


def _trace_headers(headers: Mapping[str, Any]) -> tuple[Any, Any]:
    """
    Look up the trace headers in *headers* case-insensitively.

    Per the specification, a repeated ``tracestate`` is joined into a single
    list, while a repeated -- or empty -- ``traceparent`` makes the request
    untraceable.
    """
    found: dict[str, list[Any]] = {}

    for name, value in headers.items():
        key = str(name).lower()
        if key in _TRACE_HEADER_NAMES:
            found.setdefault(key, []).extend(_header_values(value))

    traceparent: Any = None
    traceparents = [v for v in found.get("traceparent", []) if v is not None]
    if len(traceparents) == 1:
        traceparent = traceparents[0]
    elif len(traceparents) > 1:
        # A repeated traceparent is forbidden: don't trust any of them.
        traceparent = ",".join(str(v) for v in traceparents)

    tracestates = [v for v in found.get("tracestate", []) if v is not None]
    if not tracestates:
        return traceparent, None

    if not all(isinstance(v, str) for v in tracestates):
        return traceparent, tracestates[0]

    return traceparent, ",".join(tracestates)


def _resolve_trace_context(
    trace_context: TraceContext | str | Mapping[str, Any] | None,
    tracestate: str | None,
) -> TraceContext:
    """
    Turn the arguments of the public API into a `TraceContext`.
    """
    if isinstance(trace_context, TraceContext):
        if tracestate is not None:
            msg = "tracestate must not be passed together with a TraceContext"
            raise ValueError(msg)

        return trace_context

    return parse_trace_context(trace_context, tracestate)


def bind_trace_context(
    trace_context: TraceContext | str | Mapping[str, Any] | None = None,
    tracestate: str | None = None,
    *,
    key_prefix: str = "",
) -> Mapping[str, contextvars.Token[Any]]:
    r"""
    Parse inbound trace headers -- or take an already parsed `TraceContext` --
    and bind its log correlation fields to the context-local context.

    Fields from an earlier trace context are removed, so a request without a
    ``traceparent`` header can never inherit the IDs of the previous one.
    The returned `contextvars.Token`\s restore **all** trace fields -- even
    the ones this call removed -- when passed to `reset_contextvars`, which
    makes the function safe for nested entry points.

    The fields do **not** use a special type or format: they are ordinary
    context-local values that are merged by `merge_contextvars` like any
    other. Business fields bound with `bind_contextvars` are unaffected, and
    bindings of this function can be bound next to them.

    Args:
        trace_context:
            Either a `TraceContext` (for example from
            `parse_trace_context`), the value of the ``traceparent`` header,
            a mapping of headers -- looked up case-insensitively -- or `None`
            if there is no header.

        tracestate:
            The value of the ``tracestate`` header. Must be `None` if
            *trace_context* is a `TraceContext` or a mapping.

        key_prefix:
            An optional prefix for the context-local keys, for example to
            avoid clashes with business fields.

    Returns:
        A mapping of field names to `contextvars.Token`\s for use with
        `reset_contextvars` -- or `bound_trace_context` for a context
        manager.

    Raises:
        ValueError: If *trace_context* is a `TraceContext` or a mapping and
            *tracestate* is not `None`.

    .. versionadded:: 26.2.0
    """
    ctx = _resolve_trace_context(trace_context, tracestate)

    values = ctx.as_event_dict()

    return bind_contextvars(
        **{
            key_prefix + key: values.get(key, Ellipsis)
            for key in TRACE_CONTEXT_KEYS
        }
    )


def unbind_trace_context(*, key_prefix: str = "") -> None:
    """
    Remove all trace context fields from the context-local context.

    Other context-local values -- including business fields bound with
    `bind_contextvars` -- are left alone.

    Args:
        key_prefix: The prefix that was used for binding, if any.

    .. versionadded:: 26.2.0
    """
    unbind_contextvars(*(key_prefix + key for key in TRACE_CONTEXT_KEYS))


@contextlib.contextmanager
def bound_trace_context(
    trace_context: TraceContext | str | Mapping[str, Any] | None = None,
    tracestate: str | None = None,
    *,
    key_prefix: str = "",
) -> Generator[TraceContext, None, None]:
    """
    Bind the log correlation fields of *trace_context* for the duration of the
    context manager / decorator and restore the previous trace context
    afterwards.

    This also restores fields that were present before -- and removes fields
    that were not -- so nested entry points don't leak into each other.

    Can be used as a context manager or decorator.

    The arguments are the same as for `bind_trace_context`.

    Yields:
        The parsed `TraceContext`.

    Raises:
        ValueError: If *trace_context* is a `TraceContext` or a mapping and
            *tracestate* is not `None`.

    .. versionadded:: 26.2.0
    """
    ctx = _resolve_trace_context(trace_context, tracestate)

    tokens = bind_trace_context(ctx, key_prefix=key_prefix)
    try:
        yield ctx
    finally:
        reset_contextvars(**tokens)
