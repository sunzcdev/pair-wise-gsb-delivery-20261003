"""
jsonlines implementation
"""

import builtins
import codecs
import enum
import functools
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

# The optional accelerator modules are imported once, at import time, so the
# set of available backends is a property of the process and cannot change
# halfway through a reader/writer's lifetime. Which one is *used* is decided
# per reader/writer instance, and frozen on that instance (see
# ``CodecConfig``).
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


class DumpsResultConversion(enum.Enum):
    LeaveAsIs = enum.auto()
    EncodeToBytes = enum.auto()
    DecodeToString = enum.auto()


# https://docs.python.org/3/library/functions.html#open
Openable = Union[str, bytes, int, "os.PathLike[Any]"]

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


# Marker callables used as defaults, so that an explicitly provided
# ``loads``/``dumps`` callable can be distinguished from "no choice made by the
# caller". Resolving the marker happens once, when a reader/writer instance is
# created, and the resulting configuration is frozen on that instance.
def default_loads(data: Union[str, bytes]) -> Any:
    """
    Fake ``loads()`` function to use as a default marker.
    """
    raise NotImplementedError  # pragma: no cover


def default_dumps(obj: Any) -> str:
    """
    Fake ``dumps()`` function to use as a default marker.
    """
    raise NotImplementedError  # pragma: no cover


# Bound once, so that default detection is unaffected by later rebinding of the
# module-level names above.
_DEFAULT_LOADS_MARKER = default_loads
_DEFAULT_DUMPS_MARKER = default_dumps


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
class CodecConfigError(Error, ValueError):
    """
    Error raised when the requested codec configuration cannot be honoured.

    This is raised while a reader or writer is being created, i.e. before any
    data is read from the source or written to the destination. Examples are
    an unknown backend name, a backend that is not installed, or asking for
    both an explicit backend and a custom ``loads``/``dumps`` callable.

    Like :py:class:`jsonlines.InvalidLineError`, this class subclasses both
    :py:class:`jsonlines.Error` and the built-in ``ValueError``.
    """


class Backend(str, enum.Enum):
    """
    Names of the JSON codec backends that can be selected explicitly.

    The ``CUSTOM`` member is reported by :py:class:`CodecConfig` when the
    caller supplied their own ``loads``/``dumps`` callable; it cannot be
    selected by name.
    """

    JSON = "json"
    ORJSON = "orjson"
    UJSON = "ujson"

    #: Reported for caller-supplied ``loads``/``dumps`` callables.
    CUSTOM = "custom"


#: Values describing how a backend deals with a particular kind of value.
#:
#: ``"supported"`` means the value round-trips unchanged, ``"rejected"`` means
#: the backend raises an error, ``"substituted"`` means the backend silently
#: produces a different value (e.g. ``null`` for ``NaN``), ``"lossy"`` means a
#: value of a different type/precision is produced, and ``"unknown"`` means no
#: claim is made (typically for custom callables).
ValueHandling = Literal["supported", "rejected", "substituted", "lossy", "unknown"]


@attr.s(auto_attribs=True, frozen=True)
class CodecCapabilities:
    """
    Snapshot of what the selected codec backend actually does.

    The values are descriptive only: they report observed backend behaviour,
    they do not cause records to be rewritten. Attributes that do not apply to
    the direction of a codec are ``None``; attributes that cannot be verified
    (custom callables) are ``None`` or ``"unknown"``.
    """

    #: Type returned by ``dumps()``, or ``None`` when not known in advance.
    result_type: Optional[Literal["str", "bytes"]] = None

    #: How ``NaN`` values are handled.
    nan: ValueHandling = "unknown"

    #: How (positive or negative) infinity values are handled.
    infinity: ValueHandling = "unknown"

    #: How integers outside the backend's native range are handled.
    big_integers: ValueHandling = "unknown"

    #: Whether the backend can sort object keys, or ``None`` when not applicable.
    supports_sort_keys: Optional[bool] = None

    #: Whether the backend can produce both compact and non-compact output.
    #: ``False`` means output is always compact.
    supports_compact_output: Optional[bool] = None

    #: Additional human readable notes about observable backend differences.
    notes: Tuple[str, ...] = ()


