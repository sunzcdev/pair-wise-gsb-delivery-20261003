#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Recovery-aware parsing of batches of SQL statements.

Unlike :func:`sqlparse.parse`, a grouping failure on a single statement
does not abort the whole batch. Every statement is reported in its
original order together with either the successfully parsed
:class:`~sqlparse.sql.Statement` or a diagnosis of the failure.
"""

from dataclasses import dataclass
from typing import IO

from sqlparse import engine, sql


@dataclass
class ParseResult:
    """Outcome of parsing a single statement.

    On success :attr:`error` is ``None`` and :attr:`statement` holds the
    fully grouped :class:`~sqlparse.sql.Statement`. On a grouping failure
    :attr:`error` holds the exception while :attr:`statement` still points
    at the ungrouped, unformatted statement so its original text and
    tokens remain available.

    :attr:`source` always contains the verbatim source text of the
    statement, and :attr:`is_incomplete` flags a trailing statement whose
    boundary the lexer could not determine.
    """

    source: str
    statement: sql.Statement | None = None
    error: Exception | None = None
    is_incomplete: bool = False

    @property
    def ok(self):
        """``True`` when the statement was grouped without an error."""
        return self.error is None


def _statement_source(stmt):
    """Return the verbatim text of *stmt* without recursing.

    ``str(stmt)`` walks the token tree recursively, which can exhaust the
    interpreter stack for a tree built while the grouping limits are
    disabled. Recovery must still be able to report the statement's
    source, so walk the tree with an explicit stack instead.
    """
    chunks = []
    stack = list(reversed(stmt.tokens))
    while stack:
        token = stack.pop()
        if token.is_group:
            stack.extend(reversed(token.tokens))
        else:
            chunks.append(token.value)
    return ''.join(chunks)


def parse_recovery(
    sql_text: str | IO[str], encoding: str | None = None
) -> tuple[ParseResult, ...]:
    """Parse a batch of SQL statements with per-statement recovery.

    A statement whose grouping fails is returned with its error diagnosis
    instead of raising, and the following statements are still parsed.
    Statements that cannot be split because the input ends inside an
    unclosed string literal or block comment are returned once, marked
    :attr:`ParseResult.is_incomplete`, and no attempt is made to resume
    splitting at a later semicolon.

    Failed and incomplete statements keep their original text and never
    pass through the formatting filters, which expect a successfully
    grouped tree.

    :param sql_text: A string or file-like object containing one or more
        SQL statements.
    :param encoding: The encoding of the statement (optional).
    :returns: A tuple of :class:`ParseResult` instances, in input order.
    """
    stack = engine.FilterStack(recover=True)
    stack.enable_grouping()

    results = []
    for stmt in stack.run(sql_text, encoding):
        results.append(
            ParseResult(
                source=_statement_source(stmt),
                statement=stmt,
                error=stmt.grouping_error,
                is_incomplete=stmt.is_incomplete,
            )
        )
    return tuple(results)
