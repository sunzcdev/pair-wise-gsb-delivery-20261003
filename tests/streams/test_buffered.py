from __future__ import annotations

import pytest

from anyio import (
    CheckpointBudgetExceeded,
    ClosedResourceError,
    DelimiterNotFound,
    EndOfStream,
    IncompleteRead,
    create_memory_object_stream,
    create_task_group,
)
from anyio.abc import ByteReceiveStream, ObjectStream, ObjectStreamConnectable
from anyio.streams.buffered import (
    BufferedByteReceiveStream,
    BufferedByteStream,
    BufferedConnectable,
)
from anyio.streams.stapled import StapledObjectStream


async def test_receive_skips_empty_messages() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](6)
    with send_stream, receive_stream:
        stream = BufferedByteReceiveStream(receive_stream)
        for chunk in (b"", b"", b"abcd", b"", b"ef", b""):
            send_stream.send_nowait(chunk)

        send_stream.close()
        assert await stream.receive(2) == b"ab"
        assert stream.buffer == b"cd"
        assert await stream.receive(2) == b"cd"
        assert await stream.receive(2) == b"ef"
        with pytest.raises(EndOfStream):
            await stream.receive(2)


async def test_receive_exactly() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](2)
    buffered_stream = BufferedByteReceiveStream(receive_stream)
    await send_stream.send(b"abcd")
    await send_stream.send(b"efgh")
    result = await buffered_stream.receive_exactly(8)
    assert result == b"abcdefgh"
    assert isinstance(result, bytes)

    send_stream.close()
    receive_stream.close()


async def test_receive_exactly_incomplete() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    buffered_stream = BufferedByteReceiveStream(receive_stream)
    await send_stream.send(b"abcd")
    await send_stream.aclose()
    with pytest.raises(IncompleteRead):
        await buffered_stream.receive_exactly(8)

    send_stream.close()
    receive_stream.close()


@pytest.mark.parametrize(
    "method, args",
    [
        pytest.param("receive", (2,), id="receive"),
        pytest.param("receive_exactly", (2,), id="receive_exactly"),
        pytest.param("receive_until", (b"\n", 10), id="receive_until"),
    ],
)
async def test_receive_closed_with_buffer(
    method: str, args: tuple[object, ...]
) -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    with send_stream, receive_stream:
        buffered_stream = BufferedByteReceiveStream(receive_stream)
        buffered_stream.feed_data(b"abcd\n")
        await buffered_stream.aclose()
        with pytest.raises(ClosedResourceError):
            await getattr(buffered_stream, method)(*args)


async def test_receive_until() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](2)
    buffered_stream = BufferedByteReceiveStream(receive_stream)
    await send_stream.send(b"abcd")
    await send_stream.send(b"efgh")

    result = await buffered_stream.receive_until(b"de", 10)
    assert result == b"abc"
    assert isinstance(result, bytes)

    result = await buffered_stream.receive_until(b"h", 10)
    assert result == b"fg"
    assert isinstance(result, bytes)

    send_stream.close()
    receive_stream.close()


async def test_receive_until_incomplete() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    buffered_stream = BufferedByteReceiveStream(receive_stream)
    await send_stream.send(b"abcd")
    await send_stream.aclose()
    with pytest.raises(IncompleteRead):
        assert await buffered_stream.receive_until(b"de", 10)

    assert buffered_stream.buffer == b"abcd"

    send_stream.close()
    receive_stream.close()


@pytest.mark.parametrize(
    "stream_class", [BufferedByteReceiveStream, BufferedByteStream]
)
@pytest.mark.parametrize("layout", ["one-chunk", "split", "buffered"])
@pytest.mark.parametrize(
    "data,delimiter,limit",
    [(b"abcde!", b"!", 3), (b"abc\r\n", b"\r\n", 4)],
)
async def test_delimiter_beyond_limit(
    stream_class: type[BufferedByteReceiveStream | BufferedByteStream],
    layout: str,
    data: bytes,
    delimiter: bytes,
    limit: int,
) -> None:
    send, receive = create_memory_object_stream[bytes](2)
    async with send, receive:
        stream = stream_class(StapledObjectStream(send, receive))
        if layout == "buffered":
            stream.feed_data(data)
        elif layout == "split":
            await send.send(data[:limit])
            await send.send(data[limit:])
        else:
            await send.send(data)

        await send.aclose()
        with pytest.raises(DelimiterNotFound):
            await stream.receive_until(delimiter, limit)

        # The rejected read must not discard data, including a partial delimiter.
        assert (
            await stream.receive_until(delimiter, len(data)) == data[: -len(delimiter)]
        )
        assert stream.buffer == b""


