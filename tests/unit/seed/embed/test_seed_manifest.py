from __future__ import annotations

import hashlib
import json
import sys
import zipfile
from pathlib import Path
from typing import TYPE_CHECKING, Final

import pytest

from virtualenv.run import session_via_cli
from virtualenv.seed.wheels.manifest import MANIFEST_SCHEMA_VERSION, SeedManifestError

if TYPE_CHECKING:
    from collections.abc import Callable


def _digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


@pytest.fixture
def wheelhouse(tmp_path: Path) -> Path:
    folder = tmp_path / "wheelhouse"
    folder.mkdir()
    return folder


@pytest.fixture
def make_wheel(wheelhouse: Path) -> Callable[..., Path]:
    def build(version: str, payload: bytes, requires_python: str = ">=3.9") -> Path:
        path: Final[Path] = wheelhouse / f"pip-{version}-py3-none-any.whl"
        with zipfile.ZipFile(path, "w") as archive:
            archive.writestr(
                f"pip-{version}.dist-info/METADATA",
                f"Metadata-Version: 2.1\nName: pip\nVersion: {version}\nRequires-Python: {requires_python}\n",
            )
            archive.writestr(f"pip-{version}.dist-info/RECORD", "")
            archive.writestr("payload.bin", payload)
        return path

    return build


def _manifest(tmp_path: Path, packages: list[dict[str, object]], strict: bool = False) -> Path:
    path = tmp_path / "seed-manifest.json"
    path.write_text(
        json.dumps({"schema_version": MANIFEST_SCHEMA_VERSION, "strict": strict, "packages": packages}),
        encoding="utf-8",
    )
    return path


def test_seed_manifest_option_parsed(
    tmp_path: Path, wheelhouse: Path, make_wheel: Callable[..., Path]
) -> None:
    wheel = make_wheel("26.3", b"pinned")
    manifest = _manifest(
        tmp_path,
        [{"distribution": "pip", "version": "26.3", "sha256": _digest(wheel), "filename": wheel.name}],
    )

    session = session_via_cli(
        [
            str(tmp_path / "env"),
            "--no-seed",
            "--extra-search-dir",
            str(wheelhouse),
            "--seed-manifest",
            str(manifest),
            "--strict-seed-manifest",
        ]
    )

    assert session.seeder.seed_manifest is not None
    assert session.seeder.seed_manifest.strict is True
    assert session.seeder.seed_manifest.distributions == ("pip",)


def test_strict_manifest_rejects_unnamed_setuptools(
    tmp_path: Path, wheelhouse: Path, make_wheel: Callable[..., Path]
) -> None:
    wheel = make_wheel("26.3", b"pinned")
    manifest = _manifest(
        tmp_path,
        [{"distribution": "pip", "version": "26.3", "sha256": _digest(wheel), "filename": wheel.name}],
        strict=True,
    )

    with pytest.raises(SeedManifestError, match="does not name setuptools"):
        session_via_cli(
            [
                str(tmp_path / "env"),
                "--extra-search-dir",
                str(wheelhouse),
                "--seed-manifest",
                str(manifest),
                "--setuptools",
                "bundle",
            ]
        )


def test_loose_manifest_keeps_regular_selection_without_entry(tmp_path: Path, wheelhouse: Path) -> None:
    manifest = _manifest(
        tmp_path,
        [{"distribution": "setuptools", "version": "84.2.1", "sha256": "a" * 64}],
        strict=False,
    )

    session = session_via_cli(
        [
            str(tmp_path / "env"),
            "--no-seed",
            "--extra-search-dir",
            str(wheelhouse),
            "--seed-manifest",
            str(manifest),
        ]
    )

    # pip has no manifest entry, so the regular selection remains in charge
    assert session.seeder.seed_manifest is not None
    for_py_version = f"{sys.version_info.major}.{sys.version_info.minor}"
    assert session.seeder.select_seed_wheel("pip", "bundle", for_py_version) is None


