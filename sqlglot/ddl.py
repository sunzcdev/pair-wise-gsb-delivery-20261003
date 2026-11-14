"""Utilities for analyzing DDL statements, notably generated column dependencies."""

from __future__ import annotations

import typing as t
from dataclasses import dataclass

from sqlglot import expressions as exp
from sqlglot.dialects.dialect import Dialect
from sqlglot.errors import GeneratedColumnError

if t.TYPE_CHECKING:
    from sqlglot.dialects.dialect import DialectType


@dataclass(frozen=True)
class GeneratedColumnDependency:
    """
    A single dependency of a generated column on another column of the same table.

    Attributes:
        column: the normalized name of the referenced column.
        generated: whether the referenced column is itself generated.
        reference: the `Column` expression used inside the generation expression.
    """

    column: str
    generated: bool
    reference: exp.Column


@dataclass(frozen=True)
class GeneratedColumnIssue:
    """
    A problem detected while analyzing the generated columns of a table.

    Attributes:
        column: the normalized name of the generated column the problem is attached to.
        kind: one of `"self_reference"`, `"cycle"` or `"unknown_column"`.
        message: a human readable description of the problem.
        chain: the normalized column names participating in the problem. For self
            references and cycles this is the closed dependency chain; for unknown
            columns it contains the generated column and the unknown column.
    """

    column: str
    kind: t.Literal["self_reference", "cycle", "unknown_column"]
    message: str
    chain: tuple[str, ...]


@dataclass(frozen=True)
class GeneratedColumnAnalysis:
    """
    The dependency analysis of the generated columns of a single table.

    Attributes:
        table: the name of the analyzed table, as written in the DDL.
        columns: the normalized names of the table's columns, in DDL order.
        column_types: maps each column to its declared `DataType`, or None when the
            DDL does not declare one.
        generated_columns: the normalized names of the columns with a generation
            expression that could be parsed.
        dependencies: maps each generated column to the columns it references.
        computable_order: generated columns in an order in which they can be computed;
            columns involved in a cycle or referencing unknown columns are excluded.
        issues: all detected self references, cycles and unknown column references.
    """

    table: str
    columns: tuple[str, ...]
    column_types: dict[str, exp.DataType | None]
    generated_columns: tuple[str, ...]
    dependencies: dict[str, tuple[GeneratedColumnDependency, ...]]
    computable_order: tuple[str, ...]
    issues: tuple[GeneratedColumnIssue, ...]

    @property
    def valid(self) -> bool:
        """Returns whether every generated column can be computed without issues."""
        return not self.issues

    def raise_on_issues(self) -> None:
        """Raises a `GeneratedColumnError` if any issue was detected."""
        if self.issues:
            raise GeneratedColumnError("\n".join(issue.message for issue in self.issues))


@dataclass(frozen=True)
class TableSchemaError:
    """
    A failure that prevented a single table from being analyzed inside a batch.

    Attributes:
        table: the name of the table, when it could be recovered from the DDL.
        error: the exception raised while analyzing the table.
    """

    table: str | None
    error: Exception


@dataclass(frozen=True)
class TableSchemaExtraction:
    """
    The result of analyzing a batch of `CREATE TABLE` statements.

    Attributes:
        analyses: maps each successfully analyzed table to its analysis.
        errors: collects the isolated failure of each table that could not be
            analyzed, so one bad table never poisons the other catalogs.
    """

    analyses: dict[str, GeneratedColumnAnalysis]
    errors: tuple[TableSchemaError, ...]

    def raise_on_errors(self) -> None:
        """Raises a `GeneratedColumnError` if any table failed to be analyzed."""
        if self.errors:
            raise GeneratedColumnError(
                "\n".join(str(error.error) for error in self.errors)
            )


