"""
Read-only schema compatibility checking between two SQLite databases.

Given a *contract* database (the schema an ingestion writer expects) and a
*target* database (the database that will actually be written to), these
helpers decide whether the writer can keep using the target as-is, whether an
explicit schema conversion is required first, or whether the two schemas are
fundamentally incompatible.

The comparison is structural, based on ``PRAGMA`` introspection rather than the
text of the ``CREATE TABLE`` statements, so cosmetic differences in
whitespace, quoting and statement layout are ignored. Every database is
opened (or treated) read-only: nothing in this module ever writes.
"""

from __future__ import annotations

import enum
import pathlib
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any, Union

from .utils import column_affinity, sqlite3

if TYPE_CHECKING:
    import sqlite3 as _sqlite3


class CompatibilityLevel(enum.Enum):
    """
    The reportable compatibility outcomes, in increasing order of severity,
    plus ``UNKNOWN`` for structures that cannot be verified safely.
    """

    #: The contract can be used against the target without any conversion.
    COMPATIBLE = "compatible"
    #: A well-defined, data-preserving schema conversion can make them match.
    CONVERSION_REQUIRED = "conversion_required"
    #: No safe conversion exists, typically because data would have to be
    #: invented to satisfy a constraint.
    INCOMPATIBLE = "incompatible"
    #: The structure could not be parsed or verified (expression indexes,
    #: generated columns involved in a write, etc.). It must not be assumed
    #: safe.
    UNKNOWN = "unknown"


class ObjectType(enum.Enum):
    """The kind of schema object a :class:`Finding` describes."""

    DATABASE = "database"
    TABLE = "table"
    VIEW = "view"
    COLUMN = "column"
    PRIMARY_KEY = "primary_key"
    UNIQUE_CONSTRAINT = "unique_constraint"
    FOREIGN_KEY = "foreign_key"
    INDEX = "index"


@dataclass(frozen=True)
class Finding:
    """
    A single compatibility observation about a schema object.

    ``level`` classifies the observation, ``object_type`` and ``object_name``
    identify what it concerns and ``reason`` explains the difference in human
    readable terms. ``detail`` optionally carries the structured values that
    were compared, for example the contract and target column affinities.
    """

    level: CompatibilityLevel
    object_type: ObjectType
    object_name: str
    reason: str
    detail: dict[str, Any] = field(default_factory=dict)


@dataclass
class SchemaCompatibilityReport:
    """
    The result of comparing a contract schema against a target schema.

    Findings are grouped by :class:`CompatibilityLevel` through
    :attr:`findings_by_level`. ``verdict`` is the overall result, which is the
    most severe level observed - an unknown finding prevents a clean
    ``COMPATIBLE`` verdict because unverified structures must never be assumed
    safe.
    """

    findings: list[Finding] = field(default_factory=list)
    #: Tables present in both databases for which every check was compatible.
    compatible_tables: list[str] = field(default_factory=list)

    @property
    def findings_by_level(self) -> dict[CompatibilityLevel, list[Finding]]:
        grouped: dict[CompatibilityLevel, list[Finding]] = {
            level: [] for level in CompatibilityLevel
        }
        for finding in self.findings:
            grouped[finding.level].append(finding)
        return grouped

    @property
    def conversion_required(self) -> list[Finding]:
        return self.findings_by_level[CompatibilityLevel.CONVERSION_REQUIRED]

    @property
    def incompatible(self) -> list[Finding]:
        return self.findings_by_level[CompatibilityLevel.INCOMPATIBLE]

    @property
    def unknown(self) -> list[Finding]:
        return self.findings_by_level[CompatibilityLevel.UNKNOWN]

    @property
    def verdict(self) -> CompatibilityLevel:
        severity = {
            CompatibilityLevel.COMPATIBLE: 0,
            CompatibilityLevel.CONVERSION_REQUIRED: 1,
            CompatibilityLevel.UNKNOWN: 2,
            CompatibilityLevel.INCOMPATIBLE: 3,
        }
        if not self.findings and not self.compatible_tables:
            # Nothing in common at all - the contract has nowhere to write.
            return CompatibilityLevel.INCOMPATIBLE
        worst = CompatibilityLevel.COMPATIBLE
        for finding in self.findings:
            if severity[finding.level] > severity[worst]:
                worst = finding.level
        return worst


