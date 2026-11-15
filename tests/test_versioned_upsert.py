import math
import sqlite3
import threading

import pytest

from sqlite_utils import Database
from sqlite_utils.db import (
    InvalidColumns,
    InvalidVersionError,
    PrimaryKeyRequired,
    VersionConflict,
    VersionedUpsertResult,
)


def test_versioned_upsert_inserts_new_rows(fresh_db):
    table = fresh_db.table("events")
    result = table.upsert_all(
        [{"id": 1, "version": 1, "name": "Cleo"}],
        pk="id",
        version="version",
    )
    assert isinstance(result, VersionedUpsertResult)
    assert result.inserted == 1
    assert result.updated == result.stale == result.replayed == 0
    assert result.conflict_count == 0
    assert result.succeeded == 1
    assert result.skipped == 0
    assert result.total == 1
    assert list(table.rows) == [{"id": 1, "version": 1, "name": "Cleo"}]
    assert table.last_pk == 1


def test_versioned_upsert_higher_version_updates(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 1, "name": "Cleo", "age": 4}],
        pk="id",
        version="version",
    )
    result = table.upsert(
        {"id": 1, "version": 2, "name": "Cleo!",},
        pk="id",
        version="version",
    )
    assert result.updated == 1
    row = list(table.rows)[0]
    assert row == {"id": 1, "version": 2, "name": "Cleo!", "age": 4}
    assert table.last_pk == 1


def test_versioned_upsert_lower_version_is_stale_and_skipped(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 3, "name": "new"}],
        pk="id",
        version="version",
    )
    result = table.upsert_all(
        [
            {"id": 1, "version": 2, "name": "older"},
            {"id": 1, "version": 1, "name": "oldest"},
        ],
        pk="id",
        version="version",
    )
    assert result.stale == 2
    assert result.succeeded == 0
    # The stale record must not have overwritten the newer value
    assert list(table.rows) == [{"id": 1, "version": 3, "name": "new"}]


def test_versioned_upsert_out_of_order_batch(fresh_db):
    table = fresh_db.table("events")
    result = table.upsert_all(
        [
            {"id": 1, "version": 2, "name": "v2"},
            {"id": 1, "version": 1, "name": "v1"},
            {"id": 1, "version": 3, "name": "v3"},
        ],
        pk="id",
        version="version",
    )
    assert (result.inserted, result.updated, result.stale) == (1, 1, 1)
    assert list(table.rows) == [{"id": 1, "version": 3, "name": "v3"}]


def test_versioned_upsert_same_version_exact_replay(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 5, "name": "Cleo", "age": 4}],
        pk="id",
        version="version",
    )
    result = table.upsert_all(
        [{"id": 1, "version": 5, "name": "Cleo", "age": 4}],
        pk="id",
        version="version",
    )
    assert result.replayed == 1
    assert result.succeeded == 0
    assert result.conflict_count == 0
    assert list(table.rows) == [{"id": 1, "version": 5, "name": "Cleo", "age": 4}]


def test_versioned_upsert_same_version_sparse_replay_ignores_omitted_fields(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 5, "name": "Cleo", "age": 4}],
        pk="id",
        version="version",
    )
    # Only provided fields are compared - omitted "name" is not an explicit NULL
    result = table.upsert_all(
        [{"id": 1, "version": 5, "age": 4}],
        pk="id",
        version="version",
    )
    assert result.replayed == 1
    assert list(table.rows) == [{"id": 1, "version": 5, "name": "Cleo", "age": 4}]


def test_versioned_upsert_same_version_conflict_is_reported_not_applied(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 5, "name": "Cleo", "age": 4}],
        pk="id",
        version="version",
    )
    result = table.upsert_all(
        [{"id": 1, "version": 5, "name": "Pancakes", "age": 4}],
        pk="id",
        version="version",
    )
    assert result.conflict_count == 1
    assert result.inserted == result.updated == 0
    conflict = result.conflicts[0]
    assert isinstance(conflict, VersionConflict)
    assert conflict.pk == 1
    assert conflict.version == 5
    assert conflict.differing_fields == ("name",)
    assert conflict.record == {"id": 1, "version": 5, "name": "Pancakes", "age": 4}
    assert conflict.existing["name"] == "Cleo"
    # Stored row untouched
    assert list(table.rows) == [{"id": 1, "version": 5, "name": "Cleo", "age": 4}]


