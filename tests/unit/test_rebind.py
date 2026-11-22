from __future__ import annotations

import shutil
import sys
from pathlib import Path
from typing import TYPE_CHECKING

import pytest

from virtualenv.rebind import RebindError, main, rebind
from virtualenv.run import cli_run

if TYPE_CHECKING:
    from collections.abc import Callable


@pytest.fixture
def copied_env(tmp_path: Path) -> "Callable[[], Path]":
    def factory() -> Path:
        source = tmp_path / "source-env"
        # no seed: the harness temp path is deep enough that pip install would hit the Windows path limit
        cli_run([str(source), "--no-seed", "--activators", "batch,bash,nushell,fish,xonsh,powershell", "--quiet"])
        target = tmp_path / "ci artifacts" / "env with space"
        target.parent.mkdir(parents=True)
        shutil.copytree(source, target)
        # stand in for the installer generated console script launchers of a seeded environment
        launcher = target / "Scripts" / "pip.exe"
        launcher.write_bytes(b"MZ" + str(tmp_path / "source-env").encode("utf-8") + b"\x00")
        return target

    return factory


def test_rebind_updates_virtualenv_files(copied_env: "Callable[[], Path]") -> None:
    target = copied_env()
    old = str(target.parent.parent / "source-env")
    report = rebind(target)
    assert report.changed
    assert Path(report.old_dir) == Path(old)
    assert "Scripts\\activate.bat" in report.updated or "Scripts/activate.bat" in report.updated
    assert "pyvenv.cfg" in report.updated
    bat = (target / "Scripts" / "activate.bat").read_text(encoding="utf-8")
    assert str(target) in bat
    assert old not in bat
    assert report.external  # the pip launchers keep their installer written shebang
    assert all(name.endswith(".exe") for name in report.external)
    assert not report.unconfirmed
    # the external launcher is left byte for byte as the installer wrote it
    assert (target / "Scripts" / "pip.exe").read_bytes().startswith(b"MZ" + old.encode("utf-8"))


def test_rebind_is_idempotent(copied_env: "Callable[[], Path]") -> None:
    target = copied_env()
    rebind(target)
    again = rebind(target)
    assert not again.changed
    assert not again.updated


def test_check_only_writes_nothing(copied_env: "Callable[[], Path]") -> None:
    target = copied_env()
    before = (target / "Scripts" / "activate.bat").read_bytes()
    report = rebind(target, check_only=True)
    assert report.changed
    assert (target / "Scripts" / "activate.bat").read_bytes() == before


def test_base_python_mismatch_leaves_target_untouched(copied_env: "Callable[[], Path]") -> None:
    target = copied_env()
    cfg = target / "pyvenv.cfg"
    cfg.write_text(
        cfg.read_text(encoding="utf-8").replace("home =", "home = C:\\nonexistent base\nbroken =", 1),
        encoding="utf-8",
    )
    before = (target / "Scripts" / "activate.bat").read_bytes()
    with pytest.raises(RebindError, match="base python"):
        rebind(target)
    assert (target / "Scripts" / "activate.bat").read_bytes() == before


def test_unconfirmed_entries_are_listed_not_touched(copied_env: "Callable[[], Path]") -> None:
    target = copied_env()
    old = str(target.parent.parent / "source-env")
    note = target / "notes.txt"
    note.write_text(f"created at {old}", encoding="utf-8")
    report = rebind(target)
    assert "notes.txt" in report.unconfirmed
    assert note.read_text(encoding="utf-8") == f"created at {old}"


def test_main_reports_failure(copied_env: "Callable[[], Path]", capsys: pytest.CaptureFixture[str]) -> None:
    target = copied_env()
    (target / "pyvenv.cfg").unlink()
    assert main([str(target)]) == 1
    assert "left untouched" in capsys.readouterr().err


def test_env_python_runs_at_new_location(copied_env: "Callable[[], Path]") -> None:
    target = copied_env()
    rebind(target)
    import subprocess

    out = subprocess.run(
        [str(target / "Scripts" / "python.exe"), "-c", "import sys; print(sys.prefix)"],
        capture_output=True,
        text=True,
        check=True,
    )
    assert Path(out.stdout.strip()) == target