def test_missing_manifest_fails_fast(tmp_path: Path) -> None:
    with pytest.raises(SeedManifestError, match="seed manifest not found"):
        session_via_cli(
            [
                str(tmp_path / "env"),
                "--no-seed",
                "--seed-manifest",
                str(tmp_path / "missing.json"),
            ]
        )


def test_manifest_via_env_var(
    tmp_path: Path, wheelhouse: Path, make_wheel: Callable[..., Path], monkeypatch: pytest.MonkeyPatch
) -> None:
    wheel = make_wheel("26.3", b"pinned")
    manifest = _manifest(
        tmp_path,
        [{"distribution": "pip", "version": "26.3", "sha256": _digest(wheel), "filename": wheel.name}],
    )
    monkeypatch.setenv("VIRTUALENV_SEED_MANIFEST", str(manifest))

    session = session_via_cli([str(tmp_path / "env"), "--no-seed"])

    assert session.seeder.seed_manifest is not None
    assert session.seeder.seed_manifest.path == manifest


def test_manifest_via_ini_file(
    tmp_path: Path, wheelhouse: Path, make_wheel: Callable[..., Path], monkeypatch: pytest.MonkeyPatch
) -> None:
    wheel = make_wheel("26.3", b"pinned")
    manifest = _manifest(
        tmp_path,
        [{"distribution": "pip", "version": "26.3", "sha256": _digest(wheel), "filename": wheel.name}],
    )
    config = tmp_path / "virtualenv.ini"
    config.write_text(f"[virtualenv]\nseed_manifest = {manifest}\n", encoding="utf-8")
    monkeypatch.setenv("VIRTUALENV_CONFIG_FILE", str(config))

    session = session_via_cli([str(tmp_path / "env"), "--no-seed"])

    assert session.seeder.seed_manifest is not None
    assert session.seeder.seed_manifest.path == manifest


def test_cli_strict_flag_overrides_loose_file(
    tmp_path: Path, wheelhouse: Path, make_wheel: Callable[..., Path]
) -> None:
    wheel = make_wheel("26.3", b"pinned")
    manifest = _manifest(
        tmp_path,
        [{"distribution": "pip", "version": "26.3", "sha256": _digest(wheel), "filename": wheel.name}],
        strict=False,
    )

    session = session_via_cli(
        [
            str(tmp_path / "env"),
            "--no-seed",
            "--seed-manifest",
            str(manifest),
            "--strict-seed-manifest",
        ]
    )

    assert session.seeder.seed_manifest is not None
    assert session.seeder.seed_manifest.strict is True


@pytest.mark.parametrize("seeder", [pytest.param("app-data", id="app-data"), pytest.param("pip", id="pip")])
def test_strict_manifest_failure_raises_manifest_error_at_run(
    tmp_path: Path, wheelhouse: Path, make_wheel: Callable[..., Path], seeder: str
) -> None:
    from virtualenv.seed.wheels.manifest import SeedManifestError as ExpectedError

    make_wheel("26.3", b"version-matching-bytes")
    manifest = _manifest(
        tmp_path,
        [{"distribution": "pip", "version": "26.3", "sha256": "a" * 64}],
        strict=True,
    )

    session = session_via_cli(
        [
            str(tmp_path / "env"),
            "--seeder",
            seeder,
            "--no-setuptools",
            "--extra-search-dir",
            str(wheelhouse),
            "--seed-manifest",
            str(manifest),
        ]
    )

    if seeder == "app-data":
        with pytest.raises(ExpectedError, match="could not find a wheel for pip==26.3"):
            with session.seeder._get_seed_wheels(session.creator):  # noqa: SLF001
                pass
    else:
        with pytest.raises(ExpectedError, match="could not find a wheel for pip==26.3"):
            with session.seeder.get_pip_install_cmd(session.creator.exe, "3.12"):
                pass