# A path to a database file, an already-open sqlite3 connection, or a
# sqlite_utils.Database instance (accepted duck-typed via its .conn).
DatabaseLike = Union[str, pathlib.Path, "sqlite3.Connection"]


@dataclass(frozen=True)
class _Column:
    name: str
    declared_type: str
    affinity: type
    not_null: bool
    default_value: str | None
    is_pk: int
    hidden: int


@dataclass(frozen=True)
class _ForeignKey:
    columns: tuple[str, ...]
    other_table: str
    other_columns: tuple[str, ...]
    on_update: str
    on_delete: str


@dataclass(frozen=True)
class _Table:
    name: str
    columns: tuple[_Column, ...]
    is_virtual: bool
    is_strict: bool
    row_count: int
    primary_key: tuple[str, ...]
    unique_column_groups: frozenset[tuple[str, ...]]
    foreign_keys: tuple[_ForeignKey, ...]
    has_expression_index: bool
    generated_columns: frozenset[str]

    @property
    def columns_by_name(self) -> dict[str, _Column]:
        return {column.name: column for column in self.columns}


class SchemaIntrospectionError(Exception):
    """Raised when a database schema cannot be introspected read-only."""


def _quote_identifier(identifier: str) -> str:
    return '"{}"'.format(identifier.replace('"', '""'))


def _open_read_only(source: DatabaseLike) -> tuple[_sqlite3.Connection, bool]:
    """
    Open *source* read-only, or borrow an existing connection.

    Returns a ``(connection, should_close)`` pair. Connections opened here use
    the ``mode=ro`` URI so the file can never be modified, and are closed by
    the caller. Existing connections are returned untouched and not closed.
    """
    if isinstance(source, sqlite3.Connection):
        return source, False
    # A sqlite_utils Database object exposes its raw connection as .conn.
    conn = getattr(source, "conn", None)
    if isinstance(conn, sqlite3.Connection):
        return conn, False
    path = pathlib.Path(source).resolve()
    uri = path.as_uri() + "?mode=ro"
    return sqlite3.connect(uri, uri=True), True


def _table_rows(conn: _sqlite3.Connection) -> dict[str, dict[str, Any]]:
    rows: dict[str, dict[str, Any]] = {}
    for row in conn.execute(
        "SELECT name, type, sql FROM sqlite_master"
        " WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%'"
    ).fetchall():
        rows[row[0]] = {"type": row[1], "sql": row[2] or ""}
    return rows


def _introspect_columns(
    conn: _sqlite3.Connection, table: str
) -> tuple[_Column, ...]:
    # PRAGMA table_xinfo adds generated/virtual columns (hidden = 2 or 3);
    # fall back to table_info on old SQLite builds where xinfo is missing.
    try:
        raw = conn.execute(
            f"PRAGMA table_xinfo({_quote_identifier(table)})"
        ).fetchall()
    except sqlite3.OperationalError:
        raw = [
            row + (0,)
            for row in conn.execute(
                f"PRAGMA table_info({_quote_identifier(table)})"
            ).fetchall()
        ]
    columns = []
    for cid, name, declared_type, notnull, default_value, is_pk, hidden in raw:
        columns.append(
            _Column(
                name=name,
                declared_type=declared_type or "",
                affinity=column_affinity(declared_type or ""),
                not_null=bool(notnull),
                default_value=default_value,
                is_pk=is_pk,
                hidden=hidden,
            )
        )
    return tuple(columns)


