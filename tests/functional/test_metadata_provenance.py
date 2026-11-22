"""Tests for metadata that changes between prepare_metadata and the built wheel.

The tests here use a local build backend that can be told, via a JSON file in
the project directory, to report one set of metadata when preparing metadata
(which drives dependency resolution) and another set when building the wheel.
That lets us check that pip:

* records both views and labels where each came from,
* refuses to install when resolver-affecting fields change, and
* still installs when only descriptive fields change.
"""

from __future__ import annotations

import json
from pathlib import Path

from tests.lib import PipTestEnvironment

PYPROJECT_TOML = """\
[build-system]
requires = []
build-backend = "test_backend"
backend-path = ["."]
"""

BACKEND_SRC = '''\
import base64
import csv
import hashlib
import io
import json
import os
from zipfile import ZipFile

WHEEL = """\\
Wheel-Version: 1.0
Generator: metadata-change-backend
Root-Is-Purelib: true
Tag: py3-none-any
"""


def _config():
    path = os.path.join(os.path.dirname(__file__), "backend-config.json")
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def _metadata(config):
    lines = [
        "Metadata-Version: 2.1",
        "Name: " + config["name"],
        "Version: " + config["version"],
    ]
    if config.get("summary"):
        lines.append("Summary: " + config["summary"])
    for requirement in config.get("requires_dist", []):
        lines.append("Requires-Dist: " + requirement)
    return "\\n".join(lines) + "\\n"


def _distinfo(config):
    return "%s-%s.dist-info" % (config["name"], config["version"])


def _add_file(z, record, name, data):
    data = data.encode("utf-8")
    z.writestr(name, data)
    digest = hashlib.sha256(data).digest()
    encoded = base64.urlsafe_b64encode(digest).rstrip(b"=").decode("ascii")
    record.append((name, "sha256=" + encoded, len(data)))


def _write_metadata(metadata_directory, config):
    distinfo = _distinfo(config)
    target = os.path.join(metadata_directory, distinfo)
    os.makedirs(target, exist_ok=True)
    with open(os.path.join(target, "METADATA"), "w", encoding="utf-8") as f:
        f.write(_metadata(config))
    with open(os.path.join(target, "WHEEL"), "w", encoding="utf-8") as f:
        f.write(WHEEL)
    return distinfo


def get_requires_for_build_wheel(config_settings=None):
    return []


def prepare_metadata_for_build_wheel(metadata_directory, config_settings=None):
    config = _config()["prepare"]
    return _write_metadata(metadata_directory, config)


def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    config = _config()["build"]
    distinfo = _distinfo(config)
    wheel_name = "%s-%s-py3-none-any.whl" % (config["name"], config["version"])
    wheel_path = os.path.join(wheel_directory, wheel_name)
    record = []
    with ZipFile(wheel_path, "w") as z:
        _add_file(z, record, distinfo + "/WHEEL", WHEEL)
        _add_file(z, record, distinfo + "/METADATA", _metadata(config))
        _add_file(z, record, config["name"] + "/__init__.py", "")
        record_name = distinfo + "/RECORD"
        record.append((record_name, "", ""))
        buf = io.StringIO()
        csv.writer(buf).writerows(record)
        z.writestr(record_name, buf.getvalue().encode("utf-8"))
    return wheel_name


def build_editable(wheel_directory, config_settings=None, metadata_directory=None):
    return build_wheel(wheel_directory, config_settings, metadata_directory)
'''


def make_project(
    path: Path,
    *,
    prepare: dict,
    build: dict,
) -> Path:
    """Create a project whose backend can report different metadata per hook.

    ``prepare`` is the metadata reported by ``prepare_metadata_for_build_wheel``
    (used for resolution); ``build`` is the metadata written into the wheel.
    """
    project_dir = path / "project"
    project_dir.mkdir()
    project_dir.joinpath("pyproject.toml").write_text(PYPROJECT_TOML)
    project_dir.joinpath("test_backend.py").write_text(BACKEND_SRC)

    def complete(metadata: dict) -> dict:
        data = {"name": "changedmeta", "version": "1.0"}
        data.update(metadata)
        return data

    project_dir.joinpath("backend-config.json").write_text(
        json.dumps({"prepare": complete(prepare), "build": complete(build)})
    )
    return project_dir


