# This file is a part of Dramatiq.
#
# Copyright (C) 2023 CLEARTYPE SRL <bogdan@cleartype.io>
#
# Dramatiq is free software; you can redistribute it and/or modify it
# under the terms of the GNU Lesser General Public License as published by
# the Free Software Foundation, either version 3 of the License, or (at
# your option) any later version.
#
# Dramatiq is distributed in the hope that it will be useful, but WITHOUT
# ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
# FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser General Public
# License for more details.
#
# You should have received a copy of the GNU Lesser General Public License
# along with this program.  If not, see <http://www.gnu.org/licenses/>.

from __future__ import annotations

import asyncio
import concurrent.futures
import functools
import logging
import threading
from typing import Awaitable, Callable, Optional, ParamSpec, TypeVar

from .threading import Interrupt

__all__ = [
    "EventLoopThread",
    "async_to_sync",
    "get_event_loop_thread",
    "set_event_loop_thread",
]

R = TypeVar("R")
P = ParamSpec("P")

_event_loop_thread = None


def get_event_loop_thread() -> Optional[EventLoopThread]:
    """Get the global event loop thread.

    Returns:
      EventLoopThread: The global EventLoopThread.
    """
    return _event_loop_thread


def set_event_loop_thread(thread: Optional[EventLoopThread]) -> None:
    """Set the global event loop thread."""
    global _event_loop_thread
    _event_loop_thread = thread


def async_to_sync(async_fn: Callable[P, Awaitable[R]]) -> Callable[P, R]:
    """Wrap an async function to run it on the event loop thread and
    synchronously wait for its result on the calling thread.
    """

    @functools.wraps(async_fn)
    def wrapper(*args: P.args, **kwargs: P.kwargs) -> R:
        event_loop_thread = get_event_loop_thread()
        if event_loop_thread is None:
            raise RuntimeError(
                "Global event loop thread not set. Have you added the AsyncIO middleware to your middleware stack?"
            )
        return event_loop_thread.run_coroutine(async_fn(*args, **kwargs))

    return wrapper


class EventLoopThread(threading.Thread):
    """A thread that runs an asyncio event loop."""

    interrupt_check_ival: float
    logger: logging.Logger
    loop: asyncio.AbstractEventLoop

    def __init__(self, logger, interrupt_check_ival: float = 0.1):
        self.interrupt_check_ival = interrupt_check_ival
        self.logger = logger
        self.loop = asyncio.new_event_loop()
        super().__init__()

    def run(self):
        try:
            self.logger.info("Starting event loop...")
            self.loop.run_forever()
        finally:
            self.loop.close()

    def start(self, *, timeout: Optional[float] = None):
        """Starts the event loop thread.

        Parameters:
          timeout: The maximum amount of time (in seconds) to wait for
          the event loop to start.

        Raises:
          RuntimeError: If called more than once on the same thread
            value.
        """
        super().start()

        ready = threading.Event()
        self.loop.call_soon_threadsafe(ready.set)
        if not ready.wait(timeout=timeout):
            raise RuntimeError("Event loop failed to start.")
        self.logger.info("Event loop is running.")

    def stop(self):
        if self.loop.is_running():
            self.logger.info("Stopping event loop...")
            self.loop.call_soon_threadsafe(self.loop.stop)
            self.join()
            self.loop.close()

    def run_coroutine(self, coro: Awaitable[R]) -> R:
        """Runs the given coroutine on the event loop.

        Parameters:
          coro: The coroutine to run.

        Raises:
          RuntimeError: When the event loop is not running.

        Returns:
          R: The result of the coroutine.
        """
        if not self.loop.is_running():
            raise RuntimeError("Event loop is not running.")

        done = threading.Event()

        async def wrapped_coro() -> R:
            try:
                return await coro
            finally:
                done.set()

        future = asyncio.run_coroutine_threadsafe(wrapped_coro(), self.loop)
        try:
            while True:
                try:
                    # Use a timeout to be able to catch asynchronously
                    # raised dramatiq exceptions (Interrupt).
                    return future.result(timeout=self.interrupt_check_ival)
                except TimeoutError:
                    # NOTE: TimeoutError caught here could be from future.result() timing out (i.e. future not done yet),
                    # or a TimeoutError raised inside the future itself (future is done).
                    if not future.done():
                        # future not done, so .result() must've timed out. continue to wait again.
                        continue

                # If execution reaches here, it means a TimeoutError was caught above, and the future is done.
                # There are 3 possibilities here:
                # 1. TimeoutError was raised inside the future. This will re-raise it.
                # 2. First .result() call timed out, but the future completed by the time .done() was called.
                #     a) This will return the future's result, or
                #     b) raise the Exception that happened in the future.
                return future.result(timeout=0)
                # This is outside the 'except' block to avoid any
                # "During handling of the above exception, another exception occurred" messages.
                # zero timeout used because future is now done.

        except Interrupt:
            # Asynchronously raised from another thread (e.g. by the
            # TimeLimit or Shutdown middleware) while this thread was
            # blocked waiting for the coroutine's result.  The worker
            # thread must not deliver the failure until the coroutine
            # actually running on the event loop has been cancelled and
            # finished its cleanup, so delegate to the coroutine-aware
            # termination routine before re-raising.
            self.terminate_coroutine(future, done)
            raise

        finally:
            # Clear reference to the future, to prevent an Exception that was raised in the coroutine
            # (and subsequently raised from a future.result() call)
            # from being caught in a reference cycle.
            del future
            # Without this, the potential reference cycle is
            # Exception -> Traceback -> Frames -> run_coroutine() locals -> future -> Exception

    def terminate_coroutine(self, future: concurrent.futures.Future, done: threading.Event) -> None:
        """Cancel a coroutine running on the event loop and wait for it
        to finish cleaning up.

        This is the coroutine-specific counterpart to raising an
        :class:`Interrupt` in a worker thread.  While the calling
        (worker) thread has already been interrupted asynchronously,
        the actual coroutine keeps running on this thread's event loop
        until it is cancelled.  Simply returning from the worker thread
        at that point would deliver the failure (and trigger the usual
        failure/retry settlement) while the old coroutine was still
        running in the background.

        The future is cancelled from within the event loop so the
        coroutine receives a ``CancelledError``, and this method blocks
        on ``done`` (set by the coroutine's ``finally`` block) rather
        than on the future itself: a cancelled future resolves
        immediately, whereas the coroutine may still need to run its
        cleanup actions before it is really finished.

        Parameters:
          future: The future wrapping the running coroutine.
          done: An event set once the coroutine has completed,
            including any cleanup.

        Raises:
          RuntimeError: If the coroutine does not finish cleaning up
            within the allotted time.
        """
        if future.done():
            # Nothing to cancel: the coroutine already completed on its
            # own, so just make sure its cleanup has settled.
            done.wait(timeout=1.0)
            return

        # Schedule the cancellation on the event loop thread so the
        # coroutine actually receives the CancelledError.
        self.loop.call_soon_threadsafe(future.cancel)

        # Wait for the 'done' event instead of the future; the future
        # will raise CancelledError immediately while we should wait for
        # the coro to actually finish its cleanup actions.
        if not done.wait(timeout=1.0):
            raise RuntimeError("Timed out while waiting for coroutine.")
