"""Regression tests for reflection-cache invalidation on rollback.

When a transaction is rolled back, the database may undo schema changes
(added columns, created indexes, even created/dropped tables). Existing
``Table`` objects must re-read the real database structure afterwards
instead of serving their pre-rollback caches of columns and indexes.
"""

import pytest

from dataset import connect

TABLE = "rollback_cache"


@pytest.fixture(scope="function")
def tbl(db):
    table = db[TABLE]
    table.insert({"a": 1, "b": "x"})
    return table


def _real_columns(db, name=TABLE):
    return [c["name"] for c in db.inspect.get_columns(name)]


def test_rollback_resets_all_reflection_caches(db, tbl):
    # Warm every cache the Table keeps.
    assert tbl.has_column("b")
    tbl.create_index(["b"])
    assert tbl.has_index(["b"])
    assert tbl._table is not None
    assert tbl._columns is not None
    assert len(tbl._indexes) > 0

    db.begin()
    db.rollback()

    assert tbl._table is None
    assert tbl._columns is None
    assert tbl._indexes == []
    # The same handle still works and reflects the real structure.
    assert tbl.has_column("b")
    assert tbl.has_index(["b"])


def test_rollback_resees_dropped_column(db, tbl):
    assert tbl.has_column("b")
    # The real schema changes behind the Table's back, the way a
    # transactional DDL rollback (e.g. on PostgreSQL) would revert an
    # ADD COLUMN that the Table had already cached.
    db.query(f"ALTER TABLE {TABLE} DROP COLUMN b")
    assert "b" not in _real_columns(db)

    db.begin()
    db.rollback()

    # Same handle must see the database's real columns again.
    assert not tbl.has_column("b")
    assert "b" not in tbl.columns
    assert tbl.count() == 1


def test_rollback_resees_dropped_index(db, tbl):
    tbl.create_index(["b"])
    assert tbl.has_index(["b"])
    name = db.inspect.get_indexes(TABLE)[0]["name"]
    db.query(f'DROP INDEX "{name}"')

    db.begin()
    db.rollback()

    # Stale index cache would still report the rolled-back index.
    assert not tbl.has_index(["b"])
    # Re-creating must really create it, not skip it.
    tbl.create_index(["b"])
    assert tbl.has_index(["b"])


def test_rollback_sees_new_external_structures(db, tbl):
    assert not tbl.has_column("c")
    db.query(f"ALTER TABLE {TABLE} ADD COLUMN c TEXT")

    db.begin()
    db.rollback()

    assert tbl.has_column("c")
    assert "c" in tbl.columns


def test_rewrite_after_schema_reverted(db, tbl):
    # Cache a column that the database then loses (rolled-back DDL).
    tbl.insert({"a": 2, "b": "y", "c": "temp"})
    assert tbl.has_column("c")
    db.query(f"ALTER TABLE {TABLE} DROP COLUMN c")

    db.begin()
    db.rollback()

    # Writing again must re-create the missing column, not skip it.
    tbl.insert({"a": 3, "b": "z", "c": "back"})
    assert tbl.has_column("c")
    assert tbl.find_one(a=3)["c"] == "back"


def test_rewrite_after_rollback_no_duplicate_structures(db, tbl):
    tbl.create_index(["b"])
    db.begin()
    db.rollback()
    # Existing columns must not be re-created (would raise), missing
    # ones must still be added.
    tbl.insert({"a": 2, "b": "y"})
    tbl.insert({"a": 3, "b": "z", "c": 1.5})
    assert tbl.has_column("c")
    assert tbl.find_one(a=3)["c"] == 1.5
    # Existing index must not be created twice (would raise).
    tbl.create_index(["b"])
    tbl.create_index(["b"])
    assert tbl.has_index(["b"])


def test_drop_recreate_same_name_table_same_handle(db):
    tbl = db.create_table("recreate_me")
    tbl.insert({"a": 1, "b": "x"})
    tbl.create_index(["b"])
    assert tbl.has_index(["b"])

    tbl.drop()
    assert tbl._table is None
    assert tbl._columns is None
    assert tbl._indexes == []

    # Re-create through the same Table handle.
    tbl.insert({"a": 2, "c": "y"})
    assert tbl.has_column("c")
    assert not tbl.has_column("b")
    # The old table's index must not leak into the new table.
    assert not tbl.has_index(["b"])
    tbl.create_index(["c"])
    assert tbl.has_index(["c"])
    assert tbl.find_one(a=2)["c"] == "y"


def test_nested_transaction_rollback(db, tbl):
    initial = len(tbl)
    db.begin()
    db.begin()
    tbl.insert({"a": 2, "b": "y", "c": "inner"})
    db.rollback()  # inner exit flushes caches
    assert tbl._table is None
    assert tbl._columns is None
    assert tbl._indexes == []
    db.rollback()  # outer exit performs the real rollback

    assert len(tbl) == initial
    # The same handle keeps working against the real structure. Note
    # that on backends with transactional DDL the added column is gone
    # again, while on SQLite (legacy pysqlite mode) DDL autocommits and
    # the column persists — either way the cache must match reality.
    assert tbl.has_column("b")
    assert tbl.has_column("c") == ("c" in _real_columns(db))
    tbl.insert({"a": 3, "b": "z", "c": "outer"})
    assert tbl.find_one(a=3)["c"] == "outer"


def test_nested_with_block_rollback(db, tbl):
    initial = len(tbl)
    with pytest.raises(RuntimeError):
        with db:
            with db:
                tbl.insert({"a": 2, "b": "y"})
                raise RuntimeError()
    assert len(tbl) == initial
    assert tbl.has_column("b")
    assert tbl.find_one(a=1)["b"] == "x"


def test_rows_fetched_before_rollback_unaffected(db, tbl):
    rows_before = [dict(r) for r in tbl.find(order_by="id")]
    db.begin()
    tbl.insert({"a": 100, "b": "tmp"})
    db.rollback()
    # Previously fetched row objects are untouched by the rollback.
    assert [dict(r) for r in tbl.find(order_by="id")] == rows_before
    assert rows_before[0]["b"] == "x"


def test_rollback_flush_scoped_to_database_instance(tmp_path):
    url = f"sqlite:///{tmp_path / 'shared.db'}"
    db1 = connect(url)
    db2 = connect(url)
    try:
        t1 = db1["shared_tbl"]
        t1.insert({"a": 1, "b": "x"})
        t2 = db2["shared_tbl"]
        # Warm db2's caches.
        assert t2.has_column("b")
        t2.create_index(["b"])
        assert t2.has_index(["b"])
        cached_table = t2._table
        cached_columns = t2._columns

        db1.begin()
        db1.rollback()

        # db1's tables are flushed ...
        assert t1._table is None
        assert t1._columns is None
        assert t1._indexes == []
        # ... but the other Database instance keeps its own state.
        assert t2._table is cached_table
        assert t2._columns is cached_columns
        assert t2.has_column("b")
        assert t2.has_index(["b"])
        assert t2.find_one(a=1)["b"] == "x"
    finally:
        db1.close()
        db2.close()
