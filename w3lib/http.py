from __future__ import annotations

import re
from base64 import b64encode
from collections.abc import Iterable, Iterator, Mapping, MutableMapping, Sequence
from dataclasses import dataclass
from io import BytesIO
from typing import Any, TypeAlias, overload

from w3lib._util import to_unicode

HeadersDictInput: TypeAlias = Mapping[bytes, Any | Sequence[bytes]]
HeadersDictOutput: TypeAlias = MutableMapping[bytes, list[bytes]]

_TOKEN_RE = re.compile(rb"[!#$%&'*+\-.^_`|~0-9A-Za-z]+")


class HeadersParseError(ValueError):
    """Base class for raw header block parsing problems.

    Attributes:
        line: 1-based number of the physical line that caused the problem,
            or ``None`` when it is not tied to a specific line.
        raw: Raw content of the offending line, without its line terminator.
    """

    def __init__(
        self, message: str, *, line: int | None = None, raw: bytes = b""
    ) -> None:
        super().__init__(message)
        self.line = line
        self.raw = raw


class MissingColonError(HeadersParseError):
    """A non-empty line inside the header block has no ``:`` separator."""


class EmptyHeaderNameError(HeadersParseError):
    """A header line has an empty field name, i.e. it starts with ``:``."""


class InvalidHeaderNameError(HeadersParseError):
    """A header field name contains characters outside the HTTP token set."""


class ControlCharacterError(HeadersParseError):
    """A header field value contains a forbidden control character."""


class ObsFoldError(HeadersParseError):
    """A line starts with whitespace (obsolete line folding, RFC 9112)."""


class LoneCarriageReturnError(HeadersParseError):
    """A line is terminated by a bare CR not followed by a LF."""


class TrailingDataError(HeadersParseError):
    """Data found after the blank line that ends the header block."""


@dataclass(frozen=True, slots=True)
class RawHeader:
    """A single parsed header with its position in the raw input.

    ``name`` and ``value`` keep the original spelling; ``line`` is the
    1-based number of the physical line on which the header starts.
    """

    name: bytes
    value: bytes
    line: int


def _split_lines(raw: bytes) -> Iterator[tuple[bytes, int, bool]]:
    """Yield ``(line, line_number, bare_cr)`` for each physical line.

    Both CRLF and LF are accepted as line terminators. A CR not followed by
    a LF also terminates a line, but is flagged through ``bare_cr`` so the
    caller can reject or report it.
    """
    start = 0
    lineno = 1
    pos = 0
    length = len(raw)
    while pos < length:
        byte = raw[pos]
        if byte == 0x0A:  # LF
            yield raw[start:pos], lineno, False
            lineno += 1
            pos += 1
            start = pos
        elif byte == 0x0D:  # CR
            if pos + 1 < length and raw[pos + 1] == 0x0A:
                yield raw[start:pos], lineno, False
                lineno += 1
                pos += 2
                start = pos
            else:
                yield raw[start:pos], lineno, True
                lineno += 1
                pos += 1
                start = pos
        else:
            pos += 1
    if start < length:
        yield raw[start:], lineno, False


def _check_output_name(name: bytes) -> None:
    if not _TOKEN_RE.fullmatch(name):
        raise ValueError(f"Cannot serialize invalid header field name {name!r}")


def _check_output_value(value: bytes) -> None:
    if any(byte == 0x7F or byte < 0x20 and byte != 0x09 for byte in value):
        raise ValueError(
            f"Cannot serialize header value with control characters: {value!r}"
        )


