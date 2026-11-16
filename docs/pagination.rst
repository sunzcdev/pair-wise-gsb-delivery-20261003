.. _advanced.pagination:

Paged Feeds (``feedparser.paginate``)
=====================================

:func:`feedparser.parse` intentionally processes a single resource at a time.
Feeds that split their history across several pages can additionally be
collected with :func:`feedparser.paginate`, an optional iterator built on top
of :func:`~feedparser.parse`.

Each page of an Atom feed may advertise the address of the following page
with a feed-level ``<link rel="next">`` element, as defined by
`RFC 5005 <https://www.rfc-editor.org/rfc/rfc5005>`_.
``feedparser.paginate`` follows those links one page at a time and yields a
:class:`~feedparser.pagination.Page` for every page it fetches.  Every
``Page`` exposes the entries parsed from that page together with the page's
final address (its source, after any redirects).

..  code-block:: pycon

    >>> import feedparser
    >>> paginator = feedparser.paginate(
    ...     "https://example.com/feed",
    ...     max_pages=10,
    ...     max_entries=100,
    ...     allow_cross_origin=False,
    ... )
    >>> for page in paginator:
    ...     print(page.number, page.source_url, len(page.entries))
    1 https://example.com/feed 10
    2 https://example.com/feed?page=2 10
    3 https://example.com/feed?page=3 4
    >>> paginator.complete
    True

The three limits are always required as keyword arguments; there are no
implicit defaults:

*   ``max_pages`` caps how many pages may be fetched.
*   ``max_entries`` caps how many entries are yielded in total.  The page
    that reaches the limit is truncated, and its
    :attr:`~feedparser.pagination.Page.truncated` attribute is ``True``.
*   ``allow_cross_origin`` decides whether a ``rel="next"`` link pointing at
    a different scheme, host, or port than the initial :abbr:`URL (Uniform
    Resource Locator)` may be followed.  Pass ``False`` to keep the crawl on
    the original origin.

Any extra keyword arguments are forwarded to :func:`~feedparser.parse` for
every page, for example ``sanitize_html=False``.


Relative links, redirects, and cycles
-------------------------------------

Relative ``rel="next"`` addresses are resolved against the *final* address of
the page that contains them, so redirects are followed correctly before the
next address is computed.

Equivalent spellings of the same page -- an explicit default port, different
casing of the scheme or host, a trailing dot on the host, or a fragment --
are treated as the same page.  Following a link back to an already-visited
page therefore terminates the crawl instead of looping forever, with
:data:`~feedparser.pagination.STOP_CYCLE` recorded on
:attr:`~feedparser.pagination.Paginator.stop_reason`.

A feed that advertises more than one *distinct* ``rel="next"`` address is
ambiguous: the crawl stops with
:data:`~feedparser.pagination.STOP_AMBIGUOUS_NEXT`, and the candidate
addresses are available on
:attr:`~feedparser.pagination.Paginator.ambiguous_candidates`.  Repeating the
same address is not ambiguous.


Terminal states
---------------

A crawl is only reported as :attr:`~feedparser.pagination.Paginator.complete`
when the last fetched page had no usable ``rel="next"`` link.  Reaching a
limit, detecting a cycle or an ambiguous link, or a request/parse failure all
leave ``complete`` set to ``False``; inspect
:attr:`~feedparser.pagination.Paginator.stop_reason` for the machine-readable
reason and, for failures, :attr:`~feedparser.pagination.Paginator.error`.

..  code-block:: pycon

    >>> paginator = feedparser.paginate(
    ...     "https://example.com/feed",
    ...     max_pages=2,
    ...     max_entries=100,
    ...     allow_cross_origin=False,
    ... )
    >>> pages = list(paginator)
    >>> paginator.complete
    False
    >>> paginator.stop_reason == feedparser.STOP_MAX_PAGES
    True

The available terminal states are:

*   :data:`~feedparser.pagination.STOP_NO_NEXT` -- the feed ended without a
    usable next link (the only state for which ``complete`` is ``True``).
*   :data:`~feedparser.pagination.STOP_MAX_PAGES` -- the page limit was
    reached.
*   :data:`~feedparser.pagination.STOP_MAX_ENTRIES` -- the entry limit was
    reached.
*   :data:`~feedparser.pagination.STOP_CYCLE` -- the next page was already
    visited.
*   :data:`~feedparser.pagination.STOP_CROSS_ORIGIN` -- the next page is on
    another origin and cross-origin jumps are disabled.
*   :data:`~feedparser.pagination.STOP_AMBIGUOUS_NEXT` -- several distinct
    next links were advertised.
*   :data:`~feedparser.pagination.STOP_INVALID_NEXT` -- every advertised next
    link was missing or not an absolute ``http(s)`` address.
*   :data:`~feedparser.pagination.STOP_REQUEST_FAILED` -- a subsequent
    :abbr:`HTTP (Hypertext Transfer Protocol)` request could not be
    completed.
*   :data:`~feedparser.pagination.STOP_PARSE_FAILED` -- a subsequent page
    could not be parsed as a feed.

When a later page cannot be fetched or parsed, the pages that were already
completed stay available on
:attr:`~feedparser.pagination.Paginator.pages` and their entries on
:attr:`~feedparser.pagination.Paginator.entries`; the crawl is still marked
incomplete and must not be treated as a successful end state.
