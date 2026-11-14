from typing import Any

import pytest

import dataset
from dataset.util import ResultIter


@pytest.fixture(scope="function")
def pool_db(tmp_path):
    """File-based SQLite so the engine uses a real QueuePool we can audit."""
    db = dataset.connect(f"sqlite:///{tmp_path}/result_iter.db")
    db["items"].insert_many([{"n": i} for i in range(10)])
    yield db
    db.close()


def checked_out(db) -> int:
    pool = db.engine.pool
    assert hasattr(pool, "checkedout"), "test requires a counting pool"
    return pool.checkedout()


class FakeRow:
    _mapping = {"a": 1}


class BrokenProxy:
    """A result proxy stub whose close() always fails."""

    def keys(self) -> list[str]:
        return ["a"]

    def fetchall(self) -> list[Any]:
        return [FakeRow()]

    def fetchmany(self, size: int = 1) -> list[Any]:
        return [FakeRow()]

    def close(self) -> None:
        raise RuntimeError("cleanup exploded")


def broken_row_type(items: Any) -> Any:
    raise ValueError("cannot convert row")


def test_query_exhaustion_releases_connection(pool_db):
    baseline = checked_out(pool_db)
    rows = pool_db.query("SELECT * FROM items")
    assert checked_out(pool_db) == baseline + 1
    assert len(list(rows)) == 10
    # Normal exhaustion released the dedicated connection.
    assert checked_out(pool_db) == baseline
    rows.close()  # idempotent after exhaustion


def test_query_early_close_releases_connection(pool_db):
    baseline = checked_out(pool_db)
    rows = pool_db.query("SELECT * FROM items")
    next(rows)
    rows.close()
    assert checked_out(pool_db) == baseline
    rows.close()  # idempotent after early exit


def test_query_context_manager(pool_db):
    baseline = checked_out(pool_db)
    with pool_db.query("SELECT * FROM items") as rows:
        assert checked_out(pool_db) == baseline + 1
        assert next(rows)["n"] is not None
    assert checked_out(pool_db) == baseline
    rows.close()  # idempotent after context manager exit


def test_query_conversion_error_releases_connection(pool_db):
    baseline = checked_out(pool_db)
    pool_db.row_type = broken_row_type
    rows = pool_db.query("SELECT * FROM items")
    with pytest.raises(ValueError, match="cannot convert row"):
        next(rows)
    # The owned connection was returned despite the conversion error.
    assert checked_out(pool_db) == baseline
    rows.close()  # idempotent after conversion error


def test_query_in_transaction_keeps_caller_transaction(pool_db):
    db = pool_db
    table = db["items"]
    baseline = checked_out(db)
    with db:
        rows = db.query("SELECT * FROM items")
        # No extra connection is checked out inside a transaction.
        assert checked_out(db) == baseline
        next(rows)
        rows.close()
        rows.close()
        # Closing the iterator did not end the caller's transaction.
        assert db.in_transaction
        table.insert({"n": 42})
    assert table.count() == 11


def test_find_in_transaction_shared_connection(pool_db):
    db = pool_db
    table = db["items"]
    baseline = checked_out(db)
    with db:
        res1 = table.find()
        res2 = table.find()
        assert checked_out(db) == baseline
        next(res1)
        res1.close()
        res1.close()  # idempotent
        # The sibling iterator on the same connection is unaffected.
        assert len(list(res2)) == 10
        # The caller's transaction is still open and usable.
        assert db.in_transaction
        table.insert({"n": 100})
    assert table.count() == 11


def test_streamed_find_releases_connection(pool_db):
    baseline = checked_out(pool_db)
    rows = pool_db["items"].find(_streamed=True, _step=2)
    assert checked_out(pool_db) == baseline + 1
    next(rows)
    rows.close()
    assert checked_out(pool_db) == baseline
    rows.close()


def test_streamed_find_exhaustion_releases_connection(pool_db):
    baseline = checked_out(pool_db)
    rows = pool_db["items"].find(_streamed=True, _step=2)
    assert len(list(rows)) == 10
    assert checked_out(pool_db) == baseline


def test_close_failure_does_not_mask_conversion_error():
    rows = ResultIter(BrokenProxy(), row_type=broken_row_type)  # type: ignore[arg-type]
    with pytest.raises(ValueError, match="cannot convert row") as exc_info:
        next(rows)
    # The real conversion error is preserved, unchained from the
    # (suppressed) cleanup failure.
    assert exc_info.value.__context__ is None
    assert rows._closed
    rows.close()  # idempotent even after a failed cleanup


def test_explicit_close_propagates_cleanup_error_once():
    rows = ResultIter(BrokenProxy())  # type: ignore[arg-type]
    with pytest.raises(RuntimeError, match="cleanup exploded"):
        rows.close()
    rows.close()  # second call is a no-op
