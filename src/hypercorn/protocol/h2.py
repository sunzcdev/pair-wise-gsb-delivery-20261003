from __future__ import annotations

from collections.abc import Awaitable, Callable

import h2
import h2.connection
import h2.events
import h2.exceptions
import priority

from .events import (
    Body,
    Data,
    EndBody,
    EndData,
    Event as StreamEvent,
    InformationalResponse,
    Request,
    Response,
    StreamClosed,
    Trailers,
)
from .http_stream import HTTPStream
from .ws_stream import WSStream
from ..config import Config
from ..events import Closed, Event, RawData, Updated
from ..typing import AppWrapper, ConnectionState, Event as IOEvent, TaskGroup, WorkerContext
from ..utils import filter_pseudo_headers

# Per-stream high/low water marks. These bound how much a single stream
# is allowed to buffer before the coroutine producing the body is paused.
BUFFER_HIGH_WATER = 2 * 2**14  # Twice the default max frame size (two frames worth)
BUFFER_LOW_WATER = BUFFER_HIGH_WATER / 2


class BufferCompleteError(Exception):
    pass


class ConnectionBufferLimit:
    """Connection wide bound on the total bytes queued across all streams.

    The per-stream :class:`StreamBuffer` limits only bound how much a
    single stream may buffer. With many concurrent streams their buffers
    can still add up to an unbounded amount of memory. This limit is
    shared between every stream on a connection so that the actual number
    of bytes awaiting transmission is always bounded, regardless of how
    many streams are active.

    Bytes are *reserved* when they enter a stream buffer and *released*
    either as they are handed to h2 for transmission or when the owning
    stream is reset/closed. A stream whose flow control window is
    exhausted therefore holds its reservation without blocking the other
    streams, which continue to consume and release the shared budget.
    """

    def __init__(self, event_class: type[IOEvent], max_size: int) -> None:
        self.max_size = max_size
        self.queued = 0
        self._available = event_class()

    async def reserve(self, size: int) -> None:
        # Wait until the shared budget has room. A single push that is itself
        # larger than the limit is allowed through once the existing queue has
        # drained (it temporarily exceeds the steady state bound, matching the
        # per-stream behaviour), rather than deadlocking forever.
        #
        # Level triggered: clear only immediately before waiting and
        # re-check the budget after every wake-up, so a release signal can
        # never be lost to a stale wake-up.
        while self.queued > 0 and self.queued + size > self.max_size:
            await self._available.clear()
            await self._available.wait()
        self.queued += size

    async def release(self, size: int) -> None:
        self.queued -= size
        if self.queued < 0:
            self.queued = 0
        # Signal any waiting producer that budget may have freed up.
        await self._available.set()

    @property
    def exhausted(self) -> bool:
        return self.queued >= self.max_size


class StreamBuffer:
    def __init__(
        self, event_class: type[IOEvent], connection_limit: ConnectionBufferLimit
    ) -> None:
        self.buffer = bytearray()
        self._complete = False
        self._closed = False
        self._connection_limit = connection_limit
        self._is_empty = event_class()
        self._paused = event_class()

    async def drain(self) -> None:
        await self._is_empty.wait()

    def set_complete(self) -> None:
        self._complete = True

    async def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        self._complete = True
        # Return any budget still held by data that was never handed to
        # h2. Frames already given to h2 keep flowing per the protocol and
        # are not accounted for here.
        if len(self.buffer) > 0:
            await self._connection_limit.release(len(self.buffer))
        self.buffer = bytearray()
        await self._is_empty.set()
        await self._paused.set()

    @property
    def complete(self) -> bool:
        return self._complete and len(self.buffer) == 0

    async def push(self, data: bytes) -> None:
        if self._complete:
            raise BufferCompleteError()
        # First reserve against the connection wide budget, so that one
        # stalled stream cannot starve the others of memory.
        await self._connection_limit.reserve(len(data))
        self.buffer.extend(data)
        await self._is_empty.clear()
        if len(self.buffer) >= BUFFER_HIGH_WATER:
            await self._paused.wait()
            await self._paused.clear()

    async def pop(self, max_length: int) -> bytes:
        length = min(len(self.buffer), max_length)
        data = bytes(self.buffer[:length])
        del self.buffer[:length]
        # The bytes have left the buffer and are now h2's responsibility,
        # so release the shared reservation they held.
        if length > 0:
            await self._connection_limit.release(length)
        if len(data) < BUFFER_LOW_WATER:
            await self._paused.set()
        if len(self.buffer) == 0:
            await self._is_empty.set()
        return data


