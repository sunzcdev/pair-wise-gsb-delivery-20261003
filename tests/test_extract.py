import itertools

import pytest

from sqlite_utils import ANY
from sqlite_utils.db import InvalidColumns


@pytest.mark.parametrize("table", [None, "Species"])
@pytest.mark.parametrize("fk_column", [None, "species"])
def test_extract_single_column(fresh_db, table, fk_column):
    expected_table = table or "species"
    expected_fk = fk_column or f"{expected_table}_id"
    iter_species = itertools.cycle(["Palm", "Spruce", "Mangrove", "Oak"])
    fresh_db.table("tree").insert_all(
        (
            {
                "id": i,
                "name": f"Tree {i}",
                "species": next(iter_species),
                "end": 1,
            }
            for i in range(1, 1001)
        ),
        pk="id",
    )
    fresh_db.table("tree").extract("species", table=table, fk_column=fk_column)
    assert fresh_db.table("tree").schema == (
        'CREATE TABLE "tree" (\n'
        '   "id" INTEGER PRIMARY KEY,\n'
        '   "name" TEXT,\n'
        f'   "{expected_fk}" INTEGER REFERENCES "{expected_table}"("id"),\n'
        + '   "end" INTEGER\n'
        + ")"
    )
    assert fresh_db.table(expected_table).schema == (
        f'CREATE TABLE "{expected_table}" (\n' + '   "id" INTEGER PRIMARY KEY,\n'
        '   "species" TEXT\n'
        ")"
    )
    assert list(fresh_db.table(expected_table).rows) == [
        {"id": 1, "species": "Palm"},
        {"id": 2, "species": "Spruce"},
        {"id": 3, "species": "Mangrove"},
        {"id": 4, "species": "Oak"},
    ]
    assert list(itertools.islice(fresh_db.table("tree").rows, 0, 4)) == [
        {"id": 1, "name": "Tree 1", expected_fk: 1, "end": 1},
        {"id": 2, "name": "Tree 2", expected_fk: 2, "end": 1},
        {"id": 3, "name": "Tree 3", expected_fk: 3, "end": 1},
        {"id": 4, "name": "Tree 4", expected_fk: 4, "end": 1},
    ]


def test_extract_multiple_columns_with_rename(fresh_db):
    iter_common = itertools.cycle(["Palm", "Spruce", "Mangrove", "Oak"])
    iter_latin = itertools.cycle(["Arecaceae", "Picea", "Rhizophora", "Quercus"])
    fresh_db.table("tree").insert_all(
        (
            {
                "id": i,
                "name": f"Tree {i}",
                "common_name": next(iter_common),
                "latin_name": next(iter_latin),
            }
            for i in range(1, 1001)
        ),
        pk="id",
    )

    fresh_db.table("tree").extract(
        ["common_name", "latin_name"], rename={"common_name": "name"}
    )
    assert fresh_db.table("tree").schema == (
        'CREATE TABLE "tree" (\n'
        '   "id" INTEGER PRIMARY KEY,\n'
        '   "name" TEXT,\n'
        '   "common_name_latin_name_id" INTEGER REFERENCES "common_name_latin_name"("id")\n'
        ")"
    )
    assert fresh_db.table("common_name_latin_name").schema == (
        'CREATE TABLE "common_name_latin_name" (\n'
        '   "id" INTEGER PRIMARY KEY,\n'
        '   "name" TEXT,\n'
        '   "latin_name" TEXT\n'
        ")"
    )
    assert list(fresh_db.table("common_name_latin_name").rows) == [
        {"name": "Palm", "id": 1, "latin_name": "Arecaceae"},
        {"name": "Spruce", "id": 2, "latin_name": "Picea"},
        {"name": "Mangrove", "id": 3, "latin_name": "Rhizophora"},
        {"name": "Oak", "id": 4, "latin_name": "Quercus"},
    ]
    assert list(itertools.islice(fresh_db.table("tree").rows, 0, 4)) == [
        {"id": 1, "name": "Tree 1", "common_name_latin_name_id": 1},
        {"id": 2, "name": "Tree 2", "common_name_latin_name_id": 2},
        {"id": 3, "name": "Tree 3", "common_name_latin_name_id": 3},
        {"id": 4, "name": "Tree 4", "common_name_latin_name_id": 4},
    ]


