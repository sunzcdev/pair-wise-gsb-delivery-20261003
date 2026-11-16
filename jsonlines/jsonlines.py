"""
jsonlines implementation
"""

import builtins
import codecs
import enum
import hashlib
import io
import json
import os
import types
import typing
from typing import (
    Any,
    Callable,
    Dict,
    Iterable,
    Iterator,
    List,
    Literal,
    Optional,
    Tuple,
    Type,
    TypeVar,
    Union,
    cast,
    overload,
)

import attr

orjson: Optional[types.ModuleType]
try:
    import orjson
except ImportError:
    orjson = None

ujson: Optional[types.ModuleType]
try:
    import ujson
except ImportError:
    ujson = None


VALID_TYPES = {
    bool,
    dict,
    float,
    int,
    list,
    str,
}

# Characters to skip at the beginning of a line. Note: at most one such
# character is skipped per line.
SKIPPABLE_SINGLE_INITIAL_CHARS = (
    "\x1e",  # RFC7464 text sequence
    codecs.BOM_UTF8.decode(),
)

# Version of the bookmark serialization format produced by this module. Bump
# this when the meaning or layout of the bookmark fields changes.
BOOKMARK_FORMAT_VERSION = 1

# Hash algorithm used to verify the immutable file prefix up to a bookmark.
BOOKMARK_HASH_ALGORITHM = "sha256"


class DumpsResultConversion(enum.Enum):
    LeaveAsIs = enum.auto()
    EncodeToBytes = enum.auto()
    DecodeToString = enum.auto()


# https://docs.python.org/3/library/functions.html#open
Openable = Union[str, bytes, int, os.PathLike]

LoadsCallable = Callable[[Union[str, bytes]], Any]
DumpsCallable = Callable[[Any], Union[str, bytes]]

# Currently, JSON structures cannot be typed properly:
# - https://github.com/python/typing/issues/182
# - https://github.com/python/mypy/issues/731
JSONCollection = Union[Dict[str, Any], List[Any]]
JSONScalar = Union[bool, float, int, str]
JSONValue = Union[JSONCollection, JSONScalar]
TJSONValue = TypeVar("TJSONValue", bound=JSONValue)

TRW = TypeVar("TRW", bound="ReaderWriterBase")

# Default to using the fastest JSON library for reading, falling back to the
# standard library (always available) if none are installed.
if orjson is not None:
    default_loads = orjson.loads
elif ujson is not None:
    default_loads = ujson.loads
else:
    default_loads = json.loads


# For writing, use the stdlib. Other packages may be faster but their behaviour
# (supported types etc.) and output (whitespace etc.) are not the same as the
# stdlib json module, so this should be opt-in via the ‘dumps=’ arg.
def default_dumps(obj: Any) -> str:
    """
    Fake ``dumps()`` function to use as a default marker.
    """
    raise NotImplementedError  # pragma: no cover


@attr.s(auto_exc=True, auto_attribs=True)
class Error(Exception):
    """
    Base error class.
    """

    message: str


@attr.s(auto_exc=True, auto_attribs=True, init=False)
class InvalidLineError(Error, ValueError):
    """
    Error raised when an invalid line is encountered.

    This happens when the line does not contain valid JSON, or if a
    specific data type has been requested, and the line contained a
    different data type.

    The original line itself is stored on the exception instance as the
    ``.line`` attribute, and the line number as ``.lineno``.

    This class subclasses both ``jsonlines.Error`` and the built-in
    ``ValueError``.
    """

    #: The invalid line
    line: Union[str, bytes]

    #: The line number
    lineno: int

    def __init__(self, message: str, line: Union[str, bytes], lineno: int) -> None:
        self.line = line.rstrip()
        self.lineno = lineno
        super().__init__(f"{message} (line {lineno})")


@attr.s(auto_exc=True, auto_attribs=True)
class BookmarkError(Error):
    """
    Error raised when bookmark handling fails.

    This happens when:

    * a bookmark is requested for a stream that is not binary and
      seekable (e.g. a text stream or a plain iterable),
    * a bookmark is requested while the stream is positioned past the
      last fully delivered record (for example, directly after an
      invalid line was read),
    * a bookmark cannot be restored because its stream does not match
      the recorded file identity (a changed prefix, truncation, or an
      unsupported bookmark version).
    """


