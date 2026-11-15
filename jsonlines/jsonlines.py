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

# Characters to skip at the beginning of a line. Note: at most one such
# character is skipped per line.
SKIPPABLE_SINGLE_INITIAL_CHARS = (
    "\x1e",  # RFC7464 text sequence
    codecs.BOM_UTF8.decode(),
)

# Maximum number of characters of the raw line that are included in the
# ``preview`` of a RejectedLine. Keeping the preview bounded means observing
# rejected lines never requires holding arbitrarily large lines in memory.
REJECTION_PREVIEW_LIMIT = 200


class RejectionReason(str, enum.Enum):
    """
    Reason why a line was rejected by the reader.

    Instances are attached to :py:class:`RejectedLine` objects passed to
    rejection observers, and used as keys in
    :py:attr:`RejectionReport.counts`. Values are plain strings, so they
    can be logged or serialized to JSON directly.
    """

    #: the line is not valid UTF-8
    DECODE = "decode"

    #: the line does not contain valid JSON
    SYNTAX = "syntax"

    #: the decoded value does not match the requested type
    TYPE = "type"

    #: the line contains a JSON null value, which is not allowed
    NULL = "null"


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


def _make_line_preview(line: Union[str, bytes]) -> str:
    """
    Make a bounded, human readable preview of a raw line.
    """
    if isinstance(line, bytes):
        # Undecodable bytes must not prevent rejection observation, so
        # replace them instead of raising another exception.
        text = line.decode("utf-8", errors="backslashreplace")
    else:
        text = line
    text = text.rstrip()
    if len(text) > REJECTION_PREVIEW_LIMIT:
        return text[:REJECTION_PREVIEW_LIMIT] + "…"
    return text


@attr.s(auto_exc=True, auto_attribs=True, init=False)
class InvalidLineError(Error, ValueError):
    """
    Error raised when an invalid line is encountered.

    This happens when the line does not contain valid JSON, or if a
    specific data type has been requested, and the line contained a
    different data type.

    The original line itself is stored on the exception instance as the
    ``.line`` attribute, and the line number as ``.lineno``. The
    ``.reason`` attribute contains a :py:class:`RejectionReason` value
    (or ``None`` for errors not raised by line decoding), and
    ``.preview`` contains a truncated version of the original line.

    This class subclasses both ``jsonlines.Error`` and the built-in
    ``ValueError``.
    """

    #: The invalid line
    line: Union[str, bytes]

    #: The line number
    lineno: int

    #: The reason the line was rejected, if known
    reason: Optional[RejectionReason] = None

    def __init__(
        self,
        message: str,
        line: Union[str, bytes],
        lineno: int,
        reason: Optional[RejectionReason] = None,
    ) -> None:
        self.line = line.rstrip()
        self.lineno = lineno
        self.reason = reason
        super().__init__(f"{message} (line {lineno})")

    @property
    def preview(self) -> str:
        """
        Truncated preview of the original line.
        """
        return _make_line_preview(self.line)


@attr.s(auto_attribs=True, frozen=True)
class RejectedLine:
    """
    Description of a single line that was rejected during iteration.

    Instances are passed to the ``on_reject`` callback of
    :py:meth:`Reader.iter()`.

    The payload is deliberately small and bounded: :py:attr:`preview`
    is a truncated version of the raw line (at most
    :py:data:`REJECTION_PREVIEW_LIMIT` characters plus an ellipsis), so
    observers never receive whole lines and cannot accidentally retain
    unbounded memory per rejected line.
    """

    #: Line number, starting at 1
    lineno: int

    #: Why the line was rejected
    reason: RejectionReason

    #: Truncated preview of the raw line
    preview: str


