from __future__ import annotations

import asyncio
import contextlib
import logging
from asyncio import Queue
from typing import Any

from uvicorn import Config
from uvicorn._types import (
    LifespanScope,
    LifespanShutdownCompleteEvent,
    LifespanShutdownEvent,
    LifespanShutdownFailedEvent,
    LifespanStartupCompleteEvent,
    LifespanStartupEvent,
    LifespanStartupFailedEvent,
)

LifespanReceiveMessage = LifespanStartupEvent | LifespanShutdownEvent
LifespanSendMessage = (
    LifespanStartupFailedEvent
    | LifespanShutdownFailedEvent
    | LifespanStartupCompleteEvent
    | LifespanShutdownCompleteEvent
)


STATE_TRANSITION_ERROR = "Got invalid state transition on lifespan protocol."


class LifespanOn:
    def __init__(self, config: Config) -> None:
        if not config.loaded:
            config.load()

        self.config = config
        self.logger = logging.getLogger("uvicorn.error")
        self.startup_event = asyncio.Event()
        self.shutdown_event = asyncio.Event()
        self.receive_queue: Queue[LifespanReceiveMessage] = asyncio.Queue()
        self.error_occurred = False
        self.startup_failed = False
        self.startup_timeout = False
        self.shutdown_failed = False
        self.should_exit = False
        self.shutdown_started = False
        self.state: dict[str, Any] = {}
        # Holds the task running the application lifespan callable. A hard
        # reference is kept so it cannot be garbage collected mid-flight.
        self.main_lifespan_task: asyncio.Task[None] | None = None

    async def startup(self) -> None:
        self.logger.info("Waiting for application startup.")

        loop = asyncio.get_event_loop()
        self.main_lifespan_task = loop.create_task(self.main())
        # Keep a hard reference to prevent garbage collection
        # See https://github.com/Kludex/uvicorn/pull/972
        startup_event: LifespanStartupEvent = {"type": "lifespan.startup"}
        await self.receive_queue.put(startup_event)

        if self.config.timeout_lifespan_startup is None:
            await self.startup_event.wait()
        else:
            try:
                async with asyncio.timeout(self.config.timeout_lifespan_startup):
                    await self.startup_event.wait()
            except TimeoutError:
                await self._handle_startup_timeout()
                return

        if self.startup_failed or (self.error_occurred and self.config.lifespan == "on"):
            self.logger.error("Application startup failed. Exiting.")
            self.should_exit = True
        else:
            self.logger.info("Application startup complete.")

    async def _handle_startup_timeout(self) -> None:
        # The application received the startup event but never replied. This is
        # a startup failure, not a missing lifespan implementation: it must be
        # reported even when `lifespan="auto"` instead of being silently treated
        # like an application that does not implement the lifespan protocol.
        self.logger.error(
            "Application startup exceeded the configured timeout of %s seconds. Cancelling.",
            self.config.timeout_lifespan_startup,
        )
        self.startup_timeout = True
        self.startup_failed = True
        self.should_exit = True
        assert self.main_lifespan_task is not None
        # Give the application task an explicit, clear cancellation so it can
        # run any cleanup it has registered.
        self.main_lifespan_task.cancel(msg="Application startup timeout exceeded")
        # Swallow whatever the cancelled task raises: a `CancelledError` is the
        # expected outcome and any other error is already recorded by `main()`.
        # Teardown must never mask the startup failure.
        with contextlib.suppress(BaseException):
            await self.main_lifespan_task
        # Mark shutdown as already in progress: the cancelled lifespan is being
        # cleaned up here, so a later `shutdown()` call must not enter shutdown
        # a second time.
        self.shutdown_started = True

    async def shutdown(self) -> None:
        if self.error_occurred or self.shutdown_started:
            return
        self.shutdown_started = True
        self.logger.info("Waiting for application shutdown.")
        shutdown_event: LifespanShutdownEvent = {"type": "lifespan.shutdown"}
        await self.receive_queue.put(shutdown_event)
        await self.shutdown_event.wait()

        if self.shutdown_failed or (self.error_occurred and self.config.lifespan == "on"):
            self.logger.error("Application shutdown failed. Exiting.")
            self.should_exit = True
        else:
            self.logger.info("Application shutdown complete.")

    async def main(self) -> None:
        try:
            app = self.config.loaded_app
            scope: LifespanScope = {
                "type": "lifespan",
                "asgi": {"version": self.config.asgi_version, "spec_version": "2.0"},
                "state": self.state,
            }
            await app(scope, self.receive, self.send)
        except asyncio.CancelledError:
            # Raised when the startup timeout cancels this task. Propagate it so
            # the task ends in a properly cancelled state instead of being
            # mistaken for an application that does not support the lifespan.
            raise
        except BaseException as exc:
            self.asgi = None
            self.error_occurred = True
            if self.startup_failed or self.shutdown_failed:
                return
            if self.config.lifespan == "auto":
                msg = "ASGI 'lifespan' protocol appears unsupported."
                self.logger.info(msg)
            else:
                msg = "Exception in 'lifespan' protocol\n"
                self.logger.error(msg, exc_info=exc)
        finally:
            self.startup_event.set()
            self.shutdown_event.set()

    async def send(self, message: LifespanSendMessage) -> None:
        assert message["type"] in (
            "lifespan.startup.complete",
            "lifespan.startup.failed",
            "lifespan.shutdown.complete",
            "lifespan.shutdown.failed",
        )

        if message["type"] == "lifespan.startup.complete":
            assert not self.startup_event.is_set(), STATE_TRANSITION_ERROR
            assert not self.shutdown_event.is_set(), STATE_TRANSITION_ERROR
            self.startup_event.set()

        elif message["type"] == "lifespan.startup.failed":
            assert not self.startup_event.is_set(), STATE_TRANSITION_ERROR
            assert not self.shutdown_event.is_set(), STATE_TRANSITION_ERROR
            self.startup_event.set()
            self.startup_failed = True
            if message.get("message"):
                self.logger.error(message["message"])

        elif message["type"] == "lifespan.shutdown.complete":
            assert self.startup_event.is_set(), STATE_TRANSITION_ERROR
            assert not self.shutdown_event.is_set(), STATE_TRANSITION_ERROR
            self.shutdown_event.set()

        elif message["type"] == "lifespan.shutdown.failed":
            assert self.startup_event.is_set(), STATE_TRANSITION_ERROR
            assert not self.shutdown_event.is_set(), STATE_TRANSITION_ERROR
            self.shutdown_event.set()
            self.shutdown_failed = True
            if message.get("message"):
                self.logger.error(message["message"])

    async def receive(self) -> LifespanReceiveMessage:
        return await self.receive_queue.get()
