.. _reference.content_selection:

Selecting among multiple content representations
================================================

Entries may contain several ``content`` representations (see
:ref:`reference.entry.content`).  Instead of hand-rolling selection
rules, call :func:`feedparser.select_content` with a media type
preference list and a language preference list::

    import feedparser

    entry = feedparser.parse(url).entries[0]
    selection = feedparser.select_content(
        entry,
        media_types=["text/html", "text/plain"],
        languages=["zh-CN", "zh"],
    )
    if selection:
        print(selection.value)
    else:
        print(selection.reason)

The selection rules are:

*   Media types are compared after normalization: lowercased, with
    parameters (``; charset=...``) stripped, and the shorthand aliases
    ``text``, ``html``, and ``xhtml`` mapped to their canonical types.
*   Exact language matches win over same-language-family fallbacks
    (``zh`` matches ``zh-CN`` and vice versa).
*   Representations without a language tag are only used as a
    last-resort fallback when ``allow_untagged_language=True``.
*   Representations that only carry an external ``src`` pointer (no
    inline body) are excluded unless ``allow_external=True``.
    ``select_content`` never fetches external content from the network.
*   Ties are broken by source order: the earliest representation in
    ``entries[i].content`` wins.
*   When no representation is usable, the result is a distinguishable
    "no result" (``selection.found`` is false and ``selection.reason``
    explains why).  The entry's ``summary`` is never substituted for
    ``content``.

The parsed entry is never modified.  Every raw representation collected
by the parser remains available in source order as
``selection.representations``.

The returned :class:`feedparser.ContentSelection` exposes:

``found``
    Whether a representation was selected.

``reason``
    A machine-readable string: ``"selected"``, ``"no-representations"``,
    ``"no-matching-media-type"``, ``"no-matching-language"``, or
    ``"external-only"``.

``representation``
    The selected content dictionary, or ``None``.

``value`` / ``src`` / ``media_type`` / ``language``
    Convenience attributes describing the selected representation.

``index``
    The position of the selected representation in
    ``entries[i].content``.

``language_match``
    ``"exact"``, ``"family"``, ``"untagged"``, or ``None``.

``external``
    Whether the selected representation only points to external content.

``representations``
    All raw representations, in source order.
