import logging
from collections import OrderedDict
from collections.abc import Callable, Iterable, Iterator, Mapping
from datetime import date, datetime
from decimal import Decimal
from hashlib import sha1
from types import TracebackType
from typing import Any
from urllib.parse import urlencode, urlparse

from sqlalchemy import Connection, ResultProxy
from sqlalchemy.engine import Row
from sqlalchemy.exc import ResourceClosedError

QUERY_STEP = 1000

log = logging.getLogger(__name__)

# Type definitions for SQL values and rows
SQLPlainValue = (
    None  # NULL
    | bool  # BOOLEAN
    | int  # INTEGER, BIGINT
    | float  # FLOAT, REAL, DOUBLE
    | str  # VARCHAR, TEXT, CHAR
    | bytes  # BINARY, BLOB, BYTEA
    | Decimal  # NUMERIC, DECIMAL
    | date  # DATE
    | datetime  # DATETIME, TIMESTAMP
)
SQLWriteValue = (
    SQLPlainValue
    | dict[str, SQLPlainValue]  # JSON, JSONB
    | list[SQLPlainValue]  # JSON arrays
)

# Type alias for input rows (dict-like with SQL-compatible values)
WriteRow = Mapping[str, SQLWriteValue]
# Mutable row dict — used where rows are built up or mutated in place
MutableRow = dict[str, SQLWriteValue]
OutRow = Mapping[str, Any]
RowFactory = Callable[[Iterable[tuple[str, Any]]], OutRow]

row_factory: RowFactory = OrderedDict


def convert_row(factory: RowFactory, row: Row[Any]) -> OutRow:
    return factory(row._mapping.items())  # type: ignore[arg-type]


class DatasetError(Exception):
    pass


class QueryError(DatasetError):
    pass


def iter_result_proxy(
    rp: ResultProxy[Any], step: int | None = None
) -> Iterator[Row[Any]]:
    """Iterate over the ResultProxy."""
    while True:
        chunk = rp.fetchall() if step is None else rp.fetchmany(size=step)
        if not chunk:
            break
        yield from chunk


def make_sqlite_url(
    path: str,
    cache: str | None = None,
    timeout: int | None = None,
    mode: str | None = None,
    check_same_thread: bool = True,
    immutable: bool = False,
    nolock: bool = False,
) -> str:
    # NOTE: this PR
    # https://gerrit.sqlalchemy.org/c/sqlalchemy/sqlalchemy/+/1474/
    # added support for URIs in SQLite
    # The full list of supported URIs is a combination of:
    # https://docs.python.org/3/library/sqlite3.html#sqlite3.connect
    # and
    # https://www.sqlite.org/uri.html
    params: dict[str, Any] = {}
    if cache:
        assert cache in ("shared", "private")
        params["cache"] = cache
    if timeout:
        # Note: if timeout is None, it uses the default timeout
        params["timeout"] = timeout
    if mode:
        assert mode in ("ro", "rw", "rwc")
        params["mode"] = mode
    if nolock:
        params["nolock"] = 1
    if immutable:
        params["immutable"] = 1
    if not check_same_thread:
        params["check_same_thread"] = "false"
    if not params:
        return "sqlite:///" + path
    params["uri"] = "true"
    return "sqlite:///file:" + path + "?" + urlencode(params)


