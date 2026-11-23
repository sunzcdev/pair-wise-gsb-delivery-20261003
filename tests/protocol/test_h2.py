from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock, call, Mock

import h2.events
import pytest
from h2.connection import H2Connection
from h2.events import ConnectionTerminated

from hypercorn.asyncio.worker_context import EventWrapper, WorkerContext
from hypercorn.config import Config
from hypercorn.events import Closed, RawData
from hypercorn.protocol.events import EndBody, Response, StreamClosed
from hypercorn.protocol.h2 import (
    BUFFER_HIGH_WATER,
    BufferCompleteError,
    ConnectionBufferLimit,
    H2Protocol,
    StreamBuffer,
)
from hypercorn.typing import ConnectionState


@pytest.fixture(name="connection_limit")
def _connection_limit() -> ConnectionBufferLimit:
    return ConnectionBufferLimit(EventWrapper, BUFFER_HIGH_WATER)


@pytest.mark.asyncio
async def test_stream_buffer_push_and_pop() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()

    # A generous connection limit so this test exercises only the per-stream
    # high/low water backpressure.
    stream_buffer = StreamBuffer(EventWrapper, ConnectionBufferLimit(EventWrapper, 10**9))

    async def _push_over_limit() -> bool:
        await stream_buffer.push(b"a" * (BUFFER_HIGH_WATER + 1))
        return True

    task = event_loop.create_task(_push_over_limit())
    # Let the producer run until it blocks on the high water mark, so the
    # buffer is actually populated before we start popping from it.
    await asyncio.sleep(0)
    assert not task.done()  # Blocked as over high water
    first = await stream_buffer.pop(BUFFER_HIGH_WATER // 4)
    assert not task.done()  # Blocked as over low water
    second = await stream_buffer.pop(BUFFER_HIGH_WATER // 4)
    assert (await task) is True
    # Every popped byte has been released back to the connection wide budget,
    # i.e. the queued total equals exactly what remains buffered.
    assert stream_buffer._connection_limit.queued == len(stream_buffer.buffer)
    assert len(first) + len(second) == BUFFER_HIGH_WATER // 2


@pytest.mark.asyncio
async def test_stream_buffer_drain(connection_limit: ConnectionBufferLimit) -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()

    stream_buffer = StreamBuffer(EventWrapper, connection_limit)
    await stream_buffer.push(b"a" * 10)

    async def _drain() -> bool:
        await stream_buffer.drain()
        return True

    task = event_loop.create_task(_drain())
    assert not task.done()  # Blocked
    await stream_buffer.pop(20)
    assert (await task) is True


@pytest.mark.asyncio
async def test_stream_buffer_closed(connection_limit: ConnectionBufferLimit) -> None:
    stream_buffer = StreamBuffer(EventWrapper, connection_limit)
    await stream_buffer.push(b"a" * 10)
    assert connection_limit.queued == 10
    await stream_buffer.close()
    await stream_buffer._is_empty.wait()
    await stream_buffer._paused.wait()
    assert True
    # Closing returns the budget held by the never-sent bytes.
    assert connection_limit.queued == 0
    with pytest.raises(BufferCompleteError):
        await stream_buffer.push(b"a")
    # Closing a second time is a no-op (no double release).
    await stream_buffer.close()
    assert connection_limit.queued == 0


@pytest.mark.asyncio
async def test_stream_buffer_complete(connection_limit: ConnectionBufferLimit) -> None:
    stream_buffer = StreamBuffer(EventWrapper, connection_limit)
    await stream_buffer.push(b"a" * 10)
    assert not stream_buffer.complete
    stream_buffer.set_complete()
    assert not stream_buffer.complete
    await stream_buffer.pop(20)
    assert stream_buffer.complete


@pytest.mark.asyncio
async def test_connection_buffer_limit_blocks_until_released() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()
    limit = ConnectionBufferLimit(EventWrapper, 100)

    async def _reserve(size: int) -> bool:
        await limit.reserve(size)
        return True

    await limit.reserve(60)
    assert limit.queued == 60
    task = event_loop.create_task(_reserve(60))
    # 60 + 60 > 100, the reservation must wait.
    await asyncio.sleep(0)
    assert not task.done()
    await limit.release(30)  # queued drops to 30, room for another 60
    assert (await task) is True
    assert limit.queued == 90
    await limit.release(90)
    assert limit.queued == 0


@pytest.mark.asyncio
async def test_connection_buffer_limit_shared_across_streams() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()
    limit = ConnectionBufferLimit(EventWrapper, 100)
    buffer_a = StreamBuffer(EventWrapper, limit)
    buffer_b = StreamBuffer(EventWrapper, limit)

    async def _push(buffer: StreamBuffer, size: int) -> bool:
        await buffer.push(b"a" * size)
        return True

    await buffer_a.push(b"a" * 60)
    # Stream A holds 60 of the shared 100 byte budget, so stream B can only
    # reserve the remaining 40 before being blocked.
    task_b = event_loop.create_task(_push(buffer_b, 60))
    await asyncio.sleep(0)
    assert not task_b.done()
    assert limit.queued == 60

    # Popping from A frees shared budget and lets B make progress.
    await buffer_a.pop(60)
    assert (await task_b) is True
    assert limit.queued == 60  # B now holds the budget
    assert len(buffer_b.buffer) == 60

    # Resetting B returns its unsent budget.
    await buffer_b.close()
    assert limit.queued == 0


@pytest.mark.asyncio
async def test_connection_buffer_limit_large_push_drains_then_succeeds() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()
    limit = ConnectionBufferLimit(EventWrapper, 50)
    stream_buffer = StreamBuffer(EventWrapper, limit)
    await stream_buffer.push(b"a" * 50)
    assert limit.exhausted

    async def _push() -> bool:
        # A single push larger than the whole budget must not deadlock; it
        # waits for the buffer to drain and then succeeds.
        await stream_buffer.push(b"b" * 80)
        return True

    task = event_loop.create_task(_push())
    await asyncio.sleep(0)
    assert not task.done()
    await stream_buffer.pop(50)
    assert (await task) is True
    assert len(stream_buffer.buffer) == 80
    assert limit.queued == 80


@pytest.mark.asyncio
async def test_protocol_handle_protocol_error() -> None:
    protocol = H2Protocol(
        Mock(),
        Config(),
        WorkerContext(None),
        AsyncMock(),
        ConnectionState({}),
        False,
        None,
        None,
        AsyncMock(),
    )
    await protocol.handle(RawData(data=b"broken nonsense\r\n\r\n"))
    protocol.send.assert_awaited()  # type: ignore
    assert protocol.send.call_args_list == [call(Closed())]  # type: ignore


@pytest.mark.asyncio
async def test_protocol_keep_alive_max_requests() -> None:
    protocol = H2Protocol(
        Mock(),
        Config(),
        WorkerContext(None),
        AsyncMock(),
        ConnectionState({}),
        False,
        None,
        None,
        AsyncMock(),
    )
    protocol.config.keep_alive_max_requests = 0
    client = H2Connection()
    client.initiate_connection()
    headers = [
        (":method", "GET"),
        (":path", "/reqinfo"),
        (":authority", "hypercorn"),
        (":scheme", "https"),
    ]
    client.send_headers(1, headers, end_stream=True)
    await protocol.handle(RawData(data=client.data_to_send()))
    protocol.send.assert_awaited()  # type: ignore
    events = client.receive_data(protocol.send.call_args_list[1].args[0].data)  # type: ignore
    assert isinstance(events[-1], ConnectionTerminated)


def _build_protocol(connection_buffer_size: int) -> H2Protocol:
    config = Config()
    config.h2_connection_buffer_size = connection_buffer_size
    return H2Protocol(
        Mock(),
        config,
        WorkerContext(None),
        AsyncMock(),
        ConnectionState({}),
        False,
        None,
        None,
        AsyncMock(),
    )


def _client_request(client: H2Connection, stream_id: int) -> bytes:
    headers = [
        (":method", "GET"),
        (":path", f"/{stream_id}"),
        (":authority", "hypercorn"),
        (":scheme", "https"),
    ]
    client.send_headers(stream_id, headers, end_stream=True)
    return client.data_to_send()


async def _open_stream(
    protocol: H2Protocol, client: H2Connection, stream_id: int
) -> StreamBuffer:
    await protocol.handle(RawData(data=_client_request(client, stream_id)))
    # Answer the request directly without involving the ASGI app.
    await protocol.stream_send(
        Response(stream_id=stream_id, status_code=200, headers=[])
    )
    return protocol.stream_buffers[stream_id]


async def _drain_client(protocol: H2Protocol, client: H2Connection) -> list[object]:
    received: list[object] = []
    for sent in protocol.send.call_args_list:  # type: ignore
        event = sent.args[0]
        if isinstance(event, RawData):
            received.extend(client.receive_data(event.data))
    return received


@pytest.mark.asyncio
async def test_protocol_connection_buffer_is_bounded_across_streams() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()
    protocol = _build_protocol(connection_buffer_size=100)
    client = H2Connection()
    client.initiate_connection()
    await protocol.handle(RawData(data=client.data_to_send()))
    protocol.send.reset_mock()  # type: ignore

    buffer_a = await _open_stream(protocol, client, 1)
    buffer_b = await _open_stream(protocol, client, 3)

    async def _push(buffer: StreamBuffer, size: int) -> None:
        await buffer.push(b"a" * size)

    # Fill the shared budget with stream A.
    push_a = event_loop.create_task(_push(buffer_a, 100))
    await asyncio.sleep(0)
    assert push_a.done()
    assert protocol.connection_buffer_limit.queued == 100

    # Stream B can no longer reserve any room and must wait, even though B's
    # own per-stream high water mark is nowhere near reached.
    push_b = event_loop.create_task(_push(buffer_b, 40))
    await asyncio.sleep(0)
    assert not push_b.done()
    assert protocol.connection_buffer_limit.queued == 100

    # Releasing A's queued bytes lets B proceed; the total stays bounded.
    await buffer_a.pop(100)
    await push_b
    assert protocol.connection_buffer_limit.queued == 40
    await buffer_b.pop(40)
    assert protocol.connection_buffer_limit.queued == 0


@pytest.mark.asyncio
async def test_protocol_zero_window_stream_does_not_block_others() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()
    protocol = _build_protocol(connection_buffer_size=1024)
    client = H2Connection()
    client.initiate_connection()
    await protocol.handle(RawData(data=client.data_to_send()))
    protocol.send.reset_mock()  # type: ignore

    buffer_1 = await _open_stream(protocol, client, 1)
    buffer_3 = await _open_stream(protocol, client, 3)

    await buffer_1.push(b"x" * 64)
    await buffer_3.push(b"y" * 64)

    # Exhaust stream 1's flow control window so it cannot send, while stream
    # 3 keeps its default window.
    h2_stream_1 = protocol.connection._get_stream_by_id(1)
    h2_stream_1.outbound_flow_control_window = 0

    # The stalled stream is blocked and returns promptly instead of
    # busy-spinning or stalling the sending task.
    send_1 = event_loop.create_task(protocol._send_data(1))
    await asyncio.sleep(0)
    assert send_1.done()
    assert not protocol.priority._streams[1].active

    # Stream 3 still has a window and must be able to make progress even
    # though stream 1 is stalled.
    await protocol._send_data(3)
    assert len(buffer_3.buffer) == 0
    frames = await _drain_client(protocol, client)
    data_events = [e for e in frames if isinstance(e, h2.events.DataReceived)]
    assert any(e.stream_id == 3 for e in data_events)
    assert all(e.stream_id != 1 for e in data_events)


@pytest.mark.asyncio
async def test_protocol_reset_returns_budget_and_stops_sending() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()
    protocol = _build_protocol(connection_buffer_size=100)
    client = H2Connection()
    client.initiate_connection()
    await protocol.handle(RawData(data=client.data_to_send()))
    protocol.send.reset_mock()  # type: ignore

    buffer_1 = await _open_stream(protocol, client, 1)
    buffer_3 = await _open_stream(protocol, client, 3)

    # Stream 1 fills most of the connection budget.
    await buffer_1.push(b"a" * 70)
    blocked = event_loop.create_task(buffer_3.push(b"b" * 60))
    await asyncio.sleep(0)
    assert not blocked.done()  # Only 30 bytes of the 100 byte budget remain.

    # The client resets stream 1.
    client.reset_stream(1)
    await protocol.handle(RawData(data=client.data_to_send()))

    # The unsent bytes of stream 1 are dropped and their budget returned,
    # letting stream 3's push complete.
    await blocked
    assert protocol.connection_buffer_limit.queued == 60
    assert 1 not in protocol.stream_buffers
    assert 1 not in protocol.streams

    # Stream 3 completes normally and its data is actually delivered. Send
    # the buffered data first (draining the buffer), then mark the stream
    # complete so its end-of-stream frame is emitted.
    await protocol._send_data(3)
    await protocol.stream_send(EndBody(stream_id=3))
    frames = await _drain_client(protocol, client)
    data_events = [e for e in frames if isinstance(e, h2.events.DataReceived)]
    assert all(e.stream_id != 1 for e in data_events)
    assert any(e.stream_id == 3 for e in data_events)
    assert protocol.connection_buffer_limit.queued == 0


@pytest.mark.asyncio
async def test_protocol_interleaved_window_updates_and_reset() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()
    protocol = _build_protocol(connection_buffer_size=200)
    client = H2Connection()
    client.initiate_connection()
    await protocol.handle(RawData(data=client.data_to_send()))
    protocol.send.reset_mock()  # type: ignore

    buffer_1 = await _open_stream(protocol, client, 1)
    buffer_3 = await _open_stream(protocol, client, 3)

    # Pause stream 1 by draining its window.
    await buffer_1.push(b"a" * 80)
    h2_stream_1 = protocol.connection._get_stream_by_id(1)
    h2_stream_1.outbound_flow_control_window = 0
    await protocol._send_data(1)
    assert not protocol.priority._streams[1].active

    # Stream 2 (id 3) keeps feeding multiple chunks through the shared budget.
    for _ in range(3):
        await buffer_3.push(b"y" * 40)
        await protocol._send_data(3)
    assert len(buffer_3.buffer) == 0

    # Reopen stream 1's window with a WINDOW_UPDATE from the client.
    client.increment_flow_control_window(80, stream_id=1)
    await protocol.handle(RawData(data=client.data_to_send()))
    assert protocol.priority._streams[1].active
    await protocol._send_data(1)
    assert len(buffer_1.buffer) == 0

    # The queued total never exceeded the connection wide budget at any point.
    assert protocol.connection_buffer_limit.queued == 0

    # Now reset stream 3 after it has delivered everything; nothing more is
    # sent for it and the budget remains balanced.
    await protocol.stream_send(StreamClosed(stream_id=3))
    assert 3 not in protocol.stream_buffers
    assert protocol.connection_buffer_limit.queued == 0

