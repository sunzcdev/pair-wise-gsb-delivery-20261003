"""Functional tests for ``--find-links-scope`` requirements file directives.

Two local wheelhouse directories each provide a package that depends on a
shared dependency; scoped find-links declarations state which directory each
include is expected to use.
"""

from __future__ import annotations

import os
import pathlib
import textwrap

from tests.lib import PipTestEnvironment, create_basic_wheel_for_package


def _add_to_wheelhouse(
    script: PipTestEnvironment,
    wheelhouse: pathlib.Path,
    name: str,
    version: str,
    depends: list[str] | None = None,
) -> None:
    wheel = create_basic_wheel_for_package(script, name, version, depends=depends)
    wheel.replace(wheelhouse / wheel.name)


def _write_project(script: PipTestEnvironment, *, shared_in_b: bool) -> pathlib.Path:
    """Create two wheelhouses with a shared dependency and a requirements
    file that declares a find-links scope per include."""
    scratch = script.scratch_path
    wheelhouse_a = scratch / "wheelhouse-a"
    wheelhouse_a.mkdir()
    wheelhouse_b = scratch / "wheelhouse-b"
    wheelhouse_b.mkdir()

    _add_to_wheelhouse(script, wheelhouse_a, "pkg-a", "1.0", depends=["shared"])
    _add_to_wheelhouse(script, wheelhouse_b, "pkg-b", "1.0", depends=["shared"])
    _add_to_wheelhouse(script, wheelhouse_a, "shared", "1.0")
    if shared_in_b:
        _add_to_wheelhouse(script, wheelhouse_b, "shared", "1.0")

    scratch.joinpath("reqs-a.txt").write_text("pkg-a\n")
    scratch.joinpath("reqs-b.txt").write_text("pkg-b\n")
    requirements = scratch / "requirements.txt"
    requirements.write_text(
        textwrap.dedent(
            """\
            --find-links-scope ./wheelhouse-a
            -r reqs-a.txt
            --end-find-links-scope
            --find-links-scope ./wheelhouse-b
            -r reqs-b.txt
            --end-find-links-scope
            """
        )
    )
    return requirements


def test_scoped_find_links_shared_dependency(script: PipTestEnvironment) -> None:
    """The shared dependency is available from both declared sources, so
    installation succeeds."""
    requirements = _write_project(script, shared_in_b=True)

    script.pip("install", "--no-index", "-r", os.fspath(requirements))

    script.assert_installed(pkg_a="1.0", pkg_b="1.0", shared="1.0")


def test_scoped_find_links_shared_dependency_missing_from_one_source(
    script: PipTestEnvironment,
) -> None:
    """The shared dependency only exists in wheelhouse-a; the restriction
    introduced for wheelhouse-b cannot be satisfied and the error reports
    where that restriction was introduced."""
    requirements = _write_project(script, shared_in_b=False)

    result = script.pip(
        "install",
        "--no-index",
        "-r",
        os.fspath(requirements),
        expect_error=True,
    )

    assert result.returncode != 0
    assert "Cannot install 'shared'" in result.stderr
    assert "wheelhouse-b" in result.stderr
    # The location where the unsatisfiable restriction was introduced.
    assert "line 4 of" in result.stderr
    assert "requirements.txt" in result.stderr
    # The satisfiable restriction is not reported.
    assert "line 1 of" not in result.stderr


def test_scope_ends_before_unscoped_requirement(script: PipTestEnvironment) -> None:
    """After ``--end-find-links-scope`` the outer (unscoped) state is
    restored, so a later requirement may come from any find-links source."""
    scratch = script.scratch_path
    wheelhouse_a = scratch / "wheelhouse-a"
    wheelhouse_a.mkdir()
    wheelhouse_b = scratch / "wheelhouse-b"
    wheelhouse_b.mkdir()

    _add_to_wheelhouse(script, wheelhouse_a, "pkg-a", "1.0")
    _add_to_wheelhouse(script, wheelhouse_b, "pkg-b", "1.0")

    requirements = scratch / "requirements.txt"
    requirements.write_text(
        textwrap.dedent(
            """\
            --find-links-scope ./wheelhouse-a
            pkg-a
            --end-find-links-scope
            --find-links ./wheelhouse-b
            pkg-b
            """
        )
    )

    script.pip("install", "--no-index", "-r", os.fspath(requirements))

    script.assert_installed(pkg_a="1.0", pkg_b="1.0")
