from __future__ import annotations

__all__ = (
    "MemoryObjectReceiveStream",
    "MemoryObjectSendStream",
    "MemoryObjectStreamStatistics",
)

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
    current_buffer_used: int  #: number of items stored in the buffer
    #: maximum number of items that can be stored on this stream (or :data:`math.inf`)
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

    def deliver(self, item: T_Item) -> None:
        """Hand over an item to the task waiting on this receiver."""
        self.item = item

    def __repr__(self) -> str:
        # When item is not defined, we get following error with default __repr__:
        # AttributeError: 'MemoryObjectItemReceiver' object has no attribute 'item'
        item = getattr(self, "item", None)
        return f"{self.__class__.__name__}(task_info={self.task_info}, item={item!r})"


class _MemoryObjectSelection(Generic[T_Item]):
    """
    Coordinates a single atomic item selection across several receive streams.

    One shared event is registered as the wakeup key on every candidate stream, so a
    single notification covers all of them. The first stream to hand over an item
    commits the whole selection: the remaining registrations are withdrawn immediately
    (before the delivering task can yield), which guarantees that no second item can be
    taken even when several streams become ready in the same scheduling round.
    """

    __slots__ = "_registered", "event", "result"

    def __init__(self) -> None:
        self.event: Event = Event()
        self.result: tuple[MemoryObjectReceiveStream[T_Item], T_Item] | None = None
        self._registered: list[MemoryObjectReceiveStream[T_Item]] = []

    def register(self, stream: MemoryObjectReceiveStream[T_Item]) -> None:
        stream._state.waiting_receivers[self.event] = _MemoryObjectSelectionReceiver(
            stream, self
        )
        if stream._waiters is None:
            stream._waiters = set[Event]()

        stream._waiters.add(self.event)
        self._registered.append(stream)

    def deliver(
        self, stream: MemoryObjectReceiveStream[T_Item], item: T_Item
    ) -> None:
        if self.result is None:
            self.result = (stream, item)
            self.withdraw()
            self.event.set()

    def withdraw(self) -> None:
        for stream in self._registered:
            stream._state.waiting_receivers.pop(self.event, None)
            if stream._waiters is not None:
                stream._waiters.discard(self.event)

        self._registered.clear()


class _MemoryObjectSelectionReceiver(_MemoryObjectItemReceiver[T_Item]):
    """
    A receiver registered on behalf of :meth:`MemoryObjectReceiveStream.receive_any`.

    Delivering an item here commits the entire selection, withdrawing the registrations
    on the other candidate streams so that only one item is ever consumed.
    """

    def __init__(
        self,
        stream: MemoryObjectReceiveStream[T_Item],
        selection: _MemoryObjectSelection[T_Item],
    ) -> None:
        super().__init__()
        self.stream = stream
        self.selection = selection

    def deliver(self, item: T_Item) -> None:
        super().deliver(item)
        self.selection.deliver(self.stream, item)


def _restore_item(stream: MemoryObjectReceiveStream[T_Item], item: T_Item) -> None:
    """
    Put an item back on the given stream after a selection was abandoned.

    A delivered item belongs solely to the selection (the sender has already completed),
    so putting it back never duplicates it. It is offered to the next live waiting
    receiver, mirroring what a sender would have done, or else placed at the front of
    the buffer to preserve ordering.
    """
    state = stream._state
    while state.waiting_receivers:
        receive_event, receiver = state.waiting_receivers.popitem(last=False)
        if not receiver.task_info.has_pending_cancellation():
            receiver.deliver(item)
            receive_event.set()
            return

    state.buffer.appendleft(item)


@dataclass(eq=False)
class _MemoryObjectStreamState(Generic[T_Item]):
    max_buffer_size: float = field()
    buffer: deque[T_Item] = field(init=False, default_factory=deque)
    open_send_channels: int = field(init=False, default=0)
    open_receive_channels: int = field(init=False, default=0)
    waiting_receivers: OrderedDict[Event, _MemoryObjectItemReceiver[T_Item]] = field(
        init=False, default_factory=OrderedDict
    )
    waiting_senders: OrderedDict[Event, T_Item] = field(
        init=False, default_factory=OrderedDict
    )

    def statistics(self) -> MemoryObjectStreamStatistics:
        return MemoryObjectStreamStatistics(
            len(self.buffer),
            self.max_buffer_size,
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

        if self._state.waiting_senders:
            # Get the item from the next sender
            send_event, item = self._state.waiting_senders.popitem(last=False)
            self._state.buffer.append(item)
            send_event.set()

        if self._state.buffer:
            return self._state.buffer.popleft()
        elif not self._state.open_send_channels:
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

    async def receive_any(self, *others: MemoryObjectReceiveStream[T_co]) -> T_co:
        """
        Receive the next item from this stream or any of the given streams, whichever
        becomes available first.

        The candidate streams are checked in the order they were given, with this
        stream first. If more than one candidate is readable at the same time, the
        earliest one in that order wins, and exactly one item is consumed overall.

        Candidates that have been closed or exhausted are skipped as long as another
        candidate can still deliver an item, so an empty closed stream never steals the
        turn from a live one.

        Cancelling the call never removes an item that another stream already had
        queued: the registrations are withdrawn, and an item that had been handed over
        but not yet returned is put back on its originating stream.

        :param others: additional receive streams to compete with this one
        :return: the item received from the winning stream
        :raises ~anyio.ClosedResourceError: if every candidate stream has been
            explicitly closed
        :raises ~anyio.EndOfStream: if no candidate has an item available and all of
            them have been closed from the sending end

        .. versionadded:: 4.17.0
        """
        candidates = (self, *others)
        await checkpoint()
        while True:
            blocked: list[MemoryObjectReceiveStream[T_co]] = []
            end_of_stream = False
            for stream in candidates:
                if stream._closed:
                    # An explicitly closed stream can never deliver; leave the live
                    # candidates to decide the outcome
                    continue

                try:
                    return stream.receive_nowait()
                except WouldBlock:
                    blocked.append(stream)
                except EndOfStream:
                    end_of_stream = True

            if not blocked:
                if end_of_stream:
                    raise EndOfStream

                raise ClosedResourceError

            # Nothing is ready yet, so wait until any candidate either delivers an item
            # or closes
            selection = _MemoryObjectSelection[T_co]()
            seen_states: set[int] = set()
            for stream in blocked:
                # Clones share their state, and the shared state can only hold one
                # registration per event, so register each state just once
                if id(stream._state) not in seen_states:
                    seen_states.add(id(stream._state))
                    selection.register(stream)

            try:
                await selection.event.wait()
            except BaseException:
                selection.withdraw()
                if selection.result is not None:
                    # Cancelled after an item had been handed to us but before we could
                    # return it: give it back so the selection never consumes it
                    stream, item = selection.result
                    _restore_item(stream, item)

                raise
            else:
                selection.withdraw()

            if selection.result is not None:
                return selection.result[1]

            # We were woken up because a candidate closed, so scan the streams again

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
                send_events = list(self._state.waiting_senders.keys())
                for event in send_events:
                    event.set()

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
                receiver.deliver(item)
                receive_event.set()
                return

        if len(self._state.buffer) < self._state.max_buffer_size:
            self._state.buffer.append(item)
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
