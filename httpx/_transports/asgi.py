from __future__ import annotations

import typing

import anyio
import anyio.abc
from anyio.streams.memory import MemoryObjectReceiveStream

from .._models import Request, Response
from .._types import AsyncByteStream
from .base import AsyncBaseTransport

if typing.TYPE_CHECKING:  # pragma: no cover
    import asyncio

    import trio

    Event = typing.Union[asyncio.Event, trio.Event]


_Message = typing.MutableMapping[str, typing.Any]
_Receive = typing.Callable[[], typing.Awaitable[_Message]]
_Send = typing.Callable[
    [typing.MutableMapping[str, typing.Any]], typing.Awaitable[None]
]
_ASGIApp = typing.Callable[
    [typing.MutableMapping[str, typing.Any], _Receive, _Send], typing.Awaitable[None]
]

__all__ = ["ASGITransport"]


def is_running_trio() -> bool:
    try:
        # sniffio is a dependency of trio.

        # See https://github.com/python-trio/trio/issues/2802
        import sniffio

        if sniffio.current_async_library() == "trio":
            return True
    except ImportError:  # pragma: nocover
        pass

    return False


def create_event() -> Event:
    if is_running_trio():
        import trio

        return trio.Event()

    import asyncio

    return asyncio.Event()


class _ClientDisconnected(Exception):
    """
    Raised into the ASGI application when it attempts to send a response
    message after the client has disconnected.
    """


class ASGIResponseStream(AsyncByteStream):
    def __init__(self, body: list[bytes]) -> None:
        self._body = body

    async def __aiter__(self) -> typing.AsyncIterator[bytes]:
        yield b"".join(self._body)


class ASGIStreamingResponseStream(AsyncByteStream):
    """
    A response stream that is fed by an ASGI application running in a
    background task, so that the response body is produced concurrently
    with the client consuming it.
    """

    def __init__(
        self,
        receive_stream: MemoryObjectReceiveStream[bytes],
        task_group: anyio.abc.TaskGroup,
        disconnect: anyio.Event,
        app_exceptions: list[Exception],
        raise_app_exceptions: bool,
        on_close: typing.Callable[[ASGIStreamingResponseStream], None],
    ) -> None:
        self._receive_stream = receive_stream
        self._task_group = task_group
        self._disconnect = disconnect
        self._app_exceptions = app_exceptions
        self._raise_app_exceptions = raise_app_exceptions
        self._on_close = on_close
        self._closed = False

    async def __aiter__(self) -> typing.AsyncIterator[bytes]:
        try:
            async for chunk in self._receive_stream:
                yield chunk
        finally:
            await self.aclose()
        if self._raise_app_exceptions and self._app_exceptions:
            # The application raised after the response had started.
            raise self._app_exceptions[0]

    async def aclose(self) -> None:
        if self._closed:
            return
        self._closed = True
        # Notify the application that the client has gone away. Blocked
        # `receive()` calls return an `http.disconnect` message, and blocked
        # `send()` calls raise, allowing the application to wind down, so
        # that no tasks are left running once the stream is closed.
        self._disconnect.set()
        self._receive_stream.close()
        await self._task_group.__aexit__(None, None, None)
        self._on_close(self)


