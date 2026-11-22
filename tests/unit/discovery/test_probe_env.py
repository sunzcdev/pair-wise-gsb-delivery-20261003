from __future__ import annotations

import os
import sys
from argparse import Namespace
from typing import TYPE_CHECKING, Final

import pytest
from python_discovery import PythonInfo

from virtualenv.discovery.builtin import Builtin, get_interpreter
from virtualenv.discovery.probe_env import (
    CLEAN,
    INHERIT,
    POLICIES,
    SCRUB,
    ProbeEnvironmentError,
    build_probe_env,
    probe_interpreter,
    steering_variables,
    withheld_variables,
)
from virtualenv.run import session_via_cli

if TYPE_CHECKING:
    from collections.abc import Mapping
    from pathlib import Path

_PARENT: Final[dict[str, str]] = {
    "PATH": "/usr/bin",
    "HOME": "/home/dev",
    "SystemRoot": "C:/Windows",
    "PYTHONHOME": "/wrong/home",
    "PYTHONPATH": "/wrong/path",
    "PYTHONSTARTUP": "/wrong/startup.py",
    "UNRELATED": "kept",
}


class _RecordingProbe:
    """A probe double that records the environment it received and answers only when a variable is absent."""

    def __init__(self, *, answers_when_absent: str | None = None) -> None:
        self.answers_when_absent = answers_when_absent
        self.calls: list[tuple[str, dict[str, str]]] = []

    def __call__(self, executable: str, env: Mapping[str, str]) -> PythonInfo:
        self.calls.append((executable, dict(env)))
        if self.answers_when_absent is not None and self.answers_when_absent in env:
            raise RuntimeError(f"cannot query {executable} while {self.answers_when_absent} is set")
        info = PythonInfo.current_system()
        info.executable = executable
        return info

    @property
    def envs(self) -> list[dict[str, str]]:
        return [probe_env for _, probe_env in self.calls]


@pytest.mark.parametrize(
    ("policy", "kept", "dropped"),
    [
        pytest.param(INHERIT, {"PYTHONHOME", "PYTHONPATH", "UNRELATED"}, set(), id="inherit"),
        pytest.param(SCRUB, {"UNRELATED", "PATH"}, {"PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP"}, id="scrub"),
        pytest.param(CLEAN, {"PATH", "SystemRoot", "HOME"}, {"PYTHONHOME", "PYTHONPATH", "UNRELATED"}, id="clean"),
    ],
)
def test_build_probe_env_selects_variables(policy: str, kept: set[str], dropped: set[str]) -> None:
    result = build_probe_env(policy, _PARENT)

    assert kept <= set(result)
    assert not dropped & set(result)
    assert result == {key: _PARENT[key] for key in result}


def test_build_probe_env_leaves_parent_untouched() -> None:
    before = dict(_PARENT)

    build_probe_env(SCRUB, _PARENT)
    build_probe_env(CLEAN, _PARENT)

    assert _PARENT == before


def test_build_probe_env_rejects_unknown_policy() -> None:
    with pytest.raises(ValueError, match="unknown probe environment policy"):
        build_probe_env("everything", _PARENT)


def test_withheld_variables_reports_dropped_names() -> None:
    assert withheld_variables(SCRUB, _PARENT) == ("PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP")
    assert withheld_variables(INHERIT, _PARENT) == ()


def test_steering_variables_ignores_case() -> None:
    assert steering_variables({"pythonhome": "x", "PYTHONPATH": "y", "PATH": "z"}) == ("PYTHONPATH", "pythonhome")


@pytest.mark.parametrize("policy", POLICIES)
def test_probe_interpreter_hands_policy_environment(policy: str) -> None:
    probe = _RecordingProbe()

    probe_interpreter("/bin/python", policy=policy, env=_PARENT, probe=probe)

    assert probe.envs == [build_probe_env(policy, _PARENT)]


def test_probe_interpreter_keeps_requested_executable() -> None:
    probe = _RecordingProbe()

    result = probe_interpreter("/opt/odd path/python", policy=SCRUB, env=_PARENT, probe=probe)

    assert result.executable == "/opt/odd path/python"
    assert probe.calls[0][0] == "/opt/odd path/python"


def test_failure_caused_by_parent_environment_is_classified() -> None:
    probe = _RecordingProbe(answers_when_absent="PYTHONHOME")

    with pytest.raises(ProbeEnvironmentError) as context:
        probe_interpreter("/bin/python", policy=INHERIT, env=_PARENT, probe=probe)

    error = context.value
    assert error.external_env is True
    assert error.policy == INHERIT
    assert error.steering == ("PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP")
    assert error.recovered is not None
    assert "parent environment disturbed the probe" in str(error)
    # the first try inherited the variable, every retry withheld it
    assert "PYTHONHOME" in probe.envs[0]
    assert all("PYTHONHOME" not in env for env in probe.envs[1:])