def analyze_generated_columns(
    expression: str | exp.Create,
    dialect: DialectType = None,
) -> GeneratedColumnAnalysis:
    """
    Analyzes the dependencies between the generated columns of a `CREATE TABLE`.

    Only generated column syntax the library can parse is analyzed; the DDL is never
    executed against a real database. The analysis is scoped to a single table, so
    columns of other tables are never treated as dependencies.

    Args:
        expression: a `CREATE TABLE` expression or a SQL string containing one.
        dialect: the dialect used to parse `expression` when it is a string and to
            resolve identifier case (e.g. unquoted identifiers are lowercased in
            Postgres but uppercased in Snowflake).

    Returns:
        A `GeneratedColumnAnalysis` describing the dependency graph, a computable
        ordering and every detected issue.
    """
    if isinstance(expression, str):
        expression = t.cast(
            exp.Create, Dialect.get_or_raise(dialect).parse(expression)[0]
        )

    if not isinstance(expression, exp.Create) or expression.args.get("kind") != "TABLE":
        raise ValueError(f"Expected a CREATE TABLE expression, got: {expression}")

    schema = expression.this if isinstance(expression.this, exp.Schema) else None
    table = schema.this if schema else expression.this
    table_name = table.sql(dialect=dialect) if isinstance(table, exp.Table) else table.name

    if not schema:
        # CREATE TABLE ... AS SELECT / LIKE / CLONE has no inline column definitions.
        return GeneratedColumnAnalysis(table_name, (), {}, (), {}, (), ())

    dialect_ = Dialect.get_or_raise(dialect)

    def normalized(identifier: exp.Identifier) -> str:
        return dialect_.normalize_identifier(identifier.copy()).name

    column_defs = [
        column_def
        for column_def in schema.expressions
        if isinstance(column_def, exp.ColumnDef) and isinstance(column_def.this, exp.Identifier)
    ]

    columns = tuple(normalized(column_def.this) for column_def in column_defs)
    column_set = set(columns)
    column_types = {
        normalized(column_def.this): column_def.kind for column_def in column_defs
    }
    generated = {
        normalized(column_def.this)
        for column_def in column_defs
        if _generated_expression(column_def) is not None
    }

    issues: list[GeneratedColumnIssue] = []
    dependencies: dict[str, tuple[GeneratedColumnDependency, ...]] = {}
    graph: dict[str, set[str]] = {}

    for column_def in column_defs:
        generated_expression = _generated_expression(column_def)
        if generated_expression is None:
            continue

        column_name = normalized(column_def.this)

        # String literals, function names and the like never appear as `Column` nodes,
        # so they are naturally excluded from the collected references.
        references: dict[str, exp.Column] = {}
        for reference in generated_expression.find_all(exp.Column):
            references.setdefault(normalized(reference.this), reference)

        column_dependencies = [
            GeneratedColumnDependency(
                column=referenced_name,
                generated=referenced_name in generated,
                reference=references[referenced_name],
            )
            for referenced_name in sorted(references)
        ]
        dependencies[column_name] = tuple(column_dependencies)

        for referenced_name in references:
            if referenced_name not in column_set:
                issues.append(
                    GeneratedColumnIssue(
                        column=column_name,
                        kind="unknown_column",
                        message=(
                            f"Generated column '{column_name}' references unknown column "
                            f"'{referenced_name}' in table '{table_name}'."
                        ),
                        chain=(column_name, referenced_name),
                    )
                )

        # Only references to known, in-table columns participate in the graph.
        graph[column_name] = {
            referenced_name for referenced_name in references if referenced_name in column_set
        }

    issues.extend(_detect_cycles(graph, table_name))

    blocked = {issue.column for issue in issues}
    computable_order = _topological_sort(graph, blocked)

    return GeneratedColumnAnalysis(
        table=table_name,
        columns=columns,
        column_types=column_types,
        generated_columns=tuple(name for name in columns if name in generated),
        dependencies=dependencies,
        computable_order=tuple(computable_order),
        issues=tuple(issues),
    )


