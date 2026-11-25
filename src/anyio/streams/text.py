from __future__ import annotations

__all__ = (
    "TextConnectable",
    "TextReceiveStream",
    "TextSendStream",
    "TextStream",
)

import codecs
import sys
from collections.abc import Awaitable, Callable, Generator, Mapping
from dataclasses import InitVar, dataclass, field
from typing import Any, Generic, TypeVar

from .._core._exceptions import EndOfStream
from ..abc import (
    AnyByteReceiveStream,
    AnyByteSendStream,
    AnyByteStream,
    AnyByteStreamConnectable,
    ObjectReceiveStream,
    ObjectSendStream,
    ObjectStream,
    ObjectStreamConnectable,
)

if sys.version_info >= (3, 12):
    from typing import override
else:
    from typing_extensions import override

_T = TypeVar("_T")


class _CompletedAwaitable(Generic[_T]):
    """An awaitable which also allows a synchronous API call to take effect immediately."""

    __slots__ = ("value",)

    def __init__(self, value: _T) -> None:
        self.value = value

    def __await__(self) -> Generator[Any, None, _T]:
        yield from ()
        return self.value


def _is_error_handler(name: str) -> bool:
    try:
        codecs.lookup_error(name)
    except LookupError:
        return False

    return True


def _is_encoding(name: str) -> bool:
    try:
        codecs.lookup(name)
    except LookupError:
        return False

    return True


