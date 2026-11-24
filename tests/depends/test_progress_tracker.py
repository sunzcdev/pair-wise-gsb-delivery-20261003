import asyncio
from concurrent.futures import ThreadPoolExecutor
from typing import Any

import pytest
from pydantic import ValidationError

from taskiq import (
    AsyncTaskiqDecoratedTask,
    InMemoryBroker,
    SimpleRetryMiddleware,
    TaskiqDepends,
    TaskiqMessage,
)
from taskiq.abc import AsyncBroker
from taskiq.depends.progress_tracker import (
    ProgressTracker,
    TaskProgress,
    TaskState,
)
from taskiq.receiver import Receiver


def get_receiver(
    broker: AsyncBroker | None = None,
    no_parse: bool = False,
    max_async_tasks: int | None = None,
) -> Receiver:
    """
    Returns receiver with custom broker and args.

    :param broker: broker, defaults to None
    :param no_parse: parameter to taskiq_args, defaults to False
    :param cli_args: Taskiq worker CLI arguments.
    :return: new receiver.
    """
    if broker is None:
        broker = InMemoryBroker()
    return Receiver(
        broker,
        executor=ThreadPoolExecutor(max_workers=10),
        validate_params=not no_parse,
        max_async_tasks=max_async_tasks,
    )


def get_message(
    task: AsyncTaskiqDecoratedTask[Any, Any],
    task_id: str | None = None,
    *args: Any,
    labels: dict[str, str] | None = None,
    **kwargs: dict[str, Any],
) -> TaskiqMessage:
    if labels is None:
        labels = {}
    return TaskiqMessage(
        task_id=task_id or task.broker.id_generator(),
        task_name=task.task_name,
        labels=labels,
        args=list(args),
        kwargs=kwargs,
    )


@pytest.mark.parametrize(
    "state,meta",
    [
        (TaskState.STARTED, "hello world!"),
        ("retry", "retry error!"),
        ("custom state", {"Complex": "Value"}),
    ],
)
async def test_progress_tracker_ctx_raw(state: Any, meta: Any) -> None:
    broker = InMemoryBroker()

    @broker.task
    async def test_func(tes_val: ProgressTracker[Any] = TaskiqDepends()) -> None:
        await tes_val.set_progress(state, meta)

    kicker = await test_func.kiq()
    result = await kicker.wait_result()

    assert not result.is_err
    progress = await broker.result_backend.get_progress(kicker.task_id)
    assert progress is not None
    assert progress.meta == meta
    assert progress.state == state


async def test_progress_tracker_ctx_none() -> None:
    broker = InMemoryBroker()

    @broker.task
    async def test_func() -> None:
        pass

    kicker = await test_func.kiq()
    result = await kicker.wait_result()

    assert not result.is_err
    progress = await broker.result_backend.get_progress(kicker.task_id)
    assert progress is None


@pytest.mark.parametrize(
    "state,meta",
    [
        (("state", "error"), 1),
    ],
)
async def test_progress_tracker_validation_error(state: Any, meta: Any) -> None:
    broker = InMemoryBroker()

    @broker.task
    async def test_func(progress: ProgressTracker[int] = TaskiqDepends()) -> None:
        await progress.set_progress(state, meta)  # type: ignore

    kicker = await test_func.kiq()
    result = await kicker.wait_result()
    with pytest.raises(ValidationError):
        result.raise_for_error()

    progress = await broker.result_backend.get_progress(kicker.task_id)
    assert progress is None


async def test_progress_is_tagged_with_first_attempt() -> None:
    broker = InMemoryBroker()

    @broker.task
    async def test_func(
        tracker: ProgressTracker[Any] = TaskiqDepends(),
    ) -> None:
        await tracker.set_progress(TaskState.STARTED, "10%")

    kicker = await test_func.kiq()
    await kicker.wait_result()

    progress = await broker.result_backend.get_progress(kicker.task_id)
    assert progress is not None
    assert progress.attempt == 1
    assert progress.state == TaskState.STARTED
    assert progress.meta == "10%"


async def test_progress_attempt_follows_retries_label() -> None:
    broker = InMemoryBroker()

    @broker.task
    async def test_func(
        tracker: ProgressTracker[Any] = TaskiqDepends(),
    ) -> None:
        await tracker.set_progress(TaskState.STARTED, "5%")

    kicker = test_func.kicker().with_labels(_retries=2)
    sent = await kicker.kiq()
    await sent.wait_result()

    progress = await broker.result_backend.get_progress(sent.task_id)
    assert progress is not None
    assert progress.attempt == 3


async def test_backend_rejects_stale_attempt_write() -> None:
    backend = InMemoryBroker().result_backend

    await backend.set_progress(
        "task-id",
        TaskProgress(state=TaskState.STARTED, meta="5%", attempt=2),
    )
    # A late write produced by the first attempt must be ignored.
    await backend.set_progress(
        "task-id",
        TaskProgress(state=TaskState.STARTED, meta="90%", attempt=1),
    )

    progress = await backend.get_progress("task-id")
    assert progress is not None
    assert progress.attempt == 2
    assert progress.meta == "5%"


