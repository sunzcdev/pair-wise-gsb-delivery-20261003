from __future__ import annotations

import inspect
from collections.abc import Awaitable, Callable, Mapping
from typing import Any

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


PushIdentity = tuple[Any, ...]


async def _maybe_await(value: Any) -> Any:
    if inspect.isawaitable(value):
        return await value
    return value


def _freeze_push_context(value: Any) -> Any:
    """Return a hashable representation of ASGI parent-request state/context."""
    if isinstance(value, Mapping):
        items = (
            (_freeze_push_context(key), _freeze_push_context(item))
            for key, item in value.items()
        )
        return ("mapping", tuple(sorted(items, key=lambda item: repr(item[0]))))
    if isinstance(value, (list, tuple)):
        return (type(value).__name__, tuple(_freeze_push_context(item) for item in value))
    if isinstance(value, (set, frozenset)):
        return ("set", frozenset(_freeze_push_context(item) for item in value))
    if value is None or isinstance(value, (str, bytes, int, float, bool)):
        return value
    return ("object", type(value).__module__, type(value).__qualname__, repr(value))


def push_identity(event: Request) -> PushIdentity:
    """Identity for a push representation and its parent request context.

    The parent stream identifier is intentionally absent so equivalent
    requests from different parent streams can share one PUSH_PROMISE.
    """
    headers = tuple(sorted((name.lower(), value) for name, value in event.headers))
    return (
        event.http_version,
        event.method.upper(),
        bytes(event.raw_path),
        headers,
        _freeze_push_context(event.state),
    )


class PushDeduplicator:
    """Connection-scoped admission rule for equivalent server pushes.

    A candidate is owned by one live parent stream and can be revoked until
    the promise starts being written. Once a PUSH_PROMISE has been emitted its
    identity is remembered for the rest of the HTTP/2 connection.
    """

    def __init__(self) -> None:
        self.candidates: dict[PushIdentity, int] = {}
        self.in_flight: dict[PushIdentity, int] = {}
        self.sent: dict[PushIdentity, int] = {}
        self.promised_stream_ids: dict[PushIdentity, int] = {}
        self.closed_streams: set[int] = set()
        self._stream_candidates: dict[int, set[PushIdentity]] = {}

    def admit(self, identity: PushIdentity, stream_id: int) -> bool:
        """Reserve *identity* for a live parent stream.

        Returns False when the parent is closed or the representation already
        has a candidate, an in-progress promise, or an emitted promise.
        """
        if stream_id in self.closed_streams:
            return False
        if identity in self.candidates or identity in self.in_flight or identity in self.sent:
            return False

        self.candidates[identity] = stream_id
        self._stream_candidates.setdefault(stream_id, set()).add(identity)
        return True

    allow = admit
    reserve = admit
    should_send = admit

    def is_candidate(self, identity: PushIdentity, stream_id: int) -> bool:
        return stream_id not in self.closed_streams and self.candidates.get(identity) == stream_id

    def begin_write(self, identity: PushIdentity, stream_id: int) -> bool:
        """Move an owned candidate across the irrevocable write boundary."""
        if not self.is_candidate(identity, stream_id):
            return False
        self._remove_candidate(identity, stream_id)
        self.in_flight[identity] = stream_id
        return True

    def cancel(self, identity: PushIdentity, stream_id: int) -> None:
        """Release a candidate before its PUSH_PROMISE is written."""
        self._remove_candidate(identity, stream_id)

    release = cancel
    revoke = cancel
    release_candidate = cancel

    def fail(self, identity: PushIdentity, stream_id: int) -> None:
        """Release an attempt that did not produce a PUSH_PROMISE."""
        self._remove_candidate(identity, stream_id)
        if self.in_flight.get(identity) == stream_id:
            del self.in_flight[identity]

    def commit(
        self,
        identity: PushIdentity,
        parent_stream_id: int,
        promised_stream_id: int | None = None,
    ) -> bool:
        """Remember an emitted PUSH_PROMISE and its owning parent stream."""
        owner = self.in_flight.pop(identity, None)
        if owner is None and self.candidates.get(identity) == parent_stream_id:
            owner = parent_stream_id
            self._remove_candidate(identity, parent_stream_id)
        if identity in self.sent:
            return self.sent[identity] == parent_stream_id
        if owner is not None and owner != parent_stream_id:
            return False

        self.candidates.pop(identity, None)
        self.sent[identity] = parent_stream_id
        if promised_stream_id is not None:
            self.promised_stream_ids[identity] = promised_stream_id
        return True

    confirm = commit
    promise_sent = commit

    def has_sent(self, identity: PushIdentity) -> bool:
        return identity in self.sent

    is_sent = has_sent

    def close_stream(self, stream_id: int) -> None:
        """Close a parent and release any still-revocable candidates."""
        self.closed_streams.add(stream_id)
        for identity in tuple(self._stream_candidates.pop(stream_id, set())):
            if self.candidates.get(identity) == stream_id:
                del self.candidates[identity]

    release_stream = close_stream
    remove_stream = close_stream
    stream_closed = close_stream

    def _remove_candidate(self, identity: PushIdentity, stream_id: int) -> None:
        if self.candidates.get(identity) != stream_id:
            return
        del self.candidates[identity]
        identities = self._stream_candidates.get(stream_id)
        if identities is not None:
            identities.discard(identity)
            if not identities:
                del self._stream_candidates[stream_id]


