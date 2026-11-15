"""Restricted, data-only filter trees for :py:class:`dataset.Table`.

Query configurations are often stored as plain JSON (database rows, YAML
files, HTTP payloads, ...).  Such a configuration cannot carry Python
objects like SQLAlchemy clauses, so this module provides a small,
serialisable filter language that is compiled into a single SQLAlchemy
predicate.

The format is deliberately *data only*::

    {"all": [
        {"field": "country", "op": "eq", "value": "Germany"},
        {"any": [
            {"field": "temperature", "op": "gte", "value": 20},
            {"not": {"field": "city", "op": "in", "value": ["Berlin", None]}},
        ]},
    ]}

Only the following node types exist:

``all`` / ``any``
    A list of child nodes.  ``all`` is a conjunction, ``any`` a
    disjunction.  An empty ``all`` is ``TRUE`` (the neutral element of
    conjunction) and an empty ``any`` is ``FALSE`` (the neutral element
    of disjunction).
``not``
    A single child node.  Compiles to SQL ``NOT`` and therefore keeps
    SQL's three-valued logic: negating ``UNKNOWN`` stays ``UNKNOWN``.
``field``
    A comparison against a column.  ``field`` names the column,
    ``op`` the operator and ``value`` the right-hand side.  ``op``
    defaults to ``eq``.

Operators
---------

====================== ==================================================
Operator               Meaning
====================== ==================================================
``eq``, ``=``, ``==``  equal (``value: null`` becomes ``IS NULL``)
``is``                 alias for ``eq``
``ne``, ``!=``, ``<>`` not equal (``value: null`` becomes ``IS NOT NULL``)
``gt``, ``>``          greater than
``gte``, ``>=``        greater than or equal
``lt``, ``<``          less than
``lte``, ``<=``        less than or equal
``like``, ``ilike``    SQL ``LIKE`` / ``ILIKE``
``notlike``, ``notilike``
                       negated ``LIKE`` / ``ILIKE``
``startswith``         string prefix match (LIKE wildcards escaped)
``endswith``           string suffix match (LIKE wildcards escaped)
``in``                 membership
``notin``              negated membership
``between``, ``..``    inclusive range, ``value`` is ``[low, high]``
====================== ==================================================

Three-valued logic
------------------

Values are always passed as bind parameters, never spliced into the SQL
text.  ``NULL`` handling is explicit and never relies on Python
truthiness:

* ``in []``  -> ``FALSE`` (nothing can be a member of the empty set).
* ``in [..., None]`` -> ``col IN (...) OR col IS NULL``.  Listing
  ``None`` explicitly makes ``NULL`` rows match.
* ``notin`` is compiled as ``NOT (in ...)`` and therefore follows SQL
  three-valued logic.  For example ``notin ["a"]`` does *not* match rows
  where the column is ``NULL`` because ``NULL NOT IN ('a')`` is
  ``UNKNOWN``.  Use ``notin ["a", None]`` to also exclude ``NULL`` rows
  (which makes it the exact complement of ``in ["a", None]``).
* ``notin []`` -> ``TRUE`` (every row, including ``NULL`` rows).
* ``eq``/``ne`` with ``null`` compile to ``IS NULL`` / ``IS NOT NULL``.
* Ordering comparisons (``gt``/``gte``/``lt``/``lte``) and ``between`` with
  a ``null`` operand bind ``NULL`` as a parameter, so SQL's own three-valued
  logic applies: the comparison is ``UNKNOWN`` and matches no rows (and
  stays ``UNKNOWN`` under ``not``).  Use ``eq``/``ne`` for null tests.

Errors
------

Invalid specifications raise :py:class:`FilterError` (a subclass of
:py:class:`dataset.util.QueryError`) *before* any query is executed.
The message contains the JSON-path of the offending node, e.g.
``$.all[1].op``.  Unknown columns and unknown operators are errors
rather than silently becoming ``FALSE``.

To keep a hostile or broken configuration from exhausting resources,
trees are limited to :py:data:`MAX_FILTER_DEPTH` levels of nesting and
:py:data:`MAX_FILTER_NODES` nodes in total.
"""

