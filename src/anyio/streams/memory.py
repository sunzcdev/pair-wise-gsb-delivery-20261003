from __future__ import annotations

__all__ = (
    "MemoryObjectReceiveStream",
    "MemoryObjectSendStream",
    "MemoryObjectStreamBuffer",
    "MemoryObjectStreamStatistics",
)

import math
import warnings
from collections import OrderedDict, deque
from dataclasses import dataclass, field
from types import TracebackType
from typing import Generic, NamedTuple, TypeVar

from .._core._exceptions import (
    BrokenResourceError,
    ClosedResourceError,
    EndOfStream,
    WouldBlock,
)
from .._core._synchronization import Event
from .._core._testing import TaskInfo, get_current_task
from ..abc import ObjectReceiveStream, ObjectSendStream
from ..lowlevel import checkpoint

T_Item = TypeVar("T_Item")
T_co = TypeVar("T_co", covariant=True)
T_contra = TypeVar("T_contra", contravariant=True)


class MemoryObjectStreamStatistics(NamedTuple):
    #: number of items stored in this stream's part of the buffer
    current_buffer_used: int
    #: maximum total number of items that can be stored in the (possibly shared)
    #: buffer (or :data:`math.inf`)
    max_buffer_size: float
    open_send_streams: int  #: number of unclosed clones of the send stream
    open_receive_streams: int  #: number of unclosed clones of the receive stream
    #: number of tasks blocked on :meth:`MemoryObjectSendStream.send`
    tasks_waiting_send: int
    #: number of tasks blocked on :meth:`MemoryObjectReceiveStream.receive`
    tasks_waiting_receive: int


@dataclass(eq=False)
class _MemoryObjectItemReceiver(Generic[T_Item]):
    task_info: TaskInfo = field(init=False, default_factory=get_current_task)
    item: T_Item = field(init=False)

    def __repr__(self) -> str:
        # When item is not defined, we get following error with default __repr__:
        # AttributeError: 'MemoryObjectItemReceiver' object has no attribute 'item'
        item = getattr(self, "item", None)
        return f"{self.__class__.__name__}(task_info={self.task_info}, item={item!r})"


@dataclass(eq=False)
class MemoryObjectStreamBuffer:
    """
    A buffer quota that can be shared by several memory object streams.

    Pass an instance of this class as the ``max_buffer_size`` argument to
    :func:`anyio.create_memory_object_stream` to make the created streams share a
    common buffer quota. The combined number of items buffered on all streams sharing
    the same instance never exceeds ``max_size``.

    Each stream still keeps its own queue: the order in which items are sent and
    received on any one stream, as well as the closing semantics of each stream, are
    unaffected by the sharing. Items handed directly to a waiting receiver never
    consume any buffer quota, so such deliveries can complete even when another
    stream has exhausted the shared quota.

    :param max_size: the maximum total number of items that can be buffered on all
        the streams sharing this buffer (or :data:`math.inf` for an unbounded
        buffer)

    .. versionadded:: 4.17

    """

    max_size: float = field()
    _used: int = field(init=False, default=0)
    _states: list[_MemoryObjectStreamState] = field(init=False, default_factory=list)

    def __post_init__(self) -> None:
        if self.max_size != math.inf and not isinstance(self.max_size, int):
            raise ValueError("max buffer size must be either an integer or math.inf")
        if self.max_size < 0:
            raise ValueError("max buffer size cannot be negative")

    def _register(self, state: _MemoryObjectStreamState) -> None:
        self._states.append(state)

    def _unregister(self, state: _MemoryObjectStreamState) -> None:
        try:
            self._states.remove(state)
        except ValueError:
            pass

    def _notify_senders(self) -> None:
        """
        Let blocked senders buffer their items after buffer quota has been freed.

        This is necessary because a send operation can be blocked by quota consumed
        by a different stream than the one being received from.
        """
        for state in list(self._states):
            if not state.open_receive_channels:
                # The buffered items would never be received on this stream
                continue

            while self._used < self.max_size and state.waiting_senders:
                send_event, item = state.waiting_senders.popitem(last=False)
                state.buffer.append(item)
                self._used += 1
                send_event.set()


