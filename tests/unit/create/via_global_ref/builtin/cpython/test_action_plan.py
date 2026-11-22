from __future__ import annotations

import json
import os
from pathlib import Path
from typing import TYPE_CHECKING

import pytest
from python_discovery import PythonInfo

from virtualenv.config.cli.parser import VirtualEnvOptions
from virtualenv.create.creator import Creator
from virtualenv.create.plan import ActionKind, ActionMethod
from virtualenv.create.via_global_ref.builtin.cpython.cpython3 import CPython3Windows
from virtualenv.create.via_global_ref.builtin.ref import PathRefToDest, RefMust
from virtualenv.create.via_global_ref.builtin.via_global_self_do import (
    BuiltinViaGlobalRefMeta,
    ViaGlobalRefVirtualenvBuiltin,
)

if TYPE_CHECKING:
    from collections.abc import Generator

    from pytest_mock import MockerFixture

    from virtualenv.create.via_global_ref.builtin.ref import PathRef

pytestmark = pytest.mark.skipif(os.name != "nt", reason="the Windows layout is only built on Windows")

_FIXTURE = Path(__file__).parent / "cpython3_win_embed.json"


def _retarget(value, prefix):
    if isinstance(value, str):
        return value.replace("c:\\path\\to\\python", str(prefix))
    if isinstance(value, list):
        return [_retarget(i, prefix) for i in value]
    if isinstance(value, dict):
        return {k: _retarget(v, prefix) for k, v in value.items()}
    return value


def _make_layout(root: Path, names: tuple[str, ...], dll_names: tuple[str, ...]) -> PythonInfo:
    prefix = root / "python"
    (prefix / "DLLs").mkdir(parents=True)
    for name in names:
        (prefix / name).write_bytes(b"")
    for name in dll_names:
        (prefix / "DLLs" / name).write_bytes(b"")
    fixture = json.loads(_FIXTURE.read_text(encoding="utf-8"))
    return PythonInfo.from_dict(_retarget(fixture, prefix))


def _options(tmp_path: Path, *, symlinks: bool, meta: object) -> VirtualEnvOptions:
    options = VirtualEnvOptions(
        dest=str(tmp_path / "env"),
        clear=False,
        no_vcs_ignore=False,
        venv_redirect=None,
        app_data=None,
        env=os.environ,
        system_site=False,
        meta=meta,
    )
    options.set_src("symlinks", symlinks, "cli" if symlinks else "default")
    options.set_src("copies", not symlinks, "cli" if not symlinks else "default")
    return options


def _cpython_plan(tmp_path: Path, interpreter: PythonInfo) -> object:
    meta = CPython3Windows.can_create(interpreter)
    return CPython3Windows(_options(tmp_path, symlinks=False, meta=meta), interpreter).action_plan()


@pytest.fixture
def interpreter(tmp_path: Path) -> PythonInfo:
    return _make_layout(tmp_path, ("python.exe", "pythonw.exe"), ())


def test_standard_layout_plan_uses_the_creation_names(tmp_path: Path) -> None:
    interpreter = _make_layout(
        tmp_path, ("python.exe", "pythonw.exe", "libcrypto-1_1.dll", "python310.zip"), ("_ssl.pyd",)
    )
    plan = _cpython_plan(tmp_path, interpreter)

    dests = {i.name for action in plan.actions for i in action.destinations}
    assert {"python.exe", "python3.exe", "python3", "pythonw.exe", "pythonw3.exe"} <= dests
    assert (plan.creator, plan.symlinks) == ("CPython3Windows", False)
    assert "copies requested" in plan.mode_reason

    by_name = {Path(action.src).name: action for action in plan.actions}
    assert (by_name["python.exe"].kind, by_name["python.exe"].method) == (ActionKind.EXECUTABLE, ActionMethod.COPY)
    assert by_name["libcrypto-1_1.dll"].kind == ActionKind.FILE
    assert by_name["_ssl.pyd"].dest.parent.name == "Scripts"
    assert by_name["python310.zip"].dest.parent.name == "Scripts"


def test_sparse_layout_plans_only_the_files_it_has(tmp_path: Path) -> None:
    standard = _make_layout(
        tmp_path / "full", ("python.exe", "pythonw.exe", "libcrypto-1_1.dll", "python310.zip"), ("_ssl.pyd",)
    )
    sparse = _make_layout(tmp_path / "sparse", ("python.exe", "pythonw.exe"), ())
    standard_plan = _cpython_plan(tmp_path, standard)
    sparse_plan = _cpython_plan(tmp_path, sparse)

    sparse_sources = {Path(action.src).name for action in sparse_plan.actions}
    assert sparse_sources == {"python.exe", "pythonw.exe"}
    assert all(action.exists is True for action in sparse_plan.actions)
    assert {"libcrypto-1_1.dll", "_ssl.pyd", "python310.zip"}.isdisjoint(sparse_sources)
    assert len(sparse_plan.actions) < len(standard_plan.actions)


class _LinkOrCopyCreator(ViaGlobalRefVirtualenvBuiltin):
    ref: PathRef

    @classmethod
    def can_create(cls, interpreter: PythonInfo):
        meta = BuiltinViaGlobalRefMeta()
        meta.sources = [cls.ref]
        return meta

    @classmethod
    def sources(cls, interpreter: PythonInfo) -> Generator[PathRef]:
        yield cls.ref