from collections.abc import Mapping, Sequence
from datetime import date, datetime
from decimal import Decimal
from typing import TYPE_CHECKING, Any, cast

from sqlalchemy import and_, false, not_, or_, true
from sqlalchemy import column as plain_column
from sqlalchemy.sql.elements import ColumnElement
from sqlalchemy.sql.expression import bindparam

from dataset.util import QueryError, normalize_column_name

if TYPE_CHECKING:
    from dataset.table import Table

#: Maximum nesting depth of a filter tree (the root node has depth ``1``).
MAX_FILTER_DEPTH = 10

#: Maximum total number of nodes in a filter tree.
MAX_FILTER_NODES = 256

#: Type of a filter tree, as loaded from JSON/YAML/...
FilterSpec = Mapping[str, Any]

_SCALAR_TYPES = (bool, int, float, str, bytes, Decimal, date, datetime)

_COMBINATORS = ("all", "any", "not")

_EQ_OPS = frozenset({"eq", "=", "==", "is"})
_NE_OPS = frozenset({"ne", "!=", "<>"})
_GT_OPS = frozenset({"gt", ">"})
_GTE_OPS = frozenset({"gte", ">="})
_LT_OPS = frozenset({"lt", "<"})
_LTE_OPS = frozenset({"lte", "<="})
_LIKE_OPS = frozenset({"like", "ilike", "notlike", "notilike"})
_PREFIX_OPS = frozenset({"startswith", "endswith"})
_IN_OPS = frozenset({"in"})
_NOTIN_OPS = frozenset({"notin"})
_BETWEEN_OPS = frozenset({"between", ".."})

_OPS = (
    _EQ_OPS
    | _NE_OPS
    | _GT_OPS
    | _GTE_OPS
    | _LT_OPS
    | _LTE_OPS
    | _LIKE_OPS
    | _PREFIX_OPS
    | _IN_OPS
    | _NOTIN_OPS
    | _BETWEEN_OPS
)

# Canonical spelling used in error messages.
_OPERATOR_NAMES = ", ".join(
    (
        "eq",
        "ne",
        "gt",
        "gte",
        "lt",
        "lte",
        "like",
        "ilike",
        "notlike",
        "notilike",
        "startswith",
        "endswith",
        "in",
        "notin",
        "between",
    )
)


class FilterError(QueryError):
    """Raised for an invalid structured filter.

    The exception carries the JSON-path (``path``) of the offending node
    and includes it in the message.
    """

    def __init__(self, path: Sequence[str | int], message: str) -> None:
        self.path: tuple[str | int, ...] = tuple(path)
        super().__init__(
            f"invalid structured filter at {format_path(self.path)}: {message}"
        )


def format_path(path: Sequence[str | int]) -> str:
    """Render a filter path such as ``$.all[1].op``."""
    out = "$"
    for part in path:
        if isinstance(part, int):
            out += f"[{part}]"
        else:
            out += f".{part}"
    return out


def _is_scalar(value: Any) -> bool:
    return value is None or isinstance(value, _SCALAR_TYPES)


def _as_bool(expression: Any) -> ColumnElement[bool]:
    return cast(ColumnElement[bool], expression)


