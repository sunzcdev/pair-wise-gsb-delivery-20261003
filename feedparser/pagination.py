# Copyright 2026 Kurt McKee <contactme@kurtmckee.org>
# All rights reserved.
#
# This file is part of feedparser.
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

"""Optional pagination on top of :func:`feedparser.parse`.

The regular :func:`~feedparser.parse` function is intentionally limited to a
single resource.  The :func:`paginate` function in this module wraps it with an
iterator that follows the feed-level ``rel="next"`` links (RFC 5005) so that a
paged feed can be collected page by page.

Every design limit (maximum number of pages, maximum number of entries, and
whether cross-origin jumps are allowed) must be configured explicitly by the
caller.  The iterator never silently treats a truncated or failed crawl as a
successful one: inspect :attr:`Paginator.complete` and
:attr:`Paginator.stop_reason` after iteration ends.
"""

from __future__ import annotations

import urllib.parse
from dataclasses import dataclass, field
from typing import Any

import requests

from .api import parse
from .util import FeedParserDict

#: The feed ended without a ``rel="next"`` link.  This is the only terminal
#: state for which :attr:`Paginator.complete` is ``True``.
STOP_NO_NEXT = "no_next"
#: The configured maximum number of pages was reached.
STOP_MAX_PAGES = "max_pages"
#: The configured maximum number of entries was reached.
STOP_MAX_ENTRIES = "max_entries"
#: The next page's canonical address was already visited, forming a cycle.
STOP_CYCLE = "cycle"
#: The next page is served by another origin and cross-origin jumps are
#: disabled.
STOP_CROSS_ORIGIN = "cross_origin_blocked"
#: The feed advertised more than one distinct ``rel="next"`` address.
STOP_AMBIGUOUS_NEXT = "ambiguous_next"
#: Every advertised ``rel="next"`` address was missing or used a scheme other
#: than ``http``/``https``.
STOP_INVALID_NEXT = "invalid_next"
#: A subsequent HTTP request could not be completed.
STOP_REQUEST_FAILED = "request_failed"
#: A subsequent page could not be parsed as a feed.
STOP_PARSE_FAILED = "parse_failed"

_DEFAULT_PORTS = {"http": 80, "https": 443}


def _is_valid_http_url(url: str) -> bool:
    """Return whether ``url`` is a well-formed absolute ``http(s)`` URL.

    Next-link addresses come from untrusted feed content, so malformed inputs
    (such as a non-numeric port or a broken IPv6 literal) must be rejected
    instead of raising while the crawl is running.
    """

    try:
        parsed = urllib.parse.urlsplit(url)
        # Accessing ``hostname``/``port`` can itself raise for malformed input.
        hostname = parsed.hostname
        parsed.port
    except ValueError:
        return False
    return parsed.scheme in ("http", "https") and bool(hostname)


def _canonicalize(url: str) -> str:
    """Return a canonical form of an absolute ``http(s)`` URL.

    Equivalent spellings of the same page -- different casing of the scheme or
    host, a default port, a trailing dot on the host, an empty query, or a
    fragment -- collapse to the same canonical string, which makes cycles
    detectable even when a feed varies how it writes its own addresses.
    """

    parsed = urllib.parse.urlsplit(url)
    scheme = parsed.scheme.lower()
    host = (parsed.hostname or "").lower().rstrip(".")
    port = parsed.port
    if port is None or port == _DEFAULT_PORTS.get(scheme):
        netloc = host
    else:
        netloc = f"{host}:{port}"
    query = parsed.query if parsed.query else ""
    return urllib.parse.urlunsplit((scheme, netloc, parsed.path or "/", query, ""))


def _origin(url: str) -> tuple[str, str, int | None]:
    """Extract the ``(scheme, host, port)`` origin of an absolute URL."""

    parsed = urllib.parse.urlsplit(url)
    scheme = parsed.scheme.lower()
    host = (parsed.hostname or "").lower().rstrip(".")
    port = parsed.port
    if port is None:
        port = _DEFAULT_PORTS.get(scheme)
    return scheme, host, port


def _next_candidates(
    parsed_feed: FeedParserDict, base_url: str
) -> tuple[list[str], bool]:
    """Resolve the distinct absolute ``rel="next"`` link addresses.

    Relative addresses are resolved against the page's *final* address
    (``base_url``), which already accounts for any redirects followed while
    fetching the page.  Addresses that cannot be resolved to an absolute
    ``http(s)`` URL are discarded.  Order is preserved while duplicates are
    removed, so a feed repeating the same address is not ambiguous.

    :return: a tuple of the distinct usable candidate addresses and whether
        the feed advertised at least one ``rel="next"`` link at all.  The
        second value lets the caller tell "no next link" apart from "the next
        links were all unusable".
    """

    links = parsed_feed.get("feed", FeedParserDict()).get("links", []) or []
    resolved: list[str] = []
    seen: set[str] = set()
    saw_next_link = False
    for link in links:
        if not isinstance(link, dict) or link.get("rel") != "next":
            continue
        saw_next_link = True
        href = link.get("href")
        if not isinstance(href, str) or not href.strip():
            continue
        absolute = urllib.parse.urljoin(base_url, href.strip())
        if not _is_valid_http_url(absolute):
            continue
        if absolute not in seen:
            seen.add(absolute)
            resolved.append(absolute)
    return resolved, saw_next_link


