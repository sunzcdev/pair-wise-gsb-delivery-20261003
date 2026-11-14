import pytest

from sqlite_utils.db import BadMultiValues


@pytest.mark.parametrize(
    "columns,fn,expected",
    (
        (
            "title",
            lambda value: value.upper(),
            {"title": "MIXED CASE", "abstract": "Abstract"},
        ),
        (
            ["title", "abstract"],
            lambda value: value.upper(),
            {"title": "MIXED CASE", "abstract": "ABSTRACT"},
        ),
        (
            "title",
            lambda value: {"upper": value.upper(), "lower": value.lower()},
            {
                "title": '{"upper": "MIXED CASE", "lower": "mixed case"}',
                "abstract": "Abstract",
            },
        ),
    ),
)
def test_convert(fresh_db, columns, fn, expected):
    table = fresh_db.table("table")
    table.insert({"title": "Mixed Case", "abstract": "Abstract"})
    table.convert(columns, fn)
    assert list(table.rows) == [expected]


@pytest.mark.parametrize(
    "where,where_args", (("id > 1", None), ("id > :id", {"id": 1}), ("id > ?", [1]))
)
def test_convert_where(fresh_db, where, where_args):
    table = fresh_db.table("table")
    table.insert_all(
        [
            {"id": 1, "title": "One"},
            {"id": 2, "title": "Two"},
        ],
        pk="id",
    )
    table.convert(
        "title", lambda value: value.upper(), where=where, where_args=where_args
    )
    assert list(table.rows) == [{"id": 1, "title": "One"}, {"id": 2, "title": "TWO"}]


def test_convert_handles_falsey_values(fresh_db):
    # Falsey values like 0 should be converted (issue #527)
    table = fresh_db.table("table")
    table.insert_all([{"x": 0}, {"x": 1}])
    assert table.get(1)["x"] == 0
    assert table.get(2)["x"] == 1
    table.convert("x", lambda x: x + 1)
    assert table.get(1)["x"] == 1
    assert table.get(2)["x"] == 2


@pytest.mark.parametrize(
    "drop,expected",
    (
        (False, {"title": "Mixed Case", "other": "MIXED CASE"}),
        (True, {"other": "MIXED CASE"}),
    ),
)
def test_convert_output(fresh_db, drop, expected):
    table = fresh_db.table("table")
    table.insert({"title": "Mixed Case"})
    table.convert("title", lambda v: v.upper(), output="other", drop=drop)
    assert list(table.rows) == [expected]


def test_convert_output_multiple_column_error(fresh_db):
    table = fresh_db.table("table")
    with pytest.raises(ValueError) as excinfo:
        table.convert(["title", "other"], lambda v: v, output="out")
        assert "output= can only be used with a single column" in str(excinfo.value)


@pytest.mark.parametrize(
    "type,expected",
    (
        (int, {"other": 123}),
        (float, {"other": 123.0}),
    ),
)
def test_convert_output_type(fresh_db, type, expected):
    table = fresh_db.table("table")
    table.insert({"number": "123"})
    table.convert("number", lambda v: v, output="other", output_type=type, drop=True)
    assert list(table.rows) == [expected]


def test_convert_multi(fresh_db):
    table = fresh_db.table("table")
    table.insert({"title": "Mixed Case"})
    table.convert(
        "title",
        lambda v: {
            "upper": v.upper(),
            "lower": v.lower(),
            "both": {
                "upper": v.upper(),
                "lower": v.lower(),
            },
        },
        multi=True,
    )
    assert list(table.rows) == [
        {
            "title": "Mixed Case",
            "upper": "MIXED CASE",
            "lower": "mixed case",
            "both": '{"upper": "MIXED CASE", "lower": "mixed case"}',
        }
    ]


def test_convert_multi_where(fresh_db):
    table = fresh_db.table("table")
    table.insert_all(
        [
            {"id": 1, "title": "One"},
            {"id": 2, "title": "Two"},
        ],
        pk="id",
    )
    table.convert(
        "title",
        lambda v: {"upper": v.upper(), "lower": v.lower()},
        multi=True,
        where="id > ?",
        where_args=[1],
    )
    assert list(table.rows) == [
        {"id": 1, "lower": None, "title": "One", "upper": None},
        {"id": 2, "lower": "two", "title": "Two", "upper": "TWO"},
    ]


def test_convert_multi_exception(fresh_db):
    table = fresh_db.table("table")
    table.insert({"title": "Mixed Case"})
    with pytest.raises(BadMultiValues):
        table.convert("title", lambda v: v.upper(), multi=True)


def test_convert_repeated(fresh_db):
    table = fresh_db.table("table")
    col = "num"
    table.insert({col: 1})
    table.convert(col, lambda x: x * 2)
    table.convert(col, lambda _x: 0)
    assert table.get(1) == {col: 0}


