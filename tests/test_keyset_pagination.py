import os
import tempfile

import pytest

from sqlite_utils import Database
from sqlite_utils.db import (
    InvalidColumns,
    InvalidCursor,
    KeysetCursor,
    PrimaryKeyRequired,
)


@pytest.fixture
def file_db_path():
    # A workspace-local temp directory keeps file/WAL tests working in
    # sandboxes where the platform temp location is not writable. Cleanup is
    # best-effort so an environment that forbids removal does not fail tests.
    directory = os.path.join(os.getcwd(), ".keyset_pagination_tmp")
    os.makedirs(directory, exist_ok=True)
    fd, path = tempfile.mkstemp(suffix=".db", dir=directory)
    os.close(fd)
    os.remove(path)
    yield path
    for suffix in ("", "-wal", "-shm"):
        try:
            os.remove(path + suffix)
        except OSError:
            pass


@pytest.fixture
def scored_db(fresh_db):
    fresh_db.table("dogs").insert_all(
        [
            {"id": 1, "grp": "b", "score": None},
            {"id": 2, "grp": "a", "score": 10},
            {"id": 3, "grp": "a", "score": None},
            {"id": 4, "grp": "a", "score": 10},
            {"id": 5, "grp": "c", "score": 5},
            {"id": 6, "grp": "a", "score": 2},
        ],
        pk="id",
    )
    return fresh_db


def _reference_ids(db, sort_spec):
    order_terms = []
    for column, direction in sort_spec:
        quoted = f'"{column}"'
        null_term = f"{quoted} is null"
        if direction == "desc":
            null_term += " desc"
        order_terms.append(f"{null_term}, {quoted} {direction}")
    sql = f"select id from dogs order by {', '.join(order_terms)}"
    return [row[0] for row in db.execute(sql).fetchall()]


@pytest.mark.parametrize(
    "sort_spec",
    [
        [("id", "asc")],
        [("id", "desc")],
        [("score", "asc")],
        [("score", "desc")],
        [("score", "asc"), ("grp", "asc")],
        [("grp", "asc"), ("score", "desc")],
        [("score", "desc"), ("grp", "desc"), ("id", "asc")],
    ],
)
@pytest.mark.parametrize("page_size", [1, 2, 3, 5, 100])
def test_keyset_pages_match_reference_order(scored_db, sort_spec, page_size):
    paginator = scored_db.table("dogs").keyset_pages(sort_spec, page_size=page_size)
    pages = list(paginator)
    # pages never exceed the requested size and the final page can be short
    assert all(len(page) <= page_size for page in pages)
    flattened = [row["id"] for page in pages for row in page]
    assert flattened == _reference_ids(scored_db, paginator.sort)
    # every row appears exactly once - no skips or duplicates
    assert sorted(flattened) == [1, 2, 3, 4, 5, 6]


def test_rows_helper_flattens_pages(scored_db):
    paginator = scored_db.table("dogs").keyset_pages([("id", "asc")], page_size=2)
    assert [row["id"] for row in paginator.rows()] == [1, 2, 3, 4, 5, 6]


def test_exact_multiple_of_page_size_does_not_drop_final_page(fresh_db):
    fresh_db.table("nums").insert_all(
        ({"id": i} for i in range(1, 7)), pk="id"
    )
    paginator = fresh_db.table("nums").keyset_pages([("id", "asc")], page_size=3)
    pages = list(paginator)
    assert [[row["id"] for row in page] for page in pages] == [
        [1, 2, 3],
        [4, 5, 6],
    ]


def test_string_sort_specifications(scored_db):
    paginator = scored_db.table("dogs").keyset_pages(
        ["score desc", "grp", "id asc"], page_size=2
    )
    assert paginator.sort == (
        ("score", "desc"),
        ("grp", "asc"),
        ("id", "asc"),
    )
    flattened = [row["id"] for page in paginator for row in page]
    assert flattened == _reference_ids(scored_db, paginator.sort)


def test_single_column_pk_is_used_as_tiebreaker(scored_db):
    paginator = scored_db.table("dogs").keyset_pages([("grp", "asc")])
    assert paginator.sort[-1] == ("id", "asc")


def test_explicit_unique_column_appended_and_deduplicated(scored_db):
    paginator = scored_db.table("dogs").keyset_pages(
        [("grp", "asc"), ("id", "desc")], unique_column="id"
    )
    assert paginator.sort == (("grp", "asc"), ("id", "desc"))


def test_table_without_single_pk_requires_unique_column(fresh_db):
    fresh_db.execute("create table no_pk (a text, b text)")
    fresh_db.table("no_pk").insert_all(
        [{"a": "x", "b": "1"}, {"a": "x", "b": "2"}]
    )
    with pytest.raises(PrimaryKeyRequired):
        fresh_db.table("no_pk").keyset_pages([("a", "asc")])
    rows = list(
        fresh_db.table("no_pk")
        .keyset_pages([("a", "asc")], unique_column="b")
        .rows()
    )
    assert [row["b"] for row in rows] == ["1", "2"]


