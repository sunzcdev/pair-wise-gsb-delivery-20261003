import os

import pytest
from click.testing import CliRunner

from sqlite_utils import Database, cli
from sqlite_utils.db import (
    COUNTS_COUNT_MISMATCH,
    COUNTS_MISSING_CACHE,
    COUNTS_STALE_CACHE,
    COUNTS_TRIGGER_MISSING,
    CountsCheck,
    Table,
)
from sqlite_utils.utils import sqlite3


@pytest.fixture
def workdir():
    # A default-mode directory under the project root. pytest's tmp_path
    # directories are created with restrictive 0o700 permissions, which this
    # Windows mount cannot open SQLite files inside.
    path = os.path.join(os.path.dirname(__file__), "_counts_check_tmp")
    os.makedirs(path, exist_ok=True)
    return path


@pytest.fixture
def db_path(workdir):
    path = os.path.join(workdir, "counts.db")
    if os.path.exists(path):
        os.remove(path)
    db = Database(path)
    db["foo"].insert_all([{"id": i} for i in range(10)], pk="id")
    db["bar"].insert_all([{"id": i} for i in range(3)], pk="id")
    db.create_view("myview", "select * from foo")
    db.enable_counts()
    db.close()
    return path


def test_check_counts_clean(db_path):
    db = Database(db_path)
    checks = db.check_counts()
    assert checks == [
        CountsCheck("bar", True, 3, 3, []),
        CountsCheck("foo", True, 10, 10, []),
    ]


def test_check_counts_distinguishes_issue_types(db_path):
    db = Database(db_path)
    # count drift on foo
    db.table("_counts").update("foo", {"count": 99})
    # trigger damage on bar (one trigger dropped)
    db.execute('drop trigger "bar_counts_delete"')
    # a table that has no cache row at all
    db["baz"].insert({"id": 1})
    # stale row left behind by a dropped table
    db["gone"].insert({"id": 1})
    db.table("gone").enable_counts()
    db.table("gone").drop()

    by_table = {check.table: check for check in db.check_counts()}

    assert by_table["foo"].issues == [COUNTS_COUNT_MISMATCH]
    assert by_table["foo"].actual_count == 10
    assert by_table["foo"].cached_count == 99

    assert by_table["bar"].issues == [COUNTS_TRIGGER_MISSING]
    assert by_table["bar"].actual_count == 3

    assert by_table["baz"].issues == [
        COUNTS_MISSING_CACHE,
        COUNTS_TRIGGER_MISSING,
    ]
    assert by_table["baz"].actual_count == 1
    assert by_table["baz"].cached_count is None

    assert by_table["gone"].issues == [COUNTS_STALE_CACHE]
    assert by_table["gone"].exists is False
    assert by_table["gone"].actual_count is None


def test_check_counts_is_read_only(db_path):
    before = Database(db_path)
    counts_before = before.cached_counts()
    before.close()

    db = Database(db_path)
    db.table("_counts").update("foo", {"count": 99})
    db.execute('drop trigger "bar_counts_delete"')
    problems = [check for check in db.check_counts() if check.issues]
    assert len(problems) == 2
    # Capture the damaged state, then confirm a further check changes nothing.
    schema_after_check = db.schema
    cached_after_check = db.cached_counts()
    triggers_after_check = set(db.triggers_dict)
    db.check_counts()
    assert db.schema == schema_after_check
    assert db.cached_counts() == cached_after_check
    assert set(db.triggers_dict) == triggers_after_check
    db.close()

    after = Database(db_path)
    # The damage must still be present - check_counts() repaired nothing.
    assert after.cached_counts()["foo"] == 99
    assert "bar_counts_delete" not in after.triggers_dict
    assert counts_before  # sanity: fixture really had cached data
    after.close()


