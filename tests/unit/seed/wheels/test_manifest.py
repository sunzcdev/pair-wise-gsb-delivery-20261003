from __future__ import annotations

import hashlib
import json
import zipfile
from pathlib import Path
from typing import TYPE_CHECKING, Final

import pytest

from virtualenv.app_data import AppDataDiskFolder
from virtualenv.seed.wheels.embed import get_embed_wheel
from virtualenv.seed.wheels.manifest import MANIFEST_SCHEMA_VERSION, SeedManifest, SeedManifestError

if TYPE_CHECKING:
    from collections.abc import Callable


def _digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _write_manifest(path: Path, packages: list[dict[str, object]], strict: bool = False) -> Path:
    payload = {"schema_version": MANIFEST_SCHEMA_VERSION, "strict": strict, "packages": packages}
    path.write_text(json.dumps(payload), encoding="utf-8")
    return path


@pytest.fixture
def make_wheel(tmp_path: Path) -> Callable[..., Path]:
    def build(
        name: str = "pip",
        version: str = "26.3",
        requires_python: str = ">=3.9",
        folder: Path | None = None,
        payload: bytes = b"wheel-payload",
        filename: str | None = None,
    ) -> Path:
        target = folder or tmp_path
        target.mkdir(parents=True, exist_ok=True)
        wheel_name = filename or f"{name}-{version}-py3-none-any.whl"
        path: Final[Path] = target / wheel_name
        with zipfile.ZipFile(path, "w") as archive:
            archive.writestr(
                f"{name}-{version}.dist-info/METADATA",
                f"Metadata-Version: 2.1\nName: {name}\nVersion: {version}\nRequires-Python: {requires_python}\n",
            )
            archive.writestr(f"{name}-{version}.dist-info/RECORD", "")
            archive.writestr("payload.bin", payload)
        return path

    return build


def test_load_rejects_missing_file(tmp_path: Path) -> None:
    with pytest.raises(SeedManifestError, match="seed manifest not found"):
        SeedManifest.load(tmp_path / "missing.json", strict=False)


def test_load_rejects_invalid_json(tmp_path: Path) -> None:
    path = tmp_path / "manifest.json"
    path.write_text("{not json", encoding="utf-8")
    with pytest.raises(SeedManifestError, match="not valid JSON"):
        SeedManifest.load(path, strict=False)


def test_load_rejects_wrong_schema(tmp_path: Path) -> None:
    path = tmp_path / "manifest.json"
    path.write_text(json.dumps({"schema_version": 99, "packages": []}), encoding="utf-8")
    with pytest.raises(SeedManifestError, match="unsupported schema_version"):
        SeedManifest.load(path, strict=False)


def test_load_rejects_empty_packages(tmp_path: Path) -> None:
    path = _write_manifest(tmp_path / "manifest.json", [])
    with pytest.raises(SeedManifestError, match="non-empty 'packages' list"):
        SeedManifest.load(path, strict=False)


def test_load_rejects_non_boolean_strict(tmp_path: Path) -> None:
    path = tmp_path / "manifest.json"
    payload = {
        "schema_version": MANIFEST_SCHEMA_VERSION,
        "strict": "yes",
        "packages": [{"distribution": "pip", "version": "26.3", "sha256": "a" * 64}],
    }
    path.write_text(json.dumps(payload), encoding="utf-8")
    with pytest.raises(SeedManifestError, match="non-boolean 'strict'"):
        SeedManifest.load(path, strict=False)


def test_load_rejects_bad_entry_shape(tmp_path: Path) -> None:
    path = _write_manifest(tmp_path / "manifest.json", ["not-an-object"])  # type: ignore[list-item]
    with pytest.raises(SeedManifestError, match="entry 0 must be an object"):
        SeedManifest.load(path, strict=False)


