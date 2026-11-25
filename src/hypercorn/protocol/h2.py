from __future__ import annotations

import asyncio
from collections import deque
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

BUFFER_HIGH_WATER = 2 * 2**14  # Twice the default max frame size (two frames worth)
BUFFER_LOW_WATER = BUFFER_HIGH_WATER / 2


class BufferCompleteError(Exception):
    pass


class StreamBuffer:
    def __init__(self, event_class: type[IOEvent]) -> None:
        self.buffer = bytearray()
        self._complete = False
        self._is_empty = event_class()
        self._paused = event_class()

    async def drain(self) -> None:
        await self._is_empty.wait()

    def set_complete(self) -> None:
        self._complete = True

    async def close(self) -> None:
        self._complete = True
        self.buffer = bytearray()
        await self._is_empty.set()
        await self._paused.set()

    @property
    def complete(self) -> bool:
        return self._complete and len(self.buffer) == 0

    async def push(self, data: bytes) -> None:
        if self._complete:
            raise BufferCompleteError()
        self.buffer.extend(data)
        await self._is_empty.clear()
        if len(self.buffer) >= BUFFER_HIGH_WATER:
            await self._paused.wait()
            await self._paused.clear()

    async def pop(self, max_length: int) -> bytes:
        length = min(len(self.buffer), max_length)
        data = bytes(self.buffer[:length])
        del self.buffer[:length]
        if len(data) < BUFFER_LOW_WATER:
            await self._paused.set()
        if len(self.buffer) == 0:
            await self._is_empty.set()
        return data


