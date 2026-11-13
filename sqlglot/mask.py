from __future__ import annotations

import typing as t

from sqlglot import exp
from sqlglot.dialects.dialect import Dialect, DialectType
from sqlglot.errors import MaskingError
from sqlglot.optimizer.normalize_identifiers import normalize_identifiers
from sqlglot.optimizer.qualify import qualify
from sqlglot.optimizer.scope import (
    Scope,
    build_scope,
    find_all_in_scope,
    find_in_scope,
    traverse_scope,
)

if t.TYPE_CHECKING:
    from collections.abc import Callable, Iterator, Mapping

    from sqlglot.schema import Schema

    MaskSpec = t.Union[str, exp.Expr, t.Callable[[exp.Expr], exp.Expr]]


# Placeholder used in masking templates to refer to the protected source column
SOURCE_PLACEHOLDER = "source"

# A fully qualified table name, normalized to its catalog / db / table parts
TableKey = t.Tuple[str, ...]

# A (table, column) pair a projection column originates from
ColumnSource = t.Tuple[TableKey, str]


def mask_columns(
    expression: str | exp.Expr,
    masks: Mapping[tuple[str, str], MaskSpec],
    schema: dict[str, t.Any] | Schema | None = None,
    dialect: DialectType = None,
    copy: bool = True,
) -> exp.Expr:
    """
    Rewrite the final projections of an export query so that columns originating from
    protected (table, column) pairs are wrapped in masking expressions.

    Only the outermost projection list is rewritten: join conditions, filters, grouping
    keys and the projections of nested CTEs / derived tables keep their raw values, so
    the query retains its original semantics while its published output is masked.
    Stars are expanded using `schema` first, and protected sources are tracked through
    CTE renames and multi-level projections, so a same-named column projected from an
    unprotected table is left alone. `ORDER BY` terms referencing output aliases keep
    targeting the (masked) output columns.

    Example:
        >>> from sqlglot.mask import mask_columns
        >>> mask_columns("SELECT ssn FROM users", {("users", "ssn"): "MASK(:source)"}).sql()
        'SELECT MASK("ssn") AS "ssn" FROM "users" AS "users"'

    Args:
        expression: SQL string or expression to mask. Must be a query.
        masks: Mapping of (fully qualified table name, column name) to masking spec.
            A spec is either a callable that receives the source column expression and
            returns the masked expression, or a SQL string / expression template in
            which the `:source` placeholder stands for the protected column, e.g.
            `"CONCAT('***', RIGHT(:source, 4))"`. A template without a placeholder is
            used as a constant replacement. Table names are matched by suffix, so a
            spec for `"db.users"` also covers a query that references plain `users`.
        schema: Schema used to expand `*` projections and resolve column sources.
        dialect: The dialect of the input SQL and of string masking templates.
        copy: Whether to operate on a copy of the input expression. The caller's AST
            is never mutated unless this is set to False.

    Returns:
        The masked query expression.

    Raises:
        MaskingError: If a potentially sensitive projection cannot be traced back to
            its source, so publishable SQL cannot be guaranteed.
    """
    dialect = Dialect.get_or_raise(dialect)
    if isinstance(expression, str):
        query = exp.maybe_parse(expression, dialect=dialect)
    else:
        query = expression
    if copy:
        query = query.copy()

    protected = {
        (
            _normalize_table_name(table, dialect),
            _normalize_column_name(column, dialect),
        ): _compile_spec(spec, dialect)
        for (table, column), spec in masks.items()
    }
    ctx = _MaskContext(
        protected=protected,
        sensitive_names={column for _, column in protected},
        scopes={},
    )

    query = qualify(query, schema=schema, dialect=dialect, validate_qualify_columns=False)

    scope = build_scope(query)
    if not scope or not isinstance(scope.expression, (exp.Select, exp.SetOperation, exp.Subquery)):
        raise MaskingError("Cannot mask a non-query expression.")

    ctx.scopes.update({id(s.expression): s for s in traverse_scope(query)})

    for select_scope in _final_select_scopes(scope):
        for projection in select_scope.expression.selects:
            _mask_projection(projection, select_scope, ctx)

    return query


