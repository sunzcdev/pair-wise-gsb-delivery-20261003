"""
h2/alternative_service
~~~~~~~~~~~~~~~~~~~~~~

A reviewable model of RFC 7838 Alternative Service advertisements.

An ALTSVC frame carries an "Alternative Service Field Value". That value is
formatted like an HTTP header field and may describe zero or more alternative
services, each identified by a protocol identifier (for example ``h2`` or
``h3``) and, per RFC 7838, a port and optionally a host.

This module parses that field value into immutable objects. Parsing is purely
descriptive: it never opens a connection, never resolves a name and never
decides that an advertised origin (or a cross-origin address) is trustworthy.
Protocol identifiers this library does not know are preserved verbatim, so
callers can make their own decisions about them.

The parser is deliberately tolerant: malformed pieces are reported through
``AlternativeServiceField.errors`` instead of raising, and the raw bytes of
both the complete field value and every individual alternative service remain
available for auditing.
"""
from __future__ import annotations

from dataclasses import dataclass, replace
from enum import Enum

_BACKSLASH = ord(b"\\")
_QUOTE = ord(b'"')
_COMMA = ord(b",")
_SEMICOLON = ord(b";")
_EQUALS = ord(b"=")
_OWS = b" \t"

_TOKEN_CHARS = frozenset(
    b"!#$%&'*+-.^_`|~0123456789"
    b"abcdefghijklmnopqrstuvwxyz"
    b"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
)

MIN_PORT = 1
MAX_PORT = 65535


class AlternativeServiceScope(Enum):
    """
    The scope of an ALTSVC advertisement.

    RFC 7838 allows an ALTSVC frame to be carried either on stream 0, where it
    applies to the origin named in the frame, or on a regular stream, where it
    applies to the origin of that stream. Keeping the scope explicit stops a
    gateway from applying a stream-bound advertisement to a whole connection.
    """

    CONNECTION = "connection"
    STREAM = "stream"


class OriginSource(Enum):
    """
    Where the origin an advertisement applies to came from.

    ``FRAME`` means the peer sent the origin in the ALTSVC frame itself (only
    legal for connection-level advertisements). ``AUTHORITY`` means the origin
    was inferred by h2 from the ``:authority`` pseudo-header field of the
    stream the frame was received on.
    """

    FRAME = "frame"
    AUTHORITY = "authority"


class MaxAgeStatus(Enum):
    """
    The outcome of parsing the ``ma`` (maximum age) parameter.

    ``ABSENT``: no ``ma`` parameter was present.
    ``VALID``: a single well-formed value was present.
    ``INVALID``: a parameter was present but malformed, or ``ma`` was repeated.
    ``MIXED``: several services were advertised with different valid values.
    """

    ABSENT = "absent"
    VALID = "valid"
    INVALID = "invalid"
    MIXED = "mixed"


class InvalidAlternativeServiceAdvertisement(ValueError):
    """
    Raised when an ALTSVC frame combines the connection-level and stream-level
    forms illegally.

    Receiving such a frame is not a connection error: RFC 7838 says the frame
    should be ignored. This exception is exposed so callers can validate an
    advertisement before acting on it, or review why h2 dropped one.
    """


@dataclass(frozen=True)
class AlternativeService:
    """
    A single alternative service from an ALTSVC field value.

    ``protocol_id`` is kept exactly as sent, including identifiers h2 does not
    recognise. ``raw`` is the complete entry, so no information is lost even
    when parts of it could not be interpreted.
    """

    protocol_id: bytes
    """The protocol identifier (ALPN identifier) exactly as sent."""

    host: bytes | None
    """The advertised host, or ``None`` when the entry refers to the origin host."""

    port: int | None
    """The advertised port, or ``None`` when no port was specified."""

    max_age: int | None
    """The parsed ``ma`` value, or ``None`` when absent or unusable."""

    max_age_status: MaxAgeStatus
    """Whether the ``ma`` parameter was absent, valid or invalid."""

    persist: bool
    """Whether the ``persist=1`` parameter was present."""

    raw: bytes
    """The complete alternative service entry, exactly as sent."""

    unknown_parameters: tuple[bytes, ...] = ()
    """Parameters that h2 does not interpret, preserved verbatim."""

    duplicate_of: int | None = None
    """
    When this entry repeats an earlier entry in the same field value, the index
    of that earlier entry. ``None`` for the first occurrence.
    """

    @property
    def is_duplicate(self) -> bool:
        """Whether this entry repeats an earlier entry in the same field value."""
        return self.duplicate_of is not None

    @property
    def identity(self) -> tuple[bytes, bytes | None, int | None]:
        """
        The identity of the advertised service.

        Two advertisements with the same identity describe the same protocol on
        the same host and port, so the second is a repetition of the first.
        Case is folded for the protocol identifier and host.
        """
        return (
            self.protocol_id.lower(),
            self.host.lower() if self.host is not None else None,
            self.port,
        )

    @property
    def is_clearing(self) -> bool:
        """
        Whether this entry clears the alternative service it names.

        RFC 7838 gives ``ma=0`` the meaning "no longer available".
        """
        return self.max_age == 0


