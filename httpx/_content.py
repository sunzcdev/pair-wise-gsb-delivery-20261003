from __future__ import annotations

import codecs
import inspect
import warnings
from json import dumps as json_dumps
from typing import (
    Any,
    AsyncIterable,
    AsyncIterator,
    Iterable,
    Iterator,
    Mapping,
)
from urllib.parse import quote_plus, urlencode

from ._exceptions import StreamClosed, StreamConsumed
from ._multipart import MultipartStream
from ._types import (
    AsyncByteStream,
    RequestContent,
    RequestData,
    RequestFiles,
    ResponseContent,
    SyncByteStream,
)
from ._utils import peek_filelike_length, primitive_value_to_str

__all__ = ["ByteStream"]


class ByteStream(AsyncByteStream, SyncByteStream):
    def __init__(self, stream: bytes) -> None:
        self._stream = stream

    def __iter__(self) -> Iterator[bytes]:
        yield self._stream

    async def __aiter__(self) -> AsyncIterator[bytes]:
        yield self._stream


class IteratorByteStream(SyncByteStream):
    CHUNK_SIZE = 65_536

    def __init__(self, stream: Iterable[bytes]) -> None:
        self._stream = stream
        self._is_stream_consumed = False
        self._is_generator = inspect.isgenerator(stream)

    def __iter__(self) -> Iterator[bytes]:
        if self._is_stream_consumed and self._is_generator:
            raise StreamConsumed()

        self._is_stream_consumed = True
        if hasattr(self._stream, "read"):
            # File-like interfaces should use 'read' directly.
            chunk = self._stream.read(self.CHUNK_SIZE)
            while chunk:
                yield chunk
                chunk = self._stream.read(self.CHUNK_SIZE)
        else:
            # Otherwise iterate.
            for part in self._stream:
                yield part


class AsyncIteratorByteStream(AsyncByteStream):
    CHUNK_SIZE = 65_536

    def __init__(self, stream: AsyncIterable[bytes]) -> None:
        self._stream = stream
        self._is_stream_consumed = False
        self._is_generator = inspect.isasyncgen(stream)

    async def __aiter__(self) -> AsyncIterator[bytes]:
        if self._is_stream_consumed and self._is_generator:
            raise StreamConsumed()

        self._is_stream_consumed = True
        if hasattr(self._stream, "aread"):
            # File-like interfaces should use 'aread' directly.
            chunk = await self._stream.aread(self.CHUNK_SIZE)
            while chunk:
                yield chunk
                chunk = await self._stream.aread(self.CHUNK_SIZE)
        else:
            # Otherwise iterate.
            async for part in self._stream:
                yield part


class UnattachedStream(AsyncByteStream, SyncByteStream):
    """
    If a request or response is serialized using pickle, then it is no longer
    attached to a stream for I/O purposes. Any stream operations should result
    in `httpx.StreamClosed`.
    """

    def __iter__(self) -> Iterator[bytes]:
        raise StreamClosed()

    async def __aiter__(self) -> AsyncIterator[bytes]:
        raise StreamClosed()
        yield b""  # pragma: no cover


def encode_content(
    content: str | bytes | Iterable[bytes] | AsyncIterable[bytes],
) -> tuple[dict[str, str], SyncByteStream | AsyncByteStream]:
    if isinstance(content, (bytes, str)):
        body = content.encode("utf-8") if isinstance(content, str) else content
        content_length = len(body)
        headers = {"Content-Length": str(content_length)} if body else {}
        return headers, ByteStream(body)

    elif isinstance(content, Iterable) and not isinstance(content, dict):
        # `not isinstance(content, dict)` is a bit oddly specific, but it
        # catches a case that's easy for users to make in error, and would
        # otherwise pass through here, like any other bytes-iterable,
        # because `dict` happens to be iterable. See issue #2491.
        content_length_or_none = peek_filelike_length(content)

        if content_length_or_none is None:
            headers = {"Transfer-Encoding": "chunked"}
        else:
            headers = {"Content-Length": str(content_length_or_none)}
        return headers, IteratorByteStream(content)  # type: ignore

    elif isinstance(content, AsyncIterable):
        headers = {"Transfer-Encoding": "chunked"}
        return headers, AsyncIteratorByteStream(content)

    raise TypeError(f"Unexpected type for 'content', {type(content)!r}")


def _is_form_pairs(data: Any) -> bool:
    """
    Return `True` if `data` is an ordered sequence of `(key, value)` pairs,
    rather than a mapping or raw content.
    """
    return isinstance(data, (list, tuple)) and all(
        isinstance(item, (list, tuple)) and len(item) == 2 for item in data
    )


def normalize_form_charset(charset: str) -> str:
    """
    Validate a form encoding charset, returning its canonical codec name.
    """
    try:
        return codecs.lookup(charset).name
    except LookupError:
        raise ValueError(f"Unknown charset for form encoding: {charset!r}") from None


def charsets_match(first: str, second: str) -> bool:
    """
    Return `True` if two charset names refer to the same codec.
    """
    try:
        return codecs.lookup(first) == codecs.lookup(second)
    except LookupError:
        return first.lower() == second.lower()


