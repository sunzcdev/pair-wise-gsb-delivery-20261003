import asyncio
from typing import Any

from uvicorn._types import ASGIReceiveCallable, ASGISendCallable, Scope

CLOSE_HEADER = (b"connection", b"close")

HIGH_WATER_LIMIT = 65536


class ActiveRequestLimiter:
    """Tracks requests that are about to enter (or are inside) the ASGI application.

    Unlike ``limit_concurrency`` -- whose open-connection count also charges idle
    keep-alive connections and whose task count charges requests still waiting in
    the HTTP/1 pipeline -- a slot here is only taken once a request is admitted for
    execution. Keep-alive idle time and requests queued behind a pipelined response
    therefore do not consume the budget. The shared set of slots lives on the
    :class:`~uvicorn.server.ServerState`, so all connections of a worker compete for
    the same budget.
    """

    def __init__(self, limit: int | None, active_requests: set[Any]) -> None:
        self.limit = limit
        self.active_requests = active_requests

    @property
    def enabled(self) -> bool:
        return self.limit is not None

    def admit(self, token: Any) -> bool:
        """Try to reserve a slot for ``token``.

        Runs synchronously inside the event loop's protocol callback, so the
        check-and-reserve sequence cannot be interleaved with another request.
        Returns ``True`` when a slot was reserved, ``False`` when the limit was
        already reached (the caller must answer with a 503).
        """
        if self.limit is None:
            self.active_requests.add(token)
            return True
        if len(self.active_requests) >= self.limit:
            return False
        self.active_requests.add(token)
        return True

    def release(self, token: Any) -> None:
        """Give the slot back. Safe to call more than once for the same token."""
        self.active_requests.discard(token)


class FlowControl:
    def __init__(self, transport: asyncio.Transport) -> None:
        self._transport = transport
        self.read_paused = False
        self.write_paused = False
        self._is_writable_event = asyncio.Event()
        self._is_writable_event.set()

    async def drain(self) -> None:
        await self._is_writable_event.wait()  # pragma: full coverage

    def pause_reading(self) -> None:
        if not self.read_paused:
            self.read_paused = True
            self._transport.pause_reading()

    def resume_reading(self) -> None:
        if self.read_paused:
            self.read_paused = False
            self._transport.resume_reading()

    def pause_writing(self) -> None:
        if not self.write_paused:  # pragma: full coverage
            self.write_paused = True
            self._is_writable_event.clear()

    def resume_writing(self) -> None:
        if self.write_paused:  # pragma: full coverage
            self.write_paused = False
            self._is_writable_event.set()


async def service_unavailable(scope: Scope, receive: ASGIReceiveCallable, send: ASGISendCallable) -> None:
    await send(
        {
            "type": "http.response.start",
            "status": 503,
            "headers": [
                (b"content-type", b"text/plain; charset=utf-8"),
                (b"content-length", b"19"),
                (b"connection", b"close"),
            ],
        }
    )
    await send({"type": "http.response.body", "body": b"Service Unavailable", "more_body": False})
