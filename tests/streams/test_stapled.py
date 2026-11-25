from __future__ import annotations

from collections import deque
from collections.abc import Iterable
from dataclasses import InitVar, dataclass, field
from typing import Generic, TypeVar

import pytest

from anyio import (
    CancelScope,
    ClosedResourceError,
    EndOfStream,
    Event,
    create_task_group,
    get_cancelled_exc_class,
    wait_all_tasks_blocked,
)
from anyio.abc import (
    ByteReceiveStream,
    ByteSendStream,
    ByteStream,
    ObjectReceiveStream,
    ObjectSendStream,
    ObjectStream,
)
from anyio.streams.stapled import (
    StapledByteStream,
    StapledObjectStream,
    StreamCloseGroup,
)


@dataclass
class DummyByteReceiveStream(ByteReceiveStream):
    data: InitVar[bytes]
    buffer: bytearray = field(init=False)
    _closed: bool = field(init=False, default=False)

    def __post_init__(self, data: bytes) -> None:
        self.buffer = bytearray(data)

    async def receive(self, max_bytes: int = 65536) -> bytes:
        if self._closed:
            raise ClosedResourceError

        data = bytes(self.buffer[:max_bytes])
        del self.buffer[:max_bytes]
        return data

    async def aclose(self) -> None:
        self._closed = True


@dataclass
class DummyByteSendStream(ByteSendStream):
    buffer: bytearray = field(init=False, default_factory=bytearray)
    _closed: bool = field(init=False, default=False)

    async def send(self, item: bytes) -> None:
        if self._closed:
            raise ClosedResourceError

        self.buffer.extend(item)

    async def aclose(self) -> None:
        self._closed = True


@dataclass
class FailableByteSendStream(ByteSendStream):
    close_error: BaseException | None = None
    close_calls: int = field(init=False, default=0)
    _closed: bool = field(init=False, default=False)

    async def send(self, item: bytes) -> None:
        if self._closed:
            raise ClosedResourceError

    async def aclose(self) -> None:
        self.close_calls += 1
        self._closed = True
        if self.close_error is not None:
            raise self.close_error


@dataclass
class FailableByteReceiveStream(ByteReceiveStream):
    close_error: BaseException | None = None
    close_calls: int = field(init=False, default=0)
    _closed: bool = field(init=False, default=False)

    async def receive(self, max_bytes: int = 65536) -> bytes:
        if self._closed:
            raise ClosedResourceError

        return b""

    async def aclose(self) -> None:
        self.close_calls += 1
        self._closed = True
        if self.close_error is not None:
            raise self.close_error


class BlockedByteSendStream(ByteSendStream):
    def __init__(self, gate: Event) -> None:
        self.gate = gate
        self._closed = False

    async def send(self, item: bytes) -> None:
        if self._closed:
            raise ClosedResourceError

    async def aclose(self) -> None:
        await self.gate.wait()
        self._closed = True


class DummyFullDuplexByteStream(ByteStream):
    """A single bidirectional stream used as both the send and receive half."""

    def __init__(self, close_error: BaseException | None = None) -> None:
        self.close_error = close_error
        self.close_calls = 0
        self._closed = False

    async def receive(self, max_bytes: int = 65536) -> bytes:
        if self._closed:
            raise ClosedResourceError

        return b""

    async def send(self, item: bytes) -> None:
        if self._closed:
            raise ClosedResourceError

    async def send_eof(self) -> None:
        pass

    async def aclose(self) -> None:
        self.close_calls += 1
        self._closed = True
        if self.close_error is not None:
            raise self.close_error


