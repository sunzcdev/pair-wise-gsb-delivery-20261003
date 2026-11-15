
.. _advanced_filters:

Advanced filters
================

``dataset`` provides two methods for running queries: :py:meth:`table.find() <dataset.Table.find>`
and :py:meth:`db.query() <dataset.Database.query>`. The table find helper method provides 
limited, but simple filtering options::

    results = table.find(column={operator: value})
    # e.g.:
    results = table.find(name={'like': '%mole rat%'})

A special form is using keyword searches on specific columns::

    results = table.find(value=5)
    # equal to:
    results = table.find(value={'=': 5})

    # Lists, tuples and sets are turned into `IN` queries:
    results = table.find(category=('foo', 'bar'))
    # equal to:
    results = table.find(value={'in': ('foo', 'bar')})

The following comparison operators are supported:

============== ============================================================
Operator       Description
============== ============================================================
gt, >          Greater than
lt, <          Less than
gte, >=        Greater or equal
lte, <=        Less or equal
!=, <>, not    Not equal to a single value
in             Value is in the given sequence
notin          Value is not in the given sequence
like, ilike    Text search, ILIKE is case-insensitive. Use ``%`` as a wildcard
notlike        Like text search, except check if pattern does not exist
between, ..    Value is between two values in the given tuple
startswith     String starts with
endswith       String ends with
============== ============================================================

Querying for a specific value on a column that does not exist on the table
will return no results.

You can also pass `SQLAlchemy core expressions`_ directly into the
:py:meth:`table.find() <dataset.Table.find>` method as positional arguments.
Access the underlying SQLAlchemy table via ``table.table`` and its columns
via ``table.table.columns``::

    from sqlalchemy import or_

    # Get a column object:
    city = table.table.columns.city
    # Use a SQLAlchemy clause:
    results = table.find(city.ilike('amsterda%'))

    # Combine with OR:
    country = table.table.columns.country
    results = table.find(or_(city == 'Amsterdam', country == 'Germany'))

    # Combine SQLAlchemy clauses with keyword filters:
    results = table.find(city.ilike('new%'), country='US')

These clauses also work with :py:meth:`table.count() <dataset.Table.count>`,
:py:meth:`table.find_one() <dataset.Table.find_one>`, and
:py:meth:`table.delete() <dataset.Table.delete>`.

.. _structured_filters:

Structured filters
------------------

SQLAlchemy clauses are Python objects, so they cannot be stored in a JSON
or YAML configuration file, a database column or an HTTP payload. For
those cases ``dataset`` accepts a restricted, data-only filter tree via
the ``_filter`` argument of :py:meth:`table.find() <dataset.Table.find>`,
:py:meth:`table.find_one() <dataset.Table.find_one>`,
:py:meth:`table.count() <dataset.Table.count>` and
:py:meth:`table.delete() <dataset.Table.delete>`::

    results = table.find(_filter={
        "all": [
            {"field": "country", "op": "eq", "value": "Germany"},
            {"any": [
                {"field": "population", "op": "gte", "value": 100000},
                {"not": {"field": "capital", "op": "eq", "value": None}},
            ]},
        ]
    })

    # The same rows can be counted without loading them:
    assert table.count(_filter=spec) == len(list(table.find(_filter=spec)))

A tree is one of these nodes:

``{"all": [...]}`` / ``{"any": [...]}``
    A list of child nodes, combined with ``AND`` / ``OR``.  An empty
    ``all`` is ``TRUE`` and an empty ``any`` is ``FALSE`` (the identities
    of conjunction and disjunction).  These rules are explicit - they do
    not depend on Python's notion of truthiness.
``{"not": {...}}``
    Negates a single child node.  SQL's three-valued logic is preserved:
    negating ``UNKNOWN`` stays ``UNKNOWN``.
``{"field": "...", "op": "...", "value": ...}``
    Compares one column.  ``op`` defaults to ``eq``.

