"""Tests for the optional paged-feed iterator (feedparser.paginate).

All HTTP traffic is served from the in-memory `responses` mock; no external
service or socket is required.
"""

from __future__ import annotations

import typing

import pytest
import requests
import responses

import feedparser
from feedparser import pagination as pg
from feedparser.pagination import (
    STOP_AMBIGUOUS_NEXT,
    STOP_CROSS_ORIGIN,
    STOP_CYCLE,
    STOP_INVALID_NEXT,
    STOP_MAX_ENTRIES,
    STOP_MAX_PAGES,
    STOP_NO_NEXT,
    STOP_PARSE_FAILED,
    STOP_REQUEST_FAILED,
    Paginator,
)

ATOM_TEMPLATE = """\
<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Feed {page}</title>
{links}
{entries}
</feed>"""


def atom_feed(
    page: str,
    *,
    next_links: typing.Sequence[str] = (),
    entry_titles: typing.Sequence[str] = (),
    extra_links: typing.Sequence[str] = (),
) -> bytes:
    link_tags = [f'  <link rel="next" href="{target}"/>' for target in next_links]
    link_tags.extend(extra_links)
    entry_tags = [f"  <entry><title>{title}</title></entry>" for title in entry_titles]
    return ATOM_TEMPLATE.format(
        page=page,
        links="\n".join(link_tags),
        entries="\n".join(entry_tags),
    ).encode("utf-8")


@pytest.fixture
def rsps():
    """Provide an isolated, in-memory ``responses`` mock for one test.

    The rest of the test suite drives a single session-wide ``responses``
    patcher (see ``conftest.py``).  A local :class:`responses.RequestsMock`
    keeps this module's mocks fully independent: it neither touches the shared
    patcher nor leaks registrations into other test modules, and no real
    network socket is ever opened.
    """

    with responses.RequestsMock(assert_all_requests_are_fired=False) as mock:
        yield mock


def make_paginator(url: str, **kwargs: typing.Any) -> Paginator:
    kwargs.setdefault("max_pages", 10)
    kwargs.setdefault("max_entries", 100)
    kwargs.setdefault("allow_cross_origin", False)
    return feedparser.paginate(url, **kwargs)


def collect(paginator: Paginator) -> list[pg.Page]:
    return list(paginator)


def test_module_constants_are_public():
    # The terminal-state constants must be reachable from the package root.
    for name in (
        "STOP_NO_NEXT",
        "STOP_MAX_PAGES",
        "STOP_MAX_ENTRIES",
        "STOP_CYCLE",
        "STOP_CROSS_ORIGIN",
        "STOP_AMBIGUOUS_NEXT",
        "STOP_INVALID_NEXT",
        "STOP_REQUEST_FAILED",
        "STOP_PARSE_FAILED",
        "paginate",
        "Paginator",
        "Page",
    ):
        assert hasattr(feedparser, name)