def test_extract_invalid_columns(fresh_db):
    fresh_db.table("tree").insert(
        {
            "id": 1,
            "name": "Tree 1",
            "common_name": "Palm",
            "latin_name": "Arecaceae",
        },
        pk="id",
    )
    with pytest.raises(InvalidColumns):
        fresh_db.table("tree").extract(["bad_column"])


def test_extract_rowid_table(fresh_db):
    fresh_db.table("tree").insert(
        {
            "name": "Tree 1",
            "common_name": "Palm",
            "latin_name": "Arecaceae",
        }
    )
    fresh_db.table("tree").extract(["common_name", "latin_name"])
    assert fresh_db.table("tree").schema == (
        'CREATE TABLE "tree" (\n'
        '   "name" TEXT,\n'
        '   "common_name_latin_name_id" INTEGER REFERENCES "common_name_latin_name"("id")\n'
        ")"
    )
    assert fresh_db.execute("""
        select
            tree.name,
            common_name_latin_name.common_name,
            common_name_latin_name.latin_name
        from tree
            join common_name_latin_name
            on tree.common_name_latin_name_id = common_name_latin_name.id
    """).fetchall() == [("Tree 1", "Palm", "Arecaceae")]


def test_reuse_lookup_table(fresh_db):
    fresh_db.table("species").insert({"id": 1, "name": "Wolf"}, pk="id")
    fresh_db.table("sightings").insert({"id": 10, "species": "Wolf"}, pk="id")
    fresh_db.table("individuals").insert(
        {"id": 10, "name": "Terriana", "species": "Fox"}, pk="id"
    )
    fresh_db.table("sightings").extract("species", rename={"species": "name"})
    fresh_db.table("individuals").extract("species", rename={"species": "name"})
    assert fresh_db.table("sightings").schema == (
        'CREATE TABLE "sightings" (\n'
        '   "id" INTEGER PRIMARY KEY,\n'
        '   "species_id" INTEGER REFERENCES "species"("id")\n'
        ")"
    )
    assert fresh_db.table("individuals").schema == (
        'CREATE TABLE "individuals" (\n'
        '   "id" INTEGER PRIMARY KEY,\n'
        '   "name" TEXT,\n'
        '   "species_id" INTEGER REFERENCES "species"("id")\n'
        ")"
    )
    assert list(fresh_db.table("species").rows) == [
        {"id": 1, "name": "Wolf"},
        {"id": 2, "name": "Fox"},
    ]


def test_extract_error_on_incompatible_existing_lookup_table(fresh_db):
    fresh_db.table("species").insert({"id": 1})
    fresh_db.table("tree").insert({"name": "Tree 1", "common_name": "Palm"})
    with pytest.raises(InvalidColumns):
        fresh_db.table("tree").extract("common_name", table="species")

    # Try again with incompatible existing column type
    fresh_db.table("species2").insert({"id": 1, "common_name": 3.5})
    with pytest.raises(InvalidColumns):
        fresh_db.table("tree").extract("common_name", table="species2")


def test_extract_works_with_null_values(fresh_db):
    fresh_db.table("listens").insert_all(
        [
            {"id": 1, "track_title": "foo", "album_title": "bar"},
            {"id": 2, "track_title": "baz", "album_title": None},
        ],
        pk="id",
    )
    fresh_db.table("listens").extract(
        columns=["album_title"], table="albums", fk_column="album_id"
    )
    assert list(fresh_db.table("listens").rows) == [
        {"id": 1, "track_title": "foo", "album_id": 1},
        {"id": 2, "track_title": "baz", "album_id": None},
    ]
    assert list(fresh_db.table("albums").rows) == [
        {"id": 1, "album_title": "bar"},
    ]


