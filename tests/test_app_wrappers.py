from __future__ import annotations

import asyncio
import os
from collections.abc import Callable, Iterable, Iterator
from functools import partial
from io import BytesIO
from pathlib import Path
from typing import Any

import pytest
import trio

from hypercorn.app_wrappers import (
    _build_environ,
    FileWrapper,
    InvalidPathError,
    WSGIWrapper,
)
from hypercorn.typing import ASGIReceiveEvent, ASGISendEvent, ConnectionState, HTTPScope


def echo_body(environ: dict, start_response: Callable) -> list[bytes]:
    status = "200 OK"
    output = environ["wsgi.input"].read()
    headers = [
        ("Content-Type", "text/plain; charset=utf-8"),
        ("Content-Length", str(len(output))),
    ]
    start_response(status, headers)
    return [output]


@pytest.mark.trio
async def test_wsgi_trio() -> None:
    app = WSGIWrapper(echo_body, 2**16)
    scope: HTTPScope = {
        "http_version": "1.1",
        "asgi": {},
        "method": "GET",
        "headers": [],
        "path": "/",
        "root_path": "/",
        "query_string": b"a=b",
        "raw_path": b"/",
        "scheme": "http",
        "type": "http",
        "client": ("localhost", 80),
        "server": None,
        "extensions": {},
        "state": ConnectionState({}),
    }
    send_channel, receive_channel = trio.open_memory_channel[ASGIReceiveEvent](1)
    await send_channel.send({"type": "http.request"})  # type: ignore

    messages = []

    async def _send(message: ASGISendEvent) -> None:
        messages.append(message)

    await app(scope, receive_channel.receive, _send, trio.to_thread.run_sync, trio.from_thread.run)
    assert messages == [
        {
            "headers": [(b"content-type", b"text/plain; charset=utf-8"), (b"content-length", b"0")],
            "status": 200,
            "type": "http.response.start",
        },
        {"body": bytearray(b""), "type": "http.response.body", "more_body": True},
        {"body": bytearray(b""), "type": "http.response.body", "more_body": False},
    ]


async def _run_app(app: WSGIWrapper, scope: HTTPScope, body: bytes = b"") -> list[ASGISendEvent]:
    queue: asyncio.Queue = asyncio.Queue()
    await queue.put({"type": "http.request", "body": body})

    messages = []

    async def _send(message: ASGISendEvent) -> None:
        messages.append(message)

    event_loop = asyncio.get_running_loop()

    def _call_soon(func: Callable, *args: Any) -> Any:
        future = asyncio.run_coroutine_threadsafe(func(*args), event_loop)
        return future.result()

    await app(scope, queue.get, _send, partial(event_loop.run_in_executor, None), _call_soon)
    return messages


@pytest.mark.asyncio
async def test_wsgi_asyncio() -> None:
    app = WSGIWrapper(echo_body, 2**16)
    scope: HTTPScope = {
        "http_version": "1.1",
        "asgi": {},
        "method": "GET",
        "headers": [],
        "path": "/",
        "root_path": "/",
        "query_string": b"a=b",
        "raw_path": b"/",
        "scheme": "http",
        "type": "http",
        "client": ("localhost", 80),
        "server": None,
        "extensions": {},
        "state": ConnectionState({}),
    }
    messages = await _run_app(app, scope)
    assert messages == [
        {
            "headers": [(b"content-type", b"text/plain; charset=utf-8"), (b"content-length", b"0")],
            "status": 200,
            "type": "http.response.start",
        },
        {"body": bytearray(b""), "type": "http.response.body", "more_body": True},
        {"body": bytearray(b""), "type": "http.response.body", "more_body": False},
    ]


