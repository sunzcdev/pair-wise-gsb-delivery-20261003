from __future__ import annotations

import asyncio
import contextlib
import contextvars
import json
import logging
import signal
import socket
import sys
from collections.abc import Callable, Generator
from contextlib import AbstractContextManager
from typing import cast

import httpx2
import pytest

from tests.protocols.test_http import SIMPLE_GET_REQUEST
from tests.utils import run_server
from uvicorn._types import ASGIApplication, ASGIReceiveCallable, ASGISendCallable, Scope
from uvicorn.config import STARTUP_FAILURE, Config
from uvicorn.protocols.http.flow_control import HIGH_WATER_LIMIT
from uvicorn.protocols.http.h11_impl import H11Protocol
from uvicorn.protocols.http.httptools_impl import HttpToolsProtocol
from uvicorn.server import Server

pytestmark = pytest.mark.anyio


# asyncio does NOT allow raising in signal handlers, so to detect
# raised signals raised a mutable `witness` receives the signal
@contextlib.contextmanager
def capture_signal_sync(sig: signal.Signals) -> Generator[list[int], None, None]:
    """Replace `sig` handling with a normal exception via `signal"""
    witness: list[int] = []
    original_handler = signal.signal(sig, lambda signum, frame: witness.append(signum))
    yield witness
    signal.signal(sig, original_handler)


@contextlib.contextmanager
def capture_signal_async(sig: signal.Signals) -> Generator[list[int], None, None]:  # pragma: py-win32
    """Replace `sig` handling with a normal exception via `asyncio"""
    witness: list[int] = []
    original_handler = signal.getsignal(sig)
    asyncio.get_running_loop().add_signal_handler(sig, witness.append, sig)
    yield witness
    signal.signal(sig, original_handler)


async def dummy_app(scope, receive, send):  # pragma: py-win32
    pass


async def app(scope: Scope, receive: ASGIReceiveCallable, send: ASGISendCallable) -> None:
    assert scope["type"] == "http"
    await send({"type": "http.response.start", "status": 200, "headers": []})
    await send({"type": "http.response.body", "body": b"", "more_body": False})


if sys.platform == "win32":  # pragma: py-not-win32
    signals = [signal.SIGBREAK]
    signal_captures = [capture_signal_sync]
else:  # pragma: py-win32
    signals = [signal.SIGTERM, signal.SIGINT]
    signal_captures = [capture_signal_sync, capture_signal_async]


@pytest.mark.parametrize("exception_signal", signals)
@pytest.mark.parametrize("capture_signal", signal_captures)
async def test_server_interrupt(
    exception_signal: signal.Signals,
    capture_signal: Callable[[signal.Signals], AbstractContextManager[None]],
    unused_tcp_port: int,
):  # pragma: py-win32
    """Test interrupting a Server that is run explicitly inside asyncio"""

    async def interrupt_running(srv: Server):
        while not srv.started:
            await asyncio.sleep(0.01)
        signal.raise_signal(exception_signal)

    server = Server(Config(app=dummy_app, loop="asyncio", port=unused_tcp_port))
    asyncio.create_task(interrupt_running(server))
    with capture_signal(exception_signal) as witness:
        await server.serve()
    assert witness
    # set by the server's graceful exit handler
    assert server.should_exit


async def test_shutdown_on_early_exit_during_startup(unused_tcp_port: int):
    """Test that lifespan.shutdown is called even when should_exit is set during startup."""
    startup_complete = False
    shutdown_complete = False

    async def app(scope: Scope, receive: ASGIReceiveCallable, send: ASGISendCallable) -> None:
        nonlocal startup_complete, shutdown_complete
        if scope["type"] == "lifespan":
            while True:
                message = await receive()
                if message["type"] == "lifespan.startup":
                    await asyncio.sleep(0.5)
                    await send({"type": "lifespan.startup.complete"})
                    startup_complete = True
                elif message["type"] == "lifespan.shutdown":
                    await send({"type": "lifespan.shutdown.complete"})
                    shutdown_complete = True
                    return

    config = Config(app=app, lifespan="on", port=unused_tcp_port)
    server = Server(config=config)

    # Simulate a reload signal arriving during startup:
    # set should_exit before the 0.5s startup sleep finishes.
    async def set_exit():
        await asyncio.sleep(0.2)
        server.should_exit = True

    asyncio.create_task(set_exit())
    await server.serve()

    assert startup_complete
    assert shutdown_complete, "lifespan.shutdown was not called despite startup completing"