@dataclass(eq=False)
class _MemoryObjectStreamState(Generic[T_Item]):
    buffer_pool: MemoryObjectStreamBuffer = field()
    buffer: deque[T_Item] = field(init=False, default_factory=deque)
    open_send_channels: int = field(init=False, default=0)
    open_receive_channels: int = field(init=False, default=0)
    waiting_receivers: OrderedDict[Event, _MemoryObjectItemReceiver[T_Item]] = field(
        init=False, default_factory=OrderedDict
    )
    waiting_senders: OrderedDict[Event, T_Item] = field(
        init=False, default_factory=OrderedDict
    )

    def __post_init__(self) -> None:
        self.buffer_pool._register(self)

    def statistics(self) -> MemoryObjectStreamStatistics:
        return MemoryObjectStreamStatistics(
            len(self.buffer),
            self.buffer_pool.max_size,
            self.open_send_channels,
            self.open_receive_channels,
            len(self.waiting_senders),
            len(self.waiting_receivers),
        )


@dataclass(eq=False)
class MemoryObjectReceiveStream(ObjectReceiveStream[T_co], Generic[T_co]):
    _state: _MemoryObjectStreamState[T_co]
    _closed: bool = field(init=False, default=False)
    _waiters: set[Event] | None = field(init=False, default=None)

    def __post_init__(self) -> None:
        self._state.open_receive_channels += 1

    def receive_nowait(self) -> T_co:
        """
        Receive the next item if it can be done without waiting.

        :return: the received item
        :raises ~anyio.ClosedResourceError: if this send stream has been closed
        :raises ~anyio.EndOfStream: if the buffer is empty and this stream has been
            closed from the sending end
        :raises ~anyio.WouldBlock: if there are no items in the buffer and no tasks
            waiting to send

        """
        if self._closed:
            raise ClosedResourceError

        state = self._state
        buffer_pool = state.buffer_pool
        if state.buffer:
            item = state.buffer.popleft()
            buffer_pool._used -= 1
            if state.waiting_senders:
                # Move the next waiting sender's item into the freed slot
                send_event, next_item = state.waiting_senders.popitem(last=False)
                state.buffer.append(next_item)
                buffer_pool._used += 1
                send_event.set()
            else:
                # The freed slot may unblock senders on streams sharing the buffer
                buffer_pool._notify_senders()

            return item

        if state.waiting_senders:
            # Deliver the item directly to the receiver, bypassing the buffer (and
            # thus not consuming any of the potentially shared buffer quota)
            send_event, item = state.waiting_senders.popitem(last=False)
            send_event.set()
            return item

        if not state.open_send_channels:
            raise EndOfStream

        raise WouldBlock

    async def receive(self) -> T_co:
        await checkpoint()
        try:
            return self.receive_nowait()
        except WouldBlock:
            if self._waiters is None:
                self._waiters = set[Event]()

            # Add ourselves in the queue
            receive_event = Event()
            self._waiters.add(receive_event)
            receiver = _MemoryObjectItemReceiver[T_co]()
            self._state.waiting_receivers[receive_event] = receiver

            try:
                await receive_event.wait()
            finally:
                self._state.waiting_receivers.pop(receive_event, None)
                if self._waiters is not None:
                    self._waiters.remove(receive_event)

            try:
                return receiver.item
            except AttributeError:
                if self._closed:
                    raise ClosedResourceError from None
                else:
                    raise EndOfStream from None

    def clone(self) -> MemoryObjectReceiveStream[T_co]:
        """
        Create a clone of this receive stream.

        Each clone can be closed separately. Only when all clones have been closed will
        the receiving end of the memory stream be considered closed by the sending ends.

        :return: the cloned stream

        """
        if self._closed:
            raise ClosedResourceError

        return MemoryObjectReceiveStream(_state=self._state)

    def close(self) -> None:
        """
        Close the stream.

        This works the exact same way as :meth:`aclose`, but is provided as a special
        case for the benefit of synchronous callbacks.

        """
        if not self._closed:
            self._closed = True
            self._state.open_receive_channels -= 1
            if self._state.open_receive_channels == 0:
                state = self._state
                buffer_pool = state.buffer_pool
                if state.buffer:
                    # Release the quota held by items that can no longer be received
                    buffer_pool._used -= len(state.buffer)
                    state.buffer.clear()
                    buffer_pool._notify_senders()

                send_events = list(state.waiting_senders.keys())
                for event in send_events:
                    event.set()

                if not state.open_send_channels:
                    buffer_pool._unregister(state)

            if self._waiters is not None:
                for event in self._waiters:
                    self._state.waiting_receivers.pop(event, None)
                    event.set()

                self._waiters = None

    async def aclose(self) -> None:
        self.close()

    def statistics(self) -> MemoryObjectStreamStatistics:
        """
        Return statistics about the current state of this stream.

        .. versionadded:: 3.0
        """
        return self._state.statistics()

    def __enter__(self) -> MemoryObjectReceiveStream[T_co]:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: TracebackType | None,
    ) -> None:
        self.close()

    def __del__(self) -> None:
        if not self._closed:
            warnings.warn(
                f"Unclosed <{self.__class__.__name__} at {id(self):x}>",
                ResourceWarning,
                stacklevel=1,
                source=self,
            )


