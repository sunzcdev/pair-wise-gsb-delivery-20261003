from __future__ import annotations

import json
from collections.abc import Callable, Generator
from typing import Any, Literal

from starlette import status
from starlette._utils import is_async_callable
from starlette.concurrency import run_in_threadpool
from starlette.exceptions import HTTPException
from starlette.requests import Request
from starlette.responses import PlainTextResponse, Response
from starlette.types import Message, Receive, Scope, Send
from starlette.websockets import WebSocket


class HTTPEndpoint:
    def __init__(self, scope: Scope, receive: Receive, send: Send) -> None:
        assert scope["type"] == "http"
        self.scope = scope
        self.receive = receive
        self.send = send
        self._allowed_methods = [
            method
            for method in ("GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "QUERY")
            if getattr(self, method.lower(), None) is not None
            or (method == "HEAD" and getattr(self, "get", None) is not None)
        ]

    def __await__(self) -> Generator[Any, None, None]:
        return self.dispatch().__await__()

    async def dispatch(self) -> None:
        request = Request(self.scope, receive=self.receive, send=self.send)
        handler_name = "get" if request.method == "HEAD" and not hasattr(self, "head") else request.method.lower()

        handler: Callable[[Request], Any]
        if request.method in self._allowed_methods:
            handler = getattr(self, handler_name)
        else:
            handler = self.method_not_allowed
        is_async = is_async_callable(handler)
        if is_async:
            response = await handler(request)
        else:
            response = await run_in_threadpool(handler, request)
        await response(self.scope, self.receive, self.send)

    async def method_not_allowed(self, request: Request) -> Response:
        # If we're running inside a starlette application then raise an
        # exception, so that the configurable exception handler can deal with
        # returning the response. For plain ASGI apps, just return the response.
        headers = {"Allow": ", ".join(self._allowed_methods)}
        if "app" in self.scope:
            raise HTTPException(status_code=405, headers=headers)
        return PlainTextResponse("Method Not Allowed", status_code=405, headers=headers)


# Sentinel for "no per-connection encoding has been selected yet", distinct
# from `None` which is a valid encoding (pass-through of text/bytes).
_CONNECTION_ENCODING_UNSET: Any = object()


class WebSocketDecodeError(RuntimeError):
    """
    Raised when an incoming websocket message cannot be decoded with the
    encoding selected for the connection, or when no encoding could be
    selected at all.

    Carries the websocket ``code`` that was sent to the client when closing,
    so that ``WebSocketEndpoint.dispatch`` can report it to ``on_disconnect``
    instead of masking the failure as a 1011 internal error.
    """

    def __init__(self, message: str, *, code: int = status.WS_1003_UNSUPPORTED_DATA) -> None:
        super().__init__(message)
        self.code = code


class WebSocketEndpoint:
    encoding: Literal["text", "bytes", "json"] | None = None
    subprotocol_encodings: dict[str, Literal["text", "bytes", "json"]] | None = None

    def __init__(self, scope: Scope, receive: Receive, send: Send) -> None:
        assert scope["type"] == "websocket"
        self.scope = scope
        self.receive = receive
        self.send = send
        # The encoding selected for this specific connection. Set once per
        # connection by `dispatch()`, so that concurrent connections never
        # affect each other through the class-level `encoding` attribute.
        self._connection_encoding: Any = _CONNECTION_ENCODING_UNSET

    def __await__(self) -> Generator[Any, None, None]:
        return self.dispatch().__await__()

    async def dispatch(self) -> None:
        websocket = WebSocket(self.scope, receive=self.receive, send=self.send)
        await self.on_connect(websocket)

        close_code = status.WS_1000_NORMAL_CLOSURE

        try:
            self._connection_encoding = await self.select_encoding(websocket)
            while True:
                message = await websocket.receive()
                if message["type"] == "websocket.receive":
                    data = await self.decode(websocket, message)
                    await self.on_receive(websocket, data)
                elif message["type"] == "websocket.disconnect":  # pragma: no branch
                    close_code = int(message.get("code") or status.WS_1000_NORMAL_CLOSURE)
                    break
        except WebSocketDecodeError as exc:
            close_code = exc.code
            raise
        except Exception as exc:
            close_code = status.WS_1011_INTERNAL_ERROR
            raise exc
        finally:
            await self.on_disconnect(websocket, close_code)

    async def select_encoding(self, websocket: WebSocket) -> Literal["text", "bytes", "json"] | None:
        """
        Choose the message decoding for this connection.

        This is called exactly once per connection, after `on_connect()`, and
        the returned encoding is then fixed for the lifetime of the connection.

        The default implementation honours the `subprotocol_encodings` mapping:
        the negotiated subprotocol (available as `websocket.accepted_subprotocol`
        once `websocket.accept()` has been called) is looked up in the mapping.
        If no subprotocol was negotiated, or the negotiated subprotocol is not
        present in the mapping, the connection is closed with
        1002 (protocol error). If `subprotocol_encodings` is not set, the
        class-level `encoding` is used for every connection.

        Override this method to customise how the encoding is selected.
        """
        if self.subprotocol_encodings is None:
            return self.encoding

        subprotocol = websocket.accepted_subprotocol
        if subprotocol is None:
            await websocket.close(code=status.WS_1002_PROTOCOL_ERROR)
            raise WebSocketDecodeError(
                "Cannot select a message encoding: no subprotocol was negotiated.",
                code=status.WS_1002_PROTOCOL_ERROR,
            )
        if subprotocol not in self.subprotocol_encodings:
            await websocket.close(code=status.WS_1002_PROTOCOL_ERROR)
            raise WebSocketDecodeError(
                f"Cannot select a message encoding: no encoding configured for subprotocol {subprotocol!r}.",
                code=status.WS_1002_PROTOCOL_ERROR,
            )
        return self.subprotocol_encodings[subprotocol]

    async def decode(self, websocket: WebSocket, message: Message) -> Any:
        if self._connection_encoding is _CONNECTION_ENCODING_UNSET:
            encoding = self.encoding
        else:
            encoding = self._connection_encoding

        if encoding == "text":
            if message.get("text") is None:
                await websocket.close(code=status.WS_1003_UNSUPPORTED_DATA)
                raise WebSocketDecodeError("Expected text websocket messages, but got bytes")
            return message["text"]

        elif encoding == "bytes":
            if message.get("bytes") is None:
                await websocket.close(code=status.WS_1003_UNSUPPORTED_DATA)
                raise WebSocketDecodeError("Expected bytes websocket messages, but got text")
            return message["bytes"]

        elif encoding == "json":
            if message.get("text") is not None:
                text = message["text"]
            else:
                text = message["bytes"].decode("utf-8")

            try:
                return json.loads(text)
            except json.decoder.JSONDecodeError:
                await websocket.close(code=status.WS_1003_UNSUPPORTED_DATA)
                raise WebSocketDecodeError("Malformed JSON data received.")

        assert encoding is None, f"Unsupported 'encoding' attribute {encoding}"
        return message["text"] if message.get("text") is not None else message["bytes"]

    async def on_connect(self, websocket: WebSocket) -> None:
        """Override to handle an incoming websocket connection"""
        await websocket.accept()

    async def on_receive(self, websocket: WebSocket, data: Any) -> None:
        """Override to handle an incoming websocket message"""

    async def on_disconnect(self, websocket: WebSocket, close_code: int) -> None:
        """Override to handle a disconnecting websocket"""
