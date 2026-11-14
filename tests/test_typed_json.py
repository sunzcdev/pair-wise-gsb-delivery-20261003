import json

import pytest
from click.testing import CliRunner

from sqlite_utils import Database, cli
from sqlite_utils.utils import (
    JSON_MAX_SAFE_INTEGER,
    TypedJsonDecodeError,
    typed_json_decode,
    typed_json_encode,
)


@pytest.fixture
def typed_db_path(tmpdir):
    path = str(tmpdir / "typed.db")
    db = Database(path)
    db["t"].insert(
        {
            "id": 1,
            "name": "héllo 你好",
            "data": b"\x00\x01\x02blob",
            "empty_blob": b"",
            "big": 2**62,
            "neg_big": -(2**60),
            "safe": 42,
            "nothing": None,
            "doc": json.dumps({"$int": "5", "nested": {"a": [1, 2]}}),
        }
    )
    db.close()
    return path


def test_typed_json_export(typed_db_path):
    result = CliRunner().invoke(
        cli.cli, ["query", typed_db_path, "select * from t", "--typed-json"]
    )
    assert result.exit_code == 0
    (row,) = json.loads(result.output)
    assert row == {
        "id": 1,
        "name": "héllo 你好",
        "data": {"$blob": "AAECYmxvYg=="},
        "empty_blob": {"$blob": ""},
        "big": {"$int": str(2**62)},
        "neg_big": {"$int": str(-(2**60))},
        "safe": 42,
        "nothing": None,
        # Text columns are left alone without --json-cols
        "doc": '{"$int": "5", "nested": {"a": [1, 2]}}',
    }


def test_typed_json_export_escapes_marker_collisions(typed_db_path):
    result = CliRunner().invoke(
        cli.cli,
        ["query", typed_db_path, "select * from t", "--typed-json", "--json-cols"],
    )
    assert result.exit_code == 0
    (row,) = json.loads(result.output)
    # The user object containing a "$int" key must be escaped, not
    # mistaken for a marker
    assert row["doc"] == {"$escape": {"$int": "5", "nested": {"a": [1, 2]}}}


def test_typed_json_export_nl(typed_db_path):
    result = CliRunner().invoke(
        cli.cli, ["query", typed_db_path, "select * from t", "--typed-json", "--nl"]
    )
    assert result.exit_code == 0
    (row,) = [json.loads(line) for line in result.output.splitlines() if line.strip()]
    assert row["data"] == {"$blob": "AAECYmxvYg=="}
    assert row["big"] == {"$int": str(2**62)}


def test_default_json_output_unchanged(typed_db_path):
    # Without --typed-json the output keeps the previous format
    result = CliRunner().invoke(cli.cli, ["query", typed_db_path, "select * from t"])
    assert result.exit_code == 0
    (row,) = json.loads(result.output)
    assert row["data"] == {"$base64": True, "encoded": "AAECYmxvYg=="}
    # Integers stay plain JSON numbers, not strings
    assert row["big"] == 2**62
    assert row["safe"] == 42


def test_typed_json_round_trip(typed_db_path, tmpdir):
    exported = CliRunner().invoke(
        cli.cli,
        ["query", typed_db_path, "select * from t", "--typed-json", "--json-cols"],
    ).output
    path2 = str(tmpdir / "roundtrip.db")
    result = CliRunner().invoke(
        cli.cli, ["insert", path2, "t", "-", "--typed-json"], input=exported
    )
    assert result.exit_code == 0, result.output
    before = list(Database(typed_db_path)["t"].rows)
    after = list(Database(path2)["t"].rows)
    assert before == after


def test_typed_json_round_trip_nl(typed_db_path, tmpdir):
    exported = CliRunner().invoke(
        cli.cli,
        ["query", typed_db_path, "select * from t", "--typed-json", "--nl"],
    ).output
    path2 = str(tmpdir / "roundtrip_nl.db")
    result = CliRunner().invoke(
        cli.cli, ["insert", path2, "t", "-", "--nl", "--typed-json"], input=exported
    )
    assert result.exit_code == 0, result.output
    before = list(Database(typed_db_path)["t"].rows)
    after = list(Database(path2)["t"].rows)
    assert before == after


@pytest.mark.parametrize(
    "doc,error,field",
    [
        # Unknown markers must not silently pass through as objects
        ('{"id": 1, "v": {"$wat": 1}}', "unknown typed JSON marker", "'v'"),
        (
            '{"id": 1, "v": {"$blob": "QQ==", "x": 1}}',
            "unknown typed JSON marker",
            "'v'",
        ),
        # Corrupt encodings
        ('{"id": 1, "v": {"$blob": "!!!"}}', "invalid base64", "'v'"),
        ('{"id": 1, "v": {"$blob": 5}}', "must be a base64 string", "'v'"),
        # Out-of-range and malformed integers
        (
            '{"id": 1, "v": {"$int": "9223372036854775808"}}',
            "outside the SQLite 64-bit range",
            "'v'",
        ),
        ('{"id": 1, "v": {"$int": "12x3"}}', "decimal integer string", "'v'"),
        # Nested field paths are reported
        ('{"id": 1, "v": {"a": {"$blob": "?"}}}', "invalid base64", "'v.a'"),
    ],
)
def test_typed_json_import_errors_report_line_and_field(tmpdir, doc, error, field):
    path = str(tmpdir / "errors.db")
    body = '{"id": 0, "v": null}\n\n' + doc + "\n"
    result = CliRunner().invoke(
        cli.cli, ["insert", path, "t", "-", "--nl", "--typed-json"], input=body
    )
    assert result.exit_code != 0
    # Line 3, since line 1 is a valid record and line 2 is blank
    assert "line 3" in result.output
    assert field in result.output
    assert error in result.output


def test_typed_json_import_error_item_index_for_json_array(tmpdir):
    path = str(tmpdir / "errors.db")
    result = CliRunner().invoke(
        cli.cli,
        ["insert", path, "t", "-", "--typed-json"],
        input='[{"id": 1}, {"id": 2, "v": {"$blob": "?"}}]',
    )
    assert result.exit_code != 0
    assert "item 2" in result.output
    assert "'v'" in result.output


def test_typed_json_import_rejects_non_json_formats(tmpdir):
    path = str(tmpdir / "data.db")
    for extra in (["--csv"], ["--tsv"], ["--lines"], ["--text"]):
        result = CliRunner().invoke(
            cli.cli, ["insert", path, "t", "-", "--typed-json"] + extra, input="x"
        )
        assert result.exit_code != 0
        assert "--typed-json can only be used with JSON input" in result.output


def test_typed_json_import_escaped_objects_stay_objects(tmpdir):
    path = str(tmpdir / "escaped.db")
    result = CliRunner().invoke(
        cli.cli,
        ["insert", path, "t", "-", "--typed-json"],
        input='[{"id": 1, "v": {"$escape": {"$int": "5", "$blob": "x"}}}]',
    )
    assert result.exit_code == 0, result.output
    (row,) = Database(path)["t"].rows
    # Decodes to a plain object, stored as JSON text
    assert row["v"] == '{"$int": "5", "$blob": "x"}'


def test_typed_json_encode_decode_unit():
    value = {
        "blob": b"\x00\xff",
        "empty": b"",
        "big": JSON_MAX_SAFE_INTEGER + 1,
        "neg_big": -(JSON_MAX_SAFE_INTEGER + 1),
        "safe": JSON_MAX_SAFE_INTEGER,
        "zero": 0,
        "truthy": True,
        "nothing": None,
        "text": "héllo 你好",
        "nested": [{"$int": "looks like a marker"}, [b"x", 2**62]],
        "collision": {"$escape": {"deep": [1]}},
    }
    encoded = typed_json_encode(value)
    # Only unsafe integers become markers
    assert encoded["safe"] == JSON_MAX_SAFE_INTEGER
    assert encoded["zero"] == 0
    assert encoded["big"] == {"$int": str(JSON_MAX_SAFE_INTEGER + 1)}
    # Marker-shaped user objects are escaped
    assert encoded["nested"][0] == {"$escape": {"$int": "looks like a marker"}}
    assert encoded["collision"] == {"$escape": {"$escape": {"deep": [1]}}}
    assert typed_json_decode(encoded) == value


def test_typed_json_decode_error_has_field():
    with pytest.raises(TypedJsonDecodeError) as excinfo:
        typed_json_decode({"a": {"b": {"$unknown": 1}}})
    assert excinfo.value.field == "a.b"
