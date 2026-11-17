# Copyright Amethyst Reese
# Licensed under the MIT license

"""
Diagnostics support for correlating operations across threads.

The worker thread that executes queries cannot see the ``contextvars`` of the
task that queued an operation.  To still allow correlating diagnostic events
(such as ``sqlite3`` trace callbacks) with the logical operation that caused
them, aiosqlite supports attaching an optional *label mapping* to operations.

Labels are **frozen at enqueue time**: a snapshot of the current
:data:`operation_labels` context variable (merged with any explicit per-call
labels) is taken in the submitting task, before the operation is queued.
Later mutations of the context -- or of the mapping that was passed in -- do
not leak into operations that were already queued.

Labels are metadata only: they are never interpolated into SQL text, and
parameter values are not included in diagnostic events unless explicitly
requested with ``diagnostics_include_parameters=True``.
"""

import contextvars
from dataclasses import dataclass, field
from types import MappingProxyType
from typing import Any, Callable, Mapping, Optional

__all__ = [
    "DiagnosticEvent",
    "DiagnosticsListener",
    "operation_labels",
    "freeze_labels",
]

#: Context variable holding the default labels applied to operations queued
#: by the current task.  Set it with ``operation_labels.set({...})`` and
#: restore it with the returned token.  The mapping is copied (frozen) when
#: each operation is enqueued, so mutating it afterwards is safe and does
#: not affect already-queued operations.
operation_labels: contextvars.ContextVar[Mapping[str, str]] = contextvars.ContextVar(
    "aiosqlite_operation_labels",
    default=MappingProxyType({}),
)

_EMPTY_LABELS: Mapping[str, str] = MappingProxyType({})


def freeze_labels(extra: Optional[Mapping[str, str]] = None) -> Mapping[str, str]:
    """
    Snapshot the current operation labels, merged with ``extra``.

    Returns an immutable mapping.  Changes to the context variable or to
    ``extra`` after this call do not affect the returned mapping.
    """
    if not extra:
        current = operation_labels.get()
        # Already an immutable snapshot from a previous freeze; reuse it.
        if isinstance(current, MappingProxyType):
            return current
        return MappingProxyType(dict(current))

    merged = dict(operation_labels.get())
    merged.update(extra)
    return MappingProxyType(merged)


@dataclass(frozen=True)
class DiagnosticEvent:
    """
    A single diagnostic observation emitted by a connection.

    ``kind`` is one of:

    - ``"queued"``: an operation was enqueued (emitted on the submitting task)
    - ``"started"``: the worker thread began executing the operation
    - ``"trace"``: SQLite reported a statement via the trace callback;
      ``sql`` contains the statement text actually executed
    - ``"succeeded"``: the operation completed normally
    - ``"failed"``: the operation raised; ``error`` holds the exception
    - ``"callback_error"``: a user-supplied trace callback raised; recorded
      separately from query failures and never interrupts the work queue

    ``labels`` is the immutable label mapping frozen when the operation was
    enqueued.  ``parameters`` is ``None`` unless the connection was created
    with ``diagnostics_include_parameters=True``.
    """

    kind: str
    connection_id: int
    operation_id: int
    operation: str
    labels: Mapping[str, str] = field(default=_EMPTY_LABELS)
    sql: Optional[str] = None
    parameters: Optional[Any] = None
    error: Optional[BaseException] = None


#: A callable receiving :class:`DiagnosticEvent` objects.  It may be invoked
#: from the event loop thread (``queued``) or the connection worker thread
#: (all other kinds).  Exceptions raised by the listener are logged and
#: swallowed so that observability can never break the work queue.
DiagnosticsListener = Callable[[DiagnosticEvent], Any]