def test_extract_null_values_single_column(fresh_db):
    # https://github.com/simonw/sqlite-utils/issues/186
    fresh_db.table("species").insert({"id": 1, "species": "Wolf"}, pk="id")
    fresh_db.table("individuals").insert_all(
        [
            {"id": 10, "name": "Terriana", "species": "Fox"},
            {"id": 11, "name": "Spenidorm", "species": None},
            {"id": 12, "name": "Grantheim", "species": "Wolf"},
            {"id": 13, "name": "Turnutopia", "species": None},
            {"id": 14, "name": "Wargal", "species": "Wolf"},
        ],
        pk="id",
    )
    fresh_db.table("individuals").extract("species")
    # No null row should have been added to species
    assert list(fresh_db.table("species").rows) == [
        {"id": 1, "species": "Wolf"},
        {"id": 2, "species": "Fox"},
    ]
    assert list(fresh_db.table("individuals").rows) == [
        {"id": 10, "name": "Terriana", "species_id": 2},
        {"id": 11, "name": "Spenidorm", "species_id": None},
        {"id": 12, "name": "Grantheim", "species_id": 1},
        {"id": 13, "name": "Turnutopia", "species_id": None},
        {"id": 14, "name": "Wargal", "species_id": 1},
    ]


def test_extract_null_values_multiple_columns(fresh_db):
    # A row should be extracted if at least one column is not null -
    # only rows where ALL extracted columns are null are left alone
    fresh_db.table("circulation").insert_all(
        [
            {"id": 1, "title": "title one", "creator": "creator one", "year": 2018},
            {"id": 2, "title": "title two", "creator": None, "year": 2019},
            {"id": 3, "title": None, "creator": None, "year": 2020},
            {"id": 4, "title": None, "creator": None, "year": 2021},
        ],
        pk="id",
    )
    fresh_db.table("circulation").extract(
        ["title", "creator"], table="books", fk_column="book_id"
    )
    assert list(fresh_db.table("books").rows) == [
        {"id": 1, "title": "title one", "creator": "creator one"},
        {"id": 2, "title": "title two", "creator": None},
    ]
    assert list(fresh_db.table("circulation").rows) == [
        {"id": 1, "book_id": 1, "year": 2018},
        {"id": 2, "book_id": 2, "year": 2019},
        {"id": 3, "book_id": None, "year": 2020},
        {"id": 4, "book_id": None, "year": 2021},
    ]


def test_extract_null_values_existing_lookup_table_with_null_row(fresh_db):
    # Even if the lookup table already contains an all-null row, rows where
    # every extracted column is null should keep a null foreign key
    fresh_db.table("species").insert({"id": 1, "species": None}, pk="id")
    fresh_db.table("individuals").insert_all(
        [
            {"id": 10, "name": "Terriana", "species": "Fox"},
            {"id": 11, "name": "Spenidorm", "species": None},
        ],
        pk="id",
    )
    fresh_db.table("individuals").extract("species")
    assert list(fresh_db.table("species").rows) == [
        {"id": 1, "species": None},
        {"id": 2, "species": "Fox"},
    ]
    assert list(fresh_db.table("individuals").rows) == [
        {"id": 10, "name": "Terriana", "species_id": 2},
        {"id": 11, "name": "Spenidorm", "species_id": None},
    ]


def test_extract_repeated_into_shared_lookup_with_nulls(fresh_db):
    # Unique indexes treat NULLs as distinct, so INSERT OR IGNORE alone
    # cannot dedupe NULL-containing rows against the existing lookup
    # table - extracting a second table into the same lookup previously
    # inserted duplicate rows that nothing pointed to
    fresh_db.table("t1").insert_all(
        [
            {"id": 1, "species": None, "common": "X"},
            {"id": 2, "species": "Oak", "common": "Oak"},
        ],
        pk="id",
    )
    fresh_db.table("t2").insert_all(
        [{"id": 1, "species": None, "common": "X"}], pk="id"
    )
    fresh_db.table("t1").extract(["species", "common"], table="lk")
    fresh_db.table("t2").extract(["species", "common"], table="lk")
    assert fresh_db.table("lk").count == 2
    # Both tables point at the same lookup row
    t1_fk = fresh_db.execute("select lk_id from t1 where id = 1").fetchone()[0]
    t2_fk = fresh_db.execute("select lk_id from t2 where id = 1").fetchone()[0]
    assert t1_fk == t2_fk