class TestStapledByteStream:
    @pytest.fixture
    def send_stream(self) -> DummyByteSendStream:
        return DummyByteSendStream()

    @pytest.fixture
    def receive_stream(self) -> DummyByteReceiveStream:
        return DummyByteReceiveStream(b"hello, world")

    @pytest.fixture
    def stapled(
        self, send_stream: DummyByteSendStream, receive_stream: DummyByteReceiveStream
    ) -> StapledByteStream:
        return StapledByteStream(send_stream, receive_stream)

    async def test_receive_send(
        self, stapled: StapledByteStream, send_stream: DummyByteSendStream
    ) -> None:
        assert await stapled.receive(3) == b"hel"
        assert await stapled.receive() == b"lo, world"
        assert await stapled.receive() == b""

        await stapled.send(b"how are you ")
        await stapled.send(b"today?")
        assert stapled.send_stream is send_stream
        assert bytes(send_stream.buffer) == b"how are you today?"

    async def test_send_eof(self, stapled: StapledByteStream) -> None:
        await stapled.send_eof()
        await stapled.send_eof()
        with pytest.raises(ClosedResourceError):
            await stapled.send(b"world")

        assert await stapled.receive() == b"hello, world"

    async def test_aclose(self, stapled: StapledByteStream) -> None:
        await stapled.aclose()
        with pytest.raises(ClosedResourceError):
            await stapled.receive()
        with pytest.raises(ClosedResourceError):
            await stapled.send(b"")

    async def test_aclose_sender_error_closes_receiver(
        self, stapled: StapledByteStream, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        error = RuntimeError("sender close failed")

        async def close_sender() -> None:
            raise error

        monkeypatch.setattr(stapled.send_stream, "aclose", close_sender)
        with pytest.raises(RuntimeError) as exc:
            await stapled.aclose()

        assert exc.value is error
        with pytest.raises(ClosedResourceError):
            await stapled.receive()
        with pytest.raises(ClosedResourceError):
            await stapled.send(b"")

    async def test_aclose_receiver_error_preserves_origin(self) -> None:
        error = RuntimeError("receiver close failed")
        stapled = StapledByteStream(
            FailableByteSendStream(), FailableByteReceiveStream(close_error=error)
        )
        with pytest.raises(RuntimeError) as exc:
            await stapled.aclose()

        assert exc.value is error

    async def test_aclose_remembers_first_error_and_closes_all(self) -> None:
        first_error = RuntimeError("sender close failed")
        second_error = RuntimeError("receiver close failed")
        send_stream = FailableByteSendStream(close_error=first_error)
        receive_stream = FailableByteReceiveStream(close_error=second_error)
        stapled = StapledByteStream(send_stream, receive_stream)

        for _ in range(2):
            with pytest.raises(RuntimeError) as exc:
                await stapled.aclose()

            assert exc.value is first_error

        assert send_stream.close_calls == 1
        assert receive_stream.close_calls == 1

    async def test_sender_that_closed_receiver_before_error_closes_once(
        self,
    ) -> None:
        shared_receiver = FailableByteReceiveStream()

        class SenderThatClosesSharedResource(ByteSendStream):
            async def send(self, item: bytes) -> None:
                return None

            async def aclose(self) -> None:
                await shared_receiver.aclose()
                raise OSError("first side")

        stapled = StapledByteStream(SenderThatClosesSharedResource(), shared_receiver)

        with pytest.raises(OSError, match="first side") as exc_info:
            await stapled.aclose()

        assert str(exc_info.value) == "first side"
        assert shared_receiver.close_calls == 1
        with pytest.raises(ClosedResourceError):
            await stapled.receive()
        with pytest.raises(ClosedResourceError):
            await stapled.send(b"")

    async def test_owner_field_is_explicit_ownership_parameter(self) -> None:
        assert "owner" in StapledByteStream.__dataclass_fields__
        owner = StreamCloseGroup()
        stapled = StapledByteStream(
            DummyByteSendStream(), DummyByteReceiveStream(b""), owner
        )
        assert stapled.owner is owner
        assert stapled.close_group is owner

    async def test_aclose_shielded_from_cancellation(
        self, receive_stream: DummyByteReceiveStream
    ) -> None:
        gate = Event()
        send_stream = BlockedByteSendStream(gate)
        stapled = StapledByteStream(send_stream, receive_stream)

        async def unblock_then_cancel(scope: CancelScope) -> None:
            await wait_all_tasks_blocked()
            gate.set()
            await wait_all_tasks_blocked()
            scope.cancel()

        with CancelScope() as scope:
            async with create_task_group() as tg:
                tg.start_soon(unblock_then_cancel, scope)
                await stapled.aclose()

        assert scope.cancel_called
        assert send_stream._closed
        assert receive_stream._closed
        with pytest.raises(ClosedResourceError):
            await stapled.receive()
        with pytest.raises(ClosedResourceError):
            await stapled.send(b"")

    async def test_aclose_task_cancelled_still_closes_all(
        self, receive_stream: DummyByteReceiveStream
    ) -> None:
        cancelled_exc = get_cancelled_exc_class()
        send_stream = DummyByteSendStream()
        stapled = StapledByteStream(send_stream, receive_stream)

        with CancelScope() as scope:
            scope.cancel()
            with pytest.raises(cancelled_exc):
                await stapled.aclose()

        assert send_stream._closed
        assert receive_stream._closed
        with pytest.raises(ClosedResourceError):
            await stapled.receive()
        with pytest.raises(ClosedResourceError):
            await stapled.send(b"")

    async def test_aclose_same_underlying_stream_only_once(self) -> None:
        underlying = DummyFullDuplexByteStream()
        stapled = StapledByteStream(underlying, underlying)
        await stapled.aclose()

        assert underlying.close_calls == 1

    async def test_shared_close_group_closes_underlying_once(self) -> None:
        error = RuntimeError("shared close failed")
        shared = DummyFullDuplexByteStream(close_error=error)
        group = StreamCloseGroup()
        first = StapledByteStream(shared, DummyByteReceiveStream(b""), group)
        second = StapledByteStream(DummyByteSendStream(), shared, group)

        with pytest.raises(RuntimeError) as exc:
            await first.aclose()

        assert exc.value is error
        assert shared.close_calls == 1
        assert second.close_group.closed
        with pytest.raises(ClosedResourceError):
            await second.receive()
        with pytest.raises(ClosedResourceError):
            await second.send(b"")

    async def test_concurrent_aclose_closes_each_half_once(self) -> None:
        send_stream = FailableByteSendStream()
        receive_stream = FailableByteReceiveStream()
        stapled = StapledByteStream(send_stream, receive_stream)

        async with create_task_group() as task_group:
            task_group.start_soon(stapled.aclose)
            task_group.start_soon(stapled.aclose)

        assert send_stream.close_calls == 1
        assert receive_stream.close_calls == 1

    async def test_private_group_by_default(self, stapled: StapledByteStream) -> None:
        other = StapledByteStream(DummyByteSendStream(), DummyByteReceiveStream(b""))
        assert stapled.close_group is not other.close_group

    async def test_register_with_closed_group_fails(self) -> None:
        group = StreamCloseGroup()
        await group.aclose()
        with pytest.raises(RuntimeError, match="closed stream close group"):
            group.register(DummyByteSendStream())

    async def test_send_eof_does_not_close_receiver(
        self, stapled: StapledByteStream
    ) -> None:
        await stapled.send_eof()
        await stapled.send_eof()
        with pytest.raises(ClosedResourceError):
            await stapled.send(b"world")

        assert await stapled.receive() == b"hello, world"
        await stapled.aclose()

    @pytest.mark.parametrize("max_bytes", [0, -1])
    async def test_receive_invalid_max_bytes(
        self, stapled: StapledByteStream, max_bytes: int
    ) -> None:
        with pytest.raises(ValueError, match="max_bytes must be a positive integer"):
            await stapled.receive(max_bytes)


T_Item = TypeVar("T_Item")


@dataclass
class DummyObjectReceiveStream(ObjectReceiveStream[T_Item]):
    data: InitVar[Iterable[T_Item]]
    buffer: deque[T_Item] = field(init=False)
    _closed: bool = field(init=False, default=False)

    def __post_init__(self, data: Iterable[T_Item]) -> None:
        self.buffer = deque(data)

    async def receive(self) -> T_Item:
        if self._closed:
            raise ClosedResourceError
        if not self.buffer:
            raise EndOfStream

        return self.buffer.popleft()

    async def aclose(self) -> None:
        self._closed = True


@dataclass
class DummyObjectSendStream(ObjectSendStream[T_Item]):
    buffer: list[T_Item] = field(init=False, default_factory=list)
    _closed: bool = field(init=False, default=False)

    async def send(self, item: T_Item) -> None:
        if self._closed:
            raise ClosedResourceError

        self.buffer.append(item)

    async def aclose(self) -> None:
        self._closed = True


class DummyObjectSendStreamWithSendNowait(DummyObjectSendStream[T_Item]):
    def send_nowait(self, item: T_Item) -> None:
        if self._closed:
            raise ClosedResourceError

        self.buffer.append(item)


class DummyObjectSendStreamWithBrokenSendNowait(DummyObjectSendStream[T_Item]):
    def send_nowait(self, item: T_Item) -> None:
        raise AttributeError("no attribute 'nonexistent'")


@dataclass
class FailableObjectSendStream(ObjectSendStream[T_Item], Generic[T_Item]):
    close_error: BaseException | None = None
    close_calls: int = field(init=False, default=0)
    _closed: bool = field(init=False, default=False)

    async def send(self, item: T_Item) -> None:
        if self._closed:
            raise ClosedResourceError

    async def aclose(self) -> None:
        self.close_calls += 1
        self._closed = True
        if self.close_error is not None:
            raise self.close_error


@dataclass
class FailableObjectReceiveStream(ObjectReceiveStream[T_Item], Generic[T_Item]):
    close_error: BaseException | None = None
    close_calls: int = field(init=False, default=0)
    _closed: bool = field(init=False, default=False)

    async def receive(self) -> T_Item:
        if self._closed:
            raise ClosedResourceError

        raise EndOfStream

    async def aclose(self) -> None:
        self.close_calls += 1
        self._closed = True
        if self.close_error is not None:
            raise self.close_error


class DummyFullDuplexObjectStream(ObjectStream[T_Item], Generic[T_Item]):
    """A single bidirectional object stream used as both the halves."""

    def __init__(self, close_error: BaseException | None = None) -> None:
        self.close_error = close_error
        self.close_calls = 0
        self._closed = False

    async def receive(self) -> T_Item:
        if self._closed:
            raise ClosedResourceError

        raise EndOfStream

    async def send(self, item: T_Item) -> None:
        if self._closed:
            raise ClosedResourceError

    async def send_eof(self) -> None:
        pass

    async def aclose(self) -> None:
        self.close_calls += 1
        self._closed = True
        if self.close_error is not None:
            raise self.close_error


class TestStapledObjectStream:
    @pytest.fixture
    def receive_stream(self) -> DummyObjectReceiveStream[str]:
        return DummyObjectReceiveStream(["hello", "world"])

    @pytest.fixture
    def send_stream(self) -> DummyObjectSendStream[str]:
        return DummyObjectSendStream[str]()

    @pytest.fixture
    def send_stream_with_send_nowait(self) -> DummyObjectSendStreamWithSendNowait[str]:
        return DummyObjectSendStreamWithSendNowait[str]()

    @pytest.fixture
    def stapled(
        self,
        receive_stream: DummyObjectReceiveStream[str],
        send_stream: DummyObjectSendStream[str],
    ) -> StapledObjectStream[str]:
        return StapledObjectStream(send_stream, receive_stream)

    @pytest.fixture
    def stapled_with_send_nowait(
        self,
        receive_stream: DummyObjectReceiveStream[str],
        send_stream_with_send_nowait: DummyObjectSendStreamWithSendNowait[str],
    ) -> StapledObjectStream[str]:
        return StapledObjectStream(send_stream_with_send_nowait, receive_stream)

    async def test_receive_send(
        self, stapled: StapledObjectStream[str], send_stream: DummyObjectSendStream[str]
    ) -> None:
        assert await stapled.receive() == "hello"
        assert await stapled.receive() == "world"
        with pytest.raises(EndOfStream):
            await stapled.receive()

        await stapled.send("how are you ")
        await stapled.send("today?")
        assert stapled.send_stream is send_stream
        assert send_stream.buffer == ["how are you ", "today?"]

    def test_send_nowait_implemented(
        self,
        stapled_with_send_nowait: StapledObjectStream[str],
        send_stream_with_send_nowait: DummyObjectSendStream[str],
    ) -> None:
        stapled_with_send_nowait.send_nowait("hello")
        assert send_stream_with_send_nowait.buffer == ["hello"]

    def test_send_nowait_not_implemented(
        self, stapled: StapledObjectStream[str], send_stream: DummyObjectSendStream[str]
    ) -> None:
        with pytest.raises(
            NotImplementedError,
            match="'send_nowait' method not implemented in "
            "<class 'tests.streams.test_stapled.DummyObjectSendStream'>",
        ):
            stapled.send_nowait("hello")

    def test_send_nowait_does_not_mask_attribute_error(
        self, receive_stream: DummyObjectReceiveStream[str]
    ) -> None:
        stapled = StapledObjectStream(
            DummyObjectSendStreamWithBrokenSendNowait[str](), receive_stream
        )
        with pytest.raises(AttributeError, match="no attribute 'nonexistent'"):
            stapled.send_nowait("hello")

    async def test_send_eof(self, stapled: StapledObjectStream[str]) -> None:
        await stapled.send_eof()
        await stapled.send_eof()
        with pytest.raises(ClosedResourceError):
            await stapled.send("world")

        assert await stapled.receive() == "hello"
        assert await stapled.receive() == "world"

    async def test_aclose(self, stapled: StapledObjectStream[str]) -> None:
        await stapled.aclose()
        with pytest.raises(ClosedResourceError):
            await stapled.receive()
        with pytest.raises(ClosedResourceError):
            await stapled.send(b"")  # type: ignore[arg-type]

    async def test_aclose_sender_error_closes_receiver(
        self, stapled: StapledObjectStream[str], monkeypatch: pytest.MonkeyPatch
    ) -> None:
        error = RuntimeError("sender close failed")

        async def close_sender() -> None:
            raise error

        monkeypatch.setattr(stapled.send_stream, "aclose", close_sender)
        with pytest.raises(RuntimeError) as exc:
            await stapled.aclose()

        assert exc.value is error
        with pytest.raises(ClosedResourceError):
            await stapled.receive()
        with pytest.raises(ClosedResourceError):
            await stapled.send("")

    async def test_aclose_remembers_first_error_and_closes_all(self) -> None:
        first_error = RuntimeError("sender close failed")
        second_error = RuntimeError("receiver close failed")
        send_stream = FailableObjectSendStream[str](close_error=first_error)
        receive_stream = FailableObjectReceiveStream[str](close_error=second_error)
        stapled = StapledObjectStream(send_stream, receive_stream)

        for _ in range(2):
            with pytest.raises(RuntimeError) as exc:
                await stapled.aclose()

            assert exc.value is first_error

        assert send_stream.close_calls == 1
        assert receive_stream.close_calls == 1

    async def test_aclose_task_cancelled_still_closes_all(self) -> None:
        cancelled_exc = get_cancelled_exc_class()
        send_stream = DummyObjectSendStream[str]()
        receive_stream = DummyObjectReceiveStream[str](["hello"])
        stapled = StapledObjectStream(send_stream, receive_stream)

        with CancelScope() as scope:
            scope.cancel()
            with pytest.raises(cancelled_exc):
                await stapled.aclose()

        assert send_stream._closed
        assert receive_stream._closed
        with pytest.raises(ClosedResourceError):
            await stapled.receive()

    async def test_aclose_same_underlying_stream_only_once(self) -> None:
        underlying = DummyFullDuplexObjectStream[str]()
        stapled = StapledObjectStream(underlying, underlying)
        await stapled.aclose()

        assert underlying.close_calls == 1

    async def test_shared_close_group_closes_underlying_once(self) -> None:
        error = RuntimeError("shared close failed")
        shared = DummyFullDuplexObjectStream[str](close_error=error)
        group = StreamCloseGroup()
        first = StapledObjectStream(shared, DummyObjectReceiveStream[str]([]), group)
        second = StapledObjectStream(DummyObjectSendStream[str](), shared, group)

        with pytest.raises(RuntimeError) as exc:
            await first.aclose()

        assert exc.value is error
        assert shared.close_calls == 1
        assert second.close_group.closed
        with pytest.raises(ClosedResourceError):
            await second.receive()

    async def test_private_group_by_default(
        self, stapled: StapledObjectStream[str]
    ) -> None:
        other = StapledObjectStream(
            DummyObjectSendStream[str](), DummyObjectReceiveStream[str]([])
        )
        assert stapled.close_group is not other.close_group

    async def test_send_nowait_after_close(
        self, stapled: StapledObjectStream[str]
    ) -> None:
        await stapled.aclose()
        with pytest.raises(ClosedResourceError):
            stapled.send_nowait("hello")
