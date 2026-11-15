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

Independent lexical configurations
-----------------------------------

Reconfiguring the singleton as shown above changes global state, so it cannot
be used when one process has to review SQL from different sources at the same
time. A keyword added for one source would suddenly be visible to every other
source. :class:`~sqlparse.lexer.LexerConfig` provides an independent, immutable
configuration that can be passed directly to :func:`parse`, :func:`split`,
:func:`format` and :func:`~sqlparse.lexer.tokenize` through the
``lexer_config`` argument. Each call then uses a dedicated
:class:`~sqlparse.lexer.Lexer` and the default singleton is left untouched.

A config inherits the built-in regex list and keyword vocabulary and layers
customizations on top:

* ``keywords`` maps a keyword (matched case-insensitively) to a token type,
  overriding an inherited keyword of the same name.
* Mapping a keyword to :data:`~sqlparse.lexer.REMOVE_KEYWORD` removes an
  inherited keyword, so it is classified as a regular name instead.
* ``sql_regex`` optionally replaces the whole regex list with a custom one,
  for example by copying and editing :data:`sqlparse.keywords.SQL_REGEX`.

The dictionaries passed to a config are validated and copied immediately, so
later mutation of those dictionaries does not affect the config. Invalid
arguments raise :class:`~sqlparse.exceptions.SQLParseError` while the config
is being built, before any SQL is read.

..  code-block:: python

    import sqlparse
    from sqlparse.lexer import LexerConfig, REMOVE_KEYWORD

    # Source A treats "pivot" as a built-in name.
    source_a = LexerConfig(keywords={'PIVOT': sqlparse.tokens.Name.Builtin})

    # Source B treats the same word as a keyword and no longer knows "select".
    source_b = LexerConfig(keywords={
        'PIVOT': sqlparse.tokens.Keyword,
        'SELECT': REMOVE_KEYWORD,
    })

    # The two configs can be interleaved without interfering with each other
    # or with the default singleton lexer.
    sqlparse.parse("select * from t pivot (x)", lexer_config=source_a)
    sqlparse.parse("select * from t pivot (x)", lexer_config=source_b)

Passing anything other than a :class:`~sqlparse.lexer.LexerConfig` (or
``None``) as ``lexer_config`` raises :class:`~sqlparse.exceptions.SQLParseError`;
a configured :class:`~sqlparse.engine.FilterStack` never silently falls back to
the default lexer.
