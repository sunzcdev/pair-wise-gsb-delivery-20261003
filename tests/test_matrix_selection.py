from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

from nox._parametrize import Param, matrix_identity


def test_matrix_identity_is_sorted_and_axis_order_independent() -> None:
    first = Param(1, 2, arg_names=("a", "b"))
    second = Param(2, 1, arg_names=("b", "a"))

    assert matrix_identity("cell", first, "3.12") == "cell[a=1,b=2,python=3.12]"
    assert matrix_identity("cell", second, "3.12") == "cell[a=1,b=2,python=3.12]"


def _run_nox(folder: Path, *args: str) -> subprocess.CompletedProcess[str]:
    env = {**os.environ, "CI": "false", "NO_COLOR": "1", "PYTHONUTF8": "1"}
    return subprocess.run(
        [sys.executable, "-m", "nox", "--noxfile", str(folder / "noxfile.py"), *args],
        cwd=folder,
        check=False,
        capture_output=True,
        text=True,
        encoding="utf-8",
        env=env,
        timeout=45,
    )


def _write_replay_noxfile(folder: Path, *, swap_axes: bool, exclude: str) -> Path:
    folder.mkdir(parents=True, exist_ok=True)
    decorators = {
        "a": f"@nox.parametrize('a', [1, 2], exclude={exclude})",
        "b": "@nox.parametrize('b', [1, 2])",
    }
    order = ("b", "a") if swap_axes else ("a", "b")
    (folder / "noxfile.py").write_text(
        "import nox\n"
        "@nox.session(venv_backend='none')\n"
        + "\n".join(decorators[name] for name in order)
        + "\n@nox.parametrize('c', [5])\n"
        "def cell(session, a, b, c):\n"
        "    session.log(f'RAN a={a} b={b} c={c}')\n",
        encoding="utf-8",
    )
    return folder / "noxfile.py"


def test_selector_survives_axis_reorder_and_exclusion(tmp_path: Path) -> None:
    first = tmp_path / "first"
    _write_replay_noxfile(first, swap_axes=False, exclude="[]")
    listed = _run_nox(first, "--list-sessions")
    assert listed.returncode == 0, listed.stderr + listed.stdout
    selector = "cell[a=1,b=1,c=5]"
    assert selector in listed.stdout

    second = tmp_path / "second"
    _write_replay_noxfile(
        second, swap_axes=True, exclude="[{'a': 1, 'b': 2}]"
    )
    report = tmp_path / "report.json"
    run = _run_nox(
        second,
        "--sessions",
        selector,
        "--report",
        str(report),
        "--envdir",
        str(tmp_path / ".nox"),
    )
    assert run.returncode == 0, run.stderr + run.stdout
    assert "RAN a=1 b=1 c=5" in run.stdout + run.stderr

    data = json.loads(report.read_text(encoding="utf-8"))
    (entry,) = data["sessions"]
    assert entry["selector"] == selector
    assert entry["args"] == {"a": 1, "b": 1, "c": 5}
    assert entry["venv_backend"] == "none"
    assert entry["interpreter"] == sys.executable

    blocked = _run_nox(second, "--sessions", "cell[a=1,b=2,c=5]")
    output = blocked.stdout + blocked.stderr
    assert blocked.returncode != 0
    assert "was excluded by an 'exclude' filter" in output
    assert "RAN a=" not in output


def test_duplicate_short_names_report_both_inputs_before_running(
    tmp_path: Path,
) -> None:
    tmp_path.mkdir(exist_ok=True)
    (tmp_path / "noxfile.py").write_text(
        "import nox\n"
        "@nox.session(venv_backend='none')\n"
        "@nox.parametrize('a,b', [(1, 2), (3, 4)], ids=['same', 'same'], exclude=[])\n"
        "def clash(session, a, b):\n"
        "    session.log('RAN CELL')\n",
        encoding="utf-8",
    )

    result = _run_nox(tmp_path)
    output = result.stdout + result.stderr
    assert result.returncode != 0
    assert "RAN CELL" not in result.stdout
    assert "short name 'same'" in output
    assert "clash[a=1,b=2]" in output
    assert "clash[a=3,b=4]" in output
    assert "(a=1, b=2)" in output
    assert "(a=3, b=4)" in output