def test_extract_repeated_into_shared_lookup_no_nulls(fresh_db):
    # Non-NULL rows were already deduped by the unique index - keep it so
    fresh_db.table("t1").insert_all([{"id": 1, "species": "Oak"}], pk="id")
    fresh_db.table("t2").insert_all([{"id": 1, "species": "Oak"}], pk="id")
    fresh_db.table("t1").extract(["species"], table="lk")
    fresh_db.table("t2").extract(["species"], table="lk")
    assert fresh_db.table("lk").count == 1


def test_extract_preserves_strict_any(fresh_db):
    if not fresh_db.supports_strict:
        pytest.skip("SQLite version does not support strict tables")
    fresh_db.execute("create table items (id integer primary key, data any) strict")
    fresh_db.execute("insert into items values (1, ?)", ("000123",))

    fresh_db["items"].extract("data", table="data_values")

    lookup = fresh_db["data_values"]
    assert lookup.strict is True
    assert lookup.columns_dict == {"id": int, "data": ANY}
    assert fresh_db.execute(
        "select typeof(data), data from data_values"
    ).fetchone() == ("text", "000123")


def test_extract_strict_any_rejects_non_strict_lookup(fresh_db):
    if not fresh_db.supports_strict:
        pytest.skip("SQLite version does not support strict tables")
    fresh_db.execute("create table items (data any) strict")
    fresh_db.execute("insert into items values (?)", ("000123",))
    fresh_db.execute("create table data_values (id integer primary key, data any)")

    with pytest.raises(
        InvalidColumns,
        match="is not STRICT, so it cannot preserve ANY column values",
    ):
        fresh_db["items"].extract("data", table="data_values")

    assert fresh_db.execute("select typeof(data), data from items").fetchone() == (
        "text",
        "000123",
    )


def test_extract_null_strategy_lookup_row_single_column(fresh_db):
    fresh_db.table("individuals").insert_all(
        [
            {"id": 10, "name": "Terriana", "species": "Fox"},
            {"id": 11, "name": "Spenidorm", "species": None},
            {"id": 12, "name": "Turnutopia", "species": None},
        ],
        pk="id",
    )
    fresh_db.table("individuals").extract(
        "species", null_strategy="lookup_row"
    )
    assert list(fresh_db.table("species").rows) == [
        {"id": 1, "species": "Fox"},
        {"id": 2, "species": None},
    ]
    assert list(fresh_db.table("individuals").rows) == [
        {"id": 10, "name": "Terriana", "species_id": 1},
        {"id": 11, "name": "Spenidorm", "species_id": 2},
        {"id": 12, "name": "Turnutopia", "species_id": 2},
    ]


def test_extract_null_strategy_lookup_row_composite(fresh_db):
    # Partial-null combos are still their own dimension values; only the
    # all-null rows are affected by the null strategy
    fresh_db.table("circulation").insert_all(
        [
            {"id": 1, "title": "title one", "creator": "creator one", "year": 2018},
            {"id": 2, "title": "title two", "creator": None, "year": 2019},
            {"id": 3, "title": None, "creator": None, "year": 2020},
            {"id": 4, "title": None, "creator": None, "year": 2021},
        ],
        pk="id",
    )
    fresh_db.table("circulation").extract(
        ["title", "creator"],
        table="books",
        fk_column="book_id",
        null_strategy="lookup_row",
    )
    assert list(fresh_db.table("books").rows) == [
        {"id": 1, "title": "title one", "creator": "creator one"},
        {"id": 2, "title": "title two", "creator": None},
        {"id": 3, "title": None, "creator": None},
    ]
    assert list(fresh_db.table("circulation").rows) == [
        {"id": 1, "book_id": 1, "year": 2018},
        {"id": 2, "book_id": 2, "year": 2019},
        {"id": 3, "book_id": 3, "year": 2020},
        {"id": 4, "book_id": 3, "year": 2021},
    ]