@dataclass(frozen=True)
class AlternativeServiceField:
    """
    A parsed "Alternative Service Field Value".

    ``raw`` always holds the bytes exactly as received, whether or not parsing
    succeeded. ``errors`` describes anything that could not be interpreted;
    such problems never raise and never discard the raw data.
    """

    raw: bytes
    """The complete field value, exactly as sent."""

    services: tuple[AlternativeService, ...]
    """The advertised alternative services, in the order they were sent."""

    errors: tuple[str, ...]
    """Human-readable descriptions of parsing problems, if any."""

    clears_all: bool
    """
    Whether the field value is the RFC 7838 ``clear`` directive, which clears
    every alternative service for the origin at once.
    """

    max_age: int | None
    """
    The maximum age in seconds when it is unambiguous, otherwise ``None``.
    """

    max_age_status: MaxAgeStatus
    """The aggregate outcome of parsing the ``ma`` parameters."""

    @property
    def protocol_ids(self) -> tuple[bytes, ...]:
        """The protocol identifiers advertised, in order, exactly as sent."""
        return tuple(service.protocol_id for service in self.services)

    @property
    def duplicates(self) -> tuple[AlternativeService, ...]:
        """Entries that repeat an earlier entry in this field value."""
        return tuple(service for service in self.services if service.is_duplicate)

    @property
    def cleared_services(self) -> tuple[AlternativeService, ...]:
        """Entries that clear a previously advertised alternative service."""
        return tuple(service for service in self.services if service.is_clearing)

    @property
    def is_clear(self) -> bool:
        """
        Whether this field value clears alternative services.

        This is true for the explicit ``clear`` directive, and also when every
        advertised service is itself a clearing (``ma=0``) entry.
        """
        if self.clears_all:
            return True
        return bool(self.services) and all(service.is_clearing for service in self.services)


def _split_outside_quotes(data: bytes, separator: int) -> list[bytes]:
    """
    Split ``data`` on ``separator`` bytes that are not inside a quoted-string.
    """
    parts: list[bytes] = []
    current = bytearray()
    in_quotes = False
    escaped = False

    for byte in data:
        if escaped:
            current.append(byte)
            escaped = False
            continue
        if in_quotes and byte == _BACKSLASH:
            current.append(byte)
            escaped = True
            continue
        if byte == _QUOTE:
            in_quotes = not in_quotes
            current.append(byte)
            continue
        if byte == separator and not in_quotes:
            parts.append(bytes(current))
            current.clear()
            continue
        current.append(byte)

    parts.append(bytes(current))
    return parts


def _find_outside_quotes(data: bytes, target: int) -> int:
    """
    Return the index of the first ``target`` byte outside a quoted-string.
    """
    in_quotes = False
    escaped = False

    for index, byte in enumerate(data):
        if escaped:
            escaped = False
            continue
        if in_quotes and byte == _BACKSLASH:
            escaped = True
            continue
        if byte == _QUOTE:
            in_quotes = not in_quotes
            continue
        if byte == target and not in_quotes:
            return index

    return -1


def _find_closing_quote(data: bytes) -> int:
    """
    Return the index of the quote that closes the one at index 0, or ``-1``.
    """
    escaped = False

    for index in range(1, len(data)):
        byte = data[index]
        if escaped:
            escaped = False
            continue
        if byte == _BACKSLASH:
            escaped = True
            continue
        if byte == _QUOTE:
            return index

    return -1