def test_failure_of_broken_interpreter_is_classified() -> None:
    def broken(executable: str, env: Mapping[str, str]) -> PythonInfo:
        raise RuntimeError(f"{executable} is broken")

    with pytest.raises(ProbeEnvironmentError) as context:
        probe_interpreter("/bin/python", policy=INHERIT, env=_PARENT, probe=broken)

    error = context.value
    assert error.external_env is False
    assert error.recovered is None
    assert "the interpreter cannot report its information" in str(error)


def test_probe_interpreter_rejects_unknown_policy() -> None:
    with pytest.raises(ValueError, match="unknown probe environment policy"):
        probe_interpreter("/bin/python", policy="everything", env=_PARENT, probe=_RecordingProbe())


def test_probe_env_option_is_documented_and_validated(capsys) -> None:
    with pytest.raises(SystemExit) as context:
        session_via_cli(["--help"])

    assert context.value.code == 0
    out = " ".join(capsys.readouterr().out.split())
    assert "--probe-env" in out
    assert "inherit passes them all, scrub drops the ones that steer a Python at start-up" in out


def test_probe_env_rejects_unknown_choice(tmp_path: Path) -> None:
    with pytest.raises(SystemExit) as context:
        session_via_cli(["--probe-env", "everything", str(tmp_path / "env")])

    assert context.value.code == 2


def test_discover_defaults_to_inherit() -> None:
    builtin = Builtin(Namespace(app_data=None, try_first_with=[], python=["missing"], env=_PARENT))

    assert builtin.probe_env == INHERIT
    assert builtin.probe_environment == _PARENT


def test_discover_builds_scrubbed_environment() -> None:
    builtin = Builtin(Namespace(app_data=None, try_first_with=[], python=["missing"], env=_PARENT, probe_env=SCRUB))

    assert "PYTHONHOME" not in builtin.probe_environment
    assert builtin.probe_environment["UNRELATED"] == "kept"


def test_builtin_run_passes_probe_environment(session_app_data, mocker) -> None:
    called = mocker.patch(
        "virtualenv.discovery.builtin.get_interpreter", autospec=True, return_value=PythonInfo.current_system()
    )
    builtin = Builtin(
        Namespace(app_data=session_app_data, try_first_with=[], python=[sys.executable], env=_PARENT, probe_env=CLEAN)
    )

    result = builtin.run()

    assert result is not None
    assert called.call_args.kwargs["env"] == _PARENT
    assert "PYTHONHOME" not in called.call_args.kwargs["probe_env"]
    assert called.call_args.args[:2] == (sys.executable, [])


def test_builtin_run_explains_environment_caused_failure(session_app_data, tmp_path, mocker) -> None:
    executable = tmp_path / "python"
    executable.write_text("", encoding="utf-8")
    mocker.patch(
        "virtualenv.discovery.builtin.get_interpreter", autospec=True, side_effect=RuntimeError("probe failed")
    )
    classify = mocker.patch("virtualenv.discovery.builtin.probe_interpreter", autospec=True)
    builtin = Builtin(
        Namespace(app_data=session_app_data, try_first_with=[], python=[str(executable)], env=_PARENT, probe_env=INHERIT)
    )

    with pytest.raises(RuntimeError, match="probe failed"):
        builtin.run()

    assert classify.call_args.args[0] == str(executable)
    assert classify.call_args.kwargs["policy"] == INHERIT


def test_absolute_missing_path_is_not_classified(session_app_data, mocker) -> None:
    mocker.patch("virtualenv.discovery.builtin.get_interpreter", autospec=True, return_value=None)
    classify = mocker.patch("virtualenv.discovery.builtin.probe_interpreter", autospec=True)
    missing = os.path.join(os.sep, "does", "not", "exist", "python")
    builtin = Builtin(
        Namespace(app_data=session_app_data, try_first_with=[], python=[missing], env=_PARENT, probe_env=INHERIT)
    )

    assert builtin.run() is None
    classify.assert_not_called()


def test_get_interpreter_uses_probe_env(session_app_data, mocker) -> None:
    called = mocker.patch("virtualenv.discovery.builtin.python_discovery.get_interpreter", autospec=True)
    probe_env = build_probe_env(SCRUB, _PARENT)

    get_interpreter(sys.executable, [], app_data=session_app_data, env=_PARENT, probe_env=probe_env)

    assert called.call_args.args[3] == probe_env


class _DiscoveryCall:
    """One recorded discovery request: the requested key, the search environment and the probe environment."""

    def __init__(self, key: str, env: Mapping[str, str], probe_env: Mapping[str, str]) -> None:
        self.key = key
        self.env = dict(env)
        self.probe_env = dict(probe_env)


