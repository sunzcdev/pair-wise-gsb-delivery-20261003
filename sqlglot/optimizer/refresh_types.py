from __future__ import annotations

import typing as t
from collections import deque

from sqlglot import exp
from sqlglot.errors import OptimizeError
from sqlglot.optimizer.annotate_types import TypeAnnotator
from sqlglot.optimizer.scope import Scope, traverse_scope
from sqlglot.schema import Schema, ensure_schema

if t.TYPE_CHECKING:
    from sqlglot._typing import E
    from sqlglot.typing import ExprMetadataType

    from sqlglot.dialects.dialect import DialectType

# Marks a node whose previously inferred type was invalidated and has not been
# recomputed yet. Survives until the next (full or incremental) annotation pass.
STALE_META_KEY = "type_stale"


def is_stale(expression: exp.Expr) -> bool:
    """Whether `expression`'s type annotation was invalidated and is pending re-inference."""
    return expression.meta_get(STALE_META_KEY) is True


class TypeRefreshResult:
    """Summary of an incremental type refresh.

    Attributes:
        expression: The refreshed root expression (the same object passed in).
        invalidated: Nodes whose previous type annotation was dropped, including the
            submitted nodes and every node reached through dependency propagation.
        scopes: Scopes whose expressions had to be re-annotated.
        skipped: Submitted nodes that could not be refreshed because they are detached
            from the root (only populated when ``strict=False``).
        unknown: Re-annotated nodes whose inferred type is ``UNKNOWN``.
    """

    __slots__ = ("expression", "invalidated", "scopes", "skipped", "unknown")

    def __init__(
        self,
        expression: exp.Expr,
        invalidated: list[exp.Expr],
        scopes: list[Scope],
        skipped: list[exp.Expr],
        unknown: list[exp.Expr],
    ) -> None:
        self.expression = expression
        self.invalidated = invalidated
        self.scopes = scopes
        self.skipped = skipped
        self.unknown = unknown


def refresh_types(
    expression: E,
    changed: exp.Expr | t.Sequence[exp.Expr],
    schema: dict[str, object] | Schema | None = None,
    expression_metadata: ExprMetadataType | None = None,
    coerces_to: dict[exp.DType, set[exp.DType]] | None = None,
    dialect: DialectType = None,
    *,
    strict: bool = True,
) -> TypeRefreshResult:
    """Incrementally re-annotate the types affected by `changed` nodes.

    Only the dependency reach of the submitted nodes is recomputed; every other branch
    keeps its existing annotation. The resulting types are identical to running
    :func:`~sqlglot.optimizer.annotate_types` on the whole tree again.

    Dependencies are tracked through scopes, not just parent links: alias references,
    CTE outputs and correlated subqueries all propagate changes to their consumers.

    Args:
        expression: The previously annotated root expression.
        changed: One or more nodes that were replaced, inserted or mutated.
        schema: Database schema used to resolve column types.
        expression_metadata: Maps expression type to corresponding annotation function.
        coerces_to: Maps expression type to set of types it can be coerced into.
        dialect: The dialect to consult when constructing a Schema object, if needed.
        strict: When True (default), raise ``OptimizeError`` for a node that is not
            attached to `expression`. When False, collect such nodes in the result's
            ``skipped`` list instead. Repeated submissions are always de-duplicated.

    Returns:
        A :class:`TypeRefreshResult` describing what was invalidated and recomputed.
    """
    schema = ensure_schema(schema, dialect=dialect)
    nodes = _dedupe_changed(changed)

    scopes = traverse_scope(expression)
    propagator = _TaintPropagator(expression, scopes)
    if not scopes:
        # Non-traversable expressions (e.g. a bare `1 + a`) have no scope structure, so
        # every node lives in one implicit unit: taint the attached submissions directly.
        skipped = propagator.seed_unscoped(nodes, strict=strict)
    else:
        skipped = propagator.seed(nodes, strict=strict)
    propagator.propagate()

    affected = propagator.affected_scopes()

    # A fresh annotator reuses every still-valid annotation (overwrite_types=False) and
    # only re-infers the invalidated nodes, whose types were reset to None on purpose.
    annotator = TypeAnnotator(
        schema=schema,
        expression_metadata=expression_metadata,
        coerces_to=coerces_to,
        overwrite_types=False,
    )

    for scope in scopes:
        if id(scope) in affected:
            scope.expression.meta.pop("query_type", None)
            annotator.annotate_scope(scope)

    annotator.annotate_expression(expression)
    annotator._apply_default_null_types()

    invalidated = propagator.invalidated
    unknown = [node for node in invalidated if node.type is None or node.is_type(exp.DType.UNKNOWN)]
    for node in invalidated:
        node.meta.pop(STALE_META_KEY, None)

    return TypeRefreshResult(
        expression=expression,
        invalidated=invalidated,
        scopes=[scope for scope in scopes if id(scope) in affected],
        skipped=skipped,
        unknown=unknown,
    )


def _dedupe_changed(changed: exp.Expr | t.Sequence[exp.Expr]) -> list[exp.Expr]:
    if isinstance(changed, exp.Expr):
        return [changed]

    unique: dict[int, exp.Expr] = {}
    for node in changed:
        unique.setdefault(id(node), node)
    return list(unique.values())