@attr.s(auto_attribs=True, frozen=True)
class Bookmark:
    """
    Resumable reading position for a binary, seekable jsonlines stream.

    A bookmark captures the position *after* the last record that has
    been fully and successfully delivered to the caller. Reopening the
    same file and restoring the bookmark makes the reader continue with
    the next record, without re-delivering already processed records.

    Bookmarks only work with binary, seekable file-like objects, since
    only those expose reliable byte offsets.

    The recorded file identity consists of a size and a cryptographic
    digest of the file prefix up to :attr:`offset`. On restore, data may
    have been appended (a larger current size is accepted), but any
    change within the already delivered prefix is rejected with
    :py:class:`BookmarkError`.

    Instances are immutable and can be (de)serialized to plain ``dict``
    or JSON strings for persistent storage.

    :ivar version: bookmark format version
    :ivar offset: byte position after the last delivered record
    :ivar lineno: number of physical lines fully delivered so far
    :ivar size: size of the source file when the bookmark was created
    :ivar digest: hex digest of the file prefix up to ``offset``
    :ivar algorithm: hash algorithm used for :attr:`digest`
    :ivar name: optional, informational name of the source file
    """

    #: Bookmark format version.
    version: int = BOOKMARK_FORMAT_VERSION

    #: Byte offset after the last fully delivered record.
    offset: int = 0

    #: Number of physical lines fully delivered so far.
    lineno: int = 0

    #: Size of the source file at bookmark creation time.
    size: int = 0

    #: Hex digest of the file prefix up to :attr:`offset`.
    digest: str = ""

    #: Hash algorithm used for :attr:`digest`.
    algorithm: str = BOOKMARK_HASH_ALGORITHM

    #: Optional, informational name of the source file.
    name: Optional[str] = None

    def to_dict(self) -> Dict[str, Any]:
        """
        Return the bookmark data as a plain, JSON-serializable dict.
        """
        return attr.asdict(self)

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "Bookmark":
        """
        Create a bookmark from a dict previously produced by
        :py:meth:`to_dict`.

        Unknown fields are ignored so that newer bookmark files remain
        readable by older code, while a missing or unsupported
        :attr:`version` results in :py:class:`BookmarkError`.
        """
        try:
            version = data["version"]
        except (KeyError, TypeError) as orig_exc:
            raise BookmarkError("bookmark is missing a 'version' field") from orig_exc
        if version != BOOKMARK_FORMAT_VERSION:
            raise BookmarkError(
                f"unsupported bookmark version: {version!r} "
                f"(supported: {BOOKMARK_FORMAT_VERSION})"
            )
        fields = {
            field.name: data[field.name]
            for field in attr.fields(cls)
            if field.name in data
        }
        try:
            return cls(**fields)
        except (TypeError, ValueError) as orig_exc:
            raise BookmarkError(f"invalid bookmark data: {orig_exc}") from orig_exc

    def to_json(self) -> str:
        """
        Serialize the bookmark to a JSON string.
        """
        return json.dumps(self.to_dict(), sort_keys=True)

    @classmethod
    def from_json(cls, value: Union[str, bytes]) -> "Bookmark":
        """
        Restore a bookmark from a JSON string produced by
        :py:meth:`to_json`.
        """
        try:
            data = json.loads(value)
        except ValueError as orig_exc:
            raise BookmarkError(f"bookmark is not valid json: {orig_exc}") from orig_exc
        if not isinstance(data, dict):
            raise BookmarkError("bookmark json must contain an object")
        return cls.from_dict(data)


