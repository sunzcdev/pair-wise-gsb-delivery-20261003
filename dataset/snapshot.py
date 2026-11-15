import logging
from collections.abc import Iterable, Iterator, Mapping, Sequence
from typing import TYPE_CHECKING, Any

from sqlalchemy import and_, or_, select

from dataset.types import ColumnType
from dataset.util import (
    DatasetError,
    MutableRow,
    SQLWriteValue,
    WriteRow,
    ensure_strings,
)

if TYPE_CHECKING:
    from dataset.table import Table

log = logging.getLogger(__name__)

# Number of stale rows removed per DELETE statement.
DELETE_CHUNK = 500


class SnapshotError(DatasetError):
    """Raised when a snapshot sync fails key validation."""


def _chunks(items: Sequence[Any], size: int) -> Iterator[Sequence[Any]]:
    for idx in range(0, len(items), size):
        yield items[idx : idx + size]


class SnapshotSync:
    """Stage a full snapshot of rows for a scope and swap it in atomically.

    Rows are loaded for a *scope* (a set of filter values identifying the
    data source, e.g. ``{"source": "shop-a"}``) and validated against a set
    of business ``keys``. Only when the input has ended normally and no
    duplicate or missing keys were found is the visible state for the scope
    switched: existing rows are updated in place (keeping their primary
    key), new rows are inserted, and rows of the scope that are absent from
    the snapshot are deleted. If the source fails midway or key validation
    fails, the transaction is rolled back and the previous snapshot remains
    fully visible. Scopes other than the given one are never touched.
    """

    def __init__(
        self,
        table: "Table",
        keys: str | Iterable[str],
        scope: Mapping[str, SQLWriteValue] | None = None,
        chunk_size: int = 1000,
        ensure: bool | None = None,
        types: dict[str, ColumnType] | None = None,
    ) -> None:
        self.table = table
        self.keys = ensure_strings(keys)
        if not len(self.keys):
            raise SnapshotError("Snapshot sync requires at least one key column.")
        self.scope: dict[str, SQLWriteValue] = dict(scope or {})
        self.chunk_size = chunk_size
        self.ensure = ensure
        self.types = types
        self.count = 0
        self._seen: set[tuple[Any, ...]] = set()
        self._queue: list[MutableRow] = []

    @property
    def _upsert_keys(self) -> list[str]:
        """Business keys prefixed by the scope columns."""
        return list(self.scope.keys()) + self.keys

    def add(self, row: WriteRow) -> None:
        """Stage a single row of the snapshot.

        Raises :py:class:`SnapshotError` if a business key column is missing
        from the row, or if the key combination was already seen in this
        snapshot.
        """
        key_tuple = []
        for column in self.keys:
            if column not in row:
                raise SnapshotError(f"Row is missing key column: {column}")
            key_tuple.append(row[column])
        key = tuple(key_tuple)
        if key in self._seen:
            raise SnapshotError(f"Duplicate key in snapshot: {key!r}")
        self._seen.add(key)
        staged = dict(row)
        for column, value in self.scope.items():
            staged[column] = value
        self._queue.append(staged)
        self.count += 1
        if len(self._queue) >= self.chunk_size:
            self.flush()

    def add_many(self, rows: Iterable[WriteRow]) -> None:
        """Stage rows from an iterable, flushing every ``chunk_size`` rows."""
        for row in rows:
            self.add(row)

    def flush(self) -> None:
        """Write the currently staged chunk of rows to the table."""
        if not len(self._queue):
            return
        self.table.upsert_many(
            self._queue,
            self._upsert_keys,
            chunk_size=self.chunk_size,
            ensure=self.ensure,
            types=self.types,
        )
        self._queue.clear()

    def _delete_stale(self) -> None:
        """Remove rows of the scope that are not part of the snapshot."""
        table = self.table
        if not table.exists:
            return
        key_columns = [table._get_column_name(k) for k in self.keys]
        scope_args = {table._get_column_name(k): v for k, v in self.scope.items()}
        clause = table._args_to_clause(scope_args)

        pk_columns = [c.name for c in table.table.primary_key.columns]
        pk = pk_columns[0] if len(pk_columns) == 1 else None
        select_columns = [table.table.c[k] for k in key_columns]
        if pk is not None and pk not in key_columns:
            select_columns.append(table.table.c[pk])
        rp = self.table.db.executable.execute(select(*select_columns).where(clause))

        stale_ids: list[Any] = []
        stale_keys: list[tuple[Any, ...]] = []
        for row in rp:
            mapping = row._mapping
            key = tuple(mapping[k] for k in key_columns)
            if key in self._seen:
                continue
            if pk is not None:
                stale_ids.append(mapping[pk])
            else:
                stale_keys.append(key)

        if pk is not None:
            for chunk in _chunks(stale_ids, DELETE_CHUNK):
                stmt = (
                    table.table.delete()
                    .where(clause)
                    .where(table.table.c[pk].in_(chunk))
                )
                table.db.executable.execute(stmt)
        else:
            for chunk in _chunks(stale_keys, DELETE_CHUNK):
                matches = [
                    and_(
                        *[
                            table.table.c[k] == v
                            for k, v in zip(key_columns, key, strict=True)
                        ]
                    )
                    for key in chunk
                ]
                stmt = table.table.delete().where(clause).where(or_(*matches))
                table.db.executable.execute(stmt)
        if len(stale_ids) or len(stale_keys):
            log.debug(
                "Snapshot sync removed %d stale rows from %r",
                len(stale_ids) + len(stale_keys),
                table.name,
            )

    def __enter__(self) -> "SnapshotSync":
        self.table.db.begin()
        return self

    def __exit__(self, exc_type: object, exc_val: object, exc_tb: object) -> None:
        db = self.table.db
        if exc_type is not None:
            # The source failed or key validation raised: keep the old
            # snapshot visible by discarding all staged changes.
            db.rollback()
            return
        try:
            self.flush()
            self._delete_stale()
            db.commit()
        except Exception:
            db.rollback()
            raise