def _unescape_quoted(value: bytes) -> bytes:
    """
    Remove the surrounding quotes from a balanced quoted-string.

    The caller is responsible for having located the closing quote: this
    function only unescapes the interior, so a backslash always escapes the
    following byte.
    """
    result = bytearray()
    escaped = False

    for byte in value[1:-1]:
        if escaped:
            result.append(byte)
            escaped = False
        elif byte == _BACKSLASH:
            escaped = True
        else:
            result.append(byte)

    return bytes(result)


def _parse_port(remainder: bytes) -> tuple[int | None, str | None]:
    """
    Parse a ``:port`` suffix. Returns ``(port, error)``.
    """
    if not remainder.startswith(b":"):
        return None, f"unexpected data after host: {remainder!r}"

    port_bytes = remainder[1:]
    if not port_bytes:
        return None, "empty port"
    if not port_bytes.isdigit():
        return None, f"non-numeric port: {port_bytes!r}"

    port = int(port_bytes)
    if not MIN_PORT <= port <= MAX_PORT:
        return None, f"port out of range: {port}"

    return port, None


def _parse_authority(value: bytes) -> tuple[bytes | None, int | None, str | None]:
    """
    Parse an ``[ uri-host ] ":" port`` authority.

    Returns ``(host, port, error)``. An empty host means "the origin's host".
    An IPv6 literal may be bracketed. RFC 7838 makes the port mandatory, so a
    missing port is reported rather than guessed.
    """
    if not value:
        return None, None, "missing alternative service authority"

    if value.startswith(b"["):
        closing_bracket = value.find(b"]")
        if closing_bracket == -1:
            return None, None, f"unterminated IPv6 literal: {value!r}"
        host = value[1:closing_bracket]
        remainder = value[closing_bracket + 1:]
    else:
        colon = value.rfind(b":")
        if colon == -1:
            host = value
            remainder = b""
        else:
            host = value[:colon]
            remainder = value[colon:]

    port, port_error = _parse_port(remainder) if remainder else (None, "missing port")

    if port_error is not None:
        return (host or None), None, port_error

    return (host or None), port, None


def _parse_service_authority(value: bytes) -> tuple[bytes | None, int | None, str | None]:
    """
    Parse the alternative-service authority, quoted or not.

    RFC 7838 formats ``alt-authority`` as a quoted-string containing
    ``[ uri-host ] ":" port``, so quotes may enclose the port too
    (``h2=":8000"``). Unquoted forms are accepted as well for interoperation.
    """
    if value.startswith(b'"'):
        closing_quote = _find_closing_quote(value)
        if closing_quote == -1:
            return None, None, f"unterminated quoted authority: {value!r}"

        trailing = value[closing_quote + 1:].strip(_OWS)
        if trailing:
            return None, None, f"unexpected data after quoted authority: {trailing!r}"

        return _parse_authority(_unescape_quoted(value[:closing_quote + 1]))

    return _parse_authority(value)


def _parse_parameters(
    parameters: list[bytes],
) -> tuple[int | None, MaxAgeStatus, bool, tuple[bytes, ...], list[str]]:
    """
    Parse the ``;``-separated parameters of a single alternative service.

    Returns ``(max_age, max_age_status, persist, unknown_parameters, errors)``.
    """
    max_age: int | None = None
    max_age_status = MaxAgeStatus.ABSENT
    max_age_seen = False
    persist = False
    unknown: list[bytes] = []
    errors: list[str] = []

    for parameter in parameters:
        if not parameter:
            errors.append("empty alternative service parameter")
            continue

        equals = _find_outside_quotes(parameter, _EQUALS)
        if equals == -1:
            name, raw_value = parameter, None
        else:
            name = parameter[:equals].strip(_OWS)
            raw_value = parameter[equals + 1:].strip(_OWS)

        lowered = name.lower()

        if lowered == b"ma":
            if max_age_seen:
                max_age = None
                max_age_status = MaxAgeStatus.INVALID
                errors.append(f"duplicate ma parameter: {parameter!r}")
                continue

            max_age_seen = True
            if raw_value is None or not raw_value.isdigit():
                max_age = None
                max_age_status = MaxAgeStatus.INVALID
                errors.append(f"invalid ma parameter: {parameter!r}")
            else:
                max_age = int(raw_value)
                max_age_status = MaxAgeStatus.VALID
        elif lowered == b"persist":
            if raw_value != b"1":
                errors.append(f"invalid persist parameter: {parameter!r}")
            else:
                persist = True
        else:
            unknown.append(parameter)

    return max_age, max_age_status, persist, tuple(unknown), errors


