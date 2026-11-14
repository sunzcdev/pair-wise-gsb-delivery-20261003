import json

import pytest
from click.testing import CliRunner

from sqlite_utils import Database, cli
from sqlite_utils.importing import (
    SQLITE_INTEGER_MAX,
    LateValuePolicy,
    SourceRow,
    apply_inferred_types,
    infer_column_types,
    sample_and_infer,
)


def test_inference_distinguishes_empty_missing_null_bool_and_oversized_int():
    rows = [
        {"empty": "", "missing": 1, "null": None, "flag": True},
        {"empty": "", "flag": False},
        {"oversized": 2**63},
    ]
    inference = infer_column_types(rows)

    assert inference.types == {
        "empty": "TEXT",
        "missing": "INTEGER",
        "null": "TEXT",
        "flag": "INTEGER",
        "oversized": "TEXT",
    }
    assert "empty string" in inference.reasons["empty"]
    assert "missing field" in inference.reasons["missing"]
    assert "NULL" in inference.reasons["null"]
    assert "boolean" in inference.reasons["flag"]
    assert "outside SQLite's signed 64-bit range" in inference.reasons["oversized"]


def test_empty_strings_can_be_treated_as_null_for_inference():
    inference = infer_column_types(
        [{"value": "1"}, {"value": ""}, {"value": "2"}],
        empty_is_null=True,
    )
    assert inference.types == {"value": "INTEGER"}
    assert "treated as NULL" in inference.reasons["value"]


def test_sample_is_replayed_without_reading_source_twice():
    reads = []

    def rows():
        for index, value in enumerate(["1", "2", "3", "4"]):
            reads.append(index)
            yield SourceRow({"value": value}, source_row=index + 2)

    result = sample_and_infer(rows(), 2)
    assert result.types == {"value": "INTEGER"}
    assert reads == [0, 1]

    imported = list(
        apply_inferred_types(
            result,
            result.inference,
            late_value_policy=LateValuePolicy.REJECT,
        )
    )
    assert imported == [{"value": 1}, {"value": 2}, {"value": 3}, {"value": 4}]
    assert reads == [0, 1, 2, 3]


def test_cli_late_value_reject_skips_row_and_reports_source_row_and_column(tmpdir):
    db_path = str(tmpdir / "test.db")
    result = CliRunner().invoke(
        cli.cli,
        ["insert", db_path, "creatures", "-", "--csv", "--sample-size", "1"],
        input="id,age\n1,4\n2,not-an-age\n3,6\n",
    )
    assert result.exit_code == 0, result.output
    assert "source row 3 column age" in result.output
    db = Database(db_path)
    assert db.table("creatures").columns_dict == {"id": int, "age": int}
    assert list(db.query("select * from creatures")) == [
        {"id": 1, "age": 4},
        {"id": 3, "age": 6},
    ]


def test_cli_late_value_error_column_keeps_original_value_and_type(tmpdir):
    db_path = str(tmpdir / "test.db")
    result = CliRunner().invoke(
        cli.cli,
        [
            "insert",
            db_path,
            "creatures",
            "-",
            "--csv",
            "--sample-size",
            "1",
            "--on-late-mismatch",
            "error-column",
            "--error-column",
            "import_error",
        ],
        input="id,age\n1,4\n2,not-an-age\n3,6\n",
    )
    assert result.exit_code == 0, result.output
    db = Database(db_path)
    assert db.table("creatures").columns_dict == {
        "id": int,
        "age": int,
        "import_error": str,
    }
    rows = list(db.query("select * from creatures order by id"))
    assert rows[0] == {"id": 1, "age": 4, "import_error": None}
    assert rows[1]["age"] is None
    failure = json.loads(rows[1]["import_error"])
    assert failure == {
        "source_row": 3,
        "column": "age",
        "value": "not-an-age",
        "expected_type": "INTEGER",
        "reason": "value is not an integer",
    }
    assert rows[2] == {"id": 3, "age": 6, "import_error": None}
    assert db.table("creatures").schema == (
        'CREATE TABLE "creatures" (\n'
        '   "id" INTEGER,\n'
        '   "age" INTEGER,\n'
        '   "import_error" TEXT\n'
        ")"
    )


def test_cli_error_column_implies_error_column_policy(tmpdir):
    db_path = str(tmpdir / "test.db")
    result = CliRunner().invoke(
        cli.cli,
        [
            "insert",
            db_path,
            "creatures",
            "-",
            "--csv",
            "--sample-size",
            "1",
            "--error-column",
            "import_error",
        ],
        input="age\n1\nnot-an-age\n",
    )
    assert result.exit_code == 0, result.output
    row = list(Database(db_path).query("select * from creatures"))[1]
    assert row["age"] is None
    assert json.loads(row["import_error"])["source_row"] == 3


def test_cli_data_and_error_column_are_written_by_same_insert(tmpdir):
    db_path = str(tmpdir / "test.db")
    db = Database(db_path)
    db.execute(
        'create table creatures (id integer, age integer, import_error text)'
    )
    db.execute("create table audit (age, import_error)")
    db.execute(
        """
        create trigger audit_creatures after insert on creatures begin
            insert into audit (age, import_error)
            values (NEW.age, NEW.import_error);
        end;
        """
    )
    db.close()
    result = CliRunner().invoke(
        cli.cli,
        [
            "insert",
            db_path,
            "creatures",
            "-",
            "--csv",
            "--sample-size",
            "1",
            "--error-column",
            "import_error",
        ],
        input="id,age\n1,4\n2,not-an-age\n",
    )
    assert result.exit_code == 0, result.output
    audit = list(Database(db_path).query("select * from audit"))
    assert audit[1]["age"] is None
    assert json.loads(audit[1]["import_error"])["source_row"] == 3


def test_cli_json_sampling_uses_record_numbers_and_replays_first_row(tmpdir):
    db_path = str(tmpdir / "test.db")
    result = CliRunner().invoke(
        cli.cli,
        ["insert", db_path, "items", "-", "--sample-size", "1"],
        input=json.dumps([{"id": 1}, {"id": "not-an-int"}, {"id": 3}]),
    )
    assert result.exit_code == 0, result.output
    assert "source row 2 column id" in result.output
    assert list(Database(db_path).query("select * from items")) == [
        {"id": 1},
        {"id": 3},
    ]


def test_cli_oversized_json_integer_is_preserved_as_text(tmpdir):
    db_path = str(tmpdir / "test.db")
    oversized = SQLITE_INTEGER_MAX + 1
    result = CliRunner().invoke(
        cli.cli,
        ["insert", db_path, "items", "-", "--sample-size", "1"],
        input=json.dumps([{"value": oversized}]),
    )
    assert result.exit_code == 0, result.output
    db = Database(db_path)
    assert db.table("items").columns_dict == {"value": str}
    assert db.execute("select typeof(value), value from items").fetchone() == (
        "text",
        str(oversized),
    )


@pytest.mark.parametrize(
    "rows,expected",
    (
        ([{"v": "1"}, {"v": ""}], "TEXT"),
        ([{"v": "1"}, {}], "INTEGER"),
        ([{"v": True}, {"v": False}], "INTEGER"),
        ([{"v": 1}, {"v": 2.5}], "REAL"),
    ),
)
def test_inference_choices(rows, expected):
    assert infer_column_types(rows).types["v"] == expected
