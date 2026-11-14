from __future__ import annotations

import re
from base64 import b64encode
from collections.abc import Iterable, Mapping, MutableMapping, Sequence
from dataclasses import dataclass
from io import BytesIO
from typing import Any, TypeAlias, overload
from urllib.parse import urljoin

from w3lib._util import to_unicode

HeadersDictInput: TypeAlias = Mapping[bytes, Any | Sequence[bytes]]
HeadersDictOutput: TypeAlias = MutableMapping[bytes, list[bytes]]


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


class LinkHeaderError(ValueError):
    """An error found while parsing a ``Link`` header value.

    Instances of this class are collected in
    :attr:`LinkHeadersResult.errors` by :func:`parse_link_headers`; they are
    never raised by it.
    """


@dataclass
class Link:
    """A single link-value of a ``Link`` header: a target URL and its
    parameters."""

    url: str
    """The link target, resolved against the base URL if one was given to
    :func:`parse_link_headers`, and kept as-is (possibly relative)
    otherwise."""

    params: dict[str, str]
    """The link parameters, in order of appearance, with lowercased names.
    A parameter without a value maps to an empty string."""

    @property
    def rels(self) -> tuple[str, ...]:
        """The relation types of the link, i.e. the whitespace-separated
        values of its ``rel`` parameter, e.g. ``("next",)``."""
        return tuple(self.params.get("rel", "").split())


@dataclass
class LinkHeadersResult:
    """The outcome of :func:`parse_link_headers`."""

    links: list[Link]
    """The successfully parsed links, in order of appearance."""

    errors: list[LinkHeaderError]
    """The errors found while parsing, in order of appearance.

    A link-value that cannot be parsed, e.g. one with a duplicate parameter
    or broken syntax, is reported here and excluded from :attr:`links`, so
    that it is never silently mistaken for a valid link. All link-values
    that can be parsed independently are still returned in :attr:`links`.
    """


# https://www.rfc-editor.org/rfc/rfc7230#section-3.2.6
_TOKEN_RE = re.compile(r"[!#$%&'*+\-.^_`|~0-9A-Za-z]+")


def _split_quoted(value: str, separator: str) -> tuple[list[str], str | None]:
    """Split *value* at every *separator* found outside of a quoted string,
    honoring backslash escapes inside quoted strings.

    Return the parts and, if the value ends inside an unterminated quoted
    string, the unparseable remainder instead of the last part.
    """
    parts = []
    buf: list[str] = []
    in_quote = False
    escaped = False
    for char in value:
        if escaped:
            buf.append(char)
            escaped = False
        elif in_quote:
            buf.append(char)
            if char == "\\":
                escaped = True
            elif char == '"':
                in_quote = False
        elif char == '"':
            in_quote = True
            buf.append(char)
        elif char == separator:
            parts.append("".join(buf))
            buf = []
        else:
            buf.append(char)
    tail = "".join(buf)
    if in_quote or escaped:
        return parts, tail
    parts.append(tail)
    return parts, None


def _unquote(value: str) -> str | None:
    """Return the text of a quoted-string *value*, with its quotes removed
    and its quoted-pair escapes resolved, or ``None`` if *value* is not a
    valid quoted-string."""
    if len(value) < 2 or not value.endswith('"'):
        return None
    chars: list[str] = []
    inner = value[1:-1]
    index = 0
    while index < len(inner):
        char = inner[index]
        if char == "\\":
            index += 1
            if index == len(inner):
                return None
            chars.append(inner[index])
        elif char == '"':
            return None
        else:
            chars.append(char)
        index += 1
    return "".join(chars)


