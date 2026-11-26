import enum
import inspect
from collections.abc import Awaitable, Callable
from typing import Any

from pydantic import BaseModel

from taskiq.utils import maybe_awaitable


@enum.unique
class AcknowledgeType(str, enum.Enum):
    """Enum with possible acknowledge times."""

    # The message is acknowledged right when it's received,
    # before it's executed.
    WHEN_RECEIVED = "when_received"
    # This option means that the message will be
    # acknowledged right after it's executed.
    WHEN_EXECUTED = "when_executed"
    # This option means that the message will be
    # acknowledged when the task will be saved
    # only after it's saved in the result backend.
    WHEN_SAVED = "when_saved"
    # Strict version of WHEN_SAVED.
    #
    # The message is acknowledged only when the result backend
    # confirms the write. If saving the result fails (for example
    # the backend times out) the message is not acknowledged, so the
    # broker can redeliver it, and the receipt records the write as
    # UNKNOWN. Failures in the post_save middlewares happen after the
    # write is confirmed and don't block the acknowledgement.
    #
    # Tasks that explicitly skip saving their result (NoResultError)
    # follow the separate no-result agreement and are acknowledged
    # once execution completes.
    WHEN_SAVED_STRICT = "when_saved_strict"
    # This option means that the task is responsible
    # for acknowledging the message through Context.ack.
    MANUAL = "manual"


@enum.unique
class ResultSaveStatus(str, enum.Enum):
    """Outcome of persisting a task result in the result backend."""

    # The task declared that no result should be stored.
    SKIPPED = "skipped"
    # The result backend confirmed that the result was written.
    CONFIRMED = "confirmed"
    # The write did not finish successfully (for example a timeout),
    # so it is unknown whether the backend persisted the result.
    UNKNOWN = "unknown"


@enum.unique
class AckStage(str, enum.Enum):
    """Processing stages at which a message can be acknowledged."""

    # The message could not be parsed or has no registered task.
    UNPARSABLE = "unparsable"
    UNKNOWN_TASK = "unknown_task"
    # Acknowledged immediately after the message was received.
    WHEN_RECEIVED = "when_received"
    # Acknowledged right after the task finished executing.
    WHEN_EXECUTED = "when_executed"
    # Acknowledged once the result backend confirmed the write.
    WHEN_SAVED = "when_saved"
    # Acknowledged because the task explicitly skipped storing a result.
    NO_RESULT = "no_result"
    # Acknowledged manually through the task Context.
    MANUAL = "manual"


def parse_acknowledge_type(value: Any) -> AcknowledgeType:
    """Parse acknowledge type from a task label value."""
    if isinstance(value, AcknowledgeType):
        return value
    if isinstance(value, str):
        try:
            return AcknowledgeType(value.lower())
        except ValueError as exc:
            raise ValueError(f"Unknown acknowledge type value: {value}.") from exc
    raise ValueError(
        f"Unsupported acknowledge type: {value} use str or AcknowledgeType",
    )


class AckableMessage(BaseModel):
    """
    Message that can be acknowledged.

    If your broker support message acknowledgement,
    please return this type of message, so we'll be
    able to mark this message as acknowledged after
    the function will be executed.

    It adds more reliability to brokers and system
    as a whole.

    The ``ack`` callable is invoked with the processing stage at which
    taskiq decided to acknowledge the delivery (an :class:`AckStage`
    value). Callables that do not accept an argument are still supported,
    so existing broker integrations keep working unchanged.
    """

    data: bytes
    ack: Callable[..., Awaitable[None] | None]


class AckController:
    """Controls acknowledgement state for a received message."""

    def __init__(self, ack: Callable[..., Awaitable[None] | None] | None) -> None:
        self._ack = ack
        self.is_acked = False
        # The processing stage at which the message was actually
        # acknowledged. None until the first successful acknowledgement.
        self.ack_stage: str | None = None

    @property
    def is_ackable(self) -> bool:
        """Whether the current message supports acknowledgement."""
        return self._ack is not None

    async def ack(self, stage: "AckStage | str | None" = None) -> None:
        """
        Acknowledge the current message once.

        The broker-provided ack callable receives the stage that triggered
        the acknowledgement, when it accepts an argument. Legacy zero-argument
        ack callables are called without arguments.

        :param stage: processing stage that triggered the acknowledgement.
            It is recorded on the first acknowledgement and kept untouched
            by any further idempotent calls.
        """
        if self._ack is None:
            raise RuntimeError("Current message is not ackable.")
        if self.is_acked:
            return
        stage_value = stage.value if isinstance(stage, AckStage) else stage
        if self._accepts_stage(self._ack):
            await maybe_awaitable(self._ack(stage_value))
        else:
            await maybe_awaitable(self._ack())
        self.is_acked = True
        if self.ack_stage is None:
            self.ack_stage = stage_value

    @staticmethod
    def _accepts_stage(ack: Callable[..., Any]) -> bool:
        """Whether the ack callable accepts the acknowledgement stage."""
        try:
            signature = inspect.signature(ack)
        except (TypeError, ValueError):
            return False
        return any(
            parameter.kind
            in (
                inspect.Parameter.POSITIONAL_ONLY,
                inspect.Parameter.POSITIONAL_OR_KEYWORD,
                inspect.Parameter.VAR_POSITIONAL,
            )
            for parameter in signature.parameters.values()
        )
