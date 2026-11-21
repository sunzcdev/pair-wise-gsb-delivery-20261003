.. change::
    :tags: feature, operations

    Added :func:`.inspect_data_impact`, a read-only entry point that
    reports the data impact of an existing :class:`.MigrationScript` or
    operation tree, separately for the ``upgrade`` and ``downgrade``
    directions.  The resulting :class:`.DataImpactReport` lists the
    tables, columns and :class:`.DataImpactType` categories affected by
    each direction, so that a change such as a column drop that appears
    only in ``downgrade()`` is visible during review.  Operations nested
    within batch / table containers such as :class:`.ModifyTableOps` are
    expanded into their individual directives; renames are reported as
    identity changes with :attr:`.DataImpact.new_name` set; and a
    direction-level ``loses_data`` flag states whether that direction may
    discard original values.  Custom operations may declare their own
    impact by overriding :meth:`.MigrateOperation.data_impact`, which is
    independent of any database backend's DDL capabilities; operations
    that do not declare one are reported as
    :attr:`.DataImpactType.UNKNOWN` rather than being assumed safe.  The
    inspection does not modify the operation tree, so the same operations
    may still be rendered to SQL or a Python script exactly as before.