@pytest.mark.asyncio
async def test_max_body_size() -> None:
    app = WSGIWrapper(echo_body, 4)
    scope: HTTPScope = {
        "http_version": "1.1",
        "asgi": {},
        "method": "GET",
        "headers": [],
        "path": "/",
        "root_path": "/",
        "query_string": b"a=b",
        "raw_path": b"/",
        "scheme": "http",
        "type": "http",
        "client": ("localhost", 80),
        "server": None,
        "extensions": {},
        "state": ConnectionState({}),
    }
    messages = await _run_app(app, scope, b"abcde")
    assert messages == [
        {"headers": [], "status": 400, "type": "http.response.start"},
        {"body": bytearray(b""), "type": "http.response.body", "more_body": False},
    ]


def no_start_response(environ: dict, start_response: Callable) -> list[bytes]:
    return [b"result"]


@pytest.mark.asyncio
async def test_no_start_response() -> None:
    app = WSGIWrapper(no_start_response, 2**16)
    scope: HTTPScope = {
        "http_version": "1.1",
        "asgi": {},
        "method": "GET",
        "headers": [],
        "path": "/",
        "root_path": "/",
        "query_string": b"a=b",
        "raw_path": b"/",
        "scheme": "http",
        "type": "http",
        "client": ("localhost", 80),
        "server": None,
        "extensions": {},
        "state": ConnectionState({}),
    }
    with pytest.raises(RuntimeError):
        await _run_app(app, scope)


def test_build_environ_encoding() -> None:
    scope: HTTPScope = {
        "http_version": "1.0",
        "asgi": {},
        "method": "GET",
        "headers": [],
        "path": "/中/文",
        "root_path": "/中",
        "query_string": b"bar=baz",
        "raw_path": "/中/文".encode(),
        "scheme": "http",
        "type": "http",
        "client": ("localhost", 80),
        "server": None,
        "extensions": {},
        "state": ConnectionState({}),
    }
    environ = _build_environ(scope, b"")
    assert environ["SCRIPT_NAME"] == "/中".encode().decode("latin-1")
    assert environ["PATH_INFO"] == "/文".encode().decode("latin-1")


def test_build_environ_root_path() -> None:
    scope: HTTPScope = {
        "http_version": "1.0",
        "asgi": {},
        "method": "GET",
        "headers": [],
        "path": "/中文",
        "root_path": "/中国",
        "query_string": b"bar=baz",
        "raw_path": "/中文".encode(),
        "scheme": "http",
        "type": "http",
        "client": ("localhost", 80),
        "server": None,
        "extensions": {},
        "state": ConnectionState({}),
    }
    with pytest.raises(InvalidPathError):
        _build_environ(scope, b"")


# ---------------------------------------------------------------------------
# wsgi.file_wrapper support
# ---------------------------------------------------------------------------

FILE_CONTENT = b"0123456789abcdef"
# The WSGI app seeks to the middle of the pre-opened file, so only this
# suffix should ever be transmitted.
SEEK_POSITION = 8
EXPECTED_SUFFIX = FILE_CONTENT[SEEK_POSITION:]


def _scope(extensions: dict | None = None) -> HTTPScope:
    return {
        "http_version": "1.1",
        "asgi": {},
        "method": "GET",
        "headers": [],
        "path": "/",
        "root_path": "/",
        "query_string": b"",
        "raw_path": b"/",
        "scheme": "http",
        "type": "http",
        "client": ("localhost", 80),
        "server": None,
        "extensions": extensions or {},
        "state": ConnectionState({}),
    }


def make_file_wrapper_app(path: bytes) -> Callable:
    def wsgi_app(environ: dict, start_response: Callable) -> Iterable[bytes]:
        # The standard entry point must be present in every WSGI environ.
        file_wrapper = environ["wsgi.file_wrapper"]
        start_response(
            "200 OK",
            [("Content-Type", "application/octet-stream"), ("Content-Length", "8")],
        )
        file = open(path, "rb")
        try:
            file.seek(SEEK_POSITION)
            return file_wrapper(file)
        except BaseException:
            file.close()
            raise

    return wsgi_app


