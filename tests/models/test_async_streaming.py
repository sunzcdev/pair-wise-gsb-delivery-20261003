"""
Concurrency behaviour for streaming async responses.

These tests use a controllable async byte stream so that scheduling, and thus
the point at which a second reader or a close races an in-progress read, is
deterministic on both the asyncio and trio backends.
"""

import contextlib
import typing

import anyio
import pytest

import httpx


class ControlledAsyncStream(httpx.AsyncByteStream):
    """
    A stream whose first chunk is only delivered once `gate` is set.

    `active_iterators` exposes how many iterations are in flight, and
    `close_count` records how many times the underlying stream was released.
    """

    def __init__(self, chunks: list[bytes]) -> None:
        self._chunks = chunks
        self.gate = anyio.Event()
        self.started = anyio.Event()
        self.active_iterators = 0
        self.close_count = 0

    async def __aiter__(self) -> typing.AsyncIterator[bytes]:
        self.active_iterators += 1
        self.started.set()
        # Block before handing over the first chunk, so the caller is parked
        # mid-read while still holding the response stream lock.
        await self.gate.wait()
        for chunk in self._chunks:
            yield chunk

    async def aclose(self) -> None:
        self.close_count += 1


def make_response(stream: ControlledAsyncStream) -> httpx.Response:
    return httpx.Response(200, stream=stream)


@pytest.mark.anyio
async def test_second_reader_fails_without_stealing_data() -> None:
    stream = ControlledAsyncStream([b"Hello, ", b"world!"])
    response = make_response(stream)

    chunks_one: list[bytes] = []
    reader_two_error: list[BaseException] = []

    async def first_reader() -> None:
        async for chunk in response.aiter_raw():
            chunks_one.append(chunk)

    async def reader_two() -> None:
        try:
            async with contextlib.aclosing(response.aiter_raw()) as iterator:
                async for _ in iterator:
                    pass
        except httpx.StreamInUse as exc:  # pragma: no branch
            reader_two_error.append(exc)

    async with anyio.create_task_group() as task_group:
        task_group.start_soon(first_reader)
        await stream.started.wait()
        task_group.start_soon(reader_two)
        # Give the second reader a chance to fail fast on the active reader.
        with anyio.fail_after(1.0):
            await anyio.sleep(0.1)
        stream.gate.set()

    assert chunks_one == [b"Hello, ", b"world!"]
    assert len(reader_two_error) == 1
    assert isinstance(reader_two_error[0], httpx.StreamInUse)
    # The rejected reader never touched the underlying stream.
    assert stream.active_iterators == 1
    assert stream.close_count == 1
    assert response.is_closed


async def drain(iterator: typing.AsyncIterator[bytes]) -> None:
    chunks = [chunk async for chunk in iterator]
    assert b"".join(chunks) == b"Hello, world!"


@pytest.mark.anyio
async def test_cached_content_can_be_read_concurrently() -> None:
    # Once content has been buffered it is replayed from memory, and must not
    # go through the single-reader guard.
    response = httpx.Response(200, content=b"Hello, world!")
    await response.aread()

    async with anyio.create_task_group() as task_group:
        for _ in range(3):
            task_group.start_soon(lambda: drain(response.aiter_bytes()))

    assert response.content == b"Hello, world!"


@pytest.mark.anyio
async def test_close_waits_for_in_progress_read_and_releases_once() -> None:
    stream = ControlledAsyncStream([b"Hello, ", b"world!"])
    response = make_response(stream)

    reader: list[bytes] = []

    async def read_all() -> None:
        async for chunk in response.aiter_raw():
            reader.append(chunk)

    async with anyio.create_task_group() as task_group:
        task_group.start_soon(read_all)
        await stream.started.wait()
        # Queues behind the active read rather than releasing the stream.
        task_group.start_soon(response.aclose)
        await anyio.sleep(0.1)
        stream.gate.set()

    assert reader == [b"Hello, ", b"world!"]
    # The read-to-completion path and the waiting close share one release.
    assert stream.close_count == 1
    assert response.is_closed


@pytest.mark.anyio
async def test_cancelled_read_allows_a_legal_close() -> None:
    stream = ControlledAsyncStream([b"Hello, ", b"world!"])
    response = make_response(stream)

    async def blocked_read() -> None:
        async for _ in response.aiter_raw():
            pass  # pragma: no cover

    async with anyio.create_task_group() as outer_group:
        with anyio.CancelScope() as scope:
            async with anyio.create_task_group() as reader_group:
                reader_group.start_soon(blocked_read)
                await stream.started.wait()
                scope.cancel()
        # The cancelled reader has released the lock; the close now succeeds.
        outer_group.start_soon(response.aclose)

    assert stream.close_count == 1
    assert response.is_closed


@pytest.mark.anyio
async def test_read_after_close_raises_stream_closed() -> None:
    stream = ControlledAsyncStream([b"Hello, world!"])
    response = make_response(stream)

    await response.aclose()

    with pytest.raises(httpx.StreamClosed):
        async with contextlib.aclosing(response.aiter_raw()) as iterator:
            async for _ in iterator:
                pass  # pragma: no cover

    assert stream.close_count == 1