class RawHeaders(Sequence[RawHeader]):
    """Parsed raw headers, in their original order and spelling.

    Lookup is case-insensitive and multi-value aware: repeated fields such
    as ``Set-Cookie`` are returned as separate values and are never joined
    with commas. Parsing problems found in lenient mode are available in
    :attr:`issues`.
    """

    def __init__(
        self,
        headers: Iterable[RawHeader] = (),
        issues: Iterable[HeadersParseError] = (),
    ) -> None:
        self._headers = tuple(headers)
        self.issues = tuple(issues)

    @property
    def headers(self) -> tuple[RawHeader, ...]:
        """Parsed headers, in their original order."""
        return self._headers

    def __len__(self) -> int:
        return len(self._headers)

    @overload
    def __getitem__(self, index: int) -> RawHeader: ...

    @overload
    def __getitem__(self, index: slice) -> tuple[RawHeader, ...]: ...

    def __getitem__(self, index: int | slice) -> RawHeader | tuple[RawHeader, ...]:
        return self._headers[index]

    def __repr__(self) -> str:
        return f"{type(self).__name__}(headers={self._headers!r}, issues={self.issues!r})"

    def get(self, name: bytes, default: bytes | None = None) -> bytes | None:
        """Return the first value for *name* (case-insensitive).

        Returns *default* when no header with that name is present.
        """
        lowered = name.lower()
        for header in self._headers:
            if header.name.lower() == lowered:
                return header.value
        return default

    def get_all(self, name: bytes) -> list[bytes]:
        """Return all values for *name* (case-insensitive), in original order.

        Values are returned as a list and never comma-joined, so multi-value
        fields such as ``Set-Cookie`` keep each of their values intact.
        """
        lowered = name.lower()
        return [header.value for header in self._headers if header.name.lower() == lowered]

    def to_raw(self) -> bytes:
        """Serialize back to a CRLF-separated raw header block.

        Raises :exc:`ValueError` if any name or value contains characters
        that could inject extra lines, so serializing a valid result never
        produces additional headers or trailing newlines.
        """
        parts = []
        for header in self._headers:
            _check_output_name(header.name)
            _check_output_value(header.value)
            parts.append(header.name + b": " + header.value)
        return b"\r\n".join(parts)


@overload
def parse_raw_headers(headers_raw: bytes, *, strict: bool = True) -> RawHeaders: ...


@overload
def parse_raw_headers(headers_raw: None, *, strict: bool = True) -> None: ...


def parse_raw_headers(
    headers_raw: bytes | None, *, strict: bool = True
) -> RawHeaders | None:
    r"""Parse a raw HTTP header block into a :class:`RawHeaders` object.

    Unlike :func:`headers_raw_to_dict`, which silently skips malformed
    lines, this parser preserves the original order and spelling of headers,
    tracks the line number of each header, and reports every rejected line.

    In strict mode (the default) the first ambiguity raises a
    :class:`HeadersParseError` subclass:

    >>> from w3lib.http import parse_raw_headers
    >>> parse_raw_headers(b"Content-Type: text/html\r\nAccept: gzip\r\n\r\n")
    RawHeaders(headers=(RawHeader(name=b'Content-Type', value=b'text/html', line=1), RawHeader(name=b'Accept', value=b'gzip', line=2)), issues=())
    >>> parse_raw_headers(b"Content-Type text/html")
    Traceback (most recent call last):
    ...
    w3lib.http.MissingColonError: Header line has no ':' separator

    In lenient mode (``strict=False``) parsing problems are collected in
    ``result.issues`` instead of being raised, and best-effort headers are
    still returned. Obsolete folded (continuation) lines are unfolded into
    the previous header, never mistaken for a new header:

    >>> result = parse_raw_headers(b"X-A: 1\r\n folded\r\nbad line\r\n", strict=False)
    >>> result.get_all(b"x-a")
    [b'1 folded']
    >>> [type(issue).__name__ for issue in result.issues]
    ['ObsFoldError', 'MissingColonError']

    Argument is ``None`` (returns ``None``):

    >>> parse_raw_headers(None) is None
    True
    """
    if headers_raw is None:
        return None

    issues: list[HeadersParseError] = []
    headers: list[RawHeader] = []

    def reject(error: HeadersParseError) -> None:
        if strict:
            raise error
        issues.append(error)

    ended = False
    for line, lineno, bare_cr in _split_lines(headers_raw):
        if bare_cr:
            reject(
                LoneCarriageReturnError(
                    "Line terminated by a bare CR", line=lineno, raw=line
                )
            )
        if ended:
            reject(
                TrailingDataError(
                    "Data after the blank line that ends the header block",
                    line=lineno,
                    raw=line,
                )
            )
            break
        if not line:
            ended = True
            continue
        if line[:1] in (b" ", b"\t"):
            reject(
                ObsFoldError(
                    "Line starts with whitespace (obsolete line folding)",
                    line=lineno,
                    raw=line,
                )
            )
            if headers:
                previous = headers[-1]
                folded = line.strip()
                if folded:
                    headers[-1] = RawHeader(
                        previous.name,
                        previous.value + b" " + folded,
                        previous.line,
                    )
            continue
        name, sep, value = line.partition(b":")
        if not sep:
            reject(
                MissingColonError(
                    "Header line has no ':' separator", line=lineno, raw=line
                )
            )
            continue
        if not name:
            reject(
                EmptyHeaderNameError(
                    "Header line has an empty field name", line=lineno, raw=line
                )
            )
            continue
        if not _TOKEN_RE.fullmatch(name):
            reject(
                InvalidHeaderNameError(
                    f"Header field name {name!r} is not a valid HTTP token",
                    line=lineno,
                    raw=line,
                )
            )
        value = value.strip(b" \t")
        if any(byte == 0x7F or byte < 0x20 and byte != 0x09 for byte in value):
            reject(
                ControlCharacterError(
                    f"Header field value for {name!r} contains a control character",
                    line=lineno,
                    raw=line,
                )
            )
        headers.append(RawHeader(name, value, lineno))

    return RawHeaders(headers, issues)


