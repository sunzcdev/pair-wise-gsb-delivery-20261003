import asyncio
from logging import getLogger
from time import time
from typing import TYPE_CHECKING, Any, Generic, TypeVar

from taskiq.compat import parse_obj_as
from taskiq.exceptions import (
    ResultGetError,
    ResultIsReadyError,
    TaskiqResultTimeoutError,
)
from taskiq.scope import COMPLETED, scope_id_of

if TYPE_CHECKING:  # pragma: no cover
    from taskiq.abc.result_backend import AsyncResultBackend
    from taskiq.depends.progress_tracker import TaskProgress
    from taskiq.result import TaskiqResult

logger = getLogger("taskiq.task")

_ReturnType = TypeVar("_ReturnType")


class AsyncTaskiqTask(Generic[_ReturnType]):
    """AsyncTask for AsyncResultBackend."""

    def __init__(
        self,
        task_id: str,
        result_backend: "AsyncResultBackend[_ReturnType]",
        return_type: type[_ReturnType] | None = None,
        *,
        scope_id: str | None = None,
        owner_id: str | None = None,
    ) -> None:
        self.task_id = task_id
        self.result_backend = result_backend
        self.return_type = return_type
        self.scope_id = scope_id or owner_id
        self.owner_id = owner_id or scope_id

    @property
    def owner(self) -> str | None:
        """Owning run-scope id."""
        return self.owner_id

    @property
    def ownership(self) -> Any | None:
        """Backend ownership metadata when the backend supports scopes."""
        get_ownership = getattr(self.result_backend, "get_ownership", None)
        if callable(get_ownership):
            return get_ownership(self.task_id)
        return None

    def share(
        self,
        holder: Any | None = None,
        **kwargs: Any,
    ) -> "SharedAsyncTaskiqTask[_ReturnType]":
        """
        Create an explicit cross-scope reference to this result.

        The returned handle must be released (or its holder scope closed)
        before a scope-owned result can be removed while references remain.
        """
        factory = getattr(self.result_backend, "share_result", None)
        if not callable(factory):
            raise TypeError(
                f"{type(self.result_backend).__name__} does not support "
                "explicit result sharing.",
            )

        holder_id = scope_id_of(holder) or scope_id_of(
            kwargs.pop("holder_id", None),
        )
        owner = kwargs.pop("owner", None)
        owner_id = (
            scope_id_of(kwargs.pop("owner_id", None))
            or scope_id_of(owner)
            or self.owner_id
            or self.scope_id
        )
        shared = factory(
            self.task_id,
            holder=holder_id,
            owner=owner_id,
            **kwargs,
        )
        if isinstance(shared, SharedAsyncTaskiqTask):
            return shared
        # Backends may return an awaitable wrapper. Preserve the standard handle
        # surface by resolving it through the normal async path if needed.
        return shared  # type: ignore[no-any-return]

    # Readable aliases used by different resource-lifecycle conventions.
    share_result = share
    share_with = share
    as_shared = share
    shared = share
    handle = share
    shared_handle = share
    create_shared_handle = share
    hand_over = share
    handoff = share
    pass_to = share
    hold = share
    retain = share

    async def is_ready(self) -> bool:
        """
        Checks if task is completed.

        :raises ResultIsReadyError: if we can't get info about task readiness.

        :return: True if task is completed.
        """
        try:
            return await self.result_backend.is_result_ready(self.task_id)
        except Exception as exc:
            raise ResultIsReadyError from exc

    async def get_result(self, with_logs: bool = False) -> "TaskiqResult[_ReturnType]":
        """
        Get result of a task from result backend.

        :param with_logs: whether you want to fetch logs from worker.

        :raises ResultGetError: if we can't get result from ResultBackend.

        :return: task's return value.
        """
        try:
            res = await self.result_backend.get_result(
                self.task_id,
                with_logs=with_logs,
            )
            if self.return_type is not None:
                try:
                    res.return_value = parse_obj_as(
                        self.return_type,
                        res.return_value,
                    )
                except ValueError:
                    logger.warning("Cannot parse return type into %s", self.return_type)
            return res
        except Exception as exc:
            raise ResultGetError from exc

    async def wait_result(
        self,
        check_interval: float = 0.2,
        timeout: float = -1.0,
        with_logs: bool = False,
    ) -> "TaskiqResult[_ReturnType]":
        """
        Waits until result is ready.

        This method just checks whether the task is
        ready. And if it is it returns the result.

        It may throw TaskiqResultTimeoutError if
        task didn't become ready in provided
        period of time.

        :param check_interval: How often checks are performed.
        :param timeout: timeout for the result.
        :param with_logs: whether you want to fetch logs from worker.
        :raises TaskiqResultTimeoutError: if task didn't
            become ready in provided period of time.
        :return: task's return value.
        """
        start_time = time()
        while not await self.is_ready():
            if 0 < timeout < time() - start_time:
                raise TaskiqResultTimeoutError(timeout=timeout)
            await asyncio.sleep(check_interval)
        return await self.get_result(with_logs=with_logs)

    async def get_progress(self) -> "TaskProgress[Any] | None":
        """
        Get task progress.

        :return: task's progress.
        """
        return await self.result_backend.get_progress(self.task_id)