@pytest.mark.parametrize("split", [1, 3, 4, 5])
async def test_delimiter_ending_at_limit(split: int) -> None:
    data = b"abc\r\nrest"
    send, receive = create_memory_object_stream[bytes](2)
    async with send, receive:
        await send.send(data[:split])
        await send.send(data[split:])
        await send.aclose()
        stream = BufferedByteReceiveStream(receive)

        assert await stream.receive_until(b"\r\n", 5) == b"abc"
        assert await stream.receive_exactly(4) == b"rest"


async def test_delimiter_straddling_limit() -> None:
    send, receive = create_memory_object_stream[bytes](2)
    async with send, receive:
        stream = BufferedByteReceiveStream(receive)
        await send.send(b"ab")
        await send.send(b"\r\nrest")
        await send.aclose()

        with pytest.raises(DelimiterNotFound):
            await stream.receive_until(b"\r\n", 3)

        assert await stream.receive_until(b"\r\n", 4) == b"ab"
        assert await stream.receive_exactly(4) == b"rest"


async def test_buffered_stream() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    buffered_stream = BufferedByteStream(
        StapledObjectStream(send_stream, receive_stream)
    )
    await send_stream.send(b"abcd")
    assert await buffered_stream.receive_exactly(2) == b"ab"
    assert await buffered_stream.receive_exactly(2) == b"cd"

    # send_eof() should close only the sending end
    await buffered_stream.send_eof()
    pytest.raises(ClosedResourceError, send_stream.send_nowait, b"abc")
    pytest.raises(EndOfStream, receive_stream.receive_nowait)

    # aclose() closes the receive stream too
    await buffered_stream.aclose()
    pytest.raises(ClosedResourceError, receive_stream.receive_nowait)


async def test_buffered_connectable() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    memory_stream = StapledObjectStream(send_stream, receive_stream)

    class MemoryObjectConnectable(ObjectStreamConnectable[bytes]):
        async def connect(self) -> ObjectStream[bytes]:
            return memory_stream

    connectable = BufferedConnectable(MemoryObjectConnectable())
    async with await connectable.connect() as stream:
        assert isinstance(stream, BufferedByteStream)
        await stream.send(b"abcd")
        assert await stream.receive_exactly(2) == b"ab"
        assert await stream.receive_exactly(2) == b"cd"


@pytest.mark.parametrize("max_bytes", [0, -1])
async def test_receive_invalid_max_bytes(max_bytes: int) -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    buffered_stream = BufferedByteReceiveStream(receive_stream)
    with pytest.raises(ValueError, match="max_bytes must be a positive integer"):
        await buffered_stream.receive(max_bytes)

    send_stream.close()
    receive_stream.close()


@pytest.mark.parametrize("max_bytes", [0, -1])
async def test_buffered_stream_receive_invalid_max_bytes(max_bytes: int) -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    buffered_stream = BufferedByteStream(
        StapledObjectStream(send_stream, receive_stream)
    )
    with pytest.raises(ValueError, match="max_bytes must be a positive integer"):
        await buffered_stream.receive(max_bytes)

    await buffered_stream.aclose()


async def test_feed_data() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    buffered_stream = BufferedByteStream(
        StapledObjectStream(send_stream, receive_stream)
    )
    send_stream.send_nowait(b"abcd")

    # The stream has not received the sent data yet, so b"xxx" should come out of the
    # buffer first, despite this order of data input
    buffered_stream.feed_data(b"xxx")
    buffered_stream.feed_data(b"foo")
    assert await buffered_stream.receive_exactly(10) == b"xxxfooabcd"
    await buffered_stream.aclose()