def encode_urlencoded_data(
    data: RequestData, charset: str | None = None
) -> tuple[dict[str, str], ByteStream]:
    plain_data: list[tuple[Any, str]] = []
    items = data.items() if isinstance(data, Mapping) else data
    for key, value in items:
        if isinstance(value, (list, tuple)):
            plain_data.extend((key, primitive_value_to_str(item)) for item in value)
        else:
            plain_data.append((key, primitive_value_to_str(value)))

    if charset is None:
        body = urlencode(plain_data, doseq=True).encode("utf-8")
        content_type = "application/x-www-form-urlencoded"
    else:
        charset = normalize_form_charset(charset)
        encoded_pairs = []
        for position, (key, value) in enumerate(plain_data):
            if not isinstance(key, str):
                key = str(key)
            try:
                encoded_key = quote_plus(
                    key, safe="", encoding=charset, errors="strict"
                )
                encoded_value = quote_plus(
                    value, safe="", encoding=charset, errors="strict"
                )
            except UnicodeEncodeError as exc:
                raise ValueError(
                    f"Unable to encode form field #{position} (key={key!r}) "
                    f"using charset {charset!r}: {exc}"
                ) from None
            encoded_pairs.append(f"{encoded_key}={encoded_value}")
        body = "&".join(encoded_pairs).encode("ascii")
        content_type = f"application/x-www-form-urlencoded; charset={charset}"

    content_length = str(len(body))
    headers = {"Content-Length": content_length, "Content-Type": content_type}
    return headers, ByteStream(body)


def encode_multipart_data(
    data: RequestData, files: RequestFiles, boundary: bytes | None
) -> tuple[dict[str, str], MultipartStream]:
    multipart = MultipartStream(data=data, files=files, boundary=boundary)
    headers = multipart.get_headers()
    return headers, multipart


def encode_text(text: str) -> tuple[dict[str, str], ByteStream]:
    body = text.encode("utf-8")
    content_length = str(len(body))
    content_type = "text/plain; charset=utf-8"
    headers = {"Content-Length": content_length, "Content-Type": content_type}
    return headers, ByteStream(body)


def encode_html(html: str) -> tuple[dict[str, str], ByteStream]:
    body = html.encode("utf-8")
    content_length = str(len(body))
    content_type = "text/html; charset=utf-8"
    headers = {"Content-Length": content_length, "Content-Type": content_type}
    return headers, ByteStream(body)


def encode_json(json: Any) -> tuple[dict[str, str], ByteStream]:
    body = json_dumps(
        json, ensure_ascii=False, separators=(",", ":"), allow_nan=False
    ).encode("utf-8")
    content_length = str(len(body))
    content_type = "application/json"
    headers = {"Content-Length": content_length, "Content-Type": content_type}
    return headers, ByteStream(body)


def encode_request(
    content: RequestContent | None = None,
    data: RequestData | None = None,
    files: RequestFiles | None = None,
    json: Any | None = None,
    boundary: bytes | None = None,
    form_charset: str | None = None,
) -> tuple[dict[str, str], SyncByteStream | AsyncByteStream]:
    """
    Handles encoding the given `content`, `data`, `files`, and `json`,
    returning a two-tuple of (<headers>, <stream>).

    The `form_charset` switch only applies to URL-encoded form data.
    Raw `content` bodies and multipart uploads are not affected by it.
    """
    if data is not None and not isinstance(data, Mapping) and not _is_form_pairs(data):
        # We prefer to separate `content=<bytes|str|byte iterator|bytes aiterator>`
        # for raw request content, and `data=<form data>` for url encoded or
        # multipart form content.
        #
        # However for compat with requests, we *do* still support
        # `data=<bytes...>` usages. We deal with that case here, treating it
        # as if `content=<...>` had been supplied instead.
        message = "Use 'content=<...>' to upload raw bytes/text content."
        warnings.warn(message, DeprecationWarning, stacklevel=2)
        return encode_content(data)

    if content is not None:
        return encode_content(content)
    elif files:
        if data is not None and not isinstance(data, Mapping):
            # Multipart forms are keyed. Collapse any ordered
            # `(key, value)` pairs into a plain mapping.
            data = dict(data)
        return encode_multipart_data(data or {}, files, boundary)
    elif data:
        return encode_urlencoded_data(data, charset=form_charset)
    elif json is not None:
        return encode_json(json)

    return {}, ByteStream(b"")


def encode_response(
    content: ResponseContent | None = None,
    text: str | None = None,
    html: str | None = None,
    json: Any | None = None,
) -> tuple[dict[str, str], SyncByteStream | AsyncByteStream]:
    """
    Handles encoding the given `content`, returning a two-tuple of
    (<headers>, <stream>).
    """
    if content is not None:
        return encode_content(content)
    elif text is not None:
        return encode_text(text)
    elif html is not None:
        return encode_html(html)
    elif json is not None:
        return encode_json(json)

    return {}, ByteStream(b"")