def _introspect_foreign_keys(
    conn: _sqlite3.Connection, table: str
) -> tuple[_ForeignKey, ...]:
    by_id: dict[int, list[tuple[Any, ...]]] = {}
    for row in conn.execute(
        f"PRAGMA foreign_key_list({_quote_identifier(table)})"
    ).fetchall():
        id_, seq, other_table, from_, to_, on_update, on_delete, _match = row
        by_id.setdefault(id_, []).append(
            (seq, other_table, from_, to_, on_update, on_delete)
        )
    foreign_keys = []
    for id_ in sorted(by_id):
        rows = sorted(by_id[id_])
        other_table = rows[0][1]
        columns = tuple(row[2] for row in rows)
        other_columns = tuple(
            # An implicit reference (no column list) resolves to the other
            # table's primary key, which we cannot know here; keep the value
            # the pragma gave us (often None) instead of guessing.
            row[3]
            for row in rows
        )
        foreign_keys.append(
            _ForeignKey(
                columns=columns,
                other_table=other_table,
                other_columns=other_columns,
                on_update=rows[0][4],
                on_delete=rows[0][5],
            )
        )
    return tuple(foreign_keys)


def _introspect_unique_groups(
    conn: _sqlite3.Connection, table: str
) -> frozenset[tuple[str, ...]]:
    groups: set[tuple[str, ...]] = set()
    for index_row in conn.execute(
        f"PRAGMA index_list({_quote_identifier(table)})"
    ).fetchall():
        # Column layout varies across SQLite versions; resolve positionally.
        index_name = index_row[1]
        is_unique = index_row[2]
        origin = index_row[3] if len(index_row) > 3 else "c"
        if not is_unique:
            continue
        # Auto-indexes created for PRIMARY KEY constraints mirror the primary
        # key (compared separately) and are not declared UNIQUE constraints.
        if origin == "pk":
            continue
        info = conn.execute(
            f"PRAGMA index_info({_quote_identifier(index_name)})"
        ).fetchall()
        # index_info reports NULL for expression columns, marking an
        # expression-based unique index - skip it here; it is flagged
        # separately as an unverifiable structure.
        names = tuple(row[2] for row in info)
        if len(names) != len(info) or any(name is None for name in names):
            continue
        groups.add(names)
    return frozenset(groups)


def _has_expression_index(conn: _sqlite3.Connection, table: str) -> bool:
    for index_row in conn.execute(
        f"PRAGMA index_list({_quote_identifier(table)})"
    ).fetchall():
        index_name = index_row[1]
        origin = index_row[3] if len(index_row) > 3 else "c"
        if origin == "pk":
            continue
        xinfo = conn.execute(
            f"PRAGMA index_xinfo({_quote_identifier(index_name)})"
        ).fetchall()
        for row in xinfo:
            # Every xinfo row has a trailing auxiliary row with cid=-1; an
            # expression column is distinguished by being part of the index
            # key (key=1) while having no backing table column (cid=-1).
            _seqno, cid, _name, _desc, _coll, key = row
            if cid < 0 and key == 1:
                return True
    return False


def _introspect_table(
    conn: _sqlite3.Connection, name: str, metadata: dict[str, Any]
) -> _Table:
    columns = _introspect_columns(conn, name)
    primary_key = tuple(
        column.name
        for column in sorted(
            (column for column in columns if column.is_pk),
            key=lambda column: column.is_pk,
        )
    )
    generated = frozenset(
        column.name for column in columns if column.hidden in (2, 3)
    )
    sql_upper = (metadata["sql"] or "").upper()
    is_virtual = (
        metadata["type"] == "view" or "CREATE VIRTUAL TABLE" in sql_upper
    )
    is_strict = " STRICT" in sql_upper or sql_upper.endswith("STRICT")
    row_count = conn.execute(
        f"SELECT COUNT(*) FROM {_quote_identifier(name)}"
    ).fetchone()[0]
    return _Table(
        name=name,
        columns=columns,
        is_virtual=is_virtual,
        is_strict=is_strict,
        row_count=row_count,
        primary_key=primary_key,
        unique_column_groups=_introspect_unique_groups(conn, name),
        foreign_keys=_introspect_foreign_keys(conn, name),
        has_expression_index=_has_expression_index(conn, name),
        generated_columns=generated,
    )