class TestCheckpoint:
    async def test_rollback_restores_consumed_bytes(self) -> None:
        send, receive = create_memory_object_stream[bytes](4)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcdefgh")

            checkpoint = stream.checkpoint()
            assert await stream.receive_exactly(4) == b"abcd"
            assert checkpoint.consumed == 4
            assert stream.buffer == b"efgh"
            checkpoint.rollback()

            # The same bytes can be parsed again, without touching upstream
            assert await stream.receive_exactly(8) == b"abcdefgh"
            assert stream.buffer == b""

    async def test_commit_discards_consumed_bytes(self) -> None:
        send, receive = create_memory_object_stream[bytes](4)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcdefgh")

            checkpoint = stream.checkpoint()
            assert await stream.receive_exactly(4) == b"abcd"
            checkpoint.commit()
            assert checkpoint.closed

            assert await stream.receive_exactly(4) == b"efgh"

    async def test_context_manager_rolls_back_by_default(self) -> None:
        send, receive = create_memory_object_stream[bytes](4)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcdefgh")

            async with stream.checkpoint() as checkpoint:
                assert await stream.receive_exactly(3) == b"abc"

            assert checkpoint.closed
            assert await stream.receive_exactly(4) == b"abcd"

    async def test_context_manager_commit_inside_block(self) -> None:
        send, receive = create_memory_object_stream[bytes](4)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcdefgh")

            async with stream.checkpoint() as checkpoint:
                assert await stream.receive_exactly(3) == b"abc"
                checkpoint.commit()

            assert await stream.receive_exactly(5) == b"defgh"

    async def test_double_close_rejected(self) -> None:
        send, receive = create_memory_object_stream[bytes](1)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            checkpoint = stream.checkpoint()
            checkpoint.rollback()
            with pytest.raises(RuntimeError, match="already been closed"):
                checkpoint.rollback()
            with pytest.raises(RuntimeError, match="already been closed"):
                checkpoint.commit()

    async def test_nested_checkpoints_rejected(self) -> None:
        send, receive = create_memory_object_stream[bytes](1)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            stream.checkpoint()
            with pytest.raises(RuntimeError, match="already has an active checkpoint"):
                stream.checkpoint()

    async def test_new_checkpoint_allowed_after_release(self) -> None:
        send, receive = create_memory_object_stream[bytes](1)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            stream.checkpoint().commit()
            checkpoint = stream.checkpoint(4)
            assert checkpoint.budget == 4

    @pytest.mark.parametrize("budget", [0, -1])
    async def test_invalid_budget(self, budget: int) -> None:
        send, receive = create_memory_object_stream[bytes](1)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            with pytest.raises(ValueError, match="budget must be a positive integer"):
                stream.checkpoint(budget)

    async def test_budget_exceeded_receive_exactly(self) -> None:
        send, receive = create_memory_object_stream[bytes](4)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcdefgh")

            checkpoint = stream.checkpoint(budget=4)
            assert checkpoint.remaining == 4
            with pytest.raises(CheckpointBudgetExceeded) as exc:
                await stream.receive_exactly(5)

            assert exc.value.budget == 4
            # Nothing was consumed by the failed read
            assert checkpoint.consumed == 0
            assert checkpoint.remaining == 4
            assert not checkpoint.closed

            # The checkpoint is still usable
            assert await stream.receive_exactly(4) == b"abcd"
            assert checkpoint.consumed == 4
            assert checkpoint.remaining == 0
            with pytest.raises(CheckpointBudgetExceeded):
                await stream.receive(1)

    async def test_budget_exceeded_partial_budget_used(self) -> None:
        send, receive = create_memory_object_stream[bytes](4)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcdefgh")

            checkpoint = stream.checkpoint(budget=4)
            assert await stream.receive_exactly(2) == b"ab"
            assert checkpoint.remaining == 2
            with pytest.raises(CheckpointBudgetExceeded):
                await stream.receive_exactly(3)

            # The failed read must not consume or pull any bytes
            assert checkpoint.consumed == 2
            checkpoint.rollback()
            assert await stream.receive_exactly(4) == b"abcd"

    async def test_budget_enforced_across_reads(self) -> None:
        send, receive = create_memory_object_stream[bytes](4)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcdefgh")

            async with stream.checkpoint(3) as checkpoint:
                assert await stream.receive(2) == b"ab"
                assert await stream.receive(1) == b"c"
                assert checkpoint.consumed == 3
                with pytest.raises(CheckpointBudgetExceeded):
                    await stream.receive(1)

            # Auto rollback restores everything
            assert await stream.receive_exactly(4) == b"abcd"

    async def test_budget_covers_buffer_and_upstream(self) -> None:
        send, receive = create_memory_object_stream[bytes](4)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            stream.feed_data(b"ab")
            await send.send(b"cdefgh")

            checkpoint = stream.checkpoint(budget=3)
            # First two bytes come from the pre-existing buffer
            assert await stream.receive_exactly(2) == b"ab"
            assert checkpoint.consumed == 2
            assert await stream.receive(1) == b"c"
            with pytest.raises(CheckpointBudgetExceeded):
                await stream.receive_exactly(1)

            checkpoint.rollback()
            assert await stream.receive_exactly(4) == b"abcd"

    async def test_budget_exceeded_with_oversized_object_chunk(self) -> None:
        send, receive = create_memory_object_stream[bytes](1)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcdef")

            checkpoint = stream.checkpoint(budget=2)
            with pytest.raises(CheckpointBudgetExceeded):
                await stream.receive(4)

            # No upstream data should have been pulled by the failed read
            assert checkpoint.consumed == 0
            checkpoint.rollback()
            assert await stream.receive_exactly(6) == b"abcdef"

    async def test_rollback_after_split_reads_no_upstream_reread(self) -> None:
        """The prefix spans two upstream chunks; after cancellation a normal parser
        gets the complete prefix from the buffer without re-reading upstream."""
        from anyio.abc import ByteReceiveStream

        send, receive = create_memory_object_stream[bytes](4)
        upstream_reads = 0

        class CountingByteReceiveStream(ByteReceiveStream):
            def __init__(self, wrapped: object) -> None:
                self._wrapped = wrapped

            async def receive(self, max_bytes: int = 65536) -> bytes:
                nonlocal upstream_reads
                upstream_reads += 1
                return await self._wrapped.receive()  # type: ignore[attr-defined]

            async def aclose(self) -> None:
                await self._wrapped.aclose()  # type: ignore[attr-defined]

            @property
            def extra_attributes(self) -> dict[object, object]:
                return {}

        async with send, receive:
            await send.send(b"ab")
            await send.send(b"cd")
            await send.send(b"rest")
            stream = BufferedByteReceiveStream(CountingByteReceiveStream(receive))

            # Speculative reader: the prefix needs two chunks to complete
            checkpoint = stream.checkpoint(budget=8)
            assert await stream.receive_exactly(2) == b"ab"
            assert await stream.receive_exactly(2) == b"cd"
            reads_before_rollback = upstream_reads
            assert reads_before_rollback == 2

            # Speculation fails: cancel and switch to a normal parser
            checkpoint.rollback()

            # The normal parser gets the whole prefix from the checkpoint buffer
            assert await stream.receive_exactly(4) == b"abcd"
            assert upstream_reads == reads_before_rollback
            assert await stream.receive_exactly(4) == b"rest"

    async def test_cancellation_does_not_lose_buffered_bytes(self) -> None:
        send, receive = create_memory_object_stream[bytes](2)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcd")

            checkpoint = stream.checkpoint(budget=8)
            assert await stream.receive_exactly(2) == b"ab"

            # A cancellation delivered at a checkpoint must not close the checkpoint
            # nor discard the bytes already pulled into it
            async with create_task_group() as tg:
                tg.cancel_scope.cancel()

            assert not checkpoint.closed
            checkpoint.rollback()
            assert await stream.receive_exactly(4) == b"abcd"

    async def test_cancellation_during_blocking_read_is_recoverable(self) -> None:
        send, receive = create_memory_object_stream[bytes](1)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            checkpoint = stream.checkpoint(budget=8)

            async def speculative_read() -> None:
                # Blocks until the second chunk arrives or the task is cancelled
                await stream.receive_exactly(4)

            async with create_task_group() as tg:
                tg.start_soon(speculative_read)
                await send.send(b"ab")
                await send.send(b"cd")
                tg.cancel_scope.cancel()

            # Cancellation does not close the checkpoint; rollback restores the
            # bytes that made it into the buffer before the cancellation point
            assert not checkpoint.closed
            checkpoint.rollback()
            assert await stream.receive_exactly(4) == b"abcd"

    async def test_eof_during_checkpoint_is_incomplete_read(self) -> None:
        send, receive = create_memory_object_stream[bytes](1)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"ab")
            await send.aclose()

            checkpoint = stream.checkpoint(budget=8)
            with pytest.raises(IncompleteRead):
                await stream.receive_exactly(4)

            # The bytes already read remain recoverable after the early EOF
            assert checkpoint.consumed == 2
            checkpoint.rollback()
            assert stream.buffer == b"ab"
            assert await stream.receive(2) == b"ab"
            with pytest.raises(EndOfStream):
                await stream.receive(1)

    async def test_budget_failure_takes_precedence_over_eof(self) -> None:
        send, receive = create_memory_object_stream[bytes](1)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.aclose()

            checkpoint = stream.checkpoint(budget=2)
            with pytest.raises(CheckpointBudgetExceeded):
                await stream.receive_exactly(4)

            assert not checkpoint.closed
            checkpoint.rollback()

    async def test_receive_until_under_checkpoint_rollback(self) -> None:
        send, receive = create_memory_object_stream[bytes](2)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abc\nrest")

            checkpoint = stream.checkpoint(budget=16)
            assert await stream.receive_until(b"\n", 10) == b"abc"
            assert checkpoint.consumed == 4
            checkpoint.rollback()

            assert await stream.receive_until(b"\n", 10) == b"abc"
            assert await stream.receive_exactly(4) == b"rest"

    async def test_receive_until_budget_exceeded(self) -> None:
        send, receive = create_memory_object_stream[bytes](2)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcdef")

            async with stream.checkpoint(budget=3):
                with pytest.raises(CheckpointBudgetExceeded):
                    await stream.receive_until(b"\n", 10)

            # Auto rollback: nothing was lost
            assert await stream.receive_exactly(6) == b"abcdef"

    async def test_receive_until_delimiter_not_found_under_checkpoint(self) -> None:
        send, receive = create_memory_object_stream[bytes](2)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcdef")

            checkpoint = stream.checkpoint(budget=16)
            with pytest.raises(DelimiterNotFound):
                await stream.receive_until(b"\n", 4)

            # DelimiterNotFound leaves the buffer intact
            assert not checkpoint.closed
            checkpoint.rollback()
            assert await stream.receive_exactly(6) == b"abcdef"

    async def test_checkpoint_on_closed_stream(self) -> None:
        send, receive = create_memory_object_stream[bytes](1)
        stream = BufferedByteReceiveStream(receive)
        await stream.aclose()
        with pytest.raises(ClosedResourceError):
            stream.checkpoint()

        send.close()
        receive.close()

    async def test_unlimited_budget(self) -> None:
        send, receive = create_memory_object_stream[bytes](4)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            await send.send(b"abcdefgh")

            checkpoint = stream.checkpoint()
            assert checkpoint.remaining is None
            assert await stream.receive_exactly(8) == b"abcdefgh"
            assert checkpoint.consumed == 8
            checkpoint.commit()

    async def test_rollback_orders_held_before_unconsumed_buffer(self) -> None:
        send, receive = create_memory_object_stream[bytes](4)
        async with send, receive:
            stream = BufferedByteReceiveStream(receive)
            stream.feed_data(b"abcdef")

            checkpoint = stream.checkpoint(budget=8)
            assert await stream.receive_exactly(2) == b"ab"
            # More data lands in the buffer while the checkpoint is open
            stream.feed_data(b"gh")
            assert await stream.receive_exactly(2) == b"cd"
            checkpoint.rollback()

            # The held bytes come back before the bytes that were never consumed
            assert await stream.receive_exactly(6) == b"abcdef"
            assert await stream.receive_exactly(2) == b"gh"

