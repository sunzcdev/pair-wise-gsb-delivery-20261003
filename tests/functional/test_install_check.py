import json
from collections.abc import Iterable
from pathlib import Path

from tests.lib import (
    PipTestEnvironment,
    create_basic_wheel_for_package,
    create_test_package_with_setup,
)


def assert_contains_expected_lines(string: str, expected_lines: Iterable[str]) -> None:
    for expected_line in expected_lines:
        assert (expected_line + "\n") in string


def test_check_install_canonicalization(script: PipTestEnvironment) -> None:
    pkga_path = create_test_package_with_setup(
        script,
        name="pkgA",
        version="1.0",
        install_requires=["normal-missing", "SPECIAL.missing"],
    )
    normal_path = create_test_package_with_setup(
        script,
        name="normal-missing",
        version="0.1",
    )
    special_path = create_test_package_with_setup(
        script,
        name="SPECIAL.missing",
        version="0.1",
    )

    # Let's install pkgA without its dependency
    result = script.pip(
        "install", "--no-build-isolation", "--no-index", pkga_path, "--no-deps"
    )
    assert "Successfully installed pkgA-1.0" in result.stdout, str(result)

    # Install the first missing dependency. Only an error for the
    # second dependency should remain.
    result = script.pip(
        "install",
        "--no-build-isolation",
        "--no-index",
        normal_path,
        "--quiet",
        allow_stderr_error=True,
    )
    expected_lines = [
        "pkga 1.0 requires SPECIAL.missing, which is not installed.",
    ]
    # Deprecated python versions produce an extra warning on stderr
    assert_contains_expected_lines(result.stderr, expected_lines)
    assert result.returncode == 0

    # Install the second missing package and expect that there is no warning
    # during the installation. This is special as the package name requires
    # name normalization (as in https://github.com/pypa/pip/issues/5134)
    result = script.pip(
        "install",
        "--no-build-isolation",
        "--no-index",
        special_path,
        "--quiet",
    )
    assert "requires" not in result.stderr
    assert result.returncode == 0

    # Double check that all errors are resolved in the end
    result = script.pip("check")
    expected_lines = [
        "No broken requirements found.",
    ]
    assert_contains_expected_lines(result.stdout, expected_lines)
    assert result.returncode == 0


def test_check_install_does_not_warn_for_out_of_graph_issues(
    script: PipTestEnvironment,
) -> None:
    pkg_broken_path = create_test_package_with_setup(
        script,
        name="broken",
        version="1.0",
        install_requires=["missing", "conflict < 1.0"],
    )
    pkg_unrelated_path = create_test_package_with_setup(
        script,
        name="unrelated",
        version="1.0",
    )
    pkg_conflict_path = create_test_package_with_setup(
        script,
        name="conflict",
        version="1.0",
    )

    # Install a package without it's dependencies
    result = script.pip(
        "install", "--no-build-isolation", "--no-index", pkg_broken_path, "--no-deps"
    )
    assert "requires" not in result.stderr

    # Install conflict package
    result = script.pip(
        "install",
        "--no-build-isolation",
        "--no-index",
        pkg_conflict_path,
        allow_stderr_error=True,
    )
    assert_contains_expected_lines(
        result.stderr,
        [
            "broken 1.0 requires missing, which is not installed.",
            "broken 1.0 requires conflict<1.0, "
            "but you have conflict 1.0 which is incompatible.",
        ],
    )

    # Install unrelated package
    result = script.pip(
        "install",
        "--no-build-isolation",
        "--no-index",
        pkg_unrelated_path,
        "--quiet",
    )
    # should not warn about broken's deps when installing unrelated package
    assert "requires" not in result.stderr

    result = script.pip("check", expect_error=True)
    expected_lines = [
        "broken 1.0 requires missing, which is not installed.",
        "broken 1.0 has requirement conflict<1.0, but you have conflict 1.0.",
    ]
    assert_contains_expected_lines(result.stdout, expected_lines)


