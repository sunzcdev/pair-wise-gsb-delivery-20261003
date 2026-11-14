"""Tests for Database.query_retry() using a fault-injecting DB-API stub.

The stub wraps the local sqlite3 driver and lets tests inject connection
failures at exact points (before execute, before the first row, after the
first row) without relying on real network disconnects.
"""

import sqlite3
import threading
import types
from collections.abc import Iterator
from typing import Any

import pytest
from sqlalchemy import delete, select
from sqlalchemy.dialects import registry
from sqlalchemy.dialects.sqlite.pysqlite import SQLiteDialect_pysqlite
from sqlalchemy.exc import DBAPIError
from sqlalchemy.sql import column, table, text

import dataset
from dataset.util import DatasetError

from .conftest import TEST_CITY_1, TEST_DATA


class InjectedDisconnect(sqlite3.OperationalError):
    """Raised by the stub DB-API to simulate a lost connection."""


class FaultPlan:
    """Global failure-injection state for the stub DB-API."""

    def __init__(self) -> None:
        self.reset()

    def reset(self) -> None:
        # 1-based call numbers on which to raise InjectedDisconnect
        self.fail_on_execute: set[int] = set()
        self.fail_on_fetch: set[int] = set()
        # instrumentation
        self.executes = 0
        self.fetches = 0


FAULTS = FaultPlan()


class StubCursor:
    """DB-API cursor proxy that fails when the FaultPlan says so."""

    def __init__(self, real: sqlite3.Cursor) -> None:
        self._real = real

    def execute(self, sql: str, parameters: Any = ()) -> "StubCursor":
        FAULTS.executes += 1
        if FAULTS.executes in FAULTS.fail_on_execute:
            raise InjectedDisconnect("connection lost during execute")
        self._real.execute(sql, parameters)
        return self

    def executemany(self, sql: str, seq_of_parameters: Any) -> "StubCursor":
        self._real.executemany(sql, seq_of_parameters)
        return self

    def _fetch(self, func: Any, *args: Any) -> Any:
        FAULTS.fetches += 1
        if FAULTS.fetches in FAULTS.fail_on_fetch:
            raise InjectedDisconnect("connection lost during fetch")
        return func(*args)

    def fetchone(self) -> Any:
        return self._fetch(self._real.fetchone)

    def fetchmany(self, size: int | None = None) -> Any:
        if size is None:
            return self._fetch(self._real.fetchmany)
        return self._fetch(self._real.fetchmany, size)

    def fetchall(self) -> Any:
        return self._fetch(self._real.fetchall)

    @property
    def description(self) -> Any:
        return self._real.description

    @property
    def rowcount(self) -> int:
        return self._real.rowcount

    @property
    def lastrowid(self) -> int:
        return self._real.lastrowid

    @property
    def arraysize(self) -> int:
        return self._real.arraysize

    @arraysize.setter
    def arraysize(self, value: int) -> None:
        self._real.arraysize = value

    def close(self) -> None:
        self._real.close()

    def __getattr__(self, name: str) -> Any:
        return getattr(self._real, name)


class StubConnection:
    """DB-API connection proxy around a real sqlite3 connection."""

    def __init__(self, real: sqlite3.Connection) -> None:
        self._real = real

    def cursor(self) -> StubCursor:
        return StubCursor(self._real.cursor())

    def execute(self, sql: str, parameters: Any = ()) -> sqlite3.Cursor:
        # used by dataset's on_connect_statements (e.g. PRAGMA)
        return self._real.execute(sql, parameters)

    @property
    def isolation_level(self) -> Any:
        return self._real.isolation_level

    @isolation_level.setter
    def isolation_level(self, value: Any) -> None:
        self._real.isolation_level = value

    def close(self) -> None:
        self._real.close()

    def commit(self) -> None:
        self._real.commit()

    def rollback(self) -> None:
        self._real.rollback()

    def __getattr__(self, name: str) -> Any:
        return getattr(self._real, name)


def _make_dbapi() -> types.ModuleType:
    module = types.ModuleType("dataset_stub_dbapi")
    for name in dir(sqlite3):
        setattr(module, name, getattr(sqlite3, name))

    def connect(*args: Any, **kwargs: Any) -> StubConnection:
        return StubConnection(sqlite3.connect(*args, **kwargs))

    module.connect = connect  # type: ignore[attr-defined]
    return module


STUB_DBAPI = _make_dbapi()


class StubDialect(SQLiteDialect_pysqlite):
    """pysqlite dialect backed by the fault-injecting stub DB-API."""

    supports_statement_cache = True

    @classmethod
    def import_dbapi(cls) -> types.ModuleType:
        return STUB_DBAPI

    @classmethod
    def dbapi(cls) -> types.ModuleType:  # SQLAlchemy 1.4 name
        return STUB_DBAPI

    def is_disconnect(self, e: Exception, connection: Any, cursor: Any) -> bool:
        return isinstance(e, InjectedDisconnect)


registry.register("sqlite.stub", __name__, "StubDialect")


@pytest.fixture()
def db(tmp_path: Any) -> Iterator[Any]:
    FAULTS.reset()
    # File-based SQLite: a reconnect after an injected disconnect must see
    # the committed data, which an in-memory database would lose.
    db = dataset.connect(f"sqlite+stub:///{tmp_path}/retry.db")
    db["weather"].insert_many(TEST_DATA)
    FAULTS.reset()  # ignore setup statements in the instrumentation
    yield db
    db.close()
    FAULTS.reset()