class ASGITransport(AsyncBaseTransport):
    """
    A custom AsyncTransport that handles sending requests directly to an ASGI app.

    ```python
    transport = httpx.ASGITransport(
        app=app,
        root_path="/submount",
        client=("1.2.3.4", 123)
    )
    client = httpx.AsyncClient(transport=transport)
    ```

    Arguments:

    * `app` - The ASGI application.
    * `raise_app_exceptions` - Boolean indicating if exceptions in the application
       should be raised. Default to `True`. Can be set to `False` for use cases
       such as testing the content of a client 500 response.
    * `root_path` - The root path on which the ASGI application should be mounted.
    * `client` - A two-tuple indicating the client IP and port of incoming requests.
    * `streaming` - Boolean indicating if the response should be streamed back
       to the client as the application produces it, rather than being buffered
       until the application has completed. Defaults to `False`.
    * `max_buffered_chunks` - The maximum number of response body chunks that
       may be buffered in streaming mode before the application is paused,
       waiting for the client to catch up. Defaults to `16`.
    ```
    """

    def __init__(
        self,
        app: _ASGIApp,
        raise_app_exceptions: bool = True,
        root_path: str = "",
        client: tuple[str, int] = ("127.0.0.1", 123),
        streaming: bool = False,
        max_buffered_chunks: int = 16,
    ) -> None:
        if max_buffered_chunks < 0:
            raise ValueError("max_buffered_chunks must not be negative.")
        self.app = app
        self.raise_app_exceptions = raise_app_exceptions
        self.root_path = root_path
        self.client = client
        self.streaming = streaming
        self.max_buffered_chunks = max_buffered_chunks
        self._open_streams: set[ASGIStreamingResponseStream] = set()

    def _build_scope(self, request: Request) -> dict[str, typing.Any]:
        return {
            "type": "http",
            "asgi": {"version": "3.0"},
            "http_version": "1.1",
            "method": request.method,
            "headers": [(k.lower(), v) for (k, v) in request.headers.raw],
            "scheme": request.url.scheme,
            "path": request.url.path,
            "raw_path": request.url.raw_path.split(b"?")[0],
            "query_string": request.url.query,
            "server": (request.url.host, request.url.port),
            "client": self.client,
            "root_path": self.root_path,
        }

    async def handle_async_request(
        self,
        request: Request,
    ) -> Response:
        if self.streaming:
            return await self._handle_streaming_request(request)

        assert isinstance(request.stream, AsyncByteStream)

        # ASGI scope.
        scope = self._build_scope(request)

        # Request.
        request_body_chunks = request.stream.__aiter__()
        request_complete = False

        # Response.
        status_code = None
        response_headers = None
        body_parts = []
        response_started = False
        response_complete = create_event()

        # ASGI callables.

        async def receive() -> dict[str, typing.Any]:
            nonlocal request_complete

            if request_complete:
                await response_complete.wait()
                return {"type": "http.disconnect"}

            try:
                body = await request_body_chunks.__anext__()
            except StopAsyncIteration:
                request_complete = True
                return {"type": "http.request", "body": b"", "more_body": False}
            return {"type": "http.request", "body": body, "more_body": True}

        async def send(message: typing.MutableMapping[str, typing.Any]) -> None:
            nonlocal status_code, response_headers, response_started

            if message["type"] == "http.response.start":
                assert not response_started

                status_code = message["status"]
                response_headers = message.get("headers", [])
                response_started = True

            elif message["type"] == "http.response.body":
                assert not response_complete.is_set()
                body = message.get("body", b"")
                more_body = message.get("more_body", False)

                if body and request.method != "HEAD":
                    body_parts.append(body)

                if not more_body:
                    response_complete.set()

        try:
            await self.app(scope, receive, send)
        except Exception:  # noqa: PIE-786
            if self.raise_app_exceptions:
                raise

            response_complete.set()
            if status_code is None:
                status_code = 500
            if response_headers is None:
                response_headers = {}

        assert response_complete.is_set()
        assert status_code is not None
        assert response_headers is not None

        stream = ASGIResponseStream(body_parts)

        return Response(status_code, headers=response_headers, stream=stream)

    async def _handle_streaming_request(self, request: Request) -> Response:
        assert isinstance(request.stream, AsyncByteStream)

        # ASGI scope.
        scope = self._build_scope(request)

        # Request.
        request_body_chunks = request.stream.__aiter__()
        request_complete = False

        # Response.
        status_code: int | None = None
        response_headers: list[tuple[bytes, bytes]] | None = None
        app_exceptions: list[Exception] = []

        # Body chunks are passed from the application to the client through
        # a bounded queue, so that a fast application cannot run arbitrarily
        # far ahead of a slow client.
        body_send_stream, body_receive_stream = (
            anyio.create_memory_object_stream[bytes](
                max_buffer_size=self.max_buffered_chunks
            )
        )
        disconnect = anyio.Event()
        response_started = anyio.Event()
        response_complete = anyio.Event()

        # ASGI callables.

        async def receive() -> dict[str, typing.Any]:
            nonlocal request_complete

            if disconnect.is_set() or response_complete.is_set():
                return {"type": "http.disconnect"}

            if request_complete:
                # The request body has been received. Any further `receive()`
                # calls wait for the client to disconnect.
                await disconnect.wait()
                return {"type": "http.disconnect"}

            try:
                body = await request_body_chunks.__anext__()
            except StopAsyncIteration:
                request_complete = True
                return {"type": "http.request", "body": b"", "more_body": False}
            return {"type": "http.request", "body": body, "more_body": True}

        async def send(message: typing.MutableMapping[str, typing.Any]) -> None:
            nonlocal status_code, response_headers

            if disconnect.is_set():
                raise _ClientDisconnected()

            if message["type"] == "http.response.start":
                assert not response_started.is_set()

                status_code = message["status"]
                response_headers = message.get("headers", [])
                response_started.set()

            elif message["type"] == "http.response.body":
                body = message.get("body", b"")
                more_body = message.get("more_body", False)

                if body and request.method != "HEAD":
                    try:
                        await body_send_stream.send(body)
                    except anyio.BrokenResourceError:
                        raise _ClientDisconnected()

                if not more_body:
                    # The response is complete. Signal the end of the body
                    # to the client, and any further `receive()` calls to
                    # the application as an `http.disconnect`.
                    response_complete.set()
                    await body_send_stream.aclose()

        async def run_app() -> None:
            try:
                await self.app(scope, receive, send)
            except _ClientDisconnected:
                pass
            except Exception as exc:  # noqa: PIE-786
                app_exceptions.append(exc)
            finally:
                # Unblock `handle_async_request` if the application exited
                # before sending a response, and signal the end of the body.
                response_started.set()
                await body_send_stream.aclose()

        task_group = anyio.create_task_group()
        await task_group.__aenter__()
        try:
            task_group.start_soon(run_app)
            await response_started.wait()
        except BaseException:
            disconnect.set()
            body_receive_stream.close()
            await task_group.__aexit__(None, None, None)
            raise

        if status_code is None:
            # The application exited before sending a response.
            body_receive_stream.close()
            await task_group.__aexit__(None, None, None)
            if self.raise_app_exceptions and app_exceptions:
                raise app_exceptions[0]
            return Response(500, headers={}, stream=ASGIResponseStream([]))

        stream = ASGIStreamingResponseStream(
            receive_stream=body_receive_stream,
            task_group=task_group,
            disconnect=disconnect,
            app_exceptions=app_exceptions,
            raise_app_exceptions=self.raise_app_exceptions,
            on_close=self._open_streams.discard,
        )
        self._open_streams.add(stream)

        return Response(status_code, headers=response_headers, stream=stream)

    async def aclose(self) -> None:
        # Ensure that no application tasks are left running once the
        # transport is closed.
        streams = list(self._open_streams)
        self._open_streams.clear()
        for stream in streams:
            await stream.aclose()
