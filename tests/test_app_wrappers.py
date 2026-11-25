from __future__ import annotations

import asyncio
from collections.abc import Callable
from functools import partial
from typing import Any

import pytest
import trio

from hypercorn.app_wrappers import (
    _build_environ,
    ClientDisconnectedError,
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


def _stream_scope() -> HTTPScope:
    return {
        "http_version": "1.1",
        "asgi": {},
        "method": "POST",
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


async def _run_streaming_app(
    app: WSGIWrapper, scope: HTTPScope, request_events: list[ASGIReceiveEvent]
) -> tuple[list[ASGISendEvent], asyncio.Queue]:
    queue: asyncio.Queue = asyncio.Queue()
    for event in request_events:
        await queue.put(event)

    messages = []

    async def _send(message: ASGISendEvent) -> None:
        messages.append(message)

    event_loop = asyncio.get_running_loop()

    def _call_soon(func: Callable, *args: Any) -> Any:
        future = asyncio.run_coroutine_threadsafe(func(*args), event_loop)
        return future.result()

    await app(scope, queue.get, _send, partial(event_loop.run_in_executor, None), _call_soon)
    return messages, queue


@pytest.mark.trio
async def test_wsgi_streaming_trio() -> None:
    app = WSGIWrapper(echo_body, 2**16, stream_request_body=True)
    send_channel, receive_channel = trio.open_memory_channel[ASGIReceiveEvent](1)
    await send_channel.send({"type": "http.request", "body": b"hello", "more_body": False})

    messages = []

    async def _send(message: ASGISendEvent) -> None:
        messages.append(message)

    await app(
        _stream_scope(),
        receive_channel.receive,
        _send,
        trio.to_thread.run_sync,
        trio.from_thread.run,
    )
    assert messages == [
        {
            "headers": [(b"content-type", b"text/plain; charset=utf-8"), (b"content-length", b"5")],
            "status": 200,
            "type": "http.response.start",
        },
        {"body": b"hello", "type": "http.response.body", "more_body": True},
        {"body": b"", "type": "http.response.body", "more_body": False},
    ]


@pytest.mark.asyncio
async def test_wsgi_streaming_asyncio() -> None:
    app = WSGIWrapper(echo_body, 2**16, stream_request_body=True)
    messages, queue = await _run_streaming_app(
        app,
        _stream_scope(),
        [
            {"type": "http.request", "body": b"hel", "more_body": True},
            {"type": "http.request", "body": b"lo", "more_body": False},
        ],
    )
    assert messages == [
        {
            "headers": [(b"content-type", b"text/plain; charset=utf-8"), (b"content-length", b"5")],
            "status": 200,
            "type": "http.response.start",
        },
        {"body": b"hello", "type": "http.response.body", "more_body": True},
        {"body": b"", "type": "http.response.body", "more_body": False},
    ]
    assert queue.empty()


@pytest.mark.asyncio
async def test_wsgi_streaming_max_body_size() -> None:
    # The limit is enforced on the cumulative amount received
    app = WSGIWrapper(echo_body, 4, stream_request_body=True)
    messages, queue = await _run_streaming_app(
        app,
        _stream_scope(),
        [
            {"type": "http.request", "body": b"ab", "more_body": True},
            {"type": "http.request", "body": b"cde", "more_body": False},
        ],
    )
    assert messages == [
        {"headers": [], "status": 400, "type": "http.response.start"},
        {"body": b"", "type": "http.response.body", "more_body": False},
    ]
    assert queue.empty()


def read_first_chunk(environ: dict, start_response: Callable) -> list[bytes]:
    chunk = environ["wsgi.input"].read(2)
    start_response("200 OK", [("Content-Length", str(len(chunk)))])
    return [chunk]


@pytest.mark.asyncio
async def test_wsgi_streaming_partial_read_is_drained() -> None:
    # The app only reads the first chunk, the remainder must be
    # drained so the connection can finish cleanly.
    app = WSGIWrapper(read_first_chunk, 2**16, stream_request_body=True)
    messages, queue = await _run_streaming_app(
        app,
        _stream_scope(),
        [
            {"type": "http.request", "body": b"ab", "more_body": True},
            {"type": "http.request", "body": b"cd", "more_body": True},
            {"type": "http.request", "body": b"ef", "more_body": False},
        ],
    )
    assert messages == [
        {"headers": [(b"content-length", b"2")], "status": 200, "type": "http.response.start"},
        {"body": b"ab", "type": "http.response.body", "more_body": True},
        {"body": b"", "type": "http.response.body", "more_body": False},
    ]
    assert queue.empty()


def no_read_no_response(environ: dict, start_response: Callable) -> list[bytes]:
    return []


@pytest.mark.asyncio
async def test_wsgi_streaming_early_exit_max_body_size() -> None:
    # The app exits without reading or responding, the cumulative
    # amount received whilst draining still triggers the max body size.
    app = WSGIWrapper(no_read_no_response, 4, stream_request_body=True)
    messages, queue = await _run_streaming_app(
        app,
        _stream_scope(),
        [{"type": "http.request", "body": b"abcde", "more_body": False}],
    )
    assert messages == [
        {"headers": [], "status": 400, "type": "http.response.start"},
        {"body": b"", "type": "http.response.body", "more_body": False},
    ]
    assert queue.empty()


def read_or_disconnect(environ: dict, start_response: Callable) -> list[bytes]:
    try:
        output = environ["wsgi.input"].read()
    except ClientDisconnectedError:
        start_response("400 Bad Request", [("Content-Length", "0")])
        return [b""]
    else:
        start_response("200 OK", [("Content-Length", str(len(output)))])
        return [output]


@pytest.mark.asyncio
async def test_wsgi_streaming_disconnect() -> None:
    # The client disconnects before the final chunk, the reading app
    # thread gets a failure rather than waiting for an EOF that never
    # comes.
    app = WSGIWrapper(read_or_disconnect, 2**16, stream_request_body=True)
    messages, queue = await _run_streaming_app(
        app,
        _stream_scope(),
        [
            {"type": "http.request", "body": b"ab", "more_body": True},
            {"type": "http.disconnect"},
        ],
    )
    assert messages == [
        {"headers": [(b"content-length", b"0")], "status": 400, "type": "http.response.start"},
        {"body": b"", "type": "http.response.body", "more_body": True},
        {"body": b"", "type": "http.response.body", "more_body": False},
    ]
    assert queue.empty()


def buffer_stats(environ: dict, start_response: Callable) -> list[bytes]:
    stream = environ["wsgi.input"]
    max_buffered = 0
    while True:
        chunk = stream.read(1)
        max_buffered = max(max_buffered, stream.buffered)
        if chunk == b"":
            break
    output = b"%d,%d" % (max_buffered, stream.received)
    start_response("200 OK", [("Content-Length", str(len(output)))])
    return [output]


@pytest.mark.asyncio
async def test_wsgi_streaming_buffered_is_bounded() -> None:
    # Reading byte by byte the buffered amount is bounded by the
    # received chunk size, whilst received tracks the cumulative total.
    app = WSGIWrapper(buffer_stats, 2**16, stream_request_body=True)
    messages, queue = await _run_streaming_app(
        app,
        _stream_scope(),
        [
            {"type": "http.request", "body": b"aaaa", "more_body": True},
            {"type": "http.request", "body": b"bbbb", "more_body": True},
            {"type": "http.request", "body": b"cccc", "more_body": False},
        ],
    )
    assert messages == [
        {"headers": [(b"content-length", b"4")], "status": 200, "type": "http.response.start"},
        {"body": b"3,12", "type": "http.response.body", "more_body": True},
        {"body": b"", "type": "http.response.body", "more_body": False},
    ]
    assert queue.empty()
