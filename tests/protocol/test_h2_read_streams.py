from __future__ import annotations

import asyncio
from typing import Any

import h2.config
import h2.connection
import h2.errors
import h2.events
import pytest

from hypercorn.app_wrappers import ASGIWrapper
from hypercorn.asyncio.task_group import TaskGroup
from hypercorn.asyncio.worker_context import EventWrapper, WorkerContext
from hypercorn.config import Config
from hypercorn.events import Closed, RawData
from hypercorn.protocol.h2 import BufferCompleteError, H2Protocol, ReadStream
from hypercorn.typing import (
    ASGIReceiveCallable,
    ASGISendCallable,
    ConnectionState,
    Scope,
)

INITIAL_WINDOW = 65535


# ---------------------------------------------------------------------------
# ReadStream unit tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_read_stream_buffers_and_preserves_order() -> None:
    read_stream = ReadStream(EventWrapper)
    await read_stream.push(b"a", 1)
    await read_stream.push(b"bb", 2)
    assert read_stream.buffered_bytes == 3

    assert await read_stream.pop() == (b"a", 1)
    assert await read_stream.pop() == (b"bb", 2)
    assert read_stream.buffered_bytes == 0


@pytest.mark.asyncio
async def test_read_stream_pop_waits_for_data() -> None:
    loop = asyncio.get_running_loop()
    read_stream = ReadStream(EventWrapper)

    task = loop.create_task(read_stream.pop())
    await asyncio.sleep(0)
    assert not task.done()  # Nothing buffered yet.

    await read_stream.push(b"chunk", 5)
    assert await task == (b"chunk", 5)


@pytest.mark.asyncio
async def test_read_stream_pop_returns_none_after_complete() -> None:
    read_stream = ReadStream(EventWrapper)
    await read_stream.push(b"only", 4)
    await read_stream.push_complete()

    assert await read_stream.pop() == (b"only", 4)
    # The buffer drained after completion: EndBody may now be delivered.
    assert read_stream.complete
    assert await read_stream.pop() is None


@pytest.mark.asyncio
async def test_read_stream_complete_waits_for_drain() -> None:
    read_stream = ReadStream(EventWrapper)
    await read_stream.push(b"x", 1)
    await read_stream.push_complete()
    assert not read_stream.complete  # Still buffered.
    await read_stream.pop()
    assert read_stream.complete  # Drained.


@pytest.mark.asyncio
async def test_read_stream_push_after_complete_errors() -> None:
    read_stream = ReadStream(EventWrapper)
    await read_stream.push_complete()
    with pytest.raises(BufferCompleteError):
        await read_stream.push(b"late", 4)


@pytest.mark.asyncio
async def test_read_stream_close_unblocks_waiting_pop() -> None:
    loop = asyncio.get_running_loop()
    read_stream = ReadStream(EventWrapper)
    await read_stream.push(b"held", 4)

    task = loop.create_task(read_stream.pop())
    await asyncio.sleep(0)
    assert await task == (b"held", 4)

    waiting = loop.create_task(read_stream.pop())
    await asyncio.sleep(0)
    assert not waiting.done()
    await read_stream.close()
    assert await waiting is None
    assert read_stream.closed
    assert read_stream.buffered_bytes == 0


@pytest.mark.asyncio
async def test_read_stream_close_discards_backlog() -> None:
    read_stream = ReadStream(EventWrapper)
    await read_stream.push(b"a", 1)
    await read_stream.push(b"b", 1)
    await read_stream.close()
    assert read_stream.buffered_bytes == 0
    assert await read_stream.pop() is None


@pytest.mark.asyncio
async def test_read_stream_backlog_tracks_held_window() -> None:
    # With no consumer every pushed chunk stays in the read stream and its
    # tracked size equals the flow controlled window being held.
    read_stream = ReadStream(EventWrapper)
    total = 0
    for size in (100, 250, 7):
        await read_stream.push(b"x" * size, size)
        total += size
        assert read_stream.buffered_bytes == total
    assert sum(length for _, length in read_stream.buffer) == total


# ---------------------------------------------------------------------------
# Integration: per stream backpressure over a real in-process HTTP/2 link
# ---------------------------------------------------------------------------