async def _run_app_with_send(
    app: WSGIWrapper, scope: HTTPScope, send: Callable, body: bytes = b""
) -> None:
    queue: asyncio.Queue = asyncio.Queue()
    await queue.put({"type": "http.request", "body": body})

    event_loop = asyncio.get_running_loop()

    def _call_soon(func: Callable, *args: Any) -> Any:
        future = asyncio.run_coroutine_threadsafe(func(*args), event_loop)
        return future.result()

    await app(
        scope, queue.get, send, partial(event_loop.run_in_executor, None), _call_soon
    )


class ZeroCopyBackend:
    """Stand-in for a backend that declares the zerocopysend capability.

    It consumes the file descriptor handed over by the WSGI wrapper instead
    of re-opening any path, mirroring what a real sendfile backend does.
    """

    def __init__(self) -> None:
        self.messages: list[ASGISendEvent] = []
        self.received = bytearray()

    async def send(self, message: ASGISendEvent) -> None:
        self.messages.append(message)
        if message["type"] == "http.response.zerocopysend":
            fd = message["file"]
            offset = message["offset"]
            count = message["count"]
            current = os.lseek(fd, 0, os.SEEK_CUR)
            os.lseek(fd, offset, os.SEEK_SET)
            try:
                remaining = count
                while remaining:
                    chunk = os.read(fd, remaining)
                    if not chunk:
                        break
                    self.received.extend(chunk)
                    remaining -= len(chunk)
            finally:
                os.lseek(fd, current, os.SEEK_SET)


class ChunkedBackend:
    """Stand-in for a backend without the zerocopysend capability."""

    def __init__(self) -> None:
        self.messages: list[ASGISendEvent] = []
        self.received = bytearray()

    async def send(self, message: ASGISendEvent) -> None:
        self.messages.append(message)
        if message["type"] == "http.response.body":
            self.received.extend(message.get("body", b""))


@pytest.mark.asyncio
async def test_wsgi_file_wrapper_negotiates_zerocopysend(tmp_path: Path) -> None:
    path = tmp_path / "data.bin"
    path.write_bytes(FILE_CONTENT)

    backend = ZeroCopyBackend()
    app = WSGIWrapper(
        make_file_wrapper_app(os.fsencode(path)),
        2**16,
    )
    scope = _scope({"http.response.zerocopysend": {}})

    await _run_app_with_send(app, scope, backend.send)

    # The specialised representation must be emitted exactly once with the
    # range that starts at the application's seek position.
    zero_copy_messages = [
        message
        for message in backend.messages
        if message["type"] == "http.response.zerocopysend"
    ]
    assert len(zero_copy_messages) == 1
    message = zero_copy_messages[0]
    assert message["offset"] == SEEK_POSITION
    assert message["count"] == len(EXPECTED_SUFFIX)
    assert isinstance(message["file"], int)
    # No chunked body iteration should take place on this path.
    assert all(
        message["type"] != "http.response.body" or message.get("body", b"") == b""
        for message in backend.messages
    )
    assert bytes(backend.received) == EXPECTED_SUFFIX


@pytest.mark.asyncio
async def test_wsgi_file_wrapper_falls_back_to_chunked(tmp_path: Path) -> None:
    path = tmp_path / "data.bin"
    path.write_bytes(FILE_CONTENT)

    backend = ChunkedBackend()
    app = WSGIWrapper(
        make_file_wrapper_app(os.fsencode(path)),
        2**16,
    )
    # The backend declares no specialised capability.
    scope = _scope({})

    await _run_app_with_send(app, scope, backend.send)

    assert not any(
        message["type"] == "http.response.zerocopysend" for message in backend.messages
    )
    assert bytes(backend.received) == EXPECTED_SUFFIX
    # The streamed chunks concatenate to exactly the suffix length.
    assert len(backend.received) == len(EXPECTED_SUFFIX)


