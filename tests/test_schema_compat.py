import os
import os
import sqlite3
import tempfile
import tempfile

import pytest

from sqlite_utils import (
    CompatibilityLevel,
    Database,
    ObjectType,
    check_schema_compatibility,
)
from sqlite_utils.schema_compat import _normalize_default


def make_db(path, statements):
    conn = sqlite3.connect(str(path))
    conn.executescript(statements)
    conn.commit()
    conn.close()
    return str(path)


def memory_db(statements):
    conn = sqlite3.connect(":memory:")
    conn.executescript(statements)
    return conn


def findings_of_type(report, object_type):
    return [f for f in report.findings if f.object_type == object_type]


def test_identical_schema_is_compatible():
    contract = memory_db(
        "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL,"
        " note TEXT DEFAULT 'x');"
    )
    target = memory_db(
        "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL,"
        " note TEXT DEFAULT 'x');"
    )
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.COMPATIBLE
    assert report.findings == []
    assert report.compatible_tables == ["t"]


def test_layout_and_affinity_equivalent_types_are_compatible():
    # Different CREATE SQL layout and quoting, same structure. VARCHAR and
    # TEXT share the TEXT affinity, so they compare equal.
    contract = memory_db(
        'CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL, note TEXT DEFAULT "x");'
    )
    target = memory_db(
        "create table t (\n  id integer primary key,\n"
        "  name varchar(20) not null,\n  note text default ('x')\n);"
    )
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.COMPATIBLE
    assert report.compatible_tables == ["t"]


def test_affinity_mismatch_requires_conversion():
    contract = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT);")
    target = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.CONVERSION_REQUIRED
    (finding,) = findings_of_type(report, ObjectType.COLUMN)
    assert finding.level == CompatibilityLevel.CONVERSION_REQUIRED
    assert finding.object_name == "t.a"
    assert "affinity" in finding.reason


def test_not_null_relaxation_requires_conversion():
    contract = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT NOT NULL);")
    target = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.CONVERSION_REQUIRED
    (finding,) = report.findings
    assert "NOT NULL" in finding.reason


def test_target_stricter_not_null_is_compatible():
    # A stricter target constraint never breaks contract-shaped writes.
    contract = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT);")
    target = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT NOT NULL);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.COMPATIBLE


def test_default_value_mismatch_requires_conversion():
    contract = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER DEFAULT 0);")
    target = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER DEFAULT 1);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.CONVERSION_REQUIRED
    (finding,) = report.findings
    assert "default" in finding.reason.lower()


def test_missing_table_requires_conversion():
    contract = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY);")
    target = memory_db("CREATE TABLE other (id INTEGER PRIMARY KEY);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.CONVERSION_REQUIRED
    (finding,) = findings_of_type(report, ObjectType.TABLE)
    assert finding.object_name == "t"


def test_new_not_null_column_with_default_differs_from_without_default():
    contract = memory_db(
        "CREATE TABLE t (id INTEGER PRIMARY KEY,"
        " with_default TEXT NOT NULL DEFAULT 'd',"
        " without_default TEXT NOT NULL,"
        " nullable TEXT);"
    )
    target = memory_db(
        "CREATE TABLE t (id INTEGER PRIMARY KEY); INSERT INTO t VALUES (1);"
    )
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.INCOMPATIBLE
    by_name = {f.object_name: f for f in report.findings}
    # A defaulted NOT NULL column can be added - explicit conversion.
    assert (
        by_name["t.with_default"].level == CompatibilityLevel.CONVERSION_REQUIRED
    )
    # A NOT NULL column without a default cannot be added to a table with
    # existing rows - incompatible, and not lumped together with the above.
    assert by_name["t.without_default"].level == CompatibilityLevel.INCOMPATIBLE
    assert "existing row" in by_name["t.without_default"].reason
    assert by_name["t.nullable"].level == CompatibilityLevel.CONVERSION_REQUIRED


def test_new_not_null_column_without_default_on_empty_table_is_convertible():
    contract = memory_db(
        "CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT NOT NULL);"
    )
    target = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.CONVERSION_REQUIRED
    (finding,) = report.findings
    assert finding.level == CompatibilityLevel.CONVERSION_REQUIRED


def test_extra_target_not_null_column_without_default_is_incompatible():
    contract = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY);")
    target = memory_db(
        "CREATE TABLE t (id INTEGER PRIMARY KEY, extra TEXT NOT NULL);"
        " INSERT INTO t VALUES (1, 'x');"
    )
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.INCOMPATIBLE
    (finding,) = report.findings
    assert finding.object_name == "t.extra"


def test_extra_nullable_target_column_is_compatible():
    contract = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY);")
    target = memory_db(
        "CREATE TABLE t (id INTEGER PRIMARY KEY, extra TEXT DEFAULT 'e');"
    )
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.COMPATIBLE


def test_primary_key_order_difference_requires_conversion():
    contract = memory_db("CREATE TABLE t (a INTEGER, b INTEGER, PRIMARY KEY (a, b));")
    target = memory_db("CREATE TABLE t (a INTEGER, b INTEGER, PRIMARY KEY (b, a));")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.CONVERSION_REQUIRED
    (finding,) = findings_of_type(report, ObjectType.PRIMARY_KEY)
    assert "order" in finding.reason


def test_unique_constraint_differences_require_conversion():
    contract = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, code TEXT UNIQUE);")
    target = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, code TEXT);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.CONVERSION_REQUIRED
    (finding,) = findings_of_type(report, ObjectType.UNIQUE_CONSTRAINT)
    assert finding.detail["direction"] == "missing_in_target"


def test_extra_unique_constraint_in_target_requires_conversion():
    contract = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, code TEXT);")
    target = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, code TEXT UNIQUE);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.CONVERSION_REQUIRED
    (finding,) = findings_of_type(report, ObjectType.UNIQUE_CONSTRAINT)
    assert finding.detail["direction"] == "extra_in_target"


def test_foreign_key_missing_in_target_requires_conversion():
    contract = memory_db(
        "CREATE TABLE p (id INTEGER PRIMARY KEY);"
        " CREATE TABLE t (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES p(id));"
    )
    target = memory_db(
        "CREATE TABLE p (id INTEGER PRIMARY KEY);"
        " CREATE TABLE t (id INTEGER PRIMARY KEY, pid INTEGER);"
    )
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.CONVERSION_REQUIRED
    (finding,) = findings_of_type(report, ObjectType.FOREIGN_KEY)
    assert finding.detail["direction"] == "missing_in_target"


def test_foreign_key_to_missing_table_is_incompatible():
    contract = memory_db(
        "CREATE TABLE t (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES ghost(id));"
    )
    target = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, pid INTEGER);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.INCOMPATIBLE
    (finding,) = findings_of_type(report, ObjectType.FOREIGN_KEY)
    assert "ghost" in finding.reason


def test_foreign_key_action_mismatch_requires_conversion():
    contract = memory_db(
        "CREATE TABLE p (id INTEGER PRIMARY KEY);"
        " CREATE TABLE t (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES p(id) ON DELETE CASCADE);"
    )
    target = memory_db(
        "CREATE TABLE p (id INTEGER PRIMARY KEY);"
        " CREATE TABLE t (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES p(id) ON DELETE SET NULL);"
    )
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.CONVERSION_REQUIRED
    (finding,) = findings_of_type(report, ObjectType.FOREIGN_KEY)
    assert "differs" in finding.reason


def test_expression_index_is_reported_unknown():
    contract = memory_db(
        "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT);"
        " CREATE INDEX idx_name ON t (lower(name));"
    )
    target = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.UNKNOWN
    (finding,) = report.unknown
    assert finding.object_type == ObjectType.INDEX
    assert "expression" in finding.reason.lower()


def test_generated_column_is_reported_unknown():
    contract = memory_db(
        "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT,"
        " g TEXT GENERATED ALWAYS AS (upper(name)));"
    )
    target = memory_db(
        "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT,"
        " g TEXT GENERATED ALWAYS AS (lower(name)));"
    )
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.UNKNOWN
    (finding,) = report.unknown
    assert finding.object_name == "t.g"
    assert "generated" in finding.reason.lower()


def test_virtual_table_is_reported_unknown():
    contract = memory_db("CREATE VIRTUAL TABLE t USING fts5(body);")
    target = memory_db("CREATE TABLE t (body TEXT);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.UNKNOWN
    assert any(f.object_name == "t" for f in report.unknown)


def test_names_with_spaces_and_quotes_match():
    contract = memory_db(
        'CREATE TABLE "we ird""t" ("col 1" TEXT PRIMARY KEY, "col""2" INTEGER NOT NULL);'
    )
    target = memory_db(
        'CREATE TABLE "we ird""t" ("col 1" TEXT PRIMARY KEY, "col""2" INTEGER NOT NULL);'
    )
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.COMPATIBLE
    assert report.compatible_tables == ['we ird"t']


def temp_db_path():
    # Files are created directly in the system temp directory so the check
    # can run in sandboxes that disallow creating new subdirectories.
    fd, path = tempfile.mkstemp(suffix=".db", prefix="schema-compat-")
    os.close(fd)
    os.unlink(path)
    return path


def test_paths_are_opened_read_only():
    contract_path = temp_db_path()
    target_path = temp_db_path()
    try:
        make_db(contract_path, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT);")
        make_db(target_path, "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT);")
        before = os.stat(target_path)
        report = check_schema_compatibility(contract_path, target_path)
        after = os.stat(target_path)
        assert report.verdict == CompatibilityLevel.COMPATIBLE
        assert before.st_size == after.st_size
        assert before.st_mtime_ns == after.st_mtime_ns
    finally:
        for path in (contract_path, target_path):
            if os.path.exists(path):
                os.unlink(path)


def test_accepts_database_objects():
    path = temp_db_path()
    try:
        db = Database(path)
        db["t"].insert({"id": 1, "name": "x"}, pk="id")
        report = check_schema_compatibility(db, db)
        assert report.verdict == CompatibilityLevel.COMPATIBLE
        db.close()
    finally:
        if os.path.exists(path):
            os.unlink(path)


def test_empty_contract_is_incompatible():
    contract = memory_db("")
    target = memory_db("CREATE TABLE t (id INTEGER PRIMARY KEY);")
    report = check_schema_compatibility(contract, target)
    assert report.verdict == CompatibilityLevel.INCOMPATIBLE


@pytest.mark.parametrize(
    "contract_default,target_default,expected_equal",
    [
        ("0", "0", True),
        ("(0)", "0", True),
        ("1", "1.0", True),
        ("'x'", "'x'", True),
        ('"x"', "'x'", True),
        ("NULL", "null", True),
        ("CURRENT_TIMESTAMP", "current_timestamp", True),
        ("'it''s'", "'it''s'", True),
        ("0", "1", False),
        ("'x'", "'y'", False),
        ("NULL", None, False),
    ],
)
def test_normalize_default(contract_default, target_default, expected_equal):
    assert (
        _normalize_default(contract_default) == _normalize_default(target_default)
    ) == expected_equal