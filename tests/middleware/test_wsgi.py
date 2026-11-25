from __future__ import annotations

import asyncio
from collections.abc import Callable

import pytest

from hypercorn.middleware import AsyncioWSGIMiddleware
from hypercorn.typing import ASGISendEvent, ConnectionState, HTTPScope


def echo_body(environ: dict, start_response: Callable) -> list[bytes]:
    output = environ["wsgi.input"].read()
    start_response("200 OK", [("Content-Length", str(len(output)))])
    return [output]


@pytest.mark.asyncio
async def test_asyncio_wsgi_middleware_streaming_request_body() -> None:
    app = AsyncioWSGIMiddleware(echo_body, stream_request_body=True)
    scope: HTTPScope = {
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
    queue: asyncio.Queue = asyncio.Queue()
    await queue.put({"type": "http.request", "body": b"hel", "more_body": True})
    await queue.put({"type": "http.request", "body": b"lo", "more_body": False})

    messages = []

    async def _send(message: ASGISendEvent) -> None:
        messages.append(message)

    await app(scope, queue.get, _send)
    assert messages == [
        {"headers": [(b"content-length", b"5")], "status": 200, "type": "http.response.start"},
        {"body": b"hello", "type": "http.response.body", "more_body": True},
        {"body": b"", "type": "http.response.body", "more_body": False},
    ]
    assert queue.empty()