def _parse_protocol_id(raw: bytes) -> tuple[bytes, str | None]:
    """
    Parse the protocol identifier of an alternative service.

    Identifiers h2 does not recognise are still accepted and returned exactly
    as sent; only values that cannot be a protocol identifier at all produce an
    error. ``raw`` never contains an unbalanced quoted-string: the caller only
    splits on an ``=`` found outside quotes.
    """
    if raw.startswith(b'"'):
        closing_quote = _find_closing_quote(raw)
        trailing = raw[closing_quote + 1:].strip(_OWS)
        if trailing:
            return (
                _unescape_quoted(raw[:closing_quote + 1]),
                f"unexpected data after quoted identifier: {trailing!r}",
            )

        protocol_id = _unescape_quoted(raw[:closing_quote + 1])
    else:
        protocol_id = raw

    if not protocol_id:
        return protocol_id, "empty protocol identifier"
    if any(byte not in _TOKEN_CHARS for byte in protocol_id):
        return protocol_id, f"non-token protocol identifier: {protocol_id!r}"

    return protocol_id, None


def _parse_service(entry: bytes) -> tuple[AlternativeService | None, list[str]]:
    """
    Parse one alternative service entry.

    ``None`` is returned when the entry carries no ``=`` at all, since there is
    then no protocol identifier to report.
    """
    parts = _split_outside_quotes(entry, _SEMICOLON)
    service_part = parts[0].strip(_OWS)
    parameters = [part.strip(_OWS) for part in parts[1:]]

    equals = _find_outside_quotes(service_part, _EQUALS)
    if equals == -1:
        return None, [f"alternative service has no protocol identifier: {entry!r}"]

    errors: list[str] = []

    protocol_raw = service_part[:equals].strip(_OWS)
    protocol_id, protocol_error = _parse_protocol_id(protocol_raw)
    if protocol_error is not None:
        errors.append(f"{protocol_error} in alternative service: {entry!r}")

    host, port, host_error = _parse_service_authority(service_part[equals + 1:].strip(_OWS))
    if host_error is not None:
        errors.append(f"invalid alternative service host in {entry!r}: {host_error}")

    max_age, max_age_status, persist, unknown, parameter_errors = _parse_parameters(parameters)
    errors.extend(parameter_errors)

    service = AlternativeService(
        protocol_id=protocol_id,
        host=host,
        port=port,
        max_age=max_age,
        max_age_status=max_age_status,
        persist=persist,
        raw=entry,
        unknown_parameters=unknown,
    )

    return service, errors


def _duplicate_key(service: AlternativeService) -> tuple[bytes, bytes | None, int | None]:
    """
    The identity used to recognise a repeated alternative service advertisement.
    """
    return service.identity


def _aggregate_max_age(
    services: tuple[AlternativeService, ...],
) -> tuple[int | None, MaxAgeStatus]:
    """
    Summarise the ``ma`` parameters of every advertised service.
    """
    if any(service.max_age_status is MaxAgeStatus.INVALID for service in services):
        return None, MaxAgeStatus.INVALID

    valid_values = {
        service.max_age for service in services
        if service.max_age_status is MaxAgeStatus.VALID
    }
    if len(valid_values) > 1:
        return None, MaxAgeStatus.MIXED
    if len(valid_values) == 1:
        return valid_values.pop(), MaxAgeStatus.VALID

    return None, MaxAgeStatus.ABSENT


