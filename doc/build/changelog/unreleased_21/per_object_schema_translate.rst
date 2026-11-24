.. change::
    :tags: usecase, sql

    The :ref:`schema translate map <schema_translating>` feature now
    supports being applied at the level of an individual
    :class:`_schema.Table` or :class:`_sql.Alias` object, using the new
    :meth:`_expression.TableClause.execution_options` and
    :meth:`_sql.Alias.execution_options` methods.  A
    ``schema_translate_map`` set on a table or alias takes precedence
    over a map set at the statement, connection, or engine level for
    that particular table reference, while the statement, connection,
    or engine level map continues to apply to all other tables.  This
    allows a single query to render the same table from multiple
    schemas at once, such as when comparing tables across attached
    databases.  Per-table and per-alias translate maps also participate
    in the SQL compilation cache key, so statements that use differing
    translate maps are cached and rendered independently of each other.
