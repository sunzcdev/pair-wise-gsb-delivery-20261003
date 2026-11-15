"""Tests for addressing tables in a specific schema."""

import os
import shutil
import sqlite3
import tempfile

import pytest
from sqlalchemy import text

import dataset
from dataset.util import normalize_schema_name

SCHEMA_ONE = "schema one"
# A schema name with both kinds of quotes: it must be treated as an
# identifier, never spliced into SQL.
SCHEMA_TWO = "sch'ema\"two"


@pytest.fixture(scope="function")
def attached_db():
    tmpdir = tempfile.mkdtemp(prefix="dataset-schema-")
    paths = {
        "main": os.path.join(tmpdir, "main.db"),
        SCHEMA_ONE: os.path.join(tmpdir, "one.db"),
        SCHEMA_TWO: os.path.join(tmpdir, "two.db"),
    }
    db = dataset.connect(f"sqlite:///{paths['main']}")
    conn = db.executable
    conn.execute(text('ATTACH DATABASE :p AS "schema one"'), {"p": paths[SCHEMA_ONE]})
    conn.execute(
        text("ATTACH DATABASE :p AS :name"),
        {"p": paths[SCHEMA_TWO], "name": SCHEMA_TWO},
    )
    db._auto_commit()
    yield db, paths
    db.close()
    shutil.rmtree(tmpdir, ignore_errors=True)


def test_default_calls_unchanged(attached_db):
    db, _paths = attached_db
    table = db["weather"]
    assert table.schema is None
    assert table is db.get_table("weather")
    assert table is db.create_table("weather")
    assert table is db.load_table("weather")
    table.insert({"place": "Berlin"})
    assert list(table.find(place="Berlin"))[0]["place"] == "Berlin"


def test_cache_identity_includes_schema(attached_db):
    db, _paths = attached_db
    default = db["weather"]
    one = db.get_table("weather", schema=SCHEMA_ONE)
    two = db.get_table("weather", schema=SCHEMA_TWO)
    assert len({id(default), id(one), id(two)}) == 3
    assert default is not one and one is not two and default is not two
    # Asking again returns the cached handle for that schema.
    assert one is db.get_table("weather", schema=SCHEMA_ONE)
    assert two is db.load_table("weather", schema=SCHEMA_TWO)
    assert set(db._tables) == {
        (None, "weather"),
        (SCHEMA_ONE, "weather"),
        (SCHEMA_TWO, "weather"),
    }
    assert one.schema == SCHEMA_ONE
    assert two.schema == SCHEMA_TWO


def test_interleaved_operations_do_not_cross_tables(attached_db):
    db, _paths = attached_db
    default = db["weather"]
    one = db.get_table("weather", schema=SCHEMA_ONE)
    two = db.get_table("weather", schema=SCHEMA_TWO)

    # Access the default handle first: this must not leak its reflected
    # definition into the schema-bound handles.
    default.insert({"place": "main"})
    assert default.columns == ["id", "place"]

    one.insert({"place": "one", "temperature": 3})
    two.insert({"place": "two", "note": "hello", "count": 7})
    default.insert({"place": "main2"})

    assert default.columns == ["id", "place"]
    assert one.columns == ["id", "place", "temperature"]
    assert two.columns == ["id", "place", "note", "count"]

    # Each metadata object belongs to exactly one schema.
    assert one.table.metadata is not two.table.metadata
    assert one.table.metadata is not default.table.metadata
    assert one.table.schema == SCHEMA_ONE
    assert two.table.schema == SCHEMA_TWO
    assert default.table.schema is None

    assert [r["place"] for r in default.find(order_by="place")] == ["main", "main2"]
    assert [r["place"] for r in one.find()] == ["one"]
    assert [r["place"] for r in two.find()] == ["two"]

    one.update({"place": "one", "temperature": 9}, ["place"])
    two.upsert({"place": "two", "note": "changed"}, ["place"])
    assert one.find_one(place="one")["temperature"] == 9
    assert two.find_one(place="two")["note"] == "changed"
    assert default.find_one(place="main")["place"] == "main"
    assert two.find_one(place="one") is None


