from __future__ import annotations

import copy
import typing as t
from enum import auto

from sqlglot import expressions as exp
from sqlglot.errors import ErrorLevel, ParseError
from sqlglot.helper import AutoName, ensure_collection
from sqlglot.schema import MappingSchema, ensure_schema, flatten_schema
from sqlglot.trie import new_trie

if t.TYPE_CHECKING:
    from sqlglot.dialects.dialect import DialectType
    from sqlglot.schema import ColumnMapping, Schema


class AnalysisStatus(AutoName):
    """The determinism of the catalog state after analyzing a single statement."""

    SUCCESS = auto()
    """The statement was fully understood and its catalog effect (if any) was applied."""

    UNKNOWN = auto()
    """
    The statement may modify the catalog but is not part of the supported DDL subset, so its
    effect cannot be modeled. The catalog is left untouched and any conclusion that depends on
    this statement must be treated as non-deterministic.
    """

    ERROR = auto()
    """The statement could not be parsed. No partial catalog change is retained."""


class Catalog:
    """
    An in-memory, versioned catalog that models the structural changes a SQL script performs
    without connecting to a database.

    Temporary objects are tracked separately from persistent objects that share the same name:
    a temporary table shadows any same-named persistent table until the temporary table is
    dropped, at which point resolution falls back to the persistent object.

    Only the explicitly supported DDL subset is modeled. Anything else that could change the
    catalog is reported as unknown instead of guessing.

    Args:
        schema: the initial persistent schema. When a mapping/dict is provided it is copied, so
            repeated analysis never mutates the caller's input.
        dialect: the dialect used to normalize identifiers and parse string arguments.
    """

    def __init__(
        self,
        schema: Schema | dict[str, object] | None = None,
        dialect: DialectType = None,
    ) -> None:
        self.dialect = dialect
        self.version: int = 0
        self.persistent: MappingSchema = self._init_schema(schema)
        self.temporary: MappingSchema = MappingSchema(dialect=dialect)

    def _init_schema(self, schema: Schema | dict[str, object] | None) -> MappingSchema:
        if isinstance(schema, MappingSchema):
            return schema.copy()
        if schema is None:
            return MappingSchema(dialect=self.dialect)
        if isinstance(schema, dict):
            return MappingSchema(copy.deepcopy(schema), dialect=self.dialect)
        return ensure_schema(schema, dialect=self.dialect)

    @property
    def snapshot(self) -> tuple[dict[str, object], dict[str, object]]:
        """Deep copies of the current persistent and temporary table mappings."""
        return copy.deepcopy(self.persistent.mapping), copy.deepcopy(self.temporary.mapping)

    def restore(self, snapshot: tuple[dict[str, object], dict[str, object]]) -> None:
        """Restore the persistent/temporary mappings captured by `snapshot`."""
        persistent, temporary = snapshot
        self.persistent = MappingSchema(persistent, dialect=self.dialect)
        self.temporary = MappingSchema(temporary, dialect=self.dialect)

    def is_temporary(self, table: exp.Table) -> bool:
        return self.temporary.find(table, raise_on_missing=False) is not None

    def has_table(self, table: exp.Table) -> bool:
        """Whether `table` resolves, giving temporary objects precedence over persistent ones."""
        return (
            self.temporary.find(table, raise_on_missing=False) is not None
            or self.persistent.find(table, raise_on_missing=False) is not None
        )

    def column_names(self, table: exp.Table) -> list[str]:
        target = self.temporary if self.is_temporary(table) else self.persistent
        return target.column_names(table, dialect=self.dialect)

    def get_column_type(self, table: exp.Table, column: exp.Column | str) -> exp.DataType:
        target = self.temporary if self.is_temporary(table) else self.persistent
        return target.get_column_type(table, column, dialect=self.dialect)

    def _add_table(
        self,
        table: exp.Table,
        column_mapping: ColumnMapping | None,
        temporary: bool,
    ) -> None:
        target = self.temporary if temporary else self.persistent
        target.add_table(table, column_mapping, dialect=self.dialect, match_depth=False)

    def _drop_table(self, table: exp.Table, temporary: bool) -> bool:
        """
        Remove `table`, returning whether an object was actually deleted.

        A temporary drop only touches the temporary namespace; a plain drop removes a temporary
        object first and otherwise removes the persistent object, restoring persistent
        resolution for the name.
        """
        if temporary or self.temporary.find(table, raise_on_missing=False) is not None:
            parts = self.temporary.table_parts(table)
            removed = _nested_discard(self.temporary.mapping, tuple(reversed(parts)))
            self._rebuild_trie(self.temporary)
            return removed
        parts = self.persistent.table_parts(table)
        removed = _nested_discard(self.persistent.mapping, tuple(reversed(parts)))
        self._rebuild_trie(self.persistent)
        return removed

    def _rebuild_trie(self, target: MappingSchema) -> None:
        depth = target.depth()
        target.mapping_trie = new_trie(
            tuple(reversed(parts))
            for parts in flatten_schema(target.mapping, depth=depth)
        )
        target._find_cache.clear()

    def apply(self, expression: exp.Expr) -> AnalysisStatus:
        """
        Apply a single parsed statement to the catalog.

        The change is computed against a snapshot and only committed once the whole statement is
        understood, so a failed or unknown statement never leaves a half-built structure behind.

        Returns:
            `AnalysisStatus.SUCCESS` when the statement is fully modeled (including read-only
            statements that leave the catalog untouched) or `AnalysisStatus.UNKNOWN` when the
            statement could change the catalog but is outside the supported subset.
        """
        if not _mutates_catalog(expression):
            return AnalysisStatus.SUCCESS

        snapshot = self.snapshot
        try:
            unknown = self._apply_mutation(expression)
        except Exception:
            self.restore(snapshot)
            raise

        if unknown:
            self.restore(snapshot)
            return AnalysisStatus.UNKNOWN

        self.version += 1
        return AnalysisStatus.SUCCESS

    def _apply_mutation(self, expression: exp.Expr) -> bool:
        """Returns True when the mutation is unsupported, False when it was applied."""
        if isinstance(expression, exp.Create):
            return self._apply_create(expression)
        if isinstance(expression, exp.Drop):
            return self._apply_drop(expression)
        return True

    def _apply_create(self, expression: exp.Create) -> bool:
        if expression.kind != "TABLE":
            # Views and other objects can shadow tables but are not part of the supported subset.
            return True

        target = expression.this
        if not isinstance(target, (exp.Table, exp.Schema)):
            return True

        table = target.this if isinstance(target, exp.Schema) else target
        if not isinstance(table, exp.Table):
            return True

        temporary = expression.find(exp.TemporaryProperty) is not None

        if isinstance(target, exp.Schema):
            column_mapping = {
                column.name: column.kind if isinstance(column.kind, exp.DataType) else None
                for column in target.expressions
                if isinstance(column, exp.ColumnDef)
            }
            if not column_mapping:
                return True
        elif expression.expression is not None:
            column_mapping = _infer_ctas_columns(expression, self)
            if column_mapping is None:
                return True
        else:
            return True

        if expression.args.get("exists") and self.has_table(table):
            # CREATE TABLE IF NOT EXISTS is a no-op when the name already resolves.
            return False

        self._add_table(table, column_mapping, temporary)
        return False

    def _apply_drop(self, expression: exp.Drop) -> bool:
        if expression.kind != "TABLE":
            return True

        tables = list(ensure_collection(expression.args.get("tables")))
        if not tables or not all(isinstance(table, exp.Table) for table in tables):
            return True

        temporary = bool(expression.args.get("temporary")) or (
            expression.find(exp.TemporaryProperty) is not None
        )

        for table in tables:
            if not expression.args.get("exists") and not self.has_table(table):
                raise ParseError(f"Cannot drop unknown table: {table.sql(dialect=self.dialect)}")
            self._drop_table(table, temporary)
        return False


