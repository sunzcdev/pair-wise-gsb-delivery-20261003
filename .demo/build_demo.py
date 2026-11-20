"""Build the fixed-runner seed manifest demo: two local candidates of the same pip version.

One wheel is the genuine bundled pip wheel (version + sha256 + Requires-Python all match the
manifest). The other keeps the same distribution and version in its file name but carries
different bytes, so only the version matches; the manifest digest check must reject it.
"""

from __future__ import annotations

import hashlib
import json
import shutil
import sys
import zipfile
from pathlib import Path

from virtualenv.seed.wheels.embed import get_embed_wheel

ROOT = Path(__file__).resolve().parent
for_py_version = f"{sys.version_info.major}.{sys.version_info.minor}"
BUNDLED = get_embed_wheel("pip", for_py_version).path
VERSION = BUNDLED.name.split("-")[1]
WHEELHOUSE = ROOT / "wheelhouse"
GENUINE = WHEELHOUSE / BUNDLED.name
# distinct file name so both wheels can sit in one folder, but the distribution and version
# segments stay identical; only the bytes (and thus the digest) differ
TAMPERED = WHEELHOUSE / f"pip-{VERSION}-py2.py3-none-any.whl"


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main() -> None:
    WHEELHOUSE.mkdir(parents=True, exist_ok=True)
    for old in WHEELHOUSE.iterdir():
        old.unlink()

    # candidate 1: the genuine wheel - version, digest and Requires-Python all match the manifest
    shutil.copy2(BUNDLED, GENUINE)

    # candidate 2: a wheel whose file name and METADATA keep the same distribution and version
    # spelling as the genuine one, but whose bytes differ, so only the version matches. The
    # manifest that names no exact file name reaches the digest check and must reject it.
    with zipfile.ZipFile(BUNDLED) as source, zipfile.ZipFile(TAMPERED, "w") as target:
        for item in source.infolist():
            data = source.read(item.filename)
            if item.filename.endswith(".dist-info/RECORD"):
                data = data + b"# tampered payload for the manifest demo\n"
            target.writestr(item, data)
        target.writestr("manifest-demo-marker.txt", b"these bytes are not covered by the manifest digest\n")

    manifest = {
        "schema_version": 1,
        "strict": True,
        "packages": [
            {
                "distribution": "pip",
                "version": VERSION,
                "sha256": sha256(GENUINE),
                "filename": GENUINE.name,
            }
        ],
    }
    manifest_path = ROOT / "seed-manifest.json"
    manifest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")

    loose_manifest = {
        "schema_version": 1,
        "strict": False,
        "packages": [
            {
                "distribution": "pip",
                "version": VERSION,
                # a digest neither local wheel matches: loose mode warns and hands control back
                "sha256": "0" * 64,
            }
        ],
    }
    loose_path = ROOT / "seed-manifest-loose.json"
    loose_path.write_text(json.dumps(loose_manifest, indent=2) + "\n", encoding="utf-8")

    # no file-name pin: the tampered wheel passes the distribution/version checks and only loses
    # on the declared digest, demonstrating that a version-only match is not enough in strict mode
    bad_manifest = {
        "schema_version": 1,
        "strict": True,
        "packages": [
            {
                "distribution": "pip",
                "version": VERSION,
                "sha256": "0" * 64,
            }
        ],
    }
    bad_path = ROOT / "seed-manifest-bad-digest.json"
    bad_path.write_text(json.dumps(bad_manifest, indent=2) + "\n", encoding="utf-8")

    print(f"bundled wheel : {BUNDLED.name}")
    print(f"genuine wheel : {GENUINE.name}  sha256={sha256(GENUINE)}")
    print(f"tampered wheel: {TAMPERED.name} sha256={sha256(TAMPERED)}")
    print(f"manifest      : {manifest_path}")


if __name__ == "__main__":
    main()