# Capability snapshots for the known backends. These are deliberately static
# (verified against the upstream projects) so that inspecting a configuration
# never runs extra encode/decode work on caller data.
_LOAD_CAPABILITIES: Dict[Backend, CodecCapabilities] = {
    Backend.JSON: CodecCapabilities(
        nan="supported",
        infinity="supported",
        big_integers="supported",
        notes=("NaN and Infinity are accepted although they are not valid JSON",),
    ),
    Backend.ORJSON: CodecCapabilities(
        nan="rejected",
        infinity="rejected",
        big_integers="lossy",
        notes=(
            "NaN and Infinity literals are rejected",
            "integers outside the 64-bit range are decoded as floats",
        ),
    ),
    Backend.UJSON: CodecCapabilities(
        nan="supported",
        infinity="supported",
        big_integers="supported",
        notes=("NaN and Infinity are accepted although they are not valid JSON",),
    ),
}

_DUMPS_CAPABILITIES: Dict[Backend, CodecCapabilities] = {
    Backend.JSON: CodecCapabilities(
        result_type="str",
        nan="supported",
        infinity="supported",
        big_integers="supported",
        supports_sort_keys=True,
        supports_compact_output=True,
        notes=("NaN and Infinity are written as non-standard JSON literals",),
    ),
    Backend.ORJSON: CodecCapabilities(
        result_type="bytes",
        nan="substituted",
        infinity="substituted",
        big_integers="rejected",
        supports_sort_keys=True,
        supports_compact_output=False,
        notes=(
            "output is always compact; 'compact=False' cannot be honoured",
            "NaN and Infinity are serialized as null",
            "integers outside the 64-bit range raise TypeError",
        ),
    ),
    Backend.UJSON: CodecCapabilities(
        result_type="str",
        nan="supported",
        infinity="supported",
        big_integers="supported",
        supports_sort_keys=True,
        supports_compact_output=True,
        notes=("NaN and Infinity are written as non-standard JSON literals",),
    ),
}


@attr.s(auto_attribs=True, frozen=True)
class CodecConfig:
    """
    Immutable snapshot of the codec configuration of a reader or writer.

    A snapshot is created when the reader/writer is created and is not
    affected by later changes to the process-wide default backend, nor by
    later changes to the module-level references to optional JSON libraries.
    This makes it possible to inspect what a long-lived reader/writer is
    actually using.

    Snapshots compare equal when they describe the same configuration; the
    identity of the underlying callable is not part of the comparison.
    """

    #: Direction of the codec: ``"loads"`` for readers, ``"dumps"`` for writers.
    direction: Literal["loads", "dumps"]

    #: The backend that is actually used.
    backend: Backend

    #: Whether the callable was supplied by the caller instead of selected.
    is_custom: bool

    #: Capabilities of the selected backend.
    capabilities: CodecCapabilities

    #: The effective encoding/decoding callable. Not part of equality/hash,
    #: and omitted from the repr to keep snapshots readable.
    func: Callable[..., Any] = attr.ib(eq=False, hash=False, repr=False)

    #: Whether compact output was requested (writers only).
    compact: Optional[bool] = None

    #: Whether key sorting was requested (writers only).
    sort_keys: Optional[bool] = None


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
    _codec_config: CodecConfig = attr.ib(init=False)

    @property
    def codec_config(self) -> CodecConfig:
        """
        The frozen codec configuration of this reader/writer.

        The snapshot reports the backend that is actually used and the
        capabilities of that backend. It does not change when the
        process-wide default backend changes, and it never causes records to
        be rewritten to paper over backend differences.
        """
        return self._codec_config

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


# Module-level pinned default backend. ``None`` means "automatic": readers use
# the fastest available backend, writers use the standard library.
_DEFAULT_BACKEND: Optional[Backend] = None


def _backend_module(backend: Backend) -> Optional[types.ModuleType]:
    """
    Return the module object for `backend`, or ``None`` if it is unavailable.

    Availability is decided once, at import time, so the set of usable
    backends cannot change while a reader or writer is alive.
    """
    if backend is Backend.JSON:
        return json
    if backend is Backend.ORJSON:
        return orjson
    if backend is Backend.UJSON:
        return ujson
    return None  # pragma: no cover - Backend.CUSTOM has no module


