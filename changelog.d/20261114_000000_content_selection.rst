Added
-----

*   Add ``feedparser.select_content()``, a content selection interface
    for entries that carry multiple ``content`` representations.  It
    accepts media type and language preference lists and returns a
    ``ContentSelection`` with the chosen representation and the
    selection rationale.  Media types are compared case-insensitively
    with parameters stripped; exact language matches win over
    same-family fallbacks; untagged-language and external ``src``
    representations are only used when explicitly allowed; ties are
    broken by source order.  When nothing matches, a distinguishable
    "no result" is returned instead of substituting the summary, and
    all raw representations collected by the parser are preserved.