def _introspect(conn: _sqlite3.Connection) -> dict[str, _Table]:
    tables: dict[str, _Table] = {}
    for name, metadata in _table_rows(conn).items():
        if metadata["type"] == "view":
            # Views have no independent storage or constraint surface that a
            # write contract can rely on; they are reported separately.
            continue
        try:
            tables[name] = _introspect_table(conn, name, metadata)
        except sqlite3.DatabaseError as exc:
            raise SchemaIntrospectionError(
                f"Could not introspect table {name!r}: {exc}"
            ) from exc
    return tables


def _regular_and_virtual_tables(
    conn: _sqlite3.Connection,
) -> tuple[dict[str, _Table], frozenset[str]]:
    """
    Split introspected tables into regular tables and the set of virtual
    tables together with their implementation-specific shadow tables.

    Shadow tables (for example the ``*_data`` / ``*_idx`` tables behind an
    FTS5 virtual table) are not part of a write contract and cannot be
    compared structurally, so they are reported as a single unknown finding
    for the virtual table rather than compared one by one.
    """
    all_tables = _introspect(conn)
    virtual_names = frozenset(
        name for name, table in all_tables.items() if table.is_virtual
    )
    shadow_names: set[str] = set()
    for virtual_name in virtual_names:
        prefix = virtual_name + "_"
        shadow_names.update(
            name
            for name in all_tables
            if name.startswith(prefix) and not all_tables[name].is_virtual
        )
    regular = {
        name: table
        for name, table in all_tables.items()
        if name not in virtual_names and name not in shadow_names
    }
    return regular, frozenset(virtual_names | shadow_names)


def _view_names(conn: _sqlite3.Connection) -> frozenset[str]:
    return frozenset(
        row[0]
        for row in conn.execute(
            "SELECT name FROM sqlite_master WHERE type = 'view'"
            " AND name NOT LIKE 'sqlite_%'"
        ).fetchall()
    )


def _normalize_default(value: str | None) -> str | None:
    """
    Normalize a ``PRAGMA table_info`` default value so equivalent declarations
    compare equal regardless of quoting or casing. Returns ``None`` only when
    the column genuinely has no default.
    """
    if value is None:
        return None
    text = value.strip()
    # Unwrap balanced parentheses used around literal defaults, e.g. (1).
    while len(text) >= 2 and text[0] == "(" and text[-1] == ")":
        text = text[1:-1].strip()
    if len(text) >= 2 and text[0] == "'" and text[-1] == "'":
        # SQL string literal: collapse doubled quotes inside.
        return "str:" + text[1:-1].replace("''", "'")
    if len(text) >= 2 and text[0] == '"' and text[-1] == '"':
        # SQLite accepts a double-quoted string literal when no identifier of
        # that name exists; PRAGMA reports the default with its double quotes.
        return "str:" + text[1:-1].replace('""', '"')
    upper = text.upper()
    if upper in (
        "NULL",
        "TRUE",
        "FALSE",
        "CURRENT_TIME",
        "CURRENT_DATE",
        "CURRENT_TIMESTAMP",
    ):
        return "kw:" + upper
    try:
        number = float(text)
    except ValueError:
        # Parenthesized or otherwise unparseable expression: keep a trimmed
        # textual form rather than pretending it equals something else.
        return "expr:" + text
    return "num:" + repr(number)


def _finding(
    level: CompatibilityLevel,
    object_type: ObjectType,
    table: str,
    subject: str,
    reason: str,
    detail: dict[str, Any] | None = None,
) -> Finding:
    object_name = table if not subject else f"{table}.{subject}"
    return Finding(
        level=level,
        object_type=object_type,
        object_name=object_name,
        reason=reason,
        detail=detail or {},
    )


