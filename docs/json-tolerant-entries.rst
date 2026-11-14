.. _advanced.json-tolerant-entries:

Tolerant JSON Feed Entries
==========================

A JSON document can be syntactically valid while containing individual
entries whose structure or field types do not conform to the
`JSON Feed <https://jsonfeed.org/>`_ format. By default, such an entry
aborts the parse and the exception is reported in the ``bozo_exception``
field.

Pass ``tolerant_entries=True`` to :py:func:`feedparser.parse` to isolate
the faulty entries instead. Each non-conforming entry is skipped, the
remaining entries keep their original order, and every skipped entry is
recorded in ``result.skipped_entries`` together with its source index,
a JSON Pointer-like path and a human-readable reason.

..  code-block:: pycon

    >>> import feedparser
    >>> d = feedparser.parse(
    ...     open("feed.json"),
    ...     tolerant_entries=True,
    ... )
    >>> d.entries
    [...]
    >>> d.skipped_entries
    [{'index': 2,
      'path': 'items/2/url',
      'reason': "the 'url' field must be a string"}]

The isolation level is consistent for nested members: a malformed author
or attachment member aborts the entry that contains it, so a single bad
member never silently drops part of an otherwise valid entry. A boolean
is never accepted as a numeric ``size_in_bytes``.

Tolerant mode only applies to individual entries. Errors that concern
the whole document still stop parsing and set the bozo bit:

*   JSON syntax errors raise a ``json.JSONDecodeError``.
*   A top-level value that is not a JSON object, a missing or non-array
    ``items`` field, and malformed feed-level authors raise a
    :py:class:`feedparser.JSONFeedValidationError`.

When ``tolerant_entries`` is omitted or ``False``, feedparser keeps its
historical behavior and validates nothing extra.