def test_load_rejects_bad_digest(tmp_path: Path, make_wheel: Callable[..., Path]) -> None:
    make_wheel()
    path = _write_manifest(
        tmp_path / "manifest.json",
        [{"distribution": "pip", "version": "26.3", "sha256": "abc"}],
    )
    with pytest.raises(SeedManifestError, match="64-hex-character 'sha256'"):
        SeedManifest.load(path, strict=False)


def test_load_rejects_bad_version(tmp_path: Path) -> None:
    entry = {"distribution": "pip", "version": "not-a-version", "sha256": "a" * 64}
    path = _write_manifest(tmp_path / "manifest.json", [entry])
    with pytest.raises(SeedManifestError, match="invalid version"):
        SeedManifest.load(path, strict=False)


def test_load_rejects_duplicate_distribution(tmp_path: Path) -> None:
    entry = {"distribution": "Pip", "version": "26.3", "sha256": "a" * 64}
    path = _write_manifest(tmp_path / "manifest.json", [entry, {**entry, "sha256": "b" * 64}])
    with pytest.raises(SeedManifestError, match="lists pip more than once"):
        SeedManifest.load(path, strict=False)


def test_load_normalizes_names_and_digest(tmp_path: Path) -> None:
    entry = {"distribution": "Pip.Name", "version": "v26.3", "sha256": ("A" * 64)}
    path = _write_manifest(tmp_path / "manifest.json", [entry])
    manifest = SeedManifest.load(path, strict=False)
    pinned = manifest.entry_for("pip-name")
    assert pinned is not None
    assert pinned.distribution == "pip-name"
    assert pinned.version == "26.3"
    assert pinned.sha256 == "a" * 64


def test_strict_flag_combines_file_and_cli(tmp_path: Path) -> None:
    entry = {"distribution": "pip", "version": "26.3", "sha256": "a" * 64}
    path = _write_manifest(tmp_path / "file-strict.json", [entry], strict=True)
    assert SeedManifest.load(path, strict=False).strict is True
    loose_path = _write_manifest(tmp_path / "loose.json", [entry], strict=False)
    assert SeedManifest.load(loose_path, strict=True).strict is True
    assert SeedManifest.load(loose_path, strict=False).strict is False


def test_select_returns_none_without_entry(
    tmp_path: Path, make_wheel: Callable[..., Path], for_py_version: str
) -> None:
    make_wheel()
    path = _write_manifest(
        tmp_path / "manifest.json",
        [{"distribution": "setuptools", "version": "84.2.1", "sha256": "a" * 64}],
    )
    manifest = SeedManifest.load(path, strict=False)
    app_data = AppDataDiskFolder(str(tmp_path / "app-data"))
    assert manifest.select("pip", for_py_version, [tmp_path], app_data) is None


def test_select_picks_manifest_wheel_ignoring_extra_version(
    tmp_path: Path, make_wheel: Callable[..., Path], for_py_version: str
) -> None:
    pinned = make_wheel(version="26.3", payload=b"pinned")
    # another version in the same folder must not change the manifest outcome
    make_wheel(version="26.4", payload=b"newer")
    entry = {
        "distribution": "pip",
        "version": "26.3",
        "sha256": _digest(pinned),
        "filename": pinned.name,
    }
    path = _write_manifest(tmp_path / "manifest.json", [entry])
    manifest = SeedManifest.load(path, strict=False)
    app_data = AppDataDiskFolder(str(tmp_path / "app-data"))

    selected = manifest.select("pip", for_py_version, [tmp_path], app_data)

    assert selected is not None
    assert selected.path == pinned


def test_select_rejects_version_match_with_wrong_digest_in_strict(
    tmp_path: Path, make_wheel: Callable[..., Path], for_py_version: str
) -> None:
    wheel = make_wheel(version="26.3", payload=b"actual-bytes")
    entry = {"distribution": "pip", "version": "26.3", "sha256": "a" * 64, "filename": wheel.name}
    path = _write_manifest(tmp_path / "manifest.json", [entry], strict=True)
    manifest = SeedManifest.load(path, strict=True)
    app_data = AppDataDiskFolder(str(tmp_path / "app-data"))

    with pytest.raises(SeedManifestError, match="could not find a wheel for pip==26.3"):
        manifest.select("pip", for_py_version, [tmp_path], app_data)