def _compare_column(
    table: str, contract: _Column, target: _Column
) -> list[Finding]:
    findings: list[Finding] = []
    if contract.affinity is not target.affinity:
        findings.append(
            _finding(
                CompatibilityLevel.CONVERSION_REQUIRED,
                ObjectType.COLUMN,
                table,
                contract.name,
                (
                    "Column affinity differs: contract declares "
                    f"{contract.declared_type!r} "
                    f"({_affinity_name(contract.affinity)}) but target "
                    f"declares {target.declared_type!r} "
                    f"({_affinity_name(target.affinity)}); values may be "
                    "coerced on write, so an explicit type conversion is "
                    "required."
                ),
                detail={
                    "contract_declared_type": contract.declared_type,
                    "target_declared_type": target.declared_type,
                    "contract_affinity": _affinity_name(contract.affinity),
                    "target_affinity": _affinity_name(target.affinity),
                },
            )
        )
    if contract.not_null and not target.not_null:
        findings.append(
            _finding(
                CompatibilityLevel.CONVERSION_REQUIRED,
                ObjectType.COLUMN,
                table,
                contract.name,
                "Contract requires NOT NULL but the target column is "
                "nullable; the constraint must be added (and existing NULLs "
                "resolved) before the contract holds.",
                detail={
                    "contract_not_null": contract.not_null,
                    "target_not_null": target.not_null,
                },
            )
        )
    contract_default = _normalize_default(contract.default_value)
    target_default = _normalize_default(target.default_value)
    if contract_default != target_default:
        findings.append(
            _finding(
                CompatibilityLevel.CONVERSION_REQUIRED,
                ObjectType.COLUMN,
                table,
                contract.name,
                (
                    "Column default differs: contract default is "
                    f"{contract.default_value!r} but target default is "
                    f"{target.default_value!r}."
                ),
                detail={
                    "contract_default": contract.default_value,
                    "target_default": target.default_value,
                },
            )
        )
    return findings


def _affinity_name(affinity: type) -> str:
    return getattr(affinity, "__name__", str(affinity)).upper()


def _compare_missing_contract_column(
    table: str, column: _Column, target_table: _Table
) -> Finding:
    # A column the writer expects does not exist in the target yet.
    if column.not_null and column.default_value is None:
        if target_table.row_count > 0:
            level = CompatibilityLevel.INCOMPATIBLE
            reason = (
                "New NOT NULL column without a default cannot be added to a "
                f"table holding {target_table.row_count} existing row(s): "
                "there is no value to populate for them. Existing rows must "
                "be given explicit values before the column can be added."
            )
        else:
            level = CompatibilityLevel.CONVERSION_REQUIRED
            reason = (
                "New NOT NULL column without a default can be added to the "
                "empty target table with an explicit schema conversion."
            )
    elif column.not_null:
        level = CompatibilityLevel.CONVERSION_REQUIRED
        reason = (
            "New NOT NULL column has a default "
            f"({column.default_value!r}), so it can be added with an "
            "explicit schema conversion and existing rows will be populated "
            "automatically."
        )
    else:
        level = CompatibilityLevel.CONVERSION_REQUIRED
        reason = (
            "New nullable column can be added to the target with an explicit "
            "schema conversion."
        )
    return _finding(
        level,
        ObjectType.COLUMN,
        table,
        column.name,
        reason,
        detail={
            "not_null": column.not_null,
            "has_default": column.default_value is not None,
            "default_value": column.default_value,
            "target_row_count": target_table.row_count,
        },
    )


def _compare_extra_target_column(
    table: str, column: _Column
) -> Finding | None:
    # A target column the writer knows nothing about. It is harmless when
    # nullable or defaulted; only a mandatory column with no default can break
    # inserts that omit it.
    if column.not_null and column.default_value is None and not column.is_pk:
        return _finding(
            CompatibilityLevel.INCOMPATIBLE,
            ObjectType.COLUMN,
            table,
            column.name,
            "Target has an extra NOT NULL column without a default that the "
            "contract never writes; inserts that follow the contract cannot "
            "supply a value for it. A default, nullable target or an updated "
            "write contract is required.",
            detail={
                "not_null": column.not_null,
                "has_default": column.default_value is None,
            },
        )
    return None