def test_run_exits_with_startup_failure_on_unloadable_app() -> None:
    """A server exits with the dedicated startup-failure code when the app can't load.

    The multiprocess supervisor relies on `STARTUP_FAILURE` to tell a worker that
    can never boot apart from a worker crashing at runtime, and stops instead of
    restarting it forever. Regression for https://github.com/encode/uvicorn/discussions/2440.
    """
    config = Config(app="tests.test_server:does_not_exist")
    server = Server(config=config)

    with pytest.raises(SystemExit) as exc_info:
        server.run()
    assert exc_info.value.code == STARTUP_FAILURE


async def test_request_than_limit_max_requests_warn_log(
    unused_tcp_port: int, http_protocol_cls: type[H11Protocol | HttpToolsProtocol], caplog: pytest.LogCaptureFixture
):
    caplog.set_level(logging.INFO, logger="uvicorn.error")
    config = Config(app=app, limit_max_requests=1, port=unused_tcp_port, http=http_protocol_cls)
    async with run_server(config):
        async with httpx2.AsyncClient() as client:
            tasks = [client.get(f"http://127.0.0.1:{unused_tcp_port}") for _ in range(2)]
            responses = await asyncio.gather(*tasks)
            assert len(responses) == 2
    assert "Maximum request limit of 1 exceeded. Terminating process." in caplog.text


async def test_limit_max_requests_jitter(
    unused_tcp_port: int, http_protocol_cls: type[H11Protocol | HttpToolsProtocol], caplog: pytest.LogCaptureFixture
):
    caplog.set_level(logging.INFO, logger="uvicorn.error")
    config = Config(
        app=app, limit_max_requests=1, limit_max_requests_jitter=2, port=unused_tcp_port, http=http_protocol_cls
    )
    async with run_server(config) as server:
        limit = server.limit_max_requests
        assert limit is not None
        assert 1 <= limit <= 3
        async with httpx2.AsyncClient() as client:
            tasks = [client.get(f"http://127.0.0.1:{unused_tcp_port}") for _ in range(limit + 1)]
            await asyncio.gather(*tasks)
    assert f"Maximum request limit of {limit} exceeded. Terminating process." in caplog.text


@contextlib.asynccontextmanager
async def _raw_server(
    *,
    app: ASGIApplication,
    port: int,
    http_protocol_cls: type[H11Protocol | HttpToolsProtocol],
    reset_contextvars: bool = False,
):
    config = Config(app=app, port=port, loop="asyncio", http=http_protocol_cls, reset_contextvars=reset_contextvars)
    server = Server(config=config)
    task = asyncio.create_task(server.serve())

    while not server.started:
        await asyncio.sleep(0.01)

    reader, writer = await asyncio.open_connection("127.0.0.1", port)

    async def extract_json_body(request: bytes):
        writer.write(request)
        await writer.drain()

        status, *headers = (await reader.readuntil(b"\r\n\r\n")).split(b"\r\n")[:-2]
        assert status == b"HTTP/1.1 200 OK"

        content_length = next(int(h.split(b":", 1)[1]) for h in headers if h.lower().startswith(b"content-length:"))
        return json.loads(await reader.readexactly(content_length))

    try:
        yield extract_json_body
    finally:
        writer.close()
        await writer.wait_closed()
        server.should_exit = True
        await task


