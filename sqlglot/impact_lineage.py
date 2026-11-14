from __future__ import annotations

import enum
import typing as t
from dataclasses import dataclass, field

from sqlglot import Schema, exp, maybe_parse
from sqlglot.errors import SqlglotError
from sqlglot.lineage import GraphHTML
from sqlglot.optimizer import Scope, build_scope, find_all_in_scope, normalize_identifiers, qualify
from sqlglot.schema import ensure_schema

if t.TYPE_CHECKING:
    from collections.abc import Iterator, Mapping

    from sqlglot._typing import GraphHTMLArgs
    from sqlglot.dialects.dialect import DialectType
    from typing_extensions import Unpack


class ImpactType(str, enum.Enum):
    """How a source column affects an output column."""

    VALUE = "value"
    """The source column's values flow into the output value (projection lineage)."""

    ROW_SET = "row_set"
    """The source column only decides whether an input row survives (e.g. WHERE/JOIN/HAVING)."""


class NodeKind(str, enum.Enum):
    """The kind of endpoint an :class:`ImpactNode` represents."""

    COLUMN = "column"
    ROWS = "rows"
    UNKNOWN = "unknown"


@dataclass
class ImpactEdge:
    """A dependency of an :class:`ImpactNode` with one or more impact roles."""

    node: ImpactNode
    roles: set[ImpactType] = field(default_factory=set)
    uncertain: bool = False


@dataclass
class ImpactNode:
    """A node in an impact lineage graph.

    ``downstream`` holds the nodes this one depends on; each entry records which
    :class:`ImpactType` roles apply so the same source column can be both a value
    dependency and a row-set dependency.
    """

    name: str
    kind: NodeKind
    expression: exp.Expr
    source: exp.Expr
    downstream: list[ImpactEdge] = field(default_factory=list)

    def edges(self, role: ImpactType | None = None) -> list[ImpactEdge]:
        if role is None:
            return list(self.downstream)
        return [edge for edge in self.downstream if role in edge.roles]

    def dependencies(self, role: ImpactType | None = None) -> list[ImpactNode]:
        return [edge.node for edge in self.edges(role)]

    def walk(self) -> Iterator[ImpactNode]:
        visited: set[int] = set()
        queue = [self]
        while queue:
            node = queue.pop()
            if id(node) in visited:
                continue
            visited.add(id(node))
            yield node
            queue.extend(reversed([edge.node for edge in node.downstream]))

    @property
    def uncertain(self) -> bool:
        """True if any reachable dependency had to be guessed (e.g. missing schema)."""
        return any(edge.uncertain or edge.node.uncertain for edge in self.downstream)

    def to_html(
        self, dialect: DialectType = None, **opts: Unpack[GraphHTMLArgs]
    ) -> GraphHTML:
        nodes: dict[int, dict[str, t.Any]] = {}
        edges: list[dict[str, t.Any]] = []

        for node in self.walk():
            if isinstance(node.expression, exp.Table):
                label = f"FROM {node.expression.this}"
                group = 1
            else:
                label = node.expression.sql(pretty=True, dialect=dialect)
                group = 0

            if node.kind is NodeKind.ROWS:
                label = f"{node.name} (input rows)"
            elif node.kind is NodeKind.UNKNOWN:
                label = f"UNKNOWN: {node.name}"

            nodes[id(node)] = {
                "id": id(node),
                "label": label,
                "title": node.source.sql(pretty=True, dialect=dialect),
                "group": group,
            }

            for edge in node.downstream:
                edges.append(
                    {
                        "from": id(node),
                        "to": id(edge.node),
                        "label": ", ".join(sorted(role.value for role in edge.roles)),
                        "dashes": "true" if edge.uncertain else "false",
                        "color": "gray" if edge.uncertain else None,
                    }
                )

        return GraphHTML(nodes, edges, **opts)


@dataclass
class ImpactLineage:
    """Impact lineage for every output column of a query."""

    columns: dict[str, ImpactNode]

    def __getitem__(self, name: str) -> ImpactNode:
        return self.columns[name]

    def __iter__(self) -> Iterator[str]:
        return iter(self.columns)

    def __len__(self) -> int:
        return len(self.columns)

    @property
    def unknowns(self) -> list[ImpactNode]:
        """Every uncertain/unknown endpoint reachable from any output column."""
        seen: dict[int, ImpactNode] = {}
        for root in self.columns.values():
            for node in root.walk():
                if node.kind is NodeKind.UNKNOWN:
                    seen.setdefault(id(node), node)
        return list(seen.values())