def _compare_primary_key(table: str, contract: _Table, target: _Table) -> Finding | None:
    if contract.primary_key != target.primary_key:
        return _finding(
            CompatibilityLevel.CONVERSION_REQUIRED,
            ObjectType.PRIMARY_KEY,
            table,
            ",".join(contract.primary_key) or "(rowid)",
            (
                "Primary key definition or order differs: contract is "
                f"({', '.join(contract.primary_key) or 'rowid'}) but target "
                f"is ({', '.join(target.primary_key) or 'rowid'}). Row "
                "identity and upsert semantics would change, so the table "
                "must be rebuilt."
            ),
            detail={
                "contract_primary_key": list(contract.primary_key),
                "target_primary_key": list(target.primary_key),
            },
        )
    return None


def _compare_unique_constraints(
    table: str, contract: _Table, target: _Table
) -> list[Finding]:
    findings: list[Finding] = []
    for group in sorted(contract.unique_column_groups - target.unique_column_groups):
        findings.append(
            _finding(
                CompatibilityLevel.CONVERSION_REQUIRED,
                ObjectType.UNIQUE_CONSTRAINT,
                table,
                ",".join(group),
                f"UNIQUE ({', '.join(group)}) required by the contract is "
                "missing from the target and must be added after checking "
                "existing data for duplicates.",
                detail={"columns": list(group), "direction": "missing_in_target"},
            )
        )
    for group in sorted(target.unique_column_groups - contract.unique_column_groups):
        findings.append(
            _finding(
                CompatibilityLevel.CONVERSION_REQUIRED,
                ObjectType.UNIQUE_CONSTRAINT,
                table,
                ",".join(group),
                f"Target enforces UNIQUE ({', '.join(group)}) which the "
                "contract does not guarantee; contract-shaped writes may "
                "fail on duplicates, so the constraint must be reconciled.",
                detail={"columns": list(group), "direction": "extra_in_target"},
            )
        )
    return findings


def _fk_key(foreign_key: _ForeignKey) -> tuple[Any, ...]:
    return (
        foreign_key.columns,
        foreign_key.other_table,
        foreign_key.other_columns,
        foreign_key.on_update,
        foreign_key.on_delete,
    )


def _compare_foreign_keys(
    table: str,
    contract: _Table,
    target: _Table,
    target_tables: dict[str, _Table],
) -> list[Finding]:
    findings: list[Finding] = []
    target_by_columns = {fk.columns: fk for fk in target.foreign_keys}
    for contract_fk in contract.foreign_keys:
        label = ", ".join(contract_fk.columns)
        target_fk = target_by_columns.get(contract_fk.columns)
        if target_fk is None:
            referenced = target_tables.get(contract_fk.other_table)
            if referenced is None:
                level = CompatibilityLevel.INCOMPATIBLE
                reason = (
                    f"Foreign key ({label}) references table "
                    f"{contract_fk.other_table!r}, which does not exist in "
                    "the target; the reference cannot be created."
                )
            elif any(
                column is not None and column not in referenced.columns_by_name
                for column in contract_fk.other_columns
            ):
                level = CompatibilityLevel.INCOMPATIBLE
                reason = (
                    f"Foreign key ({label}) references a column missing from "
                    f"{contract_fk.other_table!r} in the target; the "
                    "reference cannot be satisfied."
                )
            else:
                level = CompatibilityLevel.CONVERSION_REQUIRED
                reason = (
                    f"Foreign key ({label}) -> "
                    f"{contract_fk.other_table}("
                    f"{', '.join(c or '' for c in contract_fk.other_columns)}) "
                    "is missing from the target and can be added with an "
                    "explicit schema conversion."
                )
            findings.append(
                _finding(
                    level,
                    ObjectType.FOREIGN_KEY,
                    table,
                    label,
                    reason,
                    detail={"direction": "missing_in_target"},
                )
            )
            continue
        if _fk_key(contract_fk) != _fk_key(target_fk):
            findings.append(
                _finding(
                    CompatibilityLevel.CONVERSION_REQUIRED,
                    ObjectType.FOREIGN_KEY,
                    table,
                    label,
                    (
                        "Foreign key definition differs: contract references "
                        f"{contract_fk.other_table}("
                        f"{', '.join(c or '' for c in contract_fk.other_columns)}) "
                        f"with ON UPDATE {contract_fk.on_update} / ON DELETE "
                        f"{contract_fk.on_delete}, but target references "
                        f"{target_fk.other_table}("
                        f"{', '.join(c or '' for c in target_fk.other_columns)}) "
                        f"with ON UPDATE {target_fk.on_update} / ON DELETE "
                        f"{target_fk.on_delete}."
                    ),
                    detail={
                        "contract": _fk_detail(contract_fk),
                        "target": _fk_detail(target_fk),
                    },
                )
            )
    contract_by_columns = {fk.columns: fk for fk in contract.foreign_keys}
    for target_fk in target.foreign_keys:
        if target_fk.columns not in contract_by_columns:
            findings.append(
                _finding(
                    CompatibilityLevel.CONVERSION_REQUIRED,
                    ObjectType.FOREIGN_KEY,
                    table,
                    ", ".join(target_fk.columns),
                    (
                        "Target adds a foreign key "
                        f"({', '.join(target_fk.columns)}) -> "
                        f"{target_fk.other_table} that the contract does not "
                        "declare; writes may be rejected by referential "
                        "integrity, so the constraint must be reconciled."
                    ),
                    detail={"direction": "extra_in_target"},
                )
            )
    return findings