def _parse_link_value(item: str) -> tuple[str, dict[str, str]]:
    """Parse a single link-value, e.g. ``<https://example.com>; rel=next``,
    into its target and its parameters."""
    if not item.startswith("<"):
        raise LinkHeaderError(f"link item does not start with '<': {item!r}")
    end = item.find(">", 1)
    if end == -1:
        raise LinkHeaderError(f"link item has no closing '>': {item!r}")
    target = item[1:end]
    if "<" in target:
        raise LinkHeaderError(f"link target contains '<': {item!r}")
    rest = item[end + 1 :]
    params: dict[str, str] = {}
    if not rest.strip(" \t"):
        return target, params
    segments, broken = _split_quoted(rest, ";")
    if broken is not None:
        raise LinkHeaderError(f"unterminated quoted string in link item: {item!r}")
    if segments[0].strip(" \t"):
        raise LinkHeaderError(
            f"unexpected text after the link target in link item: {item!r}"
        )
    for segment in segments[1:]:
        segment = segment.strip(" \t")
        if not segment:
            raise LinkHeaderError(f"empty link parameter in link item: {item!r}")
        name, eq, value = segment.partition("=")
        name = name.strip(" \t").lower()
        if not _TOKEN_RE.fullmatch(name):
            raise LinkHeaderError(
                f"invalid link parameter name in link item: {item!r}"
            )
        if name in params:
            raise LinkHeaderError(
                f"duplicate link parameter {name!r} in link item: {item!r}"
            )
        if not eq:
            params[name] = ""
            continue
        value = value.strip(" \t")
        if value.startswith('"'):
            unquoted = _unquote(value)
            if unquoted is None:
                raise LinkHeaderError(
                    f"invalid quoted-string for link parameter {name!r} "
                    f"in link item: {item!r}"
                )
            params[name] = unquoted
        else:
            if not _TOKEN_RE.fullmatch(value):
                raise LinkHeaderError(
                    f"invalid value for link parameter {name!r} "
                    f"in link item: {item!r}"
                )
            params[name] = value
    return target, params


def parse_link_headers(
    headers: Iterable[bytes | str],
    base_url: str | bytes | None = None,
) -> LinkHeadersResult:
    """Parse ``Link`` response header values (`RFC 8288`_), e.g. to find the
    ``next``, ``prev`` and ``alternate`` links of a page.

    *headers* is an iterable of raw header values, as bytes (decoded as
    ISO-8859-1) or strings, e.g. the values of all ``Link`` headers of a
    response. Links are returned in order of appearance, across all values.

    Relative link targets are resolved against *base_url*, e.g. the response
    URL, if given, and kept relative otherwise.

    Link-values that cannot be parsed, e.g. ones with duplicate parameters
    or broken syntax, are not silently dropped: they are reported in
    :attr:`LinkHeadersResult.errors` and excluded from
    :attr:`LinkHeadersResult.links`, while every link-value that can be
    parsed independently is still returned.

    >>> import w3lib.http
    >>> result = w3lib.http.parse_link_headers(
    ...     [b'</page/2>; rel="next prev", <https://example.com/fr>; rel=alternate; hreflang=fr'],
    ...     base_url="https://example.com/page/1",
    ... )  # doctest: +SKIP
    >>> result.links[0].url  # doctest: +SKIP
    'https://example.com/page/2'
    >>> result.links[0].rels  # doctest: +SKIP
    ('next', 'prev')

    .. _RFC 8288: https://www.rfc-editor.org/rfc/rfc8288.html
    """
    base = to_unicode(base_url) if base_url is not None else None
    links: list[Link] = []
    errors: list[LinkHeaderError] = []
    for header in headers:
        text = header.decode("latin-1") if isinstance(header, bytes) else header
        items, broken = _split_quoted(text, ",")
        if broken is not None:
            errors.append(
                LinkHeaderError(
                    f"unterminated quoted string in Link header value: {broken!r}"
                )
            )
        for item in items:
            item = item.strip(" \t")
            if not item:
                # Empty list elements are allowed (RFC 7230, section 7).
                continue
            try:
                target, params = _parse_link_value(item)
            except LinkHeaderError as error:
                errors.append(error)
                continue
            if base is not None:
                target = urljoin(base, target)
            links.append(Link(url=target, params=params))
    return LinkHeadersResult(links=links, errors=errors)