The supported operators mirror the keyword filters:

============================= ========================================
Operator                      Meaning
============================= ========================================
``eq``, ``=``, ``==``         equal (``value: null`` means ``IS NULL``)
``is``                        alias for ``eq``
``ne``, ``!=``, ``<>``        not equal (``null``: ``IS NOT NULL``)
``gt``, ``>``                 greater than
``gte``, ``>=``               greater than or equal
``lt``, ``<``                 less than
``lte``, ``<=``               less than or equal
``like``, ``ilike``           SQL ``LIKE`` / ``ILIKE``
``notlike``, ``notilike``     negated ``LIKE`` / ``ILIKE``
``startswith``, ``endswith``  string prefix/suffix match, escaped
``in``                        membership
``notin``                     negated membership
``between``, ``..``           inclusive range, ``[low, high]``
============================= ========================================

NULL and empty sets
~~~~~~~~~~~~~~~~~~~

The behaviour around ``NULL`` is defined explicitly:

* ``in []`` is ``FALSE`` and ``notin []`` is ``TRUE``.
* ``in [..., null]`` also matches rows where the column is ``NULL``.
* ``notin [...]`` compiles to ``NOT (column IN (...))`` and therefore
  follows SQL three-valued logic: a ``NULL`` column produces ``UNKNOWN``
  and does **not** match.  Listing ``null`` explicitly, e.g.
  ``notin ["a", null]``, makes it the exact complement of
  ``in ["a", null]``.
* ``eq``/``ne`` with ``null`` compile to ``IS NULL`` / ``IS NOT NULL``.
* Ordering comparisons (``gt``/``gte``/``lt``/``lte``) and ``between`` with
  a ``null`` operand follow SQL's three-valued logic: the comparison is
  ``UNKNOWN`` and matches no rows (and stays ``UNKNOWN`` under ``not``).
  Use ``eq``/``ne`` for explicit null tests.

Errors are raised before the query runs
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Invalid filters raise :py:class:`dataset.filters.FilterError` (a subclass
of :py:class:`dataset.QueryError`) *before* any SQL is executed.  The
message contains the JSON path of the offending node::

    >>> table.find(_filter={"all": [{"field": "nope", "op": "eq", "value": 1}]})
    FilterError: invalid structured filter at $.all[0].field: unknown column 'nope'

Unknown columns and unknown operators are reported instead of silently
turning into ``FALSE``.  All values are passed as bound parameters, so a
value can never be executed as a SQL fragment.  To keep a hostile or
broken configuration from exhausting resources, a tree is limited to
:py:data:`dataset.filters.MAX_FILTER_DEPTH` levels of nesting and
:py:data:`dataset.filters.MAX_FILTER_NODES` nodes in total.

A compiled tree can also be built once and reused::

    clause = table.build_filter(spec)
    rows = list(table.find(clause))
    total = table.count(clause)

Structured filters can be combined with positional SQLAlchemy clauses and
keyword filters; all of them are joined with ``AND``.  The existing
keyword filter syntax is unchanged.  Filter trees are restricted to
columns that actually exist on the table; if the table does not exist yet,
the query returns no rows but structural errors are still reported.

Queries using raw SQL
---------------------

To run more complex queries with JOINs, or to perform GROUP BY-style
aggregation, you can also use :py:meth:`db.query() <dataset.Database.query>`
to run raw SQL queries instead. This also supports parameterisation to avoid
SQL injections::

    statement = 'SELECT user, COUNT(*) c FROM photos GROUP BY user'
    for row in db.query(statement):
        print(row['user'], row['c'])

    # With parameter binding:
    results = db.query('SELECT * FROM users WHERE age > :min_age', min_age=21)

For fully programmatic, composable query building, consider using
`SQLAlchemy core expressions`_ directly.

.. _SQLAlchemy core expressions: https://docs.sqlalchemy.org/en/21/tutorial/data_select.html#tutorial-selecting-data