import asyncio
import uuid

import pytest

from taskiq import InMemoryBroker
from taskiq.abc.middleware import TaskiqMiddleware
from taskiq.brokers.inmemory_broker import InmemoryResultBackend
from taskiq.events import TaskiqEvents
from taskiq.state import TaskiqState


async def test_inmemory_success() -> None:
    broker = InMemoryBroker()
    test_val = uuid.uuid4().hex

    @broker.task
    async def task() -> str:
        return test_val

    kicked = await task.kiq()
    result = await kicked.wait_result()
    assert result.return_value == test_val
    assert not broker._running_tasks


async def test_cannot_listen() -> None:
    broker = InMemoryBroker()

    with pytest.raises(RuntimeError):
        async for _ in broker.listen():
            pass


async def test_startup() -> None:
    broker = InMemoryBroker()
    test_value = uuid.uuid4().hex

    @broker.on_event(TaskiqEvents.WORKER_STARTUP)
    async def _w_startup(state: TaskiqState) -> None:
        state.from_worker = test_value

    @broker.on_event(TaskiqEvents.CLIENT_STARTUP)
    async def _c_startup(state: TaskiqState) -> None:
        state.from_client = test_value

    await broker.startup()

    assert broker.state.from_worker == test_value
    assert broker.state.from_client == test_value


async def test_shutdown() -> None:
    broker = InMemoryBroker()
    test_value = uuid.uuid4().hex

    @broker.on_event(TaskiqEvents.WORKER_SHUTDOWN)
    async def _w_startup(state: TaskiqState) -> None:
        state.from_worker = test_value

    @broker.on_event(TaskiqEvents.CLIENT_SHUTDOWN)
    async def _c_startup(state: TaskiqState) -> None:
        state.from_client = test_value

    await broker.shutdown()

    assert broker.state.from_worker == test_value
    assert broker.state.from_client == test_value


async def test_middleware_startup_and_shutdown_fire() -> None:
    calls = []

    class RecordingMiddleware(TaskiqMiddleware):
        async def startup(self) -> None:
            calls.append("startup")

        async def shutdown(self) -> None:
            calls.append("shutdown")

    broker = InMemoryBroker().with_middlewares(RecordingMiddleware())

    await broker.startup()
    await broker.shutdown()

    assert calls == ["startup", "shutdown"]


async def test_execution() -> None:
    broker = InMemoryBroker()
    test_value = uuid.uuid4().hex

    @broker.task
    async def test_task() -> str:
        await asyncio.sleep(0.5)
        return test_value

    task = await test_task.kiq()
    assert not await task.is_ready()

    result = await task.wait_result()
    assert result.return_value == test_value


async def test_inline_awaits() -> None:
    broker = InMemoryBroker(await_inplace=True)
    slept = False

    @broker.task
    async def test_task() -> None:
        nonlocal slept
        await asyncio.sleep(0.2)
        slept = True

    task = await test_task.kiq()
    assert slept
    assert await task.is_ready()
    assert not broker._running_tasks


async def test_wait_all() -> None:
    broker = InMemoryBroker()
    slept = False

    @broker.task
    async def test_task() -> None:
        nonlocal slept
        await asyncio.sleep(0.2)
        slept = True

    task = await test_task.kiq()
    assert not slept
    await broker.wait_all()
    assert slept
    assert await task.is_ready()
    assert not broker._running_tasks


async def test_shared_backend_retains_explicitly_shared_result() -> None:
    backend = InmemoryResultBackend()
    first = InMemoryBroker(
        await_inplace=True,
        run_id="ownership-first",
    ).with_result_backend(backend)
    second = InMemoryBroker(
        await_inplace=True,
        run_id="ownership-second",
    ).with_result_backend(backend)

    @first.task(task_name="scoped-first-item")
    async def first_item() -> str:
        return "first-value"

    @second.task(task_name="scoped-second-item")
    async def second_item() -> str:
        return "second-value"

    first_handle = await first_item.kiq()
    second_handle = await second_item.kiq()
    shared_handle = second.hold_result(first_handle)

    ownership = backend.explain_ownership(first_handle.task_id)
    assert ownership is not None
    assert ownership["owner_id"] == first.scope_id
    assert ownership["holder_ids"] == [second.scope_id]

    await first.shutdown()

    assert first_handle.task_id in backend.results
    assert second_handle.task_id in backend.results
    assert (await shared_handle.wait_result(timeout=2)).return_value == "first-value"
    assert (await first_handle.get_result()).return_value == "first-value"
    assert (await second_handle.get_result()).return_value == "second-value"

    await second.shutdown()

    assert first_handle.task_id not in backend.results
    assert second_handle.task_id not in backend.results


async def test_first_scope_shutdown_does_not_break_second_waiter() -> None:
    backend = InmemoryResultBackend()
    first = InMemoryBroker(
        await_inplace=True,
        run_id="waiting-first",
    ).with_result_backend(backend)
    second = InMemoryBroker(
        run_id="waiting-second",
    ).with_result_backend(backend)

    entered = asyncio.Event()
    release = asyncio.Event()

    @first.task(task_name="waiting-first-item")
    async def first_item() -> str:
        return "from-first"

    @second.task(task_name="waiting-second-item")
    async def second_item() -> str:
        entered.set()
        await release.wait()
        return "from-second"

    second_handle = await second_item.kiq()
    await asyncio.wait_for(entered.wait(), timeout=2)

    first_handle = await first_item.kiq()
    shared_first = second.hold_result(first_handle)
    await first.shutdown()

    try:
        assert not await second_handle.is_ready()
        assert (await shared_first.get_result()).return_value == "from-first"
        release.set()
        result = await second_handle.wait_result(timeout=5)
        assert result.return_value == "from-second"
    finally:
        release.set()
        await second.shutdown()

    assert second_handle.task_id not in backend.results


async def test_explicit_shared_handle_release_is_ref_counted() -> None:
    broker = InMemoryBroker(await_inplace=True, run_id="handle-owner")
    backend = broker.result_backend

    @broker.task(task_name="explicit-handle-item")
    async def item() -> str:
        return "value"

    handle = await item.kiq()
    shared = handle.share(holder="cross-scope-holder")
    await broker.shutdown()

    assert handle.task_id in backend.results
    await shared.release()
    assert handle.task_id not in backend.results


async def test_result_scope_context_releases_owned_results() -> None:
    backend = InmemoryResultBackend()

    async with backend.create_scope("context-scope") as scope:
        broker = InMemoryBroker(
            await_inplace=True,
            run_scope=scope,
        ).with_result_backend(backend)

        @broker.task(task_name="context-scope-item")
        async def item() -> str:
            return "scoped"

        handle = await item.kiq()
        assert (await handle.wait_result(timeout=2)).return_value == "scoped"
        assert handle.task_id in backend.results

    assert handle.task_id not in backend.results


async def test_broker_without_explicit_scope_keeps_legacy_shutdown_behavior() -> None:
    broker = InMemoryBroker(await_inplace=True)

    @broker.task(task_name="legacy-shutdown-item")
    async def item() -> str:
        return "legacy"

    handle = await item.kiq()
    await broker.shutdown()

    assert (await handle.get_result()).return_value == "legacy"
