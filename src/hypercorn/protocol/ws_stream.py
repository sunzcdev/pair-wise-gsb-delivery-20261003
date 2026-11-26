from __future__ import annotations

from collections.abc import Awaitable, Callable, Iterable
from enum import auto, Enum
from io import BytesIO, StringIO
from time import time
from urllib.parse import unquote

from wsproto.connection import Connection, ConnectionState, ConnectionType
from wsproto.events import (
    BytesMessage,
    CloseConnection,
    Event as WSProtoEvent,
    Message,
    Ping,
    TextMessage,
)
from wsproto.extensions import Extension, PerMessageDeflate
from wsproto.frame_protocol import CloseReason
from wsproto.handshake import server_extensions_handshake, WEBSOCKET_VERSION
from wsproto.utilities import generate_accept_token, LocalProtocolError, split_comma_header

from .events import Body, Data, EndBody, EndData, Event, Request, Response, StreamClosed
from ..config import Config
from ..typing import (
    AppWrapper,
    ASGISendEvent,
    TaskGroup,
    WebsocketAcceptEvent,
    WebsocketResponseBodyEvent,
    WebsocketResponseStartEvent,
    WebsocketScope,
    WorkerContext,
)
from ..utils import (
    build_and_validate_headers,
    suppress_body,
    UnexpectedMessageError,
    valid_server_name,
)


class ASGIWebsocketState(Enum):
    # Hypercorn supports the ASGI websocket HTTP response extension,
    # which allows HTTP responses rather than acceptance.
    HANDSHAKE = auto()
    CONNECTED = auto()
    RESPONSE = auto()
    # A close frame has been sent by the server and the response from
    # the client is awaited, i.e. the close handshake is on-going.
    CLOSING = auto()
    CLOSED = auto()
    HTTPCLOSED = auto()


class FrameTooLargeError(Exception):
    pass


# The granularity (in seconds) at which the close handshake wait checks
# for the peer's confirmation.
_CLOSE_POLL = 0.1


class Handshake:
    def __init__(self, headers: list[tuple[bytes, bytes]], http_version: str) -> None:
        self.accepted = False
        self.http_version = http_version
        self.connection_tokens: list[str] | None = None
        self.extensions: list[str] | None = None
        self.key: bytes | None = None
        self.subprotocols: list[str] | None = None
        self.upgrade: bytes | None = None
        self.version: bytes | None = None
        for name, value in headers:
            name = name.lower()
            if name == b"connection":
                self.connection_tokens = split_comma_header(value)
            elif name == b"sec-websocket-extensions":
                self.extensions = split_comma_header(value)
            elif name == b"sec-websocket-key":
                self.key = value
            elif name == b"sec-websocket-protocol":
                self.subprotocols = split_comma_header(value)
            elif name == b"sec-websocket-version":
                self.version = value
            elif name == b"upgrade":
                self.upgrade = value

    def is_valid(self) -> bool:
        if self.http_version < "1.1":
            return False
        elif self.http_version == "1.1":
            if self.key is None:
                return False
            if self.connection_tokens is None or not any(
                token.lower() == "upgrade" for token in self.connection_tokens
            ):
                return False
            if self.upgrade.lower() != b"websocket":
                return False

        if self.version != WEBSOCKET_VERSION:
            return False
        return True

    def accept(
        self,
        subprotocol: str | None,
        additional_headers: Iterable[tuple[bytes, bytes]],
    ) -> tuple[int, list[tuple[bytes, bytes]], Connection]:
        headers = []
        if subprotocol is not None:
            if self.subprotocols is None or subprotocol not in self.subprotocols:
                raise Exception("Invalid Subprotocol")
            else:
                headers.append((b"sec-websocket-protocol", subprotocol.encode()))

        extensions: list[Extension] = [PerMessageDeflate()]
        accepts = None
        if self.extensions is not None:
            accepts = server_extensions_handshake(self.extensions, extensions)

        if accepts:
            headers.append((b"sec-websocket-extensions", accepts))

        if self.key is not None:
            headers.append((b"sec-websocket-accept", generate_accept_token(self.key)))

        status_code = 200
        if self.http_version == "1.1":
            headers.extend([(b"upgrade", b"WebSocket"), (b"connection", b"Upgrade")])
            status_code = 101

        for name, value in additional_headers:
            if b"sec-websocket-protocol" == name or name.startswith(b":"):
                raise Exception(f"Invalid additional header, {name.decode()}")

            headers.append((name, value))

        self.accepted = True
        return status_code, headers, Connection(ConnectionType.SERVER, extensions)