async def test_contextvars_preserved_by_default(
    http_protocol_cls: type[H11Protocol | HttpToolsProtocol], unused_tcp_port: int
):
    """By default, context set outside the ASGI task is visible inside it."""
    ctx: contextvars.ContextVar[str] = contextvars.ContextVar("ctx")
    ctx.set("outer-value")

    async def app(scope: Scope, receive: ASGIReceiveCallable, send: ASGISendCallable):
        assert scope["type"] == "http"
        while True:
            message = await receive()
            assert message["type"] == "http.request"
            if not message["more_body"]:
                break
        body = json.dumps({"ctx": ctx.get("MISSING")}).encode("utf-8")
        headers = [(b"content-type", b"application/json"), (b"content-length", str(len(body)).encode("utf-8"))]
        await send({"type": "http.response.start", "status": 200, "headers": headers})
        await send({"type": "http.response.body", "body": body})

    async with _raw_server(app=app, http_protocol_cls=http_protocol_cls, port=unused_tcp_port) as extract_json_body:
        assert await extract_json_body(SIMPLE_GET_REQUEST) == {"ctx": "outer-value"}


async def test_reset_contextvars_asyncio(
    http_protocol_cls: type[H11Protocol | HttpToolsProtocol], unused_tcp_port: int
):
    """With reset_contextvars=True, each ASGI run starts with a fresh context.

    Non-regression test for https://github.com/encode/uvicorn/issues/2167.
    """
    default_contextvars = {c.name for c in contextvars.copy_context().keys()}
    ctx: contextvars.ContextVar[str] = contextvars.ContextVar("ctx")

    async def app(scope: Scope, receive: ASGIReceiveCallable, send: ASGISendCallable):
        assert scope["type"] == "http"

        # initial context should be empty
        initial_context = {
            n: v for c, v in contextvars.copy_context().items() if (n := c.name) not in default_contextvars
        }
        # set any contextvar before the body is read
        ctx.set(scope["path"])

        while True:
            message = await receive()
            assert message["type"] == "http.request"
            if not message["more_body"]:
                break

        body = json.dumps(initial_context).encode("utf-8")
        headers = [(b"content-type", b"application/json"), (b"content-length", str(len(body)).encode("utf-8"))]
        await send({"type": "http.response.start", "status": 200, "headers": headers})
        await send({"type": "http.response.body", "body": body})

    # body larger than HIGH_WATER_LIMIT forces a reading pause on the main thread
    # and a resumption inside the ASGI task, which is where the original pollution showed up.
    large_body = b"a" * (HIGH_WATER_LIMIT + 1)
    large_request = b"\r\n".join(
        [
            b"POST /large-body HTTP/1.1",
            b"Host: example.org",
            b"Content-Type: application/octet-stream",
            f"Content-Length: {len(large_body)}".encode(),
            b"",
            large_body,
        ]
    )

    async with _raw_server(
        app=app, http_protocol_cls=http_protocol_cls, port=unused_tcp_port, reset_contextvars=True
    ) as extract_json_body:
        assert await extract_json_body(large_request) == {}
        assert await extract_json_body(SIMPLE_GET_REQUEST) == {}


def _bound_listening_socket() -> socket.socket:
    """A bound, listening TCP socket whose owner is the caller (a "parent process")."""
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.bind(("127.0.0.1", 0))
    sock.listen()
    return sock


def _connection_refused(address: tuple[str, int]) -> bool:
    """True when nothing is accepting TCP connections at ``address``.

    Uses a blocking socket on purpose: an asyncio client that fails to connect
    leaves an unclosed stream transport behind, which the test suite treats as
    an error.
    """
    probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    probe.settimeout(2)
    try:
        probe.connect(address)
    except OSError:
        return True
    else:  # pragma: no cover - only reached if an entry point was left behind
        return False
    finally:
        probe.close()


