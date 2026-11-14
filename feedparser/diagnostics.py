# Copyright 2010-2025 Kurt McKee <contactme@kurtmckee.org>
# All rights reserved.
#
# This file is a part of feedparser.
#
# Redistribution and use in source and binary forms, with or without modification,
# are permitted provided that the following conditions are met:
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

"""Stable, ordered diagnostics for feed parsing.

The legacy ``bozo_exception`` attribute can only describe one problem.  This
module records every problem encountered while fetching, decoding, strictly
parsing, and leniently recovering a feed.  Diagnostics are plain dictionaries
so callers can serialize them, and they never retain exceptions, streams, or
parser objects.
"""

from __future__ import annotations

import typing

PHASE_FETCH = "fetch"
PHASE_ENCODING = "encoding"
PHASE_STRICT = "strict"
PHASE_LOOSE = "loose"
PHASE_JSON = "json"

_STAGE_NAMES = {
    PHASE_FETCH: "fetch",
    PHASE_ENCODING: "encoding",
    PHASE_STRICT: "strict_parse",
    PHASE_LOOSE: "lenient_recovery",
    PHASE_JSON: "json_parse",
}

# Keep descriptions useful without allowing a document-sized payload through.
_MAX_MESSAGE_LENGTH = 500


def _clean_message(value: typing.Any) -> str:
    message = " ".join(str(value).split())
    if len(message) > _MAX_MESSAGE_LENGTH:
        message = message[: _MAX_MESSAGE_LENGTH - 3] + "..."
    return message


def _exception_message(exception: typing.Any) -> str:
    get_message = getattr(exception, "getMessage", None)
    if callable(get_message):
        try:
            message = get_message()
        except Exception:
            message = None
        if message:
            return _clean_message(message)

    try:
        message = str(exception)
    except Exception:
        message = ""

    if not message:
        message = type(exception).__name__
    return _clean_message(message)


def _position(exception: typing.Any, getter_name: str) -> int | None:
    getter = getattr(exception, getter_name, None)
    if not callable(getter):
        return None
    try:
        value = getter()
    except Exception:
        return None
    if isinstance(value, int) and value > 0:
        return value
    return None


def record_diagnostic(
    result: dict[str, typing.Any],
    phase: str,
    exception: typing.Any,
) -> dict[str, typing.Any]:
    """Append one stable diagnostic, unless an equivalent one already exists."""

    exception_name = type(exception).__name__
    diagnostic = {
        "phase": phase,
        "stage": _STAGE_NAMES.get(phase, phase),
        "exception": exception_name,
        "type": exception_name,
        "message": _exception_message(exception),
        "line": _position(exception, "getLineNumber"),
        "column": _position(exception, "getColumnNumber"),
    }

    diagnostics = result.get("bozo_diagnostics")
    if diagnostics is None:
        diagnostics = []
        result["bozo_diagnostics"] = diagnostics

    key = (
        diagnostic["phase"],
        diagnostic["exception"],
        diagnostic["message"],
        diagnostic["line"],
        diagnostic["column"],
    )
    for existing in diagnostics:
        existing_key = (
            existing.get("phase"),
            existing.get("exception"),
            existing.get("message"),
            existing.get("line"),
            existing.get("column"),
        )
        if existing_key == key:
            return existing

    diagnostics.append(diagnostic)
    return diagnostic


def clear_phase_diagnostics(
    result: dict[str, typing.Any],
    phase: str,
) -> None:
    """Remove diagnostics from a phase before retrying that phase."""

    diagnostics = result.get("bozo_diagnostics")
    if not diagnostics:
        return

    remaining = [
        diagnostic for diagnostic in diagnostics if diagnostic.get("phase") != phase
    ]
    if remaining:
        result["bozo_diagnostics"] = remaining
    else:
        result.pop("bozo_diagnostics", None)
