"""Tests for concurrent schema changes (automatic column expansion).

Two collection threads writing into the same table are simulated with two
independent ``Database`` instances pointing at one file-based SQLite
database. Each test uses an explicit, fixed interleaving of steps (rather
than free-running threads) so that the race between schema reflection and
``ALTER TABLE`` is exercised deterministically.
"""

import threading

import pytest
from alembic.operations import Operations
from sqlalchemy.exc import OperationalError

import dataset
from dataset.util import DatasetError


class _StepCoordinator:
    """Synchronize two workers at named, ordered checkpoints.

    ``wait_a`` / ``wait_b`` block the corresponding worker until the other
    worker releases the checkpoint with ``release_b`` / ``release_a``. This
    makes the interleaving of the two "threads" fully deterministic while
    still running the code on real OS threads and real connections.
    """

    def __init__(self) -> None:
        self._events: dict[str, threading.Event] = {}

    def _event(self, name: str) -> threading.Event:
        if name not in self._events:
            self._events[name] = threading.Event()
        return self._events[name]

    def wait_a(self, name: str) -> None:
        self._event(f"a:{name}").wait(timeout=10)

    def wait_b(self, name: str) -> None:
        self._event(f"b:{name}").wait(timeout=10)

    def release_a(self, name: str) -> None:
        self._event(f"a:{name}").set()

    def release_b(self, name: str) -> None:
        self._event(f"b:{name}").set()


def _run_pair(worker_a, worker_b) -> None:
    """Run two callables on separate threads and surface their failures."""
    errors: list[BaseException] = []

    def wrap(fn):
        def runner() -> None:
            try:
                fn()
            except BaseException as exc:  # captured and re-raised below
                errors.append(exc)

        return runner

    thread_a = threading.Thread(target=wrap(worker_a))
    thread_b = threading.Thread(target=wrap(worker_b))
    thread_a.start()
    thread_b.start()
    thread_a.join(timeout=15)
    thread_b.join(timeout=15)
    assert not thread_a.is_alive() and not thread_b.is_alive(), "worker deadlocked"
    if errors:
        raise errors[0]


@pytest.fixture()
def two_databases(tmp_path):
    """Two independent Database objects sharing one temporary SQLite file."""
    url = f"sqlite:///{tmp_path / 'concurrent_schema.db'}"
    db_a = dataset.connect(url)
    db_b = dataset.connect(url)
    # Base table is created up front; the tests only race over new columns.
    db_a["events"].insert({"seed": 1})
    yield db_a, db_b
    db_a.close()
    db_b.close()


def test_concurrent_same_type_column_converges(two_databases):
    """Two workers adding the same column with the same type cause one ALTER.

    The loser of the race hits a duplicate-column database error, which must
    be converged into a single structural change instead of propagating.
    """
    db_a, db_b = two_databases
    steps = _StepCoordinator()
    alter_calls: list[str] = []
    original_add_column = Operations.add_column

    def counting_add_column(self, table_name, column, **kw):
        alter_calls.append(column.name)
        return original_add_column(self, table_name, column, **kw)

    Operations.add_column = counting_add_column
    try:

        def worker_a() -> None:
            table_a = db_a["events"]
            # Step 1: reflect the table before the new column exists, so the
            # cached table object on this side is stale once B commits.
            assert "payload" not in table_a.columns
            steps.release_b("reflected")
            steps.wait_a("b_added")
            # Step 3: B has created and committed the column. A still relies
            # on its own local lock and stale cache, so it attempts another
            # ALTER which fails as a duplicate and is converged.
            table_a.insert({"seed": 2, "payload": "from-a"})

        def worker_b() -> None:
            table_b = db_b["events"]
            steps.wait_b("reflected")
            # Step 2: B performs the winning structural change and commits.
            table_b.insert({"seed": 3, "payload": "from-b"})
            assert "payload" in table_b.columns
            steps.release_a("b_added")

        _run_pair(worker_a, worker_b)
    finally:
        Operations.add_column = original_add_column

    # Only one structural change reached the database despite two writers.
    assert alter_calls == ["payload"], alter_calls

    # Both Database objects must now agree with the real database structure.
    table_a = db_a["events"]
    table_b = db_b["events"]
    assert "payload" in table_a.columns
    assert "payload" in table_b.columns
    pragma_rows = list(db_a.query("PRAGMA table_info(events)"))
    assert [row["name"] for row in pragma_rows] == ["id", "seed", "payload"]
    rows = list(table_a.find(order_by="seed"))
    assert [row["payload"] for row in rows] == [None, "from-a", "from-b"]
    # A's stale table object must have been replaced, not kept around.
    assert "payload" in table_a.table.c