def _parse_backend(value: Any) -> Backend:
    """
    Convert a user supplied backend name to a :py:class:`Backend` member.

    The special ``"custom"`` value is rejected here: it is only ever reported,
    never selected.
    """
    if isinstance(value, Backend):
        backend = value
    elif isinstance(value, str):
        try:
            backend = Backend(value)
        except ValueError:
            valid = ", ".join(
                repr(member.value) for member in Backend if member is not Backend.CUSTOM
            )
            raise CodecConfigError(
                f"unknown json backend {value!r}; supported backends are {valid}"
            ) from None
    else:
        raise CodecConfigError(
            "json backend must be a string or a jsonlines.Backend member, "
            f"not {type(value).__name__}"
        )

    if backend is Backend.CUSTOM:
        raise CodecConfigError(
            "'custom' is reported for caller supplied loads/dumps callables "
            "and cannot be selected explicitly"
        )

    if _backend_module(backend) is None:
        raise CodecConfigError(
            f"json backend {backend.value!r} is not available; "
            f"install the {backend.value!r} package or select another backend"
        )
    return backend


def _detect_default_backend() -> Backend:
    """
    Pick the fastest available backend, falling back to the stdlib.
    """
    for candidate in (Backend.ORJSON, Backend.UJSON):
        if _backend_module(candidate) is not None:
            return candidate
    return Backend.JSON


def get_default_backend(
    direction: Literal["loads", "dumps"] = "loads",
) -> Backend:
    """
    Return the backend that a new reader or writer would select.

    When no default has been pinned with :py:func:`set_default_backend`, this
    reports the automatic choice for `direction`: the fastest available
    decoder for readers and the standard library for writers.

    :param direction: ``"loads"`` (readers) or ``"dumps"`` (writers)
    """
    if direction not in ("loads", "dumps"):
        raise ValueError("'direction' must be either 'loads' or 'dumps'")
    if _DEFAULT_BACKEND is not None:
        # An explicitly pinned default applies to both directions; only an
        # installed backend can have been pinned, so this is always usable.
        return _DEFAULT_BACKEND
    if direction == "dumps":
        return Backend.JSON
    return _detect_default_backend()


def set_default_backend(backend: Optional[Union[Backend, str]]) -> None:
    """
    Pin the backend used by readers and writers created from now on.

    Passing ``None`` restores automatic selection. Existing reader/writer
    instances are never affected; each instance keeps the backend that was
    selected when it was created.

    :param backend: backend name (``"json"``, ``"orjson"``, ``"ujson"``), a
        :py:class:`Backend` member, or ``None`` for automatic selection
    """
    global _DEFAULT_BACKEND
    if backend is None:
        _DEFAULT_BACKEND = None
    else:
        _DEFAULT_BACKEND = _parse_backend(backend)


def _custom_capabilities() -> CodecCapabilities:
    """
    Capabilities for a caller supplied callable.

    Nothing is assumed: all value handling and the return type are reported as
    unknown rather than guessed from the environment.
    """
    return CodecCapabilities(
        notes=("capabilities are unknown for caller supplied callables",),
    )


def _check_custom_callable(name: str, value: Any) -> None:
    if not callable(value):
        raise CodecConfigError(
            f"'{name}' must be a callable, not {type(value).__name__}"
        )


