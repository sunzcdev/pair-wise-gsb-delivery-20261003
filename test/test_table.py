from datetime import datetime

import pytest
from sqlalchemy.exc import ArgumentError
from sqlalchemy.types import BIGINT, TEXT

from dataset import DatasetError, MissingKeyError, chunked

from .conftest import TEST_CITY_1, TEST_DATA


def test_insert(table):
    assert len(table) == len(TEST_DATA), len(table)
    last_id = table.insert(
        {"date": datetime(2011, 1, 2), "temperature": -10, "place": "Berlin"}
    )
    assert len(table) == len(TEST_DATA) + 1, len(table)
    assert table.find_one(id=last_id)["place"] == "Berlin"


def test_insert_ignore(table):
    table.insert_ignore(
        {"date": datetime(2011, 1, 2), "temperature": -10, "place": "Berlin"},
        ["place"],
    )
    assert len(table) == len(TEST_DATA) + 1, len(table)
    table.insert_ignore(
        {"date": datetime(2011, 1, 2), "temperature": -10, "place": "Berlin"},
        ["place"],
    )
    assert len(table) == len(TEST_DATA) + 1, len(table)


def test_insert_ignore_all_key(table):
    for _i in range(0, 4):
        table.insert_ignore(
            {"date": datetime(2011, 1, 2), "temperature": -10, "place": "Berlin"},
            ["date", "temperature", "place"],
        )
    assert len(table) == len(TEST_DATA) + 1, len(table)


def test_insert_json(table):
    last_id = table.insert(
        {
            "date": datetime(2011, 1, 2),
            "temperature": -10,
            "place": "Berlin",
            "info": {
                "currency": "EUR",
                "language": "German",
                "population": 3292365,
            },
        }
    )
    assert len(table) == len(TEST_DATA) + 1, len(table)
    assert table.find_one(id=last_id)["place"] == "Berlin"


def test_upsert(table):
    table.upsert(
        {"date": datetime(2011, 1, 2), "temperature": -10, "place": "Berlin"},
        ["place"],
    )
    assert len(table) == len(TEST_DATA) + 1, len(table)
    table.upsert(
        {"date": datetime(2011, 1, 2), "temperature": -10, "place": "Berlin"},
        ["place"],
    )
    assert len(table) == len(TEST_DATA) + 1, len(table)


def test_upsert_single_column(db):
    table = db["banana_single_col"]
    table.upsert({"color": "Yellow"}, ["color"])
    assert len(table) == 1, len(table)
    table.upsert({"color": "Yellow"}, ["color"])
    assert len(table) == 1, len(table)


def test_upsert_all_key(table):
    assert len(table) == len(TEST_DATA), len(table)
    for _i in range(0, 2):
        table.upsert(
            {"date": datetime(2011, 1, 2), "temperature": -10, "place": "Berlin"},
            ["date", "temperature", "place"],
        )
    assert len(table) == len(TEST_DATA) + 1, len(table)


def test_upsert_id(db):
    table = db["banana_with_id"]
    data = {"id": 10, "title": "I am a banana!"}
    table.upsert(data, ["id"])
    assert len(table) == 1, len(table)


def test_update_while_iter(table):
    for row in table:
        row["foo"] = "bar"
        table.update(row, ["place", "date"])
    assert len(table) == len(TEST_DATA), len(table)


def test_weird_column_names(table):
    with pytest.raises(ValueError):
        table.insert(
            {
                "date": datetime(2011, 1, 2),
                "temperature": -10,
                "foo.bar": "Berlin",
                "qux.bar": "Huhu",
            }
        )


def test_cased_column_names(db):
    tbl = db["cased_column_names"]
    tbl.insert({"place": "Berlin"})
    tbl.insert({"Place": "Berlin"})
    tbl.insert({"PLACE ": "Berlin"})
    assert len(tbl.columns) == 2, tbl.columns
    assert len(list(tbl.find(Place="Berlin"))) == 3
    assert len(list(tbl.find(place="Berlin"))) == 3
    assert len(list(tbl.find(PLACE="Berlin"))) == 3