def test_walks_all_pages_until_no_next_and_reports_page_sources(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/feed/1",
        body=atom_feed(
            "1",
            next_links=("/feed/2",),
            entry_titles=("a", "b"),
        ),
        content_type="application/atom+xml",
    )
    rsps.add(
        responses.GET,
        "http://example.test/feed/2",
        body=atom_feed("2", entry_titles=("c",)),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/feed/1")
    pages = collect(paginator)

    assert paginator.complete is True
    assert paginator.stop_reason == STOP_NO_NEXT
    assert paginator.error is None
    assert [p.number for p in pages] == [1, 2]
    assert [p.source_url for p in pages] == [
        "http://example.test/feed/1",
        "http://example.test/feed/2",
    ]
    assert [p.requested_url for p in pages] == [
        "http://example.test/feed/1",
        "http://example.test/feed/2",
    ]
    assert [[e.title for e in p.entries] for p in pages] == [
        ["a", "b"],
        ["c"],
    ]
    assert [e.title for e in paginator.entries] == ["a", "b", "c"]
    assert paginator.page_count == 2
    assert paginator.entry_count == 3
    assert pages[1].result.version == "atom10"
    assert len(rsps.calls) == 2


def test_relative_next_resolved_against_final_page_url(rsps):
    # The first request is redirected; the relative next link on the final
    # page must resolve against the post-redirect address.
    rsps.add(
        responses.GET,
        "http://example.test/old",
        status=302,
        headers={"Location": "http://example.test/new/feed"},
    )
    rsps.add(
        responses.GET,
        "http://example.test/new/feed",
        body=atom_feed("1", next_links=("page-2",), entry_titles=("a",)),
        content_type="application/atom+xml",
    )
    rsps.add(
        responses.GET,
        "http://example.test/new/page-2",
        body=atom_feed("2", entry_titles=("b",)),
        content_type="application/atom+xml",
    )

    pages = collect(make_paginator("http://example.test/old"))

    assert pages[0].requested_url == "http://example.test/old"
    assert pages[0].source_url == "http://example.test/new/feed"
    assert pages[1].requested_url == "http://example.test/new/page-2"
    assert [e.title for e in pages[1].entries] == ["b"]


def test_redirect_chain_is_followed_by_the_http_layer(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/start",
        status=301,
        headers={"Location": "http://example.test/middle"},
    )
    rsps.add(
        responses.GET,
        "http://example.test/middle",
        status=302,
        headers={"Location": "http://example.test/end"},
    )
    rsps.add(
        responses.GET,
        "http://example.test/end",
        body=atom_feed("end", entry_titles=("only",)),
        content_type="application/atom+xml",
    )

    pages = collect(make_paginator("http://example.test/start"))

    assert len(pages) == 1
    assert pages[0].source_url == "http://example.test/end"
    assert [e.title for e in pages[0].entries] == ["only"]


def test_cycle_back_to_first_page_terminates_and_keeps_completed_pages(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/p1",
        body=atom_feed("1", next_links=("p2",), entry_titles=("a",)),
        content_type="application/atom+xml",
    )
    rsps.add(
        responses.GET,
        "http://example.test/p2",
        body=atom_feed("2", next_links=("p1",), entry_titles=("b",)),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/p1")
    pages = collect(paginator)

    assert [p.source_url for p in pages] == [
        "http://example.test/p1",
        "http://example.test/p2",
    ]
    assert paginator.complete is False
    assert paginator.stop_reason == STOP_CYCLE
    # The loop page is never requested a second time.
    assert len(rsps.calls) == 2
    assert [e.title for e in paginator.entries] == ["a", "b"]


def test_self_referential_next_link_terminates(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/self",
        body=atom_feed("1", next_links=("self",), entry_titles=("a",)),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/self")
    pages = collect(paginator)

    assert len(pages) == 1
    assert paginator.stop_reason == STOP_CYCLE
    assert paginator.complete is False
    assert len(rsps.calls) == 1


def test_equivalent_addresses_are_treated_as_the_same_page(rsps):
    # Page 2 links back using an equivalent spelling: explicit default port,
    # uppercase scheme/host, trailing dot, and a fragment must all collapse.
    rsps.add(
        responses.GET,
        "http://example.test/page-1",
        body=atom_feed(
            "1",
            next_links=("http://example.test/page-2",),
            entry_titles=("a",),
        ),
        content_type="application/atom+xml",
    )
    rsps.add(
        responses.GET,
        "http://example.test/page-2",
        body=atom_feed(
            "2",
            next_links=("HTTP://EXAMPLE.TEST.:80/page-1#frag",),
            entry_titles=("b",),
        ),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/page-1")
    pages = collect(paginator)

    assert len(pages) == 2
    assert paginator.stop_reason == STOP_CYCLE
    assert len(rsps.calls) == 2


def test_duplicate_next_href_is_not_ambiguous(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/dup",
        body=atom_feed(
            "1",
            next_links=("next", "next"),
            entry_titles=("a",),
        ),
        content_type="application/atom+xml",
    )
    rsps.add(
        responses.GET,
        "http://example.test/next",
        body=atom_feed("2", entry_titles=("b",)),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/dup")
    pages = collect(paginator)

    assert paginator.stop_reason == STOP_NO_NEXT
    assert paginator.complete is True
    assert len(pages) == 2


def test_multiple_distinct_next_links_are_ambiguous(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/amb",
        body=atom_feed(
            "1",
            next_links=("a", "b"),
            entry_titles=("a",),
        ),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/amb")
    pages = collect(paginator)

    assert len(pages) == 1
    assert paginator.complete is False
    assert paginator.stop_reason == STOP_AMBIGUOUS_NEXT
    assert paginator.ambiguous_candidates == [
        "http://example.test/a",
        "http://example.test/b",
    ]
    # Neither candidate may be fetched while the ambiguity is unresolved.
    assert len(rsps.calls) == 1


def test_max_pages_stops_before_the_limit_is_exceeded(rsps):
    for number in range(1, 5):
        rsps.add(
            responses.GET,
            f"http://example.test/maxp/{number}",
            body=atom_feed(
                str(number),
                next_links=(f"/maxp/{number + 1}",),
                entry_titles=(f"e{number}",),
            ),
            content_type="application/atom+xml",
        )

    paginator = make_paginator("http://example.test/maxp/1", max_pages=2)
    pages = collect(paginator)

    assert len(pages) == 2
    assert paginator.complete is False
    assert paginator.stop_reason == STOP_MAX_PAGES
    assert len(rsps.calls) == 2
    # A second iteration restarts from the first page.
    restarted = collect(paginator)
    assert len(restarted) == 2
    assert restarted[0].number == 1
    assert len(rsps.calls) == 4


def test_max_entries_truncates_final_page_and_stops(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/maxe/1",
        body=atom_feed("1", next_links=("/maxe/2",), entry_titles=("a", "b")),
        content_type="application/atom+xml",
    )
    rsps.add(
        responses.GET,
        "http://example.test/maxe/2",
        body=atom_feed("2", next_links=("/maxe/3",), entry_titles=("c", "d", "e")),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/maxe/1", max_entries=3)
    pages = collect(paginator)

    assert [e.title for e in paginator.entries] == ["a", "b", "c"]
    assert pages[0].truncated is False
    assert pages[1].truncated is True
    assert [e.title for e in pages[1].entries] == ["c"]
    assert paginator.stop_reason == STOP_MAX_ENTRIES
    assert paginator.complete is False
    # Page 3 must never be requested once the entry budget is spent.
    assert len(rsps.calls) == 2


def test_cross_origin_next_is_blocked_by_default_setting(rsps):
    rsps.add(
        responses.GET,
        "http://a.example.test/feed",
        body=atom_feed(
            "1",
            next_links=("http://b.example.test/feed",),
            entry_titles=("a",),
        ),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://a.example.test/feed", allow_cross_origin=False)
    pages = collect(paginator)

    assert len(pages) == 1
    assert paginator.complete is False
    assert paginator.stop_reason == STOP_CROSS_ORIGIN
    assert len(rsps.calls) == 1


def test_cross_origin_next_is_followed_when_explicitly_allowed(rsps):
    rsps.add(
        responses.GET,
        "http://a.example.test/feed",
        body=atom_feed(
            "1",
            next_links=("http://b.example.test/feed",),
            entry_titles=("a",),
        ),
        content_type="application/atom+xml",
    )
    rsps.add(
        responses.GET,
        "http://b.example.test/feed",
        body=atom_feed("2", entry_titles=("b",)),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://a.example.test/feed", allow_cross_origin=True)
    pages = collect(paginator)

    assert [p.source_url for p in pages] == [
        "http://a.example.test/feed",
        "http://b.example.test/feed",
    ]
    assert paginator.complete is True
    assert paginator.stop_reason == STOP_NO_NEXT


def test_port_change_is_an_origin_change(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/feed",
        body=atom_feed(
            "1",
            next_links=("http://example.test:8080/feed",),
            entry_titles=("a",),
        ),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/feed")
    collect(paginator)

    assert paginator.stop_reason == STOP_CROSS_ORIGIN


def test_connection_error_on_later_page_preserves_completed_pages(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/ok",
        body=atom_feed("1", next_links=("/broken",), entry_titles=("a",)),
        content_type="application/atom+xml",
    )
    rsps.add(
        responses.GET,
        "http://example.test/broken",
        body=requests.ConnectionError("network down"),
    )

    paginator = make_paginator("http://example.test/ok")
    pages = collect(paginator)

    assert len(pages) == 1
    assert [e.title for e in pages[0].entries] == ["a"]
    assert [e.title for e in paginator.entries] == ["a"]
    assert paginator.complete is False
    assert paginator.stop_reason == STOP_REQUEST_FAILED
    assert isinstance(paginator.error, Exception)
    assert len(rsps.calls) == 2


def test_connection_error_on_first_page_is_not_a_success(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/down",
        body=requests.ConnectionError("network down"),
    )

    paginator = make_paginator("http://example.test/down")
    pages = collect(paginator)

    assert pages == []
    assert paginator.complete is False
    assert paginator.stop_reason == STOP_REQUEST_FAILED
    assert isinstance(paginator.error, Exception)


def test_http_error_status_is_a_request_failure(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/ok",
        body=atom_feed("1", next_links=("/missing",), entry_titles=("a",)),
        content_type="application/atom+xml",
    )
    rsps.add(
        responses.GET,
        "http://example.test/missing",
        status=500,
        body=b"internal server error",
        content_type="text/plain",
    )

    paginator = make_paginator("http://example.test/ok")
    pages = collect(paginator)

    assert len(pages) == 1
    assert paginator.stop_reason == STOP_REQUEST_FAILED
    assert paginator.complete is False
    assert "500" in str(paginator.error)


def test_unparseable_later_page_is_a_parse_failure_not_a_success(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/ok",
        body=atom_feed("1", next_links=("/garbage",), entry_titles=("a",)),
        content_type="application/atom+xml",
    )
    rsps.add(
        responses.GET,
        "http://example.test/garbage",
        body=b"this is not a feed <<<>>>",
        content_type="application/xml",
    )

    paginator = make_paginator("http://example.test/ok")
    pages = collect(paginator)

    assert len(pages) == 1
    assert [e.title for e in paginator.entries] == ["a"]
    assert paginator.stop_reason == STOP_PARSE_FAILED
    assert paginator.complete is False
    assert paginator.error is not None


def test_bozo_but_usable_page_is_still_followed(rsps):
    # A malformed-but-recognizable feed keeps its entries and version, so it
    # must not be treated as a parse failure.
    rsps.add(
        responses.GET,
        "http://example.test/bozo",
        body=(
            b'<?xml version="1.0"?>'
            b'<feed xmlns="http://www.w3.org/2005/Atom">'
            b'<link rel="next" href="/end"/>'
            b"<entry><title>a</title></entry></feed>"
        ),
        content_type="text/plain",  # triggers NonXMLContentType bozo
    )
    rsps.add(
        responses.GET,
        "http://example.test/end",
        body=atom_feed("2", entry_titles=("b",)),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/bozo")
    pages = collect(paginator)

    assert len(pages) == 2
    assert paginator.complete is True
    assert paginator.stop_reason == STOP_NO_NEXT
    assert pages[0].result.bozo


def test_non_http_next_scheme_is_invalid_not_ignored(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/feed",
        body=atom_feed(
            "1",
            next_links=("mailto:nobody@example.test",),
            entry_titles=("a",),
        ),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/feed")
    pages = collect(paginator)

    assert len(pages) == 1
    assert paginator.stop_reason == STOP_INVALID_NEXT
    assert paginator.complete is False


def test_malformed_next_url_is_invalid_not_raised(rsps):
    # A non-numeric port would raise ValueError from urlsplit().port if it
    # were followed blindly; instead it must be reported as an invalid next.
    rsps.add(
        responses.GET,
        "http://example.test/feed",
        body=atom_feed(
            "1",
            next_links=("http://example.test:not-a-port/feed",),
            entry_titles=("a",),
        ),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/feed")
    pages = collect(paginator)

    assert len(pages) == 1
    assert paginator.stop_reason == STOP_INVALID_NEXT
    assert paginator.complete is False
    assert len(rsps.calls) == 1


def test_malformed_constructor_url_is_rejected():
    with pytest.raises(ValueError):
        feedparser.paginate(
            "http://example.test:not-a-port/feed",
            max_pages=1,
            max_entries=1,
            allow_cross_origin=False,
        )
    with pytest.raises(ValueError):
        feedparser.paginate(
            "http:///feed",
            max_pages=1,
            max_entries=1,
            allow_cross_origin=False,
        )
    with pytest.raises(ValueError):
        feedparser.paginate(
            "not-a-url",
            max_pages=1,
            max_entries=1,
            allow_cross_origin=False,
        )
    with pytest.raises(ValueError):
        feedparser.paginate(
            "ftp://example.test/feed",
            max_pages=1,
            max_entries=1,
            allow_cross_origin=False,
        )
    with pytest.raises(ValueError):
        make_paginator("http://example.test/f", max_pages=0)
    with pytest.raises(ValueError):
        make_paginator("http://example.test/f", max_entries=0)
    with pytest.raises(TypeError):
        make_paginator("http://example.test/f", max_pages=True)
    with pytest.raises(TypeError):
        make_paginator("http://example.test/f", max_entries=2.0)
    with pytest.raises(TypeError):
        feedparser.paginate(
            123,  # type: ignore[arg-type]
            max_pages=1,
            max_entries=1,
            allow_cross_origin=False,
        )


def test_parse_keyword_arguments_are_forwarded(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/feed",
        body=atom_feed("1", entry_titles=("a",)),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/feed", sanitize_html=False)
    pages = collect(paginator)

    assert pages[0].result.version == "atom10"
    assert paginator.complete is True


def test_iterator_protocol_and_manual_next(rsps):
    rsps.add(
        responses.GET,
        "http://example.test/feed",
        body=atom_feed("1", entry_titles=("a",)),
        content_type="application/atom+xml",
    )

    paginator = make_paginator("http://example.test/feed")
    assert paginator.stop_reason is None
    page = next(paginator)
    assert page.number == 1
    with pytest.raises(StopIteration):
        next(paginator)
    with pytest.raises(StopIteration):
        next(paginator)
    assert paginator.stop_reason == STOP_NO_NEXT
    assert paginator.complete is True


def test_canonicalization_collapses_equivalent_spellings():
    base = "http://example.test/path"
    equivalents = [
        "http://example.test/path",
        "HTTP://example.test/path",
        "http://EXAMPLE.TEST/path",
        "http://example.test./path",
        "http://example.test:80/path",
        "http://example.test/path#fragment",
        "http://example.test/path?",
    ]
    canonical = {pg._canonicalize(url) for url in equivalents}
    assert canonical == {base}

    # Distinguish changes that must not collapse.
    assert pg._canonicalize("https://example.test/path") != pg._canonicalize(base)
    assert pg._canonicalize("http://example.test:8080/path") != pg._canonicalize(base)
    assert pg._canonicalize("http://example.test/other") != pg._canonicalize(base)
    assert pg._canonicalize("http://example.test/path?x=1") != pg._canonicalize(base)