def _resolve_loads_config(
    loads: LoadsCallable, backend: Optional[Union[Backend, str]]
) -> CodecConfig:
    """
    Determine the decoding configuration for a new reader.

    Raises :py:class:`CodecConfigError` for invalid combinations, before any
    data is read from the source.
    """
    if loads is not _DEFAULT_LOADS_MARKER:
        if backend is not None:
            raise CodecConfigError(
                "'backend' cannot be combined with a custom 'loads' callable; "
                "the custom callable would be ignored"
            )
        _check_custom_callable("loads", loads)
        return CodecConfig(
            direction="loads",
            backend=Backend.CUSTOM,
            is_custom=True,
            func=loads,
            capabilities=_custom_capabilities(),
        )

    if backend is not None:
        selected = _parse_backend(backend)
    elif _DEFAULT_BACKEND is not None:
        selected = _DEFAULT_BACKEND
    else:
        selected = _detect_default_backend()

    module = _backend_module(selected)
    if module is None:  # pragma: no cover - defensive, _parse_backend already checks
        raise CodecConfigError(f"json backend {selected.value!r} is not available")

    return CodecConfig(
        direction="loads",
        backend=selected,
        is_custom=False,
        func=cast(LoadsCallable, module.loads),
        capabilities=_LOAD_CAPABILITIES[selected],
    )


def _json_dumps(compact: Optional[bool], sort_keys: Optional[bool]) -> DumpsCallable:
    return json.JSONEncoder(
        ensure_ascii=False,
        separators=(",", ":") if compact else (", ", ": "),
        sort_keys=bool(sort_keys),
    ).encode


def _build_dumps(
    backend: Backend, compact: Optional[bool], sort_keys: Optional[bool]
) -> DumpsCallable:
    module = _backend_module(backend)
    if module is None:  # pragma: no cover - defensive, _parse_backend already checks
        raise CodecConfigError(f"json backend {backend.value!r} is not available")

    if backend is Backend.JSON:
        return _json_dumps(compact, sort_keys)

    if backend is Backend.ORJSON:
        # orjson output is always compact; a request for non-compact output
        # cannot be honoured, which is reported through the capability
        # snapshot rather than silently changing the record contents.
        option = 0
        if sort_keys:
            option |= cast(int, module.OPT_SORT_KEYS)
        if option:
            return cast(DumpsCallable, functools.partial(module.dumps, option=option))
        return cast(DumpsCallable, module.dumps)

    # ujson
    return cast(
        DumpsCallable,
        functools.partial(
            module.dumps,
            ensure_ascii=False,
            separators=(",", ":") if compact else (", ", ": "),
            sort_keys=bool(sort_keys),
        ),
    )