@attr.s(auto_attribs=True, repr=False)
class ReaderWriterBase:
    """
    Base class with shared behaviour for both the reader and writer.
    """

    _fp: Union[typing.IO[str], typing.IO[bytes], None] = attr.ib(
        default=None, init=False
    )
    _closed: bool = attr.ib(default=False, init=False)
    _should_close_fp: bool = attr.ib(default=False, init=False)

    def close(self) -> None:
        """
        Close this reader/writer.

        This closes the underlying file if that file has been opened by
        this reader/writer. When an already opened file-like object was
        provided, the caller is responsible for closing it.
        """
        if self._closed:
            return
        self._closed = True
        if self._fp is not None and self._should_close_fp:
            self._fp.close()

    def __repr__(self) -> str:
        cls_name = type(self).__name__
        wrapped = self._repr_for_wrapped()
        return f"<jsonlines.{cls_name} at 0x{id(self):x} wrapping {wrapped}>"

    def _repr_for_wrapped(self) -> str:
        raise NotImplementedError  # pragma: no cover

    def __enter__(self: TRW) -> TRW:
        return self

    def __exit__(
        self,
        exc_type: Optional[Type[BaseException]],
        exc_val: Optional[BaseException],
        exc_tb: Optional[types.TracebackType],
    ) -> None:
        self.close()