def test_invalid_column_names(db):
    tbl = db["weather"]
    with pytest.raises(ValueError):
        tbl.insert({None: "banana"})

    with pytest.raises(ValueError):
        tbl.insert({"": "banana"})

    with pytest.raises(ValueError):
        tbl.insert({"-": "banana"})


def test_delete(table):
    table.insert({"date": datetime(2011, 1, 2), "temperature": -10, "place": "Berlin"})
    original_count = len(table)
    assert len(table) == len(TEST_DATA) + 1, len(table)
    # Test bad use of API
    with pytest.raises(ArgumentError):
        table.delete({"place": "Berlin"})
    assert len(table) == original_count, len(table)

    assert table.delete(place="Berlin") is True, "should return 1"
    assert len(table) == len(TEST_DATA), len(table)
    assert table.delete() is True, "should return non zero"
    assert len(table) == 0, len(table)


def test_repr(table):
    assert repr(table) == "<Table(weather)>", (
        "the representation should be <Table(weather)>"
    )


def test_delete_nonexist_entry(table):
    assert table.delete(place="Berlin") is False, (
        "entry not exist, should fail to delete"
    )


def test_find_one(table):
    table.insert({"date": datetime(2011, 1, 2), "temperature": -10, "place": "Berlin"})
    d = table.find_one(place="Berlin")
    assert d["temperature"] == -10, d
    d = table.find_one(place="Atlantis")
    assert d is None, d


def test_count(table):
    assert len(table) == 6, len(table)
    length = table.count(place=TEST_CITY_1)
    assert length == 3, length


def test_find(table):
    ds = list(table.find(place=TEST_CITY_1))
    assert len(ds) == 3, ds
    ds = list(table.find(place=TEST_CITY_1, _limit=2))
    assert len(ds) == 2, ds
    ds = list(table.find(place=TEST_CITY_1, _limit=2, _step=1))
    assert len(ds) == 2, ds
    ds = list(table.find(place=TEST_CITY_1, _limit=1, _step=2))
    assert len(ds) == 1, ds
    ds = list(table.find(_step=2))
    assert len(ds) == len(TEST_DATA), ds
    ds = list(table.find(order_by=["temperature"]))
    assert ds[0]["temperature"] == -1, ds
    ds = list(table.find(order_by=["-temperature"]))
    assert ds[0]["temperature"] == 8, ds
    ds = list(table.find(table.table.columns.temperature > 4))
    assert len(ds) == 3, ds


def test_find_dsl(table):
    ds = list(table.find(place={"like": "%lw%"}))
    assert len(ds) == 3, ds
    ds = list(table.find(temperature={">": 5}))
    assert len(ds) == 2, ds
    ds = list(table.find(temperature={">=": 5}))
    assert len(ds) == 3, ds
    ds = list(table.find(temperature={"<": 0}))
    assert len(ds) == 1, ds
    ds = list(table.find(temperature={"<=": 0}))
    assert len(ds) == 2, ds
    ds = list(table.find(temperature={"!=": -1}))
    assert len(ds) == 5, ds
    ds = list(table.find(temperature={"between": [5, 8]}))
    assert len(ds) == 3, ds
    ds = list(table.find(place={"=": "G€lway"}))
    assert len(ds) == 3, ds
    ds = list(table.find(place={"ilike": "%LwAy"}))
    assert len(ds) == 3, ds


