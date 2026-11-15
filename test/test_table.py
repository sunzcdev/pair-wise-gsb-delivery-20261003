from datetime import datetime

import pytest
from sqlalchemy import func, select
from sqlalchemy.exc import ArgumentError
from sqlalchemy.types import BIGINT, TEXT

from dataset import chunked
from dataset.util import DatasetError, QueryError, ResultIter

from .conftest import TEST_CITY_1, TEST_CITY_2, TEST_DATA


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


def test_aggregate_returns_result_iter(table):
    results = table.aggregate([("count", "*", "num")], group_by="place")
    assert isinstance(results, ResultIter)
    rows = list(results)
    assert len(rows) == 2, rows
    # Result resources are released once the iterator is exhausted
    assert results.result_proxy is not None
    assert results.result_proxy.closed


def test_aggregate_group_by_single_spec(table):
    rows = list(table.aggregate(("count", "*", "num"), group_by="place"))
    counts = {row["place"]: row["num"] for row in rows}
    assert counts == {TEST_CITY_1: 3, TEST_CITY_2: 3}, counts


def test_aggregate_multiple_functions(table):
    rows = list(
        table.aggregate(
            [
                ("count", "*", "num"),
                ("sum", "temperature", "total"),
                ("min", "temperature", "lowest"),
                ("max", "temperature", "highest"),
                ("avg", "temperature", "average"),
            ],
            group_by="place",
        )
    )
    by_place = {row["place"]: row for row in rows}
    galway = by_place[TEST_CITY_2]
    assert galway["num"] == 3
    assert galway["total"] == 0  # 1 + -1 + 0
    assert galway["lowest"] == -1
    assert galway["highest"] == 1
    berkeley = by_place[TEST_CITY_1]
    assert berkeley["total"] == 19  # 6 + 8 + 5
    assert berkeley["average"] == 19 / 3


def test_aggregate_count_star_vs_count_column(db):
    tbl = db["null_counts"]
    tbl.insert_many(
        [
            {"grp": "a", "value": 1},
            {"grp": "a", "value": None},
            {"grp": "a", "value": 3},
            {"grp": "b", "value": None},
        ]
    )
    rows = list(
        tbl.aggregate(
            [
                ("count", "*", "all_rows"),
                ("count", "value", "non_null"),
            ],
            group_by="grp",
        )
    )
    stats = {row["grp"]: row for row in rows}
    assert stats["a"]["all_rows"] == 3
    assert stats["a"]["non_null"] == 2
    assert stats["b"]["all_rows"] == 1
    assert stats["b"]["non_null"] == 0


def test_aggregate_multi_column_grouping(table):
    rows = list(table.aggregate([("count", "*", "num")], group_by=["place", "date"]))
    assert len(rows) == 6, rows
    for row in rows:
        assert set(row.keys()) == {"place", "date", "num"}
        assert row["num"] == 1


def test_aggregate_where_filters(table):
    rows = list(
        table.aggregate(
            [("sum", "temperature", "total")],
            group_by="place",
            place=TEST_CITY_1,
        )
    )
    assert len(rows) == 1, rows
    assert rows[0]["place"] == TEST_CITY_1
    assert rows[0]["total"] == 19

    # DSL operators also work and their values are parameter-bound
    rows = list(
        table.aggregate(
            [("count", "*", "num")],
            group_by="place",
            temperature={">": 5},
        )
    )
    assert {row["place"]: row["num"] for row in rows} == {TEST_CITY_1: 2}


def test_aggregate_no_group_by(table):
    rows = list(table.aggregate([("count", "*", "num"), ("sum", "temperature", "s")]))
    assert len(rows) == 1, rows
    assert rows[0]["num"] == 6
    assert rows[0]["s"] == 19


def test_aggregate_no_group_by_no_matching_rows(table):
    # Standard SQL semantics: an aggregate without GROUP BY returns one row
    # even when the WHERE clause matches nothing.
    rows = list(
        table.aggregate(
            [
                ("count", "*", "num"),
                ("sum", "temperature", "total"),
                ("avg", "temperature", "average"),
            ],
            place="Atlantis",
        )
    )
    assert len(rows) == 1, rows
    assert rows[0]["num"] == 0
    assert rows[0]["total"] is None
    assert rows[0]["average"] is None


def test_aggregate_group_by_no_matching_rows(table):
    # With GROUP BY, no matching groups means no result rows.
    rows = list(
        table.aggregate(
            [("count", "*", "num")],
            group_by="place",
            place="Atlantis",
        )
    )
    assert rows == []


