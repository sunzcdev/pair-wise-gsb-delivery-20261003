from __future__ import annotations

import json
import logging
import os
import shutil
from typing import TYPE_CHECKING

import pytest

from virtualenv.app_data import cache_migration
from virtualenv.app_data.cache_migration import cache_dir_with_migration

if TYPE_CHECKING:
    from pathlib import Path

_MIGRATION_MARKER = ".virtualenv-migration-incomplete"
_LOGGER_NAME = "virtualenv.app_data.cache_migration"


@pytest.fixture
def app_data_dirs(tmp_path, monkeypatch):
    """Point the platformdirs locations at a disk temp dir, the new layout nested inside the old one."""
    old_dir = tmp_path / "data"
    new_dir = old_dir / "Cache"
    monkeypatch.setattr(cache_migration, "user_data_dir", lambda **_: str(old_dir))
    monkeypatch.setattr(cache_migration, "user_cache_dir", lambda **_: str(new_dir))
    return old_dir, new_dir


def _write_json(path: Path, content: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(content), encoding="utf-8")


def _populate_old_layout(old_dir: Path) -> None:
    _write_json(old_dir / "py_info" / "5" / "abc123.json", {"executable": "python"})
    _write_json(old_dir / "wheel" / "3.11" / "embed" / "3" / "pip.json", {"completed": "2026-11-19T00:00:00"})
    image = old_dir / "wheel" / "3.11" / "image" / "1" / "CopyPipInstall" / "pip-20.1.1" / "pip" / "__init__.py"
    image.parent.mkdir(parents=True, exist_ok=True)
    image.write_text("# image payload", encoding="utf-8")


def _embed_log(root: Path) -> Path:
    return root / "wheel" / "3.11" / "embed" / "3" / "pip.json"


def _image_file(root: Path) -> Path:
    return root / "wheel" / "3.11" / "image" / "1" / "CopyPipInstall" / "pip-20.1.1" / "pip" / "__init__.py"


def test_migration_moves_old_layout_into_nested_new_dir(app_data_dirs):
    old_dir, new_dir = app_data_dirs
    _populate_old_layout(old_dir)

    result = cache_dir_with_migration()

    assert result == str(new_dir)
    assert json.loads((new_dir / "py_info" / "5" / "abc123.json").read_text(encoding="utf-8")) == {
        "executable": "python"
    }
    assert json.loads(_embed_log(new_dir).read_text(encoding="utf-8")) == {"completed": "2026-11-19T00:00:00"}
    assert _image_file(new_dir).read_text(encoding="utf-8") == "# image payload"
    assert {entry.name for entry in old_dir.iterdir()} == {"Cache"}
    assert not (old_dir / _MIGRATION_MARKER).exists()


def test_migration_resumes_after_a_failed_move(app_data_dirs, monkeypatch, caplog):
    old_dir, new_dir = app_data_dirs
    _populate_old_layout(old_dir)
    real_move = shutil.move
    failing_entry = os.path.normcase(str(old_dir / "wheel"))

    def fail_on_wheel(src, dst, *args, **kwargs):
        if os.path.normcase(str(src)) == failing_entry:
            msg = "simulated move failure"
            raise OSError(msg)
        return real_move(src, dst, *args, **kwargs)

    monkeypatch.setattr(shutil, "move", fail_on_wheel)
    with caplog.at_level(logging.WARNING, logger=_LOGGER_NAME):
        first = cache_dir_with_migration()

    # the run that cannot finish keeps using the old location and leaves the marker behind
    assert first == str(old_dir)
    assert (old_dir / _MIGRATION_MARKER).is_file()
    assert "could not finish app data migration" in caplog.text
    # the rollback put back what this run moved, so the old layout is whole again
    assert (old_dir / "py_info" / "5" / "abc123.json").is_file()
    assert _embed_log(old_dir).is_file()

    monkeypatch.setattr(shutil, "move", real_move)
    caplog.clear()
    with caplog.at_level(logging.INFO, logger=_LOGGER_NAME):
        second = cache_dir_with_migration()

    # the restart resumes instead of trusting the new directory to be complete, and keeps logs and images
    assert second == str(new_dir)
    assert "resuming app data migration" in caplog.text
    assert json.loads(_embed_log(new_dir).read_text(encoding="utf-8")) == {"completed": "2026-11-19T00:00:00"}
    assert _image_file(new_dir).read_text(encoding="utf-8") == "# image payload"
    assert json.loads((new_dir / "py_info" / "5" / "abc123.json").read_text(encoding="utf-8")) == {
        "executable": "python"
    }
    assert not (old_dir / _MIGRATION_MARKER).exists()


def test_migration_recognizes_entries_moved_before_interruption(app_data_dirs, caplog):
    old_dir, new_dir = app_data_dirs
    _populate_old_layout(old_dir)
    # a crash after py_info was moved but wheel was not, with the marker left behind
    new_dir.mkdir(parents=True)
    shutil.move(str(old_dir / "py_info"), str(new_dir))
    (old_dir / _MIGRATION_MARKER).touch()

    with caplog.at_level(logging.INFO, logger=_LOGGER_NAME):
        result = cache_dir_with_migration()

    assert result == str(new_dir)
    assert "resuming app data migration" in caplog.text
    assert json.loads((new_dir / "py_info" / "5" / "abc123.json").read_text(encoding="utf-8")) == {
        "executable": "python"
    }
    assert json.loads(_embed_log(new_dir).read_text(encoding="utf-8")) == {"completed": "2026-11-19T00:00:00"}
    assert _image_file(new_dir).read_text(encoding="utf-8") == "# image payload"
    assert not (old_dir / _MIGRATION_MARKER).exists()


def test_migration_keeps_in_use_entry_on_conflict(app_data_dirs, caplog):
    old_dir, new_dir = app_data_dirs
    _populate_old_layout(old_dir)
    # the new layout generated its own embed log for the same entry while the old one was not moved yet
    _write_json(new_dir / "wheel" / "3.11" / "embed" / "3" / "pip.json", {"completed": "2026-11-20T00:00:00"})

    with caplog.at_level(logging.WARNING, logger=_LOGGER_NAME):
        result = cache_dir_with_migration()

    assert result == str(new_dir)
    # the in-use copy of the new layout wins
    assert json.loads(_embed_log(new_dir).read_text(encoding="utf-8")) == {"completed": "2026-11-20T00:00:00"}
    # the old copy is left in place, and the warning explains where each copy lives
    assert json.loads(_embed_log(old_dir).read_text(encoding="utf-8")) == {"completed": "2026-11-19T00:00:00"}
    assert "keeping in-use app data entry" in caplog.text
    assert str(new_dir / "wheel") in caplog.text
    assert str(old_dir / "wheel") in caplog.text
    # entries without a conflict still move
    assert (new_dir / "py_info" / "5" / "abc123.json").is_file()


def test_migration_is_noop_once_everything_moved(app_data_dirs):
    old_dir, new_dir = app_data_dirs
    new_dir.mkdir(parents=True)

    assert cache_dir_with_migration() == str(new_dir)
    assert not (old_dir / _MIGRATION_MARKER).exists()


def test_migration_moves_old_dir_wholesale_when_not_nested(tmp_path, monkeypatch):
    old_dir = tmp_path / "data"
    new_dir = tmp_path / "cache"
    monkeypatch.setattr(cache_migration, "user_data_dir", lambda **_: str(old_dir))
    monkeypatch.setattr(cache_migration, "user_cache_dir", lambda **_: str(new_dir))
    _populate_old_layout(old_dir)

    assert cache_dir_with_migration() == str(new_dir)
    assert not old_dir.exists()
    assert _embed_log(new_dir).is_file()


def test_no_old_layout_returns_new_dir(app_data_dirs):
    _, new_dir = app_data_dirs

    assert cache_dir_with_migration() == str(new_dir)