def parse_alternative_service_field(field_value: bytes) -> AlternativeServiceField:
    """
    Parse an RFC 7838 "Alternative Service Field Value".

    This function never raises for malformed input: problems are described in
    :attr:`AlternativeServiceField.errors` and every raw byte remains
    available. Unknown protocol identifiers and parameters are preserved as
    opaque values.

    :param field_value: The bytes of the ALTSVC field value.
    :returns: A reviewable description of the advertisement.
    """
    stripped = field_value.strip(_OWS)

    if stripped.lower() == b"clear":
        return AlternativeServiceField(
            raw=field_value,
            services=(),
            errors=(),
            clears_all=True,
            max_age=None,
            max_age_status=MaxAgeStatus.ABSENT,
        )

    errors: list[str] = []
    if not stripped:
        errors.append("empty alternative service field value")

    services: list[AlternativeService] = []
    seen: dict[tuple[bytes, bytes | None, int | None], int] = {}

    for entry in _split_outside_quotes(field_value, _COMMA):
        entry = entry.strip(_OWS)
        if not entry:
            errors.append("empty alternative service entry")
            continue

        service, service_errors = _parse_service(entry)
        errors.extend(service_errors)

        if service is None:
            continue

        key = _duplicate_key(service)
        first_index = seen.get(key)
        if first_index is None:
            seen[key] = len(services)
        else:
            service = replace(service, duplicate_of=first_index)

        services.append(service)

    frozen_services = tuple(services)
    max_age, max_age_status = _aggregate_max_age(frozen_services)

    return AlternativeServiceField(
        raw=field_value,
        services=frozen_services,
        errors=tuple(errors),
        clears_all=False,
        max_age=max_age,
        max_age_status=max_age_status,
    )


def parse_origin(origin: bytes | None) -> tuple[bytes | None, int | None]:
    """
    Split an origin authority into ``(host, port)`` for comparison purposes.

    This only interprets the shape of the value; it does not resolve or
    canonicalise the host, and a missing port stays ``None`` rather than being
    defaulted. It exists so callers can notice that an advertisement points at
    a different host or port than the origin it is bound to.
    """
    if not origin:
        return None, None

    if origin.startswith(b"["):
        closing_bracket = origin.find(b"]")
        if closing_bracket == -1:
            return origin, None
        host = origin[1:closing_bracket]
        remainder = origin[closing_bracket + 1:]
    else:
        colon = origin.rfind(b":")
        if colon == -1:
            return origin, None
        host = origin[:colon]
        remainder = origin[colon:]

    port: int | None = None
    if remainder.startswith(b":") and remainder[1:].isdigit():
        port = int(remainder[1:])

    return host or None, port


def service_is_cross_origin(service: AlternativeService, origin: bytes | None) -> bool:
    """
    Whether an advertised service leaves the origin's host or port.

    A service that names no host advertises the origin's own host. A service
    that names a different host, or a different port when the origin has an
    explicit one, is cross-origin. This is a description only: being
    cross-origin says nothing about whether the address is trustworthy.
    """
    if service.host is None and service.port is None:
        return False

    origin_host, origin_port = parse_origin(origin)

    if service.host is not None:
        if origin_host is None or service.host.lower() != origin_host.lower():
            return True

    if service.port is not None and origin_port is not None and service.port != origin_port:
        return True

    return False


def validate_connection_level_advertisement(origin: bytes | None) -> None:
    """
    Validate the connection-level form of an ALTSVC advertisement.

    An advertisement carried on stream 0 must name the origin it applies to;
    that origin may not be inferred from any stream.

    :raises InvalidAlternativeServiceAdvertisement: If no origin was supplied.
    """
    if not origin:
        msg = "connection-level ALTSVC advertisement requires a non-empty origin"
        raise InvalidAlternativeServiceAdvertisement(msg)


def validate_stream_level_advertisement(origin: bytes | None) -> None:
    """
    Validate the stream-level form of an ALTSVC advertisement.

    An advertisement carried on a regular stream inherits the origin of that
    stream, so it must not carry an origin of its own.

    :raises InvalidAlternativeServiceAdvertisement: If an origin was supplied.
    """
    if origin:
        msg = "stream-level ALTSVC advertisement must not carry an origin"
        raise InvalidAlternativeServiceAdvertisement(msg)


def validate_advertisement_scope(stream_id: int, origin: bytes | None) -> AlternativeServiceScope:
    """
    Validate an advertisement against the form implied by its carrying stream.

    This is the shared entry point used when reviewing an ALTSVC frame before
    it is accepted. It does not inspect trust in the origin.

    :param stream_id: The stream the ALTSVC frame was carried on. ``0`` means
        connection-level.
    :param origin: The origin field of the frame.
    :returns: The scope the advertisement is valid for.
    :raises InvalidAlternativeServiceAdvertisement: If the carrying stream and
        the origin field form an illegal combination.
    """
    if stream_id:
        validate_stream_level_advertisement(origin)
        return AlternativeServiceScope.STREAM

    validate_connection_level_advertisement(origin)
    return AlternativeServiceScope.CONNECTION
