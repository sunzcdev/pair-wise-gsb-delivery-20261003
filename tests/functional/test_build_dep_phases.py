"""Tests for the two PEP 517 build-dependency installation phases.

The first phase installs the static ``build-system.requires`` from
pyproject.toml; the backend then reports a second batch of requirements that
gets installed in a second phase. These tests pin down:

* that a conflict between the two phases names both the already selected
  version and the new constraint, while overlay priority is preserved;
* that a failed second-phase install leaves no half-finished state a later
  build could inherit;
* that the isolated build only uses local wheels, so a package installed in
  the host environment cannot mask a missing local wheel.
"""

from __future__ import annotations

from pathlib import Path

import tomli_w

from tests.lib import PipTestEnvironment, create_basic_wheel_for_package
from tests.lib.wheel import make_wheel


# A PEP 517 backend whose extra build requirements come from a local marker
# file. It never reaches out to the network, and it imports every reported
# requirement so a missing one fails the build loudly instead of being hidden.
LOCAL_ONLY_BACKEND = """\
import os
from pathlib import Path

DIST_INFO = "local_project-1.0.dist-info"


def _extra_requirements():
    reqfile = Path("backend_reqs.txt")
    if not reqfile.exists():
        return []
    return [
        line.strip()
        for line in reqfile.read_text().splitlines()
        if line.strip() and not line.strip().startswith("#")
    ]


def get_requires_for_build_wheel(config_settings=None):
    return _extra_requirements()


get_requires_for_build_editable = get_requires_for_build_wheel


def _check_requirements_importable():
    # Every reported requirement must be importable inside the isolated build
    # environment, i.e. installed from one of the local wheels.
    for req in _extra_requirements():
        bare_name = req.split(";", 1)[0]
        for separator in ("==", "!=", ">=", "<=", "~=", ">", "<", "="):
            bare_name = bare_name.split(separator, 1)[0]
        name = bare_name.strip().replace("-", "_")
        __import__(name)


def prepare_metadata_for_build_wheel(metadata_directory, config_settings=None):
    _check_requirements_importable()
    dist_info = Path(metadata_directory) / DIST_INFO
    dist_info.mkdir(parents=True, exist_ok=True)
    (dist_info / "METADATA").write_text(
        "Metadata-Version: 2.1\\nName: local_project\\nVersion: 1.0\\n"
    )
    (dist_info / "WHEEL").write_text(
        "Wheel-Version: 1.0\\nRoot-Is-Purelib: true\\n"
        "Tag: py3-none-any\\n"
    )
    return DIST_INFO


def build_wheel(
    wheel_directory, config_settings=None, metadata_directory=None
):
    _check_requirements_importable()
    from zipfile import ZipFile

    name = "local_project-1.0-py3-none-any.whl"
    with ZipFile(os.path.join(wheel_directory, name), "w") as archive:
        archive.writestr(
            "local_project/__init__.py", "__version__ = '1.0'\\n"
        )
        archive.writestr(
            DIST_INFO + "/METADATA",
            "Metadata-Version: 2.1\\nName: local_project\\nVersion: 1.0\\n",
        )
        archive.writestr(
            DIST_INFO + "/WHEEL",
            "Wheel-Version: 1.0\\nRoot-Is-Purelib: true\\n"
            "Tag: py3-none-any\\n",
        )
        archive.writestr(DIST_INFO + "/RECORD", "")
    return name


build_editable = build_wheel
"""


def _make_local_only_backend_wheel(script: PipTestEnvironment) -> Path:
    """Build a backend that only resolves dependencies from local wheels."""
    wheel_name = "local_only_backend-1.0-py2.py3-none-any.whl"
    wheel_path = script.scratch_path / wheel_name
    make_wheel(
        name="local_only_backend",
        version="1.0",
        extra_files={"local_only_backend.py": LOCAL_ONLY_BACKEND},
    ).save_to(wheel_path)
    return wheel_path


def _make_project(
    tmpdir: Path,
    *,
    requires: list[str],
    backend_reqs: str = "",
) -> Path:
    project_dir = tmpdir / "project"
    project_dir.mkdir()
    project_dir.joinpath("pyproject.toml").write_text(
        tomli_w.dumps(
            {
                "build-system": {
                    "requires": requires,
                    "build-backend": "local_only_backend",
                }
            }
        )
    )
    if backend_reqs:
        project_dir.joinpath("backend_reqs.txt").write_text(backend_reqs)
    return project_dir


