from __future__ import annotations

import os
import tempfile
from pathlib import Path
from typing import TYPE_CHECKING, Final

import pytest
from python_discovery import PythonInfo

from virtualenv.app_data import AppDataDiskFolder, ReadOnlyAppData, ReadOnlyFallbackAppData, make_app_data
from virtualenv.run import session_via_cli
from virtualenv.seed.wheels.embed import BUNDLE_SUPPORT
from virtualenv.util.path import safe_delete

if TYPE_CHECKING:
    from pytest_mock import MockerFixture

    from virtualenv.run.session import Session

_VERSION: Final[str] = PythonInfo.current_system().version_release_str
_PIP_STEM: Final[str] = Path(BUNDLE_SUPPORT[_VERSION]["pip"]).stem
_SETUPTOOLS_STEM: Final[str] = Path(BUNDLE_SUPPORT[_VERSION]["setuptools"]).stem
_INSTALLER: Final[str] = "CopyPipInstall"


def _image_root(cache: Path) -> Path:
    return cache / "wheel" / _VERSION / "image" / "1" / _INSTALLER


@pytest.fixture
def workspace() -> Path:
    """A short-lived workspace under the system temp root, kept short so Windows MAX_PATH does not truncate images."""
    root = Path(tempfile.mkdtemp(prefix="rofb-"))
    try:
        yield root
    finally:
        safe_delete(root)


@pytest.fixture
def warm_cache(workspace: Path) -> Path:
    """A writable app data folder holding populated pip and setuptools wheel images."""
    cache = workspace / "cache"
    session = session_via_cli(
        [
            str(workspace / "warm-env"),
            "--app-data",
            str(cache),
            "--no-periodic-update",
            "--setuptools",
            "bundle",
            "--activators=",
        ],
        setup_logging=False,
    )
    with session:
        session.run()
    assert {p.name for p in _image_root(cache).iterdir()} == {_PIP_STEM, _SETUPTOOLS_STEM}
    return cache


def _fallback_args(workspace: Path, cache: Path) -> list[str]:
    return [
        str(workspace / "env"),
        "--app-data",
        str(cache),
        "--read-only-app-data",
        "--read-only-app-data-fallback",
        "--no-periodic-update",
        "--setuptools",
        "bundle",
        "--activators=",
    ]


def test_make_app_data_selects_the_fallback_store(workspace: Path) -> None:
    cache = workspace / "cache"
    cache.mkdir()
    plain = make_app_data(str(cache), read_only=True, fallback_to_temp=False, env={})
    fallback = make_app_data(str(cache), read_only=True, fallback_to_temp=True, env={})
    try:
        assert (type(plain), type(fallback)) == (ReadOnlyAppData, ReadOnlyFallbackAppData)
    finally:
        plain.close()
        fallback.close()


def test_fallback_flag_ignored_in_writable_mode(workspace: Path) -> None:
    store = make_app_data(str(workspace / "cache"), read_only=False, fallback_to_temp=True, env={})
    try:
        assert type(store) is AppDataDiskFolder
    finally:
        store.close()


def test_fully_populated_cache_creates_no_temp_folder(warm_cache: Path, workspace: Path) -> None:
    session = session_via_cli(_fallback_args(workspace, warm_cache), setup_logging=False)
    with session:
        session.run()
    app_data = session.creator.app_data
    assert type(app_data) is ReadOnlyFallbackAppData
    assert app_data._fallback is None


def test_missing_image_is_built_in_temp_and_existing_image_reused(warm_cache: Path, workspace: Path) -> None:
    # the read-only cache now hits pip but misses setuptools
    safe_delete(_image_root(warm_cache) / _SETUPTOOLS_STEM)
    assert {p.name for p in _image_root(warm_cache).iterdir()} == {_PIP_STEM}

    session = session_via_cli(_fallback_args(workspace, warm_cache), setup_logging=False)
    with session:
        session.run()
        app_data = session.creator.app_data
        assert type(app_data) is ReadOnlyFallbackAppData
        assert app_data._fallback is not None
        temp_root = app_data._fallback.lock.path
        temp_images = temp_root / "wheel" / _VERSION / "image" / "1" / _INSTALLER

        # both images physically land: pip reused in the read-only cache, setuptools built in the temp folder
        assert (_image_root(warm_cache) / _PIP_STEM).is_dir()
        assert (temp_images / _SETUPTOOLS_STEM / "setuptools" / "__init__.py").is_file()
        assert not (temp_images / _PIP_STEM).exists()

        # both packages are installed into the environment by copying
        purelib = session.creator.purelib
        assert (purelib / "pip" / "__init__.py").is_file()
        assert (purelib / "setuptools" / "__init__.py").is_file()
        assert not (purelib / "pip").is_symlink()
        assert not (purelib / "setuptools").is_symlink()

    # closing the session removes the temporary folder, while the read-only cache is left exactly as it was
    assert not temp_root.exists()
    assert warm_cache.exists()
    assert {p.name for p in _image_root(warm_cache).iterdir()} == {_PIP_STEM}


