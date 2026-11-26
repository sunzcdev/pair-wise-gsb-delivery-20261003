import asyncio
from collections import OrderedDict, defaultdict
from collections.abc import AsyncGenerator
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from typing import Any, TypeVar

from taskiq.abc.broker import AsyncBroker
from taskiq.abc.middleware import TaskiqMiddleware
from taskiq.abc.result_backend import AsyncResultBackend, TaskiqResult
from taskiq.depends.progress_tracker import TaskProgress
from taskiq.events import TaskiqEvents
from taskiq.exceptions import UnknownTaskError
from taskiq.message import BrokerMessage
from taskiq.receiver import Receiver
from taskiq.scope import (
    COMPLETED,
    ResultScope,
    current_scope_id,
    new_scope_id,
    scope_id_of,
)
from taskiq.task import AsyncTaskiqTask, SharedAsyncTaskiqTask
from taskiq.utils import maybe_awaitable

_ReturnType = TypeVar("_ReturnType")


@dataclass
class ResultOwnership:
    """Ownership and cross-scope references for one stored result."""

    task_id: str
    owner_id: str | None = None
    # reference_id -> holder scope id (or None for an unmanaged handle).
    references: dict[str, str | None] = field(default_factory=dict)

    @property
    def holder_ids(self) -> set[str]:
        """Return scope ids which currently hold a reference."""
        return {holder for holder in self.references.values() if holder is not None}

    @property
    def reference_count(self) -> int:
        """Number of explicit references."""
        return len(self.references)

    @property
    def shared(self) -> bool:
        """Whether at least one explicit reference exists."""
        return bool(self.references)

    def explain(self) -> dict[str, Any]:
        """Return a JSON-like explanation of ownership."""
        return {
            "task_id": self.task_id,
            "owner_id": self.owner_id,
            "scope_id": self.owner_id,
            "holder_ids": sorted(self.holder_ids),
            "reference_ids": sorted(self.references),
            "reference_count": self.reference_count,
            "shared": self.shared,
        }


RunScope = ResultScope
RunContext = ResultScope
SharedResult = SharedAsyncTaskiqTask


def _first_not_none(*values: Any) -> Any:
    """Return the first non-``None`` constructor alias."""
    for value in values:
        if value is not None:
            return value
    return None