def _fk_detail(foreign_key: _ForeignKey) -> dict[str, Any]:
    return {
        "columns": list(foreign_key.columns),
        "other_table": foreign_key.other_table,
        "other_columns": list(foreign_key.other_columns),
        "on_update": foreign_key.on_update,
        "on_delete": foreign_key.on_delete,
    }


def _compare_table(
    contract: _Table,
    target: _Table,
    target_tables: dict[str, _Table],
) -> tuple[list[Finding], bool]:
    """
    Compare one table present on both sides.

    Returns the findings plus a flag indicating whether the table was fully
    compatible (no finding at all).
    """
    findings: list[Finding] = []
    table = contract.name

    if contract.is_virtual or target.is_virtual:
        findings.append(
            _finding(
                CompatibilityLevel.UNKNOWN,
                ObjectType.TABLE,
                table,
                "",
                "A virtual table is involved; its storage semantics are "
                "provided by an extension and cannot be verified from "
                "PRAGMA introspection. Compatibility is unknown.",
                detail={
                    "contract_virtual": contract.is_virtual,
                    "target_virtual": target.is_virtual,
                },
            )
        )
        return findings, False

    if contract.has_expression_index or target.has_expression_index:
        findings.append(
            _finding(
                CompatibilityLevel.UNKNOWN,
                ObjectType.INDEX,
                table,
                "",
                "An expression index is present; its indexed expressions "
                "cannot be compared structurally without evaluating them, so "
                "index compatibility is unknown.",
                detail={
                    "contract_has_expression_index": contract.has_expression_index,
                    "target_has_expression_index": target.has_expression_index,
                },
            )
        )

    contract_columns = contract.columns_by_name
    target_columns = target.columns_by_name

    # Columns the writer expects, compared one-to-one or reported as missing.
    for name, contract_column in contract_columns.items():
        target_column = target_columns.get(name)
        if target_column is None:
            if name in target.generated_columns:
                findings.append(
                    _finding(
                        CompatibilityLevel.UNKNOWN,
                        ObjectType.COLUMN,
                        table,
                        name,
                        "The target column is generated; whether a write "
                        "contract may assign to it depends on its generation "
                        "rule and could not be verified. Compatibility is "
                        "unknown.",
                        detail={"generated": True},
                    )
                )
            else:
                findings.append(
                    _compare_missing_contract_column(
                        table, contract_column, target
                    )
                )
            continue
        if name in contract.generated_columns or name in target.generated_columns:
            findings.append(
                _finding(
                    CompatibilityLevel.UNKNOWN,
                    ObjectType.COLUMN,
                    table,
                    name,
                    "A generated column is involved on one side; its "
                    "generation expression cannot be compared via "
                    "PRAGMA table_info, so column compatibility is unknown.",
                    detail={
                        "contract_generated": name in contract.generated_columns,
                        "target_generated": name in target.generated_columns,
                    },
                )
            )
            continue
        findings.extend(_compare_column(table, contract_column, target_column))

    # Columns that only exist in the target (the writer never names them).
    for name, target_column in target_columns.items():
        if name in contract_columns or name in target.generated_columns:
            continue
        extra = _compare_extra_target_column(table, target_column)
        if extra is not None:
            findings.append(extra)

    pk_finding = _compare_primary_key(table, contract, target)
    if pk_finding is not None:
        findings.append(pk_finding)

    findings.extend(_compare_unique_constraints(table, contract, target))
    findings.extend(
        _compare_foreign_keys(table, contract, target, target_tables)
    )

    return findings, not findings