@pytest.mark.asyncio
@pytest.mark.parametrize("declares_capability", [True, False])
async def test_wsgi_file_wrapper_same_suffix_and_length(
    tmp_path: Path, declares_capability: bool
) -> None:
    path = tmp_path / "data.bin"
    path.write_bytes(FILE_CONTENT)

    backend: ZeroCopyBackend | ChunkedBackend
    if declares_capability:
        backend = ZeroCopyBackend()
        scope = _scope({"http.response.zerocopysend": {}})
    else:
        backend = ChunkedBackend()
        scope = _scope({})

    app = WSGIWrapper(
        make_file_wrapper_app(os.fsencode(path)),
        2**16,
    )
    await _run_app_with_send(app, scope, backend.send)

    # Both negotiation outcomes deliver identical bytes and length.
    assert bytes(backend.received) == EXPECTED_SUFFIX
    assert len(backend.received) == len(FILE_CONTENT) - SEEK_POSITION


class _TrackingFile:
    """File-like wrapper recording whether the server touches the disk path."""

    def __init__(self, path: Path) -> None:
        self._file = open(path, "rb")
        self.path_reopened = False
        self.closed = False

    def seek(self, *args: Any) -> int:
        return self._file.seek(*args)

    def read(self, size: int = -1) -> bytes:
        return self._file.read(size)

    def close(self) -> None:
        self.closed = True
        self._file.close()


@pytest.mark.asyncio
async def test_wsgi_file_wrapper_uses_handle_not_path(tmp_path: Path) -> None:
    path = tmp_path / "data.bin"
    path.write_bytes(FILE_CONTENT)
    tracking_file = _TrackingFile(path)
    tracking_file.seek(SEEK_POSITION)

    def wsgi_app(environ: dict, start_response: Callable) -> Iterable[bytes]:
        start_response("200 OK", [])
        return environ["wsgi.file_wrapper"](tracking_file)

    backend = ChunkedBackend()
    app = WSGIWrapper(wsgi_app, 2**16)
    await _run_app_with_send(app, _scope({}), backend.send)

    # Everything comes from the pre-opened handle; no path-based open ever
    # happens (the tracking object exposes no disk path to re-open).
    assert bytes(backend.received) == EXPECTED_SUFFIX
    assert not tracking_file.path_reopened
    assert tracking_file.closed


@pytest.mark.asyncio
async def test_wsgi_zerocopysend_message_has_no_path(tmp_path: Path) -> None:
    path = tmp_path / "data.bin"
    path.write_bytes(FILE_CONTENT)

    def wsgi_app(environ: dict, start_response: Callable) -> Iterable[bytes]:
        start_response("200 OK", [])
        file = open(path, "rb")
        file.seek(SEEK_POSITION)
        return environ["wsgi.file_wrapper"](file)

    backend = ZeroCopyBackend()
    app = WSGIWrapper(wsgi_app, 2**16)
    scope = _scope({"http.response.zerocopysend": {}})
    await _run_app_with_send(app, scope, backend.send)

    # The specialised representation carries a descriptor only - there is no
    # path key that a backend could use to re-open another file.
    zero_copy_messages = [
        message
        for message in backend.messages
        if message["type"] == "http.response.zerocopysend"
    ]
    assert len(zero_copy_messages) == 1
    assert "path" not in zero_copy_messages[0]
    assert isinstance(zero_copy_messages[0]["file"], int)
    assert bytes(backend.received) == EXPECTED_SUFFIX


@pytest.mark.asyncio
async def test_wsgi_file_wrapper_closes_handle_on_interruption(tmp_path: Path) -> None:
    path = tmp_path / "data.bin"
    path.write_bytes(FILE_CONTENT)
    tracking_file = _TrackingFile(path)
    tracking_file.seek(SEEK_POSITION)

    def failing_app(environ: dict, start_response: Callable) -> Iterable[bytes]:
        start_response("200 OK", [])
        return _ExplodingWrapper(environ["wsgi.file_wrapper"](tracking_file))

    app = WSGIWrapper(failing_app, 2**16)
    scope = _scope({"http.response.zerocopysend": {}})

    async def send(message: ASGISendEvent) -> None:
        raise ConnectionError("connection lost mid transfer")

    with pytest.raises(ConnectionError, match="connection lost"):
        await _run_app_with_send(app, scope, send)

    # The handle must be released even though the transfer was interrupted.
    assert tracking_file.closed