@attr.s(auto_attribs=True, repr=False)
class Reader(ReaderWriterBase):
    """
    Reader for the jsonlines format.

    The first argument must be an iterable that yields JSON encoded
    strings. Usually this will be a readable file-like object, such as
    an open file or an ``io.TextIO`` instance, but it can also be
    something else as long as it yields strings when iterated over.

    Instances are iterable and can be used as a context manager.

    When wrapping a binary, seekable file-like object, a reader can
    create resumable :py:class:`Bookmark` instances via
    :py:meth:`bookmark` and resume from them using
    :py:meth:`restore`. This enables processing large jsonlines streams
    in batches while keeping a durable, verifiable reading position.

    The `loads` argument can be used to replace the standard json
    decoder. If specified, it must be a callable that accepts a
    (unicode) string and returns the decoded object.

    :param file_or_iterable: file-like object or iterable yielding lines as
        strings
    :param loads: custom json decoder callable
    """

    _file_or_iterable: Union[
        typing.IO[str], typing.IO[bytes], Iterable[Union[str, bytes]]
    ]
    _line_iter: Iterator[Tuple[int, Union[bytes, str]]] = attr.ib(init=False)
    _loads: LoadsCallable = attr.ib(default=default_loads, kw_only=True)

    # Bookmark support. This only works for binary, seekable streams, since
    # those expose meaningful byte offsets. All state is updated lazily so
    # that readers which never use bookmarks pay (almost) nothing.
    _bookmark_fp: Optional[typing.IO[bytes]] = attr.ib(default=None, init=False)
    _bookmark_hasher: Optional["hashlib._Hash"] = attr.ib(default=None, init=False)
    # Position after the last *fully delivered* record (committed state).
    _bookmark_offset: int = attr.ib(default=0, init=False)
    _bookmark_lineno: int = attr.ib(default=0, init=False)
    # First physical line number yielded by the line iterator. This is 1 for
    # a fresh reader, or ``bookmark.lineno + 1`` for a restored reader.
    _first_lineno: int = attr.ib(default=1, init=False)
    # Name to embed in bookmarks, purely informational.
    _bookmark_name: Optional[str] = attr.ib(default=None, init=False)

    def __attrs_post_init__(self) -> None:
        if isinstance(self._file_or_iterable, io.IOBase):
            self._fp = cast(
                Union[typing.IO[str], typing.IO[bytes]],
                self._file_or_iterable,
            )
            if (
                isinstance(self._file_or_iterable, (io.RawIOBase, io.BufferedIOBase))
                and self._file_or_iterable.seekable()
            ):
                self._bookmark_fp = cast(
                    typing.IO[bytes], self._file_or_iterable
                )
                self._bookmark_name = repr_for_fp(self._bookmark_fp)
                # If the stream does not start at its beginning, the bytes
                # before the current position were never seen by this reader
                # but still belong to the file prefix. Prime the hasher with
                # them so later bookmarks digest the whole prefix, and treat
                # the starting position as the initial committed offset.
                start_position = self._bookmark_fp.tell()
                if start_position:
                    self._bookmark_offset = start_position
                    self._bookmark_hasher = self._hash_prefix(
                        self._bookmark_fp, start_position
                    )

        self._line_iter = enumerate(self._file_or_iterable, self._first_lineno)

    @staticmethod
    def _hash_prefix(
        fp: typing.IO[bytes], length: int
    ) -> "hashlib._Hash":
        """
        Hash the first ``length`` bytes of ``fp`` and restore its position.
        """
        hasher = hashlib.new(BOOKMARK_HASH_ALGORITHM)
        current_position = fp.tell()
        fp.seek(0)
        try:
            remaining = length
            while remaining:
                chunk = fp.read(min(remaining, 1024 * 1024))
                if not chunk:
                    break
                hasher.update(chunk)
                remaining -= len(chunk)
        finally:
            fp.seek(current_position)
        return hasher

    def _get_hasher(self) -> "hashlib._Hash":
        """
        Return the running hash of the delivered file prefix.

        The hasher is either primed up front (a restored reader, or a
        stream opened at a non-zero offset) or created lazily and fed the
        raw bytes of each line incrementally as records are committed.
        """
        if self._bookmark_hasher is None:
            # Make sure this stream actually supports bookmarks.
            self._require_bookmark_stream()
            self._bookmark_hasher = hashlib.new(BOOKMARK_HASH_ALGORITHM)
        return self._bookmark_hasher

    def _require_bookmark_stream(self) -> typing.IO[bytes]:
        """
        Return the underlying stream if bookmarks are supported.
        """
        if self._bookmark_fp is None:
            raise BookmarkError(
                "bookmarks require a binary, seekable file-like object; "
                "text streams and plain iterables do not provide byte offsets"
            )
        return self._bookmark_fp

    def _stream_size(self, fp: typing.IO[bytes]) -> int:
        """
        Return the current size of the underlying stream.

        This temporarily seeks to the end and restores the position, so it
        does not disturb ongoing iteration.
        """
        current_position = fp.tell()
        try:
            return fp.seek(0, io.SEEK_END)
        finally:
            fp.seek(current_position)

    def bookmark(self) -> Bookmark:
        """
        Return a :py:class:`Bookmark` for the current reading position.

        The bookmark points at the byte (and line) directly *after* the
        last record that was fully and successfully delivered. It can be
        persisted and later passed to :py:meth:`restore` to continue
        reading without re-processing delivered records.

        A bookmark is only available for a binary, seekable stream. It is
        an error to request one while the stream is positioned past the
        last delivered record, e.g. directly after an
        :py:exc:`InvalidLineError`, since a bookmark must never point at a
        half UTF-8 character or an undelivered (bad) line. Continue
        reading to get past the offending line, or use the previously
        returned bookmark instead.

        :raises BookmarkError: if bookmarks are not supported, or if the
            stream is not positioned at a safe record boundary
        """
        fp = self._require_bookmark_stream()
        if self._closed:
            raise RuntimeError("reader is closed")
        if fp.tell() != self._bookmark_offset:
            raise BookmarkError(
                "stream is positioned past the last fully delivered record; "
                "a bookmark can only be created on a record boundary, not on "
                "a partial UTF-8 character or an undelivered (bad) line"
            )
        digest = self._get_hasher().hexdigest()
        return Bookmark(
            offset=self._bookmark_offset,
            lineno=self._bookmark_lineno,
            size=self._stream_size(fp),
            digest=digest,
            algorithm=BOOKMARK_HASH_ALGORITHM,
            name=self._bookmark_name,
        )

    @classmethod
    def restore(
        cls,
        file_or_iterable: Union[typing.IO[bytes], "os.PathLike[str]", str, int],
        bookmark: Bookmark,
        *,
        loads: Optional[LoadsCallable] = None,
    ) -> "Reader":
        """
        Reopen a jsonlines stream and continue reading at a bookmark.

        The given stream must be binary and seekable. Its already
        delivered prefix is verified against the bookmark's recorded file
        identity before anything is returned:

        * the current size must be at least the recorded offset,
        * the digest of the file prefix must match the bookmark digest.

        Data appended after the bookmark is accepted, but any change to
        the already delivered prefix (or truncation before the offset) is
        rejected with :py:class:`BookmarkError`.

        On success the stream is positioned exactly at
        :attr:`Bookmark.offset`, so the next :py:meth:`read` yields the
        first not-yet-delivered record. This method never re-delivers
        already committed records.

        The caller retains ownership of the stream: it is not closed by
        the reader, and the caller remains responsible for closing it.

        :param file_or_iterable: a binary, seekable file-like object, or
            a path / file descriptor to open in binary mode
        :param bookmark: the bookmark to restore
        :param loads: optional custom json decoder callable
        """
        opened_here = not isinstance(file_or_iterable, io.IOBase)
        fp: typing.IO[bytes]
        if opened_here:
            fp = builtins.open(file_or_iterable, mode="rb")  # type: ignore[arg-type]
        else:
            fp = cast(typing.IO[bytes], file_or_iterable)
            if not isinstance(fp, (io.RawIOBase, io.BufferedIOBase)):
                raise BookmarkError(
                    "restoring a bookmark requires a binary, seekable "
                    "file-like object"
                )

        if not fp.seekable():
            raise BookmarkError("restoring a bookmark requires a seekable stream")

        try:
            current_size = fp.seek(0, io.SEEK_END)
            if current_size < bookmark.offset:
                raise BookmarkError(
                    f"stream was truncated: size {current_size} is smaller than "
                    f"bookmark offset {bookmark.offset}"
                )
            if bookmark.algorithm != BOOKMARK_HASH_ALGORITHM:
                raise BookmarkError(
                    f"unsupported bookmark hash algorithm: "
                    f"{bookmark.algorithm!r} (supported: {BOOKMARK_HASH_ALGORITHM})"
                )

            # Verify the immutable, already delivered prefix.
            hasher = cls._hash_prefix(fp, bookmark.offset)
            if hasher.hexdigest() != bookmark.digest:
                raise BookmarkError(
                    "file prefix does not match bookmark: the already "
                    "delivered data has changed"
                )

            # Position exactly on the record boundary. This is guaranteed to
            # start a complete UTF-8 sequence, since the bookmark only points
            # at record boundaries. The Reader constructor will prime its own
            # hasher with this same verified prefix.
            fp.seek(bookmark.offset)
        except BaseException:
            if opened_here:
                fp.close()
            raise

        kwargs: Dict[str, Any] = {}
        if loads is not None:
            kwargs["loads"] = loads
        reader = cls(fp, **kwargs)
        if opened_here:
            # Keep stream ownership explicit: only close what we opened.
            reader._should_close_fp = True
        reader._bookmark_offset = bookmark.offset
        reader._bookmark_lineno = bookmark.lineno
        reader._first_lineno = bookmark.lineno + 1
        # The line iterator was created in __attrs_post_init__ starting at
        # line 1; rebuild it so line numbering continues past the prefix.
        reader._line_iter = enumerate(fp, reader._first_lineno)
        return reader

    # No type specified, None not allowed
    @overload
    def read(
        self,
        *,
        type: Literal[None] = ...,
        allow_none: Literal[False] = ...,
        skip_empty: bool = ...,
    ) -> JSONValue:
        ...  # pragma: no cover

    # No type specified, None allowed
    @overload
    def read(
        self,
        *,
        type: Literal[None] = ...,
        allow_none: Literal[True],
        skip_empty: bool = ...,
    ) -> Optional[JSONValue]:
        ...  # pragma: no cover

    # Type specified, None not allowed
    @overload
    def read(
        self,
        *,
        type: Type[TJSONValue],
        allow_none: Literal[False] = ...,
        skip_empty: bool = ...,
    ) -> TJSONValue:
        ...  # pragma: no cover

    # Type specified, None allowed
    @overload
    def read(
        self,
        *,
        type: Type[TJSONValue],
        allow_none: Literal[True],
        skip_empty: bool = ...,
    ) -> Optional[TJSONValue]:
        ...  # pragma: no cover

    # Generic definition
    @overload
    def read(
        self,
        *,
        type: Optional[Type[Any]] = ...,
        allow_none: bool = ...,
        skip_empty: bool = ...,
    ) -> Optional[JSONValue]:
        ...  # pragma: no cover

    def read(
        self,
        *,
        type: Optional[Type[Any]] = None,
        allow_none: bool = False,
        skip_empty: bool = False,
    ) -> Optional[JSONValue]:
        """
        Read and decode a line.

        The optional `type` argument specifies the expected data type.
        Supported types are ``dict``, ``list``, ``str``, ``int``,
        ``float``, and ``bool``. When specified, non-conforming lines
        result in :py:exc:`InvalidLineError`.

        By default, input lines containing ``null`` (in JSON) are
        considered invalid, and will cause :py:exc:`InvalidLineError`.
        The `allow_none` argument can be used to change this behaviour,
        in which case ``None`` will be returned instead.

        If `skip_empty` is set to ``True``, empty lines and lines
        containing only whitespace are silently skipped.
        """
        if self._closed:
            raise RuntimeError("reader is closed")
        if type is not None and type not in VALID_TYPES:
            raise ValueError("invalid type specified")

        # When bookmarking is possible, remember the raw bytes of every
        # physical line consumed by this call. Empty lines skipped via
        # 'skip_empty' are part of the stream and therefore included. The
        # state is only *committed* below, once the record has been fully
        # decoded and validated, so a failure never advances the bookmark.
        raw_lines: List[bytes] = []
        track_bytes = self._bookmark_fp is not None
        try:
            lineno, line = next(self._line_iter)
            if track_bytes and isinstance(line, bytes):
                raw_lines.append(line)
            while skip_empty and not line.rstrip():
                lineno, line = next(self._line_iter)
                if track_bytes and isinstance(line, bytes):
                    raw_lines.append(line)
        except StopIteration:
            raise EOFError from None
        if isinstance(line, bytes):
            try:
                line = line.decode("utf-8")
            except UnicodeDecodeError as orig_exc:
                exc = InvalidLineError(
                    f"line is not valid utf-8: {orig_exc}", line, lineno
                )
                raise exc from orig_exc

        if line.startswith(SKIPPABLE_SINGLE_INITIAL_CHARS):
            line = line[1:]

        try:
            value: JSONValue = self._loads(line)
        except ValueError as orig_exc:
            exc = InvalidLineError(
                f"line contains invalid json: {orig_exc}", line, lineno
            )
            raise exc from orig_exc

        if value is None:
            if allow_none:
                self._commit_bookmark(raw_lines, lineno)
                return None
            raise InvalidLineError("line contains null value", line, lineno)

        if type is not None:
            valid = isinstance(value, type)
            if type is int and isinstance(value, bool):
                # isinstance() is not sufficient, since bool is an int subclass
                valid = False
            if not valid:
                raise InvalidLineError(
                    "line does not match requested type", line, lineno
                )

        self._commit_bookmark(raw_lines, lineno)
        return value

    def _commit_bookmark(
        self, raw_lines: List[bytes], lineno: int
    ) -> None:
        """
        Advance the committed bookmark position after a delivered record.

        This is only called once a line has been read completely and
        decoded successfully, so the recorded offset always points at the
        boundary *after* a full record (and a full UTF-8 sequence), never
        at a half character or an undelivered (bad) line.
        """
        if self._bookmark_fp is None:
            return
        hasher = self._get_hasher()
        for raw_line in raw_lines:
            hasher.update(raw_line)
        self._bookmark_offset = self._bookmark_fp.tell()
        self._bookmark_lineno = lineno

    # No type specified, None not allowed
    @overload
    def iter(
        self,
        *,
        type: Literal[None] = ...,
        allow_none: Literal[False] = ...,
        skip_empty: bool = ...,
        skip_invalid: bool = ...,
    ) -> Iterator[JSONValue]:
        ...  # pragma: no cover

    # No type specified, None allowed
    @overload
    def iter(
        self,
        *,
        type: Literal[None] = ...,
        allow_none: Literal[True],
        skip_empty: bool = ...,
        skip_invalid: bool = ...,
    ) -> Iterator[JSONValue]:
        ...  # pragma: no cover

    # Type specified, None not allowed
    @overload
    def iter(
        self,
        *,
        type: Type[TJSONValue],
        allow_none: Literal[False] = ...,
        skip_empty: bool = ...,
        skip_invalid: bool = ...,
    ) -> Iterator[TJSONValue]:
        ...  # pragma: no cover

    # Type specified, None allowed
    @overload
    def iter(
        self,
        *,
        type: Type[TJSONValue],
        allow_none: Literal[True],
        skip_empty: bool = ...,
        skip_invalid: bool = ...,
    ) -> Iterator[Optional[TJSONValue]]:
        ...  # pragma: no cover

    # Generic definition
    @overload
    def iter(
        self,
        *,
        type: Optional[Type[TJSONValue]] = ...,
        allow_none: bool = ...,
        skip_empty: bool = ...,
        skip_invalid: bool = ...,
    ) -> Iterator[Optional[TJSONValue]]:
        ...  # pragma: no cover

    def iter(
        self,
        type: Optional[Type[Any]] = None,
        allow_none: bool = False,
        skip_empty: bool = False,
        skip_invalid: bool = False,
    ) -> Iterator[Optional[JSONValue]]:
        """
        Iterate over all lines.

        This is the iterator equivalent to repeatedly calling
        :py:meth:`~Reader.read()`. If no arguments are specified, this
        is the same as directly iterating over this :py:class:`Reader`
        instance.

        When `skip_invalid` is set to ``True``, invalid lines will be
        silently ignored.

        See :py:meth:`~Reader.read()` for a description of the other
        arguments.
        """
        try:
            while True:
                try:
                    yield self.read(
                        type=type, allow_none=allow_none, skip_empty=skip_empty
                    )
                except InvalidLineError:
                    if not skip_invalid:
                        raise
        except EOFError:
            pass

    def __iter__(self) -> Iterator[Any]:
        """
        See :py:meth:`~Reader.iter()`.
        """
        return self.iter()

    def _repr_for_wrapped(self) -> str:
        if self._fp is not None:
            return repr_for_fp(self._fp)
        class_name = type(self._file_or_iterable).__name__
        return f"<{class_name} at 0x{id(self._file_or_iterable):x}>"


@attr.s(auto_attribs=True, repr=False)
class Writer(ReaderWriterBase):
    """
    Writer for the jsonlines format.

    Instances can be used as a context manager.

    The `fp` argument must be a file-like object with a ``.write()``
    method accepting either text (unicode) or bytes.

    The `compact` argument can be used to to produce smaller output.

    The `sort_keys` argument can be used to sort keys in json objects,
    and will produce deterministic output.

    For more control, provide a a custom encoder callable using the
    `dumps` argument. The callable must produce (unicode) string output.
    If specified, the `compact` and `sort` arguments will be ignored.

    When the `flush` argument is set to ``True``, the writer will call
    ``fp.flush()`` after each written line.

    :param fp: writable file-like object
    :param compact: whether to use a compact output format
    :param sort_keys: whether to sort object keys
    :param dumps: custom encoder callable
    :param flush: whether to flush the file-like object after writing each line
    """

    _fp: Union[typing.IO[str], typing.IO[bytes]] = attr.ib(default=None)
    _fp_is_binary: bool = attr.ib(default=False, init=False)
    _compact: bool = attr.ib(default=False, kw_only=True)
    _sort_keys: bool = attr.ib(default=False, kw_only=True)
    _flush: bool = attr.ib(default=False, kw_only=True)
    _dumps: DumpsCallable = attr.ib(default=default_dumps, kw_only=True)
    _dumps_result_conversion: DumpsResultConversion = attr.ib(
        default=DumpsResultConversion.LeaveAsIs, init=False
    )

    def __attrs_post_init__(self) -> None:
        if isinstance(self._fp, io.TextIOBase):
            self._fp_is_binary = False
        elif isinstance(self._fp, io.IOBase):
            self._fp_is_binary = True
        else:
            try:
                self._fp.write("")  # type: ignore[call-overload]
            except TypeError:
                self._fp_is_binary = True
            else:
                self._fp_is_binary = False

        if self._dumps is default_dumps:
            self._dumps = json.JSONEncoder(
                ensure_ascii=False,
                separators=(",", ":") if self._compact else (", ", ": "),
                sort_keys=self._sort_keys,
            ).encode

        # Detect if str-to-bytes conversion (or vice versa) is needed for the
        # combination of this file-like object and the used dumps() callable.
        # This avoids checking this for each .write(). Note that this
        # deliberately does not support ‘dynamic’ return types that depend on
        # input and dump options, like simplejson on Python 2 in some cases.
        sample_dumps_result = self._dumps({})
        if isinstance(sample_dumps_result, str) and self._fp_is_binary:
            self._dumps_result_conversion = DumpsResultConversion.EncodeToBytes
        elif isinstance(sample_dumps_result, bytes) and not self._fp_is_binary:
            self._dumps_result_conversion = DumpsResultConversion.DecodeToString

    def write(self, obj: Any) -> int:
        """
        Encode and write a single object.

        :param obj: the object to encode and write
        :return: number of characters or bytes written
        """
        if self._closed:
            raise RuntimeError("writer is closed")

        line = self._dumps(obj)

        # This handles either str or bytes, but the type checker does not know
        # that this code always passes the right type of arguments.
        if self._dumps_result_conversion == DumpsResultConversion.EncodeToBytes:
            line = line.encode()  # type: ignore[union-attr]
        elif self._dumps_result_conversion == DumpsResultConversion.DecodeToString:
            line = line.decode()  # type: ignore[union-attr]

        fp = self._fp
        fp.write(line)  # type: ignore[arg-type]
        fp.write(b"\n" if self._fp_is_binary else "\n")  # type: ignore[call-overload]

        if self._flush:
            fp.flush()

        return len(line) + 1  # including newline

    def write_all(self, iterable: Iterable[Any]) -> int:
        """
        Encode and write multiple objects.

        :param iterable: an iterable of objects
        :return: number of characters or bytes written
        """
        return sum(self.write(obj) for obj in iterable)

    def _repr_for_wrapped(self) -> str:
        return repr_for_fp(self._fp)


