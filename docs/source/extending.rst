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


Writing Stateful Filters
------------------------

A :class:`~sqlparse.engine.FilterStack` is reusable: the same stack instance can
be driven through several :meth:`~sqlparse.engine.FilterStack.run` calls, and
the generators returned by those calls may even be consumed in an interleaved
fashion or closed before they are exhausted.  Filters are attached to the
stack once, so a filter that mutates itself while processing a stream needs an
explicit boundary between runs.

sqlparse models that boundary with a *session*.  When a run starts, the stack
calls ``begin()`` on every registered filter and uses the returned object for
that run only; it is discarded when the generator finishes or is closed.
Stateful filters therefore implement ``begin()`` to return a freshly reset
instance, while stateless filters can leave the default in place (it returns
``self``).

.. code-block:: python

    from sqlparse.filters import Filter


    class CountingFilter(Filter):
        def __init__(self):
            self.seen = 0

        def begin(self):
            # A run-private copy with reset state. Configuration stored on
            # the registered filter can be copied over explicitly.
            return CountingFilter()

        def process(self, stmt):
            self.seen += 1
            return stmt

Because each run keeps its own session object in the generator's local frame,
two interleaved runs never share indentation level, statement counters or any
half-consumed generator state, and abandoning or failing one run cannot
pollute the next one. The stack relies on ``begin()`` rather than copying
filters, so arbitrary objects referenced by a filter are never deep-copied.

The built-in stateful filters -- :class:`~sqlparse.filters.ReindentFilter`,
:class:`~sqlparse.filters.AlignedIndentFilter` and the output serializers --
already implement ``begin()``.

.. note::

   A custom filter that does **not** define ``begin()`` keeps working
   unchanged for backwards compatibility, but the same instance is reused on
   every run. Such a filter must not keep per-statement or per-run state; if
   it does, the state accumulates across runs and interleaved runs share it.
   Give any stateful filter a ``begin()`` method that returns a new instance
   to make it safe to reuse on a shared stack.