class SharedState:
    def __init__(self) -> None:
        self.s1_first_chunk = asyncio.Event()
        self.s1_resume = asyncio.Event()
        self.received: dict[int, list[bytes]] = {}
        # Pre-register so tests can wait on any stream before its app starts.
        self.finished: dict[int, asyncio.Event] = {
            stream_id: asyncio.Event() for stream_id in (1, 3, 5)
        }

    def register(self, stream_id: int) -> asyncio.Event:
        return self.finished.setdefault(stream_id, asyncio.Event())


class ControllableApp:
    """Pauses stream 1 after its first body chunk; the others run freely."""

    def __init__(self, state: SharedState) -> None:
        self.state = state

    async def __call__(
        self, scope: Scope, receive: ASGIReceiveCallable, send: ASGISendCallable
    ) -> None:
        stream_id = int(dict(scope["headers"])[b"x-stream-id"])
        done = self.state.register(stream_id)
        chunks: list[bytes] = []
        while True:
            message = await receive()
            if message["type"] == "http.disconnect":
                return
            if message["body"]:
                chunks.append(message["body"])
            if (
                stream_id == 1
                and len(chunks) == 1
                and not self.state.s1_resume.is_set()
            ):
                self.state.s1_first_chunk.set()
                await self.state.s1_resume.wait()
            if not message.get("more_body", False):
                break

        self.state.received[stream_id] = chunks
        body = b"".join(chunks)
        await send(
            {
                "type": "http.response.start",
                "status": 200,
                "headers": [
                    (b"content-length", str(len(body)).encode()),
                    (b"x-stream-id", str(stream_id).encode()),
                ],
            }
        )
        await send({"type": "http.response.body", "body": body, "more_body": False})
        done.set()


class H2Harness:
    """A real H2Protocol driven over in-memory queues with a real h2 client."""

    def __init__(self, state: SharedState) -> None:
        self.loop = asyncio.get_running_loop()
        config = Config()
        # A small app queue makes the backpressure threshold reachable.
        config.max_app_queue_size = 2
        self.config = config
        self.server_to_client: asyncio.Queue[bytes] = asyncio.Queue()
        self.task_group = TaskGroup(self.loop)
        self.protocol = H2Protocol(
            ASGIWrapper(ControllableApp(state)),
            config,
            WorkerContext(None),
            self.task_group,
            ConnectionState({}),
            False,
            ("127.0.0.1", 1234),
            ("127.0.0.1", 4321),
            self._server_send,
        )
        self.client = h2.connection.H2Connection(
            config=h2.config.H2Configuration(client_side=True)
        )
        self.response_bodies: dict[int, bytearray] = {}
        self._pump_task: asyncio.Task | None = None
        self._pump_stop = asyncio.Event()

    async def _server_send(self, event: Any) -> None:
        if isinstance(event, RawData):
            await self.server_to_client.put(event.data)

    async def __aenter__(self) -> "H2Harness":
        await self.task_group.__aenter__()
        await self.protocol.initiate()
        self.client.initiate_connection()
        await self.protocol.handle(RawData(data=self.client.data_to_send()))
        await self.pump()
        return self

    async def __aexit__(self, *exc: Any) -> None:
        self.stop_background_pump()
        await self.protocol.handle(Closed())
        await self.pump()
        await self.task_group.__aexit__(*exc)

    async def _to_server(self, data: bytes) -> None:
        if data:
            await self.protocol.handle(RawData(data=data))

    async def start_background_pump(self) -> None:
        """Continuously shuttle frames so large flows drain without a manual
        pump between every send. Used by the window restoration test."""
        if self._pump_task is None:
            self._pump_task = self.loop.create_task(self._background_pump())

    def stop_background_pump(self) -> None:
        if self._pump_task is not None:
            self._pump_stop.set()

    async def _background_pump(self) -> None:
        while not self._pump_stop.is_set():
            await self._drain_frames()
            await asyncio.sleep(0)

    async def pump(self) -> None:
        """Run the connection tasks until all generated frames are exchanged.

        Yields first so the spawned read/app tasks run, then repeatedly drains
        the server -> client queue and returns client acks until no further
        frames appear. The drain loop matters because processing one frame can
        let the server emit another (e.g. a WINDOW_UPDATE) within the same pump.
        """
        await asyncio.sleep(0)
        for _ in range(100):
            delivered = await self._drain_frames()
            await asyncio.sleep(0)
            if not delivered and self.server_to_client.empty():
                break

    async def _drain_frames(self) -> bool:
        delivered = False
        while not self.server_to_client.empty():
            delivered = True
            events = self.client.receive_data(await self.server_to_client.get())
            for event in events:
                if isinstance(event, h2.events.DataReceived):
                    self.client.acknowledge_received_data(
                        event.flow_controlled_length, event.stream_id
                    )
                    self.response_bodies.setdefault(
                        event.stream_id, bytearray()
                    ).extend(event.data)
        data = self.client.data_to_send()
        if data:
            delivered = True
            await self._to_server(data)
        return delivered

    def request(self, stream_id: int) -> None:
        self.client.send_headers(
            stream_id,
            [
                (":method", "POST"),
                (":path", "/"),
                (":scheme", "http"),
                (":authority", "hypercorn"),
                ("x-stream-id", str(stream_id)),
            ],
        )

    async def send_data(self, stream_id: int, data: bytes) -> None:
        self.client.send_data(stream_id, data)
        await self._to_server(self.client.data_to_send())

    async def end_stream(self, stream_id: int) -> None:
        self.client.end_stream(stream_id)
        await self._to_server(self.client.data_to_send())

    def buffered(self, stream_id: int) -> int:
        read_stream = self.protocol.read_streams.get(stream_id)
        return 0 if read_stream is None else read_stream.buffered_bytes

    def held_window(self, stream_id: int) -> int:
        """Flow controlled bytes received but not yet acknowledged."""
        read_stream = self.protocol.read_streams.get(stream_id)
        if read_stream is None:
            return 0
        return sum(length for _, length in read_stream.buffer)

    def server_inbound_window(self, stream_id: int) -> int:
        """The h2 inbound flow window the server still has for the stream.

        Shrinks as data is received and is only restored when h2 emits a
        WINDOW_UPDATE (which it batches per RFC 9113 until the window reaches
        zero or half its size).
        """
        stream = self.protocol.connection._get_stream_by_id(stream_id)
        return stream.inbound_flow_control_window

    def server_processed_unacknowledged(self, stream_id: int) -> int:
        """Bytes the app consumed but h2 has not yet returned via WINDOW_UPDATE."""
        stream = self.protocol.connection._get_stream_by_id(stream_id)
        return stream._inbound_window_manager._bytes_processed

    async def wait_for(self, predicate: Any, timeout: float = 2.0) -> None:
        async def _poll() -> None:
            while not predicate():
                await self.pump()

        await asyncio.wait_for(_poll(), timeout)


@pytest.mark.asyncio
async def test_per_stream_backpressure_independence_and_ordering() -> None:
    state = SharedState()
    async with H2Harness(state) as harness:
        # -- Stream 1 reads one chunk then the app pauses --------------------
        harness.request(1)
        await harness.send_data(1, b"s1-a")
        await harness.wait_for(state.s1_first_chunk.is_set)
        assert harness.buffered(1) == 0
        # The single consumed chunk was acknowledged: the held read buffer is
        # empty and the server accounts for the processed byte.
        assert harness.server_processed_unacknowledged(1) == len(b"s1-a")

        # Finish stream 1's body and end event while it is still paused.
        remaining = [b"s1-b", b"s1-c", b"s1-d"]
        for chunk in remaining:
            await harness.send_data(1, chunk)
        await harness.end_stream(1)

        # The surplus that does not fit in the app queue is held in the read
        # stream: bounded per stream by the h2 window, observable directly, and
        # its window has not been returned to the client. Asserted before
        # pumping so the read task has not moved the data on.
        total_remaining = sum(map(len, remaining))
        backlog = harness.buffered(1)
        assert backlog == total_remaining
        assert 0 < backlog <= INITIAL_WINDOW
        assert harness.held_window(1) == backlog
        # The inbound flow window reflects every received, un-acked byte.
        assert harness.server_inbound_window(1) == INITIAL_WINDOW - (
            len(b"s1-a") + total_remaining
        )

        # -- Stream 3 has full credit and finishes on its own ----------------
        harness.request(3)
        await harness.send_data(3, b"s3-a")
        await harness.send_data(3, b"s3-b")
        await harness.end_stream(3)
        await harness.wait_for(state.finished[3].is_set)
        assert state.received[3] == [b"s3-a", b"s3-b"]
        assert harness.buffered(3) == 0

        # The fast stream finished while stream 1 was still blocked.
        assert state.finished[3].is_set()
        assert not state.s1_resume.is_set()

        # -- Resume stream 1; the backlog drains in the correct order --------
        state.s1_resume.set()
        await harness.wait_for(state.finished[1].is_set)
        # EndBody was only delivered after every preceding Body was readable.
        assert state.received[1] == [b"s1-a", b"s1-b", b"s1-c", b"s1-d"]
        assert harness.buffered(1) == 0
        # Every received byte has now been acknowledged to h2. Below the RFC
        # batching threshold it is held as processed-unacknowledged rather than
        # emitted as a WINDOW_UPDATE, so the inbound window stays reduced but no
        # data remains buffered.
        total_body = sum(map(len, state.received[1]))
        await harness.wait_for(
            lambda: harness.server_processed_unacknowledged(1) == total_body
        )
        assert harness.server_inbound_window(1) == INITIAL_WINDOW - total_body

        # -- Stream 5 arrives while stream 1 is still settling ---------------
        harness.request(5)
        await harness.send_data(5, b"s5-a")
        await harness.send_data(5, b"s5-b")
        await harness.end_stream(5)
        await harness.wait_for(state.finished[5].is_set)
        assert state.received[5] == [b"s5-a", b"s5-b"]
        assert harness.buffered(5) == 0

        # Both fast streams ended independently; responses echo the full body.
        await harness.pump()
        assert state.finished[3].is_set() and state.finished[5].is_set()
        assert bytes(harness.response_bodies[3]) == b"s3-as3-b"
        assert bytes(harness.response_bodies[5]) == b"s5-as5-b"
        assert bytes(harness.response_bodies[1]) == b"s1-as1-bs1-cs1-d"