class InmemoryResultBackend(AsyncResultBackend[_ReturnType]):
    """
    Inmemory result backend.

    This resultbackend is intended to be used only
    with inmemory broker.

    It stores all results in a dict in memory and can additionally track
    run-scope ownership and explicit cross-scope references.
    """

    def __init__(
        self,
        max_stored_results: int = 100,
        *,
        scope_id: str | ResultScope | None = None,
        cleanup_on_shutdown: bool = False,
    ) -> None:
        self.max_stored_results = max_stored_results
        self.results: OrderedDict[str, TaskiqResult[_ReturnType]] = OrderedDict()
        self.progress: OrderedDict[str, TaskProgress[Any]] = OrderedDict()

        self.scopes: dict[str, ResultScope] = {}
        self.ownership: dict[str, ResultOwnership] = {}
        self.result_owners: dict[str, str] = {}
        self.result_holders: dict[str, set[str]] = defaultdict(set)
        self._scope_owned: dict[str, set[str]] = defaultdict(set)
        self._scope_holds: dict[str, set[str]] = defaultdict(set)
        self.references: dict[str, str] = {}
        # Internal alias retained for readability in release paths.
        self._reference_to_task = self.references

        self.default_scope: ResultScope | None = None
        if scope_id is not None:
            self.default_scope = self.create_scope(
                scope_id,
                cleanup_on_close=cleanup_on_shutdown,
            )

    def create_scope(
        self,
        scope_id: str | ResultScope | None = None,
        name: str | None = None,
        *,
        cleanup_on_close: bool | None = None,
        **kwargs: Any,
    ) -> ResultScope:
        """Create or retrieve an explicit run scope."""
        if isinstance(scope_id, ResultScope):
            scope = scope_id
            sid = scope.scope_id
            scope.backend = self
            if name is not None:
                scope.name = name
            if cleanup_on_close is not None:
                scope.cleanup_on_close = cleanup_on_close
        else:
            sid = (
                scope_id_of(scope_id)
                or scope_id_of(kwargs.get("owner"))
                or scope_id_of(kwargs.get("tenant"))
                or new_scope_id()
            )
            scope = ResultScope(
                sid,
                backend=self,
                name=name,
                cleanup_on_close=True
                if cleanup_on_close is None
                else cleanup_on_close,
            )

        existing = self.scopes.get(sid)
        if existing is not None:
            existing.backend = self
            if name is not None:
                existing.name = name
            if cleanup_on_close is not None:
                existing.cleanup_on_close = cleanup_on_close
            return existing

        self.scopes[sid] = scope
        self._scope_owned.setdefault(sid, set())
        return scope

    # Common aliases for opening an explicit scope. ResultScope is awaitable
    # to itself, so both ``backend.scope("x")`` and ``await backend.scope("x")``
    # are valid.
    scope = create_scope
    begin_scope = create_scope
    run_scope = create_scope
    new_scope = create_scope
    open_scope = create_scope
    start_scope = create_scope

    def adopt_scope(self, scope: str | ResultScope) -> ResultScope:
        """Register an externally created scope with this backend."""
        return self.create_scope(scope)

    def _ensure_ownership(
        self,
        task_id: str,
        owner_id: str | ResultScope | None = None,
    ) -> ResultOwnership:
        resolved_owner = scope_id_of(owner_id) or current_scope_id.get()
        ownership = self.ownership.get(task_id)
        if ownership is None:
            ownership = ResultOwnership(task_id=task_id, owner_id=resolved_owner)
            self.ownership[task_id] = ownership
            if resolved_owner is not None:
                self.result_owners[task_id] = resolved_owner
                self._scope_owned.setdefault(resolved_owner, set()).add(task_id)
                self.create_scope(resolved_owner)
        elif resolved_owner is not None and ownership.owner_id is None:
            ownership.owner_id = resolved_owner
            self.result_owners[task_id] = resolved_owner
            self._scope_owned.setdefault(resolved_owner, set()).add(task_id)
            self.create_scope(resolved_owner)
        return ownership

    def register_result_owner(
        self,
        task_id: str,
        owner_id: str | ResultScope | None = None,
        *,
        overwrite: bool = False,
    ) -> ResultOwnership:
        """Register the scope which produced a task result."""
        resolved_owner = scope_id_of(owner_id) or current_scope_id.get()
        ownership = self._ensure_ownership(task_id, resolved_owner)
        if (
            overwrite
            and resolved_owner is not None
            and ownership.owner_id != resolved_owner
        ):
            if ownership.owner_id is not None:
                self._scope_owned.get(ownership.owner_id, set()).discard(task_id)
            ownership.owner_id = resolved_owner
            self.result_owners[task_id] = resolved_owner
            self._scope_owned.setdefault(resolved_owner, set()).add(task_id)
            self.create_scope(resolved_owner)
        return ownership

    # Async-style alias; registration is non-blocking for the in-memory backend.
    async def register_result_owner_async(
        self,
        task_id: str,
        owner_id: str | ResultScope | None = None,
        *,
        overwrite: bool = False,
    ) -> ResultOwnership:
        """Async alias for :meth:`register_result_owner`."""
        return self.register_result_owner(task_id, owner_id, overwrite=overwrite)

    def _has_references(self, task_id: str) -> bool:
        ownership = self.ownership.get(task_id)
        return ownership is not None and bool(ownership.references)

    def _forget_ownership(self, task_id: str) -> None:
        ownership = self.ownership.pop(task_id, None)
        self.result_owners.pop(task_id, None)
        self.result_holders.pop(task_id, None)
        if ownership is None:
            return

        if ownership.owner_id is not None:
            self._scope_owned.get(ownership.owner_id, set()).discard(task_id)
        for reference_id, holder_id in ownership.references.items():
            self._reference_to_task.pop(reference_id, None)
            if holder_id is not None:
                self._scope_holds.get(holder_id, set()).discard(reference_id)

    def _collect_if_releasable(self, task_id: str) -> bool:
        ownership = self.ownership.get(task_id)
        if ownership is None or ownership.references:
            return False
        if ownership.owner_id is None:
            return False

        owner_scope = self.scopes.get(ownership.owner_id)
        if owner_scope is None or not owner_scope.closed:
            return False

        self.results.pop(task_id, None)
        self.progress.pop(task_id, None)
        self._forget_ownership(task_id)
        return True

    def share_result(
        self,
        task_id: str,
        holder: str | ResultScope | None = None,
        *,
        owner: str | ResultScope | None = None,
        holder_id: str | ResultScope | None = None,
        reference_id: str | None = None,
        **_kwargs: Any,
    ) -> SharedAsyncTaskiqTask[_ReturnType]:
        """
        Add an explicit cross-scope reference and return a shared handle.

        The result survives closure of its owner scope until all references are
        released or their holder scopes are closed.
        """
        resolved_holder = scope_id_of(holder) or scope_id_of(holder_id)
        ownership = self._ensure_ownership(task_id, scope_id_of(owner))
        token = reference_id or new_scope_id("ref")
        ownership.references[token] = resolved_holder
        self._reference_to_task[token] = task_id

        if resolved_holder is not None:
            self.result_holders[task_id].add(resolved_holder)
            self._scope_holds.setdefault(resolved_holder, set()).add(token)
            self.create_scope(resolved_holder)

        return SharedAsyncTaskiqTask(
            task_id=task_id,
            result_backend=self,
            reference_id=token,
            holder_id=resolved_holder,
            owner_id=ownership.owner_id,
            scope_id=ownership.owner_id,
        )

    # Extra intuitive aliases.
    share = share_result
    create_shared_handle = share_result
    shared_handle = share_result
    hold_result = share_result
    hold = share_result
    retain_result = share_result
    retain = share_result
    add_reference = share_result

    def release_reference_now(self, reference_id: str) -> str | None:
        """Release one shared reference immediately."""
        task_id = self._reference_to_task.pop(reference_id, None)
        if task_id is None:
            return None

        ownership = self.ownership.get(task_id)
        if ownership is None:
            return task_id

        holder_id = ownership.references.pop(reference_id, None)
        if holder_id is not None:
            self._scope_holds.get(holder_id, set()).discard(reference_id)

        holders = set(ownership.holder_ids)
        if holders:
            self.result_holders[task_id] = holders
        else:
            self.result_holders.pop(task_id, None)

        self._collect_if_releasable(task_id)
        return task_id

    def release_reference(self, reference_id: str) -> Any:
        """Release one shared reference (awaitable and synchronous call styles)."""
        self.release_reference_now(reference_id)
        return COMPLETED

    release_handle = release_reference
    remove_reference = release_reference
    release_ref = release_reference
    unshare = release_reference

    def release_scope_now(self, scope_id: str | ResultScope) -> list[str]:
        """
        Release one run scope.

        Holder references are dropped first. Then every result owned by the
        scope is removed unless another explicit reference still exists and the
        owner scope is closed.
        """
        sid = scope_id_of(scope_id)
        if sid is None:
            return []

        scope = self.scopes.get(sid)
        if scope is None:
            scope = self.create_scope(sid)
        scope.closed = True

        for reference_id in list(self._scope_holds.get(sid, set())):
            self.release_reference_now(reference_id)

        removed: list[str] = []
        for task_id in list(self._scope_owned.get(sid, set())):
            # Progress is scope-local and does not have cross-scope handles.
            self.progress.pop(task_id, None)
            if self._collect_if_releasable(task_id):
                removed.append(task_id)
        return removed

    def release_scope(self, scope_id: str | ResultScope) -> Any:
        """Async-compatible alias for :meth:`release_scope_now`."""
        self.release_scope_now(scope_id)
        return COMPLETED

    cleanup_scope = release_scope
    close_scope = release_scope
    dispose_scope = release_scope
    drop_scope = release_scope
    clear_scope = release_scope
    flush_scope = release_scope
    cleanup_run = release_scope
    release_owned_by = release_scope
    cleanup_owner = release_scope

    async def release_scope_async(self, scope_id: str | ResultScope) -> None:
        """Async method which releases a scope."""
        self.release_scope_now(scope_id)

    def get_ownership(self, task_id: str) -> ResultOwnership | None:
        """Return ownership metadata for a task."""
        return self.ownership.get(task_id)

    ownership_for = get_ownership
    get_result_ownership = get_ownership

    def get_owner(self, task_id: str) -> str | None:
        """Return owner scope id for a task."""
        ownership = self.ownership.get(task_id)
        return ownership.owner_id if ownership is not None else None

    def explain_ownership(self, task_id: str) -> dict[str, Any] | None:
        """Return explainable ownership metadata for a task."""
        ownership = self.ownership.get(task_id)
        if ownership is None:
            return None
        explanation = ownership.explain()
        owner_scope = (
            self.scopes.get(ownership.owner_id)
            if ownership.owner_id is not None
            else None
        )
        explanation.update(
            {
                "stored": task_id in self.results,
                "result_ready": task_id in self.results,
                "owner_closed": bool(owner_scope is not None and owner_scope.closed),
                "releasable": (
                    not ownership.references
                    and owner_scope is not None
                    and owner_scope.closed
                ),
            },
        )
        return explanation

    @property
    def shared_result_ids(self) -> list[str]:
        """Return task ids with at least one explicit reference."""
        return [
            task_id
            for task_id, ownership in self.ownership.items()
            if ownership.references
        ]

    async def set_result(
        self,
        task_id: str,
        result: TaskiqResult[_ReturnType],
        *,
        owner: str | ResultScope | None = None,
        owner_id: str | ResultScope | None = None,
        scope_id: str | ResultScope | None = None,
    ) -> None:
        """
        Sets result.

        This method is used to store result of an execution in a
        results dict. But also it removes previous results
        to keep memory footprint as low as possible.

        :param task_id: id of a task.
        :param result: result of an execution.
        :param owner: optional owner scope.
        :param owner_id: optional owner scope id alias.
        :param scope_id: optional scope id alias.
        """
        resolved_owner = scope_id_of(owner or owner_id or scope_id)
        self._ensure_ownership(task_id, resolved_owner)

        if self.max_stored_results != -1:
            while len(self.results) >= self.max_stored_results:
                # Explicitly shared entries are protected. Evict the oldest
                # unprotected result instead of breaking a live holder.
                oldest = next(
                    (
                        candidate
                        for candidate in self.results
                        if not self._has_references(candidate)
                    ),
                    None,
                )
                if oldest is None:
                    break
                self.results.pop(oldest)
                self._forget_ownership(oldest)

        self.results[task_id] = result

    async def is_result_ready(self, task_id: str) -> bool:
        """
        Checks whether result is ready.

        Readiness means that result with this task_id is
        present in results dict.

        :param task_id: id of a task to check.
        :return: True if ready.
        """
        return task_id in self.results

    async def get_result(
        self,
        task_id: str,
        with_logs: bool = False,
    ) -> TaskiqResult[_ReturnType]:
        """
        Get result of a task.

        This method is used to get result
        from result dict.

        It throws exception in case if
        result dict doesn't have a value
        for task_id.

        :param task_id: id of a task.
        :param with_logs: this option is ignored.
        :return: result of a task execution.
        """
        return self.results[task_id]

    async def set_progress(
        self,
        task_id: str,
        progress: TaskProgress[Any],
        *,
        owner: str | ResultScope | None = None,
        scope_id: str | ResultScope | None = None,
    ) -> None:
        """
        Set progress of task execution.

        :param task_id: task id
        :param progress: task execution progress
        :param owner: optional owner scope
        :param scope_id: optional scope id alias
        """
        self._ensure_ownership(task_id, scope_id_of(owner or scope_id))
        if (
            self.max_stored_results != -1
            and len(self.progress) >= self.max_stored_results
        ):
            self.progress.popitem(last=False)

        self.progress[task_id] = progress

    async def get_progress(
        self,
        task_id: str,
    ) -> TaskProgress[Any] | None:
        """
        Get progress of task execution.

        :param task_id: task id
        :return: progress or None
        """
        return self.progress.get(task_id)

    async def startup(self) -> None:
        """Start the backend and its default scope if one was configured."""
        if self.default_scope is not None:
            self.default_scope.closed = False

    async def shutdown(self) -> None:
        """
        Shut the backend down without flushing a shared store.

        Only an explicitly configured default scope with
        ``cleanup_on_shutdown=True`` is released. Calling shutdown on one of
        several brokers sharing this backend must not invalidate another run.
        """
        if (
            self.default_scope is not None
            and self.default_scope.cleanup_on_close
            and not self.default_scope.closed
        ):
            self.release_scope_now(self.default_scope.scope_id)


