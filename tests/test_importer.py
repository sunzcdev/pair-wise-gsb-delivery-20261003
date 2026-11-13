import contextlib

import contextlib
import pathlib

import pytest

import sqlite_utils
from sqlite_utils import Database
from sqlite_utils.importer import (
    BATCHES_TABLE,
    CURSORS_TABLE,
    BatchConflictError,
    CursorMismatchError,
)
from sqlite_utils.utils import sqlite3


@pytest.fixture
def db_file_path():
    temp_dir = pathlib.Path(__file__).parent / "_importer_tmp"
    temp_dir.mkdir(exist_ok=True)
    path = temp_dir / "imports.db"
    for stale in temp_dir.glob("imports.db*"):
        stale.unlink(missing_ok=True)
    yield path
    for child in temp_dir.glob("imports.db*"):
        with contextlib.suppress(PermissionError):
            child.unlink(missing_ok=True)
    with contextlib.suppress(OSError):
        temp_dir.rmdir()


def _records(n):
    return [{"id": i, "name": f"item-{i}"} for i in range(1, n + 1)]


def test_import_writes_data_cursor_and_ledger(fresh_db):
    result = fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="batch-1",
        expected_old_cursor=None,
        new_cursor="opaque:cursor:001",
        records=_records(3),
        pk="id",
    )

    assert result.replayed is False
    assert result.rows_written == 3
    assert result.new_cursor == "opaque:cursor:001"
    assert result.old_cursor is None
    assert fresh_db.read_import_cursor("api") == "opaque:cursor:001"
    assert list(fresh_db.table("items").rows) == _records(3)
    ledger = fresh_db.table(BATCHES_TABLE).get(("api", "batch-1"))
    assert ledger["new_cursor"] == "opaque:cursor:001"
    assert ledger["digest"] == result.digest
    assert ledger["rows_written"] == 3


def test_cursor_advances_using_expected_old_cursor(fresh_db):
    fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="batch-1",
        expected_old_cursor=None,
        new_cursor="cursor-a",
        records=_records(2),
        pk="id",
    )
    result = fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="batch-2",
        expected_old_cursor="cursor-a",
        new_cursor="cursor-b",
        records=[{"id": 3, "name": "item-3"}, {"id": 5, "name": "item-5"}],
        pk="id",
    )
    assert result.replayed is False
    assert fresh_db.read_import_cursor("api") == "cursor-b"


def test_stale_expected_cursor_is_rejected(fresh_db):
    fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="batch-1",
        expected_old_cursor=None,
        new_cursor="cursor-a",
        records=_records(1),
        pk="id",
    )
    with pytest.raises(CursorMismatchError) as excinfo:
        fresh_db.import_batch(
            source="api",
            table="items",
            batch_id="batch-2",
            expected_old_cursor="stale-cursor",
            new_cursor="cursor-b",
            records=_records(1),
            pk="id",
        )
    err = excinfo.value
    assert err.expected_cursor == "stale-cursor"
    assert err.actual_cursor == "cursor-a"
    # Nothing was written and the cursor did not move
    assert fresh_db.read_import_cursor("api") == "cursor-a"
    assert fresh_db.read_imported_batch("api", "batch-2") is None
    assert list(fresh_db.table("items").rows) == _records(1)


def test_first_import_requires_expected_old_cursor_none(fresh_db):
    with pytest.raises(CursorMismatchError):
        fresh_db.import_batch(
            source="api",
            table="items",
            batch_id="batch-1",
            expected_old_cursor="something",
            new_cursor="cursor-a",
            records=_records(1),
            pk="id",
        )
    # Whole batch rolled back: bookkeeping tables were never committed either
    assert CURSORS_TABLE not in fresh_db.table_names()
    assert BATCHES_TABLE not in fresh_db.table_names()
    assert fresh_db.read_import_cursor("api") is None


def test_retry_same_batch_id_and_digest_replays_result(fresh_db):
    first = fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="batch-1",
        expected_old_cursor=None,
        new_cursor="cursor-a",
        records=_records(2),
        pk="id",
    )
    # Retry even carries a (now wrong) expected cursor: replay wins
    second = fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="batch-1",
        expected_old_cursor=None,
        new_cursor="cursor-a",
        records=_records(2),
        pk="id",
    )
    assert second.replayed is True
    assert second.digest == first.digest
    assert second.rows_written == first.rows_written
    assert second.new_cursor == first.new_cursor
    # No duplicate rows
    assert fresh_db.table("items").count == 2


