"""
Candidate dialect comparison.

When a source system does not record which SQL dialect a statement was written
in, the first dialect that parses it without an error is not necessarily the
right one. This module compares a finite list of candidate dialects by parsing
the same statement(s) with each of them and reporting, per statement:

- which candidates parsed successfully,
- which candidates failed with an explicit error,
- which candidates fell back to an opaque ``Command`` expression.

Successful candidates are grouped by canonical AST equality: only candidates
that assign the same meaning to the same text share a group. When groups
disagree (e.g. quoting rules, array subscript offsets or function arguments
are interpreted differently), the differing locations are reported.

This tool never executes SQL. Identical parses do not prove a dialect is the
source dialect: many statements are valid and equivalent in several dialects,
so a dialect cannot be uniquely identified from arbitrary SQL.
"""

from __future__ import annotations

import typing as t
from dataclasses import dataclass, field
from enum import auto

from sqlglot import expressions as exp
from sqlglot.dialects.dialect import Dialect
from sqlglot.errors import ErrorLevel
from sqlglot.helper import AutoName
from sqlglot.tokens import TokenType

if t.TYPE_CHECKING:
    from sqlglot.dialects.dialect import DialectType

MAX_DIFFERENCES = 20


class CandidateStatus(AutoName):
    """Outcome of parsing one statement with one candidate dialect."""

    PARSED = auto()
    """The statement parsed into a structured AST."""

    FAILED = auto()
    """The candidate raised an explicit error."""

    COMMAND_FALLBACK = auto()
    """The statement only parsed as an opaque ``Command``."""


@dataclass
class DialectCandidate:
    """Per-dialect outcome for a single statement."""

    dialect: str
    status: CandidateStatus
    error: t.Optional[str] = None
    group: t.Optional[int] = None


@dataclass
class InterpretationGroup:
    """A set of dialects that parsed a statement into equal ASTs."""

    index: int
    dialects: list[str]
    sql: str


@dataclass
class Difference:
    """A single location where two interpretation groups disagree."""

    location: str
    source: str
    target: str


@dataclass
class StatementComparison:
    """Comparison of all candidate dialects for a single statement."""

    index: int
    sql: str
    candidates: list[DialectCandidate] = field(default_factory=list)
    groups: list[InterpretationGroup] = field(default_factory=list)
    differences: list[tuple[int, int, list[Difference]]] = field(default_factory=list)


@dataclass
class DialectComparisonReport:
    """Ordered per-statement comparison results."""

    statements: list[StatementComparison] = field(default_factory=list)

    def render(self) -> str:
        lines = []
        for statement in self.statements:
            lines.append(f"Statement {statement.index + 1}: {statement.sql}")
            for candidate in statement.candidates:
                if candidate.status is CandidateStatus.PARSED:
                    detail = f"parsed (group {candidate.group})"
                elif candidate.status is CandidateStatus.COMMAND_FALLBACK:
                    detail = "opaque Command fallback"
                else:
                    detail = f"failed: {candidate.error}"
                lines.append(f"  {candidate.dialect}: {detail}")
            for group in statement.groups:
                lines.append(f"  Group {group.index}: {', '.join(group.dialects)}")
                lines.append(f"    canonical: {group.sql}")
            for left, right, differences in statement.differences:
                lines.append(f"  Differences between group {left} and group {right}:")
                for difference in differences:
                    lines.append(
                        f"    {difference.location}: {difference.source} != {difference.target}"
                    )
        lines.append(
            "Note: candidates are compared by parsing only; no query is executed and"
            " identical parses do not uniquely identify the source dialect."
        )
        return "\n".join(lines)


def compare_dialects(
    sql: str, dialects: t.Sequence[DialectType], **parse_opts: t.Any
) -> DialectComparisonReport:
    """
    Parses `sql` with each candidate dialect and compares the interpretations.

    Args:
        sql: one or more SQL statements. Statement order is preserved in the report.
        dialects: the finite list of candidate dialects to compare.
        parse_opts: additional options forwarded to each dialect's parser.

    Returns:
        A report with an independent conclusion per statement and per candidate.
    """
    report = DialectComparisonReport()
    opts = {"error_level": ErrorLevel.IMMEDIATE, **parse_opts}

    for index, statement in enumerate(_split_statements(sql)):
        comparison = StatementComparison(index=index, sql=statement)
        parsed: list[tuple[str, exp.Expr]] = []

        for dialect in dialects:
            name = dialect if isinstance(dialect, str) else str(getattr(dialect, "value", dialect))
            try:
                result = Dialect.get_or_raise(dialect).parse(statement, **opts)
                expression = result[0] if result else None
                if expression is None:
                    raise ValueError("No expression was parsed")
            except Exception as e:
                comparison.candidates.append(
                    DialectCandidate(
                        dialect=name,
                        status=CandidateStatus.FAILED,
                        error=f"{type(e).__name__}: {e}",
                    )
                )
                continue

            if isinstance(expression, exp.Command):
                comparison.candidates.append(
                    DialectCandidate(dialect=name, status=CandidateStatus.COMMAND_FALLBACK)
                )
            else:
                parsed.append((name, expression))
                comparison.candidates.append(
                    DialectCandidate(dialect=name, status=CandidateStatus.PARSED)
                )

        _group_parsed(comparison, parsed)
        report.statements.append(comparison)

    return report


