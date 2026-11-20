from __future__ import annotations

import logging
import sys
from pathlib import Path
from typing import TYPE_CHECKING

import pytest

from pip._internal.build_env.venv import VenvBuildEnvironment
from pip._internal.build_env.virtual import VirtualBuildEnvironment
from pip._internal.utils.temp_dir import global_tempdir_manager

if TYPE_CHECKING:
    from pip._internal.build_env.base import Prefix


class FakeInstaller:
    """An installer that records calls and can be told to fail."""

    def __init__(self, fail: bool = False) -> None:
        self.calls: list[tuple[tuple[str, ...], str]] = []
        self._fail = fail

    def install(
        self,
        requirements,
        prefix: "Prefix",
        *,
        kind: str,
        for_req,
    ) -> None:
        self.calls.append((tuple(requirements), kind))
        if self._fail:
            raise RuntimeError("the second install failed")


def _make_dist_info(lib_dir: Path, name: str, version: str) -> None:
    """Create a minimal dist-info directory so the metadata env sees a dist."""
    dist_info = lib_dir / f"{name}-{version}.dist-info"
    dist_info.mkdir(parents=True, exist_ok=True)
    (dist_info / "METADATA").write_text(
        f"Metadata-Version: 2.1\nName: {name}\nVersion: {version}\n"
    )
    (dist_info / "WHEEL").write_text(
        "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n"
    )
    (dist_info / "RECORD").write_text("")
    (dist_info / "top_level.txt").write_text(name + "\n")


def _make_env(installer: FakeInstaller) -> VirtualBuildEnvironment:
    with global_tempdir_manager():
        return VirtualBuildEnvironment(installer)


def test_phase_check_reports_selected_version_vs_new_constraint() -> None:
    installer = FakeInstaller()
    env = _make_env(installer)
    overlay_lib = Path(env._prefixes["overlay"].lib_dirs[0])
    _make_dist_info(overlay_lib, "simplewheel", "1.0")

    conflicting, missing = env.check_requirements_for_phase(
        ["simplewheel==2.0"], phase="normal"
    )

    assert conflicting == {("simplewheel==1.0", "simplewheel==2.0")}
    assert not missing


def test_phase_check_overlay_priority_is_preserved() -> None:
    # Same package in both prefixes: the overlay selection wins.
    installer = FakeInstaller()
    env = _make_env(installer)
    overlay_lib = Path(env._prefixes["overlay"].lib_dirs[0])
    normal_lib = Path(env._prefixes["normal"].lib_dirs[0])
    _make_dist_info(overlay_lib, "pkg", "2.0")
    _make_dist_info(normal_lib, "pkg", "4.3")

    conflicting, missing = env.check_requirements_for_phase(
        ["pkg==4.3"], phase="normal"
    )

    # The overlay's 2.0 is what is "selected", so requesting 4.3 conflicts.
    assert conflicting == {("pkg==2.0", "pkg==4.3")}

    # A request satisfied by the overlay version must not conflict.
    conflicting, missing = env.check_requirements_for_phase(
        ["pkg==2.0"], phase="normal"
    )
    assert not conflicting
    assert not missing


def test_phase_check_missing_requirements_are_returned() -> None:
    installer = FakeInstaller()
    env = _make_env(installer)

    conflicting, missing = env.check_requirements_for_phase(
        ["not_installed>=1.0"], phase="normal"
    )

    assert not conflicting
    assert missing == {"not_installed>=1.0"}


def test_failed_second_install_resets_prefix_state() -> None:
    installer = FakeInstaller(fail=True)
    env = _make_env(installer)
    prefix = env._prefixes["normal"]
    normal_lib = Path(prefix.lib_dirs[0])
    normal_lib.mkdir(parents=True, exist_ok=True)
    marker = normal_lib / "leftover-from-failed-install.txt"
    marker.write_text("partial state")

    with pytest.raises(RuntimeError, match="the second install failed"):
        env.install_requirements(
            ["some-dependency"], "normal", kind="backend dependencies"
        )

    # The prefix must not be considered set up anymore...
    assert prefix.setup is False
    # ...and the partial installation must have been wiped.
    assert not marker.exists()

    # A subsequent build can therefore perform the install again instead of
    # inheriting the failed attempt's state.
    installer._fail = False
    env.install_requirements(
        ["some-dependency"], "normal", kind="backend dependencies"
    )
    assert prefix.setup is True


def test_successful_install_keeps_prefix_state() -> None:
    installer = FakeInstaller()
    env = _make_env(installer)
    prefix = env._prefixes["overlay"]

    env.install_requirements(["backend"], "overlay", kind="build dependencies")

    assert prefix.setup is True
    assert installer.calls == [(("backend",), "build dependencies")]


def test_failed_overlay_install_does_not_mark_prefix_setup(
    caplog: pytest.LogCaptureFixture,
) -> None:
    installer = FakeInstaller(fail=True)
    env = _make_env(installer)
    prefix = env._prefixes["overlay"]

    with caplog.at_level(logging.WARNING):
        with pytest.raises(RuntimeError):
            env.install_requirements(
                ["backend"], "overlay", kind="build dependencies"
            )

    assert prefix.setup is False


class _FailingVenvInstaller:
    def install(self, requirements, prefix, *, kind, for_req) -> None:
        raise RuntimeError("venv phase install failed")


def test_venv_failed_install_forgets_phase(tmp_path: Path) -> None:
    # Bypass __init__ (which creates a real virtual environment) to exercise
    # only the phase bookkeeping and rollback state machine.
    env = VenvBuildEnvironment.__new__(VenvBuildEnvironment)
    env._installer = _FailingVenvInstaller()
    env._setup_phases = set()
    env._env_path = str(tmp_path)
    env.python_executable = sys.executable

    with pytest.raises(RuntimeError, match="venv phase install failed"):
        env.install_requirements(
            ["some-dependency"], "normal", kind="backend dependencies"
        )

    # The failed phase must not be remembered, so a retry is allowed to run
    # rather than tripping the same-phase assertion.
    assert "normal" not in env._setup_phases
    # reset_install is idempotent.
    env.reset_install("normal")
    assert "normal" not in env._setup_phases


def test_venv_rejects_repeated_same_phase_without_reset() -> None:
    env = VenvBuildEnvironment.__new__(VenvBuildEnvironment)
    env._installer = FakeInstaller()
    env._setup_phases = {"overlay"}

    with pytest.raises(AssertionError):
        env.install_requirements(
            ["backend"], "overlay", kind="build dependencies"
        )
