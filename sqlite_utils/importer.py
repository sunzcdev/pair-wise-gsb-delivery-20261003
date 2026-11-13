from __future__ import annotations

import hashlib
import json
from collections.abc import Iterable
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from .db import Database


CURSORS_TABLE = "_sqlite_utils_import_cursors"
BATCHES_TABLE = "_sqlite_utils_import_batches"


class BatchImportError(Exception):
    """Base class for errors raised by the batch importer."""


class CursorMismatchError(BatchImportError):
    """Raised when the stored cursor does not match the expected old cursor."""

    def __init__(self, source: str, expected: str | None, actual: str | None):
        self.source = source
        self.expected_cursor = expected
        self.actual_cursor = actual
        super().__init__(
            "Cursor mismatch for source {!r}: expected {!r}, database has {!r}".format(
                source, expected, actual
            )
        )


class BatchConflictError(BatchImportError):
    """Raised when a batch id is reused with a different content digest."""

    def __init__(self, source: str, batch_id: str, stored_digest: str, digest: str):
        self.source = source
        self.batch_id = batch_id
        self.stored_digest = stored_digest
        self.digest = digest
        super().__init__(
            "Batch {!r} for source {!r} was already imported with a different "
            "content (stored digest {}, incoming digest {}".format(
                batch_id, source, stored_digest, digest
            )
        )


@dataclass(frozen=True)
class BatchImportResult:
    """Outcome of a batch import, whether newly written or replayed."""

    source: str
    batch_id: str
    table: str
    old_cursor: str | None
    new_cursor: str
    digest: str
    rows_written: int
    replayed: bool


def _records_digest(records: list[dict[str, Any]]) -> str:
    # Canonical JSON so key order does not affect the digest; default=str keeps
    # otherwise non-serialisable values (datetime, bytes, ...) deterministic.
    payload = json.dumps(
        records,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        default=str,
    )
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def _ensure_importer_tables(db: Database) -> None:
    db.execute(
        f"""
        CREATE TABLE IF NOT EXISTS {CURSORS_TABLE} (
            source TEXT PRIMARY KEY,
            cursor TEXT NOT NULL,
            updated_at TEXT NOT NULL DEFAULT (datetime('now'))
        )
        """
    )
    db.execute(
        f"""
        CREATE TABLE IF NOT EXISTS {BATCHES_TABLE} (
            source TEXT NOT NULL,
            batch_id TEXT NOT NULL,
            digest TEXT NOT NULL,
            old_cursor TEXT,
            new_cursor TEXT NOT NULL,
            "table" TEXT NOT NULL,
            rows_written INTEGER NOT NULL,
            created_at TEXT NOT NULL DEFAULT (datetime('now')),
            PRIMARY KEY (source, batch_id)
        )
        """
    )


def read_import_cursor(db: Database, source: str) -> str | None:
    """Return the persisted cursor for a source, or ``None`` if never seen."""
    if CURSORS_TABLE not in db.table_names():
        return None
    row = db.execute(
        f"SELECT cursor FROM {CURSORS_TABLE} WHERE source = ?", [source]
    ).fetchone()
    return None if row is None else row[0]


def read_imported_batch(
    db: Database, source: str, batch_id: str
) -> BatchImportResult | None:
    """Return the recorded result for a previously imported batch, if any."""
    if BATCHES_TABLE not in db.table_names():
        return None
    row = db.execute(
        f"""
        SELECT digest, old_cursor, new_cursor, "table", rows_written
        FROM {BATCHES_TABLE}
        WHERE source = ? AND batch_id = ?
        """,
        [source, batch_id],
    ).fetchone()
    if row is None:
        return None
    digest, old_cursor, new_cursor, table, rows_written = row
    return BatchImportResult(
        source=source,
        batch_id=batch_id,
        table=table,
        old_cursor=old_cursor,
        new_cursor=new_cursor,
        digest=digest,
        rows_written=rows_written,
        replayed=True,
    )


def import_batch(
    db: Database,
    *,
    source: str,
    table: str,
    batch_id: str,
    expected_old_cursor: str | None,
    new_cursor: str,
    records: Iterable[dict[str, Any]],
    **insert_kwargs: Any,
) -> BatchImportResult:
    """
    Import one batch of records and advance the source cursor atomically.

    The data writes, the cursor checkpoint and the batch ledger entry all
    happen inside a single transaction. If the connection already has an open
    transaction, a SQLite savepoint is used instead, so the caller's
    transaction is never committed or rolled back by this method.

    Cursors are treated as opaque strings and only ever compared for equality.

    :param source: identifier of the upstream source being read
    :param table: name of the table the records are written to
    :param batch_id: caller-provided idempotency key for this batch
    :param expected_old_cursor: cursor the caller believes is currently stored;
      ``None`` means the source has never been imported before
    :param new_cursor: cursor to persist once the batch is written
    :param records: iterable of record dictionaries (consumed eagerly so an
      iterator failure cannot leave partial state behind)
    :param insert_kwargs: extra keyword arguments forwarded to
      :meth:`Table.insert_all` (e.g. ``pk``, ``upsert``, ``alter``)
    :raises CursorMismatchError: the stored cursor differs from
      ``expected_old_cursor``
    :raises BatchConflictError: ``batch_id`` was already used for this source
      with different record contents
    """
    _require_non_empty_str("source", source)
    _require_non_empty_str("table", table)
    _require_non_empty_str("batch_id", batch_id)
    _require_non_empty_str("new_cursor", new_cursor)
    if expected_old_cursor is not None and not isinstance(expected_old_cursor, str):
        raise TypeError("expected_old_cursor must be a string or None")

    # Materialise before opening the transaction: an iterator raising must not
    # touch the cursor, the ledger or any data table.
    record_list = list(records)
    digest = _records_digest(record_list)

    with db.atomic():
        _ensure_importer_tables(db)

        ledger_row = db.execute(
            f"""
            SELECT digest, old_cursor, new_cursor, "table", rows_written
            FROM {BATCHES_TABLE}
            WHERE source = ? AND batch_id = ?
            """,
            [source, batch_id],
        ).fetchone()
        if ledger_row is not None:
            stored_digest = ledger_row[0]
            if stored_digest != digest:
                raise BatchConflictError(source, batch_id, stored_digest, digest)
            return BatchImportResult(
                source=source,
                batch_id=batch_id,
                table=ledger_row[3],
                old_cursor=ledger_row[1],
                new_cursor=ledger_row[2],
                digest=stored_digest,
                rows_written=ledger_row[4],
                replayed=True,
            )

        cursor_row = db.execute(
            f"SELECT cursor FROM {CURSORS_TABLE} WHERE source = ?", [source]
        ).fetchone()
        current_cursor = None if cursor_row is None else cursor_row[0]
        if current_cursor != expected_old_cursor:
            raise CursorMismatchError(source, expected_old_cursor, current_cursor)

        if record_list:
            db.table(table).insert_all(record_list, **insert_kwargs)
        rows_written = len(record_list)

        db.execute(
            f"""
            INSERT INTO {CURSORS_TABLE} (source, cursor)
            VALUES (?, ?)
            ON CONFLICT(source) DO UPDATE SET
                cursor = excluded.cursor,
                updated_at = datetime('now')
            """,
            [source, new_cursor],
        )
        db.execute(
            f"""
            INSERT INTO {BATCHES_TABLE}
                (source, batch_id, digest, old_cursor, new_cursor, "table",
                 rows_written)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            """,
            [
                source,
                batch_id,
                digest,
                expected_old_cursor,
                new_cursor,
                table,
                rows_written,
            ],
        )

    return BatchImportResult(
        source=source,
        batch_id=batch_id,
        table=table,
        old_cursor=expected_old_cursor,
        new_cursor=new_cursor,
        digest=digest,
        rows_written=rows_written,
        replayed=False,
    )


def _require_non_empty_str(name: str, value: Any) -> None:
    if not isinstance(value, str) or not value:
        raise ValueError(f"{name} must be a non-empty string")
