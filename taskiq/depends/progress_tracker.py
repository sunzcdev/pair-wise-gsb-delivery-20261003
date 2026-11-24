import asyncio
import enum
from typing import Generic, TypeVar

from pydantic import BaseModel, ConfigDict
from taskiq_dependencies import Depends

from taskiq.context import Context

_ProgressType = TypeVar("_ProgressType")


class TaskState(str, enum.Enum):
    """State of task execution."""

    STARTED = "STARTED"
    FAILURE = "FAILURE"
    SUCCESS = "SUCCESS"
    RETRY = "RETRY"
    CANCELLED = "CANCELLED"


class _TaskProgressConfig(BaseModel):
    model_config = ConfigDict(arbitrary_types_allowed=True)


class TaskProgress(_TaskProgressConfig, Generic[_ProgressType]):
    """Progress of task execution.

    :param state: current state of the task.
    :param meta: arbitrary progress payload.
    :param attempt: 1-based number of the execution attempt.
        Progress written without this field (e.g. data produced by
        older versions) is treated as the first attempt.
    """

    state: TaskState | str
    meta: _ProgressType | None
    attempt: int = 1


def _is_current_task_cancelling() -> bool:
    """Whether the currently running task is being cancelled.

    Works on every supported Python version. On 3.11+ we can ask the
    running task directly. On older versions we fall back to the internal
    flag that asyncio sets as soon as ``cancel()`` is requested.
    """
    task = asyncio.current_task()
    if task is None:
        return False
    cancelling = getattr(task, "cancelling", None)
    if cancelling is not None:
        return cancelling() > 0
    # Python 3.10 exposes the requested-cancellation flag only internally.
    return bool(getattr(task, "_must_cancel", False))  # noqa: SLF001


class ProgressTracker(Generic[_ProgressType]):
    """Task's dependency to set progress."""

    def __init__(
        self,
        context: Context = Depends(),
    ) -> None:
        self.context = context

    @property
    def attempt(self) -> int:
        """1-based number of the current execution attempt."""
        return int(self.context.message.labels.get("_retries", 0)) + 1

    async def set_progress(
        self,
        state: TaskState | str,
        meta: _ProgressType | None = None,
    ) -> None:
        """Set progress.

        Every progress record is tagged with the current attempt number.
        Writes coming from an older attempt (for example a coroutine that
        finished after the task was retried) cannot overwrite the progress
        of a newer attempt. The same protection applies once the current
        attempt has been cancelled, so late cleanup writes cannot clobber
        the real current state.

        :param state: TaskState or str
        :param meta: progress data
        """
        if _is_current_task_cancelling():
            return

        if meta is None:
            progress = await self.get_progress()
            if progress is not None and progress.attempt == self.attempt:
                meta = progress.meta

        progress = TaskProgress(
            state=state,
            meta=meta,
            attempt=self.attempt,
        )

        await self.context.broker.result_backend.set_progress(
            self.context.message.task_id,
            progress,
        )

    async def get_progress(self) -> TaskProgress[_ProgressType] | None:
        """Get progress."""
        return await self.context.broker.result_backend.get_progress(
            self.context.message.task_id,
        )