@dataclass(eq=False)
class MemoryObjectSendStream(ObjectSendStream[T_contra], Generic[T_contra]):
    _state: _MemoryObjectStreamState[T_contra]
    _closed: bool = field(init=False, default=False)
    _waiters: set[Event] | None = field(init=False, default=None)

    def __post_init__(self) -> None:
        self._state.open_send_channels += 1

    def send_nowait(self, item: T_contra) -> None:
        """
        Send an item immediately if it can be done without waiting.

        :param item: the item to send
        :raises ~anyio.ClosedResourceError: if this send stream has been closed
        :raises ~anyio.BrokenResourceError: if the stream has been closed from the
            receiving end
        :raises ~anyio.WouldBlock: if the buffer is full and there are no tasks waiting
            to receive

        """
        if self._closed:
            raise ClosedResourceError
        if not self._state.open_receive_channels:
            raise BrokenResourceError

        while self._state.waiting_receivers:
            receive_event, receiver = self._state.waiting_receivers.popitem(last=False)
            if not receiver.task_info.has_pending_cancellation():
                receiver.item = item
                receive_event.set()
                return

        buffer_pool = self._state.buffer_pool
        if buffer_pool._used < buffer_pool.max_size:
            self._state.buffer.append(item)
            buffer_pool._used += 1
        else:
            raise WouldBlock

    async def send(self, item: T_contra) -> None:
        """
        Send an item to the stream.

        If the buffer is full, this method blocks until there is again room in the
        buffer or the item can be sent directly to a receiver.

        :param item: the item to send
        :raises ~anyio.ClosedResourceError: if this send stream has been closed
        :raises ~anyio.BrokenResourceError: if the stream has been closed from the
            receiving end

        """
        await checkpoint()
        try:
            self.send_nowait(item)
        except WouldBlock:
            if self._waiters is None:
                self._waiters = set[Event]()

            # Wait until there's someone on the receiving end
            send_event = Event()
            self._waiters.add(send_event)
            self._state.waiting_senders[send_event] = item
            try:
                await send_event.wait()
            finally:
                if item_not_consumed := send_event in self._state.waiting_senders:
                    del self._state.waiting_senders[send_event]

                withdrawn = self._closed and send_event in self._waiters
                self._waiters.discard(send_event)

            if item_not_consumed:
                raise BrokenResourceError from None
            elif withdrawn:
                raise ClosedResourceError from None

    def clone(self) -> MemoryObjectSendStream[T_contra]:
        """
        Create a clone of this send stream.

        Each clone can be closed separately. Only when all clones have been closed will
        the sending end of the memory stream be considered closed by the receiving ends.

        :return: the cloned stream

        """
        if self._closed:
            raise ClosedResourceError

        return MemoryObjectSendStream(_state=self._state)

    def close(self) -> None:
        """
        Close the stream.

        This works the exact same way as :meth:`aclose`, but is provided as a special
        case for the benefit of synchronous callbacks.

        """
        if not self._closed:
            self._closed = True
            self._state.open_send_channels -= 1
            if self._state.open_send_channels == 0:
                receive_events = list(self._state.waiting_receivers.keys())
                self._state.waiting_receivers.clear()
                for event in receive_events:
                    event.set()

                if not self._state.open_receive_channels:
                    self._state.buffer_pool._unregister(self._state)

            if self._waiters is not None:
                withdrawn = set[Event]()
                for event in self._waiters:
                    if event in self._state.waiting_senders:
                        del self._state.waiting_senders[event]
                        withdrawn.add(event)

                    event.set()

                # An event remaining on this set signals that its item was never
                # delivered, and send() will raise ClosedResourceError
                self._waiters = withdrawn

    async def aclose(self) -> None:
        self.close()

    def statistics(self) -> MemoryObjectStreamStatistics:
        """
        Return statistics about the current state of this stream.

        .. versionadded:: 3.0
        """
        return self._state.statistics()

    def __enter__(self) -> MemoryObjectSendStream[T_contra]:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: TracebackType | None,
    ) -> None:
        self.close()

    def __del__(self) -> None:
        if not self._closed:
            warnings.warn(
                f"Unclosed <{self.__class__.__name__} at {id(self):x}>",
                ResourceWarning,
                stacklevel=1,
                source=self,
            )
