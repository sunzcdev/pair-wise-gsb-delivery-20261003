Extending :mod:`sqlparse`
=========================

.. module:: sqlparse
   :synopsis: Extending parsing capability of sqlparse.
   :no-index:

The :mod:`sqlparse` module uses a sql grammar that was tuned through usage and numerous
PR to fit a broad range of SQL syntaxes, but it cannot cater to every given case since
some SQL dialects have adopted conflicting meanings of certain keywords. Sqlparse
therefore exposes a mechanism to configure the fundamental keywords and regular
expressions that parse the language as described below.

If you find an adaptation that works for your specific use-case. Please consider
contributing it back to the community by opening a PR on
`GitHub <https://github.com/andialbrecht/sqlparse>`_.

Configuring the Lexer
---------------------

The lexer is a singleton class that breaks down the stream of characters into language
tokens. It does this by using a sequence of regular expressions and keywords that are
listed in the file ``sqlparse.keywords``. Instead of applying these fixed grammar
definitions directly, the lexer is default initialized in its method called
``default_initialization()``. As an api user, you can adapt the Lexer configuration by
applying your own configuration logic. To do so, start out by clearing previous
configurations with ``.clear()``, then apply the SQL list with
``.set_SQL_REGEX(SQL_REGEX)``, and apply keyword lists with ``.add_keywords(KEYWORDS)``.

You can do so by re-using the expressions in ``sqlparse.keywords`` (see example below),
leaving parts out, or by making up your own master list.

See the expected types of the arguments by inspecting their structure in
``sqlparse.keywords``.

.. important::

   ``.clear()`` removes *all* keyword dictionaries. Any dictionary that is
   not added back afterwards is no longer recognized, so make sure to re-add
   every dictionary you rely on. The full default set is listed in
   ``Lexer.default_initialization()``.

The following example adds support for the expression ``ZORDER BY``, and adds ``BAR`` as
a keyword to the lexer:

..  code-block:: python

    import re

    import sqlparse
    from sqlparse import keywords
    from sqlparse.lexer import Lexer

    # get the lexer singleton object to configure it
    lex = Lexer.get_default_instance()

    # Clear the default configurations.
    # After this call, reg-exps and keyword dictionaries need to be loaded
    # to make the lexer functional again.
    lex.clear()

    my_regex = (r"ZORDER\s+BY\b", sqlparse.tokens.Keyword)

    # slice the default SQL_REGEX to inject the custom object
    lex.set_SQL_REGEX(
        keywords.SQL_REGEX[:38]
        + [my_regex]
        + keywords.SQL_REGEX[38:]
    )

    # add the default keyword dictionaries
    lex.add_keywords(keywords.KEYWORDS_COMMON)
    lex.add_keywords(keywords.KEYWORDS_ORACLE)
    lex.add_keywords(keywords.KEYWORDS_MYSQL)
    lex.add_keywords(keywords.KEYWORDS_PLPGSQL)
    lex.add_keywords(keywords.KEYWORDS_HQL)
    lex.add_keywords(keywords.KEYWORDS_MSACCESS)
    lex.add_keywords(keywords.KEYWORDS_SNOWFLAKE)
    lex.add_keywords(keywords.KEYWORDS_BIGQUERY)
    lex.add_keywords(keywords.KEYWORDS)

    # add a custom keyword dictionary
    lex.add_keywords({'BAR': sqlparse.tokens.Keyword})

    # no configuration is passed here. The lexer is used as a singleton.
    sqlparse.parse("select * from foo zorder by bar;")


.. _filter-stack:

Reusing a FilterStack
---------------------

A :class:`~sqlparse.engine.FilterStack` applies a sequence of filters to a
token stream. The module-level functions :func:`sqlparse.format`,
:func:`sqlparse.parse` and :func:`sqlparse.split` build a fresh stack for
every call, but a stack can also be built once and used for many inputs,
for example when one formatter serves several files or requests:

..  code-block:: python

    from sqlparse import filters
    from sqlparse.engine import FilterStack

    stack = FilterStack()
    stack.enable_grouping()
    stack.postprocess.append(filters.SerializerUnicode())

    for sql in ['select a from foo', 'select b from bar']:
        print(''.join(stack.run(sql)))

Every call to :meth:`~sqlparse.engine.FilterStack.run` is a *session*. A
run that is consumed interleaved with another run, abandoned before its
last statement, or that raises halfway through does not affect any other
run of the same stack, and a later run starts from a clean state. In
particular a filter's state may not be observed across sessions: for
example, the reindent filter does not keep the last statement of a
previous run, and the python and php output filters name their variables
``sql``, ``sql2``, ... starting over in every run.

Filters that keep state for the duration of one run therefore implement a
``new_session()`` method. It is called once per run and per filter instance,
and returns the object that handles that run. The filters in
:mod:`sqlparse.filters` that keep state do this already. A custom filter
follows the same pattern:

..  code-block:: python

    class CountStatementsFilter:
        def __init__(self):
            self.count = 0

        def new_session(self):
            # called once when a run starts, so every run starts at zero
            return CountStatementsFilter()

        def process(self, stmt):
            self.count += 1
            return stmt

The session instance is a plain object: it is not required to be a copy of
the filter itself, and the stack never copies filter attributes on its own.
A filter that returns ``None`` from ``new_session()`` is rejected with a
:exc:`TypeError`, and declaring ``new_session = None`` opts out explicitly.

..  important::

   Filters that don't declare ``new_session()`` are used as they are and
   keep the behaviour they had before sessions were introduced. This keeps
   custom filters working, but it also means that such a filter's state is
   shared by all runs of the stack and survives an abandoned or failed run.
   If a filter keeps state that belongs to a single input, it must declare
   ``new_session()`` to be safe to reuse.

Token streams created by preprocessing filters are closed when the run
finishes, also when it is abandoned or fails. Only generator objects are
closed; a filter that returns another kind of iterable is responsible for
it itself.

Because the state belongs to the run, a failed run does not have to be
discarded along with the stack:

..  code-block:: python

    from sqlparse.exceptions import SQLParseError

    sql = 'select a, b from foo'
    try:
        for stmt in stack.run(sql):
            process(stmt)
    except SQLParseError:
        # The failed run is over; the stack is not left in a half-formatted
        # state, so the next run starts from scratch.
        pass

    # produces the same output as a stack that never failed
    for stmt in stack.run('select c from bar'):
        process(stmt)
