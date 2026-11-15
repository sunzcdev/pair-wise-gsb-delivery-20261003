"""
jsonlines implementation
"""

import builtins
import codecs
import enum
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

# RFC 7464 record separator (RS) and UTF-8 byte order mark (BOM).
RECORD_SEPARATOR = "\x1e"
BOM = codecs.BOM_UTF8.decode()

# Characters to skip at the beginning of a line. Note: at most one such
# character is skipped per line. This is only used by the backwards
# compatible (lenient) parsing mode; see the Format enum.
SKIPPABLE_SINGLE_INITIAL_CHARS = (
    RECORD_SEPARATOR,  # RFC7464 text sequence
    BOM,
)


class Format(enum.Enum):
    """
    Explicit input format handled by a :py:class:`Reader`.

    The two members distinguish the two line-oriented JSON formats that
    this library can consume:

    * :py:attr:`JSON_LINES` implements the JSON Lines / ndjson format:
      exactly one JSON value per physical line, with no record
      separators. A leading UTF-8 BOM is only permitted at the very
      start of the stream.

    * :py:attr:`RFC7464_TEXT_SEQUENCES` implements the
      :rfc:`7464` JavaScript Object Notation (JSON) Text Sequences
      format: records start with an ASCII record separator (RS, ``\\x1e``)
      and end with a line feed (LF). A record may span multiple physical
      lines, and empty records (two consecutive RS characters) are not
      valid JSON.

    When no explicit format is selected (the default), the reader uses
    the historical, lenient behaviour for backwards compatibility: it
    transparently skips a single leading BOM or RS character on each
    line and accepts both kinds of input. It never silently changes
    interpretation based on the contents of the stream.
    """

    #: JSON Lines / ndjson: one JSON value per physical line.
    JSON_LINES = enum.auto()

    #: RFC 7464 JSON Text Sequences: RS-delimited, LF-terminated records.
    RFC7464_TEXT_SEQUENCES = enum.auto()


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


class _Record(typing.NamedTuple):
    """
    A single decoded-text record produced by the reader's format logic.

    ``text`` is the payload handed to the JSON decoder (without record
    separator or BOM framing). ``lineno`` and ``column`` locate the start
    of the record (one-based), while ``stream_position`` is the
    zero-based offset of that start in the decoded stream, or ``None``
    when the format does not track it.
    """

    text: str
    lineno: int
    column: int
    stream_position: Optional[int]


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
    different data type. It is also raised when an explicit
    :py:class:`Format` is violated, e.g. when a record separator turns
    up in a JSON Lines stream.

    The original line itself is stored on the exception instance as the
    ``.line`` attribute, and the line number as ``.lineno``. For errors
    detected at a specific character position (such as mixed formats),
    the one-based column and the zero-based offset in the decoded
    stream are available as ``.column`` and ``.position`` respectively;
    they are ``None`` when not applicable.

    This class subclasses both ``jsonlines.Error`` and the built-in
    ``ValueError``.
    """

    #: The invalid line
    line: Union[str, bytes]

    #: The line number
    lineno: int

    #: The one-based column within the line, if known
    column: Optional[int]

    #: The zero-based character offset in the decoded stream, if known
    position: Optional[int]

    def __init__(
        self,
        message: str,
        line: Union[str, bytes],
        lineno: int,
        column: Optional[int] = None,
        position: Optional[int] = None,
    ) -> None:
        self.line = line.rstrip()
        self.lineno = lineno
        self.column = column
        self.position = position
        location = f"line {lineno}"
        if column is not None:
            location = f"{location}, column {column}"
        super().__init__(f"{message} ({location})")


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

    The `loads` argument can be used to replace the standard json
    decoder. If specified, it must be a callable that accepts a
    (unicode) string and returns the decoded object.

    The `format` argument selects an explicit, strict input format. Use
    :py:attr:`Format.JSON_LINES` for JSON Lines / ndjson (one JSON
    value per physical line) or
    :py:attr:`Format.RFC7464_TEXT_SEQUENCES` for :rfc:`7464` JSON Text
    Sequences (RS-delimited, LF-terminated records that may span
    multiple lines). When omitted (the default), the historical lenient
    behaviour is used, which transparently tolerates a single leading
    BOM or record separator per line for backwards compatibility.
    Lenient parsing never silently switches between formats.

    :param file_or_iterable: file-like object or iterable yielding lines as
        strings
    :param loads: custom json decoder callable
    :param format: explicit, strict :py:class:`Format` to parse, or
        ``None`` (the default) for the backwards compatible behaviour
    """

    _file_or_iterable: Union[
        typing.IO[str], typing.IO[bytes], Iterable[Union[str, bytes]]
    ]
    _line_iter: Iterator[Tuple[int, Union[bytes, str]]] = attr.ib(init=False)
    _record_iter: Iterator["_Record"] = attr.ib(init=False)
    _loads: LoadsCallable = attr.ib(default=default_loads, kw_only=True)
    _format: Optional[Format] = attr.ib(default=None, kw_only=True)

    def __attrs_post_init__(self) -> None:
        if isinstance(self._file_or_iterable, io.IOBase):
            self._fp = cast(
                Union[typing.IO[str], typing.IO[bytes]],
                self._file_or_iterable,
            )

        self._line_iter = enumerate(self._file_or_iterable, 1)

        if self._format is None:
            self._record_iter = self._iter_lenient_records()
        elif self._format is Format.JSON_LINES:
            self._record_iter = self._iter_json_lines_records()
        elif self._format is Format.RFC7464_TEXT_SEQUENCES:
            self._record_iter = self._iter_rfc7464_records()
        else:  # pragma: no cover - all enum members are handled above
            raise ValueError(f"invalid format specified: {self._format!r}")

    def _iter_lines(self) -> Iterator[Tuple[int, str]]:
        """
        Yield ``(lineno, line)`` pairs, decoding bytes lines to text.

        The decoded text keeps its trailing line ending, which matters
        for formats (RFC 7464) where records may span physical lines.
        UTF-8 decoding failures are reported as :py:class:`InvalidLineError`.
        """
        for lineno, raw_line in self._line_iter:
            if isinstance(raw_line, bytes):
                try:
                    line = raw_line.decode("utf-8")
                except UnicodeDecodeError as orig_exc:
                    exc = InvalidLineError(
                        f"line is not valid utf-8: {orig_exc}",
                        raw_line,
                        lineno,
                    )
                    raise exc from orig_exc
            else:
                line = raw_line
            yield lineno, line

    def _iter_lenient_records(self) -> Iterator["_Record"]:
        """
        Backwards compatible record handling.

        Every physical line is a record. At most one leading BOM or RS
        character is stripped per line; anything inside a JSON string
        is left untouched, since only the very first character is
        inspected. Empty physical lines are yielded as-is so that the
        ``skip_empty`` option keeps working.
        """
        for lineno, line in self._iter_lines():
            text = line
            if text.startswith(SKIPPABLE_SINGLE_INITIAL_CHARS):
                text = text[1:]
            yield _Record(
                text=text,
                lineno=lineno,
                column=1,
                stream_position=None,
            )

    def _iter_json_lines_records(self) -> Iterator["_Record"]:
        """
        Strict JSON Lines / ndjson record handling.

        Each physical line must contain exactly one JSON value. A UTF-8
        BOM is only accepted at the very start of the stream. Record
        separator (RS) characters are never valid, and their exact
        position is reported when encountered.
        """
        stream_position = 0
        bom_seen = False
        for lineno, line in self._iter_lines():
            text = line
            column = 1

            # A BOM is only valid as the first character of the stream.
            if text.startswith(BOM):
                if stream_position != 0:
                    raise InvalidLineError(
                        "unexpected UTF-8 BOM outside the start of the stream",
                        text,
                        lineno,
                        column=column,
                        position=stream_position,
                    )
                bom_seen = True
                text = text[1:]
                column += 1

            # Record separators are not part of the JSON Lines format,
            # neither between records nor inside JSON strings; the JSON
            # decoder rejects the latter, but a structural RS must be
            # reported with its position rather than stripped away.
            separator_offset = text.find(RECORD_SEPARATOR)
            if separator_offset != -1:
                bom_offset = 1 if bom_seen else 0
                raise InvalidLineError(
                    "record separator (RS) is not valid in JSON Lines input; "
                    "use Format.RFC7464_TEXT_SEQUENCES for RFC 7464 data",
                    text,
                    lineno,
                    column=column + separator_offset,
                    position=stream_position + separator_offset + bom_offset,
                )

            yield _Record(
                text=text,
                lineno=lineno,
                column=column,
                stream_position=stream_position,
            )
            stream_position += len(line)

    def _iter_rfc7464_records(self) -> Iterator["_Record"]:
        """
        Strict RFC 7464 JSON Text Sequences record handling.

        Every record starts with an RS character and ends with an LF.
        The JSON payload between them may span multiple physical lines.
        RS and BOM characters that occur inside a JSON string are part
        of the payload and are preserved for the JSON decoder rather
        than being mistaken for framing characters.
        """
        stream_position = 0
        pending: List[str] = []
        record_start_line = 0
        record_start_column = 0
        record_start_position = 0
        at_stream_start = True
        expecting_record_separator = True

        # Scanner state used to keep characters inside JSON strings out
        # of the framing logic. ``in_string`` tracks whether the current
        # position is inside a JSON string, and ``escaped`` whether the
        # previous character was an unescaped backslash.
        in_string = False
        escaped = False

        for lineno, line in self._iter_lines():
            index = 0
            while index < len(line):
                char: str = line[index]
                column = index + 1
                absolute_position = stream_position + index

                if expecting_record_separator:
                    if char == RECORD_SEPARATOR:
                        expecting_record_separator = False
                        in_string = False
                        escaped = False
                        record_start_line = lineno
                        record_start_column = column + 1
                        record_start_position = absolute_position + 1
                        at_stream_start = False
                        index += 1
                        continue
                    if at_stream_start and char == BOM:
                        # A single BOM is permitted before the first record.
                        at_stream_start = False
                        index += 1
                        continue
                    if char in (" ", "\t", "\r", "\n"):
                        # Tolerate insignificant whitespace between records,
                        # including the trailing LF of the previous record.
                        index += 1
                        continue
                    raise InvalidLineError(
                        "expected an RFC 7464 record separator (RS) at the "
                        "start of a record",
                        line,
                        lineno,
                        column=column,
                        position=absolute_position,
                    )

                if char == RECORD_SEPARATOR and not in_string:
                    # A structural RS terminates this record and starts
                    # the next one. An empty payload (two consecutive RS
                    # characters) is yielded as an empty record; by
                    # default it fails JSON decoding, while
                    # ``skip_empty=True`` can be used to skip it.
                    yield _Record(
                        text="".join(pending),
                        lineno=record_start_line,
                        column=record_start_column,
                        stream_position=record_start_position,
                    )
                    pending = []
                    # Re-process this RS as the start of the next record.
                    expecting_record_separator = True
                    continue

                if not in_string and char == BOM and pending:
                    raise InvalidLineError(
                        "unexpected UTF-8 BOM inside an RFC 7464 record",
                        line,
                        lineno,
                        column=column,
                        position=absolute_position,
                    )

                pending.append(char)

                # Update JSON string state so that framing characters
                # inside a string are left untouched.
                if in_string:
                    if escaped:
                        escaped = False
                    elif char == "\\":
                        escaped = True
                    elif char == '"':
                        in_string = False
                elif char == '"':
                    in_string = True

                index += 1

            stream_position += len(line)

        if pending or not expecting_record_separator:
            # Flush the final record. Per RFC 7464 it should end with an
            # LF; a trailing RS produces an empty final record, and a
            # truncated payload is handed to the JSON decoder so that the
            # underlying syntax error is reported.
            yield _Record(
                text="".join(pending),
                lineno=record_start_line,
                column=record_start_column,
                stream_position=record_start_position,
            )

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

        try:
            record = next(self._record_iter)
            while skip_empty and not record.text.rstrip():
                record = next(self._record_iter)
        except StopIteration:
            raise EOFError from None

        line = record.text

        try:
            value: JSONValue = self._loads(line)
        except ValueError as orig_exc:
            exc = InvalidLineError(
                f"line contains invalid json: {orig_exc}",
                line,
                record.lineno,
                column=record.column,
                position=record.stream_position,
            )
            raise exc from orig_exc

        if value is None:
            if allow_none:
                return None
            raise InvalidLineError(
                "line contains null value",
                line,
                record.lineno,
                column=record.column,
                position=record.stream_position,
            )

        if type is not None:
            valid = isinstance(value, type)
            if type is int and isinstance(value, bool):
                # isinstance() is not sufficient, since bool is an int subclass
                valid = False
            if not valid:
                raise InvalidLineError(
                    "line does not match requested type",
                    line,
                    record.lineno,
                    column=record.column,
                    position=record.stream_position,
                )

        return value

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
    format: Optional[Format] = ...,
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
    format: Optional[Format] = ...,
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
    format: Optional[Format] = None,
) -> Union[Reader, Writer]:
    """
    Open a jsonlines file for reading or writing.

    This is a convenience function to open a file and wrap it in either a
    :py:class:`Reader` or :py:class:`Writer` instance, depending on the
    specified `mode`.

    When reading, the optional `format` argument selects an explicit,
    strict :py:class:`Format`. When omitted, the reader uses the
    backwards compatible lenient behaviour and the file is opened with
    an encoding that transparently consumes a leading UTF-8 BOM.

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
    :param format: explicit, strict :py:class:`Format` to parse when
        reading, or ``None`` for the backwards compatible behaviour
    """
    if mode not in {"r", "w", "a", "x"}:
        raise ValueError("'mode' must be either 'r', 'w', 'a', or 'x'")

    cls = Reader if mode == "r" else Writer
    # With an explicit format the reader itself enforces BOM placement,
    # so the codec must not strip it; the lenient default keeps the
    # historical utf-8-sig behaviour.
    if mode == "r" and format is None:
        encoding = "utf-8-sig"
    else:
        encoding = "utf-8"
    fp = builtins.open(file, mode=mode + "t", encoding=encoding)
    kwargs = dict(
        loads=loads,
        dumps=dumps,
        compact=compact,
        sort_keys=sort_keys,
        flush=flush,
        format=format,
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