def test_repair_counts_fixes_everything_in_one_transaction(db_path):
    db = Database(db_path)
    db.table("_counts").update("foo", {"count": 99})
    db.execute('drop trigger "bar_counts_delete"')
    db["baz"].insert({"id": 1})
    db["gone"].insert({"id": 1})
    db.table("gone").enable_counts()
    db.table("gone").drop()

    repaired = db.repair_counts()
    repaired_tables = {check.table: check.issues for check in repaired}
    assert repaired_tables["foo"] == [COUNTS_COUNT_MISMATCH]
    assert repaired_tables["bar"] == [COUNTS_TRIGGER_MISSING]
    assert repaired_tables["baz"] == [
        COUNTS_MISSING_CACHE,
        COUNTS_TRIGGER_MISSING,
    ]
    assert repaired_tables["gone"] == [COUNTS_STALE_CACHE]

    # Everything is now clean, including the freshly cached table.
    assert all(not check.issues for check in db.check_counts())
    assert db.table("bar").has_counts_triggers
    assert "gone" not in db.cached_counts()
    assert db.cached_counts()["baz"] == 1


def test_repair_counts_rolls_back_on_failure(db_path):
    db = Database(db_path)
    db.table("_counts").update("foo", {"count": 99})
    original = db.cached_counts()

    # Force a failure partway through the multi-table repair by making one of
    # the generated trigger statements syntactically invalid.
    db.execute('drop trigger "foo_counts_insert"')
    real_method = Table._counts_trigger_sqls

    def broken_sqls(self):
        sqls = real_method(self)
        if self.name == "foo":
            sqls["foo_counts_insert"] = "CREATE TRIGGER OOPS SYNTAX ERROR"
        return sqls

    Table._counts_trigger_sqls = broken_sqls
    with pytest.raises(sqlite3.Error):
        try:
            db.repair_counts()
        finally:
            Table._counts_trigger_sqls = real_method

    # The pre-failure cached value is preserved (no partial repair).
    assert db.cached_counts()["foo"] == original["foo"] == 99
    # The trigger dropped before the failed repair is still absent, proving
    # the whole transaction rolled back rather than partially applying.
    assert "foo_counts_insert" not in db.triggers_dict
    # A subsequent, healthy repair completes and restores everything.
    db.repair_counts()
    assert all(not check.issues for check in db.check_counts())


def test_repair_keeps_counts_up_to_date_from_another_connection(db_path):
    db = Database(db_path)
    # A separate connection (an external tool) writes rows directly.
    other = sqlite3.connect(db_path)
    other.execute("insert into foo (id) values (11)")
    other.commit()
    other.close()

    db.table("_counts").update("foo", {"count": 0})
    db.repair_counts()

    other = sqlite3.connect(db_path)
    other.execute("insert into foo (id) values (12)")
    other.execute("delete from foo where id = 12")
    other.commit()
    other.close()

    assert db.cached_counts()["foo"] == 11
    # Trigger-driven updates keep working after the repair.
    db["foo"].insert({"id": 13})
    assert db.cached_counts()["foo"] == 12


def test_quoted_table_names_round_trip(db_path):
    db = Database(db_path)
    name = 'weird "quoted" name'
    table = db.table(name)
    table.insert_all([{"v": 1}, {"v": 2}, {"v": 3}])
    table.enable_counts()
    assert table.has_counts_triggers
    assert db.cached_counts([name]) == {name: 3}
    assert all(not check.issues for check in db.check_counts())

    # Damage the trigger and confirm the check/repair handles quoting.
    insert_trigger = table._counts_trigger_names[0]
    db.execute(f'drop trigger "{insert_trigger.replace(chr(34), chr(34) * 2)}"')
    check = next(c for c in db.check_counts() if c.table == name)
    assert check.issues == [COUNTS_TRIGGER_MISSING]
    db.repair_counts()
    assert table.has_counts_triggers
    table.insert({"v": 4})
    assert db.cached_counts([name]) == {name: 4}


