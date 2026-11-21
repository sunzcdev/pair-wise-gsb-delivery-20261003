.. change::
    :tags: feature, batch

    Added the :paramref:`.BatchOperations.add_column.backfill`
    parameter, which accepts a SQL expression that is used to populate
    the new column for rows that already exist in the table when a batch
    "move and copy" migration recreates the table.  This allows a
    non-nullable column to be added to a table that already contains rows
    where each row's value must be computed from its existing data, such
    as ``func.substr(column("email"), 1, 4)``; the column's
    ``server_default``, if any, continues to apply to rows inserted after
    the migration.  The expression is rendered as part of the "INSERT
    from SELECT" statement, and is available in both online and offline
    (``copy_from``) batch modes.  See
    :ref:`batch_add_column_existing_rows`.

    Additionally, the SQLite "move and copy" procedure now runs its
    ``CREATE TABLE`` and copy statements within a SAVEPOINT, so that a
    failure partway through one of them, such as a backfill expression
    that violates a constraint, leaves the original table and its data
    intact and does not leave a temporary table behind that would block
    a subsequent attempt at the same migration.  This applies to all
    batch recreates on SQLite, not just those using the new parameter.