@pytest.mark.asyncio
async def test_flow_window_returned_when_app_consumes_a_large_body() -> None:
    """Consuming a body past the half-window point returns credit per stream.

    h2 batches WINDOW_UPDATE until the stream window reaches zero or half its
    size. A body larger than that threshold must therefore produce real
    WINDOW_UPDATE frames that restore the client's sending window, proving the
    inbound window limit is continued rather than bypassed.
    """
    state = SharedState()
    async with H2Harness(state) as harness:
        # Stream 7: the controllable app only pauses stream 1, so this one drains.
        stream_id = 7
        chunk_size = 2000
        chunks = [b"z" * chunk_size for _ in range(17)]  # 34000 > half window
        body = b"".join(chunks)

        await harness.start_background_pump()
        harness.request(stream_id)
        for chunk in chunks:
            await harness.send_data(stream_id, chunk)
            await asyncio.sleep(0)
        await harness.end_stream(stream_id)

        await asyncio.wait_for(state.finished[stream_id].wait(), timeout=2)
        harness.stop_background_pump()
        await harness.pump()

        assert b"".join(state.received[stream_id]) == body
        # The threshold was crossed so h2 emitted at least one WINDOW_UPDATE and
        # the client recovered most of its sending window on this stream.
        assert harness.client.remote_flow_control_window(stream_id) > (
            INITIAL_WINDOW - chunk_size
        )

@pytest.mark.asyncio
async def test_stream_reset_does_not_block_other_streams() -> None:
    state = SharedState()
    async with H2Harness(state) as harness:
        try:
            harness.request(1)
            await harness.send_data(1, b"s1-a")
            await harness.wait_for(state.s1_first_chunk.is_set)
            for index in range(8):
                await harness.send_data(1, f"backlog-{index}".encode())

            harness.client.reset_stream(1, h2.errors.ErrorCodes.CANCEL)
            await asyncio.wait_for(
                harness._to_server(harness.client.data_to_send()), timeout=1
            )
            assert harness.buffered(1) == 0

            harness.request(3)
            await harness.send_data(3, b"s3-a")
            await harness.send_data(3, b"s3-b")
            await harness.end_stream(3)
            await asyncio.wait_for(harness.wait_for(state.finished[3].is_set), timeout=2)
            assert state.received[3] == [b"s3-a", b"s3-b"]
        finally:
            # Let the paused app observe disconnect so task group shutdown
            # does not wait on a consumer that will never resume.
            state.s1_resume.set()
            for _ in range(5):
                await harness.pump()