class _TaintPropagator:
    """Computes the set of nodes whose annotation depends on a changed node.

    Taint starts at each submitted node and spreads two ways:

    * structurally, up the parent chain of the scope that owns the node;
    * semantically, across scope boundaries whenever a column reads a tainted output
      name (CTE / derived table outputs, alias references and correlated columns).
    """

    def __init__(self, root: exp.Expr, scopes: list[Scope]) -> None:
        self._root = root
        self._scopes = scopes

        # Innermost scope that walks each node (a node belongs to exactly one scope).
        self._owner: dict[int, Scope] = {}
        # Output names tainted within each scope.
        self._names: dict[int, set[str]] = {}
        # Scopes whose every expression is tainted.
        self._fully_tainted: set[int] = set()
        # Nodes explicitly marked stale, in discovery order.
        self.invalidated: list[exp.Expr] = []
        self._invalidated_ids: set[int] = set()

        for scope in scopes:
            for node in scope.walk():
                self._owner[id(node)] = scope

    def seed(self, nodes: list[exp.Expr], strict: bool) -> list[exp.Expr]:
        skipped: list[exp.Expr] = []

        for node in nodes:
            if not self._check_attached(node, strict, skipped):
                continue

            owner = self._owner.get(id(node))
            if owner is None:
                # Defensive fallback: annotate everything rather than miss a dependency.
                self._taint_all_scopes()
                continue

            if isinstance(owner.expression, exp.SetOperation):
                self._taint_scope_fully(owner)
            else:
                self._taint_within_scope(owner, node)

        return skipped

    def seed_unscoped(self, nodes: list[exp.Expr], strict: bool) -> list[exp.Expr]:
        skipped: list[exp.Expr] = []
        for node in nodes:
            if not self._check_attached(node, strict, skipped):
                continue
            current: exp.Expr | None = node
            while current is not None:
                self._mark_stale(current)
                current = current.parent
        return skipped

    def _check_attached(self, node: exp.Expr, strict: bool, skipped: list[exp.Expr]) -> bool:
        if self._is_attached(node):
            return True
        if strict:
            raise OptimizeError(
                f"Cannot refresh {node.sql()!r}: it is not attached to the root expression"
            )
        skipped.append(node)
        return False

    def propagate(self) -> None:
        if self._fully_tainted == {id(scope) for scope in self._scopes}:
            return

        queue: deque[Scope] = deque(
            scope
            for scope in self._scopes
            if id(scope) in self._fully_tainted or self._names.get(id(scope))
        )
        queued: set[int] = {id(scope) for scope in queue}

        while queue:
            scope = queue.popleft()
            scope_id = id(scope)
            names = self._names.get(scope_id, set())

            if scope_id in self._fully_tainted:
                names = set(self._scope_output_names(scope))

            self._propagate_within_scope(scope)
            newly_enqueued = self._propagate_external(scope, names)
            for consumer in newly_enqueued:
                if id(consumer) not in queued:
                    queued.add(id(consumer))
                    queue.append(consumer)

    def affected_scopes(self) -> set[int]:
        affected = set(self._fully_tainted)
        for scope_id, names in self._names.items():
            if names:
                affected.add(scope_id)
        return affected

    def _propagate_external(
        self,
        scope: Scope,
        names: set[str],
    ) -> list[Scope]:
        newly_enqueued: list[Scope] = []
        if not names:
            return newly_enqueued

        # Boundary node of a subquery scope is the subquery expression living in its
        # parent scope; tainting it propagates the change up the enclosing expression.
        boundary = self._boundary_node(scope)
        if boundary is not None:
            parent_scope = self._owner.get(id(boundary))
            if parent_scope is not None and id(parent_scope) not in self._fully_tainted:
                self._taint_within_scope(parent_scope, boundary)
                newly_enqueued.append(parent_scope)

        for consumer in self._scopes:
            if id(consumer) in self._fully_tainted:
                continue

            consumes = False
            for column in consumer.columns:
                source = consumer.sources.get(column.table)
                if (
                    isinstance(source, Scope)
                    and source is scope
                    and not column.is_star
                    and column.name in names
                ):
                    consumes = True
                    break

            # Correlated columns reference ancestor scopes directly via external columns.
            if not consumes and consumer.parent is not None and scope is not consumer:
                for column in consumer.external_columns:
                    if (
                        not column.is_star
                        and column.name in names
                        and self._resolves_to(column, scope)
                    ):
                        consumes = True
                        break

            if consumes:
                self._taint_consuming_columns(consumer, names)
                newly_enqueued.append(consumer)

        return newly_enqueued

    def _taint_consuming_columns(self, scope: Scope, names: set[str]) -> None:
        tainted_any = False
        # Walk the scope rather than `scope.columns`, which also reaches unqualified alias
        # references living in ORDER/GROUP BY clauses (filtered out of `scope.columns`).
        for node in scope.walk():
            if (
                isinstance(node, exp.Column)
                and not node.is_star
                and node.name in names
            ):
                tainted_any = self._taint_within_scope(scope, node) or tainted_any

        if not tainted_any:
            # The dependency is consumed without an explicit column (e.g. a star or a
            # struct read): taint every projection so no dependent output is missed.
            for select in self._select_expressions(scope):
                self._taint_within_scope(scope, select)

    def _select_expressions(self, scope: Scope) -> list[exp.Expr]:
        expression = scope.expression
        if isinstance(expression, (exp.Selectable, exp.SetOperation)):
            return list(expression.selects)
        return []

    def _propagate_within_scope(self, scope: Scope) -> None:
        """Taint unqualified reads of tainted output names in the same scope.

        Covers alias references inside ORDER/GROUP BY-style clauses, which are not part of
        ``scope.columns``. Runs to a fixed point so a newly tainted projection can expose
        its output name to further references within the scope.
        """
        if id(scope) in self._fully_tainted:
            return

        changed = True
        while changed:
            changed = False
            names = self._names.get(id(scope), set())
            if not names:
                return

            for node in scope.walk():
                if (
                    isinstance(node, exp.Column)
                    and not node.is_star
                    and not node.table
                    and node.name in names
                    and id(node) not in self._invalidated_ids
                ):
                    self._taint_within_scope(scope, node)
                    changed = True

    def _taint_within_scope(self, scope: Scope, node: exp.Expr) -> bool:
        """Mark `node` and its ancestors inside `scope` stale; taint output names."""
        scope_root = scope.expression
        current: exp.Expr | None = node
        tainted = False

        while current is not None:
            tainted = self._mark_stale(current) or tainted
            if current is scope_root:
                break
            parent = current.parent
            if parent is None or self._owner.get(id(parent)) is not scope:
                break
            if self._is_scope_boundary(parent):
                # Stop at a query/clause container. Full annotation never assigns a type
                # to those structural nodes, and siblings live in independent clauses or
                # projections, so they must not be invalidated here.
                break
            current = parent

        scope_names = self._names.setdefault(id(scope), set())
        for name in self._output_names_for(scope, node):
            scope_names.add(name)

        return tainted

    def _is_scope_boundary(self, parent: exp.Expr) -> bool:
        if isinstance(parent, exp.Query):
            return True
        return isinstance(
            parent, (exp.Where, exp.Having, exp.Order, exp.Group, exp.Qualify, exp.Join)
        )

    def _taint_scope_fully(self, scope: Scope) -> None:
        scope_id = id(scope)
        if scope_id in self._fully_tainted:
            return

        self._fully_tainted.add(scope_id)
        for node in scope.walk():
            self._mark_stale(node)
        self._names[scope_id] = set(self._scope_output_names(scope))

    def _taint_all_scopes(self) -> None:
        for scope in self._scopes:
            self._taint_scope_fully(scope)

    def _mark_stale(self, node: exp.Expr) -> bool:
        node_id = id(node)
        if node_id in self._invalidated_ids:
            return False

        self._invalidated_ids.add(node_id)
        self.invalidated.append(node)
        node._type = None
        node.meta[STALE_META_KEY] = True
        node.meta.pop("nonnull", None)
        if isinstance(node, (exp.Query, exp.Subquery)):
            node.meta.pop("query_type", None)
        return True

    def _output_names_for(self, scope: Scope, node: exp.Expr) -> set[str]:
        expression = scope.expression
        if isinstance(expression, exp.SetOperation):
            return set(expression.named_selects)
        if not isinstance(expression, exp.Selectable):
            return set()

        names: set[str] = set()
        for select in expression.selects:
            if node is select or any(candidate is node for candidate in select.walk()):
                names.add(select.output_name)
        return names

    def _scope_output_names(self, scope: Scope) -> list[str]:
        expression = scope.expression
        if isinstance(expression, (exp.Selectable, exp.SetOperation)):
            return list(expression.named_selects)
        return []

    def _boundary_node(self, scope: Scope) -> exp.Expr | None:
        if scope.parent is None or scope.expression is self._root or scope.is_cte:
            return None

        # Find the outermost node still owned by this scope, then return the first
        # ancestor owned by the parent scope: the Subquery wrapper (derived table) or
        # the scalar subquery expression that encloses this scope.
        outermost: exp.Expr = scope.expression
        current: exp.Expr | None = scope.expression
        while current is not None and self._owner.get(id(current)) is scope:
            outermost = current
            current = current.parent

        wrapper = outermost.parent
        while wrapper is not None and self._owner.get(id(wrapper)) is not scope.parent:
            wrapper = wrapper.parent
        return wrapper

    def _resolves_to(self, column: exp.Column, scope: Scope) -> bool:
        # An external column resolves against an ancestor scope's sources or its own
        # projections; walk the ancestor chain the same way the annotator does.
        candidate: Scope | None = scope
        while candidate is not None:
            source = candidate.sources.get(column.table)
            if source is scope:
                return True
            candidate = candidate.parent
        return False

    def _is_attached(self, node: exp.Expr) -> bool:
        current: exp.Expr | None = node
        while current is not None:
            if current is self._root:
                return True
            current = current.parent
        return False
