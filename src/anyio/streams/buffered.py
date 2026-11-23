from __future__ import annotations

__all__ = (
    "BufferedByteReceiveStream",
    "BufferedByteStream",
    "BufferedConnectable",
    "Checkpoint",
)

import sys
from collections.abc import Callable, Iterable, Mapping
from dataclasses import dataclass, field
from typing import Any, Self, SupportsIndex

from .. import (
    CheckpointBudgetExceeded,
    ClosedResourceError,
    DelimiterNotFound,
    EndOfStream,
    IncompleteRead,
)
from ..abc import (
    AnyByteReceiveStream,
    AnyByteStream,
    AnyByteStreamConnectable,
    ByteReceiveStream,
    ByteStream,
    ByteStreamConnectable,
)

if sys.version_info >= (3, 12):
    from typing import override
else:
    from typing_extensions import override


@dataclass(eq=False)
class Checkpoint:
    """
    A bounded, resettable mark on a :class:`BufferedByteReceiveStream`.

    A checkpoint lets a consumer speculatively parse an optional prefix of the stream.
    Bytes consumed while the checkpoint is active are not discarded: they are held aside
    until the checkpoint is either committed (the bytes are permanently consumed) or
    rolled back (the bytes are restored to the front of the buffer so that another
    parser can re-read them from the exact same position).

    Only one checkpoint may be active on a stream at a time. The upstream stream is
    still strictly forward-only: rolling back only replays bytes that were already
    pulled into the buffer, never re-reads anything from upstream.

    Checkpoints can be used as async context managers; leaving the ``with`` block
    without an explicit decision rolls the checkpoint back::

        async with stream.checkpoint(64) as checkpoint:
            prefix = await stream.receive_exactly(4)
            if prefix == b"GOOD":
                checkpoint.commit()
            # otherwise, the checkpoint is rolled back on block exit

    :param stream: the buffered stream the checkpoint is attached to
    :param budget: maximum number of bytes that may be consumed through the
        checkpoint (``None`` for an unlimited budget); once the budget is exhausted,
        further reads raise :exc:`~anyio.CheckpointBudgetExceeded` without consuming
        any additional bytes
    """

    stream: BufferedByteReceiveStream
    budget: int | None
    _held: bytearray = field(init=False, default_factory=bytearray)
    _closed: bool = field(init=False, default=False)

    @property
    def consumed(self) -> int:
        """Number of bytes consumed through this checkpoint so far."""
        return len(self._held)

    @property
    def remaining(self) -> int | None:
        """Bytes left in the budget, or ``None`` when the budget is unlimited."""
        if self.budget is None:
            return None

        return self.budget - len(self._held)

    @property
    def closed(self) -> bool:
        """``True`` after the checkpoint has been committed or rolled back."""
        return self._closed

    def _ensure_open(self) -> None:
        if self._closed:
            raise RuntimeError("This checkpoint has already been closed")

    def _charge(self, nbytes: int) -> None:
        """Validate that ``nbytes`` more bytes fit within the budget."""
        self._ensure_open()
        if self.budget is not None and len(self._held) + nbytes > self.budget:
            raise CheckpointBudgetExceeded(self.budget)

    def commit(self) -> None:
        """
        Commit the checkpoint, permanently discarding the bytes consumed through it.

        After this call the stream position stays where the speculative read left it.

        :raises RuntimeError: if the checkpoint has already been closed

        """
        self._ensure_open()
        self._closed = True
        self.stream._release_checkpoint(self)

    def rollback(self) -> None:
        """
        Roll the checkpoint back, restoring every consumed byte to the buffer.

        The restored bytes are placed before any bytes that were already buffered but
        not yet consumed, so the next read returns the first byte read after the
        checkpoint was taken.

        :raises RuntimeError: if the checkpoint has already been closed

        """
        self._ensure_open()
        self._closed = True
        self.stream._release_checkpoint(self, restore=self._held)

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(
        self,
        exc_type: object,
        exc_val: object,
        exc_tb: object,
    ) -> None:
        if not self._closed:
            self.rollback()