def test_convert_preview_is_read_only(fresh_db):
    table = fresh_db.table("table")
    table.insert_all(
        [
            {"id": 3, "title": "Three"},
            {"id": 1, "title": "One"},
            {"id": 2, "title": "Two"},
        ],
        pk="id",
    )
    fresh_db.executescript(
        """
        create table changes (id integer);
        create trigger table_changes after update on "table"
        begin
            insert into changes (id) values (new.id);
        end;
        """
    )
    schema = fresh_db.schema
    rows = list(table.rows)

    report = table.convert("title", lambda value: value.upper(), preview=True)

    assert report["matched"] == 3
    assert report["changed"] == 3
    assert report["errors"] == []
    assert report["samples"] == [
        {
            "pk": 1,
            "before": {"title": "One"},
            "after": {"title": "ONE"},
        },
        {
            "pk": 2,
            "before": {"title": "Two"},
            "after": {"title": "TWO"},
        },
        {
            "pk": 3,
            "before": {"title": "Three"},
            "after": {"title": "THREE"},
        },
    ]
    assert fresh_db.schema == schema
    assert list(table.rows) == rows
    assert fresh_db["changes"].count == 0


def test_convert_preview_scans_beyond_sample_limit(fresh_db):
    table = fresh_db.table("table")
    table.insert_all(
        [
            {"id": 1, "value": "one"},
            {"id": 2, "value": "two"},
            {"id": 3, "value": "bad"},
            {"id": 4, "value": "four"},
        ],
        pk="id",
    )

    def convert(value):
        if value == "bad":
            raise ValueError("not convertible")
        return value.upper()

    report = table.convert("value", convert, preview=True, preview_limit=1)

    assert report["matched"] == 4
    assert report["changed"] == 3
    assert [sample["pk"] for sample in report["samples"]] == [1]
    assert report["errors"] == [
        {
            "pk": 3,
            "column": "value",
            "value": "bad",
            "error": "not convertible",
            "error_type": "ValueError",
        }
    ]
    assert report.failed == 1
    assert report.error_count == 1
    assert report["samples_truncated"] is True
    assert list(table.rows) == [
        {"id": 1, "value": "one"},
        {"id": 2, "value": "two"},
        {"id": 3, "value": "bad"},
        {"id": 4, "value": "four"},
    ]


def test_convert_preview_multiple_columns(fresh_db):
    table = fresh_db.table("table")
    table.insert({"id": 1, "one": "One", "two": "Two"}, pk="id")
    schema = fresh_db.schema

    report = table.convert(
        ["one", "two"], lambda value: value.upper(), preview=True
    )

    assert report["samples"] == [
        {
            "pk": 1,
            "before": {"one": "One", "two": "Two"},
            "after": {"one": "ONE", "two": "TWO"},
        }
    ]
    assert fresh_db.schema == schema
    assert table.get(1) == {"id": 1, "one": "One", "two": "Two"}


def test_convert_preview_multi_preserves_missing_and_none(fresh_db):
    table = fresh_db.table("table")
    table.insert_all(
        [
            {"id": 1, "value": "null"},
            {"id": 2, "value": "missing"},
            {"id": 3, "value": "empty"},
        ],
        pk="id",
    )

    def convert(value):
        if value == "null":
            return {"present": None}
        if value == "missing":
            return {"other": "set"}
        return {}

    report = table.convert("value", convert, multi=True, preview=True)

    assert report["changed"] == 2
    samples = {sample["pk"]: sample for sample in report["samples"]}
    assert samples[1]["after"] == {"present": None}
    assert samples[2]["after"] == {"other": "set"}
    assert "present" not in samples[2]["after"]
    assert samples[3]["after"] == {}
    assert table.get(1) == {"id": 1, "value": "null"}


def test_convert_preview_output_does_not_create_column(fresh_db):
    table = fresh_db.table("table")
    table.insert({"id": 1, "name": "One"}, pk="id")
    schema = fresh_db.schema

    report = table.convert(
        "name",
        lambda value: value.upper(),
        output="upper_name",
        preview=True,
    )

    assert report["samples"] == [
        {
            "pk": 1,
            "before": {"name": "One"},
            "after": {"upper_name": "ONE"},
        }
    ]
    assert "upper_name" not in table.columns_dict
    assert fresh_db.schema == schema


def test_convert_preview_limit_alias(fresh_db):
    table = fresh_db.table("table")
    table.insert_all(
        [{"id": index, "value": str(index)} for index in range(1, 5)], pk="id"
    )

    assert len(table.convert("value", lambda value: value, preview=2)["samples"]) == 2
    assert (
        len(
            table.convert(
                "value", lambda value: value, preview=True, sample_limit=3
            )["samples"]
        )
        == 3
    )
    assert (
        len(
            table.convert("value", lambda value: value, preview=True, limit=1)[
                "samples"
            ]
        )
        == 1
    )


def test_convert_preview_calls_function_again_for_real_conversion(fresh_db):
    table = fresh_db.table("table")
    table.insert_all(
        [{"id": 1, "value": "one"}, {"id": 2, "value": "two"}], pk="id"
    )
    calls = []

    def convert(value):
        calls.append(value)
        return value.upper()

    table.convert("value", convert, preview=True)
    assert calls == ["one", "two"]
    table.convert("value", convert)
    assert calls == ["one", "two", "one", "two"]


def test_convert_preview_where(fresh_db):
    table = fresh_db.table("table")
    table.insert_all(
        [{"id": 1, "value": "one"}, {"id": 2, "value": "two"}], pk="id"
    )

    report = table.convert(
        "value",
        lambda value: value.upper(),
        where="id > :id",
        where_args={"id": 1},
        preview=True,
    )

    assert report["matched"] == 1
    assert report["changed"] == 1
    assert [sample["pk"] for sample in report["samples"]] == [2]
    assert list(table.rows) == [
        {"id": 1, "value": "one"},
        {"id": 2, "value": "two"},
    ]