@dataclass
class Page:
    """One fetched page and where it came from.

    :ivar entries: the entries parsed from this page.  If the entry limit was
        reached on this page, the list is truncated to the remaining budget.
    :ivar source_url: the page's final address, after redirects.  Relative
        ``rel="next"`` links on the page are resolved against this address.
    :ivar requested_url: the address that was actually requested for the page.
    :ivar number: the 1-based position of the page in the crawl.
    :ivar truncated: ``True`` when ``entries`` was shortened to honor the
        configured entry limit.
    :ivar result: the full :class:`~feedparser.util.FeedParserDict` returned by
        :func:`~feedparser.parse` for the page.
    """

    entries: list[FeedParserDict]
    source_url: str
    requested_url: str
    number: int
    truncated: bool = False
    result: FeedParserDict = field(default_factory=FeedParserDict)


class Paginator:
    """Iterate over the pages of a paged feed.

    Instances are created by :func:`paginate`; see its documentation for the
    parameters.  Iterating over the same object again restarts the crawl from
    the initial URL rather than raising ``StopIteration`` forever.

    After iteration ends, inspect :attr:`complete`, :attr:`stop_reason`,
    :attr:`error`, :attr:`pages`, and :attr:`entries` to determine what
    happened.  A crawl is only :attr:`complete` when the last fetched page had
    no usable ``rel="next"`` link; reaching a limit, detecting a cycle,
    encountering an ambiguous link, or a request/parse failure all leave
    :attr:`complete` set to ``False``.
    """

    def __init__(
        self,
        url: str,
        *,
        max_pages: int,
        max_entries: int,
        allow_cross_origin: bool,
        parse_kwargs: dict[str, Any] | None = None,
    ) -> None:
        if not isinstance(url, str):
            raise TypeError("url must be a string")
        if not _is_valid_http_url(url):
            raise ValueError("url must be an absolute http(s) URL")
        if isinstance(max_pages, bool) or not isinstance(max_pages, int):
            raise TypeError("max_pages must be an int")
        if max_pages < 1:
            raise ValueError("max_pages must be at least 1")
        if isinstance(max_entries, bool) or not isinstance(max_entries, int):
            raise TypeError("max_entries must be an int")
        if max_entries < 1:
            raise ValueError("max_entries must be at least 1")

        self._initial_url = url
        self._max_pages = max_pages
        self._max_entries = max_entries
        self._allow_cross_origin = bool(allow_cross_origin)
        self._parse_kwargs = dict(parse_kwargs or {})

        # Pages successfully fetched, in order, and the entries they yielded.
        self.pages: list[Page] = []
        self.entries: list[FeedParserDict] = []

        #: ``True`` only when the crawl ended because the last page had no
        #: usable ``rel="next"`` link.
        self.complete: bool = False
        #: Machine-readable reason the crawl stopped.  ``None`` while the
        #: iterator has not been started and one of the ``STOP_*`` constants
        #: once it has terminated.
        self.stop_reason: str | None = None
        #: The exception that caused a ``request_failed`` or
        #: ``parse_failed`` termination, otherwise ``None``.
        self.error: Exception | None = None
        #: The distinct candidate addresses advertised when termination was
        #: caused by ``ambiguous_next``, otherwise an empty list.
        self.ambiguous_candidates: list[str] = []

        self._visited: set[str] = set()
        self._next_url: str = url
        self._finished = False

    @property
    def page_count(self) -> int:
        """Number of pages fetched so far."""

        return len(self.pages)

    @property
    def entry_count(self) -> int:
        """Number of entries yielded so far."""

        return len(self.entries)

    def __iter__(self) -> Paginator:
        # Restart the crawl if the iterator is reused.
        if self.pages or self._finished:
            self._reset()
        return self

    def __next__(self) -> Page:
        page = self._advance()
        if page is None:
            raise StopIteration
        return page

    def _advance(self) -> Page | None:
        """Fetch the next page, or record a terminal state and return ``None``.

        A page that was successfully fetched is always returned, even when the
        crawl terminates because of it (for example, the entry limit was hit
        or the page carries no ``rel="next"`` link).  The recorded terminal
        state then ends the *following* :meth:`__next__` call instead of
        swallowing the current page.
        """

        if self._finished or len(self.pages) >= self._max_pages:
            if not self._finished:
                self._stop(STOP_MAX_PAGES)
            return None

        requested_url = self._next_url

        # http.get() records transport errors on the parse result rather than
        # raising them, so the request/parse distinction is decided afterwards.
        result = parse(requested_url, **self._parse_kwargs)

        final_url = result.get("href") or requested_url
        failure = self._detect_request_failure(result)
        if failure is not None:
            # Nothing usable was fetched, so no Page is recorded.
            self._stop(STOP_REQUEST_FAILED, failure)
            return None

        if self._is_parse_failure(result):
            self._stop(
                STOP_PARSE_FAILED,
                (
                    result.get("bozo_exception")
                    if isinstance(result.get("bozo_exception"), Exception)
                    else None
                ),
            )
            return None

        page_entries = list(result.get("entries", []))
        truncated = False
        remaining = self._max_entries - len(self.entries)
        if len(page_entries) > remaining:
            page_entries = page_entries[:remaining]
            truncated = True

        page = Page(
            entries=page_entries,
            source_url=final_url,
            requested_url=requested_url,
            number=len(self.pages) + 1,
            truncated=truncated,
            result=result,
        )
        self.pages.append(page)
        self.entries.extend(page_entries)
        self._visited.add(_canonicalize(final_url))

        if truncated:
            self._stop(STOP_MAX_ENTRIES)
            return page

        candidates, saw_next_link = _next_candidates(result, final_url)
        if not candidates:
            if saw_next_link:
                # The feed pointed somewhere, but every address was unusable
                # (missing href or a non-http(s) scheme).
                self._stop(STOP_INVALID_NEXT)
                return page
            self.complete = True
            self._stop(STOP_NO_NEXT)
            return page
        if len(candidates) > 1:
            self.ambiguous_candidates = candidates
            self._stop(STOP_AMBIGUOUS_NEXT)
            return page

        next_url = candidates[0]
        if not self._allow_cross_origin and _origin(next_url) != _origin(
            self._initial_url
        ):
            self._stop(STOP_CROSS_ORIGIN)
            return page
        if _canonicalize(next_url) in self._visited:
            self._stop(STOP_CYCLE)
            return page

        self._next_url = next_url
        return page

    def _reset(self) -> None:
        self.pages = []
        self.entries = []
        self.complete = False
        self.stop_reason = None
        self.error = None
        self.ambiguous_candidates = []
        self._visited = set()
        self._next_url = self._initial_url
        self._finished = False

    def _stop(self, reason: str, error: Exception | None = None) -> None:
        """Record why the crawl terminated.

        A :attr:`complete` crawl is only recorded separately, by the caller,
        for :data:`STOP_NO_NEXT`.
        """

        self.stop_reason = reason
        self.error = error
        self._finished = True

    @staticmethod
    def _detect_request_failure(
        result: FeedParserDict,
    ) -> Exception | None:
        exception = result.get("bozo_exception")
        if isinstance(exception, requests.RequestException):
            return exception
        status = result.get("status")
        if not isinstance(status, int):
            # No HTTP response was recorded, so no page was actually fetched.
            return requests.RequestException("no HTTP response was received")
        if status >= 400:
            return requests.HTTPError(f"HTTP request failed with status {status}")
        return None

    @staticmethod
    def _is_parse_failure(result: FeedParserDict) -> bool:
        # A bozo page that still produced entries or a detected feed version
        # remains usable; a bozo page with neither yielded no parseable feed.
        return bool(
            result.get("bozo")
            and not result.get("entries")
            and not result.get("version")
        )


