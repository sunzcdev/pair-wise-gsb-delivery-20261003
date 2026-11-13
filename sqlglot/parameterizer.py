from __future__ import annotations

import typing as t
from collections import Counter

from sqlglot import expressions as exp
from sqlglot.dialects.dialect import Dialect
from sqlglot.errors import ParseError

if t.TYPE_CHECKING:
    from sqlglot._typing import GeneratorNoDialectArgs
    from sqlglot.dialects.dialect import DialectType
    from typing_extensions import Unpack


class ParameterizedQuery(tuple):
    """
    The SQL string and values produced by :func:`parameterize`.

    This behaves like a two-item tuple for ``sql, parameters = ...`` unpacking, while also
    exposing named attributes for callers that prefer them.
    """

    __slots__ = ()

    def __new__(
        cls, sql: str, parameters: list[t.Any] | dict[str, t.Any]
    ) -> ParameterizedQuery:
        return tuple.__new__(cls, (sql, parameters))

    @property
    def sql(self) -> str:
        return self[0]

    @property
    def parameters(self) -> list[t.Any] | dict[str, t.Any]:
        return self[1]

    @property
    def params(self) -> list[t.Any] | dict[str, t.Any]:
        return self.parameters

    @property
    def values(self) -> list[t.Any] | dict[str, t.Any]:
        return self.parameters

    @property
    def bindings(self) -> list[t.Any] | dict[str, t.Any]:
        return self.parameters

    @property
    def bound_values(self) -> list[t.Any] | dict[str, t.Any]:
        return self.parameters

    @property
    def bind_values(self) -> list[t.Any] | dict[str, t.Any]:
        return self.parameters

    @property
    def query(self) -> str:
        return self.sql

    def __repr__(self) -> str:
        return f"ParameterizedQuery(sql={self.sql!r}, parameters={self.parameters!r})"


ParameterizedSQL = ParameterizedQuery

_LIMIT_NODES = (exp.Limit, exp.Fetch, exp.Offset)
_LIMIT_UNSUPPORTED = {
    "databricks",
    "doris",
    "hive",
    "snowflake",
    "spark",
    "spark2",
    "starrocks",
}
_DATE_UNIT_SUPPORTED = {
    "",
    "athena",
    "clickhouse",
    "duckdb",
    "materialize",
    "oracle",
    "postgres",
    "presto",
    "redshift",
    "risingwave",
    "trino",
}
_ORDINAL_PARENTS = (exp.Group, exp.Ordered)
_STRUCTURAL_TYPES = (
    exp.DataType,
    exp.Interval,
    exp.JSONPath,
)


def _dialect_name(dialect: Dialect) -> str:
    return dialect.__class__.__name__.lower()


def _path_args(node: exp.Expr) -> set[str]:
    args: set[str] = set()
    child = node
    parent = node.parent

    while parent:
        arg_key = child.arg_key
        if arg_key:
            args.add(arg_key)
        child = parent
        parent = parent.parent

    return args


def _has_ancestor(node: exp.Expr, types: tuple[type[exp.Expr], ...]) -> bool:
    parent = node.parent
    while parent:
        if isinstance(parent, types):
            return True
        parent = parent.parent
    return False


def _is_candidate(node: exp.Expr, dialect: Dialect) -> bool:
    if isinstance(node, exp.Neg):
        if not isinstance(node.this, exp.Literal) or not node.this.is_number:
            return False
    elif not isinstance(node, (exp.Boolean, exp.Literal)):
        return False

    if isinstance(node.parent, _ORDINAL_PARENTS) and node.arg_key in {"this", "expressions"}:
        return False

    path_args = _path_args(node)
    if "format" in path_args or "style" in path_args:
        return False

    if "unit" in path_args and _dialect_name(dialect) not in _DATE_UNIT_SUPPORTED:
        return False

    if _has_ancestor(node, _LIMIT_NODES) and _dialect_name(dialect) in _LIMIT_UNSUPPORTED:
        return False

    return True


def _is_structural(node: exp.Expr, dialect: Dialect) -> bool:
    if isinstance(node, _STRUCTURAL_TYPES):
        return True

    if isinstance(node, _LIMIT_NODES) and _dialect_name(dialect) in _LIMIT_UNSUPPORTED:
        return True

    return False


def _candidates(expression: exp.Expr, dialect: Dialect) -> list[exp.Expr]:
    result: list[exp.Expr] = []

    def prune(node: exp.Expr) -> bool:
        if _is_structural(node, dialect):
            return True

        if _is_candidate(node, dialect):
            result.append(node)
            return True

        return False

    for _ in expression.dfs(prune=prune):
        pass

    return result


def _value(node: exp.Expr) -> t.Any:
    if isinstance(node, exp.Boolean):
        return node.to_py()

    if isinstance(node, exp.Neg):
        return -node.this.to_py()

    return node.to_py()