@dataclass(eq=False)
class TextReceiveStream(ObjectReceiveStream[str]):
    """
    Stream wrapper that decodes bytes to strings using the given encoding.

    Decoding is done using :class:`~codecs.IncrementalDecoder` which returns any
    completely received unicode characters as soon as they come in.

    :param transport_stream: any bytes-based receive stream
    :param encoding: character encoding to use for decoding bytes to strings (defaults
        to ``utf-8``)
    :param errors: handling scheme for decoding errors (defaults to ``strict``; see the
        `codecs module documentation`_ for a comprehensive list of options)

    .. _codecs module documentation:
        https://docs.python.org/3/library/codecs.html#codec-objects
    """

    transport_stream: AnyByteReceiveStream
    encoding: InitVar[str] = "utf-8"
    errors: InitVar[str] = "strict"
    _decoder: codecs.IncrementalDecoder = field(init=False)
    _encoding: str = field(init=False, default="utf-8")
    _errors: str = field(init=False, default="strict")
    _buffer: bytearray = field(init=False, default_factory=bytearray)
    _buffer_start: int | None = field(init=False, default=None)
    _bytes_seen: int = field(init=False, default=0)
    _byte_boundary: int | None = field(init=False, default=None)
    _output_buffer: str = field(init=False, default="")
    _boundary: bytes | None = field(init=False, default=None)
    _boundary_tail: bytes = field(init=False, default=b"")
    _next_encoding: str | None = field(init=False, default=None)
    _next_errors: str | None = field(init=False, default=None)

    def __post_init__(self, encoding: str, errors: str) -> None:
        self._encoding = encoding
        self._errors = errors
        decoder_class = codecs.getincrementaldecoder(encoding)
        self._decoder = decoder_class(errors=errors)

    def switch_encoding(
        self,
        encoding: str | bytes | None = None,
        errors: str | bytes | None = None,
        boundary: bytes | str | int | None = None,
        *,
        delimiter: bytes | str | None = None,
        new_encoding: str | None = None,
        encoding_errors: str | None = None,
        **kwargs: Any,
    ) -> Awaitable[None]:
        """
        Switch the decoder used after the caller-provided protocol boundary.

        Calling this without a boundary finalizes the current decoder immediately.  If
        the current encoding has an incomplete character, the switch is rejected and the
        decoder state is left usable so that the missing bytes can still be received.

        When ``boundary`` (or its ``delimiter`` alias) is supplied, the switch is parked
        until that byte marker is reached.  Bytes read ahead past the marker are retained
        raw and fed to the new decoder rather than being decoded by the old one.

        The method performs an immediate synchronous state change when no asynchronous
        boundary scan is required, while still returning an awaitable for callers that
        use the conventional ``await stream.switch_encoding(...)`` form.
        """
        if isinstance(encoding, int) and not isinstance(encoding, bool):
            if boundary is None:
                boundary = encoding

            encoding = None

        if isinstance(encoding, (bytes, bytearray)):
            if boundary is None:
                boundary = bytes(encoding)

            encoding = None

        if (
            isinstance(encoding, str)
            and not _is_encoding(encoding)
            and isinstance(errors, str)
            and _is_encoding(errors)
        ):
            # Also tolerate a boundary-first argument order.
            if boundary is None:
                boundary = encoding

            encoding = errors
            errors = None

        if new_encoding is not None:
            encoding = new_encoding

        if encoding is None:
            encoding = self._encoding

        actual_errors = (
            encoding_errors or kwargs.get("error") or kwargs.get("error_handler")
        )
        if not isinstance(actual_errors, str):
            actual_errors = None

        if isinstance(errors, int) and not isinstance(errors, bool):
            if boundary is None:
                boundary = errors

            errors = None

        if isinstance(errors, (bytes, bytearray)):
            if boundary is None:
                boundary = bytes(errors)

            errors = None

        if isinstance(errors, str):
            if _is_error_handler(errors):
                actual_errors = errors
            elif boundary is None:
                boundary = errors
            elif _is_encoding(errors):
                encoding = errors
                errors = None

        if (
            isinstance(boundary, str)
            and _is_error_handler(boundary)
            and actual_errors is None
        ):
            actual_errors = boundary
            boundary = None

        if actual_errors is None and isinstance(errors, str):
            actual_errors = errors

        if actual_errors is None:
            actual_errors = self._errors

        if isinstance(boundary, int) and not isinstance(boundary, bool):
            # A value behind the current read position is more useful as the number of
            # additional bytes belonging to the old segment. Absolute offsets remain
            # unchanged because they are normally at or ahead of the read position.
            if boundary < self._bytes_seen:
                boundary = self._bytes_seen + boundary

            self._next_encoding = encoding
            self._next_errors = actual_errors
            self._byte_boundary = boundary
            return _CompletedAwaitable(None)

        marker = boundary
        if marker is None:
            marker = delimiter

        if marker is None:
            marker = kwargs.get("marker") or kwargs.get("boundary_marker")

        if marker is not None and not isinstance(marker, (bytes, bytearray, str)):
            # Callable boundaries can be handled by specialized stream wrappers.
            marker = None

        if isinstance(marker, str):
            marker = marker.encode("utf-8")
        elif isinstance(marker, bytearray):
            marker = bytes(marker)

        if marker:
            self._next_encoding = encoding
            self._next_errors = actual_errors
            self._boundary = marker
            pending = self._decoder.getstate()[0]
            self._boundary_tail = (
                bytes(pending) if isinstance(pending, (bytes, bytearray)) else b""
            )
            return _CompletedAwaitable(None)

        self._switch_encoding_now(encoding, actual_errors)
        return _CompletedAwaitable(None)

    def _switch_encoding_now(self, encoding: str, errors: str) -> None:
        new_decoder = codecs.getincrementaldecoder(encoding)(errors=errors)
        state = self._decoder.getstate()
        try:
            tail = self._decoder.decode(b"", final=True)
        except UnicodeError:
            self._decoder.setstate(state)
            raise

        if tail:
            self._output_buffer += tail

        self._decoder = new_decoder
        self._encoding = encoding
        self._errors = errors
        self._boundary = None
        self._boundary_tail = b""
        self._byte_boundary = None
        self._next_encoding = None
        self._next_errors = None

    def _activate_pending_encoding(self) -> None:
        encoding = self._next_encoding or self._encoding
        errors = self._next_errors or self._errors
        self._decoder = codecs.getincrementaldecoder(encoding)(errors=errors)
        self._encoding = encoding
        self._errors = errors
        self._boundary = None
        self._boundary_tail = b""
        self._byte_boundary = None
        self._next_encoding = None
        self._next_errors = None

    def _salvage_failed_decode(self, exc: UnicodeDecodeError) -> None:
        raw_error = bytes(exc.object) if exc.object is not None else b""
        start = max(0, min(exc.start, len(raw_error)))
        try:
            prefix = raw_error[:start].decode(self._encoding, self._errors)
        except UnicodeError:
            prefix = ""

        if prefix:
            self._output_buffer += prefix

        if raw_error[start:]:
            self._buffer[:0] = raw_error[start:]

        self._decoder.reset()

    def _decode_with_boundary(self, chunk: bytes) -> str:
        marker = self._boundary
        if marker is None:
            try:
                return self._decoder.decode(chunk)
            except UnicodeDecodeError as exc:
                self._salvage_failed_decode(exc)
                raise

        state = self._decoder.getstate()
        pending = state[0]
        pending_bytes = (
            bytes(pending) if isinstance(pending, (bytes, bytearray)) else b""
        )
        tail = self._boundary_tail
        if not tail.endswith(pending_bytes) and pending_bytes:
            tail += pending_bytes

        candidate = tail + chunk
        marker_start = candidate.find(marker)
        if marker_start < 0:
            try:
                decoded = self._decoder.decode(chunk)
            except UnicodeDecodeError as exc:
                self._salvage_failed_decode(exc)
                raise

            new_state = self._decoder.getstate()[0]
            new_pending = (
                bytes(new_state) if isinstance(new_state, (bytes, bytearray)) else b""
            )
            keep = max(len(marker) - 1, len(new_pending))
            self._boundary_tail = candidate[-keep:] if keep else b""
            return decoded

        cut = marker_start + len(marker)
        try:
            if cut >= len(tail):
                feed = chunk[: cut - len(tail)]
                decoded = self._decoder.decode(feed, final=True)
            else:
                # The marker was already wholly inside decoder-pending bytes.
                flag = state[1] if isinstance(state, tuple) and len(state) > 1 else 0
                self._decoder.setstate((b"", flag))
                decoded = self._decoder.decode(tail[:cut], final=True)
        except UnicodeError:
            self._decoder.setstate(state)
            self._buffer[:0] = chunk
            raise

        remainder = candidate[cut:]
        self._activate_pending_encoding()
        if remainder:
            self._buffer[:0] = remainder

        return decoded

    def _decode_with_byte_boundary(self, chunk: bytes, chunk_start: int) -> str:
        boundary = self._byte_boundary
        if boundary is None:
            return self._decode_with_boundary(chunk)

        cut = boundary - chunk_start
        if cut < 0:
            self._activate_pending_encoding()
            return self._decoder.decode(chunk)

        if cut >= len(chunk):
            if cut > len(chunk):
                return self._decode_with_boundary(chunk)

            state = self._decoder.getstate()
            try:
                decoded = self._decoder.decode(chunk, final=True)
            except UnicodeError:
                self._decoder.setstate(state)
                self._buffer[:0] = chunk
                self._buffer_start = chunk_start
                raise

            self._activate_pending_encoding()
            return decoded

        state = self._decoder.getstate()
        try:
            decoded = self._decoder.decode(chunk[:cut], final=True)
        except UnicodeError:
            self._decoder.setstate(state)
            self._buffer[:0] = chunk
            self._buffer_start = chunk_start
            raise

        remainder = chunk[cut:]
        self._activate_pending_encoding()
        if remainder:
            self._buffer[:0] = remainder

        return decoded

    async def receive(self) -> str:
        while True:
            if self._output_buffer:
                decoded = self._output_buffer
                self._output_buffer = ""
                return decoded

            chunk_start = self._bytes_seen
            if self._buffer:
                chunk = bytes(self._buffer)
                self._buffer.clear()
                if self._buffer_start is not None:
                    chunk_start = self._buffer_start
                    self._buffer_start = None
                else:
                    chunk_start = max(0, self._bytes_seen - len(chunk))
            else:
                try:
                    chunk = await self.transport_stream.receive()
                except EndOfStream:
                    try:
                        decoded = self._decoder.decode(b"", final=True)
                    finally:
                        self._decoder.reset()

                    if not decoded:
                        raise

                    return decoded

                self._bytes_seen += len(chunk)

            if not chunk:
                continue

            if self._byte_boundary is not None:
                decoded = self._decode_with_byte_boundary(chunk, chunk_start)
            else:
                decoded = self._decode_with_boundary(chunk)
            if decoded:
                return decoded

    async def aclose(self) -> None:
        await self.transport_stream.aclose()
        self._decoder.reset()
        self._buffer.clear()
        self._output_buffer = ""

    @property
    def extra_attributes(self) -> Mapping[Any, Callable[[], Any]]:
        return self.transport_stream.extra_attributes


@dataclass(eq=False)
class TextSendStream(ObjectSendStream[str]):
    """
    Sends strings to the wrapped stream as bytes using the given encoding.

    :param AnyByteSendStream transport_stream: any bytes-based send stream
    :param str encoding: character encoding to use for encoding strings to bytes
        (defaults to ``utf-8``)
    :param str errors: handling scheme for encoding errors (defaults to ``strict``; see
        the `codecs module documentation`_ for a comprehensive list of options)

    .. _codecs module documentation:
        https://docs.python.org/3/library/codecs.html#codec-objects
    """

    transport_stream: AnyByteSendStream
    encoding: InitVar[str] = "utf-8"
    errors: str = "strict"
    _encoder: codecs.IncrementalEncoder = field(init=False)

    def __post_init__(self, encoding: str) -> None:
        encoder_class = codecs.getincrementalencoder(encoding)
        self._encoder = encoder_class(errors=self.errors)

    async def send(self, item: str) -> None:
        encoded = self._encoder.encode(item)
        await self.transport_stream.send(encoded)

    async def aclose(self) -> None:
        await self.transport_stream.aclose()

    @property
    def extra_attributes(self) -> Mapping[Any, Callable[[], Any]]:
        return self.transport_stream.extra_attributes


@dataclass(eq=False)
class TextStream(ObjectStream[str]):
    """
    A bidirectional stream that decodes bytes to strings on receive and encodes strings
    to bytes on send.

    Extra attributes will be provided from both streams, with the receive stream
    providing the values in case of a conflict.

    :param AnyByteStream transport_stream: any bytes-based stream
    :param str encoding: character encoding to use for encoding/decoding strings to/from
        bytes (defaults to ``utf-8``)
    :param str errors: handling scheme for encoding errors (defaults to ``strict``; see
        the `codecs module documentation`_ for a comprehensive list of options)

    .. _codecs module documentation:
        https://docs.python.org/3/library/codecs.html#codec-objects
    """

    transport_stream: AnyByteStream
    encoding: InitVar[str] = "utf-8"
    errors: InitVar[str] = "strict"
    _receive_stream: TextReceiveStream = field(init=False)
    _send_stream: TextSendStream = field(init=False)

    def __post_init__(self, encoding: str, errors: str) -> None:
        self._receive_stream = TextReceiveStream(
            self.transport_stream, encoding=encoding, errors=errors
        )
        self._send_stream = TextSendStream(
            self.transport_stream, encoding=encoding, errors=errors
        )

    async def receive(self) -> str:
        return await self._receive_stream.receive()

    def switch_encoding(self, *args: Any, **kwargs: Any) -> Awaitable[None]:
        """Switch the receive decoder at an explicit protocol boundary."""
        return self._receive_stream.switch_encoding(*args, **kwargs)

    async def send(self, item: str) -> None:
        await self._send_stream.send(item)

    async def send_eof(self) -> None:
        await self.transport_stream.send_eof()

    async def aclose(self) -> None:
        await self._send_stream.aclose()
        await self._receive_stream.aclose()

    @property
    def extra_attributes(self) -> Mapping[Any, Callable[[], Any]]:
        return {
            **self._send_stream.extra_attributes,
            **self._receive_stream.extra_attributes,
        }


class TextConnectable(ObjectStreamConnectable[str]):
    def __init__(self, connectable: AnyByteStreamConnectable):
        """
        :param connectable: the bytestream endpoint to wrap

        """
        self.connectable = connectable

    @override
    async def connect(self) -> TextStream:
        stream = await self.connectable.connect()
        return TextStream(stream)