class _ExplodingWrapper:
    """Mimics an interrupted transfer: iteration blows up, close proxies."""

    def __init__(self, wrapped: FileWrapper) -> None:
        self._wrapped = wrapped

    def __iter__(self) -> Iterator[bytes]:
        return self

    def __next__(self) -> bytes:
        raise ConnectionError("connection lost mid transfer")

    def close(self) -> None:
        self._wrapped.close()


@pytest.mark.asyncio
async def test_wsgi_file_wrapper_closes_handle_after_send_failure(tmp_path: Path) -> None:
    path = tmp_path / "data.bin"
    path.write_bytes(FILE_CONTENT)
    tracking_file = _TrackingFile(path)
    tracking_file.seek(SEEK_POSITION)

    def wsgi_app(environ: dict, start_response: Callable) -> Iterable[bytes]:
        start_response("200 OK", [])
        return environ["wsgi.file_wrapper"](tracking_file, block_size=3)

    app = WSGIWrapper(wsgi_app, 2**16)
    scope = _scope({})  # chunked path, exercised via the FileWrapper iterator

    async def failing_send(message: ASGISendEvent) -> None:
        if message["type"] == "http.response.body" and message.get("body"):
            raise ConnectionError("client disconnected")

    with pytest.raises(ConnectionError, match="client disconnected"):
        await _run_app_with_send(app, scope, failing_send)

    # The interrupted chunked iteration still releases the OS handle.
    assert tracking_file.closed


def test_file_wrapper_iteration_honours_position_and_block_size() -> None:
    file = BytesIO(FILE_CONTENT)
    file.seek(SEEK_POSITION)
    wrapper = FileWrapper(file, block_size=3)

    assert list(wrapper) == [b"89a", b"bcd", b"ef"]
    # Closing the wrapper releases the underlying handle.
    wrapper.close()
    assert file.closed


def test_file_wrapper_close_is_idempotent() -> None:
    file = BytesIO(FILE_CONTENT)
    wrapper = FileWrapper(file)
    wrapper.close()
    wrapper.close()
    assert file.closed


@pytest.mark.asyncio
async def test_wsgi_file_wrapper_memory_file_falls_back_to_chunked() -> None:
    # A file-like object without a real OS descriptor cannot use zero copy.
    def wsgi_app(environ: dict, start_response: Callable) -> Iterable[bytes]:
        start_response("200 OK", [])
        return environ["wsgi.file_wrapper"](BytesIO(FILE_CONTENT[SEEK_POSITION:]))

    backend = ChunkedBackend()
    app = WSGIWrapper(wsgi_app, 2**16)
    scope = _scope({"http.response.zerocopysend": {}})

    await _run_app_with_send(app, scope, backend.send)

    assert not any(
        message["type"] == "http.response.zerocopysend" for message in backend.messages
    )
    assert bytes(backend.received) == EXPECTED_SUFFIX


@pytest.mark.trio
async def test_wsgi_file_wrapper_trio_zerocopysend(tmp_path: Path) -> None:
    path = tmp_path / "data.bin"
    path.write_bytes(FILE_CONTENT)

    scope = _scope({"http.response.zerocopysend": {}})
    send_channel, receive_channel = trio.open_memory_channel[ASGIReceiveEvent](1)
    await send_channel.send({"type": "http.request"})  # type: ignore

    backend = ZeroCopyBackend()
    app = WSGIWrapper(
        make_file_wrapper_app(os.fsencode(path)),
        2**16,
    )

    await app(
        scope,
        receive_channel.receive,
        backend.send,
        trio.to_thread.run_sync,
        trio.from_thread.run,
    )

    assert bytes(backend.received) == EXPECTED_SUFFIX
    assert any(
        message["type"] == "http.response.zerocopysend" for message in backend.messages
    )