def test_startswith_endswith(db):
    t = db["prefix_test"]
    t.insert({"org": "acme"})
    t.insert({"org": "acme_labs"})
    t.insert({"org": "other"})
    t.insert({"org": "admin"})

    # Basic startswith
    rows = list(t.find(org={"startswith": "acme"}))
    assert len(rows) == 2, rows

    # Basic endswith
    rows = list(t.find(org={"endswith": "labs"}))
    assert len(rows) == 1, rows

    # LIKE metacharacters must be escaped (CVE-style regression test)
    rows = list(t.find(org={"startswith": "%"}))
    assert len(rows) == 0, f"startswith % should match nothing, got {rows}"

    rows = list(t.find(org={"startswith": "_"}))
    assert len(rows) == 0, f"startswith _ should match nothing, got {rows}"

    rows = list(t.find(org={"endswith": "%"}))
    assert len(rows) == 0, f"endswith % should match nothing, got {rows}"

    # Delete must not overmatch
    t.delete(org={"startswith": "%"})
    assert t.count() == 4, "delete with % startswith should not remove rows"


def test_offset(table):
    ds = list(table.find(place=TEST_CITY_1, _offset=1))
    assert len(ds) == 2, ds
    ds = list(table.find(place=TEST_CITY_1, _limit=2, _offset=2))
    assert len(ds) == 1, ds


def test_streamed_update(table):
    ds = list(table.find(place=TEST_CITY_1, _streamed=True, _step=1))
    assert len(ds) == 3, len(ds)
    for row in table.find(place=TEST_CITY_1, _streamed=True, _step=1):
        row["temperature"] = -1
        table.update(row, ["id"])


def test_distinct(table):
    x = list(table.distinct("place"))
    assert len(x) == 2, x
    x = list(table.distinct("place", "date"))
    assert len(x) == 6, x
    x = list(
        table.distinct(
            "place",
            "date",
            table.table.columns.date >= datetime(2011, 1, 2, 0, 0),
        )
    )
    assert len(x) == 4, x

    x = list(table.distinct("temperature", place="B€rkeley"))
    assert len(x) == 3, x
    x = list(table.distinct("temperature", place=["B€rkeley", "G€lway"]))
    assert len(x) == 6, x


def test_insert_many(table):
    data = TEST_DATA * 100
    table.insert_many(data, chunk_size=13)
    assert len(table) == len(data) + 6, (len(table), len(data))


def test_chunked_insert(table):
    data = TEST_DATA * 100
    with chunked.ChunkedInsert(table) as chunk_tbl:
        for item in data:
            chunk_tbl.insert(item)
    assert len(table) == len(data) + 6, (len(table), len(data))


def test_chunked_insert_callback(table):
    data = TEST_DATA * 100
    n_items = 0

    def callback(queue):
        nonlocal n_items
        n_items += len(queue)

    with chunked.ChunkedInsert(table, callback=callback) as chunk_tbl:
        for item in data:
            chunk_tbl.insert(item)
    assert len(data) == n_items
    assert len(table) == len(data) + 6


def test_update_many(db):
    tbl = db["update_many_test"]
    tbl.insert_many([{"temp": 10}, {"temp": 20}, {"temp": 30}])
    tbl.update_many([{"id": 1, "temp": 50}, {"id": 3, "temp": 50}], "id")

    # Ensure data has been updated.
    assert tbl.find_one(id=1)["temp"] == tbl.find_one(id=3)["temp"]


def test_chunked_update(db):
    tbl = db["update_many_test"]
    tbl.insert_many(
        [
            {"temp": 10, "location": "asdf"},
            {"temp": 20, "location": "qwer"},
            {"temp": 30, "location": "asdf"},
        ]
    )

    chunked_tbl = chunked.ChunkedUpdate(tbl, ["id"])
    chunked_tbl.update({"id": 1, "temp": 50})
    chunked_tbl.update({"id": 2, "location": "asdf"})
    chunked_tbl.update({"id": 3, "temp": 50})
    chunked_tbl.flush()

    # Ensure data has been updated.
    assert tbl.find_one(id=1)["temp"] == tbl.find_one(id=3)["temp"] == 50
    assert tbl.find_one(id=2)["location"] == tbl.find_one(id=3)["location"] == "asdf"