@overload
def headers_raw_to_dict(headers_raw: bytes) -> HeadersDictOutput: ...


@overload
def headers_raw_to_dict(headers_raw: None) -> None: ...


def headers_raw_to_dict(headers_raw: bytes | None) -> HeadersDictOutput | None:
    r"""
    Convert raw headers (single multi-line bytestring)
    to a dictionary.

    For example:

    >>> import w3lib.http
    >>> w3lib.http.headers_raw_to_dict(b"Content-type: text/html\n\rAccept: gzip\n\n")   # doctest: +SKIP
    {'Content-type': ['text/html'], 'Accept': ['gzip']}

    Incorrect input:

    >>> w3lib.http.headers_raw_to_dict(b"Content-typt gzip\n\n")
    {}
    >>>

    Argument is ``None`` (return ``None``):

    >>> w3lib.http.headers_raw_to_dict(None)
    >>>

    """

    if headers_raw is None:
        return None

    if not headers_raw:
        return {}

    result_dict: HeadersDictOutput = {}

    for header in BytesIO(headers_raw):
        key, sep, value = header.partition(b":")
        if not sep:
            continue

        key, value = key.strip(), value.strip()

        if key in result_dict:
            result_dict[key].append(value)
        else:
            result_dict[key] = [value]

    return result_dict


@overload
def headers_dict_to_raw(headers_dict: HeadersDictInput) -> bytes: ...


@overload
def headers_dict_to_raw(headers_dict: None) -> None: ...


def headers_dict_to_raw(headers_dict: HeadersDictInput | None) -> bytes | None:
    r"""
    Returns a raw HTTP headers representation of headers

    For example:

    >>> import w3lib.http
    >>> w3lib.http.headers_dict_to_raw({b'Content-type': b'text/html', b'Accept': b'gzip'}) # doctest: +SKIP
    'Content-type: text/html\\r\\nAccept: gzip'
    >>>

    Note that keys and values must be bytes.

    Argument is ``None`` (returns ``None``):

    >>> w3lib.http.headers_dict_to_raw(None)
    >>>

    """

    if headers_dict is None:
        return None

    if not headers_dict:
        return b""

    parts = bytearray()

    for key, value in headers_dict.items():
        if isinstance(value, bytes):
            if parts:
                parts.extend(b"\r\n")
            parts.extend(key + b": " + value)

        elif isinstance(value, (list, tuple)):
            for v in value:
                if parts:
                    parts.extend(b"\r\n")
                parts.extend(key + b": " + v)

    return bytes(parts)


def basic_auth_header(
    username: str | bytes, password: str | bytes, encoding: str = "ISO-8859-1"
) -> bytes:
    """
    Return an `Authorization` header field value for `HTTP Basic Access Authentication (RFC 2617)`_

    >>> import w3lib.http
    >>> w3lib.http.basic_auth_header('someuser', 'somepass')
    b'Basic c29tZXVzZXI6c29tZXBhc3M='

    .. _HTTP Basic Access Authentication (RFC 2617): http://www.ietf.org/rfc/rfc2617.txt

    """

    auth = f"{to_unicode(username)}:{to_unicode(password)}"
    # RFC 2617 doesn't define encoding, but ISO-8859-1 seems to be the
    # most widely used encoding here. See also:
    # http://greenbytes.de/tech/webdav/draft-ietf-httpauth-basicauth-enc-latest.html
    return b"Basic " + b64encode(auth.encode(encoding=encoding))