def test_auto_create_and_columns_per_schema(attached_db):
    db, _paths = attached_db
    one = db.get_table("people", schema=SCHEMA_ONE)
    two = db.get_table("people", schema=SCHEMA_TWO)
    one.insert({"name": "Alice", "age": 30})
    two.create_column("nickname", db.types.text)
    two.insert({"name": "Bob", "nickname": "bobby"})

    assert "age" in one.columns and "nickname" not in one.columns
    assert "nickname" in two.columns and "age" not in two.columns
    assert one.find_one(name="Alice")["age"] == 30
    assert two.find_one(name="Bob")["nickname"] == "bobby"
    assert db.inspect.get_columns(
        "people", schema=SCHEMA_ONE
    ) != db.inspect.get_columns("people", schema=SCHEMA_TWO)

    # Column expansion on an existing table stays in its own schema.
    one.insert({"name": "Alice", "city": "Berlin"})
    assert "city" in one.columns
    assert "city" not in two.columns
    assert "city" not in db["people"].columns
    two.insert({"name": "Bob", "country": "IE"})
    assert "country" in two.columns and "country" not in one.columns


def test_indexes_are_created_in_their_schema(attached_db):
    db, _paths = attached_db
    one = db.get_table("weather", schema=SCHEMA_ONE)
    two = db.get_table("weather", schema=SCHEMA_TWO)
    one.insert({"place": "one"})
    two.insert({"place": "two"})

    one.create_index(["place"])
    assert one.has_index(["place"])
    assert not two.has_index(["place"])
    one_indexes = db.inspect.get_indexes("weather", schema=SCHEMA_ONE)
    two_indexes = db.inspect.get_indexes("weather", schema=SCHEMA_TWO)
    assert len(one_indexes) == 1
    assert two_indexes == []
    assert one.find_one(place="one")["place"] == "one"


def test_schema_names_are_identifiers(attached_db):
    db, _paths = attached_db
    # A name with spaces and quotes must survive intact when used as an
    # identifier in DDL and DML.
    table = db.get_table("weather", schema=SCHEMA_TWO)
    table.insert({"place": 'quoted "place"'})
    table.create_column("note", db.types.text)
    assert table.columns == ["id", "place", "note"]
    assert table.find_one(place='quoted "place"')["place"] == 'quoted "place"'
    assert db.has_table("weather", schema=SCHEMA_TWO) is True
    assert db.get_table_names(schema=SCHEMA_TWO) == ["weather"]


def test_missing_schema_is_not_confused(attached_db):
    db, _paths = attached_db
    one = db.get_table("weather", schema=SCHEMA_ONE)
    one.insert({"place": "one"})
    # The table does not exist in the default schema, even though a handle
    # for it exists in an attached schema.
    default = db["weather"]
    assert not default.exists
    assert default.find_one(place="one") is None
    assert db.has_table("weather") is False
    assert db.has_table("weather", schema=SCHEMA_ONE) is True


def test_drop_only_affects_its_schema(attached_db):
    db, _paths = attached_db
    one = db.get_table("weather", schema=SCHEMA_ONE)
    two = db.get_table("weather", schema=SCHEMA_TWO)
    one.insert({"place": "one"})
    two.insert({"place": "two"})
    one.drop()
    assert not one.exists
    assert two.exists
    assert two.find_one(place="two")["place"] == "two"
    assert (SCHEMA_ONE, "weather") not in db._tables
    assert (SCHEMA_TWO, "weather") in db._tables


def test_repr_contains_schema(attached_db):
    db, _paths = attached_db
    one = db.get_table("weather", schema=SCHEMA_ONE)
    assert repr(one) == f"<Table({SCHEMA_ONE}.weather)>"
    assert repr(db["weather"]) == "<Table(weather)>"


def test_normalize_schema_name():
    assert normalize_schema_name(None) is None
    assert normalize_schema_name(" main ") == "main"
    assert normalize_schema_name('a "b"') == 'a "b"'
    with pytest.raises(ValueError):
        normalize_schema_name("")
    with pytest.raises(ValueError):
        normalize_schema_name(1)  # type: ignore[arg-type]


def test_writes_land_in_attached_file(attached_db):
    db, paths = attached_db
    one = db.get_table("weather", schema=SCHEMA_ONE)
    two = db.get_table("weather", schema=SCHEMA_TWO)
    one.insert({"place": "one"})
    two.insert({"place": "two"})

    for schema, path, expected in (
        (SCHEMA_ONE, paths[SCHEMA_ONE], "one"),
        (SCHEMA_TWO, paths[SCHEMA_TWO], "two"),
    ):
        conn = sqlite3.connect(path)
        try:
            rows = conn.execute('SELECT "place" FROM weather').fetchall()
        finally:
            conn.close()
        assert rows == [(expected,)], (schema, rows)

    main = sqlite3.connect(paths["main"])
    try:
        assert (
            main.execute(
                "SELECT name FROM sqlite_master WHERE type = 'table'"
            ).fetchall()
            == []
        )
    finally:
        main.close()
