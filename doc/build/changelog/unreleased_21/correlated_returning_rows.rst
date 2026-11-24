.. change::
    :tags: feature, engine

    Added a new execution option
    :paramref:`_engine.Connection.execution_options.correlate_returning_to_input`
    and corresponding :attr:`_engine.CursorResult.correlated_rows` accessor
    which provide an explicit view that correlates the RETURNING rows of an
    executemany ("insertmanyvalues") INSERT .. RETURNING execution to the
    input parameter sets, in input order.  Rows that were skipped by the
    database, e.g. by ``ON CONFLICT DO NOTHING``, are represented as
    ``None`` entries aligned with their input parameter sets, including
    when skipped rows occur within a single executed batch.  Batched
    execution is retained on backends that support it; correlation is
    exact when client-side sentinel columns are in use, and an informative
    error is raised when returned rows cannot be reliably correlated to
    input parameter sets.  Statements that don't use the option are
    unaffected.
