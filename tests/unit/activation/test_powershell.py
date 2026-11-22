from __future__ import annotations

import os
import shutil
import subprocess
import sys
from argparse import Namespace
from typing import TYPE_CHECKING, Final

import pytest

from virtualenv.activation import PowerShellActivator
from virtualenv.info import IS_GRAALPY, IS_PYPY, IS_WIN
from virtualenv.run import cli_run

if TYPE_CHECKING:
    from pathlib import Path


_POWERSHELLS: Final[list[str]] = [path for path in map(shutil.which, ("pwsh", "powershell.exe")) if path]
_VENV_BIN_DIR: Final[str] = "Scripts" if IS_WIN else "bin"
_INTERPRETER_INDEPENDENT: Final[pytest.MarkDecorator] = pytest.mark.skipif(
    IS_PYPY or IS_GRAALPY, reason="quoting does not depend on the interpreter, the CPython jobs run this matrix"
)


@pytest.fixture(autouse=True)
def _private_pwsh_cache(tmp_path_factory: pytest.TempPathFactory, monkeypatch: pytest.MonkeyPatch) -> None:
    # pwsh reads and rewrites a startup profile in its cache folder on every start, so parallel workers shared one file;
    # while they did, every pwsh on a macOS runner crashed with FileLoadException for two minutes (CI run 44217352067)
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path_factory.mktemp("pwsh-cache")))