async def test_backend_accepts_same_attempt_and_newer_attempt() -> None:
    backend = InMemoryBroker().result_backend

    await backend.set_progress(
        "task-id",
        TaskProgress(state=TaskState.STARTED, meta="5%", attempt=2),
    )
    await backend.set_progress(
        "task-id",
        TaskProgress(state=TaskState.STARTED, meta="10%", attempt=2),
    )
    progress = await backend.get_progress("task-id")
    assert progress is not None
    assert progress.attempt == 2
    assert progress.meta == "10%"

    await backend.set_progress(
        "task-id",
        TaskProgress(state=TaskState.SUCCESS, meta="100%", attempt=3),
    )
    progress = await backend.get_progress("task-id")
    assert progress is not None
    assert progress.attempt == 3
    assert progress.meta == "100%"


async def test_progress_without_attempt_field_is_treated_as_first() -> None:
    backend = InMemoryBroker().result_backend

    # Simulate data written by an older version without the attempt field.
    await backend._write_progress(  # noqa: SLF001
        "task-id",
        TaskProgress(state=TaskState.STARTED, meta="legacy"),
    )

    progress = await backend.get_progress("task-id")
    assert progress is not None
    assert progress.attempt == 1
    assert progress.meta == "legacy"

    # Legacy reads stay visible, and a second attempt still supersedes them.
    await backend.set_progress(
        "task-id",
        TaskProgress(state=TaskState.STARTED, meta="5%", attempt=2),
    )
    progress = await backend.get_progress("task-id")
    assert progress is not None
    assert progress.attempt == 2
    assert progress.meta == "5%"


async def test_late_write_after_retry_keeps_current_attempt() -> None:
    """First attempt's delayed 90% cannot overwrite the retry's 5%."""
    broker = InMemoryBroker()
    first_attempt_started = asyncio.Event()
    allow_first_write = asyncio.Event()
    second_attempt_started = asyncio.Event()

    @broker.task
    async def flaky_task(
        tracker: ProgressTracker[Any] = TaskiqDepends(),
    ) -> None:
        attempt = tracker.attempt
        if attempt == 1:
            first_attempt_started.set()
            # Wait until the retry is running before writing 90%.
            await allow_first_write.wait()
            await tracker.set_progress(TaskState.STARTED, "90%")
            raise RuntimeError("first attempt fails")

        second_attempt_started.set()
        await tracker.set_progress(TaskState.STARTED, "5%")

    kicker = await flaky_task.kiq()

    await first_attempt_started.wait()
    # Manually trigger the retry while the first attempt is still alive.
    retry = (
        flaky_task.kicker()
        .with_task_id(kicker.task_id)
        .with_labels(_retries=1)
    )
    retry_kicker = await retry.kiq()
    await second_attempt_started.wait()

    # Now let the stale first attempt finish its delayed write.
    allow_first_write.set()
    await broker.wait_all()
    await retry_kicker.wait_result()

    progress = await broker.result_backend.get_progress(kicker.task_id)
    assert progress is not None
    assert progress.attempt == 2
    assert progress.meta == "5%"


async def test_write_after_cancel_cannot_clobber_current_progress() -> None:
    """A cleanup write made after cancel must not change stored progress."""
    broker = InMemoryBroker()
    task_started = asyncio.Event()
    allow_task_to_finish = asyncio.Event()

    @broker.task
    async def cancellable_task(
        tracker: ProgressTracker[Any] = TaskiqDepends(),
    ) -> None:
        await tracker.set_progress(TaskState.STARTED, "5%")
        task_started.set()
        try:
            await allow_task_to_finish.wait()
        except asyncio.CancelledError:
            # Simulate cleanup code that tries to update progress.
            await tracker.set_progress(TaskState.CANCELLED, "cleanup")
            raise

    kicked = await cancellable_task.kiq()
    await task_started.wait()

    running = next(iter(broker._running_tasks))  # noqa: SLF001
    running.cancel()
    await broker.wait_all()

    progress = await broker.result_backend.get_progress(kicked.task_id)
    assert progress is not None
    assert progress.meta == "5%"
    assert progress.state == TaskState.STARTED


async def test_attempt_property_is_one_based() -> None:
    tracker = ProgressTracker.__new__(ProgressTracker)

    class _FakeMessage:
        labels: dict[str, Any] = {}

    class _FakeContext:
        message = _FakeMessage()

    tracker.context = _FakeContext()  # type: ignore[assignment]
    assert tracker.attempt == 1

    tracker.context.message.labels["_retries"] = "4"
    assert tracker.attempt == 5


async def test_retry_flow_keeps_progress_of_latest_attempt() -> None:
    """End-to-end: 90% (attempt 1) must not beat 5% written by the retry."""
    broker = InMemoryBroker().with_middlewares(
        SimpleRetryMiddleware(no_result_on_retry=True),
    )
    first_attempt_reached = asyncio.Event()
    allow_late_write = asyncio.Event()

    @broker.task(retry_on_error=True, max_retries=2)
    async def retried_task(
        tracker: ProgressTracker[Any] = TaskiqDepends(),
    ) -> None:
        if tracker.attempt == 1:
            first_attempt_reached.set()
            await allow_late_write.wait()
            # Delayed write from the first attempt, arriving only after
            # the second attempt has already reported its own progress.
            await tracker.set_progress(TaskState.STARTED, "90%")
            raise RuntimeError("boom")

        await tracker.set_progress(TaskState.STARTED, "5%")

    kicked = await retried_task.kiq()
    await first_attempt_reached.wait()

    allow_late_write.set()
    result = await kicked.wait_result(check_interval=0.05, timeout=5)
    assert not result.is_err

    progress = await broker.result_backend.get_progress(kicked.task_id)
    assert progress is not None
    assert progress.attempt == 2
    assert progress.meta == "5%"
    assert progress.state == TaskState.STARTED