class _FilterCompiler:
    """Compile one filter tree against the columns of a table."""

    def __init__(self, table: "Table", max_depth: int, max_nodes: int) -> None:
        self.table = table
        # Validation of column names is only possible once the table exists.
        # Structural validation (node shapes, operators, value types, limits)
        # always runs, so a broken configuration fails fast either way.
        self._has_columns = table.exists
        self.max_depth = max_depth
        self.max_nodes = max_nodes
        self.nodes = 0

    def compile(self, spec: Any) -> ColumnElement[bool]:
        return self._node(spec, (), 1)

    # -- tree traversal ---------------------------------------------------
    def _node(
        self, node: Any, path: tuple[str | int, ...], depth: int
    ) -> ColumnElement[bool]:
        self.nodes += 1
        if self.nodes > self.max_nodes:
            raise FilterError(
                path,
                f"too many filter nodes (maximum is {self.max_nodes})",
            )
        if depth > self.max_depth:
            raise FilterError(
                path,
                f"filter is nested too deeply (maximum depth is {self.max_depth})",
            )
        if not isinstance(node, Mapping):
            raise FilterError(
                path,
                f"expected a filter object, got {type(node).__name__}",
            )

        keys = set(node)
        combinators = [key for key in _COMBINATORS if key in keys]
        if combinators:
            if len(keys) != 1:
                raise FilterError(
                    path,
                    "a combinator node must contain exactly one of "
                    "'all', 'any' or 'not' (together with no other keys)",
                )
            return self._combinator(combinators[0], node[combinators[0]], path, depth)

        if "field" in keys:
            return self._comparison(node, keys, path)

        raise FilterError(
            path,
            "expected a filter object containing 'all', 'any', 'not' or 'field'",
        )

    def _combinator(
        self, name: str, children: Any, path: tuple[str | int, ...], depth: int
    ) -> ColumnElement[bool]:
        if name == "not":
            return _as_bool(not_(self._node(children, (*path, "not"), depth + 1)))
        if not isinstance(children, (list, tuple)):
            raise FilterError(
                (*path, name),
                f"'{name}' requires a list of filter objects",
            )
        parts = [
            self._node(child, (*path, name, index), depth + 1)
            for index, child in enumerate(children)
        ]
        if not parts:
            # Explicit identities, not Python truthiness of the sequence.
            return _as_bool(true() if name == "all" else false())
        if name == "all":
            return _as_bool(and_(*parts))
        return _as_bool(or_(*parts))

    # -- comparisons ------------------------------------------------------
    def _comparison(
        self,
        node: Mapping[str, Any],
        keys: set[str],
        path: tuple[str | int, ...],
    ) -> ColumnElement[bool]:
        extra = keys - {"field", "op", "value"}
        if extra:
            raise FilterError(
                path,
                "unknown key(s) "
                + ", ".join(repr(key) for key in sorted(extra, key=repr))
                + "; a comparison accepts 'field', 'op' and 'value'",
            )

        field = node["field"]
        if not isinstance(field, str) or not field.strip():
            raise FilterError((*path, "field"), "field must be a non-empty string")

        column_name = self._resolve_column(field, (*path, "field"))
        column = (
            self.table.table.c[column_name]
            if self._has_columns
            else plain_column(column_name)
        )

        op = node.get("op", "eq")
        if not isinstance(op, str):
            raise FilterError((*path, "op"), "op must be a string")
        if op not in _OPS:
            raise FilterError(
                (*path, "op"),
                f"unknown operator {op!r}; allowed operators are {_OPERATOR_NAMES}",
            )

        if "value" not in node:
            raise FilterError((*path, "value"), f"operator {op!r} requires a 'value'")
        value = node["value"]

        if op in _EQ_OPS:
            _check_scalar(value, (*path, "value"))
            if value is None:
                return _as_bool(column.is_(None))
            return _as_bool(column == value)
        if op in _NE_OPS:
            _check_scalar(value, (*path, "value"))
            if value is None:
                return _as_bool(column.is_not(None))
            return _as_bool(column != value)
        if op in _GT_OPS:
            return _as_bool(column > _ordering_value(value, (*path, "value")))
        if op in _GTE_OPS:
            return _as_bool(column >= _ordering_value(value, (*path, "value")))
        if op in _LT_OPS:
            return _as_bool(column < _ordering_value(value, (*path, "value")))
        if op in _LTE_OPS:
            return _as_bool(column <= _ordering_value(value, (*path, "value")))
        if op in _LIKE_OPS:
            if not isinstance(value, str):
                raise FilterError(
                    (*path, "value"), f"operator {op!r} requires a string value"
                )
            if op == "ilike":
                return _as_bool(column.ilike(value))
            if op == "notlike":
                return _as_bool(column.notlike(value))
            if op == "notilike":
                return _as_bool(column.notilike(value))
            return _as_bool(column.like(value))
        if op in _PREFIX_OPS:
            if not isinstance(value, str):
                raise FilterError(
                    (*path, "value"), f"operator {op!r} requires a string value"
                )
            # autoescape=True keeps LIKE wildcards in the value literal, so
            # user data cannot broaden the match pattern.
            if op == "startswith":
                return _as_bool(column.startswith(value, autoescape=True))
            return _as_bool(column.endswith(value, autoescape=True))
        if op in _IN_OPS:
            return self._membership(column, value, path)
        if op in _NOTIN_OPS:
            return _as_bool(not_(self._membership(column, value, path)))
        # between / ..
        return self._between(column, value, path)

    def _resolve_column(self, field: str, path: tuple[str | int, ...]) -> str:
        if not self._has_columns:
            # The table does not exist yet; the query will return no rows
            # anyway, so only structural validation is possible here.
            try:
                return normalize_column_name(field)
            except ValueError as exc:
                raise FilterError(path, str(exc)) from exc
        try:
            name = self.table._get_column_name(field)
        except ValueError as exc:
            raise FilterError(path, str(exc)) from exc
        if not self.table.has_column(name):
            raise FilterError(path, f"unknown column {field!r}")
        return name

    def _membership(
        self, column: Any, value: Any, path: tuple[str | int, ...]
    ) -> ColumnElement[bool]:
        if not isinstance(value, (list, tuple)):
            raise FilterError(
                (*path, "value"),
                "'in'/'notin' require a list of values",
            )
        for index, item in enumerate(value):
            _check_scalar(item, (*path, "value", index))

        non_null = [item for item in value if item is not None]
        has_null = len(non_null) != len(value)

        parts: list[ColumnElement[bool]] = []
        if non_null:
            parts.append(_as_bool(column.in_(non_null)))
        if has_null:
            parts.append(_as_bool(column.is_(None)))

        if not parts:
            # Empty list: explicit identity rather than Python truthiness.
            return _as_bool(false())
        if len(parts) == 1:
            return parts[0]
        return _as_bool(or_(*parts))

    def _between(
        self, column: Any, value: Any, path: tuple[str | int, ...]
    ) -> ColumnElement[bool]:
        if not isinstance(value, (list, tuple)) or len(value) != 2:
            raise FilterError(
                (*path, "value"),
                "'between' requires a list of exactly two values",
            )
        low, high = value
        # ``None`` bounds are bound as parameters; SQL's three-valued logic
        # then makes the range UNKNOWN (no rows match), never TRUE.
        _check_scalar(low, (*path, "value", 0))
        _check_scalar(high, (*path, "value", 1))
        return _as_bool(column.between(_bind_or_value(low), _bind_or_value(high)))


