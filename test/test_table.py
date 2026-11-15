import json
from datetime import datetime

import pytest
from sqlalchemy.exc import ArgumentError
from sqlalchemy.types import BIGINT, TEXT

import dataset
from dataset import chunked
from dataset.filters import MAX_FILTER_DEPTH, MAX_FILTER_NODES, FilterError

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
    assert m["temperature"] == -10, f"new temp. should be -10 but is {m['temperature']}"


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


# -- structured filters ----------------------------------------------------


def _ids(table, spec):
    """Return ids selected by a structured filter, in a stable order."""
    return sorted(row["id"] for row in table.find(_filter=spec))


@pytest.fixture
def nullable_table(db):
    """Table with NULLs to exercise SQL three-valued logic."""
    tbl = db["nullable"]
    tbl.insert_many(
        [
            {"name": "a", "score": 1},
            {"name": "b", "score": None},
            {"name": "c", "score": 3},
            {"name": None, "score": None},
        ]
    )
    return tbl


def test_filter_comparison(table):
    spec = {"field": "place", "op": "eq", "value": TEST_CITY_1}
    assert len(_ids(table, spec)) == 3

    spec = {"field": "place", "op": "=", "value": TEST_CITY_1}
    assert len(_ids(table, spec)) == 3

    # "op" defaults to equality.
    spec = {"field": "place", "value": TEST_CITY_1}
    assert len(_ids(table, spec)) == 3


def test_filter_numeric_comparisons(table):
    assert len(_ids(table, {"field": "temperature", "op": "gt", "value": 5})) == 2
    assert len(_ids(table, {"field": "temperature", "op": "gte", "value": 5})) == 3
    assert len(_ids(table, {"field": "temperature", "op": "lt", "value": 0})) == 1
    assert len(_ids(table, {"field": "temperature", "op": "lte", "value": 0})) == 2
    assert len(_ids(table, {"field": "temperature", "op": "ne", "value": -1})) == 5


def test_filter_between(table):
    spec = {"field": "temperature", "op": "between", "value": [5, 8]}
    assert len(_ids(table, spec)) == 3
    spec = {"field": "temperature", "op": "..", "value": [5, 8]}
    assert len(_ids(table, spec)) == 3


def test_filter_like(table):
    assert len(_ids(table, {"field": "place", "op": "like", "value": "%lw%"})) == 3
    assert len(_ids(table, {"field": "place", "op": "ilike", "value": "%LW%"})) == 3
    assert len(_ids(table, {"field": "place", "op": "notlike", "value": "%LW%"})) == 3


def test_filter_startswith_endswith(db):
    tbl = db["filter_prefixes"]
    tbl.insert_many([{"org": "acme"}, {"org": "acme_labs"}, {"org": "other"}])
    assert len(_ids(tbl, {"field": "org", "op": "startswith", "value": "acme"})) == 2
    assert len(_ids(tbl, {"field": "org", "op": "endswith", "value": "labs"})) == 1
    # LIKE wildcards in the value must stay literal.
    assert _ids(tbl, {"field": "org", "op": "startswith", "value": "%"}) == []
    assert _ids(tbl, {"field": "org", "op": "endswith", "value": "_"}) == []


def test_filter_boolean_combinators(table):
    spec = {
        "all": [
            {"field": "place", "op": "eq", "value": TEST_CITY_1},
            {"field": "temperature", "op": "gte", "value": 6},
        ]
    }
    assert len(_ids(table, spec)) == 2

    spec = {
        "any": [
            {"field": "temperature", "op": "lt", "value": 0},
            {"field": "temperature", "op": "gt", "value": 7},
        ]
    }
    assert len(_ids(table, spec)) == 2

    spec = {"not": {"field": "place", "op": "eq", "value": TEST_CITY_1}}
    assert len(_ids(table, spec)) == 3


def test_filter_nested_boolean(table):
    spec = {
        "all": [
            {"field": "place", "op": "eq", "value": TEST_CITY_2},
            {
                "any": [
                    {"field": "temperature", "op": "lte", "value": -1},
                    {"not": {"field": "temperature", "op": "gte", "value": 0}},
                ]
            },
        ]
    }
    assert _ids(table, spec) == [2]


def test_filter_empty_all_is_true(table):
    assert len(_ids(table, {"all": []})) == len(TEST_DATA)
    assert table.count(_filter={"all": []}) == len(TEST_DATA)