def test_concurrent_incompatible_type_conflicts(two_databases):
    """Same column name but incompatible type produces a clear conflict."""
    db_a, db_b = two_databases
    steps = _StepCoordinator()

    def worker_a() -> None:
        table_a = db_a["events"]
        assert "score" not in table_a.columns
        steps.release_b("reflected")
        steps.wait_a("b_added")
        # B created ``score`` as TEXT; A wants to store an integer.
        with pytest.raises(DatasetError, match="incompatible type"):
            table_a.insert({"seed": 4, "score": 42})

    def worker_b() -> None:
        table_b = db_b["events"]
        steps.wait_b("reflected")
        table_b.insert({"seed": 5, "score": "text-value"})
        steps.release_a("b_added")

    _run_pair(worker_a, worker_b)

    # The failed write must not leave A working with a stale table object:
    # subsequent writes to existing columns still work and see the real
    # structure (one TEXT column, no INTEGER column of the same name).
    table_a = db_a["events"]
    table_a.insert({"seed": 6})
    assert "score" in table_a.columns
    rows = list(table_a.find(order_by="seed"))
    assert [row["seed"] for row in rows] == [1, 5, 6]
    assert table_a.find_one(seed=5)["score"] == "text-value"


def test_other_database_errors_propagate(two_databases, monkeypatch):
    """Non-duplicate-column errors during expansion are not swallowed."""
    db_a, _db_b = two_databases
    table_a = db_a["events"]

    def broken_add_column(self, *args, **kwargs):
        raise OperationalError(
            "ALTER TABLE", {}, Exception("simulated storage failure")
        )

    # ``Database.op`` builds a fresh Operations object on every access, so the
    # underlying Alembic method must be patched rather than the property.
    monkeypatch.setattr(Operations, "add_column", broken_add_column)
    with pytest.raises(OperationalError, match="simulated storage failure"):
        table_a.insert({"seed": 7, "extra": "boom"})

    # The table remains usable for writes that do not need the new column.
    table_a.insert({"seed": 8})
    assert "extra" not in table_a.columns
    assert table_a.find_one(seed=8) is not None


def test_rollback_forgets_reverted_columns(tmp_path):
    """A column undone by a rollback must not survive in the reflection cache."""
    url = f"sqlite:///{tmp_path / 'rollback_schema.db'}"
    db_a = dataset.connect(url)
    db_b = dataset.connect(url)
    table = db_a["rollback_table"]
    table.insert({"seed": 1})

    with db_a:
        table.create_column("transient", db_a.types.text)
        assert table.has_column("transient")
        assert "transient" in table.columns
        # A second connection removes the column again while A's transaction
        # is open, so when A rolls back its cached schema is stale.
        db_b.executable.exec_driver_sql(
            "ALTER TABLE rollback_table RENAME COLUMN transient TO gone"
        )
        db_b.executable.commit()
        db_a.rollback()

    # Both the reflected table object and the cached column map must have
    # been invalidated by the rollback.
    assert table._table is None
    assert table._columns is None
    # The next access re-reflects against the real structure and must not
    # report a column that no longer exists.
    assert not table.has_column("transient")
    assert "transient" not in table.columns
    assert "transient" not in table.table.c
    assert "gone" in table.columns

    # Writes continue to work against the refreshed table object.
    table.insert({"seed": 2})
    assert table.find_one(seed=2) is not None
    db_a.close()
    db_b.close()


def test_reflection_cache_refreshed_after_duplicate(two_databases):
    """After a duplicate-column error the cache matches the real table."""
    db_a, db_b = two_databases
    table_a = db_a["events"]
    table_b = db_b["events"]

    # Prime A's cache with a structure that lacks the upcoming column.
    assert "note" not in table_a.columns
    table_b.insert({"seed": 9, "note": "created-by-b"})

    # B's commit is visible through a fresh statement on A's connection;
    # the insert must converge on the existing column and refresh the cache.
    table_a.insert({"seed": 10, "note": "created-by-a"})

    # The refreshed table object is the one actually used for the write.
    assert "note" in table_a.table.c
    fresh = db_a.load_table("events")
    assert "note" in fresh.columns
    assert list(fresh.find(order_by="seed"))[-1]["note"] == "created-by-a"

    # A second write must not attempt any ALTER anymore.
    table_a.insert({"seed": 11, "note": "no-new-alter"})
    assert table_a.count(note="no-new-alter") == 1