def test_upsert_many(db):
    # Also tests updating on records with different attributes
    tbl = db["upsert_many_test"]

    weight = 100
    tbl.upsert_many([{"age": 10}, {"weight": weight}], "id")
    assert tbl.find_one(id=1)["age"] == 10

    tbl.upsert_many([{"id": 1, "age": 70}, {"id": 2, "weight": weight / 2}], "id")
    assert tbl.find_one(id=2)["weight"] == weight / 2


def test_drop_operations(table):
    assert table._table is not None, "table shouldn't be dropped yet"
    table.drop()
    assert table._table is None, "table should be dropped now"
    assert list(table.all()) == [], table.all()
    assert table.count() == 0, table.count()


def test_table_drop(db, table):
    assert "weather" in db
    db["weather"].drop()
    assert "weather" not in db


def test_table_drop_then_create(db, table):
    assert "weather" in db
    db["weather"].drop()
    assert "weather" not in db
    db["weather"].insert({"foo": "bar"})


def test_columns(table):
    cols = table.columns
    assert len(list(cols)) == 4, "column count mismatch"
    assert "date" in cols and "temperature" in cols and "place" in cols


def test_drop_column(table):
    try:
        table.drop_column("date")
        assert "date" not in table.columns
    except RuntimeError:
        pass


def test_iter(table):
    c = 0
    for _row in table:
        c += 1
    assert c == len(table)


def test_update(table):
    date = datetime(2011, 1, 2)
    res = table.update(
        {"date": date, "temperature": -10, "place": TEST_CITY_1}, ["place", "date"]
    )
    assert res, "update should return True"
    m = table.find_one(place=TEST_CITY_1, date=date)
    assert m["temperature"] == -10, (
        f"new temp. should be -10 but is {m['temperature']}"
    )


def test_create_column(db, table):
    flt = db.types.float
    table.create_column("foo", flt)
    assert "foo" in table.table.c, table.table.c
    assert isinstance(table.table.c["foo"].type, flt), table.table.c["foo"].type
    assert "foo" in table.columns, table.columns


def test_ensure_column(db, table):
    flt = db.types.float
    table.create_column_by_example("foo", 0.1)
    assert "foo" in table.table.c, table.table.c
    assert isinstance(table.table.c["foo"].type, flt), table.table.c["foo"].type
    table.create_column_by_example("bar", 1)
    assert "bar" in table.table.c, table.table.c
    assert isinstance(table.table.c["bar"].type, BIGINT), table.table.c["bar"].type
    table.create_column_by_example("pippo", "test")
    assert "pippo" in table.table.c, table.table.c
    assert isinstance(table.table.c["pippo"].type, TEXT), table.table.c["pippo"].type
    table.create_column_by_example("bigbar", 11111111111)
    assert "bigbar" in table.table.c, table.table.c
    assert isinstance(table.table.c["bigbar"].type, BIGINT), table.table.c[
        "bigbar"
    ].type
    table.create_column_by_example("littlebar", -11111111111)
    assert "littlebar" in table.table.c, table.table.c
    assert isinstance(table.table.c["littlebar"].type, BIGINT), table.table.c[
        "littlebar"
    ].type


def test_key_order(db, table):
    res = db.query("SELECT temperature, place FROM weather LIMIT 1")
    keys = list(res.next().keys())
    assert keys[0] == "temperature"
    assert keys[1] == "place"


def test_empty_query(table):
    empty = list(table.find(place="not in data"))
    assert len(empty) == 0, empty


# ---------------------------------------------------------------------------
# Strict locating-key mode
# ---------------------------------------------------------------------------


def test_strict_update_missing_key_raises(table):
    # The row has no "place" key at all: strict mode must refuse rather than
    # treating it as a NULL filter.
    with pytest.raises(MissingKeyError):
        table.update({"temperature": 99}, ["place"], strict=True)
    # Nothing was written.
    assert (
        table.find_one(place=TEST_CITY_1, date=datetime(2011, 1, 1))["temperature"] == 6
    )


