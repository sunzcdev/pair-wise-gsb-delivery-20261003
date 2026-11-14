import hashlib
import os
import pathlib

import pytest
from click.testing import CliRunner

from sqlite_utils import Database, cli


def _invoke(*args, **kwargs):
    return CliRunner().invoke(cli.cli, list(args), catch_exceptions=False, **kwargs)


def test_insert_files_dedupe_stores_content_once(tmp_path):
    root = tmp_path / "tree"
    (root / "d1").mkdir(parents=True)
    (root / "d2").mkdir()
    (root / "d1" / "same.txt").write_text("identical contents", "utf-8")
    (root / "d2" / "same.txt").write_text("identical contents", "utf-8")
    (root / "empty.txt").write_bytes(b"")
    db_path = tmp_path / "files.db"
    result = _invoke(
        "insert-files", str(db_path), "files", str(root), "--dedupe", "--silent"
    )
    assert result.exit_code == 0, result.output
    db = Database(db_path)
    # Three path rows, but only two distinct contents (one shared, one empty)
    assert db.table("files").count == 3
    assert db.table("files_content").count == 2
    file_rows = list(db.table("files").rows)
    paths = {row["path"] for row in file_rows}
    assert paths == {
        os.path.join("d1", "same.txt"),
        os.path.join("d2", "same.txt"),
        "empty.txt",
    }
    # Same-named files in different directories share a single content row
    same_rows = [
        row for row in file_rows if pathlib.Path(row["path"]).name == "same.txt"
    ]
    shared_hash = hashlib.sha256(b"identical contents").hexdigest()
    assert {row["sha256"] for row in same_rows} == {shared_hash}
    content = db.table("files_content").get(shared_hash)
    assert content["content"] == b"identical contents"
    assert content["size"] == 18
    # Each path keeps its own metadata row
    assert len(same_rows) == 2
    assert all(row["mode"] is not None for row in same_rows)
    # Empty files are valid content-addressed rows
    empty = next(row for row in file_rows if row["path"] == "empty.txt")
    empty_content = db.table("files_content").get(empty["sha256"])
    assert empty_content["content"] == b""
    assert empty_content["size"] == 0
    assert empty["size"] == 0
    assert db.table("files").foreign_keys[0].other_table == "files_content"


def test_insert_files_dedupe_update_never_overwrites_shared_content(tmp_path):
    root = tmp_path / "tree"
    (root / "d1").mkdir(parents=True)
    (root / "d2").mkdir()
    (root / "d1" / "same.txt").write_text("AAA", "utf-8")
    (root / "d2" / "same.txt").write_text("AAA", "utf-8")
    db_path = tmp_path / "files.db"
    args = ("insert-files", str(db_path), "files", str(root), "--dedupe", "--silent")
    assert _invoke(*args).exit_code == 0
    old_hash = hashlib.sha256(b"AAA").hexdigest()
    new_hash = hashlib.sha256(b"BBB different").hexdigest()
    # One path gets updated content
    (root / "d1" / "same.txt").write_text("BBB different", "utf-8")
    assert _invoke(*args).exit_code == 0
    db = Database(db_path)
    rows = {row["path"]: row for row in db.table("files").rows}
    p1 = os.path.join("d1", "same.txt")
    p2 = os.path.join("d2", "same.txt")
    # The updated path points at new content; the other path stays linked
    assert rows[p1]["sha256"] == new_hash
    assert rows[p2]["sha256"] == old_hash
    # Both contents still exist: content rows are immutable, never overwritten
    content_hashes = {row["sha256"] for row in db.table("files_content").rows}
    assert content_hashes == {old_hash, new_hash}
    assert db.table("files_content").get(old_hash)["content"] == b"AAA"


def test_insert_files_dedupe_is_idempotent_on_repeat(tmp_path):
    root = tmp_path / "tree"
    (root / "d1").mkdir(parents=True)
    (root / "d2").mkdir()
    (root / "d1" / "same.txt").write_text("AAA", "utf-8")
    (root / "d2" / "same.txt").write_text("AAA", "utf-8")
    (root / "unique.txt").write_text("unique", "utf-8")
    db_path = tmp_path / "files.db"
    args = ("insert-files", str(db_path), "files", str(root), "--dedupe", "--silent")
    for _ in range(3):
        result = _invoke(*args)
        assert result.exit_code == 0, result.output
    db = Database(db_path)
    # Repeating the same batch never accumulates duplicate associations
    assert db.table("files").count == 3
    assert db.table("files_content").count == 2