def test_unknown_sort_column_raises(scored_db):
    with pytest.raises(InvalidColumns):
        scored_db.table("dogs").keyset_pages([("nope", "asc")])


def test_duplicate_sort_column_raises(scored_db):
    with pytest.raises(ValueError):
        scored_db.table("dogs").keyset_pages([("id", "asc"), ("id", "desc")])


def test_invalid_direction_raises(scored_db):
    with pytest.raises(ValueError):
        scored_db.table("dogs").keyset_pages([("id", "sideways")])


def test_page_size_must_be_positive(scored_db):
    with pytest.raises(ValueError):
        scored_db.table("dogs").keyset_pages([("id", "asc")], page_size=0)


def test_numeric_and_numeric_looking_text_do_not_collide(fresh_db):
    # "10" (text), 2 (integer) and 9 (integer) must compare by storage type,
    # not by a concatenated string representation
    fresh_db.table("mixed").insert_all(
        [
            {"id": 1, "v": "10"},
            {"id": 2, "v": 2},
            {"id": 3, "v": 9},
        ],
        pk="id",
    )
    paginator = fresh_db.table("mixed").keyset_pages([("v", "asc")], page_size=2)
    got = [row["id"] for row in paginator.rows()]
    expected = [
        row[0]
        for row in fresh_db.execute(
            'select id from mixed order by "v" is null, "v" asc, "id" asc'
        ).fetchall()
    ]
    assert got == expected


def test_null_position_is_explicit(scored_db):
    ascending = [
        row["id"]
        for row in scored_db.table("dogs")
        .keyset_pages([("score", "asc")], page_size=2)
        .rows()
    ]
    # NULLs come last in ascending order
    assert ascending[-2:] == [1, 3]
    descending = [
        row["id"]
        for row in scored_db.table("dogs")
        .keyset_pages([("score", "desc")], page_size=2)
        .rows()
    ]
    # NULLs come first in descending order
    assert descending[:2] == [1, 3]


def test_cursor_round_trip_and_resume(scored_db):
    first = scored_db.table("dogs").keyset_pages([("id", "asc")], page_size=2)
    first_pages = []
    for page in first:
        first_pages.append([row["id"] for row in page])
        break
    cursor = first.cursor
    assert isinstance(cursor, KeysetCursor)
    encoded = cursor.encode()
    assert isinstance(encoded, str)
    assert KeysetCursor.decode(encoded) == cursor

    # resume from the encoded cursor and confirm the remaining rows line up
    resumed = scored_db.table("dogs").keyset_pages(
        [("id", "asc")], page_size=2, cursor=encoded
    )
    resumed_ids = [row["id"] for page in resumed for row in page]
    full = [
        row["id"]
        for row in scored_db.table("dogs")
        .keyset_pages([("id", "asc")], page_size=2)
        .rows()
    ]
    assert first_pages[0] + resumed_ids == full


def test_cursor_rejected_for_different_sort(scored_db):
    paginator = scored_db.table("dogs").keyset_pages([("id", "asc")], page_size=2)
    list(paginator.rows())
    with pytest.raises(InvalidCursor):
        scored_db.table("dogs").keyset_pages(
            [("score", "asc")], page_size=2, cursor=paginator.cursor
        )


def test_cursor_rejected_for_different_table(scored_db):
    paginator = scored_db.table("dogs").keyset_pages([("id", "asc")], page_size=2)
    list(paginator.rows())
    scored_db.table("cats").insert({"id": 1}, pk="id")
    with pytest.raises(InvalidCursor):
        scored_db.table("cats").keyset_pages(
            [("id", "asc")], page_size=2, cursor=paginator.cursor
        )


def test_malformed_cursor_rejected(scored_db):
    with pytest.raises(InvalidCursor):
        KeysetCursor.decode("definitely-not-a-cursor")


def test_snapshot_holds_consistent_view_across_pages(file_db_path):
    path = file_db_path
    db = Database(path)
    db.enable_wal()
    db.table("t").insert_all(
        ({"id": i, "v": i} for i in range(1, 7)), pk="id"
    )
    other = Database(path)

    paginator = db.table("t").keyset_pages(
        [("id", "asc")], page_size=2, snapshot=True
    )
    iterator = iter(paginator)
    next(iterator)
    assert db.conn.in_transaction and paginator._owns_snapshot

    # concurrent connection mutates the table between pages
    other.table("t").insert({"id": 100, "v": 100})
    other.table("t").delete(3)

    snapshot_ids = [row["id"] for page in iterator for row in page]
    # the snapshot keeps the deleted row and never sees the inserted row
    assert 3 in snapshot_ids and 100 not in snapshot_ids
    assert not db.conn.in_transaction  # snapshot released on exhaustion


