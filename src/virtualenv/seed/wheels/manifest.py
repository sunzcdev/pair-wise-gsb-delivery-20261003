"""Pinned seed package manifests for fixed runners.

A manifest names the exact seed wheels a runner accepts, so adding another wheel version to an
``--extra-search-dir`` cannot change what gets seeded. Each entry pins the distribution, its
version, a SHA-256 digest and, when known, the wheel file name. Selection only accepts a wheel
that matches every pin and that supports the target interpreter.
"""

from __future__ import annotations

__lazy_modules__ = [
    "hashlib",
    "json",
    "re",
    "packaging.utils",
    "packaging.version",
]

import hashlib
import json
import logging
import re
from pathlib import Path
from typing import TYPE_CHECKING, Final, NamedTuple

from packaging.utils import canonicalize_name
from packaging.version import InvalidVersion
from packaging.version import Version as PackagingVersion

from .embed import get_embed_wheel
from .util import Wheel

if TYPE_CHECKING:
    from virtualenv.app_data.base import AppData

LOGGER: Final[logging.Logger] = logging.getLogger(__name__)  # simplint: ignore[module-variable-prefix]  # existing name

#: Manifest format version this module writes and reads.
MANIFEST_SCHEMA_VERSION: Final[int] = 1
#: Top level key holding the format version.
_SCHEMA_KEY: Final[str] = "schema_version"
#: Top level key holding the strict flag.
_STRICT_KEY: Final[str] = "strict"
#: Top level key holding the seed package entries.
_PACKAGES_KEY: Final[str] = "packages"
#: Accepted SHA-256 text: exactly 64 hexadecimal characters, compared case-insensitively.
_SHA256_RE_PATTERN: Final[str] = r"[0-9a-fA-F]{64}"


class SeedManifestError(RuntimeError):
    """A manifest is malformed or cannot be satisfied by the available wheels."""


class SeedManifestEntry(NamedTuple):
    """One pinned seed package of a manifest.

    :param distribution: the canonical distribution name, for example ``pip``
    :param version: the exact PEP 440 version the wheel must report
    :param sha256: the lowercase SHA-256 digest the wheel file must have
    :param filename: the exact wheel file name, or ``None`` to accept any file name matching the rest

    """

    distribution: str
    version: str
    sha256: str
    filename: str | None


class SeedManifest:
    """The seed packages a fixed runner accepts, loaded from a JSON manifest file.

    :param entries: the pinned seed packages, keyed by canonical distribution name
    :param strict: ``True`` to reject anything but manifest entries and the built-in bundled fallback
    :param path: the file the manifest was loaded from, for messages

    """

    def __init__(self, entries: dict[str, SeedManifestEntry], strict: bool, path: Path) -> None:
        self._entries = entries
        self.strict = strict
        self.path = path

    @classmethod
    def load(cls, path: Path, strict: bool) -> SeedManifest:
        """Load and validate a seed manifest from a JSON file.

        :param path: the manifest file
        :param strict: ``True`` to select only manifest entries and the built-in bundled fallback

        :returns: the parsed manifest

        :raises SeedManifestError: when the file is missing, not valid JSON, or has an invalid shape or entry

        """
        try:
            raw = json.loads(path.read_text(encoding="utf-8"))
        except FileNotFoundError as exception:
            msg = f"seed manifest not found: {path}"
            raise SeedManifestError(msg) from exception
        except json.JSONDecodeError as exception:
            msg = f"seed manifest {path} is not valid JSON: {exception.msg} at line {exception.lineno} column {exception.colno}"
            raise SeedManifestError(msg) from exception
        if not isinstance(raw, dict):
            msg = f"seed manifest {path} must be a JSON object, got {type(raw).__name__}"
            raise SeedManifestError(msg)

        schema = raw.get(_SCHEMA_KEY, MANIFEST_SCHEMA_VERSION)
        if schema != MANIFEST_SCHEMA_VERSION:
            msg = f"seed manifest {path} has unsupported {_SCHEMA_KEY} {schema!r}; expected {MANIFEST_SCHEMA_VERSION}"
            raise SeedManifestError(msg)

        file_strict = raw.get(_STRICT_KEY, False)
        if not isinstance(file_strict, bool):
            msg = f"seed manifest {path} has a non-boolean {_STRICT_KEY!r}: {file_strict!r}"
            raise SeedManifestError(msg)

        packages = raw.get(_PACKAGES_KEY)
        if not isinstance(packages, list) or not packages:
            msg = f"seed manifest {path} must contain a non-empty {_PACKAGES_KEY!r} list"
            raise SeedManifestError(msg)

        entries: dict[str, SeedManifestEntry] = {}
        for index, package in enumerate(packages):
            entry = _parse_entry(path, package, index)
            if entry.distribution in entries:
                msg = f"seed manifest {path} lists {entry.distribution} more than once"
                raise SeedManifestError(msg)
            entries[entry.distribution] = entry
        return cls(entries, strict or file_strict, path)

    @property
    def distributions(self) -> tuple[str, ...]:
        """The canonical names of the distributions the manifest pins."""
        return tuple(self._entries)

    def entry_for(self, distribution: str) -> SeedManifestEntry | None:
        """Return the pin for a distribution, or ``None`` when the manifest does not name it.

        :param distribution: the distribution name, in any PEP 503 spelling

        """
        return self._entries.get(canonicalize_name(distribution))

    def select(
        self,
        distribution: str,
        for_py_version: str,
        search_dirs: list[Path],
        app_data: AppData,
    ) -> Wheel | None:
        """Select the wheel a manifest entry pins, from the search dirs first and then the bundled fallback.

        :param distribution: the distribution to select a wheel for
        :param for_py_version: the Python version the wheel must support, as ``major.minor``
        :param search_dirs: the extra folders searched before the bundled fallback
        :param app_data: the application data the bundled wheel is extracted into

        :returns: the verified wheel, or ``None`` when no entry pins the distribution

        :raises SeedManifestError: in strict mode when the pinned wheel cannot be found or verified

        """
        entry = self.entry_for(distribution)
        if entry is None:
            return None
        for folder in search_dirs:
            wheel = _matching_wheel(folder, entry, for_py_version)
            if wheel is not None:
                LOGGER.debug("seed manifest %s selects %s for %s", self.path, wheel.name, distribution)
                return wheel
        bundled = _bundled_wheel(entry, for_py_version, app_data)
        if bundled is not None:
            LOGGER.debug("seed manifest %s falls back to bundled %s for %s", self.path, bundled.name, distribution)
            return bundled
        if self.strict:
            msg = (
                f"strict seed manifest {self.path} could not find a wheel for {entry.distribution}=={entry.version} "
                f"supporting Python {for_py_version} in {len(search_dirs)} search dir(s) or the bundled fallback"
            )
            raise SeedManifestError(msg)
        LOGGER.warning("seed manifest entry for %s unmatched, falling back to the regular seed selection", distribution)
        return None