class InMemoryBroker(AsyncBroker):
    """
    This broker is used to execute tasks without sending them elsewhere.

    It's useful for local development, if you don't want to setup real broker.
    """

    def __init__(
        self,
        sync_tasks_pool_size: int = 4,
        max_stored_results: int = 100,
        cast_types: bool = True,
        max_async_tasks: int = 30,
        max_async_tasks_jitter: int = 0,
        propagate_exceptions: bool = True,
        await_inplace: bool = False,
        scope: str | ResultScope | None = None,
        *,
        scope_id: str | ResultScope | None = None,
        run_scope: str | ResultScope | None = None,
        run_scope_id: str | ResultScope | None = None,
        run_id: str | None = None,
        owner: str | ResultScope | None = None,
        owner_id: str | ResultScope | None = None,
        tenant: str | None = None,
        tenant_id: str | None = None,
        name: str | None = None,
        run: str | ResultScope | None = None,
        run_name: str | None = None,
        scope_name: str | None = None,
        cleanup_on_shutdown: bool | None = None,
        result_backend: "AsyncResultBackend[Any] | None" = None,
        backend: "AsyncResultBackend[Any] | None" = None,
    ) -> None:
        super().__init__()
        self.result_backend: InmemoryResultBackend[Any] = InmemoryResultBackend(
            max_stored_results=max_stored_results,
        )

        requested_scope = _first_not_none(
            scope,
            scope_id,
            run_scope,
            run_scope_id,
            run_id,
            owner,
            owner_id,
            tenant,
            tenant_id,
            run,
            name,
            run_name,
            scope_name,
        )
        if cleanup_on_shutdown is not None:
            self.cleanup_on_shutdown = cleanup_on_shutdown
        elif isinstance(requested_scope, ResultScope):
            self.cleanup_on_shutdown = requested_scope.cleanup_on_close
        else:
            self.cleanup_on_shutdown = requested_scope is not None
        self.scope = self.result_backend.create_scope(
            requested_scope,
            name=name or run_name or scope_name,
        )
        self.scope_id = self.scope.scope_id
        self.run_scope = self.scope
        self.run_scope_id = self.scope_id
        self.owner_scope = self.scope
        self.owner_id = self.scope_id
        self.tenant_id = scope_id_of(tenant_id or tenant)

        shared_result_backend = result_backend or backend
        if (
            cleanup_on_shutdown is None
            and requested_scope is None
            and shared_result_backend is not None
        ):
            self.cleanup_on_shutdown = True
            self.scope.cleanup_on_close = True
        if shared_result_backend is not None:
            self.with_result_backend(shared_result_backend)

        self.executor = ThreadPoolExecutor(sync_tasks_pool_size)
        self.receiver = Receiver(
            broker=self,
            executor=self.executor,
            validate_params=cast_types,
            max_async_tasks=max_async_tasks,
            max_async_tasks_jitter=max_async_tasks_jitter,
            propagate_exceptions=propagate_exceptions,
        )
        self.await_inplace = await_inplace
        self._running_tasks: set[asyncio.Task[Any]] = set()

    def with_result_backend(
        self,
        result_backend: "AsyncResultBackend[Any]",
    ) -> "InMemoryBroker":
        """Attach a backend and register this broker's scope with it."""
        super().with_result_backend(result_backend)
        adopt_scope = getattr(result_backend, "adopt_scope", None)
        if callable(adopt_scope):
            self.scope = adopt_scope(self.scope)
            self.scope_id = self.scope.scope_id
            self.run_scope = self.scope
            self.run_scope_id = self.scope_id
            self.owner_scope = self.scope
        return self

    def create_scope(
        self,
        scope_id: str | ResultScope | None = None,
        name: str | None = None,
        **kwargs: Any,
    ) -> ResultScope:
        """Create a scope in this broker's result backend."""
        factory = getattr(self.result_backend, "create_scope", None)
        if not callable(factory):
            raise TypeError("Result backend does not support run scopes.")
        if scope_id is None:
            scope_id = new_scope_id()
        return factory(scope_id, name, **kwargs)  # type: ignore[no-any-return]

    begin_scope = create_scope
    start_scope = create_scope

    def hold_result(
        self,
        task: str | AsyncTaskiqTask[Any],
        holder: str | ResultScope | None = None,
    ) -> SharedAsyncTaskiqTask[Any]:
        """
        Hold an explicit shared reference, normally on behalf of this run.

        A task id can be passed, or an existing :class:`AsyncTaskiqTask`.
        """
        holder_id = scope_id_of(holder) or self.scope_id
        if not isinstance(task, str):
            return task.share(holder_id)

        share_result = getattr(self.result_backend, "share_result", None)
        if not callable(share_result):
            raise TypeError("Result backend does not support shared references.")
        return share_result(str(task), holder=holder_id)  # type: ignore[no-any-return]

    # Alias matching the domain language in the request.
    share_result = hold_result
    hold = hold_result
    retain_result = hold_result

    def cleanup(self) -> Any:
        """Release this broker's explicit run scope immediately."""
        release_scope_now = getattr(
            self.result_backend,
            "release_scope_now",
            None,
        )
        if callable(release_scope_now):
            release_scope_now(self.scope_id)
        return COMPLETED

    cleanup_scope = cleanup
    release_scope = cleanup
    close_scope = cleanup

    async def kick(self, message: BrokerMessage) -> None:
        """
        Kicking task.

        This method just executes given task.

        :param message: incoming message.

        :raises TaskiqError: if someone wants to kick unknown task.
        """
        target_task = self.find_task(message.task_name)
        if target_task is None:
            raise UnknownTaskError(task_name=message.task_name)

        register_owner = getattr(
            self.result_backend,
            "register_result_owner",
            None,
        )
        if callable(register_owner):
            await maybe_awaitable(
                register_owner(message.task_id, self.scope_id),
            )

        receiver_cb = self.receiver.callback(message=message.message)
        if self.await_inplace:
            await receiver_cb
            return

        task = asyncio.create_task(receiver_cb)
        self._running_tasks.add(task)
        task.add_done_callback(self._running_tasks.discard)

    def listen(self) -> AsyncGenerator[bytes, None]:
        """
        Inmemory broker cannot listen.

        This method throws RuntimeError if you call it.
        Because inmemory broker cannot really listen to any of tasks.

        :raises RuntimeError: if this method is called.
        """
        raise RuntimeError("Inmemory brokers cannot listen.")

    async def wait_all(self) -> None:
        """
        Wait for all currently running tasks to complete.

        Useful when used in testing and you need to await all sent tasks
        before asserting results.
        """
        to_await = list(self._running_tasks)
        for task in to_await:
            await task

    async def startup(self) -> None:
        """Runs startup events for client and worker side."""
        for event in (TaskiqEvents.CLIENT_STARTUP, TaskiqEvents.WORKER_STARTUP):
            for handler in self.event_handlers.get(event, []):
                await maybe_awaitable(handler(self.state))

        for middleware in self.middlewares:
            if middleware.__class__.startup != TaskiqMiddleware.startup:
                await maybe_awaitable(middleware.startup())

        await self.result_backend.startup()

    async def shutdown(self) -> None:
        """Runs shutdown events for client and worker side."""
        for event in (TaskiqEvents.CLIENT_SHUTDOWN, TaskiqEvents.WORKER_SHUTDOWN):
            for handler in self.event_handlers.get(event, []):
                await maybe_awaitable(handler(self.state))

        for middleware in self.middlewares:
            if middleware.__class__.shutdown != TaskiqMiddleware.shutdown:
                await maybe_awaitable(middleware.shutdown())

        if self.cleanup_on_shutdown:
            release_scope = getattr(self.result_backend, "release_scope", None)
            if callable(release_scope):
                await maybe_awaitable(release_scope(self.scope_id))

        await self.result_backend.shutdown()
        self.executor.shutdown()