def test_filter_empty_any_is_false(table):
    assert _ids(table, {"any": []}) == []
    assert table.count(_filter={"any": []}) == 0


def test_filter_empty_any_nested(table):
    spec = {"all": [{"field": "place", "op": "eq", "value": TEST_CITY_1}, {"any": []}]}
    assert _ids(table, spec) == []
    spec = {"any": [{"any": []}, {"field": "temperature", "op": "lt", "value": 0}]}
    assert _ids(table, spec) == [2]


def test_filter_in_and_notin(nullable_table):
    assert _ids(nullable_table, {"field": "score", "op": "in", "value": [1, 3]}) == [
        1,
        3,
    ]
    # NULL never matches IN without being listed explicitly.
    assert _ids(nullable_table, {"field": "score", "op": "in", "value": [1]}) == [1]
    assert _ids(nullable_table, {"field": "score", "op": "notin", "value": [1]}) == [3]


def test_filter_in_with_none_matches_null(nullable_table):
    spec = {"field": "score", "op": "in", "value": [1, None]}
    assert _ids(nullable_table, spec) == [1, 2, 4]


def test_filter_notin_with_none_excludes_null(nullable_table):
    # NOT IN with an explicit NULL member makes the complement of IN [1, None].
    spec = {"field": "score", "op": "notin", "value": [1, None]}
    assert _ids(nullable_table, spec) == [3]


def test_filter_in_null_only(nullable_table):
    assert _ids(nullable_table, {"field": "score", "op": "in", "value": [None]}) == [
        2,
        4,
    ]
    assert _ids(nullable_table, {"field": "score", "op": "notin", "value": [None]}) == [
        1,
        3,
    ]


def test_filter_empty_in_is_false(nullable_table):
    spec = {"field": "score", "op": "in", "value": []}
    assert _ids(nullable_table, spec) == []
    assert nullable_table.count(_filter=spec) == 0


def test_filter_empty_notin_is_true(nullable_table):
    spec = {"field": "score", "op": "notin", "value": []}
    assert _ids(nullable_table, spec) == [1, 2, 3, 4]


def test_filter_null_equality(nullable_table):
    spec = {"field": "score", "op": "eq", "value": None}
    assert _ids(nullable_table, spec) == [2, 4]
    spec = {"field": "score", "op": "ne", "value": None}
    assert _ids(nullable_table, spec) == [1, 3]


def test_filter_ordering_against_null_is_unknown(nullable_table):
    # Comparing with NULL is UNKNOWN, so it never matches - and neither does
    # its negation, because NOT UNKNOWN stays UNKNOWN.
    for op in ("gt", "gte", "lt", "lte"):
        spec = {"field": "score", "op": op, "value": None}
        assert _ids(nullable_table, spec) == [], op
        assert _ids(nullable_table, {"not": spec}) == [], op


def test_filter_between_with_null_is_unknown(nullable_table):
    spec = {"field": "score", "op": "between", "value": [None, 3]}
    assert _ids(nullable_table, spec) == []
    assert _ids(nullable_table, {"not": spec}) == []


def test_filter_not_keeps_three_valued_logic(nullable_table):
    # NOT (score = 1) must not match NULL rows: UNKNOWN stays UNKNOWN.
    spec = {"not": {"field": "score", "op": "eq", "value": 1}}
    assert _ids(nullable_table, spec) == [3]


def test_filter_not_empty_all(nullable_table):
    # NOT TRUE is FALSE, not "all rows".
    spec = {"not": {"all": []}}
    assert _ids(nullable_table, spec) == []
    # NOT FALSE is TRUE.
    spec = {"not": {"any": []}}
    assert _ids(nullable_table, spec) == [1, 2, 3, 4]


def test_filter_equivalent_to_keyword_filters(table):
    # A structured filter and the equivalent keyword filter must agree.
    cases = [
        (
            {"field": "place", "op": "eq", "value": TEST_CITY_1},
            {"place": TEST_CITY_1},
        ),
        (
            {"field": "temperature", "op": "between", "value": [-1, 1]},
            {"temperature": {"between": [-1, 1]}},
        ),
        (
            {"field": "place", "op": "in", "value": [TEST_CITY_1, TEST_CITY_2]},
            {"place": [TEST_CITY_1, TEST_CITY_2]},
        ),
        (
            {"field": "place", "op": "like", "value": "%lw%"},
            {"place": {"like": "%lw%"}},
        ),
    ]
    for spec, kwargs in cases:
        structured = _ids(table, spec)
        keyword = sorted(row["id"] for row in table.find(**kwargs))
        assert structured == keyword, (spec, kwargs)
        assert table.count(_filter=spec) == table.count(**kwargs)


