from __future__ import annotations

import json
from pathlib import Path
from typing import TYPE_CHECKING

import pytest

from virtualenv.completion import (
    ACTION_CREATE,
    ACTION_RESUME,
    ACTION_REUSE,
    PHASE_CONFIGURED,
    PHASE_CREATED,
    PHASE_SEEDED,
    PHASES,
    CompletionState,
    CompletionStore,
    ForeignEnvironmentError,
    creator_finished,
    decide,
    looks_like_virtualenv,
    seeder_finished,
)

if TYPE_CHECKING:
    from pytest_mock import MockerFixture


def test_state_starts_empty(tmp_path: Path) -> None:
    store = CompletionStore(tmp_path / "env")

    state = store.load()

    assert (state.phases, state.last_phase, state.complete) == ((), None, False)


def test_record_writes_phases_in_order_and_atomically(tmp_path: Path) -> None:
    store = CompletionStore(tmp_path / "env")

    for phase in PHASES:
        store.record(phase)

    state = store.load()
    assert state.phases == PHASES
    assert state.complete is True
    payload = json.loads(store.state_path.read_text(encoding="utf-8"))
    assert payload["phases"] == list(PHASES)
    assert payload["dest"] == str(store.dest)
    # the temp file used for the atomic swap never stays behind
    assert not store.state_path.with_name(f"{store.state_path.name}.tmp").exists()


def test_record_out_of_order_keeps_the_canonical_order(tmp_path: Path) -> None:
    store = CompletionStore(tmp_path / "env")

    store.record(PHASE_SEEDED)
    store.record(PHASE_CREATED)

    assert store.load().phases == (PHASE_CREATED, PHASE_SEEDED)


def test_corrupt_state_is_ignored(tmp_path: Path) -> None:
    store = CompletionStore(tmp_path / "env")
    store.state_path.parent.mkdir(parents=True)
    store.state_path.write_text("{not json", encoding="utf-8")

    assert store.load().phases == ()


def test_unknown_phases_are_dropped(tmp_path: Path) -> None:
    store = CompletionStore(tmp_path / "env")
    store.state_path.parent.mkdir(parents=True)
    store.state_path.write_text(
        json.dumps({"phases": [PHASE_CREATED, "mystery", PHASE_SEEDED]}),
        encoding="utf-8",
    )

    assert store.load().phases == (PHASE_CREATED, PHASE_SEEDED)


def test_clear_removes_destination_and_state(tmp_path: Path) -> None:
    dest = tmp_path / "env"
    dest.mkdir()
    store = CompletionStore(dest)
    store.record(PHASE_CREATED)

    store.clear()

    assert not dest.exists()


def test_lock_path_sits_beside_the_destination(tmp_path: Path) -> None:
    store = CompletionStore(tmp_path / "env")

    assert store.lock_path == tmp_path / ".virtualenv-complete.lock"


def test_lock_is_reentrant_and_reusable(tmp_path: Path) -> None:
    store = CompletionStore(tmp_path / "env")
    seen: list[str] = []

    with store.locked():
        seen.append("first-enter")
        with store.locked():  # re-entrant for the same thread/process
            seen.append("first-nested")
    with store.locked():
        seen.append("second-enter")

    assert seen == ["first-enter", "first-nested", "second-enter"]


def _creator(mocker: MockerFixture, dest: Path, exe_path: Path | None):
    creator = mocker.Mock()
    creator.dest = dest
    creator.clear = False
    creator.exe = exe_path
    return creator


def test_decide_create_for_missing_destination(tmp_path: Path, mocker: MockerFixture) -> None:
    dest = tmp_path / "env"
    creator = _creator(mocker, dest, exe_path=None)

    assert decide(dest, CompletionState(dest, ()), False, creator) == ACTION_CREATE


def test_decide_create_for_cleared_destination(tmp_path: Path, mocker: MockerFixture) -> None:
    dest = tmp_path / "env"
    dest.mkdir()
    exe = dest / "python.exe"
    exe.touch()
    creator = _creator(mocker, dest, exe_path=exe)
    creator.clear = True

    assert decide(dest, CompletionState(dest, (PHASE_CONFIGURED,)), True, creator) == ACTION_CREATE


def test_decide_reuse_for_complete_environment(tmp_path: Path, mocker: MockerFixture) -> None:
    dest = tmp_path / "env"
    dest.mkdir()
    (dest / "pyvenv.cfg").write_text("home = x\nvirtualenv = 1.0\n", encoding="utf-8")
    exe = dest / "python.exe"
    exe.touch()
    creator = _creator(mocker, dest, exe_path=exe)

    action = decide(dest, CompletionState(dest, PHASES), False, creator)

    assert action == ACTION_REUSE