def test_insert_files_dedupe_interruption_leaves_clear_boundary(
    tmp_path, monkeypatch
):
    from sqlite_utils import cli as cli_module

    root = tmp_path / "tree"
    root.mkdir()
    first = root / "first.txt"
    second = root / "second.txt"
    first.write_text("first", "utf-8")
    second.write_text("second", "utf-8")
    db_path = tmp_path / "files.db"
    real_read = cli_module._read_file_snapshot

    def fail_on_second(file_path):
        if file_path.name == "second.txt":
            raise cli_module.FileChangedDuringReadError(file_path.resolve())
        return real_read(file_path)

    monkeypatch.setattr(cli_module, "_read_file_snapshot", fail_on_second)
    result = CliRunner().invoke(
        cli.cli,
        [
            "insert-files",
            str(db_path),
            "files",
            str(first),
            str(second),
            "--dedupe",
            "--silent",
        ],
    )
    assert result.exit_code == 1
    assert "changed while it was being read" in result.output
    monkeypatch.undo()
    db = Database(db_path)
    # The first file committed; the second file's transaction was rolled back
    assert [row["path"] for row in db.table("files").rows] == [str(first)]
    assert db.table("files_content").count == 1
    # Re-running after the failure completes the batch without duplicates
    result = _invoke(
        "insert-files",
        str(db_path),
        "files",
        str(first),
        str(second),
        "--dedupe",
        "--silent",
    )
    assert result.exit_code == 0, result.output
    db = Database(db_path)
    assert db.table("files").count == 2
    assert db.table("files_content").count == 2


def test_read_file_snapshot_rejects_file_changed_during_read(tmp_path, monkeypatch):
    from sqlite_utils.cli import FileChangedDuringReadError, _read_file_snapshot

    changing = tmp_path / "changing.txt"
    changing.write_bytes(b"x" * 4096)
    real_fstat = os.fstat
    state = {"calls": 0}

    def fake_fstat(fd):
        stat_result = real_fstat(fd)
        state["calls"] += 1
        if state["calls"] == 2:
            # Simulate the file being replaced while the read is in progress
            changing.write_bytes(b"y" * 4096)
            return real_fstat(fd)
        return stat_result

    monkeypatch.setattr(os, "fstat", fake_fstat)
    with pytest.raises(FileChangedDuringReadError):
        _read_file_snapshot(changing)


def test_read_file_snapshot_hashes_exact_bytes_in_one_pass(tmp_path):
    from sqlite_utils.cli import _read_file_snapshot

    data = b"a" * (3 * 1024 * 1024 + 7)
    sample = tmp_path / "sample.bin"
    sample.write_bytes(data)
    snapshot = _read_file_snapshot(sample)
    assert snapshot["content"] == data
    assert snapshot["sha256"] == hashlib.sha256(data).hexdigest()
    assert snapshot["md5"] == hashlib.md5(data).hexdigest()
    assert snapshot["size"] == len(data)


def test_insert_files_dedupe_rejects_incompatible_options(tmp_path):
    db_path = tmp_path / "files.db"
    source = tmp_path / "a.txt"
    source.write_text("x", "utf-8")
    for extra in (
        ("--text",),
        ("--pk", "path"),
        ("-c", "path:path"),
        ("--replace",),
        ("--upsert",),
        ("--alter",),
        ("--encoding", "utf-8"),
    ):
        result = CliRunner().invoke(
            cli.cli,
            ["insert-files", str(db_path), "files", str(source), "--dedupe", *extra],
        )
        assert result.exit_code == 1
        assert "--dedupe cannot be combined" in result.output


def test_insert_files_dedupe_stdin(tmp_path):
    db_path = tmp_path / "files.db"
    result = CliRunner().invoke(
        cli.cli,
        ["insert-files", str(db_path), "files", "-", "--name", "stdin.txt", "--dedupe"],
        input="piped content",
        catch_exceptions=False,
    )
    assert result.exit_code == 0, result.output
    db = Database(db_path)
    row = next(iter(db.table("files").rows))
    assert row["path"] == "stdin.txt"
    assert row["fullpath"] is None
    assert row["mtime"] is None
    content = db.table("files_content").get(row["sha256"])
    assert content["content"] == b"piped content"