def _split_statements(sql: str) -> list[str]:
    try:
        tokens = Dialect.get_or_raise(None).tokenize(sql)
    except Exception:
        return [sql.strip()] if sql.strip() else []

    statements = []
    start = 0
    for token in tokens:
        if token.token_type == TokenType.SEMICOLON:
            statement = sql[start : token.start].strip()
            if statement:
                statements.append(statement)
            start = token.end + 1

    statement = sql[start:].strip()
    if statement:
        statements.append(statement)

    return statements


def _group_parsed(comparison: StatementComparison, parsed: list[tuple[str, exp.Expr]]) -> None:
    groups: list[tuple[exp.Expr, InterpretationGroup]] = []
    group_by_dialect = {}

    for name, expression in parsed:
        for representative, group in groups:
            if expression == representative:
                group.dialects.append(name)
                group_index = group.index
                break
        else:
            group_index = len(groups)
            group = InterpretationGroup(index=group_index, dialects=[name], sql=expression.sql())
            groups.append((expression, group))

        group_by_dialect[name] = group_index

    for candidate in comparison.candidates:
        if candidate.status is CandidateStatus.PARSED:
            candidate.group = group_by_dialect[candidate.dialect]

    comparison.groups = [group for _, group in groups]

    for (left_expression, left), (right_expression, right) in zip(groups, groups[1:]):
        differences: list[Difference] = []
        _diff_nodes(left_expression, right_expression, type(left_expression).__name__, differences)
        comparison.differences.append((left.index, right.index, differences))


def _diff_nodes(
    source: exp.Expr,
    target: exp.Expr,
    location: str,
    differences: list[Difference],
) -> None:
    if len(differences) >= MAX_DIFFERENCES:
        return

    if type(source) is not type(target):
        differences.append(Difference(location, source.sql(), target.sql()))
        return

    for key in sorted(set(source.args) | set(target.args)):
        if len(differences) >= MAX_DIFFERENCES:
            return

        source_value = source.args.get(key)
        target_value = target.args.get(key)
        child_location = f"{location}.{key}"

        if isinstance(source_value, exp.Expr) and isinstance(target_value, exp.Expr):
            _diff_nodes(source_value, target_value, child_location, differences)
        elif isinstance(source_value, list) and isinstance(target_value, list):
            for index, (source_item, target_item) in enumerate(zip(source_value, target_value)):
                if isinstance(source_item, exp.Expr) and isinstance(target_item, exp.Expr):
                    _diff_nodes(source_item, target_item, f"{child_location}[{index}]", differences)
                elif source_item != target_item:
                    differences.append(
                        Difference(
                            f"{child_location}[{index}]", repr(source_item), repr(target_item)
                        )
                    )
            if len(source_value) != len(target_value):
                differences.append(
                    Difference(
                        child_location,
                        f"{len(source_value)} item(s)",
                        f"{len(target_value)} item(s)",
                    )
                )
        elif source_value != target_value:
            differences.append(Difference(child_location, repr(source_value), repr(target_value)))


if __name__ == "__main__":
    import argparse
    import sys

    argument_parser = argparse.ArgumentParser(
        description="Compare how candidate dialects interpret the same SQL."
    )
    argument_parser.add_argument(
        "sql", metavar="sql", type=str, help="SQL statement(s) to compare, or - for stdin."
    )
    argument_parser.add_argument(
        "--dialects",
        required=True,
        type=str,
        help="Comma-separated list of candidate dialects, e.g. mysql,postgres,duckdb.",
    )
    arguments = argument_parser.parse_args()

    sql_input = sys.stdin.read() if arguments.sql == "-" else arguments.sql
    print(compare_dialects(sql_input, arguments.dialects.split(",")).render())