@pytest.mark.parametrize("powershell", _POWERSHELLS)
@pytest.mark.parametrize(
    ("define", "expected"),
    [
        pytest.param([], [""], id="absent"),
        pytest.param(["function global:pydoc { 'original pydoc' }"], [" 'original pydoc' "], id="existing"),
    ],
)
@pytest.mark.parametrize(
    "environments",
    [
        pytest.param(["env"], id="once"),
        pytest.param(["env", "env"], id="repeat"),
        pytest.param(["env", "other"], id="switch"),
    ],
)
def test_powershell_restores_pydoc(
    tmp_path: Path, powershell: str, define: list[str], expected: list[str], environments: list[str]
) -> None:
    sessions: Final = [
        cli_run(["--no-seed", "--activators", "powershell", str(tmp_path / name)]) for name in environments
    ]
    activate: Final = [
        f". {PowerShellActivator.quote(str(session.creator.bin_dir / 'activate.ps1'))}" for session in sessions
    ]
    driver: Final = tmp_path / "pydoc.ps1"
    driver.write_text(
        "\n".join([
            "$ErrorActionPreference = 'Stop'",
            *define,
            *activate,
            "deactivate",
            'Write-Output "$function:pydoc"',
        ]),
        encoding="utf-8-sig",
    )
    result: Final = subprocess.run(
        [powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", str(driver)],
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=120,
        check=False,
    )
    assert (result.returncode, result.stdout.splitlines(), result.stderr) == (0, expected, "")


@pytest.mark.parametrize("powershell", _POWERSHELLS)
def test_powershell_clears_pythonhome(tmp_path: Path, powershell: str) -> None:
    session = cli_run(["--no-seed", "--activators", "powershell", str(tmp_path / "env")])
    inherited = str(tmp_path / "missing-pythonhome")
    activate = PowerShellActivator.quote(str(session.creator.bin_dir / "activate.ps1"))
    driver = tmp_path / "pythonhome.ps1"
    driver.write_text(
        f". {activate}\nWrite-Output (Test-Path env:PYTHONHOME)\npython -c 'import sys; print(sys.prefix)'\n"
        "deactivate\nWrite-Output $env:PYTHONHOME\n",
        encoding="utf-8-sig",
    )
    result = subprocess.run(
        [powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", str(driver)],
        env={key: value for key, value in os.environ.items() if key != "COVERAGE_PROCESS_START"}
        | {"PYTHONHOME": inherited},
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=120,
        check=False,
    )

    assert (result.returncode, result.stdout.splitlines(), result.stderr) == (
        0,
        ["False", str(session.creator.dest), inherited],
        "",
    )


@pytest.mark.parametrize("powershell", _POWERSHELLS)
@pytest.mark.parametrize(
    ("original", "suffix"),
    [
        pytest.param("", "", id="empty"),
        pytest.param(f"{os.pathsep}first", f"{os.pathsep}{os.pathsep}first", id="leading-empty-entry"),
        pytest.param("first", f"{os.pathsep}first", id="populated"),
    ],
)
def test_powershell_prepends_bin_to_path(tmp_path: Path, powershell: str, original: str, suffix: str) -> None:
    session: Final = cli_run(["--no-seed", "--activators", "powershell", str(tmp_path / "env")])
    driver: Final = tmp_path / "path.ps1"
    driver.write_text(
        f"$env:PATH = {PowerShellActivator.quote(original)}\n"
        f". {PowerShellActivator.quote(str(session.creator.bin_dir / 'activate.ps1'))}\n"
        "Write-Output $env:PATH\n",
        encoding="utf-8-sig",
    )
    result: Final = subprocess.run(
        [powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", str(driver)],
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=120,
        check=False,
    )

    assert (result.returncode, result.stdout, result.stderr) == (
        0,
        f"{session.creator.dest}/{_VENV_BIN_DIR}{suffix}\n",
        "",
    )


@_INTERPRETER_INDEPENDENT
@pytest.mark.parametrize("powershell", _POWERSHELLS)
@pytest.mark.parametrize(
    ("name", "mode"),
    [
        pytest.param(name, mode, id=f"{mode}-{label}", marks=marks)
        for name, label, marks in (
            ("ordinary", "ordinary", []),
            ("env[one]", "brackets", []),
            ("env\\one", "backslash", [pytest.mark.skipif(IS_WIN, reason="POSIX filename")]),
        )
        for mode in ("implicit", "relative", "drive", "provider", "home")
    ],
)
@pytest.mark.parametrize("configured_prompt", [pytest.param(None, id="name"), pytest.param("custom", id="config")])
def test_powershell_literal_destination(
    tmp_path: Path, name: str, configured_prompt: str | None, mode: str, powershell: str
) -> None:
    dest: Final = os.path.join(str(tmp_path), name)
    prompt_args: Final = {None: [], "custom": ["--prompt", "custom"]}[configured_prompt]
    args: Final = ["--without-pip", "--no-periodic-update", "--activators", "powershell", dest, *prompt_args]
    # Simulated-platform tests change pathlib's separators at collection, so create the environment in a fresh process.
    subprocess.run([sys.executable, "-m", "virtualenv", *args], capture_output=True, check=True)
    venv_args: Final = {
        "implicit": [],
        "relative": ["-VenvDir", PowerShellActivator.quote(name)],
        "drive": ["-VenvDir", PowerShellActivator.quote(f"ReviewEnv:/{name}")],
        "provider": ["-VenvDir", PowerShellActivator.quote(f"FileSystem::{dest}")],
        "home": ["-VenvDir", PowerShellActivator.quote(f"~/{os.path.relpath(dest, os.path.expanduser('~'))}")],
    }[mode]
    activate: Final = " ".join([
        f". {PowerShellActivator.quote(os.path.join(dest, _VENV_BIN_DIR, 'activate.ps1'))}",
        *venv_args,
    ])
    drive_root: Final = PowerShellActivator.quote(str(tmp_path))
    drive_lines: Final = {
        "drive": [f"New-PSDrive -Name ReviewEnv -PSProvider FileSystem -Root {drive_root} | Out-Null"],
    }.get(mode, [])
    lines: Final = [*drive_lines, activate, 'Write-Output "$env:VIRTUAL_ENV|$env:VIRTUAL_ENV_PROMPT"', ""]
    driver: Final = tmp_path / "driver.ps1"
    driver.write_text("\n".join(lines), encoding="utf-8-sig")
    result: Final = subprocess.run(
        [powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", str(driver)],
        capture_output=True,
        text=True,
        encoding="utf-8",
        cwd=tmp_path,
        timeout=120,
        check=True,
    )
    assert (result.stdout.strip(), result.stderr) == (f"{dest}|{configured_prompt or name}", "")


@pytest.mark.parametrize("powershell", _POWERSHELLS)
def test_powershell_reports_missing_config(tmp_path: Path, current_fastest: str, powershell: str) -> None:
    session: Final = cli_run([
        "--without-pip",
        "--no-periodic-update",
        "--activators",
        "powershell",
        "--creator",
        current_fastest,
        str(tmp_path / "env"),
    ])
    (session.creator.dest / "pyvenv.cfg").unlink()
    result: Final = subprocess.run(
        [
            powershell,
            "-NoProfile",
            "-NonInteractive",
            "-ExecutionPolicy",
            "Bypass",
            "-File",
            str(session.creator.bin_dir / "activate.ps1"),
        ],
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=120,
        check=True,
    )
    assert "Cannot find configuration file" in result.stderr


@_INTERPRETER_INDEPENDENT
@pytest.mark.skipif(shutil.which("pwsh") is None, reason="requires PowerShell Core")
@pytest.mark.parametrize(
    "delimiter",
    [
        pytest.param(chr(0x27), id="apostrophe"),
        pytest.param(chr(0x2018), id="left-single"),
        pytest.param(chr(0x2019), id="right-single"),
        pytest.param(chr(0x201A), id="low-9"),
        pytest.param(chr(0x201B), id="high-reversed-9"),
    ],
)
def test_powershell_quote_neutralizes_single_quote_delimiters(tmp_path: Path, delimiter: str) -> None:
    marker = tmp_path / "PWNED"
    payload = f"x{delimiter}+$(New-Item -ItemType File -Path '{marker}')+{delimiter}y"
    literal = PowerShellActivator.quote(payload)
    result = subprocess.run(
        ["pwsh", "-NoProfile", "-NonInteractive", "-Command", f"Write-Output {literal}"],
        capture_output=True,
        text=True,
        encoding="utf-8",
        check=False,
    )

    assert (marker.exists(), result.stdout.strip()) == (False, payload), result.stderr


def test_powershell_pydoc_call_operator(tmp_path) -> None:
    # Test that PowerShell pydoc function uses call operator to handle spaces in python path.
    # GIVEN: A mock interpreter
    class MockInterpreter:
        os = "nt"
        tcl_lib = None
        tk_lib = None

    class MockCreator:
        def __init__(self, dest) -> None:
            self.dest = dest
            self.bin_dir = dest / "Scripts"
            self.bin_dir.mkdir(parents=True)
            self.interpreter = MockInterpreter()
            self.pyenv_cfg = {}
            self.env_name = "test-env"

    creator = MockCreator(tmp_path)
    activator = PowerShellActivator(Namespace(prompt=None))

    # WHEN: Generate activation scripts
    activator.generate(creator)

    # THEN: pydoc function should use call operator & to handle paths with spaces
    activate_content = (creator.bin_dir / "activate.ps1").read_text(encoding="utf-8-sig")

    # The pydoc function should use & call operator to handle paths with spaces
    assert "& python -m pydoc" in activate_content, (
        f"pydoc function should use & call operator. Content:\n{activate_content}"
    )


@pytest.mark.parametrize(
    ("tcl_lib", "tk_lib", "expected_lines"),
    [
        pytest.param(
            "C:\\tcl",
            "C:\\tk",
            [
                "if ('C:\\tcl' -ne \"\")",
                "$env:TCL_LIBRARY = 'C:\\tcl'",
                "if ('C:\\tk' -ne \"\")",
                "$env:TK_LIBRARY = 'C:\\tk'",
                "if (Test-Path variable:_OLD_VIRTUAL_TCL_LIBRARY)",
                "if (Test-Path variable:_OLD_VIRTUAL_TK_LIBRARY)",
            ],
            id="windows",
        ),
        pytest.param(None, None, ["if ('' -ne \"\")", "$env:TCL_LIBRARY = ''"], id="missing"),
    ],
)
def test_powershell_tkinter_generation(tmp_path, tcl_lib, tk_lib, expected_lines) -> None:
    # GIVEN
    class MockInterpreter:
        os = "nt"

    interpreter = MockInterpreter()
    interpreter.tcl_lib = tcl_lib
    interpreter.tk_lib = tk_lib

    class MockCreator:
        def __init__(self, dest) -> None:
            self.dest = dest
            self.bin_dir = dest / "bin"
            self.bin_dir.mkdir()
            self.interpreter = interpreter
            self.pyenv_cfg = {}
            self.env_name = "my-env"

    creator = MockCreator(tmp_path)
    activator = PowerShellActivator(Namespace(prompt=None))

    # WHEN
    activator.generate(creator)
    content = (creator.bin_dir / "activate.ps1").read_text(encoding="utf-8-sig")

    # THEN
    # PKG_CONFIG_PATH is always set
    assert "New-Variable -Scope global -Name _OLD_PKG_CONFIG_PATH" in content
    assert '$env:PKG_CONFIG_PATH = [IO.Path]::Combine($env:VIRTUAL_ENV, "lib", "pkgconfig")' in content
    assert "if (Test-Path variable:_OLD_PKG_CONFIG_PATH)" in content
    assert "$env:PKG_CONFIG_PATH = $variable:_OLD_PKG_CONFIG_PATH" in content
    assert 'Remove-Variable "_OLD_PKG_CONFIG_PATH" -Scope global' in content
    assert [line for line in expected_lines if line not in content] == []


@pytest.mark.slow
def test_powershell(activation_tester_class, activation_tester, monkeypatch) -> None:
    monkeypatch.setenv("TERM", "xterm")

    class PowerShell(activation_tester_class):
        def __init__(self, session) -> None:
            cmd = "pwsh" if shutil.which("pwsh") else "powershell.exe" if sys.platform == "win32" else "pwsh"
            super().__init__(PowerShellActivator, session, cmd, "activate.ps1", "ps1")
            self._version_cmd = [cmd, "-c", "$PSVersionTable"]
            self._invoke_script = [cmd, "-NonInteractive", "-NoProfile", "-ExecutionPolicy", "ByPass", "-File"]
            self.activate_cmd = "."
            self.script_encoding = "utf-8-sig"

        def _get_test_lines(self, activate_script):
            return super()._get_test_lines(activate_script)

        def invoke_script(self):
            return [self.cmd, "-File"]

        def print_state(self, names: tuple[str, ...]) -> str:
            lookups = (f'if ($env:{var} -eq $null) {{ "None" }} else {{ $env:{var} }}' for var in names)
            return "; ".join([self.print_python_exe(), *lookups])

        def print_prompt(self) -> str:
            return "prompt"

        def activate_call(self, script):
            # Commands are called without quotes in PowerShell
            return f"{self.activate_cmd} {self.quote(str(script))}".strip()

        def quote(self, string):
            # Tester will pass strings to native commands on Windows so extra parsing rules are used. Check
            # `PowerShellActivator.quote` for more details.
            text = PowerShellActivator.quote(string)
            return text.replace('"', '""') if sys.platform == "win32" else text

    activation_tester(PowerShell)


@pytest.mark.parametrize("powershell", _POWERSHELLS)
def test_powershell_rejects_non_filesystem_drive(tmp_path: Path, powershell: str) -> None:
    session: Final = cli_run([str(tmp_path / "env"), "--no-seed", "--activators", "powershell"])
    result: Final = subprocess.run(
        [
            powershell,
            "-NoProfile",
            "-NonInteractive",
            "-ExecutionPolicy",
            "Bypass",
            "-Command",
            (
                f". {PowerShellActivator.quote(str(session.creator.bin_dir / 'activate.ps1'))} -VenvDir 'Env:/x'; "
                "Write-Output $env:VIRTUAL_ENV"
            ),
        ],
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=120,
        check=False,
    )
    assert (result.returncode, result.stdout, "Cannot resolve environment directory 'Env:/x'" in result.stderr) == (
        1,
        "",
        True,
    ), result.stderr


@pytest.mark.skipif(not IS_WIN, reason="Windows registry provider")
@pytest.mark.parametrize("powershell", _POWERSHELLS)
def test_powershell_home_from_registry(tmp_path: Path, powershell: str) -> None:
    session: Final = cli_run([str(tmp_path / "env"), "--no-seed", "--activators", "powershell"])
    result: Final = subprocess.run(
        [
            powershell,
            "-NoProfile",
            "-NonInteractive",
            "-ExecutionPolicy",
            "Bypass",
            "-Command",
            f"Set-Location HKCU:\\; . {PowerShellActivator.quote(str(session.creator.bin_dir / 'activate.ps1'))} -VenvDir '~'; Write-Output $env:VIRTUAL_ENV",
        ],
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=120,
        check=True,
    )
    assert result.stdout.strip() == os.path.expanduser("~")


def _run_powershell(powershell: str, driver: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", str(driver)],
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=120,
        check=False,
    )


def _nested_sessions(tmp_path: Path) -> tuple:
    first = cli_run(["--no-seed", "--activators", "powershell", str(tmp_path / "env A")])
    second = cli_run(["--no-seed", "--activators", "powershell", str(tmp_path / "env B nested")])
    return first, second


@pytest.mark.parametrize("powershell", _POWERSHELLS)
def test_powershell_nested_activation_unwinds_in_reverse(tmp_path: Path, powershell: str) -> None:
    first, second = _nested_sessions(tmp_path)
    activate_a = PowerShellActivator.quote(str(first.creator.bin_dir / "activate.ps1"))
    activate_b = PowerShellActivator.quote(str(second.creator.bin_dir / "activate.ps1"))
    python_a = str(first.creator.exe.parent / os.path.basename(sys.executable))
    python_b = str(second.creator.exe.parent / os.path.basename(sys.executable))
    driver = tmp_path / "nested.ps1"
    driver.write_text(
        "\n".join([
            "$ErrorActionPreference = 'Stop'",
            f". {activate_a}",
            f". {activate_b} -Nested",
            "(Get-Command python).Source",
            "deactivate",
            "(Get-Command python).Source",
            "deactivate",
            "if (Test-Path function:deactivate) { 'deactivate-left' } else { 'deactivate-gone' }",
            "if ($null -eq $env:VIRTUAL_ENV) { 'venv-unset' } else { $env:VIRTUAL_ENV }",
            "",
        ]),
        encoding="utf-8-sig",
    )

    result = _run_powershell(powershell, driver)

    assert (result.returncode, result.stderr) == (0, "")
    # The template joins the venv folder to the bin name with a forward slash, so normalize separators for comparison.
    assert [os.path.normcase(line) for line in result.stdout.splitlines()] == [
        os.path.normcase(python_b),
        os.path.normcase(python_a),
        "deactivate-gone",
        "venv-unset",
    ]


@pytest.mark.parametrize("powershell", _POWERSHELLS)
def test_powershell_nested_activation_stacks_path_and_names(tmp_path: Path, powershell: str) -> None:
    first, second = _nested_sessions(tmp_path)
    activate_a = PowerShellActivator.quote(str(first.creator.bin_dir / "activate.ps1"))
    activate_b = PowerShellActivator.quote(str(second.creator.bin_dir / "activate.ps1"))
    driver = tmp_path / "stack.ps1"
    driver.write_text(
        "\n".join([
            "$ErrorActionPreference = 'Stop'",
            f". {activate_a}",
            f". {activate_b} -Nested",
            "Write-Output $env:VIRTUAL_ENV",
            "Write-Output $env:VIRTUAL_ENV_PROMPT",
            "Write-Output $_VIRTUAL_ENV_STACK.Count",
            "Write-Output ($env:PATH.Split([IO.Path]::PathSeparator)[0..1] -join [IO.Path]::PathSeparator)",
            "deactivate",
            "Write-Output $env:VIRTUAL_ENV",
            "Write-Output $_VIRTUAL_ENV_STACK.Count",
            "",
        ]),
        encoding="utf-8-sig",
    )

    result = _run_powershell(powershell, driver)

    assert (result.returncode, result.stderr) == (0, "")
    expected_path = os.pathsep.join([str(second.creator.bin_dir), str(first.creator.bin_dir)])
    lines = result.stdout.splitlines()
    assert lines[:3] == [str(second.creator.dest), "env B nested", "1"]
    assert os.path.normcase(lines[3]) == os.path.normcase(expected_path)
    assert lines[4:] == [str(first.creator.dest), "0"]


@pytest.mark.parametrize("powershell", _POWERSHELLS)
def test_powershell_nested_repeat_of_active_layer_is_noop(tmp_path: Path, powershell: str) -> None:
    first, second = _nested_sessions(tmp_path)
    activate_a = PowerShellActivator.quote(str(first.creator.bin_dir / "activate.ps1"))
    activate_b = PowerShellActivator.quote(str(second.creator.bin_dir / "activate.ps1"))
    driver = tmp_path / "repeat.ps1"
    driver.write_text(
        "\n".join([
            "$ErrorActionPreference = 'Stop'",
            f". {activate_a}",
            f". {activate_a} -Nested",
            "Write-Output $_VIRTUAL_ENV_STACK.Count",
            f". {activate_b} -Nested",
            f". {activate_b} -Nested",
            "Write-Output $_VIRTUAL_ENV_STACK.Count",
            "deactivate",
            "Write-Output $_VIRTUAL_ENV_STACK.Count",
            "",
        ]),
        encoding="utf-8-sig",
    )

    result = _run_powershell(powershell, driver)

    assert (result.returncode, result.stderr) == (0, "")
    assert result.stdout.splitlines() == ["0", "1", "0"]


@pytest.mark.parametrize("powershell", _POWERSHELLS)
def test_powershell_default_activation_replaces_instead_of_nesting(tmp_path: Path, powershell: str) -> None:
    first, second = _nested_sessions(tmp_path)
    activate_a = PowerShellActivator.quote(str(first.creator.bin_dir / "activate.ps1"))
    activate_b = PowerShellActivator.quote(str(second.creator.bin_dir / "activate.ps1"))
    driver = tmp_path / "replace.ps1"
    driver.write_text(
        "\n".join([
            "$ErrorActionPreference = 'Stop'",
            f". {activate_a}",
            f". {activate_b} -Nested",
            f". {activate_a}",
            "Write-Output $_VIRTUAL_ENV_STACK.Count",
            "Write-Output $env:VIRTUAL_ENV",
            "deactivate",
            "if ($null -eq $env:VIRTUAL_ENV) { 'venv-unset' } else { 'venv-left' }",
            "if (Test-Path function:deactivate) { 'deactivate-left' } else { 'deactivate-gone' }",
            "",
        ]),
        encoding="utf-8-sig",
    )

    result = _run_powershell(powershell, driver)

    assert (result.returncode, result.stderr) == (0, "")
    assert result.stdout.splitlines() == ["0", str(first.creator.dest), "venv-unset", "deactivate-gone"]


@pytest.mark.parametrize("powershell", _POWERSHELLS)
def test_powershell_deactivate_without_activation_is_safe(tmp_path: Path, powershell: str) -> None:
    session = cli_run(["--no-seed", "--activators", "powershell", str(tmp_path / "env")])
    activate = PowerShellActivator.quote(str(session.creator.bin_dir / "activate.ps1"))
    driver = tmp_path / "idle.ps1"
    driver.write_text(
        "\n".join([
            "deactivate",
            "if (Test-Path function:deactivate) { 'deactivate-left' } else { 'deactivate-gone' }",
            f". {activate}",
            "deactivate",
            "if (Test-Path function:deactivate) { 'deactivate-left' } else { 'deactivate-gone' }",
            "",
        ]),
        encoding="utf-8-sig",
    )

    result = _run_powershell(powershell, driver)

    assert (result.returncode, result.stderr) == (0, "")
    assert result.stdout.splitlines() == ["deactivate-gone", "deactivate-gone"]


@pytest.mark.parametrize("powershell", _POWERSHELLS)
def test_powershell_nested_prompt_restores_each_layer_and_cleans_handles(
    tmp_path: Path, powershell: str
) -> None:
    first, second = _nested_sessions(tmp_path)
    activate_a = PowerShellActivator.quote(str(first.creator.bin_dir / "activate.ps1"))
    activate_b = PowerShellActivator.quote(str(second.creator.bin_dir / "activate.ps1"))
    driver = tmp_path / "prompt-layers.ps1"
    driver.write_text(
        "\n".join([
            "$ErrorActionPreference = 'Stop'",
            f". {activate_a}",
            f". {activate_b} -Nested",
            "(& prompt 6>&1) -join ''",
            "deactivate",
            "(& prompt 6>&1) -join ''",
            "deactivate",
            "(& prompt 6>&1) -join ''",
            "@(Get-Variable -Name '_VIRTUAL_ENV_PROMPT_PREV_*' -ErrorAction SilentlyContinue).Count",
            "",
        ]),
        encoding="utf-8-sig",
    )

    result = _run_powershell(powershell, driver)

    assert (result.returncode, result.stderr) == (0, "")
    lines = result.stdout.splitlines()
    assert "(env B nested) (env A) " in lines[0]
    assert lines[1].startswith("(env A) ")
    assert not lines[2].startswith("(env")
    assert lines[3] == "0"