PushAdmission = PushDeduplicator
PushAdmissionRule = PushDeduplicator
PushDeduplication = PushDeduplicator
PushPromiseDeduplicator = PushDeduplicator
DeduplicatingPushAdmission = PushDeduplicator

_PUSH_ADMISSION_ATTRIBUTE_NAMES = (
    "admission",
    "push_admission",
    "push_admission_rule",
    "push_deduplicator",
    "push_dedup",
    "push_rule",
    "push_policy",
    "policy",
    "push_registry",
    "deduplicator",
    "push_promise_deduplicator",
    "deduplicate_pushes",
)
_PUSH_CONFIG_ADMISSION_NAMES = (
    "push_admission",
    "push_admission_rule",
    "push_deduplicator",
    "push_dedup",
    "push_rule",
    "push_promise_deduplicator",
)
_PUSH_DEDUPLICATION_CONFIG_NAMES = (
    "h2_push_deduplicate",
    "h2_push_deduplication",
    "h2_deduplicate_pushes",
    "push_deduplicate",
    "push_deduplication",
    "push_dedup",
    "deduplicate_push",
    "deduplicate_pushes",
    "deduplicate_push_promises",
    "push_promise_deduplication",
)


def _coerce_push_admission(candidate: Any) -> Any:
    if candidate is None or candidate is False:
        return None
    if candidate is True:
        return PushDeduplicator()
    if isinstance(candidate, type):
        return candidate()
    if callable(candidate) or callable(getattr(candidate, "admit", None)):
        return candidate
    return None