class H2Protocol:
    def __init__(
        self,
        app: AppWrapper,
        config: Config,
        context: WorkerContext,
        task_group: TaskGroup,
        connection_state: ConnectionState,
        ssl: bool,
        client: tuple[str, int] | None,
        server: tuple[str, int] | None,
        send: Callable[[Event], Awaitable[None]],
    ) -> None:
        self.app = app
        self.client = client
        self.closed = False
        self.config = config
        self.context = context
        self.task_group = task_group
        self.connection_state = connection_state

        self.connection = h2.connection.H2Connection(
            config=h2.config.H2Configuration(client_side=False, header_encoding=None)
        )
        self.connection.DEFAULT_MAX_INBOUND_FRAME_SIZE = config.h2_max_inbound_frame_size
        self.connection.local_settings = h2.settings.Settings(
            client=False,
            initial_values={
                h2.settings.SettingCodes.MAX_CONCURRENT_STREAMS: config.h2_max_concurrent_streams,
                h2.settings.SettingCodes.MAX_HEADER_LIST_SIZE: config.h2_max_header_list_size,
                h2.settings.SettingCodes.ENABLE_CONNECT_PROTOCOL: 1,
            },
        )

        self.keep_alive_requests = 0
        self.send = send
        self.server = server
        self.ssl = ssl
        self.streams: dict[int, HTTPStream | WSStream] = {}
        # The below are used by the sending task
        self.has_data = self.context.event_class()
        self.priority = priority.PriorityTree()
        self.stream_buffers: dict[int, StreamBuffer] = {}
        # Shared, connection wide bound on the bytes queued in all the
        # stream buffers combined.
        self.connection_buffer_limit = ConnectionBufferLimit(
            self.context.event_class, config.h2_connection_buffer_size
        )

    @property
    def idle(self) -> bool:
        return len(self.streams) == 0 or all(stream.idle for stream in self.streams.values())

    async def initiate(
        self, headers: list[tuple[bytes, bytes]] | None = None, settings: bytes | None = None
    ) -> None:
        if settings is not None:
            self.connection.initiate_upgrade_connection(settings)
        else:
            self.connection.initiate_connection()
        await self._flush()
        if headers is not None:
            event = h2.events.RequestReceived(stream_id=1, headers=headers)
            await self._create_stream(event)
            await self.streams[event.stream_id].handle(EndBody(stream_id=event.stream_id))
        self.task_group.spawn(self.send_task)

    async def send_task(self) -> None:
        # This should be run in a separate task to the rest of this
        # class. This allows it separately choose when to send,
        # crucially in what order.
        while not self.closed:
            try:
                stream_id = next(self.priority)
            except priority.DeadlockError:
                await self.has_data.wait()
                await self.has_data.clear()
            else:
                await self._send_data(stream_id)

    async def _send_data(self, stream_id: int) -> None:
        try:
            flow_window = self.connection.local_flow_control_window(stream_id)
            if flow_window <= 0:
                # This stream has exhausted its flow control window. Block
                # it so that the sending task moves on to the other streams
                # instead of busy spinning here. It is unblocked again when
                # a WINDOW_UPDATE arrives.
                self.priority.block(stream_id)
                return

            chunk_size = min(flow_window, self.connection.max_outbound_frame_size)
            chunk_size = max(0, chunk_size)
            data = await self.stream_buffers[stream_id].pop(chunk_size)
            if data:
                self.connection.send_data(stream_id, data)
                await self._flush()
            else:
                self.priority.block(stream_id)

            if self.stream_buffers[stream_id].complete:
                self.connection.end_stream(stream_id)
                await self._flush()
                del self.stream_buffers[stream_id]
                self.priority.remove_stream(stream_id)
        except (h2.exceptions.StreamClosedError, KeyError, h2.exceptions.ProtocolError):
            # Stream or connection has closed whilst waiting to send
            # data, not a problem - just force close it.
            stream_buffer = self.stream_buffers.pop(stream_id, None)
            if stream_buffer is not None:
                await stream_buffer.close()
            try:
                self.priority.remove_stream(stream_id)
            except priority.MissingStreamError:
                pass

    async def handle(self, event: Event) -> None:
        if isinstance(event, RawData):
            try:
                events = self.connection.receive_data(event.data)
            except h2.exceptions.ProtocolError:
                await self._flush()
                await self.send(Closed())
            else:
                await self._handle_events(events)
        elif isinstance(event, Closed):
            self.closed = True
            stream_ids = list(self.streams.keys())
            for stream_id in stream_ids:
                await self._close_stream(stream_id)
            await self.has_data.set()

    async def stream_send(self, event: StreamEvent) -> None:
        try:
            if isinstance(event, (InformationalResponse, Response)):
                self.connection.send_headers(
                    event.stream_id,
                    [(b":status", b"%d" % event.status_code)]
                    + event.headers
                    + self.config.response_headers("h2"),
                )
                await self._flush()
            elif isinstance(event, (Body, Data)):
                self.priority.unblock(event.stream_id)
                await self.has_data.set()
                await self.stream_buffers[event.stream_id].push(event.data)
            elif isinstance(event, (EndBody, EndData)):
                self.stream_buffers[event.stream_id].set_complete()
                self.priority.unblock(event.stream_id)
                await self.has_data.set()
                await self.stream_buffers[event.stream_id].drain()
            elif isinstance(event, Trailers):
                self.priority.unblock(event.stream_id)
                await self.has_data.set()
                await self.stream_buffers[event.stream_id].drain()
                self.connection.send_headers(event.stream_id, event.headers, end_stream=True)
                await self._flush()
            elif isinstance(event, StreamClosed):
                await self._close_stream(event.stream_id)
                idle = len(self.streams) == 0 or all(
                    stream.idle for stream in self.streams.values()
                )
                if idle and self.context.terminated.is_set():
                    self.connection.close_connection()
                    await self._flush()
                await self.send(Updated(idle=idle))
            elif isinstance(event, Request):
                await self._create_server_push(event.stream_id, event.raw_path, event.headers)
        except (
            BufferCompleteError,
            KeyError,
            priority.MissingStreamError,
            h2.exceptions.ProtocolError,
        ):
            # Connection has closed whilst blocked on flow control or
            # connection has advanced ahead of the last emitted event.
            return

    async def _handle_events(self, events: list[h2.events.Event]) -> None:
        for event in events:
            if isinstance(event, h2.events.RequestReceived):
                if self.context.terminated.is_set():
                    self.connection.reset_stream(event.stream_id)
                    self.connection.update_settings(
                        {h2.settings.SettingCodes.MAX_CONCURRENT_STREAMS: 0}
                    )
                else:
                    await self._create_stream(event)
                    await self.send(Updated(idle=False))

                if self.keep_alive_requests > self.config.keep_alive_max_requests:
                    self.connection.close_connection()
            elif isinstance(event, h2.events.DataReceived):
                await self.streams[event.stream_id].handle(
                    Body(stream_id=event.stream_id, data=event.data)
                )
                self.connection.acknowledge_received_data(
                    event.flow_controlled_length, event.stream_id
                )
            elif isinstance(event, h2.events.StreamEnded):
                try:
                    await self.streams[event.stream_id].handle(EndBody(stream_id=event.stream_id))
                except KeyError:
                    # Response sent before full request received,
                    # nothing to do already closed.
                    pass
            elif isinstance(event, h2.events.StreamReset):
                await self._close_stream(event.stream_id)
                await self._window_updated(event.stream_id)
            elif isinstance(event, h2.events.WindowUpdated):
                await self._window_updated(event.stream_id)
            elif isinstance(event, h2.events.PriorityUpdated):
                await self._priority_updated(event)
            elif isinstance(event, h2.events.RemoteSettingsChanged):
                if h2.settings.SettingCodes.INITIAL_WINDOW_SIZE in event.changed_settings:
                    await self._window_updated(None)
            elif isinstance(event, h2.events.ConnectionTerminated):
                await self.send(Closed())
        await self._flush()

    async def _flush(self) -> None:
        data = self.connection.data_to_send()
        if data != b"":
            await self.send(RawData(data=data))

    async def _window_updated(self, stream_id: int | None) -> None:
        if stream_id is None or stream_id == 0:
            # Unblock all streams
            for stream_id in list(self.stream_buffers.keys()):
                self.priority.unblock(stream_id)
        elif stream_id is not None and stream_id in self.stream_buffers:
            self.priority.unblock(stream_id)
        await self.has_data.set()

    async def _priority_updated(self, event: h2.events.PriorityUpdated) -> None:
        try:
            self.priority.reprioritize(
                stream_id=event.stream_id,
                depends_on=event.depends_on or None,
                weight=event.weight,
                exclusive=event.exclusive,
            )
        except priority.MissingStreamError:
            # Received PRIORITY frame before HEADERS frame
            self.priority.insert_stream(
                stream_id=event.stream_id,
                depends_on=event.depends_on or None,
                weight=event.weight,
                exclusive=event.exclusive,
            )
            self.priority.block(event.stream_id)
        await self.has_data.set()

    async def _create_stream(self, request: h2.events.RequestReceived) -> None:
        for name, value in request.headers:
            if name == b":method":
                method = value.decode("ascii").upper()
            elif name == b":path":
                raw_path = value

        if method == "CONNECT":
            self.streams[request.stream_id] = WSStream(
                self.app,
                self.config,
                self.context,
                self.task_group,
                self.ssl,
                self.client,
                self.server,
                self.stream_send,
                request.stream_id,
            )
        else:
            self.streams[request.stream_id] = HTTPStream(
                self.app,
                self.config,
                self.context,
                self.task_group,
                self.ssl,
                self.client,
                self.server,
                self.stream_send,
                request.stream_id,
            )
        self.stream_buffers[request.stream_id] = StreamBuffer(
            self.context.event_class, self.connection_buffer_limit
        )
        try:
            self.priority.insert_stream(request.stream_id)
        except priority.DuplicateStreamError:
            # Received PRIORITY frame before HEADERS frame
            pass
        else:
            self.priority.block(request.stream_id)

        await self.streams[request.stream_id].handle(
            Request(
                stream_id=request.stream_id,
                headers=filter_pseudo_headers(request.headers),
                http_version="2",
                method=method,
                raw_path=raw_path,
                state=self.connection_state,
            )
        )
        self.keep_alive_requests += 1
        await self.context.mark_request()

    async def _create_server_push(
        self, stream_id: int, path: bytes, headers: list[tuple[bytes, bytes]]
    ) -> None:
        push_stream_id = self.connection.get_next_available_stream_id()
        request_headers = [(b":method", b"GET"), (b":path", path)]
        request_headers.extend(headers)
        request_headers.extend(self.config.response_headers("h2"))
        try:
            self.connection.push_stream(
                stream_id=stream_id,
                promised_stream_id=push_stream_id,
                request_headers=request_headers,
            )
            await self._flush()
        except h2.exceptions.ProtocolError:
            # Client does not accept push promises or we are trying to
            # push on a push promises request.
            pass
        else:
            event = h2.events.RequestReceived(stream_id=push_stream_id, headers=request_headers)
            await self._create_stream(event)
            await self.streams[event.stream_id].handle(EndBody(stream_id=event.stream_id))
            self.keep_alive_requests += 1

    async def _close_stream(self, stream_id: int) -> None:
        if stream_id in self.streams:
            stream = self.streams.pop(stream_id)
            await stream.handle(StreamClosed(stream_id=stream_id))
            await self.has_data.set()
        if stream_id in self.stream_buffers:
            # The stream was reset (or the connection closed) with data
            # still queued. Close the buffer to wake any producer blocked on
            # backpressure and return the budget held by the unsent bytes.
            # Frames already handed to h2 are unaffected and continue to be
            # processed per the protocol.
            await self.stream_buffers[stream_id].close()
            del self.stream_buffers[stream_id]
            try:
                self.priority.remove_stream(stream_id)
            except priority.MissingStreamError:
                pass