def test_extract_null_strategy_lookup_row_reuses_existing_null_row(fresh_db):
    # The all-null row already present in the lookup table must be reused
    # rather than duplicated, despite SQL NULL comparison rules
    fresh_db.table("species").insert({"id": 1, "species": None}, pk="id")
    fresh_db.table("individuals").insert_all(
        [
            {"id": 10, "name": "Terriana", "species": None},
            {"id": 11, "name": "Spenidorm", "species": "Fox"},
        ],
        pk="id",
    )
    fresh_db.table("individuals").extract(
        "species", null_strategy="lookup_row"
    )
    assert list(fresh_db.table("species").rows) == [
        {"id": 1, "species": None},
        {"id": 2, "species": "Fox"},
    ]
    assert list(fresh_db.table("individuals").rows) == [
        {"id": 10, "name": "Terriana", "species_id": 1},
        {"id": 11, "name": "Spenidorm", "species_id": 2},
    ]


def test_extract_null_strategy_lookup_row_repeated_shared_lookup(fresh_db):
    # Two overlapping batches extracted into the same lookup must converge on
    # the same all-null dimension row
    fresh_db.table("t1").insert_all(
        [
            {"id": 1, "species": None, "common": "X"},
            {"id": 2, "species": None, "common": None},
        ],
        pk="id",
    )
    fresh_db.table("t2").insert_all(
        [{"id": 1, "species": None, "common": None}], pk="id"
    )
    fresh_db.table("t1").extract(
        ["species", "common"], table="lk", null_strategy="lookup_row"
    )
    fresh_db.table("t2").extract(
        ["species", "common"], table="lk", null_strategy="lookup_row"
    )
    assert fresh_db.table("lk").count == 2
    t1_fk = fresh_db.execute(
        "select lk_id from t1 where id = 2"
    ).fetchone()[0]
    t2_fk = fresh_db.execute("select lk_id from t2 where id = 1").fetchone()[0]
    assert t1_fk == t2_fk


def test_extract_empty_string_is_a_real_dimension_value(fresh_db):
    # An empty string is distinct from NULL: it always becomes a lookup row,
    # regardless of which null strategy is in use
    fresh_db.table("listens").insert_all(
        [
            {"id": 1, "album_title": ""},
            {"id": 2, "album_title": ""},
            {"id": 3, "album_title": None},
        ],
        pk="id",
    )
    fresh_db.table("listens").extract(
        columns=["album_title"], table="albums", fk_column="album_id"
    )
    assert list(fresh_db.table("albums").rows) == [
        {"id": 1, "album_title": ""},
    ]
    assert list(fresh_db.table("listens").rows) == [
        {"id": 1, "album_id": 1},
        {"id": 2, "album_id": 1},
        {"id": 3, "album_id": None},
    ]


def test_extract_empty_string_with_lookup_row_strategy(fresh_db):
    fresh_db.table("listens").insert_all(
        [
            {"id": 1, "album_title": ""},
            {"id": 2, "album_title": None},
        ],
        pk="id",
    )
    fresh_db.table("listens").extract(
        columns=["album_title"],
        table="albums",
        fk_column="album_id",
        null_strategy="lookup_row",
    )
    assert list(fresh_db.table("albums").rows) == [
        {"id": 1, "album_title": ""},
        {"id": 2, "album_title": None},
    ]
    assert list(fresh_db.table("listens").rows) == [
        {"id": 1, "album_id": 1},
        {"id": 2, "album_id": 2},
    ]


def test_extract_invalid_null_strategy(fresh_db):
    fresh_db.table("t").insert({"id": 1, "v": "a"}, pk="id")
    with pytest.raises(ValueError, match="null_strategy"):
        fresh_db.table("t").extract("v", null_strategy="bogus")


def test_extract_fk_column_case_insensitive_conflict(fresh_db):
    # SQLite identifiers are case-insensitive, so SPECIES_ID and species_id
    # are the same column and must be rejected before the table is rewritten
    fresh_db.table("trees").insert(
        {"id": 1, "Species": "Palm", "SPECIES_ID": 5}, pk="id"
    )
    schema_before = fresh_db.table("trees").schema
    with pytest.raises(InvalidColumns, match="already exists"):
        fresh_db.table("trees").extract(
            "Species", table="species", fk_column="species_id"
        )
    # The failed extraction must leave the source table untouched
    assert fresh_db.table("trees").schema == schema_before
    assert list(fresh_db.table("trees").rows) == [
        {"id": 1, "Species": "Palm", "SPECIES_ID": 5}
    ]
    assert "species" not in fresh_db.table_names()


