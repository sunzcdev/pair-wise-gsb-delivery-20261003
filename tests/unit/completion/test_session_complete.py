from __future__ import annotations

from pathlib import Path
from typing import TYPE_CHECKING

import pytest

from virtualenv.completion import (
    PHASES,
    PHASE_CONFIGURED,
    PHASE_CREATED,
    PHASE_SEEDED,
    CompletionStore,
    ForeignEnvironmentError,
)
from virtualenv.run.session import Session

if TYPE_CHECKING:
    from pytest_mock import MockerFixture


def _session(
    mocker: MockerFixture, tmp_path: Path, *, complete: bool, seeder_enabled: bool = True
) -> tuple[Session, object]:
    creator = mocker.Mock()
    creator.dest = tmp_path / "env"
    creator.clear = False
    creator.exe = creator.dest / "Scripts" / "python.exe"
    creator.purelib = creator.dest / "Lib" / "site-packages"

    def _create() -> None:
        # the real creator materializes the interpreter on disk, so the mock marks the creator phase as observable
        creator.exe.parent.mkdir(parents=True, exist_ok=True)
        creator.exe.touch(exist_ok=True)

    creator.run = mocker.Mock(side_effect=_create)

    def _seed(_creator) -> None:
        # the real seeder populates purelib, which later reuse checks to know the seeding request is satisfied
        creator.purelib.mkdir(parents=True, exist_ok=True)
        (creator.purelib / "pip").mkdir(exist_ok=True)

    # _Debug renders creator.debug as JSON when another test left the log level at DEBUG
    creator.debug = {"creator": type(creator).__name__}

    seeder = mocker.Mock()
    seeder.enabled = seeder_enabled
    seeder.run = mocker.Mock(side_effect=_seed)

    activator = mocker.Mock()
    activator.generate = mocker.Mock(return_value=[creator.dest / "Scripts" / "activate"])

    app_data = mocker.Mock()
    interpreter = mocker.Mock()
    session = Session(
        verbosity=2,
        app_data=app_data,
        interpreter=interpreter,
        creator=creator,
        seeder=seeder,
        activators=[activator],
        complete=complete,
    )
    return session, activator


def test_complete_run_records_every_phase(tmp_path: Path, mocker: MockerFixture) -> None:
    session, activator = _session(mocker, tmp_path, complete=True)

    session.run()

    store = CompletionStore(session.creator.dest)
    assert store.load().phases == PHASES
    session.creator.run.assert_called_once_with()
    session.seeder.run.assert_called_once_with(session.creator)
    activator.generate.assert_called_once_with(session.creator)
    session.creator.pyenv_cfg.write.assert_called_once_with()


def test_complete_run_reuses_a_finished_environment(tmp_path: Path, mocker: MockerFixture) -> None:
    session, _ = _session(mocker, tmp_path, complete=True)
    session.run()
    creator_run = session.creator.run.call_count
    seeder_run = session.seeder.run.call_count
    # a reused environment has to look like a virtualenv (the real creator writes pyvenv.cfg at the end)
    (session.creator.dest / "pyvenv.cfg").write_text("virtualenv = 1.0\n", encoding="utf-8")

    second, _ = _session(mocker, tmp_path, complete=True)
    second.run()

    assert second.creator.run.call_count == 0
    assert second.seeder.run.call_count == 0
    assert second.creator.pyenv_cfg.write.call_count == 0
    # the first run is untouched
    assert session.creator.run.call_count == creator_run
    assert session.seeder.run.call_count == seeder_run


def test_complete_run_resumes_after_failed_seed(tmp_path: Path, mocker: MockerFixture) -> None:
    session, _ = _session(mocker, tmp_path, complete=True)
    failure = RuntimeError("seed exploded")
    session.seeder.run.side_effect = failure

    with pytest.raises(RuntimeError, match="seed exploded"):
        session.run()

    store = CompletionStore(session.creator.dest)
    assert store.load().phases == (PHASE_CREATED,)

    resumed, activator = _session(mocker, tmp_path, complete=True)
    resumed.run()

    # the creator is not rebuilt, the seeder and activators run again and the environment completes
    resumed.creator.run.assert_not_called()
    resumed.seeder.run.assert_called_once_with(resumed.creator)
    activator.generate.assert_called_once_with(resumed.creator)
    assert CompletionStore(resumed.creator.dest).load().complete is True


def test_complete_run_without_seeder_records_no_seed_phase(tmp_path: Path, mocker: MockerFixture) -> None:
    session, _ = _session(mocker, tmp_path, complete=True, seeder_enabled=False)

    session.run()

    phases = CompletionStore(session.creator.dest).load().phases
    assert PHASE_SEEDED not in phases
    # the environment still completes, with the seeded phase simply absent
    assert PHASE_CONFIGURED in phases
    session.seeder.run.assert_not_called()


def test_enabling_seeding_later_resumes_into_the_seed_phase(tmp_path: Path, mocker: MockerFixture) -> None:
    without_seed, _ = _session(mocker, tmp_path, complete=True, seeder_enabled=False)
    without_seed.run()
    assert PHASE_SEEDED not in CompletionStore(without_seed.creator.dest).load().phases

    with_seed, _ = _session(mocker, tmp_path, complete=True, seeder_enabled=True)
    with_seed.run()

    # the interpreter is kept and the previously skipped seeder now runs instead of rebuilding the environment
    with_seed.creator.run.assert_not_called()
    with_seed.seeder.run.assert_called_once_with(with_seed.creator)
    assert CompletionStore(with_seed.creator.dest).load().complete is True


def test_complete_run_rejects_foreign_destination(tmp_path: Path, mocker: MockerFixture) -> None:
    session, _ = _session(mocker, tmp_path, complete=True)
    session.creator.dest.mkdir()
    (session.creator.dest / "unrelated.txt").write_text("not ours", encoding="utf-8")

    with pytest.raises(ForeignEnvironmentError):
        session.run()


def test_complete_run_with_clear_rebuilds(tmp_path: Path, mocker: MockerFixture) -> None:
    session, _ = _session(mocker, tmp_path, complete=True)
    session.run()

    rebuilt, _ = _session(mocker, tmp_path, complete=True)
    rebuilt.creator.clear = True
    rebuilt.run()

    rebuilt.creator.run.assert_called_once_with()
    assert CompletionStore(rebuilt.creator.dest).load().phases == PHASES


def test_complete_flag_is_exposed(tmp_path: Path, mocker: MockerFixture) -> None:
    complete, _ = _session(mocker, tmp_path, complete=True)
    plain, _ = _session(mocker, tmp_path, complete=False)

    assert (complete.complete, plain.complete) == (True, False)


def test_plain_run_writes_no_completion_state(tmp_path: Path, mocker: MockerFixture) -> None:
    session, _ = _session(mocker, tmp_path, complete=False)

    session.run()

    assert not (session.creator.dest / ".virtualenv-complete").exists()
    session.creator.run.assert_called_once_with()
    session.creator.pyenv_cfg.write.assert_called_once_with()


def test_second_phase_failure_is_resumable(tmp_path: Path, mocker: MockerFixture) -> None:
    session, _ = _session(mocker, tmp_path, complete=True)
    session.creator.pyenv_cfg.write.side_effect = OSError("disk gone")

    with pytest.raises(OSError, match="disk gone"):
        session.run()

    assert PHASE_CONFIGURED not in CompletionStore(session.creator.dest).load().phases

    resumed, _ = _session(mocker, tmp_path, complete=True)
    resumed.run()

    assert CompletionStore(resumed.creator.dest).load().complete is True
