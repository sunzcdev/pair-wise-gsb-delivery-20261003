import asyncio
from typing import Annotated

import pytest

from taskiq import InMemoryBroker, TaskiqDepends


async def test_default_execution_is_unbounded() -> None:
    broker = InMemoryBroker(max_async_tasks=1)
    entered = asyncio.Event()
    release = asyncio.Event()
    started = 0

    @broker.task
    async def task() -> None:
        nonlocal started
        started += 1
        if started == 2:
            entered.set()
        await release.wait()

    await task.kiq()
    await task.kiq()
    await asyncio.wait_for(entered.wait(), timeout=1)
    assert started == 2

    release.set()
    await broker.wait_all()


async def test_bounded_execution_queues_extra_submissions() -> None:
    broker = InMemoryBroker(max_async_tasks=1, bounded_execution=True)
    first_entered = asyncio.Event()
    release_first = asyncio.Event()
    second_entered = asyncio.Event()
    observed: list[str] = []

    @broker.task
    async def first() -> None:
        observed.append("first")
        first_entered.set()
        await release_first.wait()

    @broker.task
    async def second() -> None:
        observed.append("second")
        second_entered.set()

    first_handle = await first.kiq()
    await asyncio.wait_for(first_entered.wait(), timeout=1)
    second_handle = await second.kiq()

    await asyncio.sleep(0)
    assert not second_entered.is_set()

    release_first.set()
    await broker.wait_all()

    assert observed == ["first", "second"]
    assert (await first_handle.get_result()).is_err is False
    assert (await second_handle.get_result()).is_err is False


async def test_bounded_execution_requires_positive_capacity() -> None:
    with pytest.raises(ValueError, match="bounded_execution"):
        InMemoryBroker(max_async_tasks=0, bounded_execution=True)


async def test_borrowed_subtask_settles_before_parent_and_background() -> None:
    broker = InMemoryBroker(max_async_tasks=1, bounded_execution=True)
    events: list[str] = []
    background_entered = asyncio.Event()

    async def parent_resource() -> object:
        events.append("parent-enter")
        try:
            yield object()
        finally:
            events.append("parent-exit")

    async def child_resource() -> object:
        events.append("child-enter")
        try:
            yield object()
        finally:
            events.append("child-exit")

    @broker.task
    async def child(
        resource: Annotated[object, TaskiqDepends(child_resource)],
    ) -> str:
        events.append("child-body")
        raise RuntimeError("child failed")

    @broker.task
    async def background() -> str:
        events.append("background-body")
        background_entered.set()
        return "background"

    @broker.task
    async def parent(
        resource: Annotated[object, TaskiqDepends(parent_resource)],
    ) -> str:
        events.append("parent-body")
        child_handle = await child.kiq_borrowed()
        # Derive an ordinary background submission while the parent still owns
        # the only slot. It inherits the parent's context but not its capacity.
        await background.kiq()
        assert not background_entered.is_set()

        child_result = await child_handle.wait_result()
        assert child_result.is_err
        assert isinstance(child_result.error, RuntimeError)

        events.append("parent-return")
        return "parent"

    parent_handle = await parent.kiq()
    parent_result = await parent_handle.wait_result()

    assert parent_result.return_value == "parent"
    assert parent_result.is_err is False
    assert events.index("child-exit") < events.index("parent-return")
    assert events.index("parent-exit") < events.index("background-body")
    await asyncio.wait_for(background_entered.wait(), timeout=1)

    assert events == [
        "parent-enter",
        "parent-body",
        "child-enter",
        "child-body",
        "child-exit",
        "parent-return",
        "parent-exit",
        "background-body",
    ]
    await broker.wait_all()
    assert not broker._running_tasks


async def test_ordinary_nested_submission_waits_for_capacity() -> None:
    broker = InMemoryBroker(max_async_tasks=1, bounded_execution=True)
    observed: list[str] = []
    child_ran = asyncio.Event()

    @broker.task
    async def child() -> str:
        observed.append("child")
        child_ran.set()
        return "child"

    @broker.task
    async def parent() -> str:
        await child.kiq()
        observed.append("parent")
        # The child inherited the parent's context, but not its capacity.
        assert not child_ran.is_set()
        return "parent"

    parent_handle = await parent.kiq()
    await parent_handle.wait_result()

    assert observed[0] == "parent"

    await broker.wait_all()
    assert observed == ["parent", "child"]


async def test_borrow_request_outside_task_keeps_capacity() -> None:
    broker = InMemoryBroker(max_async_tasks=1, bounded_execution=True)
    first_entered = asyncio.Event()
    release_first = asyncio.Event()
    second_entered = asyncio.Event()

    @broker.task
    async def first() -> None:
        first_entered.set()
        await release_first.wait()

    @broker.task
    async def second() -> None:
        second_entered.set()

    await first.kiq()
    await asyncio.wait_for(first_entered.wait(), timeout=1)

    await second.kiq_borrowed()
    await asyncio.sleep(0)
    assert not second_entered.is_set()

    release_first.set()
    await broker.wait_all()
    assert second_entered.is_set()


async def test_await_inplace_is_unchanged() -> None:
    broker = InMemoryBroker(max_async_tasks=1, await_inplace=True)
    done = False

    @broker.task
    async def task() -> None:
        nonlocal done
        done = True

    await task.kiq()
    assert done
    assert not broker._running_tasks
