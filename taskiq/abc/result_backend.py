from abc import ABC, abstractmethod
from typing import TYPE_CHECKING, Any, Generic, TypeVar

from taskiq.result import TaskiqResult

if TYPE_CHECKING:  # pragma: no cover
    from taskiq.depends.progress_tracker import TaskProgress


_ReturnType = TypeVar("_ReturnType")


class AsyncResultBackend(ABC, Generic[_ReturnType]):
    """Async result backend."""

    async def startup(self) -> None:
        """Do something when starting broker."""

    async def shutdown(self) -> None:
        """Do something on shutdown."""

    @abstractmethod
    async def set_result(self, task_id: str, result: TaskiqResult[_ReturnType]) -> None:
        """
        Saves result to the result backend.

        Result must be save so it can be accessed later
        by the calling side of the system.

        :param task_id: id of a task to save.
        :param result: result of execution.
        :return: nothing.
        """

    @abstractmethod
    async def is_result_ready(self, task_id: str) -> bool:
        """
        Checks that result of task is ready.

        :param task_id: task's id.
        :return: True if task is completed.
        """

    @abstractmethod
    async def get_result(
        self,
        task_id: str,
        with_logs: bool = False,
    ) -> TaskiqResult[_ReturnType]:
        """
        Gets result from the task.

        :param task_id: task's id.
        :param with_logs: if True it will download task's logs.
        :return: task's return value.
        """

    async def set_progress(
        self,
        task_id: str,
        progress: "TaskProgress[Any]",
    ) -> None:
        """
        Saves progress.

        Progress records carry an ``attempt`` number identifying the
        execution attempt they belong to. A record is only stored when it
        is at least as fresh as the currently stored one, so late writes
        coming from an older attempt cannot overwrite the current state.
        Records without an explicit attempt are treated as the first
        attempt.

        Backends that override this method should keep the same guarantee.

        :param task_id: task's id.
        :param progress: progress of execution.
        """
        current = await self.get_progress(task_id)
        if current is not None and current.attempt > progress.attempt:
            return
        await self._write_progress(task_id, progress)

    async def _write_progress(
        self,
        task_id: str,
        progress: "TaskProgress[Any]",
    ) -> None:
        """
        Persists progress without any freshness checks.

        Override this method in a concrete backend to define how progress
        is physically stored, while keeping :meth:`set_progress`
        responsible for the attempt-based stale write protection.

        :param task_id: task's id.
        :param progress: progress of execution.
        """

    async def get_progress(
        self,
        task_id: str,
    ) -> "TaskProgress[Any] | None":
        """
        Gets progress.

        :param task_id: task's id.
        """
