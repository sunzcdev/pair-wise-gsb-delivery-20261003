.. change::
    :tags: bug, sql

    Fixed issue where replacing the compilation handler of a custom SQL
    element using the ``compiles()`` function from
    :mod:`sqlalchemy.ext.compiler` (or removing it via ``deregister()``)
    could be silently ignored for statements that were already present in a
    shared ``compiled_cache``, such as the built-in statement cache of an
    :class:`_engine.Engine` that is shared by engines produced from
    :meth:`_engine.Engine.execution_options`.  Each custom compiler
    dispatcher now carries a registration generation that participates in the
    compiled statement cache key.  Statements already handed off to the
    DBAPI continue to run with their original SQL, while new executions use
    the newly registered compiler; statements that do not involve the
    re-registered element, including those using other custom elements and
    ordinary expressions, continue to hit the same cache.