def _resolve_dumps_config(
    dumps: DumpsCallable,
    backend: Optional[Union[Backend, str]],
    compact: Optional[bool],
    sort_keys: Optional[bool],
) -> CodecConfig:
    """
    Determine the encoding configuration for a new writer.

    Raises :py:class:`CodecConfigError` for invalid combinations, before the
    destination is touched.
    """
    if dumps is not _DEFAULT_DUMPS_MARKER:
        if backend is not None:
            raise CodecConfigError(
                "'backend' cannot be combined with a custom 'dumps' callable; "
                "the custom callable would be ignored"
            )
        _check_custom_callable("dumps", dumps)
        return CodecConfig(
            direction="dumps",
            backend=Backend.CUSTOM,
            is_custom=True,
            func=dumps,
            capabilities=_custom_capabilities(),
            compact=compact,
            sort_keys=sort_keys,
        )

    if backend is not None:
        selected = _parse_backend(backend)
    elif _DEFAULT_BACKEND is not None:
        selected = _DEFAULT_BACKEND
    else:
        # Writing defaults to the stdlib: accelerator libraries produce
        # different output (whitespace, NaN handling, ...) and must be opted
        # into explicitly.
        selected = Backend.JSON

    capabilities = _DUMPS_CAPABILITIES[selected]
    return CodecConfig(
        direction="dumps",
        backend=selected,
        is_custom=False,
        func=_build_dumps(selected, compact, sort_keys),
        capabilities=capabilities,
        compact=compact,
        sort_keys=sort_keys,
    )


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

    By default the fastest available backend is used, which is frozen when the
    reader is created. The `backend` argument can be used to force a specific
    backend for this reader; it cannot be combined with `loads`. The result is
    available as an immutable :py:class:`CodecConfig` snapshot on the
    :py:attr:`codec_config` attribute.

    :param file_or_iterable: file-like object or iterable yielding lines as
        strings
    :param loads: custom json decoder callable
    :param backend: explicit json backend (``"json"``, ``"orjson"``,
        ``"ujson"``); by default the process-wide default backend is used
    """

    _file_or_iterable: Union[
        typing.IO[str], typing.IO[bytes], Iterable[Union[str, bytes]]
    ]
    _line_iter: Iterator[Tuple[int, Union[bytes, str]]] = attr.ib(init=False)
    _loads: LoadsCallable = attr.ib(default=default_loads, kw_only=True)
    _backend: Optional[Union[Backend, str]] = attr.ib(default=None, kw_only=True)

    def __attrs_post_init__(self) -> None:
        # Resolve the codec first: an invalid configuration must be reported
        # before the source is touched in any way.
        self._codec_config = _resolve_loads_config(self._loads, self._backend)
        self._loads = self._codec_config.func

        if isinstance(self._file_or_iterable, io.IOBase):
            self._fp = cast(
                Union[typing.IO[str], typing.IO[bytes]],
                self._file_or_iterable,
            )

        self._line_iter = enumerate(self._file_or_iterable, 1)

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
            lineno, line = next(self._line_iter)
            while skip_empty and not line.rstrip():
                lineno, line = next(self._line_iter)
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

    By default the standard library is used for encoding. The `backend`
    argument can be used to select another backend for this writer; the
    selection is frozen for the lifetime of the writer and cannot be combined
    with `dumps`. The resulting configuration, including the capabilities of
    the actual backend, is available as an immutable
    :py:class:`CodecConfig` snapshot on the :py:attr:`codec_config`
    attribute.

    When the `flush` argument is set to ``True``, the writer will call
    ``fp.flush()`` after each written line.

    :param fp: writable file-like object
    :param compact: whether to use a compact output format
    :param sort_keys: whether to sort object keys
    :param dumps: custom encoder callable
    :param flush: whether to flush the file-like object after writing each line
    :param backend: explicit json backend (``"json"``, ``"orjson"``,
        ``"ujson"``); by default the process-wide default backend is used
    """

    _fp: Union[typing.IO[str], typing.IO[bytes]] = attr.ib(default=None)
    _fp_is_binary: bool = attr.ib(default=False, init=False)
    _compact: bool = attr.ib(default=False, kw_only=True)
    _sort_keys: bool = attr.ib(default=False, kw_only=True)
    _flush: bool = attr.ib(default=False, kw_only=True)
    _dumps: DumpsCallable = attr.ib(default=default_dumps, kw_only=True)
    _backend: Optional[Union[Backend, str]] = attr.ib(default=None, kw_only=True)
    _dumps_result_conversion: DumpsResultConversion = attr.ib(
        default=DumpsResultConversion.LeaveAsIs, init=False
    )
    # Whether the return type of the dumps callable is known up front. It is
    # for the built-in backends (from the capability snapshot); for custom
    # callables it is observed on the first write, so that the callable is
    # never invoked with synthetic data.
    _dumps_result_type_known: bool = attr.ib(default=True, init=False)

    def __attrs_post_init__(self) -> None:
        # Resolve the codec first: an invalid configuration must be reported
        # before the destination is touched in any way.
        self._codec_config = _resolve_dumps_config(
            self._dumps, self._backend, self._compact, self._sort_keys
        )
        self._dumps = self._codec_config.func

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

        # Determine if str-to-bytes conversion (or vice versa) is needed for
        # the combination of this file-like object and the used dumps()
        # callable. This avoids checking this for each .write(). For known
        # backends the return type comes from the frozen capability snapshot;
        # for custom callables it is observed on the first write. Note that
        # this deliberately does not support ‘dynamic’ return types that
        # depend on input and dump options, like simplejson on Python 2 in
        # some cases.
        result_type = self._codec_config.capabilities.result_type
        if result_type is None:
            self._dumps_result_type_known = False
        else:
            self._set_dumps_result_conversion(result_type)

    def _set_dumps_result_conversion(self, result_type: str) -> None:
        if result_type == "str" and self._fp_is_binary:
            self._dumps_result_conversion = DumpsResultConversion.EncodeToBytes
        elif result_type == "bytes" and not self._fp_is_binary:
            self._dumps_result_conversion = DumpsResultConversion.DecodeToString
        else:
            self._dumps_result_conversion = DumpsResultConversion.LeaveAsIs

    def write(self, obj: Any) -> int:
        """
        Encode and write a single object.

        :param obj: the object to encode and write
        :return: number of characters or bytes written
        """
        if self._closed:
            raise RuntimeError("writer is closed")

        line = self._dumps(obj)

        if not self._dumps_result_type_known:
            # Custom callable: observe the return type on real output rather
            # than invoking it with a synthetic record.
            if isinstance(line, str):
                self._dumps_result_type_known = True
                self._set_dumps_result_conversion("str")
            elif isinstance(line, bytes):
                self._dumps_result_type_known = True
                self._set_dumps_result_conversion("bytes")

        # This handles either str or bytes, but the type checker does not know
        # that this code always passes the right type of arguments.
        if self._dumps_result_conversion == DumpsResultConversion.EncodeToBytes:
            line = line.encode()  # type: ignore[union-attr]
        elif self._dumps_result_conversion == DumpsResultConversion.DecodeToString:
            line = line.decode()  # type: ignore[union-attr]

        # The conversion above guarantees the type matches the stream, but that
        # invariant is not visible to the type checker.
        if self._fp_is_binary:
            binary_fp = cast(typing.IO[bytes], self._fp)
            binary_fp.write(cast(bytes, line))
            binary_fp.write(b"\n")
        else:
            text_fp = cast(typing.IO[str], self._fp)
            text_fp.write(cast(str, line))
            text_fp.write("\n")

        if self._flush:
            self._fp.flush()

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


