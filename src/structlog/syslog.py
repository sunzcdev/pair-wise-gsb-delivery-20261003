# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

"""
Render log events as `RFC 5424`_ syslog records.

This module only *encodes* events into ready-to-send records.  It never
opens network connections -- hand the returned bytes to the transport of
your choice (for example :rfc:`5426` UDP or :rfc:`6587` TCP).

.. _RFC 5424: https://www.rfc-editor.org/rfc/rfc5424
"""

from __future__ import annotations


__lazy_modules__ = ["datetime", "os", "socket"]

import datetime as dt
import os
import socket

from typing import Any

from .typing import EventDict, WrappedLogger


__all__ = ["RFC5424Renderer"]


NILVALUE = "-"
"""
The RFC 5424 NILVALUE: a single dash marks a field as absent.
"""

_SYSLOG_VERSION = "1"

#: Maps common log level names to RFC 5424 severities.
_LEVEL_TO_SEVERITY: dict[str, int] = {
    "debug": 7,
    "info": 6,
    "notice": 5,
    "warning": 4,
    "warn": 4,
    "error": 3,
    "err": 3,
    "critical": 2,
    "fatal": 2,
    "alert": 1,
    "emergency": 0,
    "panic": 0,
}

#: Event dict keys that are consumed by the header / message and therefore
#: never become structured data parameters.
_RESERVED_KEYS = frozenset({
    "event",
    "timestamp",
    "hostname",
    "app_name",
    "procid",
    "msgid",
    "facility",
    "severity",
    "level",
})

#: Maximum lengths for header fields, per RFC 5424 section 6.2.
_MAX_LENGTHS = {
    "hostname": 255,
    "app_name": 48,
    "procid": 128,
    "msgid": 32,
    "sd_id": 32,
    "param_name": 32,
}


def _is_printusascii(value: str) -> bool:
    """PRINTUSASCII: printable ASCII characters, %d33-126."""
    return all(33 <= ord(c) <= 126 for c in value)


def _check_header_field(name: str, value: str) -> str:
    """
    Validate an RFC 5424 header field value.

    The NILVALUE and missing values are returned as the NILVALUE; anything
    else must be non-empty PRINTUSASCII within its length limit.
    """
    if value == NILVALUE:
        return NILVALUE

    limit = _MAX_LENGTHS[name]
    if not value or len(value) > limit or not _is_printusascii(value):
        msg = (
            f"Invalid RFC 5424 {name.upper()}: {value!r} "
            f"(must be 1-{limit} printable ASCII characters, "
            "no spaces or control characters)"
        )
        raise ValueError(msg)

    return value


def _check_sd_name(name: str, value: str) -> str:
    """
    Validate an SD-ID or PARAM-NAME: PRINTUSASCII except SP, ``"``, ``]``,
    and ``\\``, at most 32 characters.
    """
    if (
        not value
        or len(value) > _MAX_LENGTHS[name]
        or not _is_printusascii(value)
        or any(c in value for c in '"]\\')
    ):
        msg = (
            f"Invalid RFC 5424 {name}: {value!r} "
            '(must be 1-32 printable ASCII characters, none of SP " ] \\)'
        )
        raise ValueError(msg)

    return value


def _escape_param_value(value: str) -> str:
    """Escape ``\\``, ``"``, and ``]`` inside a PARAM-VALUE."""
    return (
        value.replace("\\", "\\\\").replace('"', '\\"').replace("]", "\\]")
    )


def _format_timestamp(value: Any) -> str:
    if value is None:
        return NILVALUE

    if isinstance(value, dt.datetime):
        if value.tzinfo is None:
            # RFC 5424 requires a known reference frame; assume UTC.
            value = value.replace(tzinfo=dt.timezone.utc)
        formatted = value.isoformat()
        if formatted.endswith("+00:00"):
            formatted = formatted[:-6] + "Z"

        return formatted

    if isinstance(value, (int, float)):
        return (
            dt.datetime.fromtimestamp(value, tz=dt.timezone.utc)
            .isoformat()
            .replace("+00:00", "Z")
        )

    value = str(value)
    if value == NILVALUE:
        return NILVALUE

    if not value or not _is_printusascii(value):
        msg = f"Invalid RFC 5424 TIMESTAMP: {value!r}"
        raise ValueError(msg)

    return value