def test_query_retry_without_faults(db: Any) -> None:
    rows = list(db.query_retry("SELECT * FROM weather"))
    assert len(rows) == len(TEST_DATA)
    assert FAULTS.executes == 1


def test_disconnect_before_execute_is_retried(db: Any) -> None:
    FAULTS.fail_on_execute = {1}
    rows = list(db.query_retry("SELECT place FROM weather"))
    assert len(rows) == len(TEST_DATA)
    assert FAULTS.executes == 2


def test_disconnect_before_first_row_is_retried(db: Any) -> None:
    # execute() succeeds, but the first fetch loses the connection.
    FAULTS.fail_on_fetch = {1}
    rows = list(db.query_retry("SELECT place FROM weather"))
    assert len(rows) == len(TEST_DATA)
    assert FAULTS.executes == 2


def test_disconnect_after_first_row_is_not_retried(db: Any) -> None:
    # The first row is delivered to the caller, then the connection dies:
    # re-running would duplicate rows, so the error must propagate.
    FAULTS.fail_on_fetch = {2}
    result = db.query_retry("SELECT place FROM weather", _step=1)
    first = next(result)
    assert first["place"] == TEST_DATA[0]["place"]
    with pytest.raises(DBAPIError):
        list(result)
    assert FAULTS.executes == 1  # no retry happened


def test_statement_error_is_not_retried(db: Any) -> None:
    # A plain SQL error (not a lost connection) must not be re-run.
    with pytest.raises(DBAPIError) as exc_info:
        list(db.query_retry("SELECT * FROM missing_table"))
    assert not exc_info.value.connection_invalidated
    assert FAULTS.executes == 1


@pytest.mark.parametrize(
    "sql",
    [
        "INSERT INTO weather (place) VALUES ('x')",
        "UPDATE weather SET place = 'x'",
        "DELETE FROM weather",
        "WITH x AS (SELECT 1) SELECT * FROM x",
        "PRAGMA journal_mode=WAL",
        "-- a comment\nDELETE FROM weather",
    ],
)
def test_non_select_string_is_rejected(db: Any, sql: str) -> None:
    with pytest.raises(DatasetError):
        db.query_retry(sql)
    assert FAULTS.executes == 0


def test_select_with_leading_comment_is_allowed(db: Any) -> None:
    rows = list(db.query_retry("/* hello */\nSELECT * FROM weather"))
    assert len(rows) == len(TEST_DATA)


def test_text_clause_and_select_expression(db: Any) -> None:
    rows = list(db.query_retry(text("SELECT * FROM weather")))
    assert len(rows) == len(TEST_DATA)
    stmt = select(table("weather", column("place")))
    rows = list(db.query_retry(stmt))
    assert len(rows) == len(TEST_DATA)


def test_write_text_clause_is_rejected(db: Any) -> None:
    with pytest.raises(DatasetError):
        db.query_retry(text("DELETE FROM weather"))
    assert FAULTS.executes == 0


def test_write_expression_is_rejected(db: Any) -> None:
    with pytest.raises(DatasetError):
        db.query_retry(delete(db["weather"].table))
    assert FAULTS.executes == 0


def test_explicit_transaction_is_rejected(db: Any) -> None:
    with db, pytest.raises(DatasetError):
        db.query_retry("SELECT * FROM weather")
    assert FAULTS.executes == 0


def test_retry_limit_is_bounded(db: Any) -> None:
    FAULTS.fail_on_execute = {1, 2, 3}
    with pytest.raises(DBAPIError) as exc_info:
        db.query_retry("SELECT * FROM weather", retries=1)
    assert exc_info.value.connection_invalidated
    assert FAULTS.executes == 2  # one initial attempt + one retry


def test_failed_connection_removed_from_thread_cache(db: Any) -> None:
    before = db.executable
    FAULTS.fail_on_execute = {1}
    rows = list(db.query_retry("SELECT * FROM weather"))
    assert len(rows) == len(TEST_DATA)
    tid = threading.get_ident()
    assert tid in db.connections
    assert db.connections[tid] is not before


def test_retry_reuses_frozen_params(db: Any) -> None:
    FAULTS.fail_on_execute = {1}
    rows = list(
        db.query_retry("SELECT * FROM weather WHERE place = :place", place=TEST_CITY_1)
    )
    assert len(rows) == 3
    assert all(row["place"] == TEST_CITY_1 for row in rows)


def test_row_type_side_effects_not_duplicated(db: Any) -> None:
    calls = []

    def counting_row_type(items: Any) -> Any:
        calls.append(1)
        return dict(items)

    db.row_type = counting_row_type
    FAULTS.fail_on_fetch = {1}
    rows = list(db.query_retry("SELECT * FROM weather"))
    assert len(rows) == len(TEST_DATA)
    # exactly one conversion per delivered row, none repeated by the retry
    assert len(calls) == len(TEST_DATA)


def test_empty_result(db: Any) -> None:
    rows = list(db.query_retry("SELECT * FROM weather WHERE place = 'nowhere'"))
    assert rows == []
