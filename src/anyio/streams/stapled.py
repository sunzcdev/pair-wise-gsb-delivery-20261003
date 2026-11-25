from __future__ import annotations

__all__ = (
    "MultiListener",
    "StapledByteStream",
    "StapledObjectStream",
    "StreamCloseGroup",
    "StreamOwner",
)

from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any, Generic, TypeVar

from .. import CancelScope, Lock
from .._core._exceptions import ClosedResourceError
from ..abc import (
    AsyncResource,
    ByteReceiveStream,
    ByteSendStream,
    ByteStream,
    Listener,
    ObjectReceiveStream,
    ObjectSendStream,
    ObjectStream,
    TaskGroup,
)
from ..lowlevel import checkpoint_if_cancelled

T_Item = TypeVar("T_Item")
T_Stream = TypeVar("T_Stream")


class _CloseOnceState:
    __slots__ = ("called", "error", "lock")

    def __init__(self) -> None:
        self.called = False
        self.lock = Lock()
        self.error: BaseException | None = None


def _patch_aclose_once(
    resource: AsyncResource,
    on_closed: Callable[[BaseException | None], None] | None = None,
) -> Callable[[], None]:
    """
    Temporarily wrap ``resource.aclose`` so re-entrant closes are deduplicated.

    The returned callback restores the original method. If the resource does
    not allow its ``aclose`` attribute to be replaced, closure is still
    deduplicated for calls made through the close group.
    """
    original_aclose = resource.aclose
    state = _CloseOnceState()

    async def aclose_once() -> None:
        async with state.lock:
            if state.called:
                if state.error is not None:
                    raise state.error

                return

            state.called = True
            try:
                await original_aclose()
            except BaseException as exc:
                state.error = exc
                raise
            finally:
                if on_closed is not None:
                    on_closed(state.error)

    aclose_attribute = "aclose"
    try:
        setattr(resource, aclose_attribute, aclose_once)
    except (AttributeError, TypeError):
        return lambda: None

    def restore() -> None:
        try:
            if resource.aclose is aclose_once:
                setattr(resource, aclose_attribute, original_aclose)
        except (AttributeError, TypeError):
            pass

    return restore


class StreamCloseGroup(AsyncResource):
    """
    Coordinates idempotent closure of streams referenced by stapled streams.

    Pass the same group as the ``owner`` of several stapled streams when their
    send or receive halves refer to the same underlying resource. The group
    closes each registered resource at most once, preserves the first close
    error, and marks all participating stapled streams as closed once closure
    starts.
    """

    __slots__ = (
        "_close_error",
        "_closed",
        "_closed_ids",
        "_lock",
        "_member_errors",
        "_members",
    )

    def __init__(self) -> None:
        self._members: list[AsyncResource] = []
        self._closed_ids: set[int] = set()
        self._member_errors: dict[int, BaseException] = {}
        self._closed = False
        self._close_error: BaseException | None = None
        self._lock = Lock()

    @property
    def closed(self) -> bool:
        """Return ``True`` if coordinated closure has started."""
        return self._closed

    @property
    def close_error(self) -> BaseException | None:
        """Return the first error encountered while closing members."""
        return self._close_error

    def register(self, stream: AsyncResource) -> None:
        """
        Register a stream for coordinated closure.

        Registering the same object more than once has no additional effect.
        """
        if self._closed:
            raise RuntimeError(
                "cannot register streams with a closed stream close group"
            )

        if not any(member is stream for member in self._members):
            self._members.append(stream)

    async def aclose_member(self, stream: AsyncResource) -> None:
        """Close one registered member at most once."""
        self.register(stream)
        with CancelScope(shield=True):
            async with self._lock:
                restorers = self._patch_members_locked()
                try:
                    await self._aclose_member_locked(stream)
                finally:
                    for restore in reversed(restorers):
                        restore()

    def _patch_members_locked(self) -> list[Callable[[], None]]:
        restorers = []
        for stream in self._members:
            member_id = id(stream)

            def record_close(
                error: BaseException | None, member_id: int = member_id
            ) -> None:
                self._closed_ids.add(member_id)
                if error is not None:
                    self._member_errors[member_id] = error

            restorers.append(_patch_aclose_once(stream, record_close))

        return restorers

    async def _aclose_member_locked(self, stream: AsyncResource) -> None:
        member_id = id(stream)
        if member_id in self._closed_ids:
            error = self._member_errors.get(member_id)
            if error is not None:
                raise error

            return

        self._closed_ids.add(member_id)
        try:
            with CancelScope(shield=True):
                await stream.aclose()
        except BaseException as exc:
            self._member_errors[member_id] = exc
            raise

    async def aclose(self) -> None:
        """
        Close every registered stream exactly once.

        Closure of individual members is shielded from cancellation. The first
        close error is re-raised after all possible members have been closed.
        When there is no close error, a pending cancellation is delivered at a
        checkpoint.
        """
        with CancelScope(shield=True):
            async with self._lock:
                if not self._closed:
                    self._closed = True
                    restorers = self._patch_members_locked()
                    first_error: BaseException | None = None
                    try:
                        for stream in self._members:
                            try:
                                await self._aclose_member_locked(stream)
                            except BaseException as exc:
                                if first_error is None:
                                    first_error = exc
                    finally:
                        for restore in reversed(restorers):
                            restore()

                    self._close_error = first_error

                error = self._close_error

        if error is not None:
            raise error

        await checkpoint_if_cancelled()


#: Explicit ownership coordinator for :class:`StapledByteStream` and
#: :class:`StapledObjectStream`.
StreamOwner = StreamCloseGroup


