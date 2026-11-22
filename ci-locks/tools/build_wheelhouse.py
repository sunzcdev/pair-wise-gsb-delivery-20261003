"""Build the local, offline wheelhouse shared by both CI runner targets.

The wheelhouse deliberately contains every resolution dimension exercised by
the two PEP 751 lock files in this directory:

* a pure-Python wheel usable by every runner            (sharedlib)
* two wheels carrying the SAME version number but with
  different contents, tags and therefore different
  sha256 digests, one per runner                        (nativepkg 2.0)
* two interpreter-gated wheels selected by the declared
  Python version of each runner                         (pylib 1.0)
* two platform-gated pure wheels selected by markers    (osutil 1.0)
* one wheel only referenced directly via a file path    (directref 1.0)

The script only relies on the Python standard library, so it works without
network access or a build backend.
"""

from __future__ import annotations

import hashlib
import zipfile
from pathlib import Path

WHEELHOUSE = Path(__file__).resolve().parent.parent / "wheelhouse"

# Fixed zip metadata keeps every rebuild byte-identical, so the recorded
# sha256 digests stay stable across machines and runs (no embedded clocks).
ZIP_DATE_TIME = (2026, 11, 22, 0, 0, 0)
ZIP_EXTERNAL_ATTR = (0o644 << 16)

# (distribution, normalized import module, version, wheel tag triple, payload)
WHEELS: list[tuple[str, str, str, str, str]] = [
    # sharedlib: universal pure-Python wheel, identical on every runner.
    ("sharedlib", "sharedlib", "1.4.0", "py3-none-any",
     'VALUE = "sharedlib 1.4.0 - universal"\n'),
    # nativepkg: same version 2.0 on both runners, but two distinct artifacts
    # with different platform tags, contents and sha256 digests.
    ("nativepkg", "nativepkg", "2.0",
     "cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64",
     'VALUE = "nativepkg 2.0 - linux x86_64 cp312 build"\n'),
    ("nativepkg", "nativepkg", "2.0",
     "cp313-cp313-win_amd64",
     'VALUE = "nativepkg 2.0 - windows amd64 cp313 build"\n'),
    # pylib: version 1.0 everywhere, interpreter tag differentiates the runner.
    ("pylib", "pylib", "1.0", "cp312-none-any",
     'VALUE = "pylib 1.0 - cp312 wheel"\n'),
    ("pylib", "pylib", "1.0", "cp313-none-any",
     'VALUE = "pylib 1.0 - cp313 wheel"\n'),
    # osutil: pure wheels selected through environment markers, not tags.
    ("osutil", "osutil", "1.0", "py3-none-manylinux2014_x86_64",
     'VALUE = "osutil 1.0 - linux build"\n'),
    ("osutil", "osutil", "1.0", "py3-none-win_amd64",
     'VALUE = "osutil 1.0 - windows build"\n'),
    # hostwin: wheel matching the native Windows / CPython 3.12 machine used
    # for the real-pip end-to-end check (no cross-target simulation needed).
    ("hostwin", "hostwin", "1.0", "cp312-cp312-win_amd64",
     'VALUE = "hostwin 1.0 - native windows cp312 build"\n'),
    # directref: pinned by a direct file reference rather than the index.
    ("directref", "directref", "3.1.0", "py3-none-any",
     'VALUE = "directref 3.1.0 - direct file reference"\n'),
]


def wheel_filename(dist: str, version: str, tag: str) -> str:
    return f"{dist.replace('-', '_')}-{version}-{tag}.whl"


def build_wheel(
    dist: str, module: str, version: str, tag: str, payload: str
) -> Path:
    interpreter, abi, platform = tag.split("-")
    path = WHEELHOUSE / wheel_filename(dist, version, tag)
    dist_info = f"{dist.replace('-', '_')}-{version}.dist-info"
    metadata = (
        f"Metadata-Version: 2.1\n"
        f"Name: {dist}\n"
        f"Version: {version}\n"
        f"Summary: Offline wheelhouse fixture for {dist}\n"
    )
    wheel_meta = (
        "Wheel-Version: 1.0\n"
        "Generator: ci-locks/build_wheelhouse.py\n"
        "Root-Is-Purelib: true\n"
        f"Tag: {interpreter}-{abi}-{platform}\n"
    )
    record = (
        f"{module}/__init__.py,,\n"
        f"{dist_info}/METADATA,,\n"
        f"{dist_info}/WHEEL,,\n"
        f"{dist_info}/RECORD,,\n"
    )
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as zf:
        for arcname, data in (
            (f"{module}/__init__.py", payload),
            (f"{dist_info}/METADATA", metadata),
            (f"{dist_info}/WHEEL", wheel_meta),
            (f"{dist_info}/RECORD", record),
        ):
            info = zipfile.ZipInfo(arcname, date_time=ZIP_DATE_TIME)
            info.external_attr = ZIP_EXTERNAL_ATTR
            info.compress_type = zipfile.ZIP_DEFLATED
            zf.writestr(info, data)
    return path


def main() -> None:
    WHEELHOUSE.mkdir(parents=True, exist_ok=True)
    for old in WHEELHOUSE.glob("*.whl"):
        old.unlink()
    print(f"Building wheelhouse in {WHEELHOUSE}")
    for dist, module, version, tag, payload in WHEELS:
        path = build_wheel(dist, module, version, tag, payload)
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        print(f"  {path.name}\n    sha256={digest}")


if __name__ == "__main__":
    main()