def _single_ref(ref: PathRef) -> type[_LinkOrCopyCreator]:
    return type("_Single", (_LinkOrCopyCreator,), {"ref": ref})


@pytest.fixture
def a_ref(tmp_path: Path) -> PathRef:
    src = tmp_path / "host.dll"
    src.write_bytes(b"")
    return PathRefToDest(src, dest=lambda creator, s: creator.bin_dir / s.name, must=RefMust.NA)


def _single_plan(tmp_path: Path, a_ref: PathRef, interpreter: PythonInfo, *, symlinks: bool, meta) -> object:
    creator = _single_ref(a_ref)
    return creator(_options(tmp_path, symlinks=symlinks, meta=meta), interpreter).action_plan()


def test_symlink_refused_by_the_host_falls_back_to_copy(
    tmp_path: Path, a_ref: PathRef, interpreter: PythonInfo, mocker: MockerFixture
) -> None:
    mocker.patch.object(type(a_ref), "can_symlink", new_callable=mocker.PropertyMock, return_value=False)
    mocker.patch.object(type(a_ref), "can_copy", new_callable=mocker.PropertyMock, return_value=True)
    meta = BuiltinViaGlobalRefMeta()
    meta.sources = [a_ref]
    meta.symlink_error = "the filesystem does not supports symlink"
    plan = _single_plan(tmp_path, a_ref, interpreter, symlinks=True, meta=meta)

    action = plan.actions[0]
    assert action.method == ActionMethod.SYMLINK
    assert action.fallback is not None
    assert (action.fallback.method, action.fallback.available) == (ActionMethod.COPY, True)
    assert "symlink" in action.fallback.reason


def test_read_only_host_leaves_no_working_fallback(
    tmp_path: Path, a_ref: PathRef, interpreter: PythonInfo, mocker: MockerFixture
) -> None:
    mocker.patch.object(type(a_ref), "can_symlink", new_callable=mocker.PropertyMock, return_value=False)
    mocker.patch.object(type(a_ref), "can_copy", new_callable=mocker.PropertyMock, return_value=False)
    meta = BuiltinViaGlobalRefMeta()
    meta.sources = [a_ref]
    plan = _single_plan(tmp_path, a_ref, interpreter, symlinks=False, meta=meta)

    action = plan.actions[0]
    assert action.method == ActionMethod.COPY
    assert action.fallback is not None
    assert (action.fallback.method, action.fallback.available) == (None, False)


def test_creator_without_a_layout_reports_an_unknown_action(tmp_path: Path, interpreter: PythonInfo) -> None:
    class Opaque(Creator):
        def create(self) -> None:
            pass

    plan = Opaque(_options(tmp_path, symlinks=False, meta=BuiltinViaGlobalRefMeta()), interpreter).action_plan()
    assert len(plan.actions) == 1
    assert (plan.actions[0].kind, plan.actions[0].method) == (ActionKind.UNKNOWN, ActionMethod.UNKNOWN)
    assert "does not describe" in plan.actions[0].reason


@pytest.mark.parametrize(
    ("names", "dll_names"),
    [
        pytest.param(("python.exe", "pythonw.exe", "libcrypto-1_1.dll", "python310.zip"), ("_ssl.pyd",), id="standard"),
        pytest.param(("python.exe", "pythonw.exe"), (), id="sparse"),
    ],
)
def test_plan_matches_the_files_creation_writes(
    tmp_path: Path, names: tuple[str, ...], dll_names: tuple[str, ...]
) -> None:
    interpreter = _make_layout(tmp_path, names, dll_names)
    meta = CPython3Windows.can_create(interpreter)
    creator = CPython3Windows(_options(tmp_path, symlinks=False, meta=meta), interpreter)
    planned = {i for action in creator.action_plan().actions for i in action.destinations}

    creator.create()

    for dest in planned:
        assert dest.exists(), f"planned {dest} was not created"

    script_dir = tmp_path / "env" / "Scripts"
    written_by_refs = {
        i
        for i in script_dir.iterdir()
        if not i.name.startswith(("activate", "deactivate")) and i.name not in {"pydoc.bat"}
    }
    assert written_by_refs <= planned


def test_show_plan_reports_without_creating(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    from virtualenv.run import cli_run

    dest = tmp_path / "env"
    cli_run(
        [
            "--show-plan",
            "--no-seed",
            "--no-periodic-update",
            "--app-data",
            str(tmp_path / "app-data"),
            str(dest),
        ]
    )

    out, _ = capsys.readouterr()
    report = json.loads(out)
    assert (report["creator"], dest.exists()) == ("CPython3Windows", False)
    assert report["actions"]
    assert {"src", "destinations", "kind", "method", "reason", "fallback"} <= set(report["actions"][0])


def test_delegated_creator_reports_mode_and_an_unknown_layout(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    from virtualenv.run import cli_run

    dest = tmp_path / "env"
    cli_run(
        [
            "--show-plan",
            "--creator",
            "venv",
            "--no-seed",
            "--no-periodic-update",
            "--app-data",
            str(tmp_path / "app-data"),
            str(dest),
        ]
    )

    report = json.loads(capsys.readouterr().out)
    assert (report["creator"], dest.exists()) == ("Venv", False)
    assert report["symlinks"] is not None
    assert [(i["kind"], i["method"]) for i in report["actions"]] == [("unknown", "unknown")]
