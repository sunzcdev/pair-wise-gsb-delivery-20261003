.. change::
    :tags: feature, autogenerate

    Improved the autogenerate comparison of indexes to leave an itemized
    conclusion for each aspect of the index: uniqueness, keys, direction of
    the keys and filter condition (WHERE clause).  The log message emitted
    for a changed index now states exactly which aspect differs.  Aspects
    that the database did not reflect enough information to compare, such
    as the ordering of index keys on backends that don't reflect it, are
    now reported as "cannot compare" and the index is otherwise assumed to
    be equal.

.. change::
    :tags: feature, autogenerate, postgresql

    The PostgreSQL index comparison now also compares the filter
    condition (``postgresql_where``) of partial indexes, normalizing
    spelling differences that PostgreSQL considers equivalent, such as
    whitespace, parenthesization and ``::`` casts, by compiling the
    metadata expression with the dialect.  Quoted identifiers and
    quoted literals are now considered significant and are no longer
    folded together with unquoted names when comparing index
    expressions.

.. change::
    :tags: feature, autogenerate, sqlite

    Autogenerate now compares the filter condition (``sqlite_where``)
    of SQLite partial indexes, which the dialect reflects; a changed,
    added or removed filter condition is now detected and produces the
    usual drop/add index pair.
