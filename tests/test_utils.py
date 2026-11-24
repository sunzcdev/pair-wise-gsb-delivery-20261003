import asyncio
import sys
from collections.abc import AsyncGenerator
from typing import cast

import pytest

from taskiq.utils import maybe_awaitable, merge_async_iterables

if sys.version_info >= (3, 11):
    from builtins import BaseExceptionGroup
else:
    from exceptiongroup import BaseExceptionGroup


async def test_maybe_awaitable_coroutine() -> None:
    async def meme() -> int:
        return 1

    val: int = await maybe_awaitable(meme())
    assert val == 1


async def test_maybe_awaitable_sync() -> None:
    def meme() -> int:
        return 1

    assert await maybe_awaitable(meme()) == 1


async def test_merge_async_iterables_merges_all_iterables() -> None:
    async def gen(*items: int) -> AsyncGenerator[int, None]:
        for item in items:
            await asyncio.sleep(0)
            yield item

    merged = [
        item
        async for item in merge_async_iterables(
            gen(1, 2, 3),
            gen(4, 5, 6),
        )
    ]

    assert sorted(merged) == [1, 2, 3, 4, 5, 6]


async def test_merge_async_iterables_without_iterables() -> None:
    # The TypeVar cannot be inferred from an empty argument list,
    # so the return type is provided explicitly.
    merged = cast("AsyncGenerator[int, None]", merge_async_iterables())
    assert [item async for item in merged] == []


async def test_merge_async_iterables_propagates_inner_errors() -> None:
    async def failing() -> AsyncGenerator[int, None]:
        yield 1
        raise ValueError("inner generator failed")

    with pytest.raises(BaseExceptionGroup) as exc_info:
        async for _ in merge_async_iterables(failing()):
            pass

    assert isinstance(exc_info.value.exceptions[0], ValueError)


async def test_merge_async_iterables_closes_inner_iterables() -> None:
    closed: list[str] = []

    def tracking(name: str) -> AsyncGenerator[int, None]:
        async def gen() -> AsyncGenerator[int, None]:
            try:
                while True:
                    yield 1
                    await asyncio.sleep(0)
            finally:
                closed.append(name)

        return gen()

    merged = merge_async_iterables(tracking("first"), tracking("second"))
    await merged.__anext__()
    # At this point one producer is blocked in send() and the other
    # waits for its next value. Closing must be clean and run the
    # cleanup of every inner generator.
    await merged.aclose()

    assert sorted(closed) == ["first", "second"]


async def test_merge_async_iterables_rejects_non_positive_buffer() -> None:
    with pytest.raises(ValueError, match="max_buffer_size"):
        async for _ in merge_async_iterables(max_buffer_size=0):
            pass


async def test_merge_async_iterables_bounded_buffer_backpressures() -> None:
    gate = asyncio.Event()
    produced = 0

    async def producer() -> AsyncGenerator[int, None]:
        nonlocal produced
        # Produce two items, then block before producing any more.
        for item in range(2):
            produced += 1
            yield item
        await gate.wait()
        for item in range(2, 4):
            produced += 1
            yield item

    merged = merge_async_iterables(producer(), max_buffer_size=1)
    # Two items were produced: one is handed to the consumer and one
    # fills the single buffer slot. The producer must now be blocked
    # on the gate instead of buffering further items.
    first = await asyncio.wait_for(merged.__anext__(), timeout=1)
    await asyncio.sleep(0.05)
    assert produced == 2

    # Taking the buffered item frees the slot, but the producer is
    # still gated, so nothing further is produced yet.
    second = await asyncio.wait_for(merged.__anext__(), timeout=1)
    await asyncio.sleep(0.05)
    assert produced == 2

    gate.set()
    third = await asyncio.wait_for(merged.__anext__(), timeout=1)
    fourth = await asyncio.wait_for(merged.__anext__(), timeout=1)
    await merged.aclose()

    assert [first, second, third, fourth] == [0, 1, 2, 3]
    assert produced == 4


async def test_merge_async_iterables_buffer_blocks_full_producer() -> None:
    total = 10
    gate = asyncio.Event()

    async def producer() -> AsyncGenerator[int, None]:
        for item in range(total):
            yield item
        await gate.wait()

    merged = merge_async_iterables(producer(), max_buffer_size=2)
    # Drain only a few items while the producer keeps trying to push
    # the remaining ones. A bounded buffer must keep the pending count
    # small instead of buffering every produced value.
    received = [await asyncio.wait_for(merged.__anext__(), timeout=1) for _ in range(3)]
    assert received == [0, 1, 2]

    gate.set()
    rest = [
        await asyncio.wait_for(merged.__anext__(), timeout=1)
        for _ in range(total - 3)
    ]
    await merged.aclose()

    assert received + rest == list(range(total))