class RFC5424Renderer:
    r"""
    Render *event_dict* as an `RFC 5424`_ syslog record.

    The result is a ``bytes`` object, encoded in UTF-8, that can be sent to
    a syslog collector as-is.  Only encoding happens here -- no network
    connections are opened and the passed *event_dict* is never modified.

    The header fields are taken from the following event dict keys, falling
    back to the defaults passed to the constructor, and finally to the
    NILVALUE ``-``:

    - ``timestamp``: A `datetime.datetime`, a POSIX timestamp, or an
      RFC 3339 string.  Naive datetimes are assumed to be UTC.
    - ``hostname``, ``app_name``, ``procid``, ``msgid``: Printable ASCII
      strings (see :rfc:`5424` section 6.2 for the exact grammars).
    - ``facility`` / ``severity``: Integers that make up the PRIORITY
      value.  If no severity is given, it is derived from the ``level``
      key (e.g. ``"warning"`` becomes 4) and defaults to 6 (informational).

    The ``event`` key becomes the MSG part.  All remaining keys become
    PARAMs of one SD-ELEMENT whose SD-ID is *sd_id*; ``\``, ``"``, and
    ``]`` in parameter values are escaped as required by the RFC.

    Args:
        facility: Default syslog facility (0-23).  Default: 1 (user).

        severity:
            Default syslog severity (0-7).  If `None`, it is derived from
            the event's ``level`` key, defaulting to 6 (informational).

        hostname: Default HOSTNAME.  Default: `socket.gethostname()`.

        app_name: Default APP-NAME.  Default: NILVALUE.

        procid: Default PROCID.  Default: the current process ID.

        msgid: Default MSGID.  Default: NILVALUE.

        sd_id: SD-ID of the SD-ELEMENT holding the remaining event dict
            keys.  Default: ``"structlog"``.

        include_bom:
            Prefix a Unicode message with the byte order mark ``\ufeff``
            as mandated by RFC 5424 section 6.4 for UTF-8-encoded MSG.
            Default: `True`.

        octet_counting:
            Frame the record using :rfc:`6587` octet counting, i.e. prefix
            it with the exact byte length of the record.  This allows
            receivers to reassemble records unambiguously even if the
            message contains newlines.  The length is computed from the
            final encoded bytes.  Default: `False`.

            If `False`, newlines within the message are escaped as
            ``\n`` so one event always occupies exactly one line.

    Raises:
        ValueError: If a header field, the SD-ID, or a structured data
            parameter name violates the RFC 5424 grammar.

    .. versionadded:: 26.2.0

    .. _RFC 5424: https://www.rfc-editor.org/rfc/rfc5424
    """

    def __init__(
        self,
        *,
        facility: int = 1,
        severity: int | None = None,
        hostname: str | None = None,
        app_name: str | None = None,
        procid: str | int | None = None,
        msgid: str | None = None,
        sd_id: str = "structlog",
        include_bom: bool = True,
        octet_counting: bool = False,
    ):
        if (
            not isinstance(facility, int)
            or isinstance(facility, bool)
            or not 0 <= facility <= 23
        ):
            msg = f"Invalid RFC 5424 facility: {facility!r} (must be 0-23)"
            raise ValueError(msg)

        if severity is not None and (
            not isinstance(severity, int)
            or isinstance(severity, bool)
            or not 0 <= severity <= 7
        ):
            msg = f"Invalid RFC 5424 severity: {severity!r} (must be 0-7)"
            raise ValueError(msg)

        if hostname is None:
            hostname = socket.gethostname()
        if procid is None:
            procid = os.getpid()

        self._facility = facility
        self._severity = severity
        self._hostname = _check_header_field("hostname", str(hostname))
        self._app_name = (
            NILVALUE
            if app_name is None
            else _check_header_field("app_name", str(app_name))
        )
        self._procid = _check_header_field("procid", str(procid))
        self._msgid = (
            NILVALUE
            if msgid is None
            else _check_header_field("msgid", str(msgid))
        )
        self._sd_id = _check_sd_name("sd_id", sd_id)
        self._include_bom = include_bom
        self._octet_counting = octet_counting

    def __call__(
        self, _: WrappedLogger, __: str, event_dict: EventDict
    ) -> bytes:
        facility = event_dict.get("facility", self._facility)
        if not isinstance(facility, int) or not 0 <= facility <= 23:
            msg = f"Invalid RFC 5424 facility: {facility!r} (must be 0-23)"
            raise ValueError(msg)

        severity = event_dict.get("severity", self._severity)
        if severity is None:
            severity = _LEVEL_TO_SEVERITY.get(
                str(event_dict.get("level", "")).lower(), 6
            )
        if not isinstance(severity, int) or not 0 <= severity <= 7:
            msg = f"Invalid RFC 5424 severity: {severity!r} (must be 0-7)"
            raise ValueError(msg)

        timestamp = _format_timestamp(event_dict.get("timestamp"))

        hostname = event_dict.get("hostname")
        hostname = (
            self._hostname
            if hostname is None
            else _check_header_field("hostname", str(hostname))
        )
        app_name = event_dict.get("app_name")
        app_name = (
            self._app_name
            if app_name is None
            else _check_header_field("app_name", str(app_name))
        )
        procid = event_dict.get("procid")
        procid = (
            self._procid
            if procid is None
            else _check_header_field("procid", str(procid))
        )
        msgid = event_dict.get("msgid")
        msgid = (
            self._msgid
            if msgid is None
            else _check_header_field("msgid", str(msgid))
        )

        header = " ".join(
            (
                f"<{facility * 8 + severity}>{_SYSLOG_VERSION}",
                timestamp,
                hostname,
                app_name,
                procid,
                msgid,
            )
        )

        sd_params = []
        for key, value in event_dict.items():
            if key in _RESERVED_KEYS:
                continue
            _check_sd_name("param_name", key)
            sd_params.append(
                f'{key}="{_escape_param_value(str(value))}"'
            )

        if sd_params:
            structured_data = f"[{self._sd_id} {' '.join(sd_params)}]"
        else:
            structured_data = NILVALUE

        record = f"{header} {structured_data}"

        event = event_dict.get("event")
        if event is not None:
            message = str(event)
            if message:
                if not self._octet_counting:
                    # Keep one event on one line for LF-delimited
                    # transports; octet-counted framing doesn't need this.
                    message = message.replace("\r", "\\r").replace(
                        "\n", "\\n"
                    )
                if self._include_bom:
                    message = "\ufeff" + message
                record = f"{record} {message}"

        data = record.encode("utf-8")

        if self._octet_counting:
            # RFC 6587 section 3.4.1: the length of the final record in
            # octets, so receivers don't rely on (possibly forged)
            # newlines to find record boundaries.
            data = str(len(data)).encode("ascii") + b" " + data

        return data