def test_select_warns_and_returns_none_in_loose_mode(
    tmp_path: Path, make_wheel: Callable[..., Path], caplog: pytest.LogCaptureFixture, for_py_version: str
) -> None:
    wheel = make_wheel(version="26.3")
    entry = {"distribution": "pip", "version": "26.3", "sha256": "a" * 64, "filename": wheel.name}
    path = _write_manifest(tmp_path / "manifest.json", [entry], strict=False)
    manifest = SeedManifest.load(path, strict=False)
    app_data = AppDataDiskFolder(str(tmp_path / "app-data"))

    assert manifest.select("pip", for_py_version, [tmp_path], app_data) is None
    assert "sha256 does not match the seed manifest" in caplog.text


def test_select_rejects_wheel_unsupported_by_target_python(
    tmp_path: Path, make_wheel: Callable[..., Path]
) -> None:
    wheel = make_wheel(version="26.3", requires_python=">=3.13")
    entry = {"distribution": "pip", "version": "26.3", "sha256": _digest(wheel), "filename": wheel.name}
    path = _write_manifest(tmp_path / "manifest.json", [entry], strict=True)
    manifest = SeedManifest.load(path, strict=True)
    app_data = AppDataDiskFolder(str(tmp_path / "app-data"))

    with pytest.raises(SeedManifestError, match="supporting Python 3.12"):
        manifest.select("pip", "3.12", [tmp_path], app_data)


def test_select_falls_back_to_bundled_wheel(
    tmp_path: Path, for_py_version: str
) -> None:
    bundled = get_embed_wheel("pip", for_py_version)
    assert bundled is not None
    entry = {
        "distribution": "pip",
        "version": bundled.version,
        "sha256": _digest(bundled.path),
        "filename": bundled.name,
    }
    path = _write_manifest(tmp_path / "manifest.json", [entry])
    manifest = SeedManifest.load(path, strict=True)
    app_data = AppDataDiskFolder(str(tmp_path / "app-data"))

    selected = manifest.select("pip", for_py_version, [tmp_path / "empty"], app_data)

    assert selected is not None
    assert selected.name == bundled.name


def test_bundled_fallback_requires_digest_match(
    tmp_path: Path, for_py_version: str
) -> None:
    bundled = get_embed_wheel("pip", for_py_version)
    assert bundled is not None
    entry = {"distribution": "pip", "version": bundled.version, "sha256": "a" * 64}
    path = _write_manifest(tmp_path / "manifest.json", [entry], strict=True)
    manifest = SeedManifest.load(path, strict=True)
    app_data = AppDataDiskFolder(str(tmp_path / "app-data"))

    with pytest.raises(SeedManifestError, match="could not find a wheel for pip"):
        manifest.select("pip", for_py_version, [tmp_path / "empty"], app_data)


def test_select_skips_non_wheel_and_other_distribution_files(
    tmp_path: Path, make_wheel: Callable[..., Path], for_py_version: str
) -> None:
    pinned = make_wheel(version="26.3")
    make_wheel(name="setuptools", version="26.3")
    (tmp_path / "notes.txt").write_text("not a wheel", encoding="utf-8")
    entry = {"distribution": "pip", "version": "26.3", "sha256": _digest(pinned)}
    path = _write_manifest(tmp_path / "manifest.json", [entry])
    manifest = SeedManifest.load(path, strict=False)
    app_data = AppDataDiskFolder(str(tmp_path / "app-data"))

    selected = manifest.select("pip", for_py_version, [tmp_path], app_data)

    assert selected is not None
    assert selected.path == pinned