def test_normal_mode_sees_concurrent_changes(file_db_path):
    path = file_db_path
    db = Database(path)
    db.enable_wal()
    db.table("t").insert_all(
        ({"id": i, "v": i} for i in range(1, 7)), pk="id"
    )
    other = Database(path)

    paginator = db.table("t").keyset_pages([("id", "asc")], page_size=2)
    iterator = iter(paginator)
    next(iterator)
    assert not db.conn.in_transaction  # no snapshot transaction is opened
    other.table("t").insert({"id": 100, "v": 100})
    other.table("t").delete(3)

    seen = [row["id"] for page in iterator for row in page]
    assert 100 in seen and 3 not in seen


def test_snapshot_inside_caller_transaction_is_not_owned(file_db_path):
    path = file_db_path
    db = Database(path)
    db.table("t").insert_all(({"id": i} for i in range(1, 5)), pk="id")
    db.begin()
    try:
        paginator = db.table("t").keyset_pages(
            [("id", "asc")], page_size=2, snapshot=True
        )
        list(paginator)
        assert paginator._owns_snapshot is False
        # the caller's transaction must still be open and untouched
        assert db.conn.in_transaction
    finally:
        db.rollback()


def test_early_close_releases_owned_snapshot(file_db_path):
    path = file_db_path
    db = Database(path)
    db.table("t").insert_all(({"id": i} for i in range(1, 7)), pk="id")
    paginator = db.table("t").keyset_pages(
        [("id", "asc")], page_size=2, snapshot=True
    )
    iterator = iter(paginator)
    next(iterator)
    assert db.conn.in_transaction
    paginator.close()
    assert not db.conn.in_transaction
    paginator.close()  # idempotent
    # iterating after close is a programmer error
    with pytest.raises(RuntimeError):
        list(paginator)


def test_context_manager_closes_snapshot(file_db_path):
    path = file_db_path
    db = Database(path)
    db.table("t").insert_all(({"id": i} for i in range(1, 5)), pk="id")
    with db.table("t").keyset_pages(
        [("id", "asc")], page_size=2, snapshot=True
    ) as paginator:
        iterator = iter(paginator)
        next(iterator)
        assert db.conn.in_transaction
    assert not db.conn.in_transaction


def test_query_error_releases_owned_snapshot(file_db_path):
    path = file_db_path
    db = Database(path)
    db.table("t").insert_all(
        ({"id": i, "v": i} for i in range(1, 7)), pk="id"
    )
    paginator = db.table("t").keyset_pages(
        [("v", "asc")], page_size=2, snapshot=True
    )
    iterator = iter(paginator)
    next(iterator)
    db.execute("alter table t rename column v to w")
    with pytest.raises(Exception):
        next(iterator)
    assert not db.conn.in_transaction
    assert paginator._closed


def test_no_offset_in_generated_sql(scored_db):
    paginator = scored_db.table("dogs").keyset_pages([("id", "asc")], page_size=2)
    with scored_db.tracer(lambda sql, params: captured.append(sql)):
        captured = []
        list(paginator)
    assert captured, "no SQL was traced"
    assert all("offset" not in sql.lower() for sql in captured)
    assert all("limit 2" in sql.lower() for sql in captured)


def test_empty_table_produces_no_pages(fresh_db):
    fresh_db.table("empty").insert({"id": 1}, pk="id")
    fresh_db.execute("delete from empty")
    paginator = fresh_db.table("empty").keyset_pages([("id", "asc")])
    assert list(paginator) == []
    assert paginator._closed


def test_resuming_from_final_cursor_returns_nothing(fresh_db):
    fresh_db.table("nums").insert_all(({"id": i} for i in range(1, 5)), pk="id")
    first = fresh_db.table("nums").keyset_pages([("id", "asc")], page_size=10)
    list(first)
    resumed = fresh_db.table("nums").keyset_pages(
        [("id", "asc")], page_size=10, cursor=first.cursor
    )
    assert list(resumed) == []


def test_bytes_values_round_trip_through_cursor(fresh_db):
    fresh_db.table("blobs").insert_all(
        [{"id": 1, "blob": b"a"}, {"id": 2, "blob": b"b"}], pk="id"
    )
    paginator = fresh_db.table("blobs").keyset_pages(
        [("blob", "asc")], page_size=1
    )
    list(paginator.rows())
    decoded = KeysetCursor.decode(paginator.cursor.encode())
    assert decoded == paginator.cursor
    assert b"b" in decoded.last_values


def test_missing_table_without_unique_column_raises(fresh_db):
    with pytest.raises(PrimaryKeyRequired):
        fresh_db.table("ghost").keyset_pages([("id", "asc")])