def _reject_inapplicable_options(cls_name: str, **options: Any) -> None:
    """
    Mirror the strict signatures of :py:class:`Reader` and
    :py:class:`Writer` for the keyword arguments of :py:func:`open`.

    Options that only apply to the other direction are rejected instead of
    being silently ignored.
    """
    for name, value in options.items():
        if value is not None:
            raise TypeError(
                f"{cls_name}.__init__() got an unexpected keyword argument "
                + repr(name)
            )


@overload
def open(
    file: Openable,
    mode: Literal["r"] = ...,
    *,
    loads: Optional[LoadsCallable] = ...,
    backend: Optional[Union[Backend, str]] = ...,
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
    backend: Optional[Union[Backend, str]] = ...,
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
    backend: Optional[Union[Backend, str]] = ...,
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
    backend: Optional[Union[Backend, str]] = None,
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
    :param backend: explicit json backend (``"json"``, ``"orjson"``,
        ``"ujson"``); by default the process-wide default backend is used
    """
    if mode not in {"r", "w", "a", "x"}:
        raise ValueError("'mode' must be either 'r', 'w', 'a', or 'x'")

    cls = Reader if mode == "r" else Writer

    # Keep the historical strictness: options that do not apply to the chosen
    # direction are rejected instead of silently ignored.
    if mode == "r":
        _reject_inapplicable_options(
            "Reader",
            dumps=dumps,
            compact=compact,
            sort_keys=sort_keys,
            flush=flush,
        )
    else:
        _reject_inapplicable_options("Writer", loads=loads)

    # Validate the codec configuration before the file is opened, so that an
    # invalid backend or an invalid combination of options cannot create or
    # truncate the destination.
    if cls is Reader:
        _resolve_loads_config(loads if loads is not None else default_loads, backend)
    else:
        _resolve_dumps_config(
            dumps if dumps is not None else default_dumps,
            backend,
            bool(compact),
            bool(sort_keys),
        )

    encoding = "utf-8-sig" if mode == "r" else "utf-8"
    fp = builtins.open(file, mode=mode + "t", encoding=encoding)
    instance: Union[Reader, Writer]
    if mode == "r":
        instance = Reader(
            fp,
            loads=loads if loads is not None else _DEFAULT_LOADS_MARKER,
            backend=backend,
        )
    else:
        instance = Writer(
            fp,
            dumps=dumps if dumps is not None else _DEFAULT_DUMPS_MARKER,
            compact=bool(compact),
            sort_keys=bool(sort_keys),
            flush=bool(flush),
            backend=backend,
        )
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