def test_versioned_upsert_sparse_update_preserves_omitted_columns(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 1, "name": "Cleo", "age": 4, "note": "x"}],
        pk="id",
        version="version",
    )
    result = table.upsert_all(
        [{"id": 1, "version": 2, "age": 5}],
        pk="id",
        version="version",
    )
    assert result.updated == 1
    assert list(table.rows) == [
        {"id": 1, "version": 2, "name": "Cleo", "age": 5, "note": "x"}
    ]


def test_versioned_upsert_explicit_null_is_distinct_from_omitted(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 1, "name": "Cleo", "note": "keep"}],
        pk="id",
        version="version",
    )
    table.upsert_all(
        [{"id": 1, "version": 2, "note": None}],
        pk="id",
        version="version",
    )
    row = list(table.rows)[0]
    assert row["note"] is None
    assert row["name"] == "Cleo"
    # Replaying the same explicit NULL against the stored NULL is a replay
    replay = table.upsert_all(
        [{"id": 1, "version": 2, "note": None}],
        pk="id",
        version="version",
    )
    assert replay.replayed == 1


def test_versioned_upsert_compound_primary_key(fresh_db):
    table = fresh_db.table("summary")
    result = table.upsert_all(
        [
            {"source": "a", "object": "b", "version": 1, "count": 10},
            {"source": "a", "object": "b", "version": 2, "count": 20},
            {"source": "a", "object": "b", "version": 1, "count": 99},
        ],
        pk=("source", "object"),
        version="version",
    )
    assert (result.inserted, result.updated, result.stale) == (1, 1, 1)
    assert table.last_pk == ("a", "b")
    assert list(table.rows) == [
        {"source": "a", "object": "b", "version": 2, "count": 20}
    ]


def test_versioned_upsert_uses_existing_compound_pk(fresh_db):
    fresh_db.execute(
        "create table summary (source text, object text, version integer, "
        "count integer, primary key (source, object))"
    )
    table = fresh_db.table("summary")
    result = table.upsert(
        {"source": "a", "object": "b", "version": 1, "count": 10},
        version="version",
    )
    assert result.inserted == 1
    assert table.last_pk == ("a", "b")


def test_versioned_upsert_missing_version_column_rejects_batch(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 1, "name": "Cleo"}],
        pk="id",
        version="version",
    )
    with pytest.raises(InvalidVersionError):
        table.upsert_all(
            [{"id": 2, "name": "no version here"}],
            pk="id",
            version="version",
            alter=True,
        )
    # Nothing was written
    assert table.count == 1


@pytest.mark.parametrize(
    "bad_value",
    [None, True, False, math.nan, [1], {"v": 1}, object()],
)
def test_versioned_upsert_invalid_version_values_rejected(fresh_db, bad_value):
    table = fresh_db.table("events")
    with pytest.raises(InvalidVersionError):
        table.upsert_all(
            [{"id": 1, "version": bad_value, "name": "Cleo"}],
            pk="id",
            version="version",
        )
    assert not table.exists()


def test_versioned_upsert_invalid_version_rolls_back_entire_batch(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 1, "name": "Cleo"}],
        pk="id",
        version="version",
    )
    # The valid first record would insert id=2, but the invalid second one
    # must roll the whole transaction back
    with pytest.raises(InvalidVersionError):
        table.upsert_all(
            [
                {"id": 2, "version": 1, "name": "Nixie"},
                {"id": 3, "version": None, "name": "Bad"},
            ],
            pk="id",
            version="version",
        )
    assert [row["id"] for row in table.rows] == [1]


