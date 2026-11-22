"""Generate the PEP 751 lock files for the two CI runner targets.

This mirrors what ``pip lock`` writes (same vendored ``packaging.pylock``
model and ``tomli_w`` serializer), but assembles one lock per declared
runner target plus a combined lock covering both targets.  Every artifact is
referenced through the local ``wheelhouse/`` directory with its real sha256
digest, so the locks are fully offline and self-verifying.

Runner targets
--------------
linux-py312   CPython 3.12 on Linux x86_64
windows-py313 CPython 3.13 on Windows AMD64

The same package version may resolve to different physical artifacts on the
two runners; each artifact keeps its own digest.  The ``directref`` package is
a direct file reference (an ``archive`` entry) whose provenance is retained
through a relative path into the wheelhouse.
"""

from __future__ import annotations

import hashlib
from pathlib import Path

from pip._vendor import tomli_w
from pip._vendor.packaging.markers import Marker
from pip._vendor.packaging.pylock import (
    Package,
    PackageArchive,
    PackageWheel,
    Pylock,
)
from pip._vendor.packaging.specifiers import SpecifierSet
from pip._vendor.packaging.utils import canonicalize_name
from pip._vendor.packaging.version import Version

CI_DIR = Path(__file__).resolve().parent.parent
WHEELHOUSE = CI_DIR / "wheelhouse"

LINUX_ENV_MARKER = 'sys_platform == "linux" and python_version == "3.12"'
WINDOWS_ENV_MARKER = 'sys_platform == "win32" and python_version == "3.13"'


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def wheel_record(filename: str) -> PackageWheel:
    """A wheel entry with relative-path provenance inside the wheelhouse."""
    return PackageWheel(
        name=filename,
        path=f"wheelhouse/{filename}",
        hashes={"sha256": sha256(WHEELHOUSE / filename)},
    )


def direct_archive(filename: str) -> PackageArchive:
    """A direct file reference (PEP 751 archive) retaining its source path."""
    return PackageArchive(
        path=f"wheelhouse/{filename}",
        hashes={"sha256": sha256(WHEELHOUSE / filename)},
    )


# Wheel filenames produced by build_wheelhouse.py.
SHAREDLIB = "sharedlib-1.4.0-py3-none-any.whl"
NATIVE_LINUX = (
    "nativepkg-2.0-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl"
)
NATIVE_WINDOWS = "nativepkg-2.0-cp313-cp313-win_amd64.whl"
PYLIB_LINUX = "pylib-1.0-cp312-none-any.whl"
PYLIB_WINDOWS = "pylib-1.0-cp313-none-any.whl"
OSUTIL_LINUX = "osutil-1.0-py3-none-manylinux2014_x86_64.whl"
OSUTIL_WINDOWS = "osutil-1.0-py3-none-win_amd64.whl"
HOSTWIN = "hostwin-1.0-cp312-cp312-win_amd64.whl"
DIRECTREF = "directref-3.1.0-py3-none-any.whl"


def single_target_packages(*, linux: bool) -> list[Package]:
    native = NATIVE_LINUX if linux else NATIVE_WINDOWS
    pylib = PYLIB_LINUX if linux else PYLIB_WINDOWS
    osutil = OSUTIL_LINUX if linux else OSUTIL_WINDOWS
    return [
        Package(
            name=canonicalize_name("directref"),
            archive=direct_archive(DIRECTREF),
        ),
        Package(
            name=canonicalize_name("nativepkg"),
            version=Version("2.0"),
            wheels=[wheel_record(native)],
        ),
        Package(
            name=canonicalize_name("osutil"),
            version=Version("1.0"),
            wheels=[wheel_record(osutil)],
        ),
        Package(
            name=canonicalize_name("pylib"),
            version=Version("1.0"),
            wheels=[wheel_record(pylib)],
        ),
        Package(
            name=canonicalize_name("sharedlib"),
            version=Version("1.4.0"),
            wheels=[wheel_record(SHAREDLIB)],
        ),
    ]


