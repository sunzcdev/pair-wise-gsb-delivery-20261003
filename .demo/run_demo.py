"""Demonstrate the fixed-runner seed manifest end to end on the Windows runner.

Scenarios:
  1. strict manifest satisfied by the genuine local wheel  -> environment seeds that exact wheel
  2. strict manifest where a version-only local candidate fails the digest -> hard error
  3. no manifest (regular virtualenv) -> original selection behavior is unchanged
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent
WHEELHOUSE = ROOT / "wheelhouse"
COMMON_ENV = {
    **os.environ,
    "VIRTUALENV_NO_DOWNLOAD": "1",
    "VIRTUALENV_NO_PERIODIC_UPDATE": "1",
}


def run(title: str, args: list[str]) -> int:
    print("=" * 78)
    print(title)
    print("-" * 78)
    cmd = [sys.executable, "-m", "virtualenv", *args]
    print("command:", " ".join(cmd))
    print("-" * 78)
    completed = subprocess.run(cmd, text=True, capture_output=True, env=COMMON_ENV, check=False)
    print(completed.stdout, end="")
    if completed.stderr:
        print(completed.stderr, end="", file=sys.stderr)
    print(f"[exit code {completed.returncode}]")
    return completed.returncode


def seeded_pip(env_dir: Path) -> str | None:
    site_packages = env_dir / "Lib" / "site-packages"
    dist_infos = sorted(site_packages.glob("pip-*.dist-info")) if site_packages.exists() else []
    return dist_infos[0].name if dist_infos else None


def has_tamper_marker(env_dir: Path) -> bool:
    # the tampered wheel carries this extra file; the genuine wheel never does
    site_packages = env_dir / "Lib" / "site-packages"
    return site_packages.exists() and any(site_packages.rglob("manifest-demo-marker.txt"))


def main() -> None:
    # scenario 1: genuine wheel satisfies the strict manifest
    good_env = ROOT / "env-strict-ok"
    shutil.rmtree(good_env, ignore_errors=True)
    code = run(
        "1. strict manifest, genuine wheel matches version + sha256 + Requires-Python",
        [
            str(good_env),
            "--no-setuptools",
            "--extra-search-dir",
            str(WHEELHOUSE),
            "--seed-manifest",
            str(ROOT / "seed-manifest.json"),
            "-v",
        ],
    )
    print(f"=> seeded package: {seeded_pip(good_env)}, tamper marker present: {has_tamper_marker(good_env)} (exit {code})")

    # scenario 2: only the version matches; digest (and thus declared content) does not
    bad_env = ROOT / "env-strict-bad"
    shutil.rmtree(bad_env, ignore_errors=True)
    code = run(
        "2. strict manifest, local candidate matches version only, digest mismatch",
        [
            str(bad_env),
            "--no-setuptools",
            "--extra-search-dir",
            str(WHEELHOUSE),
            "--seed-manifest",
            str(ROOT / "seed-manifest-bad-digest.json"),
        ],
    )
    print(f"=> environment has no seed packages: {seeded_pip(bad_env) is None} (exit {code})")

    # scenario 3: loose manifest rejects the bad digest with a warning but keeps the run alive
    loose_env = ROOT / "env-loose"
    shutil.rmtree(loose_env, ignore_errors=True)
    code = run(
        "3. loose manifest, digest mismatch warns and falls back to the regular bundled selection",
        [
            str(loose_env),
            "--no-setuptools",
            "--extra-search-dir",
            str(WHEELHOUSE),
            "--seed-manifest",
            str(ROOT / "seed-manifest-loose.json"),
        ],
    )
    print(f"=> seeded package: {seeded_pip(loose_env)} (exit {code})")

    # scenario 4: no manifest keeps the ordinary virtualenv selection (both candidates present)
    plain_env = ROOT / "env-plain"
    shutil.rmtree(plain_env, ignore_errors=True)
    code = run(
        "4. no manifest, regular virtualenv selection with both candidates in the search dir",
        [
            str(plain_env),
            "--no-setuptools",
            "--extra-search-dir",
            str(WHEELHOUSE),
        ],
    )
    print(f"=> seeded package: {seeded_pip(plain_env)} (exit {code})")


if __name__ == "__main__":
    main()