def test_retry_after_cursor_advanced_still_replays(fresh_db):
    # Simulates a crash after commit but before the caller recorded success:
    # the retry's expected cursor has moved on, yet the batch ledger wins.
    fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="batch-1",
        expected_old_cursor=None,
        new_cursor="cursor-a",
        records=_records(2),
        pk="id",
    )
    fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="batch-2",
        expected_old_cursor="cursor-a",
        new_cursor="cursor-b",
        records=[{"id": 3, "name": "item-3"}],
        pk="id",
    )
    replayed = fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="batch-1",
        expected_old_cursor="cursor-b",
        new_cursor="cursor-a",
        records=_records(2),
        pk="id",
    )
    assert replayed.replayed is True
    assert replayed.new_cursor == "cursor-a"
    assert fresh_db.read_import_cursor("api") == "cursor-b"
    assert fresh_db.table("items").count == 3


def test_same_batch_id_different_digest_conflicts(fresh_db):
    fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="batch-1",
        expected_old_cursor=None,
        new_cursor="cursor-a",
        records=_records(2),
        pk="id",
    )
    with pytest.raises(BatchConflictError) as excinfo:
        fresh_db.import_batch(
            source="api",
            table="items",
            batch_id="batch-1",
            expected_old_cursor="cursor-a",
            new_cursor="cursor-b",
            records=[{"id": 99, "name": "different"}],
            pk="id",
        )
    assert excinfo.value.stored_digest != excinfo.value.digest
    # State stays at the original batch
    assert fresh_db.read_import_cursor("api") == "cursor-a"
    assert fresh_db.table("items").count == 2


def test_iterator_error_does_not_advance_cursor(fresh_db):
    def failing_iter():
        yield {"id": 1, "name": "ok"}
        raise RuntimeError("source exploded")

    with pytest.raises(RuntimeError):
        fresh_db.import_batch(
            source="api",
            table="items",
            batch_id="batch-1",
            expected_old_cursor=None,
            new_cursor="cursor-a",
            records=failing_iter(),
            pk="id",
        )
    assert fresh_db.read_import_cursor("api") is None
    assert "items" not in fresh_db.table_names()
    assert fresh_db.read_imported_batch("api", "batch-1") is None


def test_constraint_failure_rolls_back_cursor_and_data(fresh_db):
    fresh_db.execute(
        "CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT NOT NULL)"
    )
    bad_records = [
        {"id": 1, "name": "ok"},
        {"id": 2, "name": None},  # NOT NULL violation in the back half
    ]
    with pytest.raises(sqlite3.IntegrityError):
        fresh_db.import_batch(
            source="api",
            table="items",
            batch_id="batch-1",
            expected_old_cursor=None,
            new_cursor="cursor-a",
            records=bad_records,
        )
    assert fresh_db.read_import_cursor("api") is None
    assert fresh_db.table("items").count == 0
    assert fresh_db.read_imported_batch("api", "batch-1") is None


def test_outer_transaction_rollback_undoes_everything(fresh_db):
    fresh_db.begin()
    with fresh_db.atomic():
        fresh_db.import_batch(
            source="api",
            table="items",
            batch_id="batch-1",
            expected_old_cursor=None,
            new_cursor="cursor-a",
            records=_records(2),
            pk="id",
        )
        assert fresh_db.conn.in_transaction
    fresh_db.rollback()

    assert fresh_db.read_import_cursor("api") is None
    assert "items" not in fresh_db.table_names()
    assert BATCHES_TABLE not in fresh_db.table_names()
    assert fresh_db.read_imported_batch("api", "batch-1") is None


def test_does_not_commit_callers_transaction(fresh_db):
    fresh_db.execute("CREATE TABLE side (x TEXT)")
    fresh_db.begin()
    fresh_db.execute("INSERT INTO side VALUES ('uncommitted')")
    with fresh_db.atomic():
        fresh_db.import_batch(
            source="api",
            table="items",
            batch_id="batch-1",
            expected_old_cursor=None,
            new_cursor="cursor-a",
            records=_records(1),
            pk="id",
        )
        # Still inside the caller's single transaction
        assert fresh_db.conn.in_transaction
    fresh_db.rollback()

    assert fresh_db.table("side").count == 0
    assert "items" not in fresh_db.table_names()
    assert fresh_db.read_import_cursor("api") is None


