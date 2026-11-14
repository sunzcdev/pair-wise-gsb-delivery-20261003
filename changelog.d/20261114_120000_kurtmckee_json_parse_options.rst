Fixed
-----

*   Pass the ``resolve_relative_uris`` and ``sanitize_html`` arguments
    to the JSON feed parser.

    Previously, JSON ``content_html`` was always sanitized
    and relative URIs were never resolved,
    regardless of the arguments passed to ``parse()``.

*   Keep both the ``content_text`` and ``content_html`` representations
    when a JSON feed item provides both.

    ``entry.content`` is now a list of content dicts,
    like RSS/Atom entries, with the text representation first.
    Plain text content is never treated as HTML.
