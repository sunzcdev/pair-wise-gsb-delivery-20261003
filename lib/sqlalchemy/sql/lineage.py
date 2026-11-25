# sql/lineage.py
# Copyright (C) 2005-2026 the SQLAlchemy authors and contributors
# <see AUTHORS file>
#
# This module is part of SQLAlchemy and is released under
# the MIT License: https://www.opensource.org/licenses/mit-license.php
# mypy: allow-untyped-defs, allow-untyped-calls

"""Introspection of the *column source* (lineage) of a Core statement.

This module provides a read-only view describing, for every output position of
a :class:`_expression.Select` (or other :class:`_expression.ReturnsRows`
construct such as a :class:`_expression.CTE`, :class:`_expression.Subquery` or
compound select), which underlying expressions and physical table columns
ultimately feed that position.

The view is purely introspective: it walks the existing clause structure and
never rewrites, compiles or executes the statement.  Building the report and
running the statement are therefore completely independent activities.

Features:

* A :class:`_expression.CompoundSelect` (``UNION`` / ``UNION ALL`` /
  ``INTERSECT`` / ``EXCEPT``) reports one :class:`BranchOrigin` per branch for
  every output position, so the alternative sources on each side stay
  distinguishable even when they share a name.
* A :class:`_expression.CTE` or :class:`_expression.Subquery` keeps the
  reference to its wrapped statement; resolution continues through it by
  *position*, which keeps distinct identities apart when two columns share the
  same name (or when the projection is reordered).
* Columns participating in an expression are reported individually in
  addition to the expression itself.
* Anything that cannot be traced to a physical column is still reported with
  an explicit :attr:`Origin.kind` (``"expression"``, ``"literal"``,
  ``"parameter"``, ``"text"`` or ``"unknown"``) instead of being silently
  dropped.

"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any
from typing import List
from typing import Optional
from typing import TYPE_CHECKING

from .elements import AbstractTextClause
from .elements import BindParameter
from .elements import KeyedColumnElement
from .elements import Label
from .selectable import Alias
from .selectable import CompoundSelect
from .selectable import CTE
from .selectable import Select
from .selectable import SelectStatementGrouping
from .selectable import Subquery
from .selectable import TextualSelect
from . import visitors

if TYPE_CHECKING:
    from .selectable import ReturnsRows
    from .type_api import TypeEngine

__all__ = [
    "Origin",
    "BranchOrigin",
    "ColumnLineage",
    "ColumnLineageReport",
    "column_lineage",
    "lineage_for_column",
]


# ----------------------------------------------------------------------------
# Data model
# ----------------------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class Origin:
    """A single terminal source of a value.

    An :class:`Origin` is a leaf in the lineage graph: either a physical
    table column or a value that has no physical column behind it (a literal,
    a bound parameter, a raw SQL text fragment or an opaque expression).

    """

    kind: str
    """What kind of source this is.

    One of:

    * ``"column"`` - a physical (or table-valued) column
    * ``"expression"`` - an expression whose leaves are reported separately
    * ``"literal"`` - an inline literal value
    * ``"parameter"`` - a bound parameter
    * ``"text"`` - a raw :class:`_expression.TextClause` fragment
    * ``"unknown"`` - a source that could not be resolved

    """

    table: Optional[str]
    """The name of the table (or table-valued construct) the column belongs
    to, if any."""

    name: Optional[str]
    """The column name, if this is a ``"column"`` origin."""

    expression: Optional[str]
    """A SQL rendering of the originating expression, when available."""

    value: Any = None
    """The Python value of a literal/parameter, when available."""

    def describe(self) -> str:
        """Return a short human readable description of this origin."""
        if self.kind == "column":
            if self.table:
                return f"{self.table}.{self.name}"
            return self.name or "<column>"
        elif self.kind == "literal":
            return f"literal({self.value!r})"
        elif self.kind == "parameter":
            if self.value is not None:
                return f"param({self.value!r})"
            return "param(?)"
        elif self.kind == "text":
            return f"text({self.expression})"
        elif self.kind == "expression":
            return f"expression({self.expression})"
        return f"unknown({self.expression})"


@dataclass(frozen=True, slots=True)
class BranchOrigin:
    """The source of one output position within one branch of a query.

    For a plain (non-compound) statement there is a single implicit branch
    (``branch`` is ``0``).  For a compound select each branch contributes one
    :class:`BranchOrigin`.  When a branch reaches through a nested compound
    CTE/subquery, :attr:`branch_chain` records the branch index chosen at every
    level of nesting.
    """

    branch: int
    """Zero based index of the branch within the outermost compound select,
    or ``0`` for a non-compound statement."""

    label: Optional[str]
    """The label/name of the expression as selected within this branch."""

    expression: str
    """A SQL rendering of the expression selected in this branch."""

    origins: tuple[Origin, ...]
    """The terminal origins feeding this branch expression."""

    branch_chain: tuple[int, ...] = ()
    """Branch index chosen at each level of nested compound resolution.

    The first element is the branch of the outermost compound; later elements
    are the branches chosen inside nested compound CTEs/subqueries.
    """

    path: tuple[str, ...] = ()
    """Names of the nested CTE/subquery constructs traversed from the
    outermost statement down to this branch."""

    def describe(self) -> str:
        body = ", ".join(o.describe() for o in self.origins) or "unknown"
        chain = " -> ".join(str(i) for i in self.branch_chain) or "0"
        return f"branch {chain}: {self.expression} <- {body}"


@dataclass(frozen=True, slots=True)
class ColumnLineage:
    """The full lineage of one output position of a statement."""

    index: int
    """The zero based output position."""

    name: Optional[str]
    """The result-set column name at this position."""

    type: Optional[TypeEngine[Any]]
    """The SQL type associated with the output column."""

    sources: tuple[BranchOrigin, ...]
    """One :class:`BranchOrigin` per branch that can produce this position."""

    path: tuple[str, ...]
    """Names of the nested constructs traversed from the outer statement to
    the terminal sources (e.g. the CTE / subquery / union names)."""

    @property
    def is_ambiguous(self) -> bool:
        """True when more than one distinct branch can supply the value.

        This is always the case for the branches of a compound select unless
        all branches resolve to an identical single origin.
        """
        unique = {tuple(sorted(o.describe() for o in b.origins)) for b in self.sources}
        return len(unique) > 1

    @property
    def has_unknown(self) -> bool:
        """True when any source could not be traced to a concrete origin."""
        return any(o.kind == "unknown" for b in self.sources for o in b.origins)

    def describe(self) -> str:
        header = f"[{self.index}] {self.name or '?'}"
        lines = [header]
        prefix = "  "
        for name in self.path:
            lines.append(f"{prefix}via {name}")
        for branch in self.sources:
            lines.append(f"{prefix}{branch.describe()}")
        return "\n".join(lines)


@dataclass(frozen=True, slots=True)
class ColumnLineageReport:
    """Result of :func:`column_lineage`: the lineage of every output
    position of a statement in projection order."""

    statement: Any
    columns: tuple[ColumnLineage, ...]

    def __iter__(self):
        return iter(self.columns)

    def __len__(self):
        return len(self.columns)

    def __getitem__(self, key):
        if isinstance(key, str):
            for col in self.columns:
                if col.name == key:
                    return col
            raise KeyError(key)
        return self.columns[key]

    def describe(self) -> str:
        return "\n".join(c.describe() for c in self.columns)


# ----------------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------------


def _safe_str(element: Any) -> Optional[str]:
    if element is None:
        return None
    try:
        return str(element)
    except Exception:
        try:
            return element.compile(compile_kwargs={"literal_binds": True})
        except Exception:
            return repr(element)


def _unwrap_grouping(obj):
    """Unwrap :class:`SelectStatementGrouping` wrappers around nested
    compound selects."""
    while isinstance(obj, SelectStatementGrouping):
        obj = obj.element
    return obj


def _column_identity_index(container) -> dict:
    """Map every exported column of ``container`` (by identity) to its
    position."""
    return {id(col): idx for idx, col in enumerate(container.exported_columns)}


def _physical_origins(column: KeyedColumnElement) -> List[Origin]:
    """Resolve a FROM-clause column down to its physical table column(s).

    Alias/subquery columns keep the columns they proxy in ``_proxies``; walk
    that chain.  A lightweight :func:`_expression.table` column has no proxy
    and is itself treated as the terminal column.
    """
    proxies = list(getattr(column, "_proxies", ()) or ())
    if not proxies:
        if isinstance(column, BindParameter):
            return [
                Origin(
                    kind="parameter" if column.value is None else "literal",
                    table=None,
                    name=None,
                    expression=_safe_str(column),
                    value=column.value,
                )
            ]
        table = getattr(column, "table", None)
        if table is None:
            # a tableless ColumnClause, e.g. literal_column("'x'"): raw SQL
            # with no physical table behind it
            return [
                Origin(
                    kind="expression",
                    table=None,
                    name=getattr(column, "name", None),
                    expression=_safe_str(column),
                )
            ]
        return [
            Origin(
                kind="column",
                table=getattr(table, "name", None),
                name=getattr(column, "name", None),
                expression=_safe_str(column),
            )
        ]

    origins: List[Origin] = []
    for proxy in proxies:
        if isinstance(proxy, KeyedColumnElement):
            origins.extend(_physical_origins(proxy))
        else:
            origins.append(
                Origin(
                    kind="expression",
                    table=None,
                    name=None,
                    expression=_safe_str(proxy),
                )
            )
    # de-duplicate while preserving order (same physical column may appear via
    # several proxy layers)
    seen = set()
    unique: List[Origin] = []
    for origin in origins:
        key = (origin.kind, origin.table, origin.name, origin.expression)
        if key not in seen:
            seen.add(key)
            unique.append(origin)
    return unique


def _position_in(container, column: KeyedColumnElement) -> Optional[int]:
    """Return the exported-column position of ``column`` in ``container``.

    Identity is used first (the normal case for a column taken directly from
    ``container.c``); proxy identity is used as a fallback for columns that
    were copied/annotated.
    """
    columns = list(container.exported_columns)
    for idx, candidate in enumerate(columns):
        if candidate is column:
            return idx
    proxied = {id(p) for p in getattr(column, "_proxies", ()) or ()}
    for idx, candidate in enumerate(columns):
        if id(candidate) in proxied:
            return idx
        candidate_proxies = getattr(candidate, "_proxies", ()) or ()
        if any(id(p) in proxied or p is column for p in candidate_proxies):
            return idx
    return None


# ----------------------------------------------------------------------------
# Core resolution
#
# Resolution always works positionally and returns one BranchOrigin per
# branch of the (possibly nested) compound structure at that position.  A
# plain select is treated as a compound with a single branch.
# ----------------------------------------------------------------------------


def _unknown_origin(expression) -> Origin:
    return Origin(
        kind="unknown",
        table=None,
        name=None,
        expression=_safe_str(expression),
    )


def _resolve_container_position(
    container, position: int, path: tuple[str, ...]
) -> tuple[BranchOrigin, ...]:
    """Resolve output ``position`` of a CTE/Subquery/CompoundSelect/Select
    into per-branch :class:`BranchOrigin` objects."""

    element = getattr(container, "element", container)
    element = _unwrap_grouping(element)

    if isinstance(element, CompoundSelect):
        return _resolve_compound(element, position, path)

    if isinstance(element, Select):
        return _resolve_select(element, position, path)

    if isinstance(element, TextualSelect):
        # a select() built from text(): every output column is an opaque
        # fragment of the raw SQL text
        exported = list(container.exported_columns)
        col = exported[position] if position < len(exported) else None
        return (
            BranchOrigin(
                branch=0,
                label=getattr(col, "name", None),
                expression=_safe_str(col) or "?",
                origins=(
                    Origin(
                        kind="text",
                        table=None,
                        name=getattr(col, "name", None),
                        expression=_safe_str(element.element),
                    ),
                ),
                path=path,
            ),
        )

    # unknown wrapper - state explicitly rather than guessing
    exported = list(container.exported_columns)
    col = exported[position] if position < len(exported) else None
    return (
        BranchOrigin(
            branch=0,
            label=getattr(col, "name", None),
            expression=_safe_str(col) or "?",
            origins=(_unknown_origin(element),),
            path=path,
        ),
    )


def _resolve_compound(
    compound: CompoundSelect, position: int, path: tuple[str, ...]
) -> tuple[BranchOrigin, ...]:
    branches: List[BranchOrigin] = []
    for branch_idx, raw_branch in enumerate(compound.selects):
        branch = _unwrap_grouping(raw_branch)
        if isinstance(branch, CompoundSelect):
            # nested compound: flatten its branches at this same position,
            # prefixing every branch chain with this level's branch index
            for sub in _resolve_compound(branch, position, path):
                branches.append(
                    BranchOrigin(
                        branch=branch_idx,
                        label=sub.label,
                        expression=sub.expression,
                        origins=sub.origins,
                        branch_chain=(branch_idx,) + sub.branch_chain,
                        path=sub.path,
                    )
                )
            continue

        if not isinstance(branch, Select):
            branches.append(
                BranchOrigin(
                    branch=branch_idx,
                    label=None,
                    expression=_safe_str(branch) or "?",
                    origins=(_unknown_origin(branch),),
                    branch_chain=(branch_idx,),
                    path=path,
                )
            )
            continue

        all_cols = list(branch._all_selected_columns)
        if position >= len(all_cols):
            branches.append(
                BranchOrigin(
                    branch=branch_idx,
                    label=None,
                    expression="<missing position>",
                    origins=(
                        Origin(
                            kind="unknown",
                            table=None,
                            name=None,
                            expression=(
                                f"branch {branch_idx} has no column at "
                                f"position {position}"
                            ),
                        ),
                    ),
                    branch_chain=(branch_idx,),
                    path=path,
                )
            )
            continue
        branches.extend(
            _branch_origin_for_expression(
                branch_idx, all_cols[position], path
            )
        )
    return tuple(branches)


def _resolve_select(
    select: Select, position: int, path: tuple[str, ...]
) -> tuple[BranchOrigin, ...]:
    all_cols = list(select._all_selected_columns)
    if position >= len(all_cols):
        return (
            BranchOrigin(
                branch=0,
                label=None,
                expression="<missing position>",
                origins=(
                    Origin(
                        kind="unknown",
                        table=None,
                        name=None,
                        expression=f"no column at position {position}",
                    ),
                ),
            ),
        )
    return _branch_origin_for_expression(0, all_cols[position], path)


def _is_nested_selectable(table) -> bool:
    """True for CTEs / subqueries (which wrap a selectable and must be
    resolved positionally), False for plain table aliases (whose columns
    resolve directly to physical table columns)."""
    return isinstance(table, (CTE, Subquery)) and hasattr(table, "element")


def _terminal_branches_for_expression(
    expression, label, path: tuple[str, ...]
) -> tuple[BranchOrigin, ...]:
    """Resolve one selected expression into one or more :class:`BranchOrigin`.

    A reference into a nested CTE/subquery may itself expand into several
    branches (when that construct is compound); branch identity is preserved
    so the two sides of a UNION stay distinguishable at every nesting level.
    """
    if isinstance(expression, AbstractTextClause):
        return (
            BranchOrigin(
                branch=0,
                label=label,
                expression=_safe_str(expression),
                origins=(
                    Origin(
                        kind="text",
                        table=None,
                        name=None,
                        expression=_safe_str(expression),
                    ),
                ),
                path=path,
            ),
        )

    if isinstance(expression, BindParameter):
        return (
            BranchOrigin(
                branch=0,
                label=label,
                expression=_safe_str(expression),
                origins=(
                    Origin(
                        kind="parameter"
                        if expression.value is None
                        else "literal",
                        table=None,
                        name=None,
                        expression=_safe_str(expression),
                        value=expression.value,
                    ),
                ),
                path=path,
            ),
        )

    if isinstance(expression, KeyedColumnElement):
        table = getattr(expression, "table", None)
        if _is_nested_selectable(table):
            nested_position = _position_in(table, expression)
            if nested_position is not None:
                nested = _resolve_container_position(
                    table,
                    nested_position,
                    _extend_path(path, table),
                )
                # preserve branch identity; only re-tag the outer label
                return tuple(
                    BranchOrigin(
                        branch=sub.branch,
                        label=label,
                        expression=sub.expression,
                        origins=sub.origins,
                        branch_chain=sub.branch_chain,
                        path=sub.path,
                    )
                    for sub in nested
                )
        return (
            BranchOrigin(
                branch=0,
                label=label,
                expression=_safe_str(expression),
                origins=tuple(_physical_origins(expression)),
                path=path,
            ),
        )

    # generic expression: the expression itself plus its leaves.  Any leaf
    # that points into a nested compound selectable contributes the origins of
    # each of its branches.
    origins: List[Origin] = [
        Origin(
            kind="expression",
            table=None,
            name=None,
            expression=_safe_str(expression),
        )
    ]
    seen = set()

    def _add(origin: Origin) -> None:
        key = (origin.kind, origin.table, origin.name, origin.expression)
        if key not in seen:
            seen.add(key)
            origins.append(origin)

    nested_branch_origins: List[
        tuple[tuple[int, ...], tuple[Origin, ...], tuple[str, ...]]
    ] = []
    for node in visitors.iterate(expression, {}):
        if isinstance(node, BindParameter):
            _add(
                Origin(
                    kind="parameter" if node.value is None else "literal",
                    table=None,
                    name=None,
                    expression=_safe_str(node),
                    value=node.value,
                )
            )
        elif isinstance(node, AbstractTextClause):
            _add(
                Origin(
                    kind="text",
                    table=None,
                    name=None,
                    expression=_safe_str(node),
                )
            )
        elif isinstance(node, KeyedColumnElement):
            node_table = getattr(node, "table", None)
            if _is_nested_selectable(node_table):
                nested_position = _position_in(node_table, node)
                if nested_position is not None:
                    for sub in _resolve_container_position(
                        node_table,
                        nested_position,
                        _extend_path(path, node_table),
                    ):
                        nested_branch_origins.append(
                            (sub.branch_chain, sub.origins, sub.path)
                        )
                    continue
            for origin in _physical_origins(node):
                _add(origin)

    if nested_branch_origins:
        # one BranchOrigin per distinct (branch chain, path) involved
        result: List[BranchOrigin] = []
        per_chain: dict[
            tuple[tuple[int, ...], tuple[str, ...]], List[Origin]
        ] = {}
        for chain, branch_origins, sub_path in nested_branch_origins:
            key = (chain, sub_path)
            bucket = per_chain.setdefault(key, [])
            for origin in branch_origins:
                okey = (
                    origin.kind,
                    origin.table,
                    origin.name,
                    origin.expression,
                )
                if okey not in seen:
                    seen.add(okey)
                    bucket.append(origin)
        for chain, sub_path in sorted(per_chain):
            result.append(
                BranchOrigin(
                    branch=chain[0] if chain else 0,
                    label=label,
                    expression=_safe_str(expression),
                    origins=tuple(origins + per_chain[(chain, sub_path)]),
                    branch_chain=chain,
                    path=sub_path,
                )
            )
        return tuple(result)

    return (
        BranchOrigin(
            branch=0,
            label=label,
            expression=_safe_str(expression),
            origins=tuple(origins),
            path=path,
        ),
    )


def _branch_origin_for_expression(
    branch_idx: int, branch_expr, path: tuple[str, ...]
) -> tuple[BranchOrigin, ...]:
    label = getattr(branch_expr, "name", None)
    inner = branch_expr.element if isinstance(branch_expr, Label) else branch_expr

    branches = _terminal_branches_for_expression(inner, label, path)
    if not branches:
        branches = (
            BranchOrigin(
                branch=branch_idx,
                label=label,
                expression=_safe_str(branch_expr),
                origins=(_unknown_origin(inner),),
                branch_chain=(branch_idx,),
                path=path,
            ),
        )
    else:
        # tag with the index of this compound branch and prepend it to the
        # nested branch chain; the resolved path is carried through unchanged
        branches = tuple(
            BranchOrigin(
                branch=branch_idx,
                label=sub.label,
                expression=sub.expression,
                origins=sub.origins,
                branch_chain=(branch_idx,) + sub.branch_chain,
                path=sub.path,
            )
            for sub in branches
        )
    return branches


# ----------------------------------------------------------------------------
# Public API
# ----------------------------------------------------------------------------


def _container_name(container) -> Optional[str]:
    """The explicit name of a CTE/subquery/alias, or ``None`` for an
    anonymous select/compound construct."""
    name = getattr(container, "name", None)
    if isinstance(container, (CTE, Subquery, Alias)) and name:
        return name
    return None


def _extend_path(path: tuple[str, ...], container) -> tuple[str, ...]:
    name = _container_name(container)
    if name and (not path or path[-1] != name):
        return path + (name,)
    return path


def _lineage_for_container(container, path: tuple[str, ...]) -> List[ColumnLineage]:
    result: List[ColumnLineage] = []
    for position, col in enumerate(container.exported_columns):
        branches = _resolve_container_position(container, position, path)
        # surface the deepest nested path reached by any branch
        deepest = path
        for branch in branches:
            if len(branch.path) > len(deepest):
                deepest = branch.path
        result.append(
            ColumnLineage(
                index=position,
                name=getattr(col, "name", None),
                type=getattr(col, "type", None),
                sources=branches,
                path=deepest,
            )
        )
    return result


def lineage_for_column(
    selectable: "ReturnsRows", column: KeyedColumnElement
) -> ColumnLineage:
    """Return the :class:`ColumnLineage` of one column of ``selectable``.

    ``column`` is normally one of ``selectable.exported_columns`` (or
    ``selectable.c``).  The statement is not modified.
    """
    position = _position_in(selectable, column)
    if position is None:
        raise ValueError(
            f"column {column!r} is not an exported column of {selectable!r}"
        )
    name = _container_name(selectable)
    branches = _resolve_container_position(
        selectable, position, (name,) if name else ()
    )
    return ColumnLineage(
        index=position,
        name=getattr(column, "name", None),
        type=getattr(column, "type", None),
        sources=branches,
        path=(name,) if name else (),
    )


def column_lineage(statement: "ReturnsRows") -> ColumnLineageReport:
    """Build a :class:`ColumnLineageReport` describing the source of every
    output column of a Core statement.

    The report is produced entirely by introspecting the clause structure of
    ``statement``; the statement is never rewritten, compiled for inspection
    or executed.  It may subsequently be executed normally.

    :param statement: a :class:`_expression.Select`,
     :class:`_expression.CompoundSelect`, :class:`_expression.CTE`,
     :class:`_expression.Subquery` or other
     :class:`_expression.ReturnsRows` construct.

    """
    name = _container_name(statement)
    path = (name,) if name else ()
    columns = tuple(_lineage_for_container(statement, path))
    return ColumnLineageReport(statement=statement, columns=columns)