def test_decide_resumes_when_seeding_is_requested_but_never_done(tmp_path: Path, mocker: MockerFixture) -> None:
    dest = tmp_path / "env"
    dest.mkdir()
    (dest / "pyvenv.cfg").write_text("home = x\nvirtualenv = 1.0\n", encoding="utf-8")
    exe = dest / "python.exe"
    exe.touch()
    creator = _creator(mocker, dest, exe_path=exe)
    # an enabled seeder asks for packages, but the environment's purelib is empty
    creator.purelib = dest / "purelib"
    seeder = mocker.Mock()
    seeder.enabled = True

    action = decide(dest, CompletionState(dest, PHASES), False, creator, seeder)

    assert action == ACTION_RESUME


def test_decide_reuses_when_enabled_seeder_is_satisfied(tmp_path: Path, mocker: MockerFixture) -> None:
    dest = tmp_path / "env"
    dest.mkdir()
    (dest / "pyvenv.cfg").write_text("home = x\nvirtualenv = 1.0\n", encoding="utf-8")
    exe = dest / "python.exe"
    exe.touch()
    creator = _creator(mocker, dest, exe_path=exe)
    creator.purelib = dest / "purelib"
    creator.purelib.mkdir()
    (creator.purelib / "pip").mkdir()
    seeder = mocker.Mock()
    seeder.enabled = True

    assert decide(dest, CompletionState(dest, PHASES), False, creator, seeder) == ACTION_REUSE


def test_decide_resume_for_recorded_incomplete_environment(tmp_path: Path, mocker: MockerFixture) -> None:
    dest = tmp_path / "env"
    dest.mkdir()
    creator = _creator(mocker, dest, exe_path=None)

    action = decide(dest, CompletionState(dest, (PHASE_CREATED,)), False, creator)

    assert action == ACTION_RESUME


def test_decide_resume_for_pre_complete_mode_virtualenv(tmp_path: Path, mocker: MockerFixture) -> None:
    dest = tmp_path / "env"
    dest.mkdir()
    (dest / "pyvenv.cfg").write_text("home = x\nvirtualenv = 1.0\n", encoding="utf-8")
    creator = _creator(mocker, dest, exe_path=None)

    assert decide(dest, CompletionState(dest, ()), False, creator) == ACTION_RESUME


def test_decide_rejects_foreign_folder(tmp_path: Path, mocker: MockerFixture) -> None:
    dest = tmp_path / "env"
    dest.mkdir()
    (dest / "keep.txt").write_text("not an environment", encoding="utf-8")
    creator = _creator(mocker, dest, exe_path=None)

    with pytest.raises(ForeignEnvironmentError, match="not a virtual environment"):
        decide(dest, CompletionState(dest, ()), False, creator)


def test_decide_complete_state_with_missing_exe_is_resumed(tmp_path: Path, mocker: MockerFixture) -> None:
    dest = tmp_path / "env"
    dest.mkdir()
    (dest / "pyvenv.cfg").write_text("virtualenv = 1.0\n", encoding="utf-8")
    creator = _creator(mocker, dest, exe_path=None)

    # recorded as configured but the interpreter vanished: healing wins over reuse
    assert decide(dest, CompletionState(dest, PHASES), False, creator) == ACTION_RESUME


def test_looks_like_virtualenv_reads_pyvenv_cfg(tmp_path: Path) -> None:
    plain = tmp_path / "plain"
    plain.mkdir()
    env = tmp_path / "env"
    env.mkdir()
    (env / "pyvenv.cfg").write_text("virtualenv = 1.0\n", encoding="utf-8")

    assert looks_like_virtualenv(plain) is False
    assert looks_like_virtualenv(env) is True
    assert looks_like_virtualenv(tmp_path / "missing") is False


def test_creator_finished_checks_executable(tmp_path: Path, mocker: MockerFixture) -> None:
    creator = mocker.Mock()
    creator.exe = tmp_path / "python.exe"

    assert creator_finished(creator) is False

    creator.exe.touch()
    assert creator_finished(creator) is True


def test_seeder_finished_without_seeder_is_done(tmp_path: Path, mocker: MockerFixture) -> None:
    creator = mocker.Mock()
    creator.purelib = tmp_path / "purelib"

    assert seeder_finished(None, creator) is True


def test_seeder_finished_with_disabled_seeder_is_done(mocker: MockerFixture) -> None:
    seeder = mocker.Mock()
    seeder.enabled = False
    creator = mocker.Mock()

    assert seeder_finished(seeder, creator) is True


def test_seeder_finished_requires_populated_purelib(tmp_path: Path, mocker: MockerFixture) -> None:
    seeder = mocker.Mock()
    seeder.enabled = True
    creator = mocker.Mock()
    creator.purelib = tmp_path / "purelib"

    assert seeder_finished(seeder, creator) is False

    creator.purelib.mkdir()
    assert seeder_finished(seeder, creator) is False

    (creator.purelib / "pip").mkdir()
    assert seeder_finished(seeder, creator) is True
