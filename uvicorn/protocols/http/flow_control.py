import asyncio

from uvicorn._types import ASGIReceiveCallable, ASGISendCallable, Scope

CLOSE_HEADER = (b"connection", b"close")

HIGH_WATER_LIMIT = 65536


class WriteTimeoutError(TimeoutError):
    """Raised when an ASGI ``send`` waits longer than the configured write timeout.

    The clock only runs while the transport is write-paused, i.e. while the
    application is actually waiting for permission to write. Time the
    application spends producing the next chunk is not counted.
    """


class FlowControl:
    def __init__(
        self,
        transport: asyncio.Transport,
        write_timeout: float | None = None,
        loop: asyncio.AbstractEventLoop | None = None,
    ) -> None:
        self._transport = transport
        self._write_timeout = write_timeout
        self._loop = loop or asyncio.get_event_loop()
        self.read_paused = False
        self.write_paused = False
        self._is_writable_event = asyncio.Event()
        self._is_writable_event.set()

    async def drain(self) -> None:
        """Wait until the transport is writable again.

        When a write timeout is configured, the clock only runs while the
        transport is write-paused, i.e. while the application is actually
        waiting for permission to write; time spent producing the next chunk
        is not counted. If the pause outlasts the timeout, the wait is aborted
        with :class:`WriteTimeoutError` and the transport is closed, so the
        request ends with a single, definite result even when resuming,
        closing and the timeout collide. The timer is always disarmed before
        returning, so no task or handle is ever left in flight.
        """
        if self._write_timeout is None or self._write_timeout <= 0 or not self.write_paused:
            await self._is_writable_event.wait()  # pragma: full coverage
            return

        loop = self._loop
        timed_out = False

        def _on_write_timeout() -> None:
            nonlocal timed_out
            # The timer wins the race regardless of anything else happening on
            # the connection, so the flag alone decides the outcome. Closing
            # the transport triggers ``connection_lost`` ->
            # ``resume_writing``, which unblocks the drain below.
            timed_out = True
            if not self._transport.is_closing():
                self._transport.close()

        timeout_handle = loop.call_later(self._write_timeout, _on_write_timeout)
        try:
            await self._is_writable_event.wait()
            if timed_out:
                raise WriteTimeoutError(
                    f"Timed out waiting for the client to drain the write buffer after {self._write_timeout}s."
                )
        finally:
            # Disarm the loser of the resume/close/timeout race so no handle
            # survives this drain, and every drain gets a fresh timer.
            timeout_handle.cancel()

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