def test_extract_fk_column_matching_extracted_column_is_allowed(fresh_db):
    # A foreign key named after the extracted column is fine - that column
    # is dropped and replaced by the foreign key
    fresh_db.table("trees").insert(
        {"id": 1, "species": "Palm"}, pk="id"
    )
    fresh_db.table("trees").extract("species", fk_column="species")
    assert fresh_db.table("trees").columns_dict == {"id": int, "species": int}
    assert list(fresh_db.table("species").rows) == [{"id": 1, "species": "Palm"}]


def test_extract_overlapping_batches_keep_dimension_table_stable(fresh_db):
    # Importing two overlapping batches and extracting each into the same
    # dimension table must not duplicate existing combinations
    fresh_db.table("items").insert_all(
        [
            {"id": 1, "a": "x", "b": None},
            {"id": 2, "a": None, "b": None},
            {"id": 3, "a": "y", "b": "z"},
        ],
        pk="id",
    )
    fresh_db.table("items").extract(
        ["a", "b"], table="dim", fk_column="dim_id"
    )
    dimension_count_after_first_batch = fresh_db.table("dim").count

    # A second, overlapping batch arrives via freshly imported columns
    fresh_db.table("items").add_column("a", str)
    fresh_db.table("items").add_column("b", str)
    fresh_db.table("items").update(1, {"a": "x", "b": None})
    fresh_db.table("items").update(2, {"a": None, "b": None})
    fresh_db.table("items").update(3, {"a": "y", "b": "z"})
    fresh_db.table("items").insert(
        {"id": 4, "a": "x", "b": None}, pk="id"
    )
    fresh_db.table("items").insert(
        {"id": 5, "a": "q", "b": None}, pk="id"
    )
    fresh_db.table("items").extract(
        ["a", "b"], table="dim", fk_column="dim_id2"
    )

    # Only the genuinely new combination was added
    assert (
        fresh_db.table("dim").count
        == dimension_count_after_first_batch + 1
    )
    assert list(fresh_db.table("dim").rows) == [
        {"id": 1, "a": "x", "b": None},
        {"id": 2, "a": "y", "b": "z"},
        {"id": 3, "a": "q", "b": None},
    ]
    # Overlapping rows from both batches reference the same dimension rows
    first_batch_fk = fresh_db.execute(
        "select dim_id from items where id = 1"
    ).fetchone()[0]
    second_batch_fk = fresh_db.execute(
        "select dim_id2 from items where id = 4"
    ).fetchone()[0]
    assert first_batch_fk == second_batch_fk


def test_extract_preserves_foreign_keys_and_indexes(fresh_db):
    fresh_db.table("items").insert_all(
        [{"id": 1, "v": None}, {"id": 2, "v": "a"}, {"id": 3, "v": "a"}],
        pk="id",
    )
    fresh_db.table("items").extract(
        "v", table="dim", null_strategy="lookup_row"
    )
    foreign_key = fresh_db.table("items").foreign_keys[0]
    assert (foreign_key.column, foreign_key.other_table) == ("dim_id", "dim")
    assert fresh_db.table("dim").indexes[0].unique == 1
    assert fresh_db.table("dim").indexes[0].columns == ["v"]
    assert fresh_db.execute("PRAGMA foreign_key_check").fetchall() == []


def test_extract_failure_rolls_back_source_and_lookup_changes(fresh_db):
    # An incompatible existing lookup table aborts the whole extraction:
    # neither the rewritten source table nor lookup rows may remain
    fresh_db.table("species").insert({"id": 1})
    fresh_db.table("tree").insert_all(
        [
            {"id": 1, "name": "Tree 1", "common_name": "Palm"},
            {"id": 2, "name": "Tree 2", "common_name": "Oak"},
        ],
        pk="id",
    )
    schema_before = fresh_db.table("tree").schema
    with pytest.raises(InvalidColumns):
        fresh_db.table("tree").extract("common_name", table="species")
    assert fresh_db.table("tree").schema == schema_before
    assert list(fresh_db.table("tree").rows) == [
        {"id": 1, "name": "Tree 1", "common_name": "Palm"},
        {"id": 2, "name": "Tree 2", "common_name": "Oak"},
    ]
    assert list(fresh_db.table("species").rows) == [{"id": 1}]