@t.overload
def impact_lineage(
    column: str | exp.Column,
    sql: str | exp.Expr,
    schema: dict | Schema,
    sources: Mapping[str, str | exp.Query] | None = None,
    dialect: DialectType = None,
    scope: Scope | None = None,
    copy: bool = True,
    **kwargs: t.Any,
) -> ImpactNode: ...


@t.overload
def impact_lineage(
    column: None,
    sql: str | exp.Expr,
    schema: dict | Schema,
    sources: Mapping[str, str | exp.Query] | None = None,
    dialect: DialectType = None,
    scope: Scope | None = None,
    copy: bool = True,
    **kwargs: t.Any,
) -> ImpactLineage: ...


def impact_lineage(
    column: str | exp.Column | None,
    sql: str | exp.Expr,
    schema: dict | Schema,
    sources: Mapping[str, str | exp.Query] | None = None,
    dialect: DialectType = None,
    scope: Scope | None = None,
    copy: bool = True,
    **kwargs: t.Any,
) -> ImpactNode | ImpactLineage:
    """Build impact lineage, separating value dependencies from row-set dependencies.

    Unlike :func:`sqlglot.lineage.lineage`, the returned graph labels every edge with
    the role(s) the source column plays for the output column:

    * :attr:`ImpactType.VALUE` - the source value flows into the projected value.
    * :attr:`ImpactType.ROW_SET` - the source only filters/joins/groups rows
      (WHERE, JOIN ... ON, HAVING, QUALIFY, GROUP BY, DISTINCT, ...).

    The same source column can carry both roles. When a dependency cannot be resolved
    deterministically (for example an unqualified column with multiple sources and no
    schema), the edge is marked ``uncertain`` and terminates at an UNKNOWN node rather
    than being drawn as a definite edge.

    Args:
        column: The output column to trace, or None for every top-level output column.
        sql: The SQL string or expression.
        schema: The schema of tables.
        sources: A mapping of queries used to continue building lineage.
        dialect: The dialect of input SQL.
        scope: A pre-created scope to use instead.
        copy: Whether to copy the Expr arguments.
        **kwargs: Qualification optimizer kwargs.
    """
    root_scope, resolved_schema = _prepare(
        sql, schema, sources, dialect, scope, copy, **kwargs
    )

    builder = _Builder(root_scope, resolved_schema, dialect)

    if column is not None:
        column_name = normalize_identifiers.normalize_identifiers(column, dialect=dialect).name
        if not any(
            select.alias_or_name == column_name for select in root_scope.expression.selects
        ) and not root_scope.expression.is_star:
            raise SqlglotError(f"Cannot find column '{column_name}' in query.")
        return builder.build_output(column_name)

    outputs: dict[str, ImpactNode] = {}
    selects = (
        root_scope.expression.selects
        if not root_scope.expression.is_star
        else [exp.Star()]
    )
    for select in selects:
        name = select.alias_or_name
        if not name:
            raise SqlglotError(
                f"Cannot fetch lineage for unnamed projection: {select.sql(dialect=dialect)}."
            )
        outputs[name] = builder.build_output(name)

    return ImpactLineage(outputs)


def _prepare(
    sql: str | exp.Expr,
    schema: dict | Schema,
    sources: Mapping[str, str | exp.Query] | None,
    dialect: DialectType | None,
    scope: Scope | None,
    copy: bool,
    **kwargs: t.Any,
) -> tuple[Scope, Schema]:
    expression = maybe_parse(sql, copy=copy, dialect=dialect)

    if sources:
        expression = exp.expand(
            expression,
            {
                k: t.cast(exp.Query, maybe_parse(v, copy=copy, dialect=dialect))
                for k, v in sources.items()
            },
            dialect=dialect,
            copy=copy,
        )

    resolved_schema = ensure_schema(schema, dialect=dialect)

    if not scope:
        expression = qualify.qualify(
            expression,
            dialect=dialect,
            schema=resolved_schema,
            **{"validate_qualify_columns": False, "identify": False, **kwargs},
        )
        scope = build_scope(expression)

    if not scope:
        raise SqlglotError("Cannot build lineage, sql must be SELECT")

    if not isinstance(scope.expression, exp.Selectable):
        raise SqlglotError("Cannot build lineage, sql must be a query")

    return scope, resolved_schema


