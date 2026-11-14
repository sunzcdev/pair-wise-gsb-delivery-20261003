Added
-----

*   Added an opt-in extension preservation feature. When
    ``preserve_extensions=True`` is passed to ``feedparser.parse()`` (or
    ``feedparser.PRESERVE_EXTENSIONS`` is set), custom XML extension elements
    that are not consumed by the built-in namespace handlers are preserved as
    ordered, nested structures in ``extensions`` lists on the feed and entry
    dictionaries. Elements are keyed by namespace URI and local name and keep
    their attributes, text, and child element order. Nesting depth and total
    preserved text are bounded by ``extension_max_depth`` and
    ``extension_max_text_length``; exceeded limits are reported in
    ``result['extension_diagnostics']``.