def test_filter_find_and_count_agree(table, nullable_table):
    specs = [
        {"field": "place", "op": "eq", "value": TEST_CITY_1},
        {"all": []},
        {"any": []},
        {"not": {"field": "place", "op": "eq", "value": TEST_CITY_1}},
        {"any": [{"field": "temperature", "op": "gt", "value": 5}, {"any": []}]},
    ]
    for spec in specs:
        assert len(_ids(table, spec)) == table.count(_filter=spec), spec

    nullable_specs = [
        {"field": "score", "op": "in", "value": [1, None]},
        {"field": "score", "op": "notin", "value": [1, None]},
        {"field": "score", "op": "notin", "value": []},
        {"not": {"field": "score", "op": "eq", "value": 1}},
    ]
    for spec in nullable_specs:
        assert len(_ids(nullable_table, spec)) == nullable_table.count(_filter=spec), (
            spec
        )


def test_filter_build_and_pass_clause(table):
    spec = {"all": [{"field": "place", "op": "eq", "value": TEST_CITY_1}]}
    clause = table.build_filter(spec)
    assert len(list(table.find(clause))) == 3
    assert table.count(clause) == 3
    # The dedicated _filter entry point must select the same rows.
    assert sorted(row["id"] for row in table.find(clause)) == _ids(table, spec)


def test_filter_combines_with_other_filters(table):
    spec = {"field": "place", "op": "eq", "value": TEST_CITY_1}
    assert _ids(table, spec) == [4, 5, 6]
    # Keyword filters and positional clauses are ANDed with the structured
    # filter, so the result is a subset of the structured-filter result.
    filtered = sorted(
        row["id"] for row in table.find(_filter=spec, temperature={"gte": 6})
    )
    assert filtered == [4, 5]
    positional = sorted(
        row["id"]
        for row in table.find(table.table.columns.temperature < 6, _filter=spec)
    )
    assert positional == [6]
    assert table.count(table.table.columns.temperature < 6, _filter=spec) == 1


def test_filter_find_one(table):
    spec = {"field": "place", "op": "eq", "value": TEST_CITY_1}
    row = table.find_one(_filter=spec)
    assert row is not None
    assert row["place"] == TEST_CITY_1
    assert (
        table.find_one(_filter={"field": "place", "op": "eq", "value": "nope"}) is None
    )


def test_filter_delete(table):
    assert table.delete(_filter={"field": "place", "op": "eq", "value": TEST_CITY_1})
    assert len(table) == len(TEST_DATA) - 3
    assert table.count(place=TEST_CITY_1) == 0
    assert table.delete(_filter={"any": []}) is False


def test_filter_column_case_insensitive(db):
    tbl = db["filter_casing"]
    tbl.insert({"Place": "Berlin"})
    tbl.insert({"place": "Berlin"})
    spec = {"field": "PLACE", "op": "eq", "value": "Berlin"}
    assert len(_ids(tbl, spec)) == 2


def test_filter_unknown_field_is_an_error(table):
    with pytest.raises(FilterError) as excinfo:
        table.find(_filter={"field": "nope", "op": "eq", "value": 1})
    assert "$.field" in str(excinfo.value)
    assert "nope" in str(excinfo.value)
    assert isinstance(excinfo.value, dataset.QueryError)


def test_filter_unknown_field_path_inside_tree(table):
    spec = {
        "all": [
            {"field": "temperature", "op": "gt", "value": 0},
            {"any": [{"field": "missing", "op": "eq", "value": 1}]},
        ]
    }
    with pytest.raises(FilterError) as excinfo:
        table.count(_filter=spec)
    assert "$.all[1].any[0].field" in str(excinfo.value)


def test_filter_unknown_field_errors_before_query(table, monkeypatch):
    # Reaching the database would fail loudly; the error must be raised first.
    executed = []
    original = dataset.table.Table._args_to_clause

    def spy(self, args, clauses=()):
        executed.append(True)
        return original(self, args, clauses=clauses)

    monkeypatch.setattr(dataset.table.Table, "_args_to_clause", spy)
    with pytest.raises(FilterError):
        list(table.find(_filter={"field": "nope", "op": "eq", "value": 1}))
    assert executed == []


