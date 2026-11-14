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

from __future__ import annotations

import calendar
import copy
import datetime
import email.utils
import time
import typing

import requests

from .datetimes import _parse_date
from .util import FeedParserDict

# HTTP "Accept" header to send to servers when downloading feeds.
ACCEPT_HEADER: str = (
    "application/atom+xml"
    ",application/rdf+xml"
    ",application/rss+xml"
    ",application/x-netcdf"
    ",application/xml"
    ";q=0.9,text/xml"
    ";q=0.2,*/*"
    ";q=0.1"
)


def _format_modified(
    modified: str | datetime.datetime | time.struct_time | tuple[int, ...],
) -> str:
    """Format a Last-Modified value for use in an HTTP request header."""

    if isinstance(modified, str):
        return modified

    if isinstance(modified, datetime.datetime):
        if modified.tzinfo is None:
            modified = modified.replace(tzinfo=datetime.timezone.utc)
        modified = modified.astimezone(datetime.timezone.utc)
        return email.utils.format_datetime(modified, usegmt=True)

    # time.struct_time is a tuple subclass. All values in it are GMT.
    return email.utils.formatdate(calendar.timegm(tuple(modified)[:9]), usegmt=True)


def _previous_etag(result) -> str | None:
    if not result:
        return None
    etag = result.get("etag")
    if etag:
        return etag
    headers = result.get("headers") or {}
    return headers.get("etag") or None


def _previous_modified(result):
    if not result:
        return None
    modified = result.get("modified")
    if modified:
        return modified
    headers = result.get("headers") or {}
    return headers.get("last-modified") or None


def _store_modified(result, modified, *, parsed=None):
    if not modified:
        return
    if not isinstance(modified, str):
        modified = _format_modified(modified)
    result["modified"] = modified
    result["modified_parsed"] = parsed if parsed is not None else _parse_date(modified)


def _preserve_validators(result, previous_result, etag, modified):
    if not result.get("etag"):
        etag = etag or _previous_etag(previous_result)
        if etag:
            result["etag"] = etag

    if not result.get("modified"):
        modified = modified or _previous_modified(previous_result)
        if modified:
            parsed = None
            if previous_result and _previous_modified(previous_result) == modified:
                parsed = previous_result.get("modified_parsed")
            _store_modified(result, modified, parsed=parsed)


def _merge_not_modified(result, previous_result, etag, modified):
    if previous_result is None:
        # The result may already contain values from a previous parse,
        # even if a separate previous_result object was not supplied.
        merged = copy.deepcopy(result)
        merged.setdefault("bozo", False)
        merged.setdefault("entries", [])
        merged.setdefault("feed", FeedParserDict())
    else:
        # The caller may keep the previous result around for the next request.
        merged = copy.deepcopy(previous_result)

    previous_headers = merged.get("headers") or {}
    current_headers = dict(result.get("headers") or {})
    for name in ("etag", "last-modified"):
        if name not in current_headers and name in previous_headers:
            current_headers[name] = previous_headers[name]
    merged["headers"] = current_headers

    if "href" in result:
        merged["href"] = result["href"]
    merged["status"] = 304

    if result.get("etag"):
        merged["etag"] = result["etag"]
    if result.get("modified"):
        _store_modified(
            merged,
            result["modified"],
            parsed=result.get("modified_parsed"),
        )
    _preserve_validators(merged, previous_result, etag, modified)
    if merged.get("modified") and not merged.get("modified_parsed"):
        _store_modified(merged, merged.get("modified"))

    merged["not_modified"] = True
    merged["bozo"] = False
    merged.pop("bozo_exception", None)
    merged.setdefault("version", "")
    merged["debug_message"] = (
        "The feed has not changed since you last checked, "
        "so the server sent no data.  This is a feature, not a bug!"
    )

    result.clear()
    result.update(merged)


def get(
    url: str,
    result: dict[str, typing.Any],
    etag: str | None = None,
    modified: (
        str | datetime.datetime | time.struct_time | tuple[int, ...] | None
    ) = None,
    previous_result: dict[str, typing.Any] | None = None,
) -> bytes:
    if etag is None:
        etag = _previous_etag(previous_result)
    if modified is None:
        modified = _previous_modified(previous_result)

    headers = {"Accept": ACCEPT_HEADER}
    if etag:
        # ETags must be sent verbatim. In particular, weak ETags (W/"...")
        # must not be normalized into strong ETags.
        headers["If-None-Match"] = etag
    if modified:
        headers["If-Modified-Since"] = _format_modified(modified)

    try:
        response = requests.get(
            url,
            headers=headers,
            timeout=10,
        )
    except requests.RequestException as exception:
        result["bozo"] = True
        result["bozo_exception"] = exception
        return b""

    # Lowercase the HTTP header keys for comparisons per RFC 2616.
    result["headers"] = {k.lower(): v for k, v in response.headers.items()}

    # save HTTP headers
    if "etag" in result["headers"]:
        result["etag"] = result["headers"]["etag"]
    if "last-modified" in result["headers"]:
        modified = result["headers"]["last-modified"]
        if modified:
            result["modified"] = modified
            result["modified_parsed"] = _parse_date(modified)
    result["href"] = response.url
    result["status"] = response.status_code

    if response.status_code == 304:
        _merge_not_modified(result, previous_result, etag, modified)
    else:
        _preserve_validators(result, previous_result, etag, modified)

    return response.content