def combined_packages() -> list[Package]:
    return [
        # One direct file reference, shared by both runners.
        Package(
            name=canonicalize_name("directref"),
            archive=direct_archive(DIRECTREF),
        ),
        # Same version 2.0, two artifacts: wheel tags decide per runner,
        # and each artifact carries its own digest.
        Package(
            name=canonicalize_name("nativepkg"),
            version=Version("2.0"),
            wheels=[
                wheel_record(NATIVE_LINUX),
                wheel_record(NATIVE_WINDOWS),
            ],
        ),
        # Two package entries with mutually exclusive markers; selection
        # never sees both at once, so no ambiguity error is raised.
        Package(
            name=canonicalize_name("osutil"),
            version=Version("1.0"),
            marker=Marker('sys_platform == "linux"'),
            wheels=[wheel_record(OSUTIL_LINUX)],
        ),
        Package(
            name=canonicalize_name("osutil"),
            version=Version("1.0"),
            marker=Marker('sys_platform == "win32"'),
            wheels=[wheel_record(OSUTIL_WINDOWS)],
        ),
        # Interpreter tags (cp312 vs cp313) differentiate the runners.
        Package(
            name=canonicalize_name("pylib"),
            version=Version("1.0"),
            wheels=[
                wheel_record(PYLIB_LINUX),
                wheel_record(PYLIB_WINDOWS),
            ],
        ),
        # Universal wheel, identical artifact on both runners.
        Package(
            name=canonicalize_name("sharedlib"),
            version=Version("1.4.0"),
            wheels=[wheel_record(SHAREDLIB)],
        ),
    ]


def write_lock(path: Path, lock: Pylock) -> None:
    lock.validate()  # PEP 751 compliance check before serializing.
    path.write_text(tomli_w.dumps(lock.to_dict()), encoding="utf-8")
    print(f"wrote {path.relative_to(CI_DIR)}")


def main() -> None:
    linux_lock = Pylock(
        lock_version=Version("1.0"),
        created_by="pip",
        environments=[Marker(LINUX_ENV_MARKER)],
        requires_python=SpecifierSet(">=3.12,<3.13"),
        packages=single_target_packages(linux=True),
    )
    windows_lock = Pylock(
        lock_version=Version("1.0"),
        created_by="pip",
        environments=[Marker(WINDOWS_ENV_MARKER)],
        requires_python=SpecifierSet(">=3.13,<3.14"),
        packages=single_target_packages(linux=False),
    )
    combined_lock = Pylock(
        lock_version=Version("1.0"),
        created_by="pip",
        environments=[
            Marker(LINUX_ENV_MARKER),
            Marker(WINDOWS_ENV_MARKER),
        ],
        requires_python=SpecifierSet(">=3.12,<3.14"),
        packages=combined_packages(),
    )

    write_lock(CI_DIR / "pylock.linux-py312.toml", linux_lock)
    write_lock(CI_DIR / "pylock.windows-py313.toml", windows_lock)
    write_lock(CI_DIR / "pylock.ci.toml", combined_lock)

    # Host lock: a native Windows / CPython 3.12 target matching the machine
    # this repository is exercised on.  It reuses the same wheelhouse so that
    # a real ``pip install --dry-run`` can verify the full offline pipeline.
    host_lock = Pylock(
        lock_version=Version("1.0"),
        created_by="pip",
        environments=[Marker('sys_platform == "win32" and python_version == "3.12"')],
        requires_python=SpecifierSet(">=3.12,<3.13"),
        packages=[
            Package(
                name=canonicalize_name("directref"),
                archive=direct_archive(DIRECTREF),
            ),
            Package(
                name=canonicalize_name("hostwin"),
                version=Version("1.0"),
                wheels=[wheel_record(HOSTWIN)],
            ),
            Package(
                name=canonicalize_name("osutil"),
                version=Version("1.0"),
                wheels=[wheel_record(OSUTIL_WINDOWS)],
            ),
            Package(
                name=canonicalize_name("pylib"),
                version=Version("1.0"),
                wheels=[wheel_record(PYLIB_LINUX)],
            ),
            Package(
                name=canonicalize_name("sharedlib"),
                version=Version("1.4.0"),
                wheels=[wheel_record(SHAREDLIB)],
            ),
        ],
    )
    write_lock(CI_DIR / "pylock.host-win-py312.toml", host_lock)


if __name__ == "__main__":
    main()
