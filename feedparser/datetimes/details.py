# Copyright 2010-2025 Kurt McKee <contactme@kurtmckee.org>
# Copyright 2002-2008 Mark Pilgrim
# All rights reserved.
#
# This file is a part of feedparser.
#
# Redistribution and use in source and binary forms, with or without
# modification, are permitted provided that the following conditions are met:
#
# * Redistributions of source code must retain the above copyright notice,
#   this list of conditions and the following disclaimer.
# * Redistributions in binary form must reproduce the above copyright notice,
#   this list of conditions and the following disclaimer in the documentation
#   and/or other materials provided with the distribution.
#
# THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS 'AS IS'
# AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
# IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
# ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
# LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
# CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
# SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
# INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
# CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
# ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
# POSSIBILITY OF SUCH DAMAGE.

"""Record date parsing details alongside the legacy 9-tuple values.

Incremental synchronization cannot treat a date-only value like
``2026-11-14`` as a timestamp with second precision, and a value that
doesn't declare a timezone must not be normalized using the machine's
local timezone settings. This module complements the historic 9-tuple
date parsers with a detail mapping that records:

*   ``raw``: the original date text.
*   ``parsed``: the legacy UTC 9-tuple, or ``None``.
*   ``precision``: ``"date"``, ``"minute"``, ``"second"``,
    or ``"subsecond"``.
*   ``timezone_explicit``: whether a timezone was explicitly declared.
*   ``utc``: a timezone-aware ``datetime`` normalized to UTC, or ``None``.
    This is only set when a timezone was explicitly declared in the date
    text itself; it never relies on the machine's local timezone settings.
*   ``error``: the failure reason when the value cannot be parsed or
    contains invalid or out-of-range components, else ``None``.

Invalid values (like February 29 on a non-leap year) are never silently
corrected to another date in this detail mapping; the original text is
preserved in ``raw`` and the failure reason is available in ``error``.
"""

from __future__ import annotations

import datetime
import re
from time import struct_time

from ..util import FeedParserDict

PRECISION_DATE = "date"
PRECISION_MINUTE = "minute"
PRECISION_SECOND = "second"
PRECISION_SUBSECOND = "subsecond"

# A time of day, with optional seconds and optional fractional seconds.
_TIME_RE = re.compile(
    r"(?P<hour>\d{1,2}):(?P<minute>\d{2})"
    r"(?::(?P<second>\d{2})(?P<fraction>[.,]\d+)?)?"
)

_MONTH_NAMES = {
    "jan": 1,
    "feb": 2,
    "mar": 3,
    "apr": 4,
    "may": 5,
    "jun": 6,
    "jul": 7,
    "aug": 8,
    "sep": 9,
    "oct": 10,
    "nov": 11,
    "dec": 12,
}

# Date component patterns used to validate that the values in the original
# text represent a real calendar date. The first pattern that matches the
# text is used; out-of-range values are reported, never auto-corrected.
_DATE_PATTERNS = (
    # "2026-11-14", "2026/11/14"
    re.compile(r"(?P<year>\d{4})[-/](?P<month>\d{1,2})[-/](?P<day>\d{1,2})(?!\d)"),
    # "20261114"
    re.compile(r"(?<![\d-])(?P<year>\d{4})(?P<month>\d{2})(?P<day>\d{2})(?!\d)"),
    # "2026-318", "2026318" (ordinal day of year)
    re.compile(r"(?<![\d-])(?P<year>\d{4})-?(?P<ordinal>\d{3})(?!\d)"),
    # "26-11-14", "261114" (two-digit year, current century per ISO 8601)
    re.compile(r"(?<![\d-])(?P<year2>\d{2})-?(?P<month>\d{2})-?(?P<day>\d{2})(?!\d)"),
    # "2026-11" (year and month only)
    re.compile(r"(?<![\d-])(?P<year>\d{4})-(?P<month>\d{2})(?!\d)"),
    # "14 Nov 2026", "14 November 2026"
    re.compile(
        r"(?<![\d:])(?P<day>\d{1,2})\s+"
        r"(?P<month_name>[a-z]{3,})\s+"
        r"(?P<year>\d{2,4})(?![\d:])",
        re.IGNORECASE,
    ),
)

_TIMEZONE_NAMES = (
    "ut",
    "utc",
    "gmt",
    "adt",
    "ast",
    "at",
    "edt",
    "est",
    "et",
    "cdt",
    "cst",
    "ct",
    "mdt",
    "mst",
    "mt",
    "pdt",
    "pst",
    "pt",
    "met",
    "mest",
    "z",
)
_TIMEZONE_NAMES_RE = "|".join(_TIMEZONE_NAMES)

