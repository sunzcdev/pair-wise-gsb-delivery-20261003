Added
-----

*   Add a ``tolerant_entries`` option to ``feedparser.parse()`` for JSON
    feeds. When enabled, an entry whose structure or field types do not
    conform to the JSON Feed format is skipped instead of aborting the
    parse. Each skipped entry is reported in ``result.skipped_entries``
    with its source index, path and reason, while feed-level errors and
    JSON syntax errors still stop parsing. The default behavior is
    unchanged.