def test_strict_update_explicit_none_matches_nulls(db):
    tbl = db["strict_nulls"]
    tbl.insert_many(
        [
            {"group": "a", "place": None, "temperature": 1},
            {"group": "a", "place": None, "temperature": 2},
            {"group": "a", "place": "Berlin", "temperature": 3},
            {"group": "b", "place": None, "temperature": 4},
        ]
    )
    # Explicit None is allowed and must only touch rows that are actually
    # NULL for *both* parts of the composite key.
    result = tbl.update(
        {"group": "a", "place": None, "temperature": 0},
        ["group", "place"],
        strict=True,
    )
    assert result == 2
    rows = list(tbl.find(group="a", place=None))
    assert {row["temperature"] for row in rows} == {0}
    # The non-NULL row and the other group are untouched.
    assert tbl.find_one(group="a", place="Berlin")["temperature"] == 3
    assert tbl.find_one(group="b", place=None)["temperature"] == 4


def test_lenient_update_missing_key_matches_nulls(db):
    # Documents the legacy behavior: a missing key behaves like an explicit
    # NULL filter and can overwrite unrelated NULL-keyed rows.
    tbl = db["lenient_nulls"]
    tbl.insert_many(
        [
            {"group": "a", "place": None, "temperature": 1},
            {"group": "b", "place": None, "temperature": 2},
        ]
    )
    # "place" is missing from the row -> lenient mode treats it as None.
    result = tbl.update({"group": "a", "temperature": 0}, ["group", "place"])
    assert result == 1
    # Only the (a, NULL) row is hit; the (b, NULL) row is protected by group.
    assert tbl.find_one(group="a", place=None)["temperature"] == 0
    assert tbl.find_one(group="b", place=None)["temperature"] == 2

    # With a single key that is missing, every NULL-keyed row is in scope.
    tbl2 = db["lenient_nulls_single"]
    tbl2.insert_many([{"place": None, "v": 1}, {"place": None, "v": 2}])
    result = tbl2.update({"v": 9}, ["place"])
    assert result == 2
    assert {row["v"] for row in tbl2.find()} == {9}

    # The same call is rejected in strict mode.
    with pytest.raises(MissingKeyError):
        tbl2.update({"v": 10}, ["place"], strict=True)
    assert {row["v"] for row in tbl2.find()} == {9}


def test_strict_update_nonexistent_key_column(db):
    tbl = db["strict_bad_column"]
    tbl.insert_many([{"place": "Berlin", "v": 1}])
    with pytest.raises(DatasetError):
        tbl.update({"v": 2}, ["does_not_exist"], strict=True)
    # No data change, and strict mode did not auto-create the column.
    assert "does_not_exist" not in tbl.columns
    assert tbl.find_one(place="Berlin")["v"] == 1


def test_lenient_update_nonexistent_key_column(db):
    tbl = db["lenient_bad_column"]
    tbl.insert_many([{"place": "Berlin", "v": 1}])
    # Lenient mode builds an impossible filter and matches nothing, without
    # raising.
    result = tbl.update({"v": 2}, ["does_not_exist"])
    assert result == 0
    assert tbl.find_one(place="Berlin")["v"] == 1


def test_strict_update_composite_key_checked_item_by_item(table):
    # "date" is present but "place" is missing: every component is checked.
    with pytest.raises(MissingKeyError):
        table.update(
            {"date": datetime(2011, 1, 1), "temperature": 99},
            ["place", "date"],
            strict=True,
        )
    unchanged = table.find_one(place=TEST_CITY_1, date=datetime(2011, 1, 1))
    assert unchanged["temperature"] == 6


def test_strict_rejects_normalized_key_collision(table):
    with pytest.raises(DatasetError):
        table.update(
            {"place": "Berlin", "PLACE": "Berlin", "temperature": 1},
            ["place", "PLACE"],
            strict=True,
        )
    # No write happened.
    assert (
        table.find_one(place=TEST_CITY_1, date=datetime(2011, 1, 1))["temperature"] == 6
    )