class ReadStream:
    """A per stream buffer for the request body on its way to the ASGI app.

    Inbound data is no longer handed directly to the app from the connection
    event handling task. Instead each stream gets one of these buffers and a
    single task that drains it into the app queue. This gives per stream
    backpressure - a slow app on one stream only blocks that stream's task,
    leaving the connection free to keep reading and advancing the other
    streams.

    The amount of buffered data is bounded by the HTTP/2 flow control
    window. Received data is only acknowledged (returning window to the
    client) once it has been handed to the app, hence a stalled stream stops
    accumulating data at the window limit rather than growing without bound.
    """

    def __init__(self, event_class: type[IOEvent]) -> None:
        # Each entry is (data, flow_controlled_length). The flow controlled
        # length is retained so the exact amount of window consumed (which
        # includes any padding) can be returned when the data is consumed.
        self.buffer: deque[tuple[bytes, int]] = deque()
        self.buffered_bytes = 0
        self._complete = False
        self._closed = False
        self._has_data = event_class()
        self._waiting = False
        self._unfinished = 0
        self._in_flight = 0
        self._drained = event_class()

    async def push(self, data: bytes, flow_controlled_length: int) -> None:
        if self._complete:
            raise BufferCompleteError()
        if self._unfinished == 0:
            await self._drained.clear()
        self._unfinished += 1
        self.buffer.append((data, flow_controlled_length))
        self.buffered_bytes += len(data)
        if self._waiting:
            await self._has_data.set()

    async def push_complete(self) -> None:
        """Mark the end of the request body (the EndBody event)."""
        self._complete = True
        if self._waiting:
            await self._has_data.set()

    async def pop(self) -> tuple[bytes, int] | None:
        """Wait for and return the next chunk, or None once complete.

        Returns None once the buffer has fully drained after completion (or
        after :meth:`close`). The terminal EndBody marker is not stored
        here; the caller should check :attr:`complete` and emit EndBody
        itself when the buffer has drained.
        """
        while not self.buffer:
            if self._complete or self._closed:
                return None
            # Arm before clearing, then re-check: this is race free even when
            # ``clear`` is a real checkpoint (trio), because any push that
            # lands in the window is caught by the re-check below rather than
            # being waited on.
            self._waiting = True
            await self._has_data.clear()
            if self.buffer or self._complete or self._closed:
                self._waiting = False
                continue
            try:
                await self._has_data.wait()
            finally:
                self._waiting = False
        data, flow_controlled_length = self.buffer.popleft()
        self.buffered_bytes -= len(data)
        self._in_flight += 1
        return data, flow_controlled_length

    async def task_done(self) -> None:
        """Record completion of a tuple returned by :meth:`pop`."""
        self._in_flight -= 1
        self._unfinished -= 1
        if self._unfinished == 0:
            await self._drained.set()

    async def join(self) -> None:
        """Wait for all pushed items to be popped and completed."""
        while self._unfinished > 0:
            await self._drained.wait()

    @property
    def closed(self) -> bool:
        return self._closed

    async def close(self) -> None:
        """Release anything buffered and unblock a waiting pop."""
        self._closed = True
        self._complete = True
        queued = len(self.buffer)
        self.buffer.clear()
        self.buffered_bytes = 0
        self._unfinished -= queued
        if self._unfinished <= 0:
            self._unfinished = 0
            await self._drained.set()
        await self._has_data.set()

    @property
    def complete(self) -> bool:
        return self._complete and not self.buffer


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
        # Per stream buffers decoupling inbound request bodies from the apps.
        self.read_streams: dict[int, ReadStream] = {}
        # Direct event/users of _handle_events may not have run
        # _create_stream. These tasks keep that compatibility path moving.
        self._fallback_read_tasks: set[asyncio.Task[None]] = set()
        # The below are used by the sending task
        self.has_data = self.context.event_class()
        self.priority = priority.PriorityTree()
        self.stream_buffers: dict[int, StreamBuffer] = {}

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
            await self.read_streams[event.stream_id].push_complete()
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
            chunk_size = min(
                self.connection.local_flow_control_window(stream_id),
                self.connection.max_outbound_frame_size,
            )
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
            await self.stream_buffers[stream_id].close()
            del self.stream_buffers[stream_id]
            self.priority.remove_stream(stream_id)

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
        fallback_stream_ids: set[int] = set()
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
                # Hand the data to the stream's read buffer. It is only
                # acknowledged (freeing flow control window) once the app
                # has accepted it, via the read stream task. This bounds the
                # buffered body to the stream's window and keeps a slow
                # stream from stalling the rest of the connection.
                read_stream = self.read_streams.get(event.stream_id)
                if read_stream is None:
                    read_stream = ReadStream(self.context.event_class)
                    self.read_streams[event.stream_id] = read_stream
                    if self._spawn_fallback_read_task(event.stream_id):
                        fallback_stream_ids.add(event.stream_id)
                        await read_stream.push(event.data, event.flow_controlled_length)
                    else:
                        stream = self.streams.get(event.stream_id)
                        if stream is not None:
                            await stream.handle(
                                Body(stream_id=event.stream_id, data=event.data)
                            )
                        self.connection.acknowledge_received_data(
                            event.flow_controlled_length, event.stream_id
                        )
                        self.read_streams.pop(event.stream_id, None)
                else:
                    await read_stream.push(event.data, event.flow_controlled_length)
            elif isinstance(event, h2.events.StreamEnded):
                read_stream = self.read_streams.get(event.stream_id)
                if read_stream is not None:
                    # The read stream task delivers EndBody once the body has
                    # drained, preserving ordering.
                    await read_stream.push_complete()
                else:
                    stream = self.streams.get(event.stream_id)
                    if stream is not None:
                        await stream.handle(EndBody(stream_id=event.stream_id))
            elif isinstance(event, h2.events.StreamReset):
                await self._close_stream(event.stream_id, defer_app_close=True)
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
        for stream_id in fallback_stream_ids:
            read_stream = self.read_streams.get(stream_id)
            if read_stream is not None:
                await read_stream.join()

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
        self.stream_buffers[request.stream_id] = StreamBuffer(self.context.event_class)
        self.read_streams[request.stream_id] = ReadStream(self.context.event_class)
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
        self.task_group.spawn(self._read_stream_task, request.stream_id)
        self.keep_alive_requests += 1
        await self.context.mark_request()

    async def _read_stream_task(self, stream_id: int) -> None:
        """Deliver a single stream's request body to its app, in order.

        This runs independently per stream so an app whose queue is full
        only blocks this task. The connection keeps processing other
        streams, and flow control window is only returned to the client as
        the body is consumed here.
        """
        read_stream = self.read_streams[stream_id]
        try:
            while True:
                item = await read_stream.pop()
                if item is None:
                    break
                data, flow_controlled_length = item
                try:
                    stream = self.streams.get(stream_id)
                    if stream is not None and not getattr(stream, "closed", False):
                        # Backpressure on a full app queue only suspends this
                        # stream's task; the connection task is never blocked.
                        await stream.handle(Body(stream_id=stream_id, data=data))
                    # Return the window now the data has been handed over, so a
                    # stalled stream stops receiving at the window boundary.
                    self.connection.acknowledge_received_data(
                        flow_controlled_length, stream_id
                    )
                    await self._flush()
                finally:
                    await read_stream.task_done()

            # The buffer is empty. Only deliver EndBody once every preceding
            # Body chunk has been delivered (guaranteed above) and unless the
            # stream was torn down early.
            if not read_stream.closed:
                stream = self.streams.get(stream_id)
                if stream is not None and not getattr(stream, "closed", False):
                    await stream.handle(EndBody(stream_id=stream_id))
        finally:
            self.read_streams.pop(stream_id, None)

    def _spawn_fallback_read_task(self, stream_id: int) -> bool:
        """Start an asyncio worker for event-level compatibility callers.

        Production streams are created by ``_create_stream`` and scheduled on
        the configured structured task group. Tests and direct protocol users
        may inject stream objects and feed events directly without that setup.
        This fallback keeps those callers ordered and non-head-of-line
        blocked when running on asyncio.
        """
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            # On Trio all production streams still use the task group. A
            # direct event-level caller without _create_stream retains the
            # old direct handling path.
            return False

        task = loop.create_task(self._read_stream_task(stream_id))
        self._fallback_read_tasks.add(task)

        def _discard(done_task: asyncio.Task[None]) -> None:
            self._fallback_read_tasks.discard(done_task)
            if not done_task.cancelled():
                done_task.exception()

        task.add_done_callback(_discard)
        return True

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
            await self.read_streams[event.stream_id].push_complete()
            self.keep_alive_requests += 1

    async def _close_stream(
        self, stream_id: int, *, defer_app_close: bool = False
    ) -> None:
        read_stream = self.read_streams.pop(stream_id, None)
        if read_stream is not None:
            # Discarded frames are no longer waiting for the app. Return
            # their connection-level flow control credit so one reset stream
            # cannot leave the shared connection window depleted. A popped
            # frame is owned by the read task and is acknowledged there.
            discarded_flow_length = sum(
                flow_controlled_length
                for _, flow_controlled_length in read_stream.buffer
            )
            # Unblock a read task that may be waiting for more protocol
            # data. A read task blocked in the bounded app queue is made
            # harmless by removing the stream and closing its buffer.
            await read_stream.close()
            if discarded_flow_length:
                self.connection.acknowledge_received_data(
                    discarded_flow_length, stream_id
                )
                await self._flush()
        if stream_id in self.streams:
            stream = self.streams.pop(stream_id)
            if defer_app_close:
                # A reset must not make the connection task wait behind a
                # full app queue. The structured task group still owns the
                # delivery, while this connection continues other streams.
                self.task_group.spawn(
                    stream.handle, StreamClosed(stream_id=stream_id)
                )
            else:
                await stream.handle(StreamClosed(stream_id=stream_id))
            await self.has_data.set()