def _report_item(report_path: Path) -> dict:
    report = json.loads(report_path.read_text())
    items = {item["metadata"]["name"]: item for item in report["install"]}
    return items["changedmeta"]


def test_descriptive_change_is_reported_and_install_continues(
    script: PipTestEnvironment,
    tmp_path: Path,
) -> None:
    """A changed Summary is reported, but does not block the install."""
    project_dir = make_project(
        script.scratch_path,
        prepare={"summary": "summary at resolution time"},
        build={"summary": "summary at build time"},
    )
    report_path = tmp_path / "report.json"

    result = script.pip(
        "install",
        "--no-build-isolation",
        "--no-index",
        project_dir,
        "--report",
        str(report_path),
    )

    script.assert_installed(**{"changedmeta": "1.0"})
    assert "Successfully installed changedmeta-1.0" in result.stdout

    provenance = _report_item(report_path)["metadata_provenance"]
    # The two views are labelled by the source they came from.
    assert provenance["resolution"]["source"] == "prepare-metadata"
    assert provenance["built"]["source"] == "built-wheel"
    assert provenance["resolution"]["digest"] != provenance["built"]["digest"]
    assert provenance["blocking"] is False
    fields = {
        difference["field"]: difference for difference in provenance["differences"]
    }
    assert "summary" in fields
    assert fields["summary"]["affects_resolution"] is False


def test_dependency_change_is_rejected_before_install(
    script: PipTestEnvironment,
    tmp_path: Path,
) -> None:
    """A changed Requires-Dist stops the install and explains the change."""
    project_dir = make_project(
        script.scratch_path,
        prepare={"requires_dist": []},
        build={"requires_dist": ["extra-dependency"]},
    )
    report_path = tmp_path / "report.json"

    result = script.pip(
        "install",
        "--no-build-isolation",
        "--no-index",
        project_dir,
        "--report",
        str(report_path),
        expect_error=True,
    )

    assert result.returncode != 0
    # The problem is reported by name, with both sources distinguished.
    assert "built-metadata-changed" in result.stderr
    assert "prepare-metadata" in result.stderr
    assert "built-wheel" in result.stderr
    assert "Requires-Dist" in result.stderr
    # Nothing was installed.
    script.assert_not_installed("changedmeta")

    # The report was written and flags the change as blocking.
    provenance = _report_item(report_path)["metadata_provenance"]
    assert provenance["blocking"] is True
    fields = {
        difference["field"]: difference for difference in provenance["differences"]
    }
    assert fields["requires_dist"]["affects_resolution"] is True


def test_identical_metadata_reports_no_differences(
    script: PipTestEnvironment,
    tmp_path: Path,
) -> None:
    """When both hooks agree there is nothing to report and install proceeds."""
    project_dir = make_project(
        script.scratch_path,
        prepare={
            "requires_dist": ['requests >= 2.0; python_version < "0"'],
            "summary": "same",
        },
        build={
            "requires_dist": ['Requests>=2.0; python_version < "0"'],
            "summary": "same",
        },
    )
    report_path = tmp_path / "report.json"

    result = script.pip(
        "install",
        "--no-build-isolation",
        "--no-index",
        project_dir,
        "--report",
        str(report_path),
        expect_stderr=True,
    )

    assert result.returncode == 0
    provenance = _report_item(report_path)["metadata_provenance"]
    # Equivalent spellings are not flagged, and the digests match.
    assert provenance["differences"] == []
    assert provenance["blocking"] is False
    assert provenance["resolution"]["digest"] == provenance["built"]["digest"]