def _parse_entry(path: Path, package: object, index: int) -> SeedManifestEntry:
    where = f"seed manifest {path} entry {index}"
    if not isinstance(package, dict):
        msg = f"{where} must be an object, got {type(package).__name__}"
        raise SeedManifestError(msg)
    name = package.get("distribution")
    if not isinstance(name, str) or not name:
        msg = f"{where} has no string 'distribution'"
        raise SeedManifestError(msg)
    distribution = canonicalize_name(name)
    version = package.get("version")
    if not isinstance(version, str) or not version:
        msg = f"{where} for {distribution} has no string 'version'"
        raise SeedManifestError(msg)
    try:
        normalized_version = str(PackagingVersion(version))
    except InvalidVersion as exception:
        msg = f"{where} for {distribution} has invalid version {version!r}"
        raise SeedManifestError(msg) from exception
    digest = package.get("sha256")
    if not isinstance(digest, str) or not _is_sha256(digest):
        msg = f"{where} for {distribution} has no 64-hex-character 'sha256'"
        raise SeedManifestError(msg)
    filename = package.get("filename")
    if filename is not None and (not isinstance(filename, str) or not filename):
        msg = f"{where} for {distribution} has an empty 'filename'"
        raise SeedManifestError(msg)
    if filename is not None and not filename.endswith(".whl"):
        msg = f"{where} for {distribution} filename {filename!r} must end with .whl"
        raise SeedManifestError(msg)
    return SeedManifestEntry(distribution, normalized_version, digest.lower(), filename)


def _is_sha256(value: str) -> bool:
    return re.fullmatch(_SHA256_RE_PATTERN, value) is not None


def _matching_wheel(folder: Path, entry: SeedManifestEntry, for_py_version: str) -> Wheel | None:
    if not folder.exists():
        return None
    candidates: list[Wheel] = []
    for filename in folder.iterdir():
        wheel = Wheel.from_path(filename)
        if wheel is None:
            continue
        if canonicalize_name(wheel.distribution) != entry.distribution or not wheel.has_version(entry.version):
            continue
        if entry.filename is not None and wheel.name != entry.filename:
            continue
        if not wheel.support_py(for_py_version):
            continue
        candidates.append(wheel)
    for wheel in candidates:
        if _digest_matches(wheel, entry.sha256):
            return wheel
        LOGGER.warning(
            "reject %s for %s==%s: sha256 does not match the seed manifest",
            wheel.name,
            entry.distribution,
            entry.version,
        )
    return None


def _digest_matches(wheel: Wheel, expected: str) -> bool:
    return hashlib.sha256(wheel.path.read_bytes()).hexdigest() == expected


def _bundled_wheel(entry: SeedManifestEntry, for_py_version: str, app_data: AppData) -> Wheel | None:
    bundled = get_embed_wheel(entry.distribution, for_py_version)
    if bundled is None or not bundled.has_version(entry.version):
        return None
    if entry.filename is not None and bundled.name != entry.filename:
        return None
    if not _digest_matches(bundled, entry.sha256):
        LOGGER.warning(
            "bundled %s does not match the seed manifest digest for %s==%s",
            bundled.name,
            entry.distribution,
            entry.version,
        )
        return None
    if not bundled.support_py(for_py_version):
        return None
    # callers pass a callable for the folder, despite the Path | None annotation
    with app_data.ensure_extracted(bundled.path, lambda: app_data.house) as wheel_path:  # ty: ignore[invalid-argument-type]
        return Wheel(wheel_path)


__all__ = [
    "MANIFEST_SCHEMA_VERSION",
    "SeedManifest",
    "SeedManifestEntry",
    "SeedManifestError",
]