def extract_table_schemas(
    expressions: str | t.Sequence[str | exp.Expr],
    dialect: DialectType = None,
) -> TableSchemaExtraction:
    """
    Analyzes the generated columns of every `CREATE TABLE` statement in a batch.

    Each table is analyzed independently: a failing table never prevents the other
    tables from being analyzed and same-named columns never link tables together.

    Args:
        expressions: a SQL string containing one or more statements, or a sequence of
            SQL strings / parsed expressions.
        dialect: the dialect used to parse the statements and resolve identifier case.

    Returns:
        A `TableSchemaExtraction` holding the analysis of every table and the isolated
        error of each table that could not be analyzed.
    """
    dialect_ = Dialect.get_or_raise(dialect)
    if isinstance(expressions, str):
        parsed_expressions: t.Sequence[exp.Expr | None] = dialect_.parse(expressions)
    else:
        parsed_expressions = [
            expression
            if isinstance(expression, exp.Expr)
            else dialect_.parse(expression)[0]
            for expression in expressions
        ]

    analyses: dict[str, GeneratedColumnAnalysis] = {}
    errors: list[TableSchemaError] = []
    for parsed in parsed_expressions:
        if isinstance(parsed, exp.Create) and parsed.args.get("kind") == "TABLE":
            try:
                analysis = analyze_generated_columns(parsed, dialect=dialect)
            except Exception as exception:
                # Isolate each table so an error in one never poisons the other catalogs.
                errors.append(
                    TableSchemaError(
                        table=_table_name(parsed, dialect),
                        error=exception
                        if isinstance(exception, GeneratedColumnError)
                        else GeneratedColumnError(str(exception)),
                    )
                )
                continue
            analyses[analysis.table] = analysis

    return TableSchemaExtraction(analyses=analyses, errors=tuple(errors))


def _table_name(expression: exp.Create, dialect: DialectType) -> str | None:
    table = expression.this
    if isinstance(table, exp.Schema):
        table = table.this
    return table.sql(dialect=dialect) if isinstance(table, exp.Table) else None


def _generated_expression(column_def: exp.ColumnDef) -> exp.Expr | None:
    """Returns the parsed generation expression of a column, or None."""
    for constraint in column_def.constraints:
        kind = constraint.kind
        if isinstance(kind, exp.ComputedColumnConstraint):
            return kind.this
        if isinstance(kind, exp.GeneratedAsIdentityColumnConstraint) and kind.expression:
            # Some dialects (e.g. Postgres, Oracle) represent generated columns this way.
            return kind.expression
    return None


def _detect_cycles(
    graph: dict[str, set[str]], table_name: str
) -> list[GeneratedColumnIssue]:
    issues: list[GeneratedColumnIssue] = []
    visiting: set[str] = set()
    visited: set[str] = set()
    stack: list[str] = []

    def visit(node: str) -> None:
        if node in visited:
            return

        if node in visiting:
            cycle_start = stack.index(node)
            chain = tuple(stack[cycle_start:] + [node])
            kind = "self_reference" if len(chain) == 2 else "cycle"
            message = (
                f"Generated column {'self reference' if kind == 'self_reference' else 'cycle'} "
                f"detected in table '{table_name}': {' -> '.join(chain)}."
            )
            for column in chain[:-1]:
                issues.append(
                    GeneratedColumnIssue(
                        column=column,
                        kind=kind,
                        message=message,
                        chain=chain,
                    )
                )
            return

        visiting.add(node)
        stack.append(node)
        for dependency in sorted(graph.get(node, ())):
            visit(dependency)
        stack.pop()
        visiting.discard(node)
        visited.add(node)

    for node in sorted(graph):
        visit(node)

    # The same closed chain is reported once per column participating in it.
    deduped: dict[tuple[str, ...], GeneratedColumnIssue] = {}
    for issue in issues:
        deduped.setdefault(issue.chain, issue)
    return list(deduped.values())


def _topological_sort(
    graph: dict[str, set[str]], blocked: t.Collection[str]
) -> list[str]:
    """Topologically sorts generated columns, skipping blocked nodes and dependents."""
    remaining = {
        node: {dependency for dependency in deps if dependency in graph}
        for node, deps in graph.items()
        if node not in blocked
    }

    ordered: list[str] = []
    while remaining:
        ready = sorted(node for node, deps in remaining.items() if not deps)
        if not ready:
            break
        ordered.extend(ready)
        for node in ready:
            remaining.pop(node)
        for deps in remaining.values():
            deps.difference_update(ready)

    return ordered