def test_versioned_upsert_incomparable_stored_and_incoming_version(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 5, "name": "Cleo"}],
        pk="id",
        version="version",
    )
    # A string cannot be ordered against the stored integer in Python - the
    # record must be rejected rather than relying on SQLite type affinity
    with pytest.raises(InvalidVersionError):
        table.upsert_all(
            [{"id": 1, "version": "6", "name": "Cleo"}],
            pk="id",
            version="version",
        )
    assert list(table.rows) == [{"id": 1, "version": 5, "name": "Cleo"}]


def test_versioned_upsert_string_versions_use_python_ordering(fresh_db):
    table = fresh_db.table("events")
    result = table.upsert_all(
        [
            {"id": 1, "version": "b", "name": "b"},
            {"id": 1, "version": "a", "name": "a"},
            {"id": 1, "version": "c", "name": "c"},
        ],
        pk="id",
        version="version",
    )
    assert (result.inserted, result.updated, result.stale) == (1, 1, 1)
    assert list(table.rows) == [{"id": 1, "version": "c", "name": "c"}]


def test_versioned_upsert_float_versions(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 1.0, "name": "one"}],
        pk="id",
        version="version",
    )
    result = table.upsert_all(
        [{"id": 1, "version": 1.5, "name": "one-point-five"}],
        pk="id",
        version="version",
    )
    assert result.updated == 1
    assert list(table.rows)[0]["version"] == 1.5


def test_versioned_upsert_requires_pk(fresh_db):
    with pytest.raises(PrimaryKeyRequired):
        fresh_db.table("events").upsert_all(
            [{"version": 1, "name": "Cleo"}], version="version"
        )


def test_versioned_upsert_missing_pk_value_rejected(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 1, "name": "Cleo"}],
        pk="id",
        version="version",
    )
    with pytest.raises(PrimaryKeyRequired):
        table.upsert_all(
            [{"version": 2, "name": "No pk"}], pk="id", version="version"
        )
    with pytest.raises(PrimaryKeyRequired):
        table.upsert_all(
            [{"id": None, "version": 2, "name": "Null pk"}],
            pk="id",
            version="version",
        )


def test_versioned_upsert_version_column_cannot_be_pk(fresh_db):
    with pytest.raises(ValueError):
        fresh_db.table("events").upsert_all(
            [{"version": 1, "name": "Cleo"}], pk="version", version="version"
        )


def test_versioned_upsert_unknown_column_requires_alter(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 1, "name": "Cleo"}],
        pk="id",
        version="version",
    )
    with pytest.raises(InvalidColumns):
        table.upsert_all(
            [{"id": 1, "version": 2, "brand_new": "x"}],
            pk="id",
            version="version",
        )
    result = table.upsert_all(
        [{"id": 1, "version": 2, "brand_new": "x"}],
        pk="id",
        version="version",
        alter=True,
    )
    assert result.updated == 1
    assert list(table.rows)[0]["brand_new"] == "x"


def test_versioned_upsert_rejects_unsupported_options(fresh_db):
    table = fresh_db.table("events")
    with pytest.raises(ValueError):
        table.upsert_all(
            [{"id": 1, "version": 1}],
            pk="id",
            version="version",
            hash_id="hash",
        )
    with pytest.raises(ValueError):
        table.upsert_all(
            [{"id": 1, "version": 1}],
            pk="id",
            version="version",
            extracts={"id": "other"},
        )
    with pytest.raises(ValueError):
        table.upsert_all(
            [{"id": 1, "version": 1}],
            pk="id",
            version="version",
            conversions={"id": "upper(?)"},
        )


def test_versioned_upsert_rejects_list_mode_records(fresh_db):
    with pytest.raises(ValueError):
        fresh_db.table("events").upsert_all(
            [["id", "version"], [1, 1]], pk="id", version="version"
        )


def test_versioned_upsert_empty_input(fresh_db):
    table = fresh_db.table("events")
    result = table.upsert_all([], pk="id", version="version")
    assert isinstance(result, VersionedUpsertResult)
    assert result.total == 0
    assert not table.exists()