def test_second_phase_conflict_names_selected_version_and_constraint(
    script: PipTestEnvironment, tmpdir: Path
) -> None:
    _make_local_only_backend_wheel(script)
    create_basic_wheel_for_package(script, "simplewheel", "1.0")
    create_basic_wheel_for_package(script, "simplewheel", "2.0")

    # Phase 1 (overlay) selects simplewheel 1.0; the backend then asks for an
    # incompatible 2.0 in phase 2.
    project_dir = _make_project(
        tmpdir,
        requires=["local_only_backend", "simplewheel==1.0"],
        backend_reqs="simplewheel==2.0",
    )

    result = script.pip(
        "install",
        "--no-index",
        "-f",
        script.scratch_path,
        project_dir,
        expect_error=True,
    )
    assert result.returncode != 0
    assert (
        "simplewheel==1.0 is incompatible with simplewheel==2.0"
    ) in result.stderr, str(result)


def test_second_phase_satisfied_by_overlay_does_not_reinstall(
    script: PipTestEnvironment, tmpdir: Path
) -> None:
    _make_local_only_backend_wheel(script)
    create_basic_wheel_for_package(script, "simplewheel", "1.0")
    create_basic_wheel_for_package(script, "simplewheel", "2.0")

    # The phase-2 constraint is already satisfied by the overlay selection, so
    # no second install happens and the overlay priority wins.
    project_dir = _make_project(
        tmpdir,
        requires=["local_only_backend", "simplewheel==1.0"],
        backend_reqs="simplewheel",
    )

    result = script.pip(
        "install",
        "--no-index",
        "-f",
        script.scratch_path,
        project_dir,
    )
    result.assert_installed("local_project", editable=False)
    assert "Installing backend dependencies:" not in result.stdout


def test_failed_second_phase_install_does_not_poison_retry(
    script: PipTestEnvironment, tmpdir: Path
) -> None:
    _make_local_only_backend_wheel(script)
    create_basic_wheel_for_package(script, "extradep", "1.0")

    # The backend asks for two requirements; only one has a local wheel. The
    # second-phase installation therefore fails (nothing can be built).
    project_dir = _make_project(
        tmpdir,
        requires=["local_only_backend"],
        backend_reqs="extradep==1.0\nghostdep==9.9",
    )

    first = script.pip(
        "install",
        "--no-index",
        "-f",
        script.scratch_path,
        project_dir,
        expect_error=True,
    )
    assert first.returncode != 0
    assert "ghostdep" in first.stderr

    # Make the second phase satisfiable using only local wheels, then rebuild.
    # The retry must run against a fresh build environment instead of reusing
    # the failed attempt's half-finished prefix.
    project_dir.joinpath("backend_reqs.txt").write_text("extradep==1.0")
    second = script.pip(
        "install",
        "--no-index",
        "-f",
        script.scratch_path,
        project_dir,
    )
    second.assert_installed("local_project", editable=False)


def test_missing_local_wheel_is_not_masked_by_host_package(
    script: PipTestEnvironment, tmpdir: Path
) -> None:
    _make_local_only_backend_wheel(script)
    # The requested build dependency has no wheel in the build's local wheel
    # directory (script.scratch_path, passed via -f). A copy is installed into
    # the host (test) environment from a separate directory that is never
    # handed to the build, so the isolated environment cannot see it.
    host_dir = tmpdir / "host_packages"
    host_dir.mkdir()
    make_wheel(
        name="hostonly",
        version="1.0",
        extra_files={"hostonly/__init__.py": "__version__ = '1.0'\n"},
    ).save_to(host_dir / "hostonly-1.0-py2.py3-none-any.whl")
    script.pip(
        "install",
        "--no-index",
        "-f",
        host_dir,
        "hostonly==1.0",
    )

    project_dir = _make_project(
        tmpdir,
        requires=["local_only_backend"],
        backend_reqs="hostonly==1.0",
    )

    result = script.pip(
        "install",
        "--no-index",
        "-f",
        script.scratch_path,
        project_dir,
        expect_error=True,
    )
    assert result.returncode != 0
    # The host-installed copy cannot satisfy the build requirement: the local
    # wheel lookup inside the isolated environment must fail instead.
    joined = result.stderr + result.stdout
    assert "hostonly" in joined
    assert (
        "No matching distribution" in joined
        or "Cannot install build" in joined
    )
