from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Final

import pytest

from virtualenv.completion import PHASES, STATE_DIR, STATE_FILE

_HERE: Final[Path] = Path(__file__).parent
_WORKER: Final[Path] = _HERE / "_complete_mode" / "worker.py"


@pytest.fixture
def short_workspace() -> Path:
    # pytest's tmp_path nests deeply, and pip's package tree then passes the Windows 260 character path limit; a short
    # root keeps the created environment below it. The folder is unique per test and removed afterwards.
    base = Path(tempfile.mkdtemp(prefix="ve-cmpl-"))
    try:
        yield base
    finally:
        shutil.rmtree(base, ignore_errors=True)


def _worker_env(dest: Path, app_data: Path, counter: Path, fail_times: int) -> dict[str, str]:
    return {
        **os.environ,
        "COMPLETION_TEST_DEST": str(dest),
        "COMPLETION_TEST_APP_DATA": str(app_data),
        "COMPLETION_FAIL_COUNTER": str(counter),
        "COMPLETION_FAIL_TIMES": str(fail_times),
    }


def _run_worker(dest: Path, app_data: Path, counter: Path, fail_times: int) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(_WORKER)],
        capture_output=True,
        text=True,
        encoding="utf-8",
        env=_worker_env(dest, app_data, counter, fail_times),
        timeout=300,
        check=False,
    )


def _completed_dest(result: subprocess.CompletedProcess[str]) -> str | None:
    for line in result.stdout.splitlines():
        if line.startswith("COMPLETED_DEST="):
            return line.removeprefix("COMPLETED_DEST=")
    return None


def _state(dest: Path) -> dict[str, object]:
    return json.loads((dest / STATE_DIR / STATE_FILE).read_text(encoding="utf-8"))


def _env_python(dest: Path) -> Path:
    scripts = dest / ("Scripts" if sys.platform == "win32" else "bin")
    return scripts / ("python.exe" if sys.platform == "win32" else "python")


def _env_pip(dest: Path) -> Path:
    scripts = dest / ("Scripts" if sys.platform == "win32" else "bin")
    return scripts / ("pip.exe" if sys.platform == "win32" else "pip")


@pytest.mark.timeout(600)
def test_two_workers_serialize_and_resume_a_failed_seed(short_workspace: Path) -> None:
    dest = short_workspace / "env"
    app_data = short_workspace / "app-data"
    counter = short_workspace / "fail-counter"

    # two processes compete for the same destination; the seeder fails for the first two invocations across both
    processes = [
        subprocess.Popen(
            [sys.executable, str(_WORKER)],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            encoding="utf-8",
            env=_worker_env(dest, app_data, counter, fail_times=2),
        )
        for _ in range(2)
    ]
    outputs = [process.communicate() for process in processes]
    codes = [process.returncode for process in processes]

    # at least one racing process hits the failing seeder; neither may expose a usable-but-unseeded environment
    assert any(code != 0 for code in codes), outputs
    assert any("injected seeder failure" in (err or "") for _out, err in outputs), outputs

    # the next request resumes the interrupted creation and confirms the environment
    retry = _run_worker(dest, app_data, counter, fail_times=0)
    assert retry.returncode == 0, retry.stderr

    # the entry point of the successful run points at the confirmed destination
    assert _completed_dest(retry) == str(dest)
    assert _state(dest)["phases"] == list(PHASES)
    assert _state(dest)["dest"] == str(dest)

    # the resumed environment really works: its interpreter starts and the seeded pip is importable
    assert _env_pip(dest).exists()
    probe = subprocess.run(
        [str(_env_python(dest)), "-c", "import pip; print(pip.__version__)"],
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=120,
        check=False,
    )
    assert probe.returncode == 0, probe.stderr
    assert probe.stdout.strip()


@pytest.mark.timeout(600)
def test_second_request_reuses_the_confirmed_environment(short_workspace: Path) -> None:
    dest = short_workspace / "env"
    app_data = short_workspace / "app-data"
    counter = short_workspace / "fail-counter"

    first = _run_worker(dest, app_data, counter, fail_times=0)
    assert first.returncode == 0, first.stderr
    assert _completed_dest(first) == str(dest)

    second = _run_worker(dest, app_data, counter, fail_times=0)
    assert second.returncode == 0, second.stderr
    # the reused entry point still resolves to the confirmed destination, without recreating it
    assert _completed_dest(second) == str(dest)
    assert "reuse complete virtual environment" in second.stderr + second.stdout
    assert _state(dest)["phases"] == list(PHASES)
