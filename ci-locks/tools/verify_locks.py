"""Verify that every lock re-selects the same files for its declared targets.

For each runner target we:

1. Build the marker environment and the wheel tag list the runner would have.
2. Run the exact ``Pylock.select`` machinery pip uses at install time.
3. Resolve each selected entry's relative ``path`` against the lock location,
   i.e. the same base resolution pip performs for local lock files.
4. Re-hash the file on disk and compare it with the digest recorded in the
   lock, proving the locked artifact and the wheelhouse file are identical.

The run is fully offline: only the local wheelhouse is consulted.
"""

from __future__ import annotations

import hashlib
import sys
from dataclasses import dataclass
from pathlib import Path

from pip._vendor.packaging.markers import Environment
from pip._vendor.packaging.pylock import (
    Package,
    PackageArchive,
    PackageWheel,
    Pylock,
    PylockSelectError,
)
from pip._vendor.packaging.tags import Tag
from pip._internal.utils.compat import tomllib
from pip._internal.utils.pylock import (
    package_archive_requirement_url,
    package_wheel_requirement_url,
)
from pip._internal.utils.urls import url_to_path

CI_DIR = Path(__file__).resolve().parent.parent


@dataclass(frozen=True)
class Target:
    name: str
    environment: Environment
    tags: list[Tag]
    expected_files: set[str]


LINUX_TAGS = [
    Tag("cp312", "cp312", "manylinux2014_x86_64"),
    Tag("cp312", "cp312", "manylinux_2_17_x86_64"),
    Tag("cp312", "abi3", "manylinux2014_x86_64"),
    Tag("cp312", "none", "manylinux2014_x86_64"),
    Tag("cp312", "none", "any"),
    Tag("cp311", "abi3", "manylinux2014_x86_64"),
    Tag("cp310", "abi3", "manylinux2014_x86_64"),
    Tag("py3", "none", "manylinux2014_x86_64"),
    Tag("py312", "none", "any"),
    Tag("py3", "none", "any"),
]

WINDOWS_TAGS = [
    Tag("cp313", "cp313", "win_amd64"),
    Tag("cp313", "abi3", "win_amd64"),
    Tag("cp313", "none", "any"),
    Tag("cp312", "abi3", "win_amd64"),
    Tag("py3", "none", "win_amd64"),
    Tag("py313", "none", "any"),
    Tag("py3", "none", "any"),
]

LINUX_ENV: Environment = {
    "implementation_name": "cpython",
    "implementation_version": "3.12.14",
    "os_name": "posix",
    "platform_machine": "x86_64",
    "platform_release": "6.8.0-generic",
    "platform_system": "Linux",
    "platform_version": "#1 SMP",
    "python_full_version": "3.12.14",
    "platform_python_implementation": "CPython",
    "python_version": "3.12",
    "sys_platform": "linux",
}

WINDOWS_ENV: Environment = {
    "implementation_name": "cpython",
    "implementation_version": "3.13.7",
    "os_name": "nt",
    "platform_machine": "AMD64",
    "platform_release": "11",
    "platform_system": "Windows",
    "platform_version": "10.0.26200",
    "python_full_version": "3.13.7",
    "platform_python_implementation": "CPython",
    "python_version": "3.13",
    "sys_platform": "win32",
}

TARGETS = [
    Target(
        name="linux-py312",
        environment=LINUX_ENV,
        tags=LINUX_TAGS,
        expected_files={
            "sharedlib-1.4.0-py3-none-any.whl",
            "nativepkg-2.0-cp312-cp312-manylinux_2_17_x86_64."
            "manylinux2014_x86_64.whl",
            "pylib-1.0-cp312-none-any.whl",
            "osutil-1.0-py3-none-manylinux2014_x86_64.whl",
            "directref-3.1.0-py3-none-any.whl",
        },
    ),
    Target(
        name="windows-py313",
        environment=WINDOWS_ENV,
        tags=WINDOWS_TAGS,
        expected_files={
            "sharedlib-1.4.0-py3-none-any.whl",
            "nativepkg-2.0-cp313-cp313-win_amd64.whl",
            "pylib-1.0-cp313-none-any.whl",
            "osutil-1.0-py3-none-win_amd64.whl",
            "directref-3.1.0-py3-none-any.whl",
        },
    ),
]


def load_lock(path: Path) -> Pylock:
    return Pylock.from_dict(tomllib.loads(path.read_text(encoding="utf-8")))


def selected_path(
    lock_path: Path,
    package: Package,
    entry: PackageWheel | PackageArchive,
) -> Path:
    """Resolve an entry the same way pip resolves relative lock paths."""
    if isinstance(entry, PackageWheel):
        url = package_wheel_requirement_url(str(lock_path), entry)
    else:
        assert isinstance(entry, PackageArchive)
        url = package_archive_requirement_url(str(lock_path), entry)
    return Path(url_to_path(url))


def verify_lock_for_target(lock_path: Path, target: Target) -> set[str]:
    lock = load_lock(lock_path)
    selected = lock.select(environment=target.environment, tags=target.tags)
    files: set[str] = set()
    print(f"\n== {lock_path.name} @ {target.name} ==")
    for package, entry in selected:
        resolved = selected_path(lock_path, package, entry)
        kind = "archive(direct)" if isinstance(entry, PackageArchive) else "wheel"
        assert resolved.exists(), f"missing provenance file: {resolved}"
        recorded = entry.hashes["sha256"]
        actual = hashlib.sha256(resolved.read_bytes()).hexdigest()
        assert recorded == actual, (
            f"hash mismatch for {resolved.name}: "
            f"lock={recorded} file={actual}"
        )
        marker = f" marker={package.marker}" if package.marker else ""
        print(
            f"  {package.name:<10} {kind:<14} {resolved.name}{marker}"
            f"\n             sha256={actual}"
        )
        files.add(resolved.name)
    assert files == target.expected_files, (
        f"{lock_path.name}@{target.name} selected {files}, "
        f"expected {target.expected_files}"
    )
    return files


# Each lock is checked against the targets it declares.  The combined lock
# covers both runners; each single-target lock only covers its own runner and
# is separately expected to reject the other one (negative checks below).
LOCK_TARGET_MATRIX: list[tuple[Path, list[Target]]] = [
    (CI_DIR / "pylock.linux-py312.toml", [TARGETS[0]]),
    (CI_DIR / "pylock.windows-py313.toml", [TARGETS[1]]),
    (CI_DIR / "pylock.ci.toml", TARGETS),
]


def main() -> int:
    failures = 0
    for lock_path, lock_targets in LOCK_TARGET_MATRIX:
        for target in lock_targets:
            try:
                verify_lock_for_target(lock_path, target)
            except (PylockSelectError, AssertionError) as exc:
                print(f"FAIL {lock_path.name}@{target.name}: {exc}")
                failures += 1

    # The single-target locks must refuse the other runner's environment.
    cross_checks = [
        (CI_DIR / "pylock.linux-py312.toml", TARGETS[1]),
        (CI_DIR / "pylock.windows-py313.toml", TARGETS[0]),
    ]
    print("\n== negative checks (single-target locks reject the other runner) ==")
    for lock_path, target in cross_checks:
        lock = load_lock(lock_path)
        try:
            list(lock.select(environment=target.environment, tags=target.tags))
        except PylockSelectError as exc:
            print(f"  OK  {lock_path.name} rejects {target.name}: {exc}")
        else:
            print(f"  FAIL {lock_path.name} unexpectedly accepted {target.name}")
            failures += 1

    print("\n" + ("ALL CHECKS PASSED" if failures == 0 else f"{failures} FAILURE(S)"))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
