#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Parse SQL statements."""

# Setup namespace
from collections.abc import Generator, Iterable
from typing import IO, Any

from sqlparse import cli, engine, filters, formatter, sql, tokens

__version__ = "0.6.1"
__all__ = ["cli", "engine", "filters", "formatter", "sql", "tokens"]


def parse(
    sql: str, encoding: str | None = None
) -> tuple[sql.Statement, ...]:
    """Parse sql and return a list of statements.

    :param sql: A string containing one or more SQL statements.
    :param encoding: The encoding of the statement (optional).
    :returns: A tuple of :class:`~sqlparse.sql.Statement` instances.
    """
    return tuple(parsestream(sql, encoding))


def parsestream(
    stream: str | IO[str], encoding: str | None = None
) -> Generator[sql.Statement, None, None]:
    """Parses sql statements from file-like object.

    :param stream: A file-like object.
    :param encoding: The encoding of the stream contents (optional).
    :returns: A generator of :class:`~sqlparse.sql.Statement` instances.
    """
    stack = engine.FilterStack()
    stack.enable_grouping()
    return stack.run(stream, encoding)


def format(sql: str, encoding: str | None = None, **options: Any) -> str:
    """Format *sql* according to *options*.

    Available options are documented in :ref:`formatting`.

    In addition to the formatting options this function accepts the
    keyword "encoding" which determines the encoding of the statement.

    :returns: The formatted SQL statement as string.
    """
    stack = engine.FilterStack()
    options = formatter.validate_options(options)
    stack = formatter.build_filter_stack(stack, options)
    stack.postprocess.append(filters.SerializerUnicode())
    return "".join(stack.run(sql, encoding))


def split(
    sql: str, encoding: str | None = None, strip_semicolon: bool = False
) -> list[str]:
    """Split *sql* into single statements.

    :param sql: A string containing one or more SQL statements.
    :param encoding: The encoding of the statement (optional).
    :param strip_semicolon: If True, remove trailing semicolons
        (default: False).
    :returns: A list of strings.
    """
    stack = engine.FilterStack(strip_semicolon=strip_semicolon)
    return [str(stmt).strip() for stmt in stack.run(sql, encoding)]


def splitstream(
    chunks: Iterable[str], encoding: str | None = None
) -> Generator[str, None, None]:
    """Split sql statements from an iterable of text chunks.

    Unlike :func:`parsestream`, which may read the whole input before
    producing statements, this generator consumes the input chunk by
    chunk and yields each statement as soon as it is complete.  Only the
    currently unfinished statement is kept in memory.

    The result is identical to :func:`split` called on the joined
    chunks, regardless of where the chunk boundaries fall -- even in the
    middle of escape sequences, string or dollar-quoted literals,
    comments, or semicolons.

    Use :class:`sqlparse.engine.StreamStatementSplitter` directly when
    you need to know whether the input ended with unclosed structures
    (string, comment, dollar-quoted literal, parenthesis or
    BEGIN ... END block); its ``close()`` method reports a separate
    incomplete status for that case.

    :param chunks: An iterable of text chunks (``str``, or ``bytes``
        when *encoding* is given).
    :param encoding: The encoding of byte chunks (optional).
    :returns: A generator of strings.
    """
    splitter = engine.StreamStatementSplitter(encoding=encoding)
    for chunk in chunks:
        yield from splitter.feed(chunk)
    result = splitter.close()
    yield from result.statements
    if result.statement is not None:
        yield result.statement
