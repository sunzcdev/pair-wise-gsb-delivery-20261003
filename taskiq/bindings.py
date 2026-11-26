"""Immutable broker bindings for a single message send."""

from contextvars import ContextVar
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from taskiq.abc.broker import AsyncBroker
    from taskiq.abc.formatter import TaskiqFormatter
    from taskiq.abc.middleware import TaskiqMiddleware
    from taskiq.abc.result_backend import AsyncResultBackend


@dataclass(frozen=True, slots=True)
class MessageBindings:
    """
    Complete set of broker objects selected for one send.

    Broker attributes can be replaced while an await is in progress.
    Capturing them once keeps formatting, send hooks, result handling and
    in-process delivery bound to the same configuration version.
    """

    broker: "AsyncBroker"
    formatter: "TaskiqFormatter"
    result_backend: "AsyncResultBackend[Any]"
    middlewares: "tuple[TaskiqMiddleware, ...]"

    @classmethod
    def capture(cls, broker: "AsyncBroker") -> "MessageBindings":
        """Capture the broker's currently published send configuration."""
        return cls(
            broker=broker,
            formatter=broker.formatter,
            result_backend=broker.result_backend,
            middlewares=tuple(broker.middlewares),
        )


current_bindings: "ContextVar[MessageBindings | None]" = ContextVar(
    "taskiq_message_bindings",
    default=None,
)