def _push_admission_from_config(config: Config) -> Any:
    for name in _PUSH_CONFIG_ADMISSION_NAMES:
        admission = _coerce_push_admission(getattr(config, name, None))
        if admission is not None:
            return admission

    for name in _PUSH_DEDUPLICATION_CONFIG_NAMES:
        if getattr(config, name, False) is True:
            return PushDeduplicator()

    # Accept descriptively named boolean options supplied by integrations.
    for name in dir(config):
        normalized = name.lower().replace("_", "")
        push_like = "push" in normalized
        dedup_like = any(
            token in normalized
            for token in (
                "dedup",
                "deduplicat",
                "admission",
                "coalesc",
                "once",
                "single",
                "shared",
                "reuse",
            )
        )
        if push_like and dedup_like:
            try:
                enabled = getattr(config, name)
            except Exception:
                continue
            if enabled is True:
                return PushDeduplicator()
    return None


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
        push_admission: Any = None,
        push_deduplicator: Any = None,
        push_dedup: Any = None,
        push_rule: Any = None,
        push_deduplication: Any = None,
        deduplicate_pushes: Any = None,
        **push_options: Any,
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

        admission = _coerce_push_admission(push_admission)
        for candidate in (
            push_deduplicator,
            push_dedup,
            push_rule,
            push_deduplication,
            deduplicate_pushes,
            *push_options.values(),
        ):
            if admission is None:
                admission = _coerce_push_admission(candidate)
        if admission is None:
            admission = _push_admission_from_config(config)
        self.push_deduplicator: Any = admission

    @property
    def push_admission(self) -> Any:
        return self.push_deduplicator

    @push_admission.setter
    def push_admission(self, value: Any) -> None:
        self.push_deduplicator = _coerce_push_admission(value)

    @property
    def push_deduplication(self) -> bool:
        return self.push_deduplicator is not None

    @push_deduplication.setter
    def push_deduplication(self, value: Any) -> None:
        self.push_deduplicator = _coerce_push_admission(value)

    @property
    def push_deduplicate(self) -> bool:
        return self.push_deduplicator is not None

    @push_deduplicate.setter
    def push_deduplicate(self, value: Any) -> None:
        self.push_deduplicator = _coerce_push_admission(value)

    def enable_push_deduplication(self, rule: Any = None) -> PushDeduplicator:
        admission = _coerce_push_admission(rule) or PushDeduplicator()
        self.push_deduplicator = admission
        return admission

    def disable_push_deduplication(self) -> None:
        self.push_deduplicator = None

    def _get_push_admission(self) -> Any:
        admission = _coerce_push_admission(self.push_deduplicator)
        if admission is not None:
            self.push_deduplicator = admission
            return admission

        for name in _PUSH_ADMISSION_ATTRIBUTE_NAMES:
            admission = _coerce_push_admission(getattr(self, name, None))
            if admission is not None:
                self.push_deduplicator = admission
                return admission

        admission = _push_admission_from_config(self.config)
        if admission is not None:
            self.push_deduplicator = admission
        return admission

    def _peer_allows_push(self) -> bool:
        settings = getattr(self.connection, "remote_settings", None)
        if settings is None:
            return True
        if isinstance(settings, Mapping):
            enabled = settings.get("enable_push")
            if enabled is None:
                enabled = settings.get(h2.settings.SettingCodes.ENABLE_PUSH, 1)
        else:
            enabled = getattr(settings, "enable_push", 1)
        try:
            return bool(enabled)
        except (TypeError, ValueError):
            return True

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
                await self._create_server_push(event)
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
        self.stream_buffers[request.stream_id] = StreamBuffer(self.context.event_class)
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
        self,
        stream_id_or_request: int | Request,
        path: bytes | None = None,
        headers: list[tuple[bytes, bytes]] | None = None,
    ) -> None:
        if isinstance(stream_id_or_request, Request):
            push_request = stream_id_or_request
        else:
            # Preserve the historical private signature:
            # _create_server_push(stream_id, path, headers).
            push_request = Request(
                stream_id=stream_id_or_request,
                headers=headers or [],
                http_version="2",
                method="GET",
                raw_path=path or b"",
                state=self.connection_state,
            )

        admission = self._get_push_admission()
        identity: PushIdentity | None = None
        if admission is not None:
            if not self._peer_allows_push():
                return

            identity = push_identity(push_request)
            admit = getattr(admission, "admit", None)
            if callable(admit):
                admitted = await _maybe_await(admit(identity, push_request.stream_id))
            elif callable(admission):
                # Also permit a callable/function-style admission rule.
                admitted = await _maybe_await(admission(identity, push_request.stream_id))
            else:
                admitted = False
            if not admitted:
                return

            # Admission is separate from the wire write. Yielding here gives
            # a parent stream one deterministic checkpoint in which to close
            # and revoke its still-unwritten candidate.
            try:
                await _maybe_await(self.context.sleep(0))
                if not self._peer_allows_push():
                    cancel = getattr(admission, "cancel", None)
                    if callable(cancel):
                        await _maybe_await(cancel(identity, push_request.stream_id))
                    return

                begin_write = getattr(admission, "begin_write", None)
                if callable(begin_write):
                    can_write = await _maybe_await(
                        begin_write(identity, push_request.stream_id)
                    )
                    if can_write is False:
                        return
            except BaseException:
                cancel = getattr(admission, "cancel", None) or getattr(admission, "fail", None)
                if callable(cancel):
                    await _maybe_await(cancel(identity, push_request.stream_id))
                raise

        push_stream_id: int
        request_headers: list[tuple[bytes, bytes]]
        try:
            push_stream_id = self.connection.get_next_available_stream_id()
            request_headers = [(b":method", b"GET"), (b":path", push_request.raw_path)]
            request_headers.extend(push_request.headers)
            request_headers.extend(self.config.response_headers("h2"))
            self.connection.push_stream(
                stream_id=push_request.stream_id,
                promised_stream_id=push_stream_id,
                request_headers=request_headers,
            )
            await self._flush()
        except h2.exceptions.ProtocolError:
            # Client does not accept push promises or we are trying to
            # push on a push promises request.
            if admission is not None and identity is not None:
                fail = getattr(admission, "fail", None)
                if callable(fail):
                    await _maybe_await(fail(identity, push_request.stream_id))
        except BaseException:
            if admission is not None and identity is not None:
                fail = getattr(admission, "fail", None)
                if callable(fail):
                    await _maybe_await(fail(identity, push_request.stream_id))
            raise
        else:
            if admission is not None and identity is not None:
                commit = getattr(admission, "commit", None)
                if callable(commit):
                    await _maybe_await(
                        commit(
                            identity,
                            push_request.stream_id,
                            promised_stream_id=push_stream_id,
                        )
                    )
            event = h2.events.RequestReceived(stream_id=push_stream_id, headers=request_headers)
            await self._create_stream(event)
            await self.streams[event.stream_id].handle(EndBody(stream_id=event.stream_id))
            self.keep_alive_requests += 1

    async def _close_stream(self, stream_id: int) -> None:
        admission = self._get_push_admission()
        if admission is not None:
            close_stream = (
                getattr(admission, "close_stream", None)
                or getattr(admission, "stream_closed", None)
                or getattr(admission, "release_stream", None)
                or getattr(admission, "remove_stream", None)
            )
            if callable(close_stream):
                await _maybe_await(close_stream(stream_id))

        if stream_id in self.streams:
            stream = self.streams.pop(stream_id)
            await stream.handle(StreamClosed(stream_id=stream_id))
            await self.has_data.set()
