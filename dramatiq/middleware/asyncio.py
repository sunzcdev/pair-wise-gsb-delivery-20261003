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

from typing import Optional

from ..asyncio import (
    EventLoopThread,
    bind_event_loop_thread,
    get_event_loop_thread,
    set_event_loop_thread,
)
from ..logging import get_logger
from .middleware import Middleware


class AsyncIO(Middleware):
    """This middleware manages the event loop thread for async actors.

    Each instance owns its own event loop thread: it is created when
    the worker boots, bound to the worker's threads for dispatch, and
    stopped when the worker shuts down.  This means multiple workers
    can run side by side in the same process without clobbering each
    other's event loops.  The event loop thread is also kept as the
    process-wide default so that the common single-worker setup keeps
    working without any extra configuration.
    """

    def __init__(self) -> None:
        self.logger = get_logger(__name__, type(self))
        self.event_loop_thread: Optional[EventLoopThread] = None

    def before_worker_boot(self, broker, worker):
        self.event_loop_thread = EventLoopThread(self.logger)
        self.event_loop_thread.start(timeout=1.0)
        set_event_loop_thread(self.event_loop_thread)

    def after_worker_thread_boot(self, broker, thread):
        # Bind this worker thread to the event loop owned by this
        # middleware so that messages are dispatched to the loop that
        # belongs to the worker that received them.
        bind_event_loop_thread(self.event_loop_thread)

    def before_worker_thread_shutdown(self, broker, thread):
        bind_event_loop_thread(None)

    def after_worker_shutdown(self, broker, worker):
        event_loop_thread = self.event_loop_thread
        if event_loop_thread is None:
            event_loop_thread = get_event_loop_thread()
        if event_loop_thread is None:
            return
        event_loop_thread.stop()
        event_loop_thread.join()
        self.event_loop_thread = None
        # Only clear the process-wide default if it still points at
        # the event loop owned by this middleware; another worker
        # running in the same process may have replaced it.
        if get_event_loop_thread() is event_loop_thread:
            set_event_loop_thread(None)