class _MaskContext(t.NamedTuple):
    protected: dict[tuple[TableKey, str], Callable[[exp.Expr], exp.Expr]]
    sensitive_names: set[str]
    scopes: dict[int, Scope]


def _normalize_table_name(table: str, dialect: Dialect) -> TableKey:
    parsed = normalize_identifiers(exp.to_table(table, dialect=dialect), dialect=dialect)
    return tuple(part for part in (parsed.catalog, parsed.db, parsed.name) if part)


def _normalize_column_name(column: str, dialect: Dialect) -> str:
    identifier = exp.parse_identifier(column, dialect=dialect)
    return normalize_identifiers(identifier, dialect=dialect).name


def _compile_spec(spec: MaskSpec, dialect: Dialect) -> Callable[[exp.Expr], exp.Expr]:
    if callable(spec):
        return spec

    template = exp.maybe_parse(spec, dialect=dialect) if isinstance(spec, str) else spec

    def _apply(column: exp.Expr) -> exp.Expr:
        return exp.replace_placeholders(template.copy(), **{SOURCE_PLACEHOLDER: column})

    return _apply


def _table_key(table: exp.Table) -> TableKey:
    return tuple(part for part in (table.catalog, table.db, table.name) if part)


def _tables_match(protected: TableKey, actual: TableKey) -> bool:
    shorter, longer = (protected, actual) if len(protected) <= len(actual) else (actual, protected)
    return longer[len(longer) - len(shorter) :] == shorter


def _find_spec(
    resolved: set[ColumnSource], ctx: _MaskContext
) -> Callable[[exp.Expr], exp.Expr] | None:
    best: tuple[TableKey, Callable[[exp.Expr], exp.Expr]] | None = None
    for table_key, column in resolved:
        for (protected_table, protected_column), spec in ctx.protected.items():
            if protected_column != column or not _tables_match(protected_table, table_key):
                continue
            if best is None or len(protected_table) > len(best[0]):
                best = (protected_table, spec)
            elif len(protected_table) == len(best[0]) and best[1] is not spec:
                raise MaskingError(
                    f"Ambiguous masking specs for column '{column}'"
                    f" from table '{'.'.join(table_key)}'."
                )
    return best[1] if best else None


def _final_select_scopes(scope: Scope) -> Iterator[Scope]:
    expression = scope.expression
    if isinstance(expression, exp.Select):
        yield scope
    elif isinstance(expression, exp.SetOperation):
        for branch in scope.set_operation_scopes:
            yield from _final_select_scopes(branch)
    elif isinstance(expression, exp.Subquery):
        for subquery_scope in scope.subquery_scopes:
            yield from _final_select_scopes(subquery_scope)


def _mask_projection(projection: exp.Expr, scope: Scope, ctx: _MaskContext) -> None:
    if projection.is_star:
        _check_star_projection(projection, scope, ctx)
        return

    for column in list(find_all_in_scope(projection, exp.Column)):
        if column.is_star:
            _check_star_projection(column, scope, ctx)
            continue
        resolved = _resolve_column(column, scope, ctx, set())
        if resolved is None:
            _fail_if_sensitive(column, projection, ctx)
            continue
        spec = _find_spec(resolved, ctx)
        if spec:
            column.replace(spec(column.copy()))

    for subquery in list(find_all_in_scope(projection, exp.Subquery)):
        _mask_subquery(subquery, projection, ctx)


def _mask_subquery(subquery: exp.Subquery, projection: exp.Expr, ctx: _MaskContext) -> None:
    subquery_scope = ctx.scopes.get(id(subquery.this))
    resolved = None
    if subquery_scope is not None and isinstance(subquery_scope.expression, exp.Select):
        selects = subquery_scope.expression.selects
        if len(selects) == 1:
            resolved = _resolve_projection(selects[0], subquery_scope, ctx, set())

    if resolved is None:
        _fail_if_sensitive(subquery, projection, ctx)
        return

    spec = _find_spec(resolved, ctx)
    if spec:
        subquery.replace(spec(subquery.copy()))


