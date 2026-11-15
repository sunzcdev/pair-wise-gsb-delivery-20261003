#
# Copyright (C) 2009-2020 the sqlparse authors and contributors
# <see AUTHORS file>
#
# This module is part of python-sqlparse and is released under
# the BSD License: https://opensource.org/licenses/BSD-3-Clause

"""Recovery-aware parsing of batches of SQL statements.

Unlike :func:`sqlparse.parse`, a grouping failure on a single statement
does not abort the whole batch. Each statement is reported in its
original order together with either its successfully grouped structure
or a diagnosis of the failure.
"""

from dataclasses import dataclass
from typing import IO

from sqlparse import engine, sql
from sqlparse.exceptions import IncompleteInputError


@dataclass
class ParseResult:
    """Outcome of parsing a single statement.

    The verbatim source text of the statement is always available in
    :attr:`source`. On success :attr:`error` is ``None`` and
    :attr:`statement` holds the fully grouped
    :class:`~sqlparse.sql.Statement`. On a grouping failure
    :attr:`error` holds the captured exception, while :attr:`statement`
    keeps the ungrouped statement so its tokens are still inspectable.

    :attr:`is_incomplete` marks the trailing statement whose boundary
    the lexer could not determine because the input ends inside an open
    quoted literal or block comment.
    """

    source: str
    statement: sql.Statement | None = None
    error: Exception | None = None
    is_incomplete: bool = False

    @property
    def ok(self):
        """``True`` when the statement was grouped without an error."""
        return self.error is None


def _source_text(stmt):
    """Return the statement text without recursing over its tree.

    A statement whose grouping was aborted may hold a partially nested
    structure. Flattening it recursively could hit the very recursion
    limit that caused the failure, so walk the tree iteratively.
    """
    parts = []
    stack = [stmt]
    while stack:
        node = stack.pop()
        if node.is_group:
            stack.extend(reversed(node.tokens))
        else:
            parts.append(node.value)
    return ''.join(parts)


def parse_recovery(
    sql_text: str | IO[str], encoding: str | None = None
) -> tuple[ParseResult, ...]:
    """Parse a batch of SQL statements with per-statement recovery.

    Unlike :func:`sqlparse.parse`, a statement whose grouping fails is
    returned with its diagnosis instead of aborting the batch, and the
    following statements are still parsed. Statements that cannot be
    split because the input ends inside an unclosed quoted literal or
    block comment are reported once, marked
    :attr:`ParseResult.is_incomplete`; splitting is not resumed at a
    semicolon inside the open construct.

    Statements reported with an error are not passed through the
    formatting filters, so their source text is preserved.

    :param sql_text: A string or file-like object containing one or more
        SQL statements.
    :param encoding: The encoding of the statement (optional).
    :returns: A tuple of :class:`ParseResult` instances in input order.
    """
    stack = engine.FilterStack(recover=True)
    stack.enable_grouping()

    results = []
    for stmt in stack.run(sql_text, encoding):
        error = stmt.grouping_error
        results.append(
            ParseResult(
                source=_source_text(stmt),
                statement=stmt,
                error=error,
                is_incomplete=isinstance(error, IncompleteInputError),
            )
        )
    return tuple(results)