class _Builder:
    def __init__(self, root_scope: Scope, schema: Schema, dialect: DialectType) -> None:
        self.root_scope = root_scope
        self.schema = schema
        self.dialect = dialect

        # query expression -> Scope, used to resolve correlated outer references across
        # SetOperation branches whose Scope.parent is not populated.
        self._scopes_by_expr: dict[int, Scope] = {
            id(scope.expression): scope for scope in root_scope.traverse()
        }

        # (scope id, column name or sentinel) -> node, shared across output columns
        self._node_cache: dict[tuple[int, str], ImpactNode] = {}
        self._filter_cache: dict[
            int, list[tuple[ImpactNode, set[ImpactType], bool]]
        ] = {}

    # ------------------------------------------------------------------ public
    def build_output(self, column: str | int) -> ImpactNode:
        return self._build_column(column, self.root_scope)

    # -------------------------------------------------------------- node lookup
    def _node_for(
        self, scope: Scope, name: str, expression: exp.Expr, source: exp.Expr
    ) -> ImpactNode:
        key = (id(scope), name)
        node = self._node_cache.get(key)
        if node is None:
            node = ImpactNode(
                name=self._display_name(scope, name),
                kind=NodeKind.COLUMN,
                expression=expression,
                source=source,
            )
            self._node_cache[key] = node
        return node

    def _leaf(self, name: str, expression: exp.Expr, kind: NodeKind) -> ImpactNode:
        key = (id(expression), name)
        node = self._node_cache.get(key)
        if node is None:
            node = ImpactNode(name=name, kind=kind, expression=expression, source=expression)
            self._node_cache[key] = node
        return node

    @staticmethod
    def _display_name(scope: Scope, name: str) -> str:
        scope_name = scope.expression.parent.alias_or_name if scope.expression.parent else ""
        return f"{scope_name}.{name}" if scope_name else str(name)

    @staticmethod
    def _connect(
        parent: ImpactNode,
        node: ImpactNode,
        roles: set[ImpactType],
        uncertain: bool = False,
    ) -> ImpactEdge:
        for edge in parent.downstream:
            if edge.node is node:
                edge.roles.update(roles)
                edge.uncertain = edge.uncertain or uncertain
                return edge
        edge = ImpactEdge(node=node, roles=set(roles), uncertain=uncertain)
        parent.downstream.append(edge)
        return edge

    # ------------------------------------------------------------- column build
    def _build_column(self, column: str | int, scope: Scope) -> ImpactNode:
        if isinstance(scope.expression, exp.SetOperation):
            return self._build_set_operation(column, scope)

        selectable = t.cast(exp.Select, scope.expression)
        select = (
            selectable.selects[column]
            if isinstance(column, int)
            else self._resolve_select(selectable, column)
        )

        name = select.alias_or_name
        node = self._node_for(scope, name, select, selectable)

        if isinstance(select, exp.Star) or (isinstance(select, exp.Column) and select.is_star):
            self._add_star_dependencies(node, scope, select)
        elif self._is_row_level(select):
            self._add_value_dependencies(node, scope, select)
            self._add_row_dependency(node, scope)
        else:
            self._add_value_dependencies(node, scope, select)

        self._add_filter_dependencies(node, scope)
        return node

    @staticmethod
    def _resolve_select(selectable: exp.Select, column: str) -> exp.Expr:
        for select in selectable.selects:
            if select.alias_or_name == column:
                return select
        if selectable.is_star:
            return exp.Star()
        return selectable

    # ------------------------------------------------------------ value edges
    def _add_value_dependencies(self, node: ImpactNode, scope: Scope, expression: exp.Expr) -> None:
        subquery_scopes = {
            id(subscope.expression): subscope for subscope in scope.subquery_scopes
        }

        # Scalar subqueries embedded in the projection are value dependencies.
        for subquery in find_all_in_scope(expression, *exp.UNWRAPPED_QUERIES):
            subscope = subquery_scopes.get(id(subquery))
            if subscope is None:
                continue
            self._connect(
                node,
                self._build_subquery_value(subscope),
                {ImpactType.VALUE},
            )

        for column in find_all_in_scope(expression, exp.Column):
            if self._in_aggregate_filter(column, expression):
                continue
            if column.is_star:
                self._add_star_dependencies(node, scope, column)
                continue
            target, uncertain = self._resolve_column(column, scope)
            self._connect(node, target, {ImpactType.VALUE}, uncertain)

        # FILTER (WHERE ...) clauses inside aggregates act on the row set.
        for filter_node in find_all_in_scope(expression, exp.Filter):
            condition = filter_node.expression
            for column in find_all_in_scope(condition, exp.Column):
                target, uncertain = self._resolve_column(column, scope)
                self._connect(node, target, {ImpactType.ROW_SET}, uncertain)

    @staticmethod
    def _in_aggregate_filter(column: exp.Column, expression: exp.Expr) -> bool:
        for filter_node in find_all_in_scope(expression, exp.Filter):
            if any(column is node for node in filter_node.expression.walk()):
                return True
        return False

    def _build_subquery_value(self, scope: Scope) -> ImpactNode:
        if isinstance(scope.expression, exp.SetOperation):
            return self._build_set_operation(0, scope)
        return self._build_column(0, scope)

    # ------------------------------------------------------------ row-set edges
    def _add_filter_dependencies(self, node: ImpactNode, scope: Scope) -> None:
        for target, roles, uncertain in self._collect_filter_edges(scope):
            self._connect(node, target, roles, uncertain)

    def _collect_filter_edges(
        self, scope: Scope
    ) -> list[tuple[ImpactNode, set[ImpactType], bool]]:
        cached = self._filter_cache.get(id(scope))
        if cached is not None:
            return cached

        edges: list[tuple[ImpactNode, set[ImpactType], bool]] = []
        if not isinstance(scope.expression, exp.Select):
            self._filter_cache[id(scope)] = edges
            return edges

        selectable = scope.expression
        subquery_scopes = {id(subscope.expression): subscope for subscope in scope.subquery_scopes}

        filter_expressions: list[exp.Expr] = []
        where = selectable.args.get("where")
        if where:
            filter_expressions.append(where)
        having = selectable.args.get("having")
        if having:
            filter_expressions.append(having)
        qualify_node = selectable.args.get("qualify")
        if qualify_node:
            filter_expressions.append(qualify_node)
        group = selectable.args.get("group")
        if group:
            filter_expressions.append(group)
        for join in selectable.args.get("joins") or []:
            on = join.args.get("on")
            if on:
                filter_expressions.append(on)

        for filter_expression in filter_expressions:
            # Subqueries inside a filter (EXISTS / IN / scalar) decide row survival.
            for subquery in find_all_in_scope(filter_expression, *exp.UNWRAPPED_QUERIES):
                subscope = subquery_scopes.get(id(subquery))
                if subscope is None:
                    continue
                is_exists = isinstance(subquery.parent, exp.Exists)
                child = self._build_filter_subquery(subscope, exists=is_exists)
                edges.append((child, {ImpactType.ROW_SET}, False))

            for column in find_all_in_scope(filter_expression, exp.Column):
                if column.is_star:
                    continue
                target, uncertain = self._resolve_column(column, scope)
                edges.append((target, {ImpactType.ROW_SET}, uncertain))

        # DISTINCT makes every projected value part of the row-set definition.
        if selectable.args.get("distinct") and not selectable.is_star:
            for select in selectable.selects:
                for column in find_all_in_scope(select, exp.Column):
                    if column.is_star:
                        continue
                    target, uncertain = self._resolve_column(column, scope)
                    edges.append(
                        (target, {ImpactType.VALUE, ImpactType.ROW_SET}, uncertain)
                    )

        self._filter_cache[id(scope)] = edges
        return edges

    def _build_filter_subquery(self, scope: Scope, exists: bool) -> ImpactNode:
        if isinstance(scope.expression, exp.SetOperation):
            child = self._build_set_operation(0, scope)
        else:
            child = self._build_column(0, scope)

        if exists:
            # An EXISTS predicate has no value output: its projections only feed the
            # row-set decision, so demote any VALUE role collected inside the subquery.
            for subnode in child.walk():
                for edge in subnode.downstream:
                    edge.roles.discard(ImpactType.VALUE)
                    edge.roles.add(ImpactType.ROW_SET)
        return child

    # ------------------------------------------------------------- row / stars
    @staticmethod
    def _is_row_level(expression: exp.Expr) -> bool:
        # COUNT(*) / COUNT(1) and similar value-less aggregates quantify input rows,
        # so they keep a dependency on the input row set even without a value column.
        return bool(
            list(expression.find_all(exp.Star))
            or (
                list(expression.find_all(exp.AggFunc))
                and not list(expression.find_all(exp.Column))
            )
        )

    def _add_row_dependency(self, node: ImpactNode, scope: Scope) -> None:
        for name, source in scope.sources.items():
            rows_node = self._rows_node(name, source)
            self._connect(node, rows_node, {ImpactType.ROW_SET})

    def _add_star_dependencies(
        self, node: ImpactNode, scope: Scope, star: exp.Column | exp.Star
    ) -> None:
        table = star.table if isinstance(star, exp.Column) else ""
        targets = self._star_targets(table, scope)
        if not targets:
            unknown = self._leaf(star.sql(comments=False), exp.Placeholder(), NodeKind.UNKNOWN)
            self._connect(node, unknown, {ImpactType.VALUE}, uncertain=True)
            return

        for target, uncertain in targets:
            self._connect(node, target, {ImpactType.VALUE}, uncertain)

    def _star_targets(
        self, table: str, scope: Scope
    ) -> list[tuple[ImpactNode, bool]]:
        targets: list[tuple[ImpactNode, bool]] = []
        if table in scope.sources:
            source_items = [(table, scope.sources[table])]
        else:
            source_items = list(scope.sources.items())

        for source_name, source in source_items:
            if isinstance(source, Scope):
                for name in source.expression.named_selects:
                    if name == "*":
                        continue
                    targets.append((self._build_column(name, source), False))
            elif isinstance(source, exp.Table):
                columns = list(self.schema.column_names(source, dialect=self.dialect))
                if columns:
                    for column_name in columns:
                        leaf = self._leaf(
                            f"{source_name}.{column_name}",
                            source,
                            NodeKind.COLUMN,
                        )
                        targets.append((leaf, False))
                else:
                    unknown = self._leaf(
                        f"{source_name}.*", exp.Placeholder(), NodeKind.UNKNOWN
                    )
                    targets.append((unknown, True))
        return targets

    def _rows_node(self, source_name: str, source: exp.Table | Scope) -> ImpactNode:
        expression = source.expression if isinstance(source, Scope) else source
        return self._leaf(f"{source_name}.*", expression, NodeKind.ROWS)

    # --------------------------------------------------------- set operations
    def _build_set_operation(self, column: str | int, scope: Scope) -> ImpactNode:
        index = column if isinstance(column, int) else self._set_operation_index(scope, column)
        name = scope.expression.selects[index].alias_or_name
        node = self._node_for(scope, name, scope.expression.selects[index], scope.expression)

        for branch in scope.set_operation_scopes:
            self._connect(node, self._build_column(index, branch), {ImpactType.VALUE})

        # Filters of each branch still decide which rows feed the set operation.
        for branch in scope.set_operation_scopes:
            self._add_filter_dependencies(node, branch)
        return node

    @staticmethod
    def _set_operation_index(scope: Scope, column: str) -> int:
        for index, select in enumerate(scope.expression.selects):
            if select.alias_or_name == column or select.is_star:
                return index
        raise SqlglotError(f"Could not find {column} in {scope.expression}")

    # ----------------------------------------------------------- column resolve
    def _resolve_column(
        self, column: exp.Column, scope: Scope
    ) -> tuple[ImpactNode, bool]:
        table = column.table
        source = scope.sources.get(table)

        if source is None and not table:
            # Mirror lineage's single-source inference: an unqualified column is only
            # attributed to the sole source deterministically; otherwise it is unknown.
            named_sources = {name: src for name, src in scope.sources.items() if name}
            if len(named_sources) == 1:
                table, source = next(iter(named_sources.items()))
            elif len(scope.sources) == 1:
                table, source = next(iter(scope.sources.items()))

        if source is None:
            outer = self._resolve_outer(column, scope)
            if outer is not None:
                return outer
            unknown = self._leaf(column.sql(comments=False), exp.Placeholder(), NodeKind.UNKNOWN)
            return unknown, True

        if isinstance(source, Scope):
            return self._build_column(column.name, source), False

        leaf = self._leaf(
            f"{table}.{column.name}", t.cast(exp.Table, source), NodeKind.COLUMN
        )
        return leaf, False

    def _resolve_outer(
        self, column: exp.Column, scope: Scope
    ) -> tuple[ImpactNode, bool] | None:
        # Correlated reference: walk the AST ancestor chain (SetOperation branches do
        # not populate Scope.parent) and resolve against the first enclosing scope
        # that owns the referenced table.
        node = scope.expression.parent
        while node is not None:
            outer = self._scopes_by_expr.get(id(node))
            if outer is not None and column.table in outer.sources:
                return self._resolve_column(column, outer)
            node = node.parent
        return None



