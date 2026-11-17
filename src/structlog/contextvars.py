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
.. versionchanged:: 26.2.0
   Added explicit sequence contexts for recovering the commit order of
   events across threads and async subtasks: `sequence_context`,
   `SequenceContext`, `fork_sequence_context`, and the
   `add_sequence_number` processor.

See :doc:`contextvars`.
"""

from __future__ import annotations

import contextlib
import contextvars
import threading

from collections.abc import Generator, Mapping
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


# The sequence context lives in a dedicated ContextVar whose name does *not*
# carry the STRUCTLOG_KEY_PREFIX prefix, so it is neither picked up by
# merge_contextvars/get_contextvars nor cleared by clear_contextvars. Its
# lifecycle is managed exclusively through the explicit API below.


class SequenceContext:
    """
    An explicit sequence context that hands out monotonically increasing
    sequence numbers, starting at 1.

    The counter is a single mutable object shared by every context (async
    task, greenlet, or thread) that the `SequenceContext` is propagated to.
    Therefore, copying a `contextvars.Context` (for example by creating an
    `asyncio.Task`) does **not** create a second, same-named sequence that
    restarts at 1 -- all copies keep drawing numbers from the same counter.
    An independent sequence can only be created explicitly using
    `SequenceContext.fork` or `sequence_context`.

    The counter is thread-safe: threads that share one `SequenceContext`
    never draw duplicate numbers.

    Args:
        name:
            An optional name for the sequence. If present, it is added to
            event dictionaries as ``seq_id`` by `add_sequence_number`.

    .. versionadded:: 26.2.0
    """

    __slots__ = ("_forks", "_lock", "_next", "name")

    def __init__(self, name: str | None = None) -> None:
        self.name = name
        self._lock = threading.Lock()
        self._next = 1
        self._forks = 0

    def __repr__(self) -> str:
        return f"<SequenceContext(name={self.name!r})>"

    def next(self) -> int:
        """
        Draw and return the next sequence number of this sequence.
        """
        with self._lock:
            n = self._next
            self._next += 1

        return n

    def fork(self, name: str | None = None) -> SequenceContext:
        """
        Explicitly create a new, independent sequence that starts at 1.

        If *name* is not given and this sequence is named, the fork's name
        is derived as ``"{self.name}/{n}"`` where *n* is the number of this
        fork, so a fork never collides with its parent's name.

        .. versionadded:: 26.2.0
        """
        with self._lock:
            self._forks += 1
            n = self._forks

        if name is None and self.name is not None:
            name = f"{self.name}/{n}"

        return SequenceContext(name)


_SEQUENCE_CONTEXT: contextvars.ContextVar[SequenceContext | None] = (
    contextvars.ContextVar("_structlog_sequence_context", default=None)
)


def get_sequence_context() -> SequenceContext | None:
    """
    Return the currently active `SequenceContext`, or None if no sequence
    context is active.

    Use this together with `bind_sequence_context` to propagate a sequence
    into a thread, because `contextvars` are not automatically propagated
    across thread boundaries.

    .. versionadded:: 26.2.0
    """
    return _SEQUENCE_CONTEXT.get()


def bind_sequence_context(
    sequence: SequenceContext,
) -> contextvars.Token[SequenceContext | None]:
    """
    Bind an existing *sequence* to the current context and return the
    resulting token, suitable for `reset_sequence_context`.

    This is the way to *share* one sequence across threads or tasks: obtain
    the sequence in the parent using `get_sequence_context` and bind it in
    the child. All binders draw from the same counter.

    .. versionadded:: 26.2.0
    """
    return _SEQUENCE_CONTEXT.set(sequence)


def reset_sequence_context(
    token: contextvars.Token[SequenceContext | None],
) -> None:
    """
    Reset the sequence context using a *token* returned by
    `bind_sequence_context`.

    .. versionadded:: 26.2.0
    """
    _SEQUENCE_CONTEXT.reset(token)


@contextlib.contextmanager
def sequence_context(
    name: str | None = None,
) -> Generator[SequenceContext, None, None]:
    """
    Start a new, explicit sequence context and yield its `SequenceContext`.

    On exit, the previous state is restored: events committed after the
    context has been left are **not** numbered anymore (unless another
    sequence context is active), so one request's sequence never leaks
    numbers into the next request.

    Can be used as a context manager or decorator.

    .. versionadded:: 26.2.0
    """
    sequence = SequenceContext(name)
    token = _SEQUENCE_CONTEXT.set(sequence)
    try:
        yield sequence
    finally:
        _SEQUENCE_CONTEXT.reset(token)


def fork_sequence_context(name: str | None = None) -> SequenceContext:
    """
    Explicitly fork the currently active sequence: create a new, independent
    `SequenceContext` (starting at 1) using `SequenceContext.fork` and bind
    it to the current context.

    Use this in a subtask that should have its own numbering instead of
    sharing the parent's sequence. The fork's name is derived from the
    parent's name unless *name* is given.

    Raise `RuntimeError` if no sequence context is active: forking is only
    meaningful relative to an existing sequence.

    .. versionadded:: 26.2.0
    """
    parent = _SEQUENCE_CONTEXT.get()
    if parent is None:
        msg = "Cannot fork: no sequence context is active."
        raise RuntimeError(msg)

    forked = parent.fork(name)
    _SEQUENCE_CONTEXT.set(forked)

    return forked


def add_sequence_number(
    logger: WrappedLogger, method_name: str, event_dict: EventDict
) -> EventDict:
    """
    A processor that assigns the next sequence number of the active
    `SequenceContext` to the event.

    The number is added as ``seq``; if the sequence is named, its name is
    added as ``seq_id``. If no sequence context is active, the event is
    passed through unchanged.

    The number is taken at *commit time* -- when the event actually passes
    through the processor chain. From then on it is baked into the event
    dict, so a wrapped logger that writes the event out later (for example
    from another thread or a queue) can never change it.

    The fixed rule for filtered events is: **events that are dropped before
    this processor runs do not consume sequence numbers.** That includes
    events filtered out by log level, be it by
    `structlog.make_filtering_bound_logger` or by a filtering processor
    (like `structlog.stdlib.filter_by_level`) placed before this one.
    Conversely, events dropped by a processor placed *after* this one have
    already consumed their number.

    .. versionadded:: 26.2.0
    """
    sequence = _SEQUENCE_CONTEXT.get()
    if sequence is None:
        return event_dict

    event_dict["seq"] = sequence.next()
    if sequence.name is not None:
        event_dict["seq_id"] = sequence.name

    return event_dict