def test_report_compares_dependency_graph_edges(
    script: PipTestEnvironment, tmp_path: Path
) -> None:
    """An upgrade adds one gap to an environment that already has others.

    The environment starts with three packages installed without their
    dependencies, which is how a real environment accumulates relations that
    were broken all along:

    * ``app 1.0`` is satisfied by ``lib 1``;
    * ``plugin 1.0`` wants ``lib<2``;
    * ``legacy 1.0`` wants ``gone`` (never installed) and ``olddep`` (also not).

    Upgrading ``app`` to 2.0 pulls in ``lib 3`` and ``olddep``, which makes
    ``plugin`` incompatible with ``lib`` and satisfies ``legacy``'s need for
    ``olddep``.  So exactly one relation is introduced, exactly one resolved,
    and ``legacy``'s need for ``gone`` was already broken.  The report must say
    so, and must agree with ``pip check`` after the install.
    """
    wheels_dir = tmp_path / "wheels"
    wheels_dir.mkdir()

    wheels = [
        create_basic_wheel_for_package(script, "lib", "1"),
        create_basic_wheel_for_package(script, "lib", "3"),
        create_basic_wheel_for_package(script, "olddep", "1"),
        create_basic_wheel_for_package(script, "app", "1.0", depends=["lib>=1"]),
        create_basic_wheel_for_package(
            script, "app", "2.0", depends=["lib>=3", "olddep<2"]
        ),
        create_basic_wheel_for_package(script, "plugin", "1.0", depends=["lib<2"]),
        create_basic_wheel_for_package(
            script, "legacy", "1.0", depends=["gone", "olddep"]
        ),
    ]
    for wheel in wheels:
        wheel.rename(wheels_dir / wheel.name)

    # Seed the pre-existing breakage.
    script.pip_install_local("lib==1", find_links=wheels_dir)
    script.pip_install_local("app==1.0", find_links=wheels_dir)
    script.pip_install_local("plugin==1.0", "--no-deps", find_links=wheels_dir)
    script.pip_install_local("legacy==1.0", "--no-deps", find_links=wheels_dir)

    check_before = script.pip("check", expect_error=True)
    assert_contains_expected_lines(
        check_before.stdout,
        [
            "legacy 1.0 requires gone, which is not installed.",
            "legacy 1.0 requires olddep, which is not installed.",
        ],
    )

    report_path = tmp_path / "report.json"
    result = script.pip_install_local(
        "--upgrade",
        "--dry-run",
        "--report",
        str(report_path),
        "app==2.0",
        find_links=wheels_dir,
    )
    assert "Would install app-2.0 lib-3 olddep-1" in result.stdout

    conflicts = json.loads(report_path.read_text(encoding="utf-8"))["conflicts"]

    # The upgrade introduces exactly one relation: plugin now clashes with lib.
    assert conflicts["introduced"] == [
        {
            "kind": "conflicting",
            "project": "plugin",
            "project_version": "1.0",
            "dependency": "lib",
            "requirements": ["lib<2"],
            "installed_version": "3",
        }
    ]
    # ... and resolves exactly one: legacy's need for olddep is now satisfied.
    assert conflicts["resolved"] == [
        {
            "kind": "missing",
            "project": "legacy",
            "project_version": "1.0",
            "dependency": "olddep",
            "requirements": ["olddep"],
            "installed_version": None,
        }
    ]
    # legacy's missing "gone" was broken before and still is.
    assert conflicts["already_present"] == [
        {
            "kind": "missing",
            "project": "legacy",
            "project_version": "1.0",
            "dependency": "gone",
            "requirements": ["gone"],
            "installed_version": None,
        }
    ]

    # --dry-run must not have touched the environment.
    assert script.pip("check", expect_error=True).stdout == check_before.stdout

    # Apply the upgrade and compare against `pip check`.
    script.pip_install_local("--upgrade", "app==2.0", find_links=wheels_dir)
    check_after = script.pip("check", expect_error=True)
    assert set(check_after.stdout.splitlines()) - {""} == {
        "legacy 1.0 requires gone, which is not installed.",
        "plugin 1.0 has requirement lib<2, but you have lib 3.",
    }

    def check_line(relation: dict[str, object]) -> str:
        req = relation["requirements"][0]  # type: ignore[index]
        if relation["kind"] == "missing":
            return (
                f"{relation['project']} {relation['project_version']} requires "
                f"{req}, which is not installed."
            )
        return (
            f"{relation['project']} {relation['project_version']} has requirement "
            f"{req}, but you have {relation['dependency']} "
            f"{relation['installed_version']}."
        )

    # Everything the report calls introduced or already present is what
    # `pip check` reports afterwards; nothing it calls resolved remains.
    surviving = conflicts["introduced"] + conflicts["already_present"]
    actual_lines = {line for line in check_after.stdout.splitlines() if line.strip()}
    assert {check_line(r) for r in surviving} == actual_lines
    assert not ({check_line(r) for r in conflicts["resolved"]} & actual_lines)


def test_report_omits_conflicts_without_conflict_checking(
    script: PipTestEnvironment, tmp_path: Path
) -> None:
    """``--no-warn-conflicts`` skips the comparison, so the report has no key."""
    wheels_dir = tmp_path / "wheels"
    wheels_dir.mkdir()
    for wheel in (
        create_basic_wheel_for_package(script, "app", "1.0"),
        create_basic_wheel_for_package(script, "lib", "1", depends=["gone"]),
    ):
        wheel.rename(wheels_dir / wheel.name)
    script.pip_install_local("lib==1", "--no-deps", find_links=wheels_dir)

    report_path = tmp_path / "report.json"
    script.pip_install_local(
        "--dry-run",
        "--no-warn-conflicts",
        "--report",
        str(report_path),
        "app",
        find_links=wheels_dir,
    )

    report = json.loads(report_path.read_text(encoding="utf-8"))
    assert "conflicts" not in report

    # With the check enabled, the pre-existing breakage shows up.
    script.pip_install_local(
        "--dry-run",
        "--report",
        str(report_path),
        "app",
        find_links=wheels_dir,
    )
    conflicts = json.loads(report_path.read_text(encoding="utf-8"))["conflicts"]
    assert conflicts["already_present"] == [
        {
            "kind": "missing",
            "project": "lib",
            "project_version": "1",
            "dependency": "gone",
            "requirements": ["gone"],
            "installed_version": None,
        }
    ]