class ResultIter(Iterator[OutRow]):
    """SQLAlchemy ResultProxies are not iterable to get a
    list of dictionaries. This is to wrap them.

    The iterator owns the result proxy (i.e. the cursor) it wraps and
    releases it as soon as the iterator is exhausted, closed, or hits an
    error during row conversion. If a dedicated ``connection`` is passed
    in, the iterator takes ownership of that connection as well and
    returns it to the connection pool on :py:meth:`close`. Connections
    shared with the caller — such as the connection backing an open
    transaction — must not be passed in and are never closed here, so
    closing one iterator can neither invalidate other results on the
    same connection nor end the caller's transaction.

    The iterator can be used as a context manager to guarantee release
    of the underlying resources::

        with db.query('SELECT * FROM tbl') as rows:
            for row in rows:
                ...

    :py:meth:`close` is idempotent: it may be called repeatedly, after
    normal exhaustion, early exit, or conversion errors.
    """

    def __init__(
        self,
        result_proxy: ResultProxy[Any] | None,
        row_type: RowFactory = row_factory,
        step: int | None = None,
        connection: Connection | None = None,
    ):
        self.row_type = row_type
        self.result_proxy = result_proxy
        self._conn = connection
        self._closed = False
        if result_proxy is None:
            self.keys: list[str] = []
            self._iter: Iterator[Row[Any]] = iter([])
        else:
            try:
                self.keys = list(result_proxy.keys())
                self._iter = iter_result_proxy(result_proxy, step=step)
            except ResourceClosedError:
                self.keys = []
                self._iter = iter([])

    def __next__(self) -> OutRow:
        try:
            row = next(self._iter)
        except StopIteration:
            self._close_safely()
            raise
        except Exception:
            self._close_safely()
            raise
        try:
            return convert_row(self.row_type, row)
        except Exception:
            # Release resources, but never let a cleanup failure mask the
            # original conversion error.
            self._close_safely()
            raise

    next = __next__

    def __iter__(self) -> Iterator[OutRow]:
        return self

    def __enter__(self) -> "ResultIter":
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc_value: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        self.close()

    def _close_safely(self) -> None:
        """Close without raising, logging cleanup failures instead."""
        try:
            self.close()
        except Exception:
            log.warning("Failed to close result iterator cleanly", exc_info=True)

    def close(self) -> None:
        """Release the cursor and any connection owned by this iterator.

        Only the result proxy and a connection explicitly handed to this
        iterator are released; shared connections and their transactions
        are left untouched. This method is idempotent.
        """
        if self._closed:
            return
        self._closed = True
        result_proxy, self.result_proxy = self.result_proxy, None
        conn, self._conn = self._conn, None
        try:
            if result_proxy is not None:
                result_proxy.close()
        finally:
            if conn is not None:
                conn.close()


def ensure_strings(value: str | Iterable[str] | None) -> list[str]:
    """Normalize a string-or-list-of-strings argument to a list."""
    if value is None:
        return []
    if isinstance(value, str):
        return [value]
    return list(value)


def normalize_column_name(name: str) -> str:
    """Check if a string is a reasonable thing to use as a column name."""
    if not isinstance(name, str):
        raise ValueError(f"{name!r} is not a valid column name.")

    # limit to 63 characters
    name = name.strip()[:63]
    # column names can be 63 *bytes* max in postgresql
    if isinstance(name, str):
        while len(name.encode("utf-8")) >= 64:
            name = name[: len(name) - 1]

    if not len(name) or "." in name or "-" in name:
        raise ValueError(f"{name!r} is not a valid column name.")
    return name


def normalize_column_key(name: str | None) -> str | None:
    """Return a comparable column name."""
    if name is None or not isinstance(name, str):
        return None
    return name.upper().strip().replace(" ", "")


def normalize_table_name(name: str) -> str:
    """Check if the table name is obviously invalid."""
    if not isinstance(name, str):
        raise ValueError(f"Invalid table name: {name!r}")
    name = name.strip()[:63]
    if not len(name):
        raise ValueError(f"Invalid table name: {name!r}")
    return name


def safe_url(url: str) -> str:
    """Remove password from printed connection URLs."""
    parsed = urlparse(url)
    if parsed.password is not None:
        pwd = f":{parsed.password}@"
        url = url.replace(pwd, ":*****@")
    return url


def index_name(table: str, columns: list[str]) -> str:
    """Generate an artificial index name."""
    sig = "||".join(columns)
    key = sha1(sig.encode("utf-8")).hexdigest()[:16]
    return f"ix_{table}_{key}"


def pad_chunk_columns(
    chunk: list[MutableRow], columns: Iterable[str]
) -> list[MutableRow]:
    """Given a set of items to be inserted, make sure they all have the
    same columns by padding columns with None if they are missing."""
    for record in chunk:
        for column in columns:
            record.setdefault(column, None)
    return chunk