def check_schema_compatibility(
    contract: DatabaseLike, target: DatabaseLike
) -> SchemaCompatibilityReport:
    """
    Check whether an ingestion write *contract* can keep writing to *target*.

    :param contract: The database (path, ``sqlite3.Connection`` or
      :class:`sqlite_utils.Database`) whose schema the writer is built against.
    :param target: The database the writer would actually write to.
    :return: A :class:`SchemaCompatibilityReport` whose findings are grouped
      into compatible, conversion-required, incompatible and unknown results.

    Both databases are opened with the ``mode=ro`` URI when paths are given,
    so this function never creates or modifies a database file. Existing
    connections are used as-is and remain owned by the caller.
    """
    contract_conn, close_contract = _open_read_only(contract)
    target_conn, close_target = _open_read_only(target)
    try:
        contract_tables, contract_virtual = _regular_and_virtual_tables(
            contract_conn
        )
        target_tables, target_virtual = _regular_and_virtual_tables(target_conn)
        contract_views = _view_names(contract_conn)
        target_views = _view_names(target_conn)

        report = SchemaCompatibilityReport()

        for table_name in sorted(contract_views - target_views):
            report.findings.append(
                _finding(
                    CompatibilityLevel.CONVERSION_REQUIRED,
                    ObjectType.VIEW,
                    table_name,
                    "",
                    "View required by the contract does not exist in the "
                    "target and must be created.",
                )
            )

        # Virtual tables and their shadow tables cannot be verified via
        # PRAGMA structure alone and are never guessed to be safe.
        for table_name in sorted(contract_virtual | target_virtual):
            report.findings.append(
                _finding(
                    CompatibilityLevel.UNKNOWN,
                    ObjectType.TABLE,
                    table_name,
                    "",
                    "Virtual table or virtual-table shadow storage is "
                    "present; its storage semantics are provided by an "
                    "extension and cannot be verified from PRAGMA "
                    "introspection. Compatibility is unknown.",
                    detail={
                        "in_contract": table_name in contract_virtual,
                        "in_target": table_name in target_virtual,
                    },
                )
            )

        for table_name in sorted(contract_tables):
            target_table = target_tables.get(table_name)
            if target_table is None:
                report.findings.append(
                    _finding(
                        CompatibilityLevel.CONVERSION_REQUIRED,
                        ObjectType.TABLE,
                        table_name,
                        "",
                        "Table required by the contract does not exist in "
                        "the target and must be created before writes can "
                        "proceed.",
                    )
                )
                continue
            table_findings, fully_compatible = _compare_table(
                contract_tables[table_name], target_table, target_tables
            )
            report.findings.extend(table_findings)
            if fully_compatible:
                report.compatible_tables.append(table_name)

        return report
    finally:
        if close_contract:
            contract_conn.close()
        if close_target:
            target_conn.close()