@dataclass(eq=False)
class BufferedByteReceiveStream(ByteReceiveStream):
    """
    Wraps any bytes-based receive stream and uses a buffer to provide sophisticated
    receiving capabilities in the form of a byte stream.
    """

    receive_stream: AnyByteReceiveStream
    _buffer: bytearray = field(init=False, default_factory=bytearray)
    _checkpoint: Checkpoint | None = field(init=False, default=None)
    _closed: bool = field(init=False, default=False)

    async def aclose(self) -> None:
        await self.receive_stream.aclose()
        self._closed = True

    @property
    def buffer(self) -> bytes:
        """The bytes currently in the buffer (not including held checkpoint bytes)."""
        return bytes(self._buffer)

    @property
    def extra_attributes(self) -> Mapping[Any, Callable[[], Any]]:
        return self.receive_stream.extra_attributes

    def feed_data(self, data: Iterable[SupportsIndex], /) -> None:
        """
        Append data directly into the buffer.

        Any data in the buffer will be consumed by receive operations before receiving
        anything from the wrapped stream.

        :param data: the data to append to the buffer (can be bytes or anything else
            that supports ``__index__()``)

        """
        self._buffer.extend(data)

    def checkpoint(self, budget: int | None = None) -> Checkpoint:
        """
        Open a bounded checkpoint at the current stream position.

        :param budget: maximum number of bytes that may be consumed while the
            checkpoint is open, or ``None`` for no limit
        :return: the new checkpoint
        :raises ValueError: if ``budget`` is not a positive integer
        :raises RuntimeError: if another checkpoint is already active on this stream

        """
        if self._closed:
            raise ClosedResourceError
        if budget is not None and budget < 1:
            raise ValueError("budget must be a positive integer or None")
        if self._checkpoint is not None:
            raise RuntimeError("This stream already has an active checkpoint")

        checkpoint = Checkpoint(self, budget)
        self._checkpoint = checkpoint
        return checkpoint

    def _release_checkpoint(
        self, checkpoint: Checkpoint, restore: bytearray | None = None
    ) -> None:
        if self._checkpoint is not checkpoint:
            raise RuntimeError("The checkpoint is not active on this stream")

        self._checkpoint = None
        if restore:
            self._buffer[:0] = restore

    def _consume_buffered(self, nbytes: int) -> bytes:
        """
        Remove up to ``nbytes`` bytes from the front of the buffer.

        When a checkpoint is active, the removed bytes are held by the checkpoint
        instead of being discarded, so they can later be restored on a rollback.
        """
        nbytes = min(nbytes, len(self._buffer))
        chunk = bytes(self._buffer[:nbytes])
        del self._buffer[:nbytes]
        if self._checkpoint is not None:
            self._checkpoint._held.extend(chunk)

        return chunk

    async def receive(self, max_bytes: int = 65536) -> bytes:
        if max_bytes < 1:
            raise ValueError("max_bytes must be a positive integer")

        if self._closed:
            raise ClosedResourceError

        if self._checkpoint is not None:
            remaining_budget = self._checkpoint.remaining
            if remaining_budget is not None:
                # An exhausted budget rejects every further read, even when the
                # buffer still holds bytes that were pulled in but not consumed.
                if remaining_budget == 0:
                    raise CheckpointBudgetExceeded(self._checkpoint.budget)

                # The request can only be shrunk to fit the budget when the bytes are
                # already buffered; otherwise reject it before touching upstream.
                if remaining_budget < max_bytes and len(self._buffer) < max_bytes:
                    raise CheckpointBudgetExceeded(self._checkpoint.budget)

                max_bytes = min(max_bytes, remaining_budget)

        # Serve as much as possible from already buffered data first, so that the
        # budget only has to cover bytes that would actually be pulled in.
        if self._buffer:
            chunk = self._consume_buffered(max_bytes)
            max_bytes -= len(chunk)
            if max_bytes == 0:
                return chunk
        else:
            chunk = b""

        if self._checkpoint is not None:
            self._checkpoint._charge(max_bytes)

        if isinstance(self.receive_stream, ByteReceiveStream):
            extra = await self.receive_stream.receive(max_bytes)
        else:
            # With a bytes-oriented object stream, we need to handle any surplus bytes
            # we get from the receive() call
            while True:
                if extra := await self.receive_stream.receive():
                    break

        if len(extra) > max_bytes:
            # Save the surplus bytes in the buffer. With an active checkpoint the
            # surplus stays out of the held bytes so the budget is never exceeded.
            self._buffer.extend(extra[max_bytes:])
            extra = extra[:max_bytes]

        if self._checkpoint is not None:
            self._checkpoint._held.extend(extra)

        return chunk + extra

    async def receive_exactly(self, nbytes: int) -> bytes:
        """
        Read exactly the given amount of bytes from the stream.

        :param nbytes: the number of bytes to read
        :return: the bytes read
        :raises ~anyio.IncompleteRead: if the stream was closed before the requested
            amount of bytes could be read from the stream
        :raises ~anyio.CheckpointBudgetExceeded: if an active checkpoint's budget does
            not allow reading this many bytes
        :raises ~anyio.ClosedResourceError: if this stream has been explicitly closed

        """
        if self._closed:
            raise ClosedResourceError

        if self._checkpoint is not None:
            self._checkpoint._charge(nbytes)

        parts: list[bytes] = []
        remaining = nbytes
        while remaining > 0:
            if self._buffer:
                part = self._consume_buffered(remaining)
                parts.append(part)
                remaining -= len(part)
                continue

            try:
                if isinstance(self.receive_stream, ByteReceiveStream):
                    chunk = await self.receive_stream.receive(remaining)
                else:
                    chunk = await self.receive_stream.receive()
            except EndOfStream as exc:
                raise IncompleteRead from exc

            if len(chunk) > remaining:
                # Save the surplus bytes in the buffer for later
                self._buffer.extend(chunk[remaining:])
                chunk = chunk[:remaining]

            if self._checkpoint is not None:
                self._checkpoint._held.extend(chunk)

            parts.append(chunk)
            remaining -= len(chunk)

        return b"".join(parts)

    async def receive_until(self, delimiter: bytes, max_bytes: int) -> bytes:
        """
        Read from the stream until the delimiter is found or max_bytes have been read.

        :param delimiter: the marker to look for in the stream
        :param max_bytes: maximum number of bytes that will be read before raising
            :exc:`~anyio.DelimiterNotFound`
        :return: the bytes read (not including the delimiter)
        :raises ~anyio.IncompleteRead: if the stream was closed before the delimiter
            was found
        :raises ~anyio.DelimiterNotFound: if the delimiter is not found within the
            bytes read up to the maximum allowed
        :raises ~anyio.CheckpointBudgetExceeded: if an active checkpoint's budget is
            exhausted before the delimiter is found
        :raises ~anyio.ClosedResourceError: if this stream has been explicitly closed

        """
        if self._closed:
            raise ClosedResourceError

        delimiter_size = len(delimiter)
        offset = 0
        while True:
            # Nothing is consumed (and thus nothing is held by the checkpoint) until
            # the delimiter has been found, so the whole search window stays in the
            # buffer across iterations.
            index = self._buffer.find(delimiter, offset, max_bytes)
            if index >= 0:
                found = self._consume_buffered(index + delimiter_size)
                return found[:-delimiter_size]

            # Check if the limit has been reached without finding the delimiter
            if len(self._buffer) >= max_bytes:
                raise DelimiterNotFound(max_bytes)

            if self._checkpoint is not None:
                # Pulling another chunk may bring at least one more consumable byte
                self._checkpoint._charge(len(self._buffer) + 1)

            # Read more data into the buffer from the socket
            try:
                data = await self.receive_stream.receive()
            except EndOfStream as exc:
                raise IncompleteRead from exc

            # Move the offset forward and add the new data to the buffer
            offset = max(len(self._buffer) - delimiter_size + 1, 0)
            self._buffer.extend(data)


class BufferedByteStream(BufferedByteReceiveStream, ByteStream):
    """
    A full-duplex variant of :class:`BufferedByteReceiveStream`. All writes are passed
    through to the wrapped stream as-is.
    """

    def __init__(self, stream: AnyByteStream):
        """
        :param stream: the stream to be wrapped

        """
        super().__init__(stream)
        self._stream = stream

    @override
    async def send_eof(self) -> None:
        await self._stream.send_eof()

    @override
    async def send(self, item: bytes) -> None:
        await self._stream.send(item)


class BufferedConnectable(ByteStreamConnectable):
    """
    Wraps a byte stream connectable to produce :class:`BufferedByteStream` connections.

    Use this when you want the streams returned by :meth:`connect` to have the buffered
    receive API (e.g. :meth:`~BufferedByteReceiveStream.receive_exactly` and
    :meth:`~BufferedByteReceiveStream.receive_until`).

    :param connectable: the byte stream connectable to wrap
    """

    def __init__(self, connectable: AnyByteStreamConnectable):
        """
        :param connectable: the connectable to wrap

        """
        self.connectable = connectable

    @override
    async def connect(self) -> BufferedByteStream:
        stream = await self.connectable.connect()
        return BufferedByteStream(stream)
