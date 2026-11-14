.. _reference.dedup:

``dedup``
=====================

A deduplicated view of the parsed entries, provided to help consumers
handle entries that appear repeatedly in the same feed. It is a
dictionary with two keys:

``entries``
    The deduplicated entries. Entries are considered identical only when
    they share an explicit ``id``/``guid``; titles or other content are
    never used to guess identity. Entries without an identity are kept
    as-is. Each identity appears once, at the position of its first
    occurrence in the feed. When several entries share an identity and
    have comparable update times, the newest entry is kept. Every entry
    in this list is one of the original entries; fields from different
    versions are never merged into an entry that never existed.

``conflicts``
    A list of conflicts between same-identity entries that could not be
    ordered by update time (because the times are equal or missing) and
    whose contents differ. Each conflict is a dictionary with the entry
    ``id``, the ``kept_index``, and the ``indices`` of all candidates,
    referring to positions in the original ``entries`` list. The first
    occurrence is kept in the view; no candidate is silently dropped in
    favor of the last one.

The original ``entries`` list is never modified by the deduplication.

.. tip::

    This element always exists, although both lists may be empty.

.. seealso::

    *   :ref:`reference.entry.id`