@attr.s(auto_attribs=True)
class RejectionReport:
    """
    Counters for rejected lines observed during iteration.

    A report is created for every call to :py:meth:`Reader.iter()` and
    available as :py:attr:`Reader.rejection_report` afterwards. Since
    instances are callable, one can also pass an externally created
    report directly as the ``on_reject`` callback::

        report = jsonlines.RejectionReport()
        for obj in reader.iter(skip_invalid=True, on_reject=report):
            ...

    Only aggregate counts are kept; rejected lines themselves are not
    stored, so memory use does not grow with the number of rejections.
    """

    #: Number of rejected lines per reason
    counts: Dict[RejectionReason, int] = attr.ib(factory=dict)

    #: Total number of rejected lines
    total: int = 0

    #: Current number of consecutive rejected lines
    consecutive: int = 0

    #: Longest run of consecutive rejected lines seen so far
    max_consecutive: int = 0

    #: Configured absolute rejection limit, if any
    max_rejects: Optional[int] = None

    #: Configured consecutive rejection limit, if any
    max_consecutive_rejects: Optional[int] = None

    #: Whether iteration ended before reaching the end of the input
    incomplete: bool = False

    #: Why iteration ended early: ``"max_rejects"``,
    #: ``"max_consecutive_rejects"``, or ``"observer_error"``
    stop_reason: Optional[str] = None

    def __call__(self, rejected: RejectedLine) -> None:
        self.record(rejected)

    def record(self, rejected: RejectedLine) -> None:
        """
        Record a single rejected line.
        """
        self.total += 1
        self.counts[rejected.reason] = self.counts.get(rejected.reason, 0) + 1
        self.consecutive += 1
        if self.consecutive > self.max_consecutive:
            self.max_consecutive = self.consecutive

    def accepted(self) -> None:
        """
        Record that a valid line was read.
        """
        self.consecutive = 0

    def count(self, reason: RejectionReason) -> int:
        """
        Number of rejections for a single reason.
        """
        return self.counts.get(reason, 0)

    def mark_incomplete(self, stop_reason: str) -> None:
        """
        Mark the iteration as incomplete.
        """
        self.incomplete = True
        self.stop_reason = stop_reason


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

    :param file_or_iterable: file-like object or iterable yielding lines as
        strings
    :param loads: custom json decoder callable
    """

    _file_or_iterable: Union[
        typing.IO[str], typing.IO[bytes], Iterable[Union[str, bytes]]
    ]
    _line_iter: Iterator[Tuple[int, Union[bytes, str]]] = attr.ib(init=False)
    _loads: LoadsCallable = attr.ib(default=default_loads, kw_only=True)
    _rejection_report: Optional[RejectionReport] = attr.ib(default=None, init=False)
    # Set only when read() itself detected the end of the input, so that
    # an EOFError raised by the underlying source is not mistaken for it.
    _input_exhausted: bool = attr.ib(default=False, init=False)

    def __attrs_post_init__(self) -> None:
        if isinstance(self._file_or_iterable, io.IOBase):
            self._fp = cast(
                Union[typing.IO[str], typing.IO[bytes]],
                self._file_or_iterable,
            )

        self._line_iter = enumerate(self._file_or_iterable, 1)

    @property
    def rejection_report(self) -> Optional[RejectionReport]:
        """
        Report for the most recent :py:meth:`iter` call, if any.

        This is ``None`` before :py:meth:`iter` has been called. Every
        :py:meth:`iter` call creates a fresh report; rejected lines
        themselves are not retained, only counts.
        """
        return self._rejection_report

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

        The resulting :py:exc:`InvalidLineError` has its ``.reason``
        attribute set to the corresponding :py:class:`RejectionReason`.
        """
        if self._closed:
            raise RuntimeError("reader is closed")
        if type is not None and type not in VALID_TYPES:
            raise ValueError("invalid type specified")

        try:
            lineno, line = next(self._line_iter)
            while skip_empty and not line.rstrip():
                lineno, line = next(self._line_iter)
        except StopIteration:
            self._input_exhausted = True
            raise EOFError from None

        if isinstance(line, bytes):
            try:
                line = line.decode("utf-8")
            except UnicodeDecodeError as orig_exc:
                exc = InvalidLineError(
                    f"line is not valid utf-8: {orig_exc}",
                    line,
                    lineno,
                    RejectionReason.DECODE,
                )
                raise exc from orig_exc

        if line.startswith(SKIPPABLE_SINGLE_INITIAL_CHARS):
            line = line[1:]

        try:
            value: JSONValue = self._loads(line)
        except ValueError as orig_exc:
            exc = InvalidLineError(
                f"line contains invalid json: {orig_exc}",
                line,
                lineno,
                RejectionReason.SYNTAX,
            )
            raise exc from orig_exc

        if value is None:
            if allow_none:
                return None
            raise InvalidLineError(
                "line contains null value", line, lineno, RejectionReason.NULL
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
                    lineno,
                    RejectionReason.TYPE,
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
        on_reject: Optional[Callable[[RejectedLine], Any]] = ...,
        max_rejects: Optional[int] = ...,
        max_consecutive_rejects: Optional[int] = ...,
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
        on_reject: Optional[Callable[[RejectedLine], Any]] = ...,
        max_rejects: Optional[int] = ...,
        max_consecutive_rejects: Optional[int] = ...,
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
        on_reject: Optional[Callable[[RejectedLine], Any]] = ...,
        max_rejects: Optional[int] = ...,
        max_consecutive_rejects: Optional[int] = ...,
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
        on_reject: Optional[Callable[[RejectedLine], Any]] = ...,
        max_rejects: Optional[int] = ...,
        max_consecutive_rejects: Optional[int] = ...,
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
        on_reject: Optional[Callable[[RejectedLine], Any]] = ...,
        max_rejects: Optional[int] = ...,
        max_consecutive_rejects: Optional[int] = ...,
    ) -> Iterator[Optional[TJSONValue]]:
        ...  # pragma: no cover

    def iter(
        self,
        type: Optional[Type[Any]] = None,
        allow_none: bool = False,
        skip_empty: bool = False,
        skip_invalid: bool = False,
        *,
        on_reject: Optional[Callable[[RejectedLine], Any]] = None,
        max_rejects: Optional[int] = None,
        max_consecutive_rejects: Optional[int] = None,
    ) -> Iterator[Optional[JSONValue]]:
        """
        Iterate over all lines.

        This is the iterator equivalent to repeatedly calling
        :py:meth:`~Reader.read()`. If no arguments are specified, this
        is the same as directly iterating over this :py:class:`Reader`
        instance.

        When `skip_invalid` is set to ``True``, invalid lines will be
        silently ignored, except that they are reported through the
        optional `on_reject` callback (see below). Rejected lines are
        always counted in the :py:attr:`rejection_report`.

        The `on_reject` argument registers an observer that is called
        with a :py:class:`RejectedLine` for every rejected line. This
        does not change the skipping behaviour: lines are still skipped
        only when `skip_invalid` is true. When `skip_invalid` is false
        and `on_reject` is given, the observer is called and the
        :py:exc:`InvalidLineError` is re-raised afterwards. A
        :py:class:`RejectionReport` instance can be passed directly,
        since reports are callable. If the observer raises, iteration
        stops and the observer’s exception propagates unchanged; the
        report is marked incomplete with ``stop_reason ==
        "observer_error"``. Exceptions raised while reading from the
        underlying source are never swallowed.

        The `max_rejects` (absolute) and `max_consecutive_rejects`
        (consecutive) arguments limit the number of rejected lines.
        When a limit is exceeded, iteration stops and the report is
        marked incomplete (``stop_reason`` is ``"max_rejects"`` or
        ``"max_consecutive_rejects"``). Limits apply to rejections
        while skipping; they are irrelevant when `skip_invalid` is
        false.

        See :py:meth:`~Reader.read()` for a description of the other
        arguments.
        """
        if max_rejects is not None and max_rejects < 0:
            raise ValueError("max_rejects must be non-negative")
        if max_consecutive_rejects is not None and max_consecutive_rejects < 0:
            raise ValueError("max_consecutive_rejects must be non-negative")

        report = RejectionReport(
            max_rejects=max_rejects,
            max_consecutive_rejects=max_consecutive_rejects,
        )
        self._rejection_report = report

        return self._iter_lines(
            type=type,
            allow_none=allow_none,
            skip_empty=skip_empty,
            skip_invalid=skip_invalid,
            on_reject=on_reject,
            report=report,
        )

    def _iter_lines(
        self,
        *,
        type: Optional[Type[Any]],
        allow_none: bool,
        skip_empty: bool,
        skip_invalid: bool,
        on_reject: Optional[Callable[[RejectedLine], Any]],
        report: RejectionReport,
    ) -> Iterator[Optional[JSONValue]]:
        while True:
            try:
                value = self.read(
                    type=type, allow_none=allow_none, skip_empty=skip_empty
                )
            except EOFError:
                if self._input_exhausted:
                    # Genuine end of input; nothing was rejected.
                    return
                # An EOFError from the source or from a custom loads()
                # callable is not the end of the input; propagate it.
                raise
            except InvalidLineError as exc:
                rejected = RejectedLine(
                    lineno=exc.lineno,
                    reason=exc.reason or RejectionReason.SYNTAX,
                    preview=exc.preview,
                )
                report.record(rejected)
                if on_reject is not None:
                    try:
                        on_reject(rejected)
                    except BaseException:
                        # The observer failed: stop iterating and let the
                        # observer's exception propagate unchanged. This
                        # deliberately also applies to exceptions that
                        # iteration itself treats as control flow (e.g.
                        # EOFError), so a failing observer is never
                        # mistaken for the end of the input.
                        report.mark_incomplete("observer_error")
                        raise
                if not skip_invalid:
                    raise
                if report.max_rejects is not None and report.total > report.max_rejects:
                    report.mark_incomplete("max_rejects")
                    return
                if (
                    report.max_consecutive_rejects is not None
                    and report.consecutive > report.max_consecutive_rejects
                ):
                    report.mark_incomplete("max_consecutive_rejects")
                    return
            else:
                report.accepted()
                yield value

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
