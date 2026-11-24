# Copyright 2016 Alethea Katherine Flowers
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

import ctypes
import logging
import os
import platform
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path
from textwrap import dedent
from typing import Any
from unittest import mock

import pytest

import nox.command
import nox.popen

PYTHON = sys.executable

IS_WINDOWS = sys.platform.startswith("win")

skip_on_windows_primary_console_session = pytest.mark.skipif(
    sys.platform.startswith("win") and "SECONDARY_CONSOLE_SESSION" not in os.environ,
    reason="On Windows, this test must run in a separate console session.",
)

only_on_windows = pytest.mark.skipif(
    not sys.platform.startswith("win"), reason="Only run this test on Windows."
)


def _write_echo_batch(tmp_path: Path, suffix: str = ".bat") -> Path:
    """Write a batch script that echoes its first argument."""
    batch = tmp_path / f"echo-arg{suffix}"
    batch.write_text(
        f'@"{PYTHON}" -c "import sys; print(sys.argv[1])" %*\n',
        encoding="utf-8",
    )
    return batch


def test_windows_batch_command() -> None:
    command = nox.popen._windows_batch_command(
        [r"C:\Program Files\tool.cmd", "plain", "has space", "requests<99", "py(3)"]
    )

    expected = r'"C:\Program Files\tool.cmd" plain "has space" "requests<99" "py(3)"'
    assert command == expected


@pytest.mark.parametrize("argument", ['a"<b', 'a">b', '"urllib3<1.25', 'urllib3<1.25"'])
def test_windows_batch_command_rejects_quoted_metacharacters(argument: str) -> None:
    # cmd.exe only counts quote characters, so an argument mixing quotes and
    # metacharacters cannot be escaped reliably; 'a">b' would even run
    # successfully while silently redirecting output into a stray file "b".
    with pytest.raises(ValueError, match="Cannot escape"):
        nox.popen._windows_batch_command([r"C:\tool.bat", argument])


def test_windows_batch_command_accepts_pre_quoted_argument() -> None:
    # the historical workaround from #312: the caller quotes the constraint
    # themselves, and the outer quotes already protect the metacharacters.
    command = nox.popen._windows_batch_command([r"C:\tool.bat", '"urllib3<1.25"'])

    assert command == r'C:\tool.bat "urllib3<1.25"'


@pytest.mark.parametrize(
    "argument",
    ["%NOX_TEST_VAR%", "pre%NOX_TEST_VAR%post", "%%NOX_TEST_VAR%%", '"%NOX_TEST_VAR%"'],
)
def test_windows_batch_command_rejects_percent_expansion(argument: str) -> None:
    # cmd.exe expands %VAR% references even inside double quotes, so an
    # argument referencing a defined variable cannot be passed through
    # faithfully; a variable value could even inject extra commands.
    with pytest.raises(ValueError, match="would expand %NOX_TEST_VAR%"):
        nox.popen._windows_batch_command(
            [r"C:\tool.bat", argument], {"nox_test_var": "value"}
        )


@pytest.mark.parametrize("argument", ["%RANDOM%", "%__CD__%", "%=C:%", "%PATH:;=,%"])
def test_windows_batch_command_rejects_dynamic_percent_expansion(
    argument: str,
) -> None:
    # these expand even when the variable is missing from the child
    # environment: cmd.exe resolves them dynamically or defines them itself.
    with pytest.raises(ValueError, match="would expand"):
        nox.popen._windows_batch_command([r"C:\tool.bat", argument], {"PATH": "C:;D:"})


@pytest.mark.parametrize(
    "argument", ["100%", "%%", "%undefined_variable%", "https://x.test/%20a%20b.whl"]
)
def test_windows_batch_command_allows_literal_percents(argument: str) -> None:
    # cmd.exe leaves references to undefined variables alone, so these
    # arguments survive verbatim and are safe to pass through.
    command = nox.popen._windows_batch_command([r"C:\tool.bat", argument], {})

    assert command == f"C:\\tool.bat {argument}"


@only_on_windows
@pytest.mark.parametrize("suffix", [".bat", ".cmd"])
@pytest.mark.parametrize(
    "argument",
    [
        "name&value",
        "name<value",
        "name>value",
        "name^value",
        "name|value",
        "name(value",
        "name)value",
    ],
)
def test_run_windows_batch_metacharacter_arg(
    tmp_path: Path, suffix: str, argument: str
) -> None:
    batch = _write_echo_batch(tmp_path, suffix)

    result = nox.command.run([batch, argument], silent=True)

    assert result.strip() == argument


