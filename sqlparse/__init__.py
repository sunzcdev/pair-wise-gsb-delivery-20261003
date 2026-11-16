#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Parse SQL statements."""

# Setup namespace
from collections.abc import Generator
from typing import IO, Any

from sqlparse import cli, engine, filters, formatter, sql, tokens

__version__ = "0.6.1"
__all__ = ["cli", "engine", "filters", "formatter", "sql", "tokens"]


def parse(
    sql: str, encoding: str | None = None, lexer: Any = None
) -> tuple[sql.Statement, ...]:
    """Parse sql and return a list of statements.

    :param sql: A string containing one or more SQL statements.
    :param encoding: The encoding of the statement (optional).
    :param lexer: A :class:`~sqlparse.lexer.Lexer` or
        :class:`~sqlparse.lexer.LexerConfig` to tokenize with instead of
        the shared default lexer (optional).
    :returns: A tuple of :class:`~sqlparse.sql.Statement` instances.
    """
    return tuple(parsestream(sql, encoding, lexer=lexer))


def parsestream(
    stream: str | IO[str], encoding: str | None = None, lexer: Any = None
) -> Generator[sql.Statement, None, None]:
    """Parses sql statements from file-like object.

    :param stream: A file-like object.
    :param encoding: The encoding of the stream contents (optional).
    :param lexer: A :class:`~sqlparse.lexer.Lexer` or
        :class:`~sqlparse.lexer.LexerConfig` to tokenize with instead of
        the shared default lexer (optional). Invalid values are rejected
        before the stream is read.
    :returns: A generator of :class:`~sqlparse.sql.Statement` instances.
    """
    stack = engine.FilterStack(lexer=lexer)
    stack.enable_grouping()
    return stack.run(stream, encoding)


def format(
    sql: str, encoding: str | None = None, lexer: Any = None, **options: Any
) -> str:
    """Format *sql* according to *options*.

    Available options are documented in :ref:`formatting`.

    In addition to the formatting options this function accepts the
    keyword "encoding" which determines the encoding of the statement,
    and the keyword "lexer" which accepts a
    :class:`~sqlparse.lexer.Lexer` or :class:`~sqlparse.lexer.LexerConfig`
    to tokenize with instead of the shared default lexer.

    :returns: The formatted SQL statement as string.
    """
    stack = engine.FilterStack(lexer=lexer)
    options = formatter.validate_options(options)
    stack = formatter.build_filter_stack(stack, options)
    stack.postprocess.append(filters.SerializerUnicode())
    return "".join(stack.run(sql, encoding))


def split(
    sql: str,
    encoding: str | None = None,
    strip_semicolon: bool = False,
    lexer: Any = None,
) -> list[str]:
    """Split *sql* into single statements.

    :param sql: A string containing one or more SQL statements.
    :param encoding: The encoding of the statement (optional).
    :param strip_semicolon: If True, remove trailing semicolons
        (default: False).
    :param lexer: A :class:`~sqlparse.lexer.Lexer` or
        :class:`~sqlparse.lexer.LexerConfig` to tokenize with instead of
        the shared default lexer (optional).
    :returns: A list of strings.
    """
    stack = engine.FilterStack(strip_semicolon=strip_semicolon, lexer=lexer)
    return [str(stmt).strip() for stmt in stack.run(sql, encoding)]
