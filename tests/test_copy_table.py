import pathlib
import secrets
import shutil

import pytest

from sqlite_utils import Database
from sqlite_utils.db import (
    TableAlreadyExists,
    TableCopyDependencyError,
)
from sqlite_utils.utils import sqlite3


@pytest.fixture
def temp_db_dir():
    base = pathlib.Path(".test-copy-table-tmp")
    base.mkdir(exist_ok=True)
    directory = base / secrets.token_hex(8)
    directory.mkdir()
    try:
        yield directory
    finally:
        shutil.rmtree(directory, ignore_errors=True)
        try:
            base.rmdir()
        except OSError:
            pass


def _create_database(path, sql):
    conn = sqlite3.connect(str(path))
    try:
        conn.executescript(sql)
        conn.commit()
    finally:
        conn.close()


def test_copy_table_between_attached_databases(temp_db_dir):
    source_path = temp_db_dir / "source.db"
    target_path = temp_db_dir / "target.db"
    _create_database(
        source_path,
        """
        CREATE TABLE "odd.name" (
            "id" INTEGER PRIMARY KEY,
            "value" TEXT NOT NULL DEFAULT 'x',
            "computed" TEXT GENERATED ALWAYS AS (upper("value")) VIRTUAL,
            CHECK (length("value") > 0)
        );
        CREATE INDEX "partial odd" ON "odd.name" ("value")
            WHERE "odd.name"."value" <> 'skip';
        CREATE INDEX "expression odd" ON "odd.name" (lower("value"));
        CREATE TABLE "audit log" ("message" TEXT);
        CREATE TRIGGER "audit odd" AFTER INSERT ON "odd.name" BEGIN
            INSERT INTO "audit log" ("message") VALUES (NEW."value");
        END;
        INSERT INTO "odd.name" ("id", "value") VALUES (1, 'Hello'), (2, 'skip');
        """,
    )
    db = Database(source_path)
    db.attach("other db", target_path)
    db.execute('CREATE TABLE "other db"."audit log" ("message" TEXT)')

    result = db.copy_table(
        "main",
        "odd.name",
        "other db",
        'copy"name',
        copy_triggers=True,
    )

    assert result.rows_copied == 2
    assert result.indexes == ("expression odd", "partial odd")
    assert result.triggers == ("audit odd",)
    assert db.execute(
        'SELECT "id", "value", "computed" FROM "other db"."copy""name" '
        'ORDER BY "id"'
    ).fetchall() == [(1, "Hello", "HELLO"), (2, "skip", "SKIP")]
    columns = db.execute(
        'PRAGMA "other db".table_xinfo("copy""name")'
    ).fetchall()
    assert columns[2] == (2, "computed", "TEXT", 0, None, 0, 2)
    objects = db.execute(
        'SELECT type, name, sql FROM "other db".sqlite_master '
        "WHERE type IN ('index', 'trigger') ORDER BY type, name"
    ).fetchall()
    assert [row[:2] for row in objects] == [
        ("index", "expression odd"),
        ("index", "partial odd"),
        ("trigger", "audit odd"),
    ]
    assert objects[0][2] == (
        'CREATE INDEX "expression odd" ON "copy""name" (lower("value"))'
    )
    assert 'ON "copy""name"' in objects[1][2]
    assert "WHERE \"copy\"\"name\".\"value\" <> 'skip'" in objects[1][2]
    assert 'ON "copy""name"' in objects[2][2]
    assert 'INSERT INTO "audit log"' in objects[2][2]

    # Copying data must not fire the copied trigger, and the source is untouched.
    assert db.execute('SELECT * FROM "other db"."audit log"').fetchall() == []
    assert db.table("odd.name").count == 2
    assert db.table("audit log").count == 2

    db.execute(
        'INSERT INTO "other db"."copy""name" ("id", "value") VALUES (3, \'third\')'
    )
    assert db.execute('SELECT * FROM "other db"."audit log"').fetchall() == [
        ("third",)
    ]


def test_copy_table_target_and_replace(temp_db_dir):
    source_path = temp_db_dir / "source.db"
    target_path = temp_db_dir / "target.db"
    _create_database(
        source_path,
        """
        CREATE TABLE source (id INTEGER PRIMARY KEY, value TEXT);
        CREATE INDEX source_value ON source (value) WHERE source.value IS NOT NULL;
        INSERT INTO source VALUES (1, 'new');
        """,
    )
    _create_database(
        target_path,
        """
        CREATE TABLE destination (id INTEGER PRIMARY KEY, value TEXT);
        INSERT INTO destination VALUES (2, 'old');
        """,
    )
    db = Database(source_path)
    db.attach("target", target_path)

    with pytest.raises(TableAlreadyExists, match="Use replace=True"):
        db.copy_table("main", "source", "target", "destination")
    assert db.execute('SELECT * FROM "target"."destination"').fetchall() == [
        (2, "old")
    ]

    result = db.copy_table(
        "main", "source", "target", "destination", replace=True
    )
    assert result.rows_copied == 1
    assert db.execute('SELECT * FROM "target"."destination"').fetchall() == [
        (1, "new")
    ]
    copied_index_sql = db.execute(
        'SELECT sql FROM "target".sqlite_master WHERE type = ? '
        "AND tbl_name = ?",
        ("index", "destination"),
    ).fetchone()[0]
    assert 'ON "destination" (value)' in copied_index_sql
    assert 'WHERE "destination".value IS NOT NULL' in copied_index_sql
    assert db.execute("SELECT * FROM main.source").fetchall() == [(1, "new")]