def test_cursor_is_opaque_string_no_ordering_assumed(fresh_db):
    # Values that would sort/numeric-compare oddly are only equality checked
    cursors = ["zzz", "007", "999999999999999999999999", "水/2026/11"]
    previous = None
    for index, cursor in enumerate(cursors):
        result = fresh_db.import_batch(
            source="api",
            table="items",
            batch_id=f"batch-{index}",
            expected_old_cursor=previous,
            new_cursor=cursor,
            records=[{"id": index, "name": cursor}],
            pk="id",
        )
        assert result.new_cursor == cursor
        previous = cursor
    assert fresh_db.read_import_cursor("api") == cursors[-1]


def test_sources_are_independent(fresh_db):
    fresh_db.import_batch(
        source="source-a",
        table="items",
        batch_id="b",
        expected_old_cursor=None,
        new_cursor="a-cursor",
        records=[{"id": 1}],
        pk="id",
    )
    # Same batch id against a different source is a fresh import
    result = fresh_db.import_batch(
        source="source-b",
        table="items",
        batch_id="b",
        expected_old_cursor=None,
        new_cursor="b-cursor",
        records=[{"id": 2}],
        pk="id",
    )
    assert result.replayed is False
    assert fresh_db.read_import_cursor("source-a") == "a-cursor"
    assert fresh_db.read_import_cursor("source-b") == "b-cursor"


def test_digest_ignores_key_order(fresh_db):
    first = fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="b",
        expected_old_cursor=None,
        new_cursor="c",
        records=[{"id": 1, "name": "x"}],
        pk="id",
    )
    replayed = fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="b",
        expected_old_cursor=None,
        new_cursor="c",
        records=[{"name": "x", "id": 1}],
        pk="id",
    )
    assert replayed.replayed is True
    assert replayed.digest == first.digest


def test_empty_batch_advances_cursor_without_creating_table(fresh_db):
    result = fresh_db.import_batch(
        source="api",
        table="items",
        batch_id="b",
        expected_old_cursor=None,
        new_cursor="c",
        records=[],
    )
    assert result.rows_written == 0
    assert "items" not in fresh_db.table_names()
    assert fresh_db.read_import_cursor("api") == "c"


def test_persists_across_reopen(db_file_path):
    path = db_file_path
    db = Database(path)
    db.import_batch(
        source="api",
        table="items",
        batch_id="batch-1",
        expected_old_cursor=None,
        new_cursor="cursor-a",
        records=_records(3),
        pk="id",
    )
    db.close()

    reopened = Database(path)
    assert reopened.read_import_cursor("api") == "cursor-a"
    assert list(reopened.table("items").rows) == _records(3)
    replayed = reopened.import_batch(
        source="api",
        table="items",
        batch_id="batch-1",
        expected_old_cursor="cursor-a",
        new_cursor="cursor-a",
        records=_records(3),
        pk="id",
    )
    assert replayed.replayed is True
    assert reopened.table("items").count == 3
    # Next batch resumes exactly from the stored checkpoint
    reopened.import_batch(
        source="api",
        table="items",
        batch_id="batch-2",
        expected_old_cursor="cursor-a",
        new_cursor="cursor-b",
        records=[{"id": 4, "name": "item-4"}],
        pk="id",
    )
    assert reopened.read_import_cursor("api") == "cursor-b"
    assert reopened.table("items").count == 4
    reopened.close()


def test_invalid_arguments_raise_type_or_value_errors(fresh_db):
    kwargs = dict(
        source="api",
        table="items",
        batch_id="b",
        expected_old_cursor=None,
        new_cursor="c",
        records=[],
    )
    for key, bad_value in (
        ("source", ""),
        ("table", ""),
        ("batch_id", ""),
        ("new_cursor", ""),
    ):
        with pytest.raises(ValueError):
            fresh_db.import_batch(**{**kwargs, key: bad_value})
    with pytest.raises(TypeError):
        fresh_db.import_batch(**{**kwargs, "expected_old_cursor": 123})


def test_exceptions_share_base_class():
    assert issubclass(CursorMismatchError, sqlite_utils.BatchImportError)
    assert issubclass(BatchConflictError, sqlite_utils.BatchImportError)