class StatementResult(t.NamedTuple):
    """The outcome of analyzing one statement."""

    expression: exp.Expr | None
    """The parsed AST, or None when the statement failed to parse."""

    status: AnalysisStatus
    """Whether the statement was fully modeled, unknown, or failed to parse."""

    catalog_version: int
    """The catalog version in effect while the statement was analyzed."""

    error: ParseError | None
    """The parse error when `status` is `AnalysisStatus.ERROR`, otherwise None."""


def analyze_script(
    sql: str | t.Collection[str],
    schema: Schema | dict[str, object] | None = None,
    dialect: DialectType = None,
    error_level: ErrorLevel = ErrorLevel.IGNORE,
) -> list[StatementResult]:
    """
    Analyze one or more SQL statements in order, maintaining an in-memory catalog as it evolves.

    The catalog starts from `schema` and is updated statement-by-statement as supported DDL
    (CREATE TABLE, CTAS and DROP TABLE) is encountered, so later statements resolve against the
    structures introduced by earlier ones. No database connection is made.

    Each result records the catalog version visible to its statement: the version is incremented
    only after a statement that successfully changes the catalog, so read-only statements and
    unknown/failed statements reuse the current version. The caller's `schema` is never mutated.

    Args:
        sql: a SQL script or a collection of individual SQL statements.
        schema: the initial persistent schema (a mapping or `Schema` instance), copied on input.
        dialect: the dialect used for parsing and identifier normalization.
        error_level: how parse errors are reported. `IGNORE` records an `ERROR` result and
            continues; `RAISE`/`IMMEDIATE` re-raise the underlying `ParseError`.

    Returns:
        One `StatementResult` per input statement, in statement order.
    """
    catalog = Catalog(schema, dialect=dialect)
    statements = _parse_statements(sql, dialect, error_level)

    results: list[StatementResult] = []
    for expression, error in statements:
        version = catalog.version
        if error is not None:
            if error_level in (ErrorLevel.RAISE, ErrorLevel.IMMEDIATE):
                raise error
            results.append(StatementResult(expression, AnalysisStatus.ERROR, version, error))
            continue

        try:
            status = catalog.apply(expression)
        except ParseError as apply_error:
            if error_level in (ErrorLevel.RAISE, ErrorLevel.IMMEDIATE):
                raise apply_error
            results.append(
                StatementResult(expression, AnalysisStatus.ERROR, version, apply_error)
            )
        else:
            results.append(StatementResult(expression, status, version, None))

    return results