class _RecordingDiscovery:
    """A discovery double that records the search environment and the probe environment it was handed."""

    def __init__(self) -> None:
        self.calls: list[_DiscoveryCall] = []

    def __call__(  # ruff:ignore[too-many-arguments] each argument is a separate discovery input
        self,
        key: str,
        try_first_with: object,
        cache: object = None,
        env: Mapping[str, str] | None = None,
        app_data: object = None,
        probe_env: Mapping[str, str] | None = None,
        **_: object,
    ) -> PythonInfo:
        self.calls.append(_DiscoveryCall(key, env or {}, probe_env or {}))
        return PythonInfo.current_system()

    @property
    def probe_envs(self) -> list[dict[str, str]]:
        return [call.probe_env for call in self.calls]


def test_two_doubles_and_one_request_show_probe_input_source(tmp_path: Path, mocker) -> None:
    # the fixed request: the project names its interpreter and asks for discovery only, nothing is created
    request: Final = ["-p", sys.executable, "--no-seed", "--activators=", str(tmp_path / "env")]
    parent: Final = {"PATH": os.environ.get("PATH", ""), "PYTHONHOME": "/launcher/home", "PYTHONPATH": "/launcher/path"}
    inherited, scrubbed = _RecordingDiscovery(), _RecordingDiscovery()
    discovery = mocker.patch("virtualenv.discovery.builtin.get_interpreter", autospec=True)

    results = {}
    for policy, double in ((INHERIT, inherited), (SCRUB, scrubbed)):
        discovery.side_effect = double
        results[policy] = session_via_cli(["--probe-env", policy, *request], env=dict(parent))

    # both doubles were asked for the project's executable, unchanged, and both received the caller's environment
    assert [call.key for call in inherited.calls] == [call.key for call in scrubbed.calls] == [sys.executable]
    assert inherited.calls[0].env == scrubbed.calls[0].env == parent
    # only the probe environment differs: the inherit double saw the launcher's PYTHONHOME, the scrub double did not
    assert inherited.probe_envs[0]["PYTHONHOME"] == "/launcher/home"
    assert "PYTHONHOME" not in scrubbed.probe_envs[0]
    assert inherited.probe_envs[0]["PATH"] == scrubbed.probe_envs[0]["PATH"]
    # the creator of each session still holds the caller's snapshot, so its parameters survive the probe policy
    for session in results.values():
        assert session.creator.env["PYTHONHOME"] == "/launcher/home"
        assert session.creator.dest == (tmp_path / "env").resolve()


# The integration tests below run the real subprocess probe; a bogus PYTHONHOME disturbs it only under CPython.
_NEEDS_CPYTHON: Final = pytest.mark.skipif(
    PythonInfo.current_system().implementation != "CPython", reason="only CPython honors PYTHONHOME at start-up"
)


@pytest.fixture
def disturbed_env(tmp_path: Path) -> dict[str, str]:
    parent = dict(os.environ)
    parent["PYTHONHOME"] = str(tmp_path / "missing-home")
    return parent


@_NEEDS_CPYTHON
def test_inherit_policy_reports_environment_disturbance(disturbed_env: dict[str, str]) -> None:
    with pytest.raises(ProbeEnvironmentError) as context:
        probe_interpreter(sys.executable, policy=INHERIT, env=disturbed_env)

    error = context.value
    assert error.external_env is True
    assert "PYTHONHOME" in error.steering
    assert error.recovered is not None
    assert error.recovered.version_info == PythonInfo.current_system().version_info


@_NEEDS_CPYTHON
@pytest.mark.parametrize("policy", [SCRUB, CLEAN])
def test_narrow_policies_probe_despite_disturbance(policy: str, disturbed_env: dict[str, str]) -> None:
    result = probe_interpreter(sys.executable, policy=policy, env=disturbed_env)

    assert os.path.normcase(result.executable) == os.path.normcase(sys.executable)
    assert result.version_info == PythonInfo.current_system().version_info


@_NEEDS_CPYTHON
def test_broken_interpreter_stays_broken_under_every_policy(tmp_path: Path) -> None:
    # a file that exists but cannot be queried as a Python interpreter
    broken = tmp_path / "broken-python.txt"
    broken.write_text("not an interpreter", encoding="utf-8")

    with pytest.raises(ProbeEnvironmentError) as context:
        probe_interpreter(str(broken), policy=INHERIT, env=dict(os.environ))

    assert context.value.external_env is False
    assert context.value.recovered is None
    assert "cannot report its information" in str(context.value)


@_NEEDS_CPYTHON
def test_session_with_scrubbed_probe_and_disturbed_launcher(tmp_path: Path, disturbed_env: dict[str, str]) -> None:
    dest = tmp_path / "env"

    session = session_via_cli(
        ["-p", sys.executable, "--probe-env", SCRUB, "--no-seed", "--activators=", str(dest)],
        env=disturbed_env,
    )

    assert session.interpreter.version_info == PythonInfo.current_system().version_info
    assert session.creator.dest == dest.resolve()
    # the creator received the disturbed launcher environment unchanged, only the probe was scrubbed
    assert session.creator.env["PYTHONHOME"] == disturbed_env["PYTHONHOME"]