@dataclass(eq=False)
class StapledByteStream(ByteStream):
    """
    Combines two byte streams into a single, bidirectional byte stream.

    Extra attributes will be provided from both streams, with the receive stream
    providing the values in case of a conflict.

    Closing the stapled stream marks it closed before closing both halves. Each
    underlying object is closed at most once. Pass a shared
    :class:`StreamCloseGroup` as ``owner`` when multiple stapled streams refer
    to the same underlying resources.

    :param ByteSendStream send_stream: the sending byte stream
    :param ByteReceiveStream receive_stream: the receiving byte stream
    :param StreamCloseGroup owner: the explicit close ownership coordinator

    """

    send_stream: ByteSendStream
    receive_stream: ByteReceiveStream
    owner: StreamCloseGroup = field(default_factory=StreamCloseGroup, repr=False)
    _closed: bool = field(init=False, default=False)
    _eof_sent: bool = field(init=False, default=False)

    def __post_init__(self) -> None:
        self.owner.register(self.send_stream)
        self.owner.register(self.receive_stream)

    @property
    def close_group(self) -> StreamCloseGroup:
        """The close ownership coordinator used by this stapled stream."""
        return self.owner

    async def receive(self, max_bytes: int = 65536) -> bytes:
        if max_bytes < 1:
            raise ValueError("max_bytes must be a positive integer")

        if self._closed or self.owner.closed:
            raise ClosedResourceError

        return await self.receive_stream.receive(max_bytes)

    async def send(self, item: bytes) -> None:
        if self._closed or self._eof_sent or self.owner.closed:
            raise ClosedResourceError

        await self.send_stream.send(item)

    async def send_eof(self) -> None:
        if self._closed or self._eof_sent:
            return

        self._eof_sent = True
        await self.owner.aclose_member(self.send_stream)

    async def aclose(self) -> None:
        self._closed = True
        await self.owner.aclose()

    @property
    def extra_attributes(self) -> Mapping[Any, Callable[[], Any]]:
        return {
            **self.send_stream.extra_attributes,
            **self.receive_stream.extra_attributes,
        }


@dataclass(eq=False)
class StapledObjectStream(ObjectStream[T_Item], Generic[T_Item]):
    """
    Combines two object streams into a single, bidirectional object stream.

    Extra attributes will be provided from both streams, with the receive stream
    providing the values in case of a conflict.

    :param ObjectSendStream send_stream: the sending object stream
    :param ObjectReceiveStream receive_stream: the receiving object stream
    :param StreamCloseGroup owner: the explicit close ownership coordinator
    """

    send_stream: ObjectSendStream[T_Item]
    receive_stream: ObjectReceiveStream[T_Item]
    owner: StreamCloseGroup = field(default_factory=StreamCloseGroup, repr=False)
    _closed: bool = field(init=False, default=False)
    _eof_sent: bool = field(init=False, default=False)

    def __post_init__(self) -> None:
        self.owner.register(self.send_stream)
        self.owner.register(self.receive_stream)

    @property
    def close_group(self) -> StreamCloseGroup:
        """The close ownership coordinator used by this stapled stream."""
        return self.owner

    async def receive(self) -> T_Item:
        if self._closed or self.owner.closed:
            raise ClosedResourceError

        return await self.receive_stream.receive()

    async def send(self, item: T_Item) -> None:
        if self._closed or self._eof_sent or self.owner.closed:
            raise ClosedResourceError

        await self.send_stream.send(item)

    def send_nowait(self, item: T_Item) -> None:
        if self._closed or self._eof_sent or self.owner.closed:
            raise ClosedResourceError

        try:
            send_nowait = self.send_stream.send_nowait  # type: ignore[attr-defined]
        except AttributeError as exc:
            raise NotImplementedError(
                f"'send_nowait' method not implemented in {type(self.send_stream)}"
            ) from exc

        send_nowait(item)

    async def send_eof(self) -> None:
        if self._closed or self._eof_sent:
            return

        self._eof_sent = True
        await self.owner.aclose_member(self.send_stream)

    async def aclose(self) -> None:
        self._closed = True
        await self.owner.aclose()

    @property
    def extra_attributes(self) -> Mapping[Any, Callable[[], Any]]:
        return {
            **self.send_stream.extra_attributes,
            **self.receive_stream.extra_attributes,
        }


@dataclass(eq=False)
class MultiListener(Listener[T_Stream], Generic[T_Stream]):
    """
    Combines multiple listeners into one, serving connections from all of them at once.

    Any MultiListeners in the given collection of listeners will have their listeners
    moved into this one.

    Extra attributes are provided from each listener, with each successive listener
    overriding any conflicting attributes from the previous one.

    :param listeners: listeners to serve
    :type listeners: Sequence[Listener[T_Stream]]
    """

    listeners: Sequence[Listener[T_Stream]]

    def __post_init__(self) -> None:
        listeners: list[Listener[T_Stream]] = []
        for listener in self.listeners:
            if isinstance(listener, MultiListener):
                listeners.extend(listener.listeners)
                del listener.listeners[:]  # type: ignore[attr-defined]
            else:
                listeners.append(listener)

        self.listeners = listeners

    async def serve(
        self, handler: Callable[[T_Stream], Any], task_group: TaskGroup | None = None
    ) -> None:
        from .. import create_task_group

        async with create_task_group() as tg:
            for listener in self.listeners:
                tg.start_soon(listener.serve, handler, task_group)

    async def aclose(self) -> None:
        for listener in self.listeners:
            await listener.aclose()

    @property
    def extra_attributes(self) -> Mapping[Any, Callable[[], Any]]:
        attributes: dict = {}
        for listener in self.listeners:
            attributes.update(listener.extra_attributes)

        return attributes