def _check_scalar(value: Any, path: tuple[str | int, ...]) -> None:
    if _is_scalar(value):
        return
    raise FilterError(
        path,
        f"unsupported value of type {type(value).__name__}; values must be "
        "plain data (str, int, float, bool, bytes, Decimal, date, datetime "
        "or null) and are always passed as bind parameters",
    )


def _bind_or_value(value: Any) -> Any:
    """Return ``value`` as-is, or a bind parameter when it is ``None``.

    SQLAlchemy refuses to build ``column > None`` (or ``between(None, x)``)
    directly.  Wrapping ``None`` in a bind parameter keeps the value
    strictly data-only while letting SQL's three-valued logic decide the
    outcome.
    """
    if value is None:
        return bindparam(None, None, unique=True)
    return value


def _ordering_value(value: Any, path: tuple[str | int, ...]) -> Any:
    # ``None`` is allowed: it is bound as a parameter and SQL's three-valued
    # logic makes the comparison UNKNOWN (no rows match). This keeps the
    # result consistent with the rest of the tree, including under ``not``.
    _check_scalar(value, path)
    return _bind_or_value(value)


def compile_filter(
    spec: FilterSpec,
    table: "Table",
    *,
    max_depth: int = MAX_FILTER_DEPTH,
    max_nodes: int = MAX_FILTER_NODES,
) -> ColumnElement[bool]:
    """Compile a structured filter ``spec`` against ``table``.

    The returned SQLAlchemy predicate can be passed to
    :py:meth:`Table.find() <dataset.Table.find>` and
    :py:meth:`Table.count() <dataset.Table.count>` alike, so both entry
    points select exactly the same rows.

    Invalid specifications raise :py:class:`FilterError` before any query
    is executed.
    """
    return _FilterCompiler(table, max_depth=max_depth, max_nodes=max_nodes).compile(
        spec
    )