def test_aggregate_having_without_group_by(table):
    # HAVING filters the single aggregate row; database semantics decide
    # whether it survives.
    rows = list(table.aggregate([("count", "*", "num")], having={"num": {">": 6}}))
    assert rows == []
    rows = list(table.aggregate([("count", "*", "num")], having={"num": {"<=": 6}}))
    assert len(rows) == 1
    assert rows[0]["num"] == 6


def test_aggregate_having_alias(table):
    rows = list(
        table.aggregate(
            [("count", "*", "num"), ("sum", "temperature", "total")],
            group_by="place",
            having={"num": {">=": 3}},
        )
    )
    assert len(rows) == 2, rows
    for row in rows:
        assert row["num"] >= 3

    rows = list(
        table.aggregate(
            [("count", "*", "num")],
            group_by="place",
            having={"num": {">": 3}},
        )
    )
    assert rows == []

    # HAVING can also reference a GROUP BY column
    rows = list(
        table.aggregate(
            [("count", "*", "num")],
            group_by="place",
            having={"place": TEST_CITY_1},
        )
    )
    assert len(rows) == 1
    assert rows[0]["place"] == TEST_CITY_1


def test_aggregate_having_equality_and_between(table):
    rows = list(
        table.aggregate(
            [("count", "*", "num"), ("sum", "temperature", "total")],
            group_by="place",
            having={"num": 3, "total": {"between": [-5, 10]}},
        )
    )
    assert {row["place"] for row in rows} == {TEST_CITY_2}


def test_aggregate_having_unknown_alias(table):
    with pytest.raises(QueryError):
        list(
            table.aggregate(
                [("count", "*", "num")],
                group_by="place",
                having={"nope": 1},
            )
        )


def test_aggregate_alias_conflicts_with_group_column(table):
    with pytest.raises(QueryError):
        table.aggregate([("count", "*", "place")], group_by="place")


def test_aggregate_duplicate_aliases(table):
    with pytest.raises(QueryError):
        table.aggregate(
            [
                ("count", "*", "num"),
                ("sum", "temperature", "num"),
            ],
            group_by="place",
        )


def test_aggregate_unknown_function(table):
    with pytest.raises(QueryError):
        table.aggregate([("median", "temperature", "m")], group_by="place")


def test_aggregate_star_only_for_count(table):
    with pytest.raises(QueryError):
        table.aggregate([("sum", "*", "s")], group_by="place")


def test_aggregate_unknown_column(table):
    with pytest.raises(DatasetError):
        table.aggregate([("sum", "nope", "s")], group_by="place")
    with pytest.raises(DatasetError):
        table.aggregate([("count", "*", "num")], group_by="nope")


def test_aggregate_empty_specs(table):
    with pytest.raises(QueryError):
        table.aggregate([], group_by="place")


def test_aggregate_malformed_spec(table):
    with pytest.raises(QueryError):
        table.aggregate([("count", "*")], group_by="place")  # type: ignore[list-item]


def test_aggregate_case_insensitive_columns_and_aliases(table):
    rows = list(
        table.aggregate(
            [("COUNT", "*", "NUM")],
            group_by="PLACE",
            having={"num": 3},
        )
    )
    assert len(rows) == 2
    assert "NUM" in rows[0]


def test_aggregate_on_missing_table(db):
    tbl = db.load_table("does_not_exist")
    assert list(tbl.aggregate([("count", "*", "num")])) == []


def test_aggregate_values_are_bound(table):
    # Compile the statement and make sure filter values become bind parameters
    # rather than being interpolated into the SQL text.
    place = table.table.c.place
    stmt = select(place.label("place"), func.count().label("num")).where(
        table._args_to_clause({"place": TEST_CITY_1})
    )
    compiled = stmt.compile(table.db.engine)
    assert TEST_CITY_1 in compiled.params.values()
    assert TEST_CITY_1 not in str(compiled)


def test_aggregate_streamed_close_releases_connection(db, table):
    results = table.aggregate(
        [("count", "*", "num")],
        group_by="place",
        _streamed=True,
        _step=1,
    )
    stream_conn = results._conn
    assert stream_conn is not None
    assert not stream_conn.closed
    # Closing without consuming everything still returns the stream connection
    results.close()
    assert stream_conn.closed
    assert results._conn is None
    # Closing again is a safe no-op
    results.close()


def test_aggregate_streamed_consume(table):
    rows = list(
        table.aggregate(
            [("count", "*", "num")],
            group_by="place",
            _streamed=True,
            _step=1,
        )
    )
    assert len(rows) == 2