def test_versioned_upsert_mixed_batch_stats(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 5, "name": "Cleo", "age": 4}],
        pk="id",
        version="version",
    )
    result = table.upsert_all(
        [
            {"id": 2, "version": 1, "name": "Nixie"},       # insert
            {"id": 1, "version": 6, "name": "Cleo2"},       # update
            {"id": 1, "version": 2, "name": "old"},         # stale
            {"id": 1, "version": 6, "name": "Cleo2"},       # replay
            {"id": 1, "version": 5, "name": "conflict"},    # stale (row now at v6)
        ],
        pk="id",
        version="version",
    )
    assert result.inserted == 1
    assert result.updated == 1
    assert result.stale == 2
    assert result.replayed == 1
    assert result.conflict_count == 0
    assert result.succeeded == 2
    assert result.skipped == 3
    assert result.total == 5


def test_versioned_upsert_conflict_alongside_other_outcomes(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"id": 1, "version": 5, "name": "Cleo", "age": 4}],
        pk="id",
        version="version",
    )
    result = table.upsert_all(
        [
            {"id": 2, "version": 1, "name": "Nixie"},       # insert
            {"id": 1, "version": 6, "name": "Cleo2"},       # update
            {"id": 1, "version": 6, "name": "different"},   # conflict
        ],
        pk="id",
        version="version",
    )
    assert result.inserted == 1
    assert result.updated == 1
    assert result.conflict_count == 1
    assert result.total == 3


def test_ordinary_upsert_behaviour_unchanged(fresh_db):
    table = fresh_db.table("table")
    table.insert_all([{"id": 1, "name": "Cleo"}], pk="id", replace=True)
    returned = table.upsert({"id": 1, "age": 5}, pk="id", alter=True)
    # Plain upsert still returns the Table, not a result object
    assert returned is table
    assert list(table.rows) == [{"id": 1, "name": "Cleo", "age": 5}]
    returned_all = table.upsert_all(
        [{"id": 2, "name": "Nixie"}], pk="id", alter=True
    )
    assert returned_all is table


def test_versioned_upsert_case_insensitive_column_names(fresh_db):
    table = fresh_db.table("events")
    table.upsert_all(
        [{"ID": 1, "Version": 1, "Name": "Cleo"}],
        pk="id",
        version="version",
    )
    result = table.upsert_all(
        [{"id": 1, "version": 2, "name": "Cleo!"}],
        pk="id",
        version="version",
    )
    assert result.updated == 1
    assert list(table.rows) == [{"ID": 1, "Version": 2, "Name": "Cleo!"}]


def _make_threadsafe_databases(path):
    # The --sqlite-autocommit test mode patches connections to use PEP 249
    # autocommit, which Database() explicitly rejects when wrapping an
    # existing connection; the interleave tests need that wrapping to share
    # the connection across threads.
    autocommit = getattr(sqlite3, "LEGACY_TRANSACTION_CONTROL", -1)
    conn1 = sqlite3.connect(path, check_same_thread=False)
    conn2 = sqlite3.connect(path, check_same_thread=False, timeout=10)
    if getattr(conn1, "autocommit", None) not in (None, autocommit):
        conn1.close()
        conn2.close()
        pytest.skip("interleave tests require legacy-transaction connections")
    return Database(conn1), Database(conn2)