def test_views_and_system_tables_are_not_repaired(db_path):
    db = Database(db_path)
    # The view must never appear as an eligible data table in the audit.
    audited = {check.table for check in db.check_counts()}
    assert "myview" not in audited
    assert db._counts_table_name not in audited

    # A stale cache row that points at a view is treated as stale, not data.
    db.execute(
        'insert into _counts ("table", count) values ("myview", 123)'
    )
    check = next(c for c in db.check_counts() if c.table == "myview")
    assert check.issues == [COUNTS_STALE_CACHE]
    db.repair_counts()
    assert "myview" not in db.cached_counts()


def test_virtual_tables_are_excluded(workdir):
    path = os.path.join(workdir, "virtual.db")
    if os.path.exists(path):
        os.remove(path)
    db = Database(path)
    db["docs"].insert_all([{"title": "a"}, {"title": "b"}], pk="title")
    db["docs"].enable_fts(["title"])
    db.enable_counts()
    # A cache row naming the FTS virtual table is stale, not a data table.
    db.execute(
        'insert or replace into _counts ("table", count) values ("docs_fts", 123)'
    )
    audited = {check.table for check in db.check_counts()}
    # The FTS virtual table itself is never audited as a data table.
    fts_check = next(c for c in db.check_counts() if c.table == "docs_fts")
    assert fts_check.issues == [COUNTS_STALE_CACHE]
    assert fts_check.exists is False
    assert "docs" in audited
    # Every existing, audited object is a non-virtual data table.
    eligible = {t.name for t in db._counts_eligible_tables()}
    assert {c.table for c in db.check_counts() if c.exists} == eligible
    assert "docs_fts" not in eligible
    db.repair_counts()
    assert "docs_fts" not in db.cached_counts()
    assert all(not c.issues for c in db.check_counts())


def test_check_counts_without_counts_table(fresh_db):
    fresh_db["only"].insert({"v": 1})
    check = next(iter(fresh_db.check_counts()))
    assert check.table == "only"
    assert check.issues == [COUNTS_MISSING_CACHE, COUNTS_TRIGGER_MISSING]
    repaired = fresh_db.repair_counts()
    assert [c.table for c in repaired] == ["only"]
    assert all(not c.issues for c in fresh_db.check_counts())


def test_altered_trigger_body_is_detected_and_repaired(db_path):
    db = Database(db_path)
    name = "foo_counts_insert"
    db.execute(f'drop trigger "{name}"')
    db.execute(
        f'create trigger "{name}" after insert on "foo" '
        'begin select 1; end'
    )
    check = next(c for c in db.check_counts() if c.table == "foo")
    assert check.issues == [COUNTS_TRIGGER_MISSING]
    db.repair_counts()
    assert db.table("foo").has_counts_triggers
    assert all(not c.issues for c in db.check_counts())


def test_cli_check_counts_clean(db_path):
    result = CliRunner().invoke(cli.cli, ["check-counts", db_path])
    assert result.exit_code == 0
    assert result.output == ""


def test_cli_check_counts_reports_and_fails(db_path):
    db = Database(db_path)
    db.table("_counts").update("foo", {"count": 99})
    db.execute('drop trigger "bar_counts_delete"')
    db.close()

    result = CliRunner().invoke(cli.cli, ["check-counts", db_path])
    assert result.exit_code != 0
    assert "foo: count mismatch (cached=99, actual=10)" in result.output
    assert "bar: missing or altered count trigger(s)" in result.output

    # The check command must not have repaired anything.
    assert Database(db_path).cached_counts()["foo"] == 99


def test_cli_repair_counts(db_path):
    db = Database(db_path)
    db.table("_counts").update("foo", {"count": 99})
    db.close()

    result = CliRunner().invoke(cli.cli, ["repair-counts", db_path])
    assert result.exit_code == 0
    assert "foo: repaired count-mismatch" in result.output

    fixed = Database(db_path)
    assert fixed.cached_counts()["foo"] == 10
    assert all(not c.issues for c in fixed.check_counts())