@only_on_windows
def test_run_windows_batch_paren_arg_in_block(tmp_path: Path) -> None:
    # conda.bat-style scripts wrap commands in if (...) blocks, where an
    # unquoted ")" in an argument ends the block early and silently corrupts
    # the argument (or aborts with "... was unexpected at this time").
    batch = tmp_path / "block.bat"
    batch.write_text(
        f'@echo off\nif "1"=="1" (\n'
        f'  "{PYTHON}" -c "import sys; print(sys.argv[1])" %*\n)\n',
        encoding="utf-8",
    )

    result = nox.command.run([batch, "(pkg)"], silent=True)

    assert result.strip() == "(pkg)"


@only_on_windows
def test_run_windows_batch_pre_quoted_arg(tmp_path: Path) -> None:
    # the pre-quoted idiom from #312 keeps working; the child process sees
    # the argument without the caller's quotes, which is what conda expects.
    batch = _write_echo_batch(tmp_path)

    result = nox.command.run([batch, '"urllib3<1.25"'], silent=True)

    assert result.strip() == "urllib3<1.25"


@only_on_windows
def test_run_windows_batch_quoted_metacharacter_arg(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    batch = _write_echo_batch(tmp_path)
    monkeypatch.chdir(tmp_path)

    with pytest.raises(ValueError, match="Cannot escape"):
        nox.command.run([batch, 'a">b'], silent=True)

    # the command must not have run at all, let alone create "b" via redirection
    assert [path.name for path in tmp_path.iterdir()] == [batch.name]


def test_run_defaults() -> None:
    result = nox.command.run([PYTHON, "-c", "print(123)"])

    assert result is True


def test_run_silent(capsys: pytest.CaptureFixture[str]) -> None:
    result = nox.command.run([PYTHON, "-c", "print(123)"], silent=True)

    out, _ = capsys.readouterr()

    assert "123" in result
    assert out == ""


@pytest.mark.skipif(shutil.which("git") is None, reason="Needs git")
def test_run_not_in_path() -> None:
    # Paths falls back on the environment PATH if the command is not found.
    result = nox.command.run(["git", "--version"], paths=["."])
    assert result is True


def test_run_verbosity(
    capsys: pytest.CaptureFixture[str], caplog: pytest.LogCaptureFixture
) -> None:
    caplog.clear()
    with caplog.at_level(logging.DEBUG):
        result = nox.command.run([PYTHON, "-c", "print(123)"], silent=True)

        out, _ = capsys.readouterr()

        assert "123" in result
        assert out == ""

    logs = [rec for rec in caplog.records if rec.levelname == "OUTPUT"]
    assert not logs

    caplog.clear()
    with caplog.at_level(logging.DEBUG - 1):
        result = nox.command.run([PYTHON, "-c", "print(123)"], silent=True)

        out, _ = capsys.readouterr()

        assert "123" in result
        assert out == ""

    logs = [rec for rec in caplog.records if rec.levelname == "OUTPUT"]
    assert logs
    assert logs[0].message.strip() == "123"


def test_run_verbosity_failed_command(caplog: pytest.LogCaptureFixture) -> None:
    caplog.clear()
    with caplog.at_level(logging.DEBUG):
        with pytest.raises(nox.command.CommandFailed) as exc:
            nox.command.run([PYTHON, "-c", "print(123); exit(1)"], silent=True)

        assert "123" in caplog.text
        assert exc.value.return_code == 1

    logs = [rec for rec in caplog.records if rec.levelname == "OUTPUT"]
    assert not logs

    caplog.clear()
    with caplog.at_level(logging.DEBUG - 1):
        with pytest.raises(nox.command.CommandFailed) as exc:
            nox.command.run([PYTHON, "-c", "print(123); exit(42)"], silent=True)

        assert "123" in caplog.text
        assert exc.value.return_code == 42

    # Nothing is logged but the error is still written to stderr
    assert not logs


@pytest.mark.skipif(
    sys.platform.startswith("win"),
    reason="See https://github.com/python/cpython/issues/85815",
)
def test_run_non_str() -> None:
    result = nox.command.run(
        [Path(PYTHON), "-c", "import sys; print(sys.argv)", Path(PYTHON)],
        silent=True,
    )

    assert PYTHON in result


def test_run_env_unicode() -> None:
    result = nox.command.run(
        [PYTHON, "-c", 'import os; print(os.environ["SIGIL"])'],
        silent=True,
        env={"SIGIL": "123"},
    )

    assert "123" in result


def test_run_env_remove(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("EMPTY", "notempty")
    nox.command.run(
        [PYTHON, "-c", 'import os; assert "EMPTY" in os.environ'],
        silent=True,
    )
    nox.command.run(
        [PYTHON, "-c", 'import os; assert "EMPTY" not in os.environ'],
        silent=True,
        env={"EMPTY": None},
    )


@mock.patch("nox.command._PLATFORM", "win32")
def test_run_env_systemroot() -> None:
    systemroot = os.environ.setdefault("SYSTEMROOT", "sigil")

    result = nox.command.run(
        [PYTHON, "-c", 'import os; print(os.environ["SYSTEMROOT"])'], silent=True
    )

    assert systemroot in result


def test_run_not_found() -> None:
    with pytest.raises(nox.command.CommandFailed):
        nox.command.run(["nonexistentcmd"])


def test_run_path_nonexistent() -> None:
    result = nox.command.run(
        [PYTHON, "-c", "import sys; print(sys.executable)"],
        silent=True,
        paths=["/non/existent"],
    )

    assert "/non/existent" not in result


def test_run_path_existent(tmp_path: Path) -> None:
    executable_name = (
        "testexc.exe" if "windows" in platform.platform().lower() else "testexc"
    )
    tmp_path.touch()
    executable = tmp_path.joinpath(executable_name)
    executable.touch()
    executable.chmod(0o700)

    with mock.patch("nox.command.popen") as mock_command:
        mock_command.return_value = (0, "")
        nox.command.run([executable_name], silent=True, paths=[str(tmp_path)])
        mock_command.assert_called_with(
            [str(executable)],
            env=None,
            silent=True,
            stdout=None,
            stderr=subprocess.STDOUT,
            interrupt_timeout=0.3,
            terminate_timeout=0.2,
        )


def _write_tool(
    directory: Path, name: str, body: str, *, executable: bool = True
) -> Path:
    """Write an executable named *name* into *directory* and return its path.

    On Windows a ``.cmd`` launcher is written alongside a ``.py`` worker (the
    shape Nox's own virtualenvs use); elsewhere the script is made executable
    directly.
    """
    if IS_WINDOWS:
        worker = directory / f"{name}.py"
        worker.write_text(body, encoding="utf-8")
        launcher = directory / f"{name}.cmd"
        launcher.write_text(f'@"{PYTHON}" "%~dp0{name}.py" %*\n', encoding="utf-8")
        return launcher

    tool = directory / name
    tool.write_text(f"#!{PYTHON}\n{body}", encoding="utf-8")
    if executable:
        tool.chmod(0o755)
    return tool


def _tool_paths(tool: Path) -> list[Path]:
    """All files that make up *tool* (launcher plus its worker on Windows)."""
    if IS_WINDOWS:
        return [tool, tool.with_suffix(".py")]
    return [tool]


def _remove_tool(tool: Path) -> None:
    for path in _tool_paths(tool):
        path.unlink(missing_ok=True)


def _starts_with_executable(args: Any, executable: str) -> bool:
    """Whether a Popen *args* value invokes *executable*.

    On Windows nox.popen rewrites ``.cmd``/``.bat`` invocations into a string
    command line, so both list and string forms must be recognized.
    """
    if isinstance(args, (list, tuple)):
        candidate = os.fspath(args[0])
    else:
        candidate = os.fspath(args).split(" ", 1)[0].strip('"')
    return os.path.normcase(candidate) == os.path.normcase(executable)


def test_run_fallback_when_preferred_vanishes_before_start(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    # The preferred entry exists in the virtualenv bin dir; the explicit
    # fallback lives elsewhere.
    venv_bin = tmp_path / "venv-bin"
    venv_bin.mkdir()
    other_bin = tmp_path / "other-bin"
    other_bin.mkdir()
    cwd = tmp_path / "work"
    cwd.mkdir()

    marker = tmp_path / "marker.txt"
    primary = _write_tool(
        venv_bin,
        "frobnicate",
        (
            "from pathlib import Path\n"
            f"Path(r'{marker}').write_text('primary', encoding='utf-8')\n"
        ),
    )
    fallback = _write_tool(
        other_bin,
        "frobnicate-fallback",
        (
            "import os, sys\n"
            "from pathlib import Path\n"
            f"Path(r'{marker}').write_text(\n"
            "    'fallback:' + os.getcwd() + ':' + ' '.join(sys.argv[1:]),\n"
            "    encoding='utf-8')\n"
        ),
    )

    resolved = nox.command.which("frobnicate", [str(venv_bin)])

    # Local launch stand-in: hold the preferred tool at the "resolved but not
    # yet started" stage, then remove it before Popen actually spawns it.
    real_popen = subprocess.Popen
    launches: list[Any] = []

    class VanishingPopen(real_popen):  # type: ignore[misc, valid-type]
        def __init__(self, args: Any, *popen_args: Any, **popen_kwargs: Any) -> None:
            launches.append(args)
            if _starts_with_executable(args, resolved):
                _remove_tool(primary)
            super().__init__(args, *popen_args, **popen_kwargs)

    monkeypatch.setattr(subprocess, "Popen", VanishingPopen)
    monkeypatch.chdir(cwd)

    caplog.set_level(logging.WARNING)
    result = nox.command.run(
        ["frobnicate", "--flag", "value"],
        paths=[str(venv_bin)],
        fallback=fallback,
        silent=True,
        env={"FROBNICATE_VAR": "session-env"},
    )

    # The fallback ran exactly once and recorded its actual source.
    assert len(launches) == 2
    assert _starts_with_executable(launches[0], resolved)
    assert _starts_with_executable(launches[1], os.fspath(fallback))
    recorded = marker.read_text(encoding="utf-8")
    assert recorded.startswith("fallback:")
    assert str(cwd) in recorded
    assert "--flag value" in recorded
    assert "fallback executable" in caplog.text
    assert result == ""


def test_run_fallback_not_used_when_preferred_succeeds(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    venv_bin = tmp_path / "venv-bin"
    venv_bin.mkdir()
    other_bin = tmp_path / "other-bin"
    other_bin.mkdir()

    marker = tmp_path / "marker.txt"
    primary = _write_tool(
        venv_bin,
        "frobnicate",
        (
            "import sys\n"
            "from pathlib import Path\n"
            f"Path(r'{marker}').write_text('primary', encoding='utf-8')\n"
            "sys.exit(7)\n"
        ),
    )
    fallback = _write_tool(
        other_bin,
        "frobnicate-fallback",
        (
            "from pathlib import Path\n"
            f"Path(r'{marker}').write_text('fallback', encoding='utf-8')\n"
        ),
    )

    real_popen = subprocess.Popen
    launches: list[Any] = []

    class RecordingPopen(real_popen):  # type: ignore[misc, valid-type]
        def __init__(self, args: Any, *popen_args: Any, **popen_kwargs: Any) -> None:
            launches.append(args)
            super().__init__(args, *popen_args, **popen_kwargs)

    monkeypatch.setattr(subprocess, "Popen", RecordingPopen)
    caplog.set_level(logging.WARNING)

    # Exit code 7 is explicitly acceptable to the caller, so the preferred
    # tool's result is used and the fallback must never be touched.
    result = nox.command.run(
        ["frobnicate"],
        paths=[str(venv_bin)],
        fallback=fallback,
        success_codes=[0, 7],
    )

    assert result is True
    assert len(launches) == 1
    assert _starts_with_executable(launches[0], os.fspath(primary))
    assert marker.read_text(encoding="utf-8") == "primary"
    assert "fallback" not in caplog.text


def test_run_without_fallback_does_not_reuse_previous_one(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    venv_bin = tmp_path / "venv-bin"
    venv_bin.mkdir()
    other_bin = tmp_path / "other-bin"
    other_bin.mkdir()

    marker = tmp_path / "marker.txt"
    primary = _write_tool(
        venv_bin,
        "frobnicate",
        (
            "from pathlib import Path\n"
            f"Path(r'{marker}').write_text('primary', encoding='utf-8')\n"
        ),
    )
    fallback = _write_tool(
        other_bin,
        "frobnicate-fallback",
        (
            "from pathlib import Path\n"
            f"Path(r'{marker}').write_text('fallback', encoding='utf-8')\n"
        ),
    )

    resolved = nox.command.which("frobnicate", [str(venv_bin)])
    real_popen = subprocess.Popen

    class VanishingPopen(real_popen):  # type: ignore[misc, valid-type]
        def __init__(self, args: Any, *popen_args: Any, **popen_kwargs: Any) -> None:
            if _starts_with_executable(args, resolved):
                _remove_tool(primary)
            super().__init__(args, *popen_args, **popen_kwargs)

    monkeypatch.setattr(subprocess, "Popen", VanishingPopen)

    # First call explicitly names a fallback and succeeds via that fallback.
    first = nox.command.run(["frobnicate"], paths=[str(venv_bin)], fallback=fallback)
    assert first is True
    assert marker.read_text(encoding="utf-8") == "fallback"

    # A separate call without a fallback cannot reuse the earlier fallback.
    marker.unlink()
    with pytest.raises(nox.command.CommandFailed, match="not found"):
        nox.command.run(["frobnicate"], paths=[str(venv_bin)])
    assert not marker.exists()


def test_run_fallback_when_preferred_unresolved(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    venv_bin = tmp_path / "venv-bin"
    venv_bin.mkdir()
    fallback = _write_tool(
        tmp_path,
        "frobnicate-fallback",
        "import sys\nprint('from fallback')\n",
    )

    caplog.set_level(logging.WARNING)
    result = nox.command.run(
        ["frobnicate", "--version"],
        paths=[str(venv_bin)],
        fallback=fallback,
        silent=True,
    )

    assert "from fallback" in result
    assert "fallback executable" in caplog.text


def test_run_fallback_that_is_also_missing(tmp_path: Path) -> None:
    venv_bin = tmp_path / "venv-bin"
    venv_bin.mkdir()
    missing_fallback = tmp_path / "does-not-exist"

    with pytest.raises(nox.command.CommandFailed, match="fallback"):
        nox.command.run(
            ["frobnicate"],
            paths=[str(venv_bin)],
            fallback=missing_fallback,
        )


def test_run_fallback_after_start_failure_runs_once(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    # Deterministic, platform-independent stand-in: the resolved preferred
    # entry fails to start, the explicit fallback then runs exactly once.
    venv_bin = tmp_path / "venv-bin"
    venv_bin.mkdir()
    _write_tool(
        venv_bin,
        "frobnicate",
        "raise SystemExit('the real primary never runs')\n",
    )
    fallback = _write_tool(
        tmp_path,
        "frobnicate-fallback",
        "import sys\nprint('from fallback')\n",
    )
    resolved = nox.command.which("frobnicate", [str(venv_bin)])

    real_popen = nox.popen.popen

    def fake_popen(args: Any, **kwargs: Any) -> tuple[int, str]:
        if _starts_with_executable(args, resolved):
            raise FileNotFoundError(2, "The system cannot find the file specified")
        return real_popen(args, **kwargs)

    caplog.set_level(logging.WARNING)
    with mock.patch("nox.command.popen", side_effect=fake_popen) as mocked:
        result = nox.command.run(
            ["frobnicate", "--flag"],
            paths=[str(venv_bin)],
            fallback=fallback,
            silent=True,
        )

    assert mocked.call_count == 2
    assert _starts_with_executable(mocked.call_args_list[0][0][0], resolved)
    assert _starts_with_executable(mocked.call_args_list[1][0][0], str(fallback))
    assert result.strip() == "from fallback"
    assert "disappeared" in caplog.text
    assert "Ran fallback executable" in caplog.text


def test_run_missing_fallback_start_failure_raises(tmp_path: Path) -> None:
    venv_bin = tmp_path / "venv-bin"
    venv_bin.mkdir()
    _write_tool(
        venv_bin,
        "frobnicate",
        "raise SystemExit('the real primary never runs')\n",
    )
    fallback = tmp_path / "missing-fallback"

    with (
        mock.patch("nox.command.popen", side_effect=FileNotFoundError) as mocked,
        pytest.raises(nox.command.CommandFailed, match="Fallback program"),
    ):
        nox.command.run(["frobnicate"], paths=[str(venv_bin)], fallback=fallback)

    assert mocked.call_count == 2


def test_run_fallback_shares_env(tmp_path: Path) -> None:
    venv_bin = tmp_path / "venv-bin"
    venv_bin.mkdir()
    fallback = _write_tool(
        tmp_path,
        "frobnicate-fallback",
        "import os, sys\nsys.stdout.write(os.environ['NOX_FALLBACK_TEST'])\n",
    )

    result = nox.command.run(
        ["frobnicate"],
        paths=[str(venv_bin)],
        fallback=fallback,
        env={"NOX_FALLBACK_TEST": "shared-env"},
        silent=True,
    )

    assert result.strip() == "shared-env"


def test_run_external_warns(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.WARNING)

    nox.command.run([PYTHON, "--version"], silent=True, paths=[tmp_path])

    assert "external=True" in caplog.text


def test_run_external_silences(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.WARNING)

    nox.command.run([PYTHON, "--version"], silent=True, paths=[tmp_path], external=True)

    assert "external=True" not in caplog.text


def test_run_external_raises(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.ERROR)

    with pytest.raises(nox.command.CommandFailed) as exc:
        nox.command.run(
            [PYTHON, "--version"], silent=True, paths=[tmp_path], external="error"
        )

    assert "external=True" in caplog.text
    assert exc.value.return_code is None


def test_run_external_raises_without_log(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.ERROR)

    with pytest.raises(nox.command.CommandFailed):
        nox.command.run(
            [PYTHON, "--version"],
            silent=True,
            paths=[tmp_path],
            external="error",
            log=False,
        )

    assert "external=True" in caplog.text


def test_run_external_no_warning_without_log(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.WARNING)

    nox.command.run([PYTHON, "--version"], silent=True, paths=[tmp_path], log=False)

    assert "external=True" not in caplog.text


def test_exit_codes() -> None:
    assert nox.command.run([PYTHON, "-c", "import sys; sys.exit(0)"])

    with pytest.raises(nox.command.CommandFailed):
        nox.command.run([PYTHON, "-c", "import sys; sys.exit(1)"])

    assert nox.command.run(
        [PYTHON, "-c", "import sys; sys.exit(1)"], success_codes=[1, 2]
    )


def test_fail_with_silent(
    caplog: pytest.LogCaptureFixture, capsys: pytest.CaptureFixture[str]
) -> None:
    caplog.clear()
    with pytest.raises(nox.command.CommandFailed):
        nox.command.run(
            [
                PYTHON,
                "-c",
                (
                    'import sys; sys.stdout.write("out");'
                    'sys.stderr.write("err"); sys.exit(1)'
                ),
            ],
            silent=True,
        )
    _out, err = capsys.readouterr()
    assert "out" not in err
    assert "err" not in err
    assert "out" in caplog.text
    assert "err" in caplog.text


@pytest.fixture
def marker(tmp_path: Path) -> Path:
    """A marker file for process communication."""
    return tmp_path / "marker"


def enable_ctrl_c(*, enabled: bool) -> None:
    """Enable keyboard interrupts (CTRL-C) on Windows."""
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)  # type: ignore[attr-defined]

    if not kernel32.SetConsoleCtrlHandler(None, not enabled):
        raise ctypes.WinError(ctypes.get_last_error())  # type: ignore[attr-defined]


def interrupt_process(proc: subprocess.Popen[Any]) -> None:
    """Send SIGINT or CTRL_C_EVENT to the process."""
    if sys.platform.startswith("win"):
        # Disable Ctrl-C so we don't terminate ourselves.
        enable_ctrl_c(enabled=False)

        # Send the keyboard interrupt to all processes attached to the current
        # console session.
        os.kill(0, signal.CTRL_C_EVENT)  # type: ignore[attr-defined]
    else:
        proc.send_signal(signal.SIGINT)


@pytest.fixture
def command_with_keyboard_interrupt(
    monkeypatch: pytest.MonkeyPatch, marker: Any
) -> None:
    """Monkeypatch Popen.communicate to raise KeyboardInterrupt."""
    if sys.platform.startswith("win"):
        # Enable Ctrl-C because the child inherits the setting from us.
        enable_ctrl_c(enabled=True)

    communicate = subprocess.Popen.communicate

    def wrapper(
        proc: subprocess.Popen[Any], *args: Any, **kwargs: Any
    ) -> tuple[Any, Any]:
        # Raise the interrupt only on the first call, so Nox has a chance to
        # shut down the child process subsequently.

        if wrapper.firstcall:  # type: ignore[attr-defined]
            wrapper.firstcall = False  # type: ignore[attr-defined]

            # Give the child time to install its signal handlers.
            while not marker.exists():
                time.sleep(0.05)

            # Send a real keyboard interrupt to the child.
            interrupt_process(proc)

            # Fake a keyboard interrupt in the parent.
            raise KeyboardInterrupt

        return communicate(proc, *args, **kwargs)

    wrapper.firstcall = True  # type: ignore[attr-defined]

    monkeypatch.setattr("subprocess.Popen.communicate", wrapper)


def format_program(program: str, marker: Any) -> str:
    """Preprocess the Python program run by the child process."""
    main = f"""
    import time
    from pathlib import Path

    Path({str(marker)!r}).touch()
    time.sleep(3)
    """
    return dedent(program).format(MAIN=dedent(main))


def run_pytest_in_new_console_session(test: str) -> None:
    """Run the given test in a separate console session."""
    env = dict(os.environ, SECONDARY_CONSOLE_SESSION="")
    creationflags = subprocess.CREATE_NO_WINDOW  # type: ignore[attr-defined]

    subprocess.run(
        [sys.executable, "-m", "pytest", f"{__file__}::{test}"],
        env=env,
        check=True,
        capture_output=True,
        creationflags=creationflags,
    )


@skip_on_windows_primary_console_session
@pytest.mark.parametrize(
    "program",
    [
        """
        {MAIN}
        """,
        """
        import signal

        signal.signal(signal.SIGINT, signal.SIG_IGN)

        {MAIN}
        """,
        """
        import signal

        signal.signal(signal.SIGINT, signal.SIG_IGN)
        signal.signal(signal.SIGTERM, signal.SIG_IGN)

        {MAIN}
        """,
    ],
)
@pytest.mark.usefixtures("command_with_keyboard_interrupt")
def test_interrupt_raises(
    program: str,
    marker: Any,
) -> None:
    """It kills the process and reraises the keyboard interrupt."""
    with pytest.raises(KeyboardInterrupt):
        nox.command.run([PYTHON, "-c", format_program(program, marker)])


@only_on_windows
def test_interrupt_raises_on_windows() -> None:
    """It kills the process and reraises the keyboard interrupt."""
    run_pytest_in_new_console_session("test_interrupt_raises")


@skip_on_windows_primary_console_session
def test_interrupt_handled(command_with_keyboard_interrupt: None, marker: Any) -> None:  # noqa: ARG001
    """It does not raise if the child handles the keyboard interrupt."""
    program = """
    import signal

    def exithandler(sig, frame):
        raise SystemExit()

    signal.signal(signal.SIGINT, exithandler)

    {MAIN}
    """
    # The child can be slow to exit on loaded runners (e.g., writing coverage)
    nox.command.run(
        [PYTHON, "-c", format_program(program, marker)], interrupt_timeout=10
    )


@only_on_windows
def test_interrupt_handled_on_windows() -> None:
    """It does not raise if the child handles the keyboard interrupt."""
    run_pytest_in_new_console_session("test_interrupt_handled")


def test_custom_stdout(capsys: pytest.CaptureFixture[str], tmp_path: Path) -> None:
    with (tmp_path / "out.txt").open("w+", encoding="utf-8") as stdout:
        nox.command.run(
            [
                PYTHON,
                "-c",
                (
                    'import sys; sys.stdout.write("out");'
                    'sys.stderr.write("err"); sys.exit(0)'
                ),
            ],
            stdout=stdout,
        )
        out, err = capsys.readouterr()
        assert not out
        assert "out" not in err
        assert "err" not in err
        stdout.seek(0)
        tempfile_contents = stdout.read()
        assert "out" in tempfile_contents
        assert "err" in tempfile_contents


def test_custom_stdout_silent_flag(tmp_path: Path) -> None:
    with (tmp_path / "out.txt").open("w+", encoding="utf-8") as stdout:  # noqa: SIM117
        with pytest.raises(ValueError, match="silent"):
            nox.command.run([PYTHON, "-c", 'print("hi")'], stdout=stdout, silent=True)


def test_custom_stdout_failed_command(
    capsys: pytest.CaptureFixture[str], tmp_path: Path
) -> None:
    with (tmp_path / "out.txt").open("w+", encoding="utf-8") as stdout:
        with pytest.raises(nox.command.CommandFailed):
            nox.command.run(
                [
                    PYTHON,
                    "-c",
                    (
                        'import sys; sys.stdout.write("out");'
                        'sys.stderr.write("err"); sys.exit(1)'
                    ),
                ],
                stdout=stdout,
            )
        out, err = capsys.readouterr()
        assert not out
        assert "out" not in err
        assert "err" not in err
        stdout.seek(0)
        tempfile_contents = stdout.read()
        assert "out" in tempfile_contents
        assert "err" in tempfile_contents


def test_custom_stderr(capsys: pytest.CaptureFixture[str], tmp_path: Path) -> None:
    with (tmp_path / "err.txt").open("w+", encoding="utf-8") as stderr:
        nox.command.run(
            [
                PYTHON,
                "-c",
                (
                    'import sys; sys.stdout.write("out");'
                    'sys.stderr.write("err"); sys.exit(0)'
                ),
            ],
            stderr=stderr,
        )
        out, err = capsys.readouterr()
        assert not err
        assert "out" not in out
        assert "err" not in out
        stderr.seek(0)
        tempfile_contents = stderr.read()
        assert "out" not in tempfile_contents
        assert "err" in tempfile_contents


def test_custom_stderr_failed_command(
    capsys: pytest.CaptureFixture[str], tmp_path: Path
) -> None:
    with (tmp_path / "out.txt").open("w+", encoding="utf-8") as stderr:
        with pytest.raises(nox.command.CommandFailed):
            nox.command.run(
                [
                    PYTHON,
                    "-c",
                    (
                        'import sys; sys.stdout.write("out");'
                        'sys.stderr.write("err"); sys.exit(1)'
                    ),
                ],
                stderr=stderr,
            )
        out, err = capsys.readouterr()
        assert not err
        assert "out" not in out
        assert "err" not in out
        stderr.seek(0)
        tempfile_contents = stderr.read()
        assert "out" not in tempfile_contents
        assert "err" in tempfile_contents


def test_output_decoding() -> None:
    result = nox.popen.decode_output(b"abc")

    assert result == "abc"


def test_output_decoding_non_ascii() -> None:
    result = nox.popen.decode_output("ü".encode())

    assert result == "ü"


def test_output_decoding_utf8_only_fail(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(nox.popen.locale, "getpreferredencoding", lambda: "utf8")  # type: ignore[attr-defined]

    with pytest.raises(UnicodeDecodeError) as exc:
        nox.popen.decode_output(b"\x95")

    assert exc.value.encoding == "utf-8"


def test_output_decoding_utf8_fail_cp1252_success(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(nox.popen.locale, "getpreferredencoding", lambda: "cp1252")  # type: ignore[attr-defined]

    result = nox.popen.decode_output(b"\x95")

    assert result == "•"  # U+2022


def test_output_decoding_both_fail(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(nox.popen.locale, "getpreferredencoding", lambda: "ascii")  # type: ignore[attr-defined]

    with pytest.raises(UnicodeDecodeError) as exc:
        nox.popen.decode_output(b"\x95")

    assert exc.value.encoding == "ascii"


@pytest.mark.parametrize("external", [False, "error"])
def test_run_external_directory_prefix(
    external: nox.command.ExternalType, caplog: pytest.LogCaptureFixture
) -> None:
    binary_dir = Path(PYTHON).parent
    prefix_dir = binary_dir.with_name(binary_dir.name[:-1])
    caplog.set_level(logging.WARNING)

    if external == "error":
        with pytest.raises(
            nox.command.CommandFailed, match="External program disallowed"
        ):
            nox.command.run(
                [PYTHON, "--version"],
                silent=True,
                paths=[prefix_dir],
                external=external,
            )
    else:
        nox.command.run(
            [PYTHON, "--version"], silent=True, paths=[prefix_dir], external=external
        )

    assert "external=True" in caplog.text


@pytest.mark.parametrize("normalized", [False, True])
def test_run_internal_directory(normalized: bool) -> None:
    binary_dir = Path(PYTHON).parent
    path = binary_dir / ".." / binary_dir.name if normalized else binary_dir

    result = nox.command.run(
        [PYTHON, "--version"], silent=True, paths=[path], external="error"
    )

    assert "Python" in result