@overload
def open(
    file: Openable,
    mode: Literal["r"] = ...,
    *,
    loads: Optional[LoadsCallable] = ...,
) -> Reader:
    ...  # pragma: no cover


@overload
def open(
    file: Openable,
    mode: Literal["w", "a", "x"],
    *,
    dumps: Optional[DumpsCallable] = ...,
    compact: Optional[bool] = ...,
    sort_keys: Optional[bool] = ...,
    flush: Optional[bool] = ...,
) -> Writer:
    ...  # pragma: no cover


@overload
def open(
    file: Openable,
    mode: str = ...,
    *,
    loads: Optional[LoadsCallable] = ...,
    dumps: Optional[DumpsCallable] = ...,
    compact: Optional[bool] = ...,
    sort_keys: Optional[bool] = ...,
    flush: Optional[bool] = ...,
) -> Union[Reader, Writer]:
    ...  # pragma: no cover


def open(
    file: Openable,
    mode: str = "r",
    *,
    loads: Optional[LoadsCallable] = None,
    dumps: Optional[DumpsCallable] = None,
    compact: Optional[bool] = None,
    sort_keys: Optional[bool] = None,
    flush: Optional[bool] = None,
) -> Union[Reader, Writer]:
    """
    Open a jsonlines file for reading or writing.

    This is a convenience function to open a file and wrap it in either a
    :py:class:`Reader` or :py:class:`Writer` instance, depending on the
    specified `mode`.

    Additional keyword arguments will be passed on to the reader and writer;
    see their documentation for available options.

    The resulting reader or writer must be closed after use by the
    caller, which will also close the opened file.  This can be done by
    calling ``.close()``, but the easiest way to ensure proper resource
    finalisation is to use a ``with`` block (context manager), e.g.

    ::

        with jsonlines.open('out.jsonl', mode='w') as writer:
            writer.write(...)

    :param file: name or ‘path-like object’ of the file to open
    :param mode: whether to open the file for reading (``r``),
        writing (``w``), appending (``a``), or exclusive creation (``x``).
    """
    if mode not in {"r", "w", "a", "x"}:
        raise ValueError("'mode' must be either 'r', 'w', 'a', or 'x'")

    cls = Reader if mode == "r" else Writer
    encoding = "utf-8-sig" if mode == "r" else "utf-8"
    fp = builtins.open(file, mode=mode + "t", encoding=encoding)
    kwargs = dict(
        loads=loads,
        dumps=dumps,
        compact=compact,
        sort_keys=sort_keys,
        flush=flush,
    )
    kwargs = {key: value for key, value in kwargs.items() if value is not None}
    instance: Union[Reader, Writer] = cls(fp, **kwargs)
    instance._should_close_fp = True
    return instance


def repr_for_fp(fp: typing.IO[Any]) -> str:
    """
    Helper to make a useful repr() for a file-like object.
    """
    name = getattr(fp, "name", None)
    if name is not None:
        return repr(name)
    else:
        return repr(fp)