def test_filter_unknown_operator_is_an_error(table):
    with pytest.raises(FilterError) as excinfo:
        table.find(_filter={"field": "place", "op": "regex", "value": "x"})
    assert "$.op" in str(excinfo.value)
    assert "regex" in str(excinfo.value)


def test_filter_malformed_nodes(table):
    bad_specs = [
        "not a mapping",
        42,
        {},
        {"nope": []},
        {"all": "not a list"},
        {"any": {"field": "place", "op": "eq", "value": "x"}},
        {"not": [{"field": "place", "op": "eq", "value": "x"}]},
        {"all": [], "field": "place"},
        {"field": "place", "op": "eq", "value": "x", "extra": 1},
        {"field": "", "op": "eq", "value": "x"},
        {"field": 1, "op": "eq", "value": "x"},
        {"field": "place", "op": 1, "value": "x"},
        {"field": "place", "op": "eq"},
        {"field": "place", "op": "in", "value": "x"},
        {"field": "place", "op": "in", "value": [["nested"]]},
        {"field": "temperature", "op": "between", "value": [1]},
        {"field": "temperature", "op": "between", "value": [1, 2, 3]},
        {"field": "place", "op": "like", "value": 1},
    ]
    for spec in bad_specs:
        with pytest.raises(FilterError):
            table.count(_filter=spec)


def test_filter_values_are_not_sql_fragments(table):
    # A string value that looks like SQL is treated as data, not as a clause.
    marker = "x' OR 1=1; DROP TABLE weather; --"
    table.insert({"place": marker, "temperature": 99})
    spec = {"field": "place", "op": "eq", "value": marker}
    rows = list(table.find(_filter=spec))
    assert len(rows) == 1
    assert rows[0]["place"] == marker
    assert table.count() == len(TEST_DATA) + 1
    # A value that is not a string is not a fragment either.
    with pytest.raises(FilterError):
        table.find(_filter={"field": "place", "op": "eq", "value": {"x": 1}})


def test_filter_compiled_sql_uses_bind_parameters(table):
    marker = "Robert'); DROP TABLE weather;--"
    clause = table.build_filter(
        {"any": [{"field": "place", "op": "eq", "value": marker}]}
    )
    stmt = table.table.select().where(clause)
    compiled = stmt.compile(dialect=table.db.engine.dialect)
    assert marker not in str(compiled)
    assert marker in compiled.params.values()


def test_filter_depth_limit(table):
    def nest(depth):
        spec: dict = {"all": []}
        for _ in range(depth - 1):
            spec = {"all": [spec]}
        return spec

    # A tree at the limit compiles fine.
    table.count(_filter=nest(MAX_FILTER_DEPTH))
    with pytest.raises(FilterError) as excinfo:
        table.count(_filter=nest(MAX_FILTER_DEPTH + 1))
    assert "too deeply" in str(excinfo.value)


def test_filter_node_limit(table):
    # 1 'any' node + (MAX_FILTER_NODES - 1) leaf nodes == exactly the limit.
    allowed = {"any": [{"all": []} for _ in range(MAX_FILTER_NODES - 1)]}
    table.count(_filter=allowed)
    too_many = {"any": [{"all": []} for _ in range(MAX_FILTER_NODES)]}
    with pytest.raises(FilterError) as excinfo:
        table.count(_filter=too_many)
    assert "too many" in str(excinfo.value)


def test_filter_nonexistent_table(db):
    missing = db["filter_missing_table"]
    spec = {"field": "anything", "op": "eq", "value": 1}
    assert list(missing.find(_filter=spec)) == []
    assert missing.count(_filter=spec) == 0
    assert missing.find_one(_filter=spec) is None
    assert missing.delete(_filter=spec) is False
    assert "filter_missing_table" not in db
    with pytest.raises(FilterError):
        missing.find(_filter={"field": "anything", "op": "nope", "value": 1})


def test_filter_json_round_trip(table):
    spec = {
        "all": [
            {"field": "place", "op": "eq", "value": TEST_CITY_1},
            {"field": "temperature", "op": "in", "value": [5, 6, None]},
        ]
    }
    round_tripped = json.loads(json.dumps(spec))
    assert _ids(table, spec) == _ids(table, round_tripped) == [4, 6]