class WebsocketBuffer:
    def __init__(self, max_length: int) -> None:
        self.value: BytesIO | StringIO | None = None
        self.length = 0
        self.max_length = max_length

    def extend(self, event: Message) -> None:
        if self.value is None:
            if isinstance(event, TextMessage):
                self.value = StringIO()
            else:
                self.value = BytesIO()
        self.length += self.value.write(event.data)
        if self.length > self.max_length:
            raise FrameTooLargeError()

    def clear(self) -> None:
        self.value = None
        self.length = 0

    def to_message(self) -> dict:
        return {
            "type": "websocket.receive",
            "bytes": self.value.getvalue() if isinstance(self.value, BytesIO) else None,
            "text": self.value.getvalue() if isinstance(self.value, StringIO) else None,
        }


class WSStream:
    def __init__(
        self,
        app: AppWrapper,
        config: Config,
        context: WorkerContext,
        task_group: TaskGroup,
        ssl: bool,
        client: tuple[str, int] | None,
        server: tuple[str, int] | None,
        send: Callable[[Event], Awaitable[None]],
        stream_id: int,
    ) -> None:
        self.app = app
        self.app_put: Callable | None = None
        self.buffer = WebsocketBuffer(config.websocket_max_message_size)
        self.client = client
        self.closed = False
        self.config = config
        self.context = context
        self.task_group = task_group
        self.response: WebsocketResponseStartEvent
        self.scope: WebsocketScope
        self.send = send
        # RFC 8441 for HTTP/2 says use http or https, ASGI says ws or wss
        self.scheme = "wss" if ssl else "ws"
        self.server = server
        self.start_time: float
        self.state = ASGIWebsocketState.HANDSHAKE
        self.stream_id = stream_id

        self.connection: Connection
        self.handshake: Handshake

        # The close code sent by the peer, if a close frame was received.
        self.peer_close_code: int | None = None
        self._peer_close_received = False
        # Guards the single cleanup and single disconnect delivery.
        self._finalised = False
        self._disconnect_sent = False

    @property
    def idle(self) -> bool:
        return self.state in {ASGIWebsocketState.CLOSED, ASGIWebsocketState.HTTPCLOSED}

    async def handle(self, event: Event) -> None:
        if isinstance(event, StreamClosed):
            await self._handle_stream_closed()
        elif self.closed:
            return
        elif isinstance(event, Request):
            await self._handle_request(event)
        elif self.state in {ASGIWebsocketState.CLOSED, ASGIWebsocketState.HTTPCLOSED}:
            # The stream has finished, so any late data is ignored.
            return
        elif isinstance(event, (Body, Data)) and not self.handshake.accepted:
            await self._send_error_response(400)
            self.closed = True
        elif isinstance(event, (Body, Data)):
            self.connection.receive_data(event.data)
            await self._handle_events()

    async def _handle_request(self, event: Request) -> None:
        self.start_time = time()
        self.handshake = Handshake(event.headers, event.http_version)
        path, _, query_string = event.raw_path.partition(b"?")
        self.scope = {
            "type": "websocket",
            "asgi": {"spec_version": "2.3", "version": "3.0"},
            "scheme": self.scheme,
            "http_version": event.http_version,
            "path": unquote(path.decode("ascii")),
            "raw_path": path,
            "query_string": query_string,
            "root_path": self.config.root_path,
            "headers": event.headers,
            "client": self.client,
            "server": self.server,
            "state": event.state.copy(),  # type: ignore[typeddict-item]
            "subprotocols": self.handshake.subprotocols or [],
            "extensions": {"websocket.http.response": {}},
        }

        if not valid_server_name(self.config, event):
            await self._send_error_response(404)
            self.closed = True
        elif not self.handshake.is_valid():
            await self._send_error_response(400)
            self.closed = True
        else:
            self.app_put = await self.task_group.spawn_app(
                self.app, self.config, self.scope, self.app_send
            )
            await self.app_put({"type": "websocket.connect"})

    async def _handle_stream_closed(self) -> None:
        # The bearing stream (or connection) is gone, so the close can
        # no longer be awaited. The app is told once and no stream end
        # is sent as the protocol has already removed the stream.
        code = (
            CloseReason.NORMAL_CLOSURE.value
            if self.state in {ASGIWebsocketState.CLOSED, ASGIWebsocketState.HTTPCLOSED}
            else CloseReason.ABNORMAL_CLOSURE.value
        )
        await self._finalise(code, send_stream_end=False)

    async def app_send(self, message: ASGISendEvent | None) -> None:
        if self.closed:
            # Allow app to finish after close
            return

        if message is None:  # ASGI App has finished sending messages
            # Cleanup if required
            if self.state == ASGIWebsocketState.HANDSHAKE:
                await self._send_error_response(500)
                await self.config.log.access(
                    self.scope, {"status": 500, "headers": []}, time() - self.start_time
                )
                await self.send(StreamClosed(stream_id=self.stream_id))
            elif self.state == ASGIWebsocketState.CONNECTED:
                # The application failed without closing, so an internal
                # error close is sent and the stream released directly.
                await self._send_wsproto_event(CloseConnection(code=CloseReason.INTERNAL_ERROR))
                await self.send(StreamClosed(stream_id=self.stream_id))
            elif self.state == ASGIWebsocketState.CLOSING:
                # Wait for the peer confirmation (or the close timeout),
                # the stream is released by whichever happens first.
                pass
            else:
                await self.send(StreamClosed(stream_id=self.stream_id))
        else:
            if message["type"] == "websocket.accept" and self.state == ASGIWebsocketState.HANDSHAKE:
                await self._accept(message)
            elif (
                message["type"] == "websocket.http.response.start"
                and self.state == ASGIWebsocketState.HANDSHAKE
            ):
                self.response = message
            elif message["type"] == "websocket.http.response.body" and self.state in {
                ASGIWebsocketState.HANDSHAKE,
                ASGIWebsocketState.RESPONSE,
            }:
                await self._send_rejection(message)
            elif message["type"] == "websocket.send" and self.state == ASGIWebsocketState.CONNECTED:
                event: WSProtoEvent
                if message.get("bytes") is not None:
                    event = BytesMessage(data=bytes(message["bytes"]))
                elif not isinstance(message["text"], str):
                    raise TypeError(f"{message['text']} should be a str")
                else:
                    event = TextMessage(data=message["text"])
                await self._send_wsproto_event(event)
            elif message["type"] == "websocket.send" and self.state == ASGIWebsocketState.CLOSING:
                # Further messages cannot be sent whilst the close is
                # being confirmed, so they are no-ops.
                pass
            elif (
                message["type"] == "websocket.close"
                and self.state == ASGIWebsocketState.HANDSHAKE
            ):
                self.state = ASGIWebsocketState.HTTPCLOSED
                await self._send_error_response(403)
            elif (
                message["type"] == "websocket.close"
                and self.state == ASGIWebsocketState.CONNECTED
            ):
                await self._send_close(
                    int(message.get("code", CloseReason.NORMAL_CLOSURE)),
                    message.get("reason"),
                )
            elif (
                message["type"] == "websocket.close"
                and self.state == ASGIWebsocketState.CLOSING
            ):
                # Only the first close is sent, the rest are no-ops to
                # allow the app to finish after having closed.
                pass
            else:
                raise UnexpectedMessageError(self.state, message["type"])

    async def _send_close(self, code: int, reason: str | None) -> None:
        # The close frame is sent, but the bearing stream is not ended
        # here. It stays open until the peer confirms, or until the
        # configured close timeout elapses. The application can then be
        # told a confirmed close apart from a timeout.
        self.state = ASGIWebsocketState.CLOSING
        await self._send_wsproto_event(CloseConnection(code=code, reason=reason))
        timeout = self.config.websocket_close_timeout
        if (
            not self._finalised
            and timeout is not None
            and hasattr(self.task_group, "spawn")
        ):
            # The bounded wait runs as a concurrent task so that other
            # streams (e.g. on the same HTTP/2 connection) are served
            # whilst this stream awaits its confirmation. When the
            # timeout is ``None`` no timer is needed: peer/transport
            # events still finalise the close.
            self.task_group.spawn(self._close_timeout, timeout)

    async def _close_timeout(self, timeout: float | None) -> None:
        # The wait is sliced so that a confirmation arriving early ends
        # the task promptly, rather than keeping the connection's task
        # group alive for the whole (configured) bound. A ``None``
        # timeout means the peer confirmation is awaited indefinitely.
        deadline = None if timeout is None else self.context.time() + timeout
        while not self._finalised and self.state == ASGIWebsocketState.CLOSING:
            if deadline is None:
                await self.context.sleep(_CLOSE_POLL)
                continue
            remaining = deadline - self.context.time()
            if remaining <= 0:
                break
            await self.context.sleep(min(_CLOSE_POLL, remaining))
        if self._finalised or self.state != ASGIWebsocketState.CLOSING:
            # The peer confirmed (or the stream ended) whilst waiting.
            return
        if self._peer_close_received:
            # The protocol task has received the peer's close and is
            # sending the matching response/finalisation. Let it own the
            # terminal code/order rather than racing it with a timeout.
            return
        # The peer never confirmed the close within the configured
        # bound, so the stream is released and the difference is
        # reported to the app as an abnormal closure.
        await self._finalise(CloseReason.ABNORMAL_CLOSURE.value)

    async def _handle_events(self) -> None:
        for event in self.connection.events():
            if isinstance(event, Message):
                if self.state != ASGIWebsocketState.CONNECTED:
                    # No messages are delivered to the application whilst
                    # the close handshake is in progress.
                    continue

                try:
                    self.buffer.extend(event)
                except FrameTooLargeError:
                    await self._send_wsproto_event(
                        CloseConnection(code=CloseReason.MESSAGE_TOO_BIG)
                    )
                    break

                if event.message_finished:
                    await self.app_put(self.buffer.to_message())
                    self.buffer.clear()
            elif isinstance(event, Ping):
                await self._send_wsproto_event(event.response())
            elif isinstance(event, CloseConnection):
                await self._handle_close(event)

    async def _handle_close(self, event: CloseConnection) -> None:
        # Record receipt synchronously before any await so a racing
        # timeout cannot claim that no confirmation was received.
        self._peer_close_received = True
        self.peer_close_code = int(event.code) if event.code is not None else None
        if self.connection.state == ConnectionState.REMOTE_CLOSING:
            # The peer initiated the close, so it is responded to in
            # order to complete the handshake before the stream ends.
            await self._send_wsproto_event(event.response())
        if self.peer_close_code is None:
            code = CloseReason.NORMAL_CLOSURE.value
        else:
            code = self.peer_close_code
        await self._finalise(code)

    async def _finalise(self, code: int, send_stream_end: bool = True) -> None:
        # Ensures the stream is cleaned up, and the disconnect is sent
        # to the app, exactly once whichever of the peer close, timeout
        # or transport close arrives first. The state becomes closed so
        # the stream no longer holds the connection open.
        if self._finalised:
            return
        self._finalised = True
        self.closed = True
        self.state = ASGIWebsocketState.CLOSED
        if send_stream_end:
            await self.send(EndData(stream_id=self.stream_id))
            await self.send(StreamClosed(stream_id=self.stream_id))
        if self.app_put is not None and not self._disconnect_sent:
            self._disconnect_sent = True
            await self.app_put({"type": "websocket.disconnect", "code": code})

    async def _send_error_response(self, status_code: int) -> None:
        await self.send(
            Response(
                stream_id=self.stream_id,
                status_code=status_code,
                headers=[(b"content-length", b"0"), (b"connection", b"close")],
            )
        )
        await self.send(EndBody(stream_id=self.stream_id))
        await self.config.log.access(
            self.scope, {"status": status_code, "headers": []}, time() - self.start_time
        )

    async def _send_wsproto_event(self, event: WSProtoEvent) -> None:
        try:
            data = self.connection.send(event)
        except LocalProtocolError:
            pass
        else:
            await self.send(Data(stream_id=self.stream_id, data=data))

    async def _accept(self, message: WebsocketAcceptEvent) -> None:
        self.state = ASGIWebsocketState.CONNECTED
        status_code, headers, self.connection = self.handshake.accept(
            message.get("subprotocol"), message.get("headers", [])
        )
        await self.send(
            Response(stream_id=self.stream_id, status_code=status_code, headers=headers)
        )
        await self.config.log.access(
            self.scope, {"status": status_code, "headers": []}, time() - self.start_time
        )
        if self.config.websocket_ping_interval is not None:
            self.task_group.spawn(self._send_pings)

    async def _send_rejection(self, message: WebsocketResponseBodyEvent) -> None:
        body_suppressed = suppress_body("GET", self.response["status"])
        if self.state == ASGIWebsocketState.HANDSHAKE:
            headers = build_and_validate_headers(self.response["headers"])
            await self.send(
                Response(
                    stream_id=self.stream_id,
                    status_code=int(self.response["status"]),
                    headers=headers,
                )
            )
            self.state = ASGIWebsocketState.RESPONSE
        if not body_suppressed:
            await self.send(Body(stream_id=self.stream_id, data=bytes(message.get("body", b""))))
        if not message.get("more_body", False):
            self.state = ASGIWebsocketState.HTTPCLOSED
            await self.send(EndBody(stream_id=self.stream_id))
            await self.config.log.access(self.scope, self.response, time() - self.start_time)

    async def _send_pings(self) -> None:
        while not self.closed:
            await self._send_wsproto_event(Ping())
            await self.context.sleep(self.config.websocket_ping_interval)
