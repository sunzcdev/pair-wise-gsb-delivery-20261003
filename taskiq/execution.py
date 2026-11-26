"""In-process execution capacity shared by nested tasks.

Some brokers execute tasks inside the current event loop and may bound how many
of them run at the same time. A task that already holds such an execution
opportunity can explicitly lend it to a subtask sent to the same broker. The
subtask then runs within the caller's opportunity instead of waiting for a new
one.

The helpers below only describe capacity. A send never borrows implicitly: the
sender has to ask for it, for example with ``AsyncKicker.with_borrowed_slot``
or ``AsyncTaskiqDecoratedTask.kiq_borrowed``.
"""

from contextvars import ContextVar
from typing import TYPE_CHECKING

if TYPE_CHECKING:  # pragma: no cover
    from taskiq.abc.broker import AsyncBroker


class ExecutionSlot:
    """
    One execution opportunity held by a running in-process task.

    The broker creates a slot for every task it starts and publishes it through
    :data:`current_execution_slot` while the task body runs. An active slot can
    be lent to nested tasks that explicitly ask to borrow it, which is how a
    bounded broker avoids a parent/child deadlock on a single slot.
    """

    __slots__ = ("_broker", "_released")

    def __init__(self, broker: "AsyncBroker") -> None:
        self._broker = broker
        self._released = False

    @property
    def broker(self) -> "AsyncBroker":
        """Broker that granted this slot."""
        return self._broker

    @property
    def is_released(self) -> bool:
        """Whether the owning task has already finished."""
        return self._released

    def mark_released(self) -> None:
        """Prevent the slot from being borrowed after the owner finished."""
        self._released = True

    def can_be_borrowed_by(self, broker: "AsyncBroker") -> bool:
        """
        Check whether a task of the given broker may borrow this slot.

        :param broker: broker that wants to use the slot.
        :return: True if the slot is still active and belongs to the broker.
        """
        return not self._released and self._broker is broker


current_execution_slot: "ContextVar[ExecutionSlot | None]" = ContextVar(
    "taskiq_current_execution_slot",
    default=None,
)
"""Slot held by the task that is currently executing, if any."""


current_borrow_request: "ContextVar[bool]" = ContextVar(
    "taskiq_borrow_execution_slot",
    default=False,
)
"""Whether the send in progress explicitly asks to borrow the current slot.

The flag is set by the kicker around a single ``broker.kick`` call and is
consumed by in-process brokers. It is deliberately not an ambient mode:
ordinary background sends reset it, so inheriting a task's context never lets
them skip execution capacity.
"""