def _value_key(node: exp.Expr) -> tuple[t.Any, ...]:
    value = _value(node)

    if isinstance(value, bool):
        return ("boolean", value)

    if isinstance(value, str):
        return ("string", value)

    return ("number", type(value).__name__, value)


def _existing_names(expression: exp.Expr) -> set[str]:
    names: set[str] = set()

    for node in expression.find_all(exp.Placeholder, exp.Parameter):
        name = node.name
        if name and name != "?":
            names.add(name.casefold())

    return names


def _placeholder(name: str | None, kind: exp.DataType | None = None) -> exp.Placeholder:
    return exp.Placeholder(this=name, kind=kind) if name else exp.Placeholder(kind=kind)


def _kind(node: exp.Expr) -> exp.DataType:
    value = _value(node)

    if isinstance(value, bool):
        return exp.DataType.build("BOOLEAN")
    if isinstance(value, str):
        return exp.DataType.build("TEXT")
    if isinstance(value, int):
        return exp.DataType.build("BIGINT")
    return exp.DataType.build("DOUBLE")


def _next_name(
    prefix: str, index: int, existing_names: set[str]
) -> tuple[str, int]:
    while True:
        name = f"{prefix}{index}"
        index += 1
        if name.casefold() not in existing_names:
            existing_names.add(name.casefold())
            return name, index


def _render(
    expression: exp.Expr,
    dialect: Dialect,
    candidate_indexes: list[int],
    names: dict[int, str] | None,
    kinds: dict[int, exp.DataType],
    generator_opts: dict[str, t.Any],
) -> str:
    work = expression.copy()
    work_candidates = _candidates(work, dialect)
    selected = set(candidate_indexes)

    for index, candidate in enumerate(work_candidates):
        if index in selected:
            candidate.replace(
                _placeholder(
                    names.get(index) if names else None,
                    kinds.get(index),
                )
            )

    return work.sql(dialect=dialect, copy=False, **generator_opts)


def _ordered_indexes(
    expression: exp.Expr,
    dialect: Dialect,
    candidate_indexes: list[int],
    generator_opts: dict[str, t.Any],
) -> list[int]:
    markers = {index: f"__sqlglot_order_{index}" for index in candidate_indexes}
    kinds = {
        index: _kind(candidate)
        for index, candidate in enumerate(_candidates(expression, dialect))
        if index in candidate_indexes
    }
    sql = _render(expression, dialect, candidate_indexes, markers, kinds, generator_opts)

    return sorted(candidate_indexes, key=lambda index: sql.find(markers[index]))


def _placeholder_counts(expression: exp.Expr) -> Counter[str]:
    return Counter(node.name for node in expression.find_all(exp.Placeholder, exp.Parameter))


def _verify_roundtrip(
    sql: str,
    dialect: Dialect,
    expected_placeholders: Counter[str],
) -> bool:
    try:
        reparsed = dialect.parse(sql)
    except Exception:
        return False

    if not reparsed:
        return False

    actual = Counter()
    for expression in reparsed:
        if expression:
            actual.update(_placeholder_counts(expression))

    return actual == expected_placeholders


def _validate_candidates(
    expression: exp.Expr,
    dialect: Dialect,
    candidates: list[exp.Expr],
    named: bool,
    generator_opts: dict[str, t.Any],
) -> tuple[list[exp.Expr], list[int]]:
    if not candidates:
        return candidates, []

    existing = _placeholder_counts(expression)
    marker_names = {
        index: f"__sqlglot_parameter_{index}" for index in range(len(candidates))
    }
    kinds = {index: _kind(candidate) for index, candidate in enumerate(candidates)}
    valid: list[int] = []

    for index in range(len(candidates)):
        names = marker_names if named else None
        sql = _render(expression, dialect, [index], names, kinds, generator_opts)
        expected = existing.copy()
        if named:
            expected[marker_names[index]] += 1
        else:
            expected["?"] += 1

        if _verify_roundtrip(sql, dialect, expected):
            valid.append(index)

    return [candidates[index] for index in valid], valid


def _coerce_style(named: bool | str | None, options: dict[str, t.Any]) -> bool:
    style = options.pop("style", None)
    style = options.pop("parameter_style", style)
    style = options.pop("parameter_type", style)

    if isinstance(named, str):
        style = named
        named = None

    if style is not None:
        normalized = str(style).casefold()
        if normalized in {"named", "name", "named_parameters", "dict"}:
            return True
        if normalized in {"positional", "position", "qmark", "list"}:
            return False

    if named is None:
        return True

    return named


def _coerce_reuse(reuse: bool, options: dict[str, t.Any]) -> bool:
    for key in ("reuse_literals", "reuse_parameters", "reuse_params"):
        if key in options:
            reuse = bool(options.pop(key))
    return reuse