def test_environment_never_references_the_temp_folder(warm_cache: Path, workspace: Path) -> None:
    safe_delete(_image_root(warm_cache) / _SETUPTOOLS_STEM)
    session: Session = session_via_cli(_fallback_args(workspace, warm_cache), setup_logging=False)
    with session:
        session.run()
        temp_root = str(session.creator.app_data._fallback.lock.path)

    # every symlink (if the platform made any) must stay clear of the removed temporary folder
    offenders = [
        entry
        for base in (session.creator.purelib, session.creator.script_dir)
        for entry in base.rglob("*")
        if entry.is_symlink() and temp_root in os.readlink(entry)
    ]
    assert offenders == []
    # the installed files keep working from their own copies after the temp folder is gone
    assert (session.creator.purelib / "setuptools" / "__init__.py").is_file()


def test_symlink_app_data_still_copies_under_transient_fallback(warm_cache: Path, workspace: Path) -> None:
    safe_delete(_image_root(warm_cache) / _SETUPTOOLS_STEM)
    args = [*_fallback_args(workspace, warm_cache), "--symlink-app-data"]
    session = session_via_cli(args, setup_logging=False)
    with session:
        session.run()
    # the transient fallback forces the copy installer regardless of the symlink request, so nothing dangles
    purelib = session.creator.purelib
    assert not (purelib / "pip").is_symlink()
    assert not (purelib / "setuptools").is_symlink()
    assert (purelib / "pip" / "__init__.py").is_file()
    assert (purelib / "setuptools" / "__init__.py").is_file()


def test_without_fallback_flag_missing_image_fails(
    warm_cache: Path, workspace: Path, mocker: MockerFixture
) -> None:
    safe_delete(_image_root(warm_cache) / _SETUPTOOLS_STEM)
    # Windows only applies the read-only bit to files, so force the staging write to fail; on POSIX a chmod'ed
    # read-only folder raises the same error
    mocker.patch(
        "virtualenv.seed.embed.via_app_data.pip_install.base.PipInstall.build_image",
        autospec=True,
        side_effect=PermissionError("read-only app data"),
    )
    args = [
        str(workspace / "env"),
        "--app-data",
        str(warm_cache),
        "--read-only-app-data",
        "--no-periodic-update",
        "--setuptools",
        "bundle",
        "--activators=",
    ]
    with pytest.raises(RuntimeError, match="failed to build image setuptools"):
        session = session_via_cli(args, setup_logging=False)
        with session:
            session.run()


def test_env_var_enables_the_fallback(
    warm_cache: Path, workspace: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    safe_delete(_image_root(warm_cache) / _SETUPTOOLS_STEM)
    monkeypatch.setenv("VIRTUALENV_READ_ONLY_APP_DATA_FALLBACK", "true")
    args = [
        str(workspace / "env"),
        "--app-data",
        str(warm_cache),
        "--read-only-app-data",
        "--no-periodic-update",
        "--setuptools",
        "bundle",
        "--activators=",
    ]
    session = session_via_cli(args, setup_logging=False)
    with session:
        session.run()
    assert type(session.creator.app_data) is ReadOnlyFallbackAppData
    assert (session.creator.purelib / "setuptools" / "__init__.py").is_file()


def test_reset_and_py_info_clear_are_refused(workspace: Path) -> None:
    cache = workspace / "cache"
    cache.mkdir()
    store = ReadOnlyFallbackAppData(str(cache))
    try:
        with pytest.raises(RuntimeError, match="^read-only app data does not support reset$"):
            store.reset()
        with pytest.raises(NotImplementedError):
            store.py_info_clear()
    finally:
        store.close()


def test_close_is_idempotent_and_lazy(workspace: Path) -> None:
    cache = workspace / "cache"
    cache.mkdir()
    store = ReadOnlyFallbackAppData(str(cache))
    try:
        # nothing missing has been asked for, so no temporary folder exists yet
        assert store._fallback is None
        image = store.wheel_image(_VERSION, f"{_INSTALLER}/{_PIP_STEM}")
        assert store._fallback is not None
        assert image == store._fallback.wheel_image(_VERSION, f"{_INSTALLER}/{_PIP_STEM}")
        temp_root = store._fallback.lock.path
        store.close()
        assert not temp_root.exists()
        store.close()  # must not raise on a second close
    finally:
        store.close()


def test_populated_read_only_image_is_served_without_touching_temp(warm_cache: Path) -> None:
    store = ReadOnlyFallbackAppData(str(warm_cache))
    try:
        served = store.wheel_image(_VERSION, f"{_INSTALLER}/{_PIP_STEM}")
        assert served == _image_root(warm_cache) / _PIP_STEM
        assert store._fallback is None
    finally:
        store.close()


def test_py_info_miss_writes_to_temp_without_touching_read_only(workspace: Path) -> None:
    cache = workspace / "cache"
    cache.mkdir()
    interpreter = workspace / "python"
    interpreter.touch()
    store = ReadOnlyFallbackAppData(str(cache))
    try:
        py_info = store.py_info(interpreter)
        assert not py_info.exists()
        # discovery writes the probe result when the read-only folder has no cache entry
        with py_info.locked():
            py_info.write({"content": {"version": _VERSION}})
        assert store._fallback is not None
        assert py_info.exists() and py_info.read() == {"content": {"version": _VERSION}}
        # the read-only folder received no python info, everything went to the temporary store
        assert not (cache / "py_info").exists()
        temp_root = store._fallback.lock.path
        store.close()
        assert not temp_root.exists()
    finally:
        store.close()