class SharedAsyncTaskiqTask(AsyncTaskiqTask[_ReturnType]):
    """
    Explicit shared handle which keeps a scoped result alive.

    The handle is awaitable to itself, allowing both::

        shared = task.share(holder="second-run")
        shared = await task.share(holder="second-run")

    and then the usual ``get_result``/``wait_result`` methods work.
    """

    def __init__(
        self,
        task_id: str,
        result_backend: "AsyncResultBackend[_ReturnType]",
        return_type: type[_ReturnType] | None = None,
        *,
        scope_id: str | None = None,
        owner_id: str | None = None,
        reference_id: str | None = None,
        holder_id: str | None = None,
    ) -> None:
        super().__init__(
            task_id=task_id,
            result_backend=result_backend,
            return_type=return_type,
            scope_id=scope_id,
            owner_id=owner_id,
        )
        self.reference_id = reference_id
        self.holder_id = holder_id
        self.released = False

    @property
    def holder(self) -> str | None:
        """Scope id holding this shared reference."""
        return self.holder_id

    def explain(self) -> dict[str, Any] | None:
        """Return explainable backend ownership metadata if available."""
        explain_ownership = getattr(
            self.result_backend,
            "explain_ownership",
            None,
        )
        if callable(explain_ownership):
            return explain_ownership(self.task_id)  # type: ignore[no-any-return]
        return None

    def release(self) -> Any:
        """Release this shared reference immediately."""
        if not self.released:
            self.released = True
            release_reference = getattr(
                self.result_backend,
                "release_reference_now",
                None,
            )
            if callable(release_reference) and self.reference_id is not None:
                release_reference(self.reference_id)
        return COMPLETED

    async def release_async(self) -> None:
        """Async alias for releasing this shared reference."""
        await self.release()

    def close(self) -> Any:
        """Close and release the shared handle."""
        return self.release()

    aclose = release
    dispose = release

    async def __aenter__(self) -> "SharedAsyncTaskiqTask[_ReturnType]":
        return self

    async def __aexit__(self, *_args: Any) -> None:
        await self.release()

    def __await__(self) -> Any:
        async def identity() -> "SharedAsyncTaskiqTask[_ReturnType]":
            return self

        return identity().__await__()


SharedResultHandle = SharedAsyncTaskiqTask
SharedTaskiqHandle = SharedAsyncTaskiqTask
SharedResult = SharedAsyncTaskiqTask
SharedResultLease = SharedAsyncTaskiqTask
ResultLease = SharedAsyncTaskiqTask
ResultHandle = AsyncTaskiqTask