async def test_multi_listener_rollback_keeps_borrowed_sockets_usable():
    """First listener starts, second fails to initialise: roll back cleanly.

    The borrowed sockets are owned by the caller (modelled parent process).
    On rollback the server this process created must stop accepting, the
    lifespan must be shut down, and the parent must still own usable,
    connectable handles -- the server never closes them.
    """
    parent_sockets = [_bound_listening_socket(), _bound_listening_socket()]
    first_addr = parent_sockets[0].getsockname()
    second_addr = parent_sockets[1].getsockname()

    # Simulate the second listener failing to initialise: its handle is dead.
    parent_sockets[1].close()

    lifespan_events: list[str] = []

    async def lifespan_app(scope: Scope, receive: ASGIReceiveCallable, send: ASGISendCallable) -> None:
        if scope["type"] != "lifespan":  # pragma: no cover
            return
        while True:
            message = await receive()
            if message["type"] == "lifespan.startup":
                lifespan_events.append("startup")
                await send({"type": "lifespan.startup.complete"})
            elif message["type"] == "lifespan.shutdown":
                lifespan_events.append("shutdown")
                await send({"type": "lifespan.shutdown.complete"})
                return

    config = Config(app=lifespan_app, lifespan="on", loop="asyncio")
    server = Server(config=config)

    with pytest.raises(OSError):
        await server.serve(sockets=parent_sockets)

    # The failed startup never flipped the "started" flag, so _serve did not
    # run the normal shutdown path either.
    assert server.started is False

    # The already-started lifespan got a matching shutdown event.
    assert lifespan_events == ["startup", "shutdown"]

    # The server this process created on the first listener stopped accepting.
    assert all(not created.is_serving() for created in server.servers)

    # The borrowed first handle is still owned by the parent and usable: the
    # parent can accept a connection on it itself. The rollback never closed
    # it; on loops where asyncio closed the underlying fd, a fresh listening
    # handle at the same address was put back in the parent's list.
    first_handle = parent_sockets[0]
    assert first_handle.fileno() != -1
    first_handle.settimeout(2)
    client = socket.create_connection(first_addr, timeout=2)
    accepted, _ = first_handle.accept()
    accepted.close()
    client.close()

    # The failed second handle stays dead; nothing re-opened a listener there.
    assert parent_sockets[1].fileno() == -1
    assert _connection_refused(second_addr)

    first_handle.close()


class _NonClosingServer:
    """Stand-in for an asyncio.Server whose loop only stops serving, keeping the fd open."""

    def __init__(self) -> None:
        self.closed = False

    def close(self) -> None:
        self.closed = True

    async def wait_closed(self) -> None:
        return None


async def test_rollback_preserves_handle_when_loop_keeps_fd_open():
    """When the loop keeps the listening fd open, the borrowed object is left in place.

    The process-created server is still told to stop accepting, but the borrowed
    handle and its file descriptor are preserved (not replaced) and remain usable
    by the owner, since this process never owned the handle.
    """
    borrowed = _bound_listening_socket()
    address = borrowed.getsockname()
    original_fd = borrowed.fileno()
    sockets = [borrowed]

    config = Config(app=dummy_app, lifespan="off", loop="asyncio")
    config.load()
    server = Server(config=config)
    server.lifespan = config.lifespan_class(config)

    fake_server = _NonClosingServer()
    await server._rollback_borrowed_sockets(
        sockets, [(0, cast(asyncio.base_events.Server, fake_server))]
    )

    # The process-created server was asked to stop accepting...
    assert fake_server.closed
    # ...while the borrowed handle and its fd were preserved, not replaced.
    assert sockets[0] is borrowed
    assert borrowed.fileno() == original_fd

    # And the owner can still accept connections on it itself.
    borrowed.settimeout(2)
    client = socket.create_connection(address, timeout=2)
    accepted, _ = borrowed.accept()
    accepted.close()
    client.close()
    borrowed.close()

