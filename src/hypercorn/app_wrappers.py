from __future__ import annotations

import sys
from collections.abc import Callable, Iterator
from functools import partial
from io import BytesIO

from .typing import (
    ASGIFramework,
    ASGIReceiveCallable,
    ASGIReceiveEvent,
    ASGISendCallable,
    HTTPScope,
    Scope,
    WSGIFramework,
)


class InvalidPathError(Exception):
    pass


class BodyTooLargeError(Exception):
    """Raised when the request body exceeds the configured maximum size."""

    pass


class ClientDisconnectedError(Exception):
    """Raised when the client disconnects before the body is fully received."""

    pass


class _StreamingBody:
    """A file-like ``wsgi.input`` that receives the body as it is read.

    The body is pulled from the server message by message as the app
    reads, which allows uploads to be validated whilst they arrive
    rather than after they have been fully buffered. Only data that
    has been received but not yet read is buffered, so the buffered
    amount is bounded (at most one received message more than the app
    has asked for). The ``max_body_size`` is enforced on the
    cumulative amount received.
    """

    def __init__(
        self, receive_message: Callable[[], ASGIReceiveEvent], max_body_size: int
    ) -> None:
        self._receive_message = receive_message
        self._max_body_size = max_body_size
        self._buffer = bytearray()
        self.received = 0  # Cumulative bytes taken from the stream
        self._complete = False  # The stream has ended or disconnected
        self._closed = False
        self._error: Exception | None = None

    @property
    def buffered(self) -> int:
        """The number of bytes currently buffered (received, not yet read)."""
        return len(self._buffer)

    @property
    def complete(self) -> bool:
        """True once the stream has ended or the client disconnected."""
        return self._complete

    def _mark_complete(self) -> None:
        self._complete = True

    def _pull(self) -> None:
        if self._error is not None:
            raise self._error
        message = self._receive_message()
        if message["type"] == "http.disconnect":
            self._complete = True
            self._error = ClientDisconnectedError(
                "The client disconnected before the body was fully received"
            )
            raise self._error
        data = bytes(message.get("body", b""))
        self.received += len(data)
        if not message.get("more_body"):
            self._complete = True
        if self.received > self._max_body_size:
            self._error = BodyTooLargeError(
                f"The body exceeds the maximum size of {self._max_body_size} bytes"
            )
            raise self._error
        self._buffer.extend(data)

    def read(self, size: int | None = -1) -> bytes:
        if self._closed:
            raise ValueError("I/O operation on closed stream")
        if self._error is not None:
            raise self._error
        if size is None or size < 0:
            while not self._complete:
                self._pull()
            data = bytes(self._buffer)
            self._buffer.clear()
            return data
        elif size == 0:
            return b""
        else:
            while len(self._buffer) < size and not self._complete:
                self._pull()
            data = bytes(self._buffer[:size])
            del self._buffer[:size]
            return data

    def readline(self, size: int | None = -1) -> bytes:
        if self._closed:
            raise ValueError("I/O operation on closed stream")
        if self._error is not None:
            raise self._error
        limit = -1 if size is None else size
        while True:
            newline = self._buffer.find(b"\n", 0, None if limit < 0 else limit)
            if newline != -1:
                end = newline + 1
                break
            elif limit >= 0 and len(self._buffer) >= limit:
                end = limit
                break
            elif self._complete:
                end = len(self._buffer)
                break
            self._pull()
        data = bytes(self._buffer[:end])
        del self._buffer[:end]
        return data

    def readlines(self, hint: int | None = -1) -> list[bytes]:
        lines = []
        total = 0
        while True:
            line = self.readline()
            if line == b"":
                break
            lines.append(line)
            total += len(line)
            if hint is not None and hint > 0 and total >= hint:
                break
        return lines

    def __iter__(self) -> Iterator[bytes]:
        return self

    def __next__(self) -> bytes:
        line = self.readline()
        if line == b"":
            raise StopIteration
        return line

    def close(self) -> None:
        self._closed = True


class ASGIWrapper:
    def __init__(self, app: ASGIFramework) -> None:
        self.app = app

    async def __call__(
        self,
        scope: Scope,
        receive: ASGIReceiveCallable,
        send: ASGISendCallable,
        sync_spawn: Callable,
        call_soon: Callable,
    ) -> None:
        await self.app(scope, receive, send)