def test_strict_update_accepts_case_insensitive_row_keys(table):
    target_date = datetime(2011, 1, 1)
    result = table.update(
        {"PLACE": TEST_CITY_1, "Date": target_date, "temperature": 42},
        ["place", "date"],
        strict=True,
    )
    assert result == 1
    assert table.find_one(place=TEST_CITY_1, date=target_date)["temperature"] == 42


def test_strict_upsert_missing_key_raises(db):
    tbl = db["strict_upsert"]
    tbl.insert_many([{"place": None, "v": 1}])
    with pytest.raises(MissingKeyError):
        tbl.upsert({"v": 2}, ["place"], strict=True)
    assert tbl.find_one(place=None)["v"] == 1


def test_strict_upsert_explicit_none_updates_null_row(db):
    tbl = db["strict_upsert_nulls"]
    tbl.insert_many([{"place": None, "v": 1}, {"place": "Berlin", "v": 2}])
    tbl.upsert({"place": None, "v": 9}, ["place"], strict=True)
    assert len(tbl) == 2
    assert tbl.find_one(place=None)["v"] == 9
    assert tbl.find_one(place="Berlin")["v"] == 2


def test_strict_upsert_inserts_when_no_match(db):
    tbl = db["strict_upsert_insert"]
    tbl.insert_many([{"place": "Berlin", "v": 1}])
    tbl.upsert({"place": "Galway", "v": 2}, ["place"], strict=True)
    assert len(tbl) == 2
    assert tbl.find_one(place="Galway")["v"] == 2


def test_strict_update_many_whole_batch_validated(db):
    tbl = db["strict_update_many"]
    tbl.insert_many([{"id": 1, "v": 1}, {"id": 2, "v": 2}, {"id": 3, "v": 3}])
    rows = [
        {"id": 1, "v": 10},
        {"v": 20},  # missing "id": invalid even though it is not the first row
        {"id": 3, "v": 30},
    ]
    with pytest.raises(MissingKeyError):
        tbl.update_many(rows, "id", chunk_size=1, strict=True)
    # chunk_size=1 would have flushed row 0 first in a naive implementation;
    # upfront validation guarantees nothing was written.
    assert tbl.find_one(id=1)["v"] == 1
    assert tbl.find_one(id=2)["v"] == 2
    assert tbl.find_one(id=3)["v"] == 3


def test_strict_update_many_explicit_none_scope(db):
    tbl = db["strict_update_many_nulls"]
    tbl.insert_many(
        [
            {"group": "a", "place": None, "v": 1},
            {"group": "a", "place": None, "v": 2},
            {"group": "a", "place": "Berlin", "v": 3},
        ]
    )
    tbl.update_many(
        [
            {"group": "a", "place": None, "v": 0},
        ],
        ["group", "place"],
        strict=True,
    )
    assert {row["v"] for row in tbl.find(group="a", place=None)} == {0}
    assert tbl.find_one(place="Berlin")["v"] == 3


def test_strict_update_many_happy_path(db):
    tbl = db["strict_update_many_ok"]
    tbl.insert_many([{"id": 1, "v": 1}, {"id": 2, "v": 2}])
    tbl.update_many([{"ID": 1, "v": 10}, {"id": 2, "v": 20}], "id", strict=True)
    assert tbl.find_one(id=1)["v"] == 10
    assert tbl.find_one(id=2)["v"] == 20


def test_strict_update_many_missing_chunk_boundary(db):
    # A bad row behind the chunk boundary must still abort the whole batch.
    tbl = db["strict_update_many_boundary"]
    tbl.insert_many([{"id": i, "v": i} for i in range(1, 6)])
    rows = [{"id": i, "v": i * 10} for i in range(1, 5)]
    rows.append({"v": 50})  # row index 4 has no id
    with pytest.raises(MissingKeyError):
        tbl.update_many(rows, "id", chunk_size=2, strict=True)
    for i in range(1, 6):
        assert tbl.find_one(id=i)["v"] == i