def paginate(
    url: str,
    *,
    max_pages: int,
    max_entries: int,
    allow_cross_origin: bool,
    **parse_kwargs: Any,
) -> Paginator:
    """Create a :class:`Paginator` that follows a feed's ``rel="next"`` links.

    Unlike :func:`~feedparser.parse`, which processes a single resource, the
    returned iterator fetches a page at a time.  Each :class:`Page` exposes the
    entries parsed from that page together with the page's final address
    (its source, after redirects).

    :param url: the absolute ``http`` or ``https`` URL of the first page.
    :param max_pages: maximum number of pages to fetch.  Must be at least 1.
        Reaching the limit stops the crawl with
        :data:`~feedparser.pagination.STOP_MAX_PAGES` and leaves
        :attr:`~Paginator.complete` ``False``.
    :param max_entries: maximum number of entries to yield across all pages.
        Must be at least 1.  The page that reaches the limit is truncated, and
        the crawl stops with :data:`~feedparser.pagination.STOP_MAX_ENTRIES`.
    :param allow_cross_origin: when ``False`` (the safe default must be passed
        explicitly), a ``rel="next"`` link pointing at a different scheme,
        host, or port than ``url`` stops the crawl with
        :data:`~feedparser.pagination.STOP_CROSS_ORIGIN`.  Pass ``True`` to
        allow following links across origins.
    :param parse_kwargs: additional keyword arguments forwarded to
        :func:`~feedparser.parse` for every page (for example
        ``sanitize_html=False``).

    :raises ValueError: if ``url`` is not an absolute ``http(s)`` URL or if a
        limit is smaller than 1.
    :raises TypeError: if a limit is not an integer.
    """

    return Paginator(
        url,
        max_pages=max_pages,
        max_entries=max_entries,
        allow_cross_origin=allow_cross_origin,
        parse_kwargs=parse_kwargs,
    )