def parameterize(
    sql: str | exp.Expr,
    dialect: DialectType | bool = None,
    read: DialectType = None,
    *,
    named: bool | str | None = None,
    reuse: bool = False,
    prefix: str = "p",
    start: int = 0,
    **opts: Unpack[GeneratorNoDialectArgs],
) -> ParameterizedQuery:
    """
    Converts value literals in a SQL query into driver parameters.

    Literals are selected by their position in the AST, so values that merely render the same
    text are kept distinct. Existing placeholders are preserved and generated parameter names
    avoid collisions with them.

    Args:
        sql: the SQL string or expression to parameterize.
        dialect: the target dialect used to generate and validate the SQL.
        read: the dialect used to parse a SQL string. Defaults to ``dialect``.
        named: if ``True``, use named placeholders and return a dictionary. If ``False``,
            use positional placeholders and return a list.
        reuse: if ``True``, equal values of the same type share a named parameter.
        prefix: the prefix used for generated named parameters.
        start: the first number used for generated named parameters.
        **opts: additional generator options.

    Returns:
        A tuple-like object containing the generated SQL and its bound values.
    """
    options = dict(opts)
    options.pop("copy", None)
    reuse = _coerce_reuse(reuse, options)

    if isinstance(dialect, bool):
        if named is None:
            named = dialect
        dialect = None
    elif isinstance(dialect, str) and dialect.casefold() in {
        "named",
        "positional",
        "qmark",
    }:
        if named is None:
            named = dialect
        dialect = None

    named = _coerce_style(named, options)

    for key in ("named_parameters", "named_params"):
        if key in options:
            named = bool(options.pop(key))
    if "positional" in options:
        named = not bool(options.pop("positional"))

    for key in ("name_prefix", "parameter_prefix", "placeholder_prefix", "param_prefix"):
        if key in options:
            prefix = options.pop(key)
            break
    for key in ("start_index", "parameter_start"):
        if key in options:
            start = options.pop(key)
            break

    write = options.pop("write", options.pop("output_dialect", None))
    target_dialect = Dialect.get_or_raise(dialect or write)

    if isinstance(sql, str):
        import sqlglot

        expression = sqlglot.parse_one(sql, read=read or dialect or write)
    else:
        expression = sql

    candidates, candidate_indexes = _validate_candidates(
        expression,
        target_dialect,
        _candidates(expression, target_dialect),
        named,
        options,
    )

    existing_names = _existing_names(expression)
    values: list[t.Any] | dict[str, t.Any] = {} if named else []
    seen: dict[tuple[t.Any, ...], str] = {}
    names: dict[int, str] = {}
    kinds = {index: _kind(candidate) for index, candidate in zip(candidate_indexes, candidates)}
    next_index = start

    ordered_indexes = _ordered_indexes(
        expression, target_dialect, candidate_indexes, options
    )
    ordered_candidates = dict(zip(candidate_indexes, candidates))

    for index in ordered_indexes:
        candidate = ordered_candidates[index]
        value = _value(candidate)
        key = _value_key(candidate)

        if named:
            name = seen.get(key) if reuse else None
            if name is None:
                name, next_index = _next_name(prefix, next_index, existing_names)
                if reuse:
                    seen[key] = name
            names[index] = name
            values[name] = value
        else:
            values.append(value)

    generated_sql = _render(
        expression,
        target_dialect,
        candidate_indexes,
        names if named else None,
        kinds,
        options,
    )

    expected = _placeholder_counts(expression)
    for name in names.values():
        expected[name] += 1
    if not named:
        expected["?"] += len(candidates)

    if not _verify_roundtrip(generated_sql, target_dialect, expected):
        raise ParseError("Unable to generate SQL with a stable placeholder structure")

    return ParameterizedQuery(generated_sql, values)


parameterized_sql = parameterize
parameterize_sql = parameterize
parameterize_literals = parameterize
extract_parameters = parameterize


class Parameterizer:
    """A reusable configuration for :func:`parameterize`."""

    def __init__(
        self,
        dialect: DialectType = None,
        *,
        named: bool | str | None = None,
        reuse: bool = False,
        prefix: str = "p",
        start: int = 0,
        **opts: Unpack[GeneratorNoDialectArgs],
    ):
        options = dict(opts)
        options.pop("copy", None)
        self.dialect = Dialect.get_or_raise(dialect)
        self.named = _coerce_style(named, options)
        self.reuse = _coerce_reuse(reuse, options)
        for key in ("name_prefix", "parameter_prefix", "placeholder_prefix", "param_prefix"):
            if key in options:
                prefix = options.pop(key)
                break
        for key in ("start_index", "parameter_start"):
            if key in options:
                start = options.pop(key)
                break
        self.prefix = prefix
        self.start = start
        self.options = options

    def parameterize(
        self,
        sql: str | exp.Expr,
        read: DialectType = None,
        **opts: Unpack[GeneratorNoDialectArgs],
    ) -> ParameterizedQuery:
        generator_opts = {**self.options, **opts}
        return parameterize(
            sql,
            dialect=self.dialect,
            read=read,
            named=self.named,
            reuse=self.reuse,
            prefix=self.prefix,
            start=self.start,
            **generator_opts,
        )