def test_strict_upsert_many_batch_rejected(db):
    tbl = db["strict_upsert_many"]
    tbl.insert_many([{"id": 1, "v": 1}])
    with pytest.raises(MissingKeyError):
        tbl.upsert_many(
            [{"id": 1, "v": 10}, {"v": 20}], "id", chunk_size=1, strict=True
        )
    assert tbl.find_one(id=1)["v"] == 1
    assert len(tbl) == 1


def test_strict_insert_ignore_missing_key_raises(table):
    with pytest.raises(MissingKeyError):
        table.insert_ignore(
            {"temperature": 1, "date": datetime(2011, 1, 1)},
            ["place", "date"],
            strict=True,
        )


def test_strict_insert_ignore_explicit_none(db):
    tbl = db["strict_insert_ignore_nulls"]
    tbl.insert_many([{"place": None, "v": 1}])
    # Matches the existing NULL row, so nothing is inserted.
    result = tbl.insert_ignore({"place": None, "v": 2}, ["place"], strict=True)
    assert result is False
    assert len(tbl) == 1
    # A new non-NULL value does not match, so it gets inserted.
    result = tbl.insert_ignore({"place": "Berlin", "v": 3}, ["place"], strict=True)
    assert result is not False
    assert len(tbl) == 2


def test_strict_chunked_update_rejects_missing_key(db):
    tbl = db["strict_chunked"]
    tbl.insert_many([{"id": 1, "v": 1}, {"id": 2, "v": 2}])
    # With a chunk size larger than the queue, flush() validates both queued
    # rows together before any statement is executed.
    with (
        pytest.raises(MissingKeyError),
        chunked.ChunkedUpdate(tbl, ["id"], chunksize=100, strict=True) as upd,
    ):
        upd.update({"id": 1, "v": 10})
        upd.update({"v": 20})
    assert tbl.find_one(id=1)["v"] == 1
    assert tbl.find_one(id=2)["v"] == 2


def test_strict_chunked_update_happy_path(db):
    tbl = db["strict_chunked_ok"]
    tbl.insert_many([{"id": 1, "v": 1}, {"id": 2, "v": 2}])
    with chunked.ChunkedUpdate(tbl, ["id"], strict=True) as upd:
        upd.update({"id": 1, "v": 10})
        upd.update({"id": 2, "v": 20})
    assert tbl.find_one(id=1)["v"] == 10
    assert tbl.find_one(id=2)["v"] == 20


def test_strict_update_validates_before_schema_change(db):
    # A fresh table does not exist yet; strict mode must fail validation
    # instead of auto-creating it.
    tbl = db["strict_no_table"]
    with pytest.raises(DatasetError):
        tbl.update({"id": 1, "v": 1}, ["id"], strict=True)
    assert not tbl.exists


def test_strict_update_no_schema_change_on_missing_key(db):
    tbl = db["strict_no_schema_change"]
    tbl.insert_many([{"id": 1, "v": 1}])
    with pytest.raises(MissingKeyError):
        tbl.update({"new_col": 2, "v": 3}, ["id"], ensure=True, strict=True)
    # The failed strict call must not have created "new_col".
    assert "new_col" not in tbl.columns
    assert tbl.find_one(id=1)["v"] == 1


def test_strict_update_inside_transaction_commits_and_rolls_back(db):
    tbl = db["strict_tx"]
    tbl.insert_many([{"id": 1, "v": 1}, {"id": 2, "v": 2}])
    with db:
        tbl.update_many([{"id": 1, "v": 10}, {"id": 2, "v": 20}], "id", strict=True)
    assert tbl.find_one(id=1)["v"] == 10
    assert tbl.find_one(id=2)["v"] == 20

    try:
        with db:
            tbl.update({"id": 1, "v": 30}, ["id"], strict=True)
            raise RuntimeError("boom")
    except RuntimeError:
        pass
    assert tbl.find_one(id=1)["v"] == 10


def test_missing_key_error_is_dataset_error():
    assert issubclass(MissingKeyError, DatasetError)
    assert issubclass(MissingKeyError, Exception)