def _resolve_column(
    column: exp.Column, scope: Scope, ctx: _MaskContext, seen: set[tuple[int, str]]
) -> set[ColumnSource] | None:
    table = column.table
    if table:
        source = scope.sources.get(table)
        if source is None:
            return None
    else:
        named_sources = [name for name in scope.sources if name]
        if len(named_sources) != 1:
            return None
        source = scope.sources[named_sources[0]]

    return _resolve_from_source(column.name, source, ctx, seen)


def _resolve_from_source(
    name: str, source: exp.Table | Scope, ctx: _MaskContext, seen: set[tuple[int, str]]
) -> set[ColumnSource] | None:
    if isinstance(source, exp.Table):
        return {(_table_key(source), name)}

    key = (id(source), name)
    if key in seen:
        return set()
    seen = seen | {key}

    expression = source.expression
    if isinstance(expression, exp.Subquery):
        if len(source.subquery_scopes) != 1:
            return None
        return _resolve_from_source(name, source.subquery_scopes[0], ctx, seen)

    if isinstance(expression, exp.SetOperation):
        names = expression.named_selects
        if name not in names:
            return None
        index = names.index(name)
        resolved: set[ColumnSource] = set()
        for branch in source.set_operation_scopes:
            branch_resolved = _resolve_projection(
                branch.expression.selects[index], branch, ctx, seen
            )
            if branch_resolved is None:
                return None
            resolved |= branch_resolved
        return resolved

    if isinstance(expression, exp.Select):
        projection = next(
            (select for select in expression.selects if select.alias_or_name == name), None
        )
        if projection is None:
            return None
        return _resolve_projection(projection, source, ctx, seen)

    return None


def _resolve_projection(
    projection: exp.Expr, scope: Scope, ctx: _MaskContext, seen: set[tuple[int, str]]
) -> set[ColumnSource] | None:
    if projection.is_star or find_in_scope(projection, exp.Subquery):
        return None

    resolved: set[ColumnSource] = set()
    for column in find_all_in_scope(projection, exp.Column):
        if column.is_star:
            return None
        column_resolved = _resolve_column(column, scope, ctx, seen)
        if column_resolved is None:
            return None
        resolved |= column_resolved
    return resolved


def _check_star_projection(projection: exp.Expr, scope: Scope, ctx: _MaskContext) -> None:
    if not ctx.protected:
        return

    if isinstance(projection, exp.Column) and projection.table:
        candidates = [projection.table]
    else:
        candidates = list(scope.selected_sources)

    for name in candidates:
        source = scope.sources.get(name)
        if isinstance(source, exp.Table):
            sensitive = any(
                _tables_match(protected_table, _table_key(source))
                for protected_table, _ in ctx.protected
            )
        elif isinstance(source, Scope) and isinstance(source.expression, exp.Selectable):
            sensitive = any(
                _star_scope_is_sensitive(output_name, source, ctx)
                for output_name in source.expression.named_selects
            )
        else:
            sensitive = True

        if sensitive:
            raise MaskingError(
                f"Cannot verify the columns of star projection '{projection.sql()}'"
                f" from source '{name}'{_position(projection)};"
                " refusing to produce publishable SQL."
            )


def _star_scope_is_sensitive(output_name: str, source: Scope, ctx: _MaskContext) -> bool:
    resolved = _resolve_from_source(output_name, source, ctx, set())
    if resolved is None:
        return output_name == "*" or output_name in ctx.sensitive_names
    return _find_spec(resolved, ctx) is not None


def _fail_if_sensitive(node: exp.Expr, projection: exp.Expr, ctx: _MaskContext) -> None:
    name = node.name if isinstance(node, exp.Column) else ""
    alias = projection.alias_or_name
    if name in ctx.sensitive_names or (alias and alias in ctx.sensitive_names):
        raise MaskingError(
            f"Cannot verify the source of potentially sensitive projection"
            f" '{projection.sql()}'{_position(node)}; refusing to produce publishable SQL."
        )


def _position(node: exp.Expr) -> str:
    identifier = node.this
    if isinstance(identifier, exp.Identifier):
        line, col = identifier.meta_get("line"), identifier.meta_get("col")
        if line and col:
            return f" at Line: {line}, Col: {col}"
    return ""