def _parse_statements(
    sql: str | t.Collection[str],
    dialect: DialectType,
    error_level: ErrorLevel,
) -> list[tuple[exp.Expr | None, ParseError | None]]:
    if isinstance(sql, str):
        raw_statements = (sql,)
    else:
        raw_statements = tuple(sql)

    from sqlglot.dialects.dialect import Dialect

    parser_dialect = Dialect.get_or_raise(dialect)
    parsed: list[tuple[exp.Expr | None, ParseError | None]] = []
    for statement in raw_statements:
        try:
            expressions = parser_dialect.parse(statement, error_level=error_level)
        except ParseError as error:
            parsed.append((None, error))
            continue

        for expression in expressions:
            if expression is None:
                parsed.append((None, ParseError(f"No expression was parsed from '{statement}'.")))
            else:
                parsed.append((expression, None))
    return parsed


def _mutates_catalog(expression: exp.Expr) -> bool:
    if isinstance(expression, (exp.Create, exp.Drop, exp.Alter, exp.TruncateTable)):
        return True

    # A Command is the parser's fallback for syntax it does not model (e.g. RENAME TABLE), so it
    # must be treated as potentially catalog-mutating rather than assumed to be read-only.
    return isinstance(expression, exp.Command)


def _nested_discard(mapping: dict[str, object], parts: t.Sequence[str]) -> bool:
    """Delete the leaf at `parts` from a nested mapping, pruning emptied parent dicts."""
    if not parts:
        return False

    chain: list[dict[str, object]] = [mapping]
    container: dict[str, object] = mapping
    for part in parts[:-1]:
        nested = container.get(part)
        if not isinstance(nested, dict):
            return False
        container = nested
        chain.append(container)

    if parts[-1] not in container:
        return False

    del container[parts[-1]]

    for parent, part in zip(reversed(chain[:-1]), reversed(parts[:-1])):
        if not parent[part]:
            del parent[part]
        else:
            break
    return True


def _infer_ctas_columns(
    expression: exp.Create,
    catalog: Catalog,
) -> ColumnMapping | None:
    """
    Build the column mapping for a CREATE TABLE AS SELECT statement.

    Returns None when the output columns cannot be determined (e.g. an unqualified star whose
    source is unknown), which makes the whole statement unknown rather than guessing.
    """
    query = expression.expression
    if not isinstance(query, exp.Query):
        return None

    query = _expand_stars(query, catalog)
    if query is None:
        return None

    try:
        from sqlglot.optimizer.annotate_types import annotate_types

        annotated = annotate_types(query, dialect=catalog.dialect)
    except Exception:
        annotated = query

    columns: dict[str, exp.DataType | None] = {}
    for projection in annotated.expressions:
        if isinstance(projection, exp.Star):
            return None

        name = projection.alias_or_name
        if not name or name == "*":
            return None

        data_type = projection.type if isinstance(projection.type, exp.DataType) else None
        if data_type is not None and data_type.is_type(exp.DataType.Type.UNKNOWN):
            data_type = None
        columns[name] = data_type

    return columns or None


def _expand_stars(query: exp.Query, catalog: Catalog) -> exp.Query | None:
    """
    Resolve star projections against the catalog so a CTAS over a known table gets explicit
    columns. Returns None when a star references a table the catalog cannot resolve.
    """
    if not any(query.find_all(exp.Star)):
        return query.copy()

    persistent, temporary = catalog.snapshot
    resolution_schema = MappingSchema(
        _merge_mappings(persistent, temporary), dialect=catalog.dialect
    )

    try:
        from sqlglot.optimizer.qualify import qualify

        qualified = qualify(
            query.copy(),
            schema=resolution_schema,
            dialect=catalog.dialect,
            validate_qualify_columns=False,
        )
    except Exception:
        return None

    return None if any(qualified.find_all(exp.Star)) else qualified


def _merge_mappings(
    persistent: dict[str, object], temporary: dict[str, object]
) -> dict[str, object]:
    """
    Overlay temporary table mappings on top of the persistent ones.

    The merge descends catalog/db namespaces but stops at the table level, because a temporary
    table fully shadows any same-named persistent table rather than extending its columns.
    """
    merged = copy.deepcopy(persistent)
    _overlay_tables(merged, copy.deepcopy(temporary), _table_depth(persistent, temporary))
    return merged


def _table_depth(persistent: dict[str, object], temporary: dict[str, object]) -> int:
    """Nesting depth (1 = bare table) at which a mapping level holds table entries."""
    from sqlglot.helper import dict_depth

    return max(dict_depth(persistent) - 1, dict_depth(temporary) - 1, 1)


def _overlay_tables(
    target: dict[str, object], source: dict[str, object], table_depth: int
) -> None:
    for key, value in source.items():
        existing = target.get(key)
        if table_depth > 1 and isinstance(value, dict) and isinstance(existing, dict):
            _overlay_tables(existing, value, table_depth - 1)
        else:
            target[key] = value