def test_copy_table_reports_unresolved_foreign_keys(temp_db_dir):
    source_path = temp_db_dir / "source.db"
    target_path = temp_db_dir / "target.db"
    _create_database(
        source_path,
        """
        CREATE TABLE parent (id INTEGER PRIMARY KEY);
        CREATE TABLE child (
            id INTEGER PRIMARY KEY,
            parent_id INTEGER REFERENCES parent(id)
        );
        CREATE TABLE tree (
            id INTEGER PRIMARY KEY,
            parent_id INTEGER REFERENCES "tree"(id)
        );
        INSERT INTO parent VALUES (1);
        INSERT INTO child VALUES (1, 1);
        INSERT INTO tree VALUES (1, NULL);
        """,
    )
    _create_database(
        target_path,
        'CREATE TABLE parent (id INTEGER PRIMARY KEY, note TEXT);',
    )
    db = Database(source_path)
    db.attach("target", target_path)

    with pytest.raises(TableCopyDependencyError) as error:
        db.copy_table("main", "child", "target", "child")
    assert "parent" in str(error.value)
    assert "will not be redirected" in str(error.value)
    assert db.execute(
        'SELECT name FROM "target".sqlite_master '
        "WHERE type = 'table' ORDER BY name"
    ).fetchall() == [("parent",)]

    result = db.copy_table("main", "tree", "target", "tree_copy")
    assert result.rows_copied == 1
    create_sql = db.execute(
        'SELECT sql FROM "target".sqlite_master WHERE name = ?',
        ("tree_copy",),
    ).fetchone()[0]
    assert 'REFERENCES "tree_copy"(id)' in create_sql
    assert db.execute(
        'PRAGMA "target".foreign_key_list("tree_copy")'
    ).fetchall()[0][2] == "tree_copy"


def test_copy_table_does_not_split_dotted_names(temp_db_dir):
    source_path = temp_db_dir / "source.db"
    target_path = temp_db_dir / "target.db"
    _create_database(
        source_path,
        '''
        CREATE TABLE "main.source" (id INTEGER PRIMARY KEY, value TEXT);
        INSERT INTO "main.source" VALUES (1, 'value');
        ''',
    )
    db = Database(source_path)
    db.attach("target", target_path)

    result = db.copy_table(
        "main", "main.source", "target", "target.destination"
    )
    assert result.rows_copied == 1
    assert db.execute(
        'SELECT * FROM "target"."target.destination"'
    ).fetchall() == [(1, "value")]
    with pytest.raises(ValueError, match="Unknown database alias"):
        db.copy_table("main.nope", "source", "target", "destination")


def test_copy_table_rolls_back_after_source_read_failure(
    temp_db_dir, monkeypatch
):
    source_path = temp_db_dir / "source.db"
    target_path = temp_db_dir / "target.db"
    _create_database(
        source_path,
        """
        CREATE TABLE source (id INTEGER PRIMARY KEY, value TEXT);
        INSERT INTO source VALUES (1, 'new');
        """,
    )
    _create_database(
        target_path,
        """
        CREATE TABLE destination (id INTEGER PRIMARY KEY, value TEXT);
        CREATE INDEX destination_value ON destination (value);
        CREATE TRIGGER destination_trigger AFTER INSERT ON destination BEGIN
            UPDATE destination SET value = upper(value) WHERE id = NEW.id;
        END;
        INSERT INTO destination VALUES (2, 'old');
        """,
    )
    db = Database(source_path)
    db.attach("target", target_path)
    original_execute = db.execute
    source_schema_before = db.execute(
        "SELECT type, name, sql FROM main.sqlite_master ORDER BY type, name"
    ).fetchall()

    def execute_with_reader_failure(sql, parameters=None):
        if sql.startswith('INSERT INTO "target"."destination"'):
            raise sqlite3.OperationalError("simulated source read failure")
        return original_execute(sql, parameters)

    monkeypatch.setattr(db, "execute", execute_with_reader_failure)
    with pytest.raises(sqlite3.OperationalError, match="source read failure"):
        db.copy_table(
            "main", "source", "target", "destination", replace=True
        )

    monkeypatch.setattr(db, "execute", original_execute)
    assert db.execute('SELECT * FROM "target"."destination"').fetchall() == [
        (2, "OLD")
    ]
    assert db.execute(
        'SELECT type, name FROM "target".sqlite_master ORDER BY type, name'
    ).fetchall() == [
        ("index", "destination_value"),
        ("table", "destination"),
        ("trigger", "destination_trigger"),
    ]
    assert db.execute("SELECT * FROM main.source").fetchall() == [(1, "new")]
    assert db.execute(
        "SELECT type, name, sql FROM main.sqlite_master ORDER BY type, name"
    ).fetchall() == source_schema_before