class WSGIWrapper:
    def __init__(
        self, app: WSGIFramework, max_body_size: int, stream_request_body: bool = False
    ) -> None:
        self.app = app
        self.max_body_size = max_body_size
        self.stream_request_body = stream_request_body

    async def __call__(
        self,
        scope: Scope,
        receive: ASGIReceiveCallable,
        send: ASGISendCallable,
        sync_spawn: Callable,
        call_soon: Callable,
    ) -> None:
        if scope["type"] == "http":
            await self.handle_http(scope, receive, send, sync_spawn, call_soon)
        elif scope["type"] == "websocket":
            await send({"type": "websocket.close"})  # type: ignore
        elif scope["type"] == "lifespan":
            return
        else:
            raise Exception(f"Unknown scope type, {scope['type']}")

    async def handle_http(
        self,
        scope: HTTPScope,
        receive: ASGIReceiveCallable,
        send: ASGISendCallable,
        sync_spawn: Callable,
        call_soon: Callable,
    ) -> None:
        body_stream: _StreamingBody | None = None
        if self.stream_request_body:
            body = _StreamingBody(partial(call_soon, receive), self.max_body_size)
            body_stream = body
        else:
            buffer = bytearray()
            while True:
                message = await receive()
                buffer.extend(message.get("body", b""))  # type: ignore
                if len(buffer) > self.max_body_size:
                    await send({"type": "http.response.start", "status": 400, "headers": []})
                    await send({"type": "http.response.body", "body": b"", "more_body": False})
                    return
                if not message.get("more_body"):
                    break
            body = bytes(buffer)

        try:
            environ = _build_environ(scope, body)
        except InvalidPathError:
            await send({"type": "http.response.start", "status": 404, "headers": []})
        else:
            response_started = False
            try:
                response_started = await sync_spawn(
                    self.run_app, environ, partial(call_soon, send)
                )
            finally:
                if body_stream is not None:
                    await self._drain_body(body_stream, receive, send, response_started)
        await send({"type": "http.response.body", "body": b"", "more_body": False})

    async def _drain_body(
        self,
        body_stream: _StreamingBody,
        receive: ASGIReceiveCallable,
        send: ASGISendCallable,
        response_started: bool,
    ) -> None:
        # Ensure the remainder of the body is consumed so that the
        # connection can be closed or recycled cleanly, even if the
        # app stopped reading early or exited. The max_body_size check
        # still applies to the cumulative amount received.
        while not body_stream.complete:
            message = await receive()
            if message["type"] == "http.disconnect":
                body_stream._mark_complete()
                break
            body_stream.received += len(message.get("body", b""))
            if body_stream.received > self.max_body_size and not response_started:
                await send({"type": "http.response.start", "status": 400, "headers": []})
                response_started = True
            if not message.get("more_body"):
                body_stream._mark_complete()

    def run_app(self, environ: dict, send: Callable) -> bool:
        headers: list[tuple[bytes, bytes]]
        response_started = False
        status_code: int | None = None

        def start_response(
            status: str,
            response_headers: list[tuple[str, str]],
            exc_info: Exception | None = None,
        ) -> None:
            nonlocal headers, response_started, status_code

            raw, _ = status.split(" ", 1)
            status_code = int(raw)
            headers = [
                (name.lower().encode("latin-1"), value.encode("latin-1"))
                for name, value in response_headers
            ]
            response_started = True

        try:
            response_body = self.app(environ, start_response)
            try:
                first_chunk = True
                for output in response_body:
                    if first_chunk:
                        if not response_started:
                            raise RuntimeError("WSGI app did not call start_response")

                        send(
                            {
                                "type": "http.response.start",
                                "status": status_code,
                                "headers": headers,
                            }
                        )
                        first_chunk = False

                    send({"type": "http.response.body", "body": output, "more_body": True})
            finally:
                if hasattr(response_body, "close"):
                    response_body.close()
        except BodyTooLargeError:
            if not response_started:
                send({"type": "http.response.start", "status": 400, "headers": []})
                response_started = True
        except ClientDisconnectedError:
            pass  # The client has gone, any response would be discarded

        return response_started


def _build_environ(scope: HTTPScope, body: bytes | _StreamingBody) -> dict:
    server = scope.get("server") or ("localhost", 80)
    path = scope["path"]
    script_name = scope.get("root_path", "")
    if path.startswith(script_name):
        path = path[len(script_name) :]
        path = path if path != "" else "/"
    else:
        raise InvalidPathError()

    environ = {
        "REQUEST_METHOD": scope["method"],
        "SCRIPT_NAME": script_name.encode("utf8").decode("latin1"),
        "PATH_INFO": path.encode("utf8").decode("latin1"),
        "QUERY_STRING": scope["query_string"].decode("ascii"),
        "SERVER_NAME": server[0],
        "SERVER_PORT": server[1],
        "SERVER_PROTOCOL": "HTTP/%s" % scope["http_version"],
        "wsgi.version": (1, 0),
        "wsgi.url_scheme": scope.get("scheme", "http"),
        "wsgi.input": BytesIO(body) if isinstance(body, bytes) else body,
        "wsgi.errors": sys.stdout,
        "wsgi.multithread": True,
        "wsgi.multiprocess": True,
        "wsgi.run_once": False,
    }

    if scope.get("client") is not None:
        environ["REMOTE_ADDR"] = scope["client"][0]

    for raw_name, raw_value in scope.get("headers", []):
        name = raw_name.decode("latin1")
        if name == "content-length":
            corrected_name = "CONTENT_LENGTH"
        elif name == "content-type":
            corrected_name = "CONTENT_TYPE"
        else:
            corrected_name = "HTTP_%s" % name.upper().replace("-", "_")
        # HTTPbis say only ASCII chars are allowed in headers, but we latin1 just in case
        value = raw_value.decode("latin1")
        if corrected_name in environ:
            value = environ[corrected_name] + "," + value  # type: ignore
        environ[corrected_name] = value
    return environ
