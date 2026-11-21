from __future__ import annotations

import math
from collections import deque
from collections.abc import AsyncGenerator
from itertools import islice

import attrs
from anyio import BrokenResourceError, ClosedResourceError, create_memory_object_stream
from anyio.streams.memory import MemoryObjectReceiveStream, MemoryObjectSendStream

from .._events import Event
from .._exceptions import ReplayPositionExpired
from .._utils import create_repr
from .base import BaseEventBroker


@attrs.define(eq=False, repr=False)
class LocalEventBroker(BaseEventBroker):
    """
    Asynchronous, local event broker.

    This event broker only broadcasts within the process it runs in, and is therefore
    not suitable for multi-node or multiprocess use cases.

    Does not serialize events.

    The broker explicitly retains a short window of the most recently published
    events (``replay_buffer_size`` events). Each published event is assigned a
    monotonically increasing position, and a receiver (such as a scheduling status
    panel that just reconnected) can use :meth:`read_events` to catch up from the
    last position it saw, or :meth:`stream_events` to atomically switch over from
    catching up to receiving live events. If the requested position has already
    fallen outside the retained window, :exc:`~apscheduler.ReplayPositionExpired`
    is raised to indicate that the current state must be fetched again.

    :param replay_buffer_size: the maximum number of published events retained for
        replay (0 disables retention)
    """

    replay_buffer_size: int = 100
    _event_log: deque[Event] = attrs.field(init=False)
    _next_position: int = attrs.field(init=False, default=1)
    _replay_streams: list[MemoryObjectSendStream[tuple[int, Event]]] = attrs.field(
        init=False, factory=list
    )

    @_event_log.default
    def _create_event_log(self) -> deque[Event]:
        return deque(maxlen=self.replay_buffer_size)

    def __repr__(self) -> str:
        return create_repr(self)

    @property
    def current_position(self) -> int:
        """
        The position of the most recently published event (0 if none have been
        published yet).

        This can be used as a token marking the receiver's place in the event
        stream, to be later passed to :meth:`read_events` or :meth:`stream_events`.
        """
        return self._next_position - 1

    @property
    def _oldest_position(self) -> int:
        return self._next_position - len(self._event_log)

    async def publish(self, event: Event) -> None:
        await self.publish_local(event)

    async def publish_local(self, event: Event) -> None:
        # Record the event in the replay window and forward it to any replay
        # streams *before* dispatching to regular subscribers. This ensures that
        # any event observed by a live receiver is already visible to read_events()
        # and that both always see the events in the same order.
        position = self._next_position
        self._next_position += 1
        self._event_log.append(event)
        for stream in list(self._replay_streams):
            try:
                stream.send_nowait((position, event))
            except (ClosedResourceError, BrokenResourceError):
                self._replay_streams.remove(stream)

        await super().publish_local(event)

    def _read_log(self, since: int) -> tuple[int, list[tuple[int, Event]]]:
        """
        Read the retained events published after the given position.

        This method is deliberately synchronous and awaits nothing, so the snapshot
        it takes is atomic with respect to other tasks in the event loop.

        :return: the current position and the events (with their positions)
            published after ``since``
        """
        current = self.current_position
        if since > current:
            raise ValueError(
                f"position {since} is ahead of the current position ({current})"
            )
        elif since < self._oldest_position - 1:
            raise ReplayPositionExpired(since, self._oldest_position)

        skip = max(since - self._oldest_position + 1, 0)
        events = [
            (self._oldest_position + index, event)
            for index, event in enumerate(islice(self._event_log, skip, None))
        ]
        return current, events

    def read_events(self, since: int = 0) -> tuple[int, list[Event]]:
        """
        Read the events published after the given position.

        This is meant for receivers that track the event stream via positions: pass
        the last position returned by an earlier call (or
        :attr:`current_position`), and apply the returned events in order. If this
        call succeeds, no events were missed; if the position has already expired
        from the retained window, :exc:`~apscheduler.ReplayPositionExpired` is
        raised and the receiver should fetch the current state again before
        resuming.

        :param since: the last position the receiver has seen (0 to read all
            retained events)
        :return: the new position and the events published after ``since``
        :raises ReplayPositionExpired: if ``since`` is older than the oldest
            retained position
        """
        position, events = self._read_log(since)
        return position, [event for _, event in events]

    def stream_events(self, since: int | None = None) -> AsyncGenerator[Event, None]:
        """
        Return an async generator that first replays the retained events and then
        seamlessly switches over to live events.

        The generator is registered with the broker immediately, so events
        published after this call are not missed even if iteration starts later.
        The catch-up and the live events are handed off in order, without gaps or
        duplicates: any event published while the catch-up is in progress is
        delivered exactly once, in its proper place.

        The caller should close the generator (``await stream.aclose()``) when it
        is no longer needed.

        :param since: the last position the receiver has seen (``None`` to only
            receive events published from this point on)
        :raises ReplayPositionExpired: if ``since`` is older than the oldest
            retained position
        """
        send, receive = create_memory_object_stream[tuple[int, Event]](math.inf)
        self._replay_streams.append(send)
        try:
            if since is None:
                since = self.current_position

            position, events = self._read_log(since)
        except BaseException:
            self._replay_streams.remove(send)
            send.close()
            receive.close()
            raise

        return self._stream_events(send, receive, position, events)

    async def _stream_events(
        self,
        send: MemoryObjectSendStream[tuple[int, Event]],
        receive: MemoryObjectReceiveStream[tuple[int, Event]],
        position: int,
        events: list[tuple[int, Event]],
    ) -> AsyncGenerator[Event, None]:
        try:
            for _, event in events:
                yield event

            async with receive:
                async for position_, event in receive:
                    if position_ > position:
                        position = position_
                        yield event
        finally:
            self._replay_streams.remove(send)
            await send.aclose()