def test_versioned_upsert_interleaved_connections_no_lost_insert(tmp_path):
    # Two local connections to the same file database. Connection 1 parks
    # inside its versioned upsert, between the existence check and the write.
    # Connection 2 must block on the write lock rather than reading a stale
    # "row does not exist" state and inserting a duplicate/losing row.
    path = str(tmp_path / "interleave.db")
    db1, db2 = _make_threadsafe_databases(path)
    db1.execute(
        "CREATE TABLE events (id INTEGER PRIMARY KEY, version INTEGER, name TEXT)"
    )
    t1, t2 = db1.table("events"), db2.table("events")

    reached_check, release = threading.Event(), threading.Event()

    def tracer(sql, params):
        if "_su_rowid" in sql and not reached_check.is_set():
            reached_check.set()
            release.wait(timeout=10)

    db1._tracer = tracer
    outcomes = {}

    def worker1():
        outcomes["r1"] = t1.upsert(
            {"id": 1, "version": 10, "name": "ten"},
            pk="id",
            version="version",
        )

    thread1 = threading.Thread(target=worker1)
    thread1.start()
    assert reached_check.wait(5)

    conn2_finished = threading.Event()

    def worker2():
        outcomes["r2"] = t2.upsert(
            {"id": 1, "version": 5, "name": "five"},
            pk="id",
            version="version",
        )
        conn2_finished.set()

    thread2 = threading.Thread(target=worker2)
    thread2.start()
    # The second connection cannot make progress while the first holds the
    # BEGIN IMMEDIATE write lock
    assert not conn2_finished.wait(1)

    release.set()
    thread1.join()
    thread2.join()
    assert conn2_finished.wait(5)

    assert outcomes["r1"].inserted == 1
    assert outcomes["r2"].stale == 1
    assert list(t1.rows) == [{"id": 1, "version": 10, "name": "ten"}]
    db1.close()
    db2.close()


def test_versioned_upsert_interleaved_connections_no_stale_overwrite(tmp_path):
    # Same interleaving, but the row already exists with version 5. The parked
    # connection 1 is applying version 10; connection 2's version 7 must be
    # classified stale once it finally acquires the lock.
    path = str(tmp_path / "stale.db")
    db1, db2 = _make_threadsafe_databases(path)
    db1.execute(
        "CREATE TABLE events (id INTEGER PRIMARY KEY, version INTEGER, name TEXT)"
    )
    db1.execute("INSERT INTO events VALUES (1, 5, 'five')")
    t1, t2 = db1.table("events"), db2.table("events")

    reached_check, release = threading.Event(), threading.Event()

    def tracer(sql, params):
        if "_su_rowid" in sql and not reached_check.is_set():
            reached_check.set()
            release.wait(timeout=10)

    db1._tracer = tracer
    outcomes = {}

    def worker1():
        outcomes["r1"] = t1.upsert(
            {"id": 1, "version": 10, "name": "ten"},
            pk="id",
            version="version",
        )

    thread1 = threading.Thread(target=worker1)
    thread1.start()
    assert reached_check.wait(5)

    conn2_finished = threading.Event()

    def worker2():
        outcomes["r2"] = t2.upsert(
            {"id": 1, "version": 7, "name": "seven"},
            pk="id",
            version="version",
        )
        conn2_finished.set()

    thread2 = threading.Thread(target=worker2)
    thread2.start()
    assert not conn2_finished.wait(1)

    release.set()
    thread1.join()
    thread2.join()
    assert conn2_finished.wait(5)

    assert outcomes["r1"].updated == 1
    assert outcomes["r2"].stale == 1
    assert list(t1.rows) == [{"id": 1, "version": 10, "name": "ten"}]
    db1.close()
    db2.close()


def test_atomic_immediate_acquires_write_lock(tmp_path):
    # atomic(immediate=True) takes the write lock immediately; a second
    # connection cannot begin its own write transaction until it is released.
    path = str(tmp_path / "atomic.db")
    db1, db2 = _make_threadsafe_databases(path)
    db1.execute("CREATE TABLE t (id INTEGER PRIMARY KEY)")
    finished = threading.Event()

    def worker():
        with db2.atomic(immediate=True):
            db2.execute("INSERT INTO t (id) VALUES (2)")
        finished.set()

    with db1.atomic(immediate=True):
        db1.execute("INSERT INTO t (id) VALUES (1)")
        thread = threading.Thread(target=worker)
        thread.start()
        assert not finished.wait(1)
    thread.join()
    assert finished.wait(5)
    assert [row["id"] for row in db1.table("t").rows] == [1, 2]
    db1.close()
    db2.close()