_TIMEZONE_PATTERNS = (
    # A numeric offset at the end of the text: "+0000", "-05:00".
    re.compile(r"[+-]\d{2}:?\d{2}(?::?\d{2})?\s*$"),
    # A numeric offset before a trailing year (asctime-style): "+0000 2004".
    re.compile(r"[+-]\d{2}:?\d{2}\s+\d{4}\s*$"),
    # A "Z" immediately after the time: "2003-12-31T10:14:55Z".
    re.compile(r"(?<=\d)z\s*$", re.IGNORECASE),
    # A named timezone at the end of the text, like "GMT" or "EST",
    # possibly followed by a numeric offset, like "GMT-05:00".
    re.compile(
        r"(?:^|\s)(?:etc/)?(?:" + _TIMEZONE_NAMES_RE + r")(?:[+-]\d{2}:?\d{2})?\s*$",
        re.IGNORECASE,
    ),
    # A named timezone before a trailing year (asctime-style): "PST 2004".
    re.compile(
        r"(?:^|\s)(?:" + _TIMEZONE_NAMES_RE + r")\s+\d{4}\s*$",
        re.IGNORECASE,
    ),
)


def _detect_precision(text: str) -> str:
    """Detect the precision of the time of day declared in the text."""

    match = _TIME_RE.search(text)
    if match is None:
        return PRECISION_DATE
    if match.group("fraction"):
        return PRECISION_SUBSECOND
    if match.group("second") is not None:
        return PRECISION_SECOND
    return PRECISION_MINUTE


def _detect_timezone(text: str) -> bool:
    """Detect whether the text explicitly declares a timezone."""

    return any(pattern.search(text) for pattern in _TIMEZONE_PATTERNS)


def _validate_date_match(match: re.Match) -> str | None:
    """Validate date components extracted from the original text."""

    groups = match.groupdict()
    if groups.get("ordinal"):
        year = int(groups["year"])
        ordinal = int(groups["ordinal"])
        try:
            result = datetime.date(year, 1, 1) + datetime.timedelta(days=ordinal - 1)
        except (OverflowError, ValueError):
            return f"the ordinal day {ordinal} is out of range for year {year}"
        if ordinal < 1 or result.year != year:
            return f"the ordinal day {ordinal} is out of range for year {year}"
        return None

    if groups.get("year2") is not None:
        # Two-digit years use the current century, per ISO 8601.
        current_year = datetime.datetime.now(datetime.timezone.utc).year
        year = current_year // 100 * 100 + int(groups["year2"])
    else:
        year = int(groups["year"])

    if groups.get("month_name"):
        month = _MONTH_NAMES.get(groups["month_name"][:3].lower())
        if month is None:
            # The month name isn't recognized, so it can't be validated here.
            return None
    else:
        month = int(groups["month"])

    day = int(groups.get("day") or 1)
    try:
        datetime.date(year, month, day)
    except ValueError as error:
        return f"invalid date components: {error}"
    return None


def _find_component_error(text: str) -> str | None:
    """Find invalid date or time components in the original text.

    Values like February 29 on a non-leap year, month 13, or hour 25 are
    reported here so they are never silently normalized into another date.
    """

    for pattern in _DATE_PATTERNS:
        match = pattern.search(text)
        if match is None:
            continue
        error = _validate_date_match(match)
        if error is not None:
            return error
        break

    time_match = _TIME_RE.search(text)
    if time_match is not None:
        hour = int(time_match.group("hour"))
        minute = int(time_match.group("minute"))
        second = time_match.group("second")
        second = int(second) if second is not None else 0
        if hour > 23 or minute > 59 or second > 59:
            return f"the time of day {time_match.group(0)!r} is out of range"
    return None


def build_date_details(
    date_string: str | None, date9tuple: struct_time | tuple | None
) -> FeedParserDict:
    """Build a detail mapping for a date string and its legacy 9-tuple.

    :param date_string: the original date text.
    :param date9tuple: the result of the legacy 9-tuple date parsers.
    :return: a :class:`FeedParserDict` with the keys ``raw``, ``parsed``,
        ``precision``, ``timezone_explicit``, ``utc``, and ``error``.
    """

    details = FeedParserDict()
    details["raw"] = date_string
    details["parsed"] = date9tuple
    details["precision"] = None
    details["timezone_explicit"] = False
    details["utc"] = None
    details["error"] = None

    if not date_string or not str(date_string).strip():
        details["error"] = "the date value is empty"
        return details

    text = str(date_string).strip()
    precision = _detect_precision(text)
    timezone_explicit = _detect_timezone(text)
    details["precision"] = precision
    details["timezone_explicit"] = timezone_explicit

    error = _find_component_error(text)
    if error is None and date9tuple is None:
        error = "the date value was not recognized"
    if error is not None:
        details["error"] = error
        return details

    # Only normalize to UTC when the timezone was explicitly declared.
    # A missing timezone is never filled in from the machine's local
    # timezone settings.
    if timezone_explicit:
        try:
            utc = datetime.datetime(*date9tuple[:6], tzinfo=datetime.timezone.utc)
        except (OverflowError, ValueError):
            details["error"] = "the date is outside the supported range"
            return details
        if precision == PRECISION_SUBSECOND:
            fraction = _TIME_RE.search(text).group("fraction")
            digits = fraction[1:]  # Strip the leading "." or ",".
            utc = utc.replace(microsecond=int(digits[:6].ljust(6, "0")))
        details["utc"] = utc

    return details
