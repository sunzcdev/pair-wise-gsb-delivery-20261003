import pathlib
import sys

from tests.lib import (
    PipTestEnvironment,
    create_basic_wheel_for_package,
    create_test_package_with_setup,
)
from tests.lib.wheel import make_wheel


def test_new_resolver_conflict_requirements_file(
    tmpdir: pathlib.Path, script: PipTestEnvironment
) -> None:
    create_basic_wheel_for_package(script, "base", "1.0")
    create_basic_wheel_for_package(script, "base", "2.0")
    create_basic_wheel_for_package(
        script,
        "pkga",
        "1.0",
        depends=["base==1.0"],
    )
    create_basic_wheel_for_package(
        script,
        "pkgb",
        "1.0",
        depends=["base==2.0"],
    )

    req_file = tmpdir.joinpath("requirements.txt")
    req_file.write_text("pkga\npkgb")

    result = script.pip(
        "install",
        "--no-cache-dir",
        "--no-index",
        "--find-links",
        script.scratch_path,
        "-r",
        req_file,
        expect_error=True,
    )

    message = "package versions have conflicting dependencies"
    assert message in result.stderr, str(result)


def test_new_resolver_conflict_constraints_file(
    tmpdir: pathlib.Path, script: PipTestEnvironment
) -> None:
    create_basic_wheel_for_package(script, "pkg", "1.0")

    constraints_file = tmpdir.joinpath("constraints.txt")
    constraints_file.write_text("pkg!=1.0")

    result = script.pip(
        "install",
        "--no-cache-dir",
        "--no-index",
        "--find-links",
        script.scratch_path,
        "-c",
        constraints_file,
        "pkg==1.0",
        expect_error=True,
    )

    assert "ResolutionImpossible" in result.stderr, str(result)

    message = "The user requested (constraint) pkg!=1.0"
    assert message in result.stdout, str(result)


def test_new_resolver_requires_python_error(script: PipTestEnvironment) -> None:
    compatible_python = f">={sys.version_info.major}.{sys.version_info.minor}"
    incompatible_python = f"<{sys.version_info.major}.{sys.version_info.minor}"

    pkga = create_test_package_with_setup(
        script,
        name="pkga",
        version="1.0",
        python_requires=compatible_python,
    )
    pkgb = create_test_package_with_setup(
        script,
        name="pkgb",
        version="1.0",
        python_requires=incompatible_python,
    )

    # This always fails because pkgb can never be satisfied.
    result = script.pip(
        "install", "--no-build-isolation", "--no-index", pkga, pkgb, expect_error=True
    )

    # The error message should mention the Requires-Python: value causing the
    # conflict, not the compatible one.
    assert incompatible_python in result.stderr, str(result)
    assert compatible_python not in result.stderr, str(result)


def test_new_resolver_checks_requires_python_before_dependencies(
    script: PipTestEnvironment,
) -> None:
    incompatible_python = f"<{sys.version_info.major}.{sys.version_info.minor}"

    pkg_dep = create_basic_wheel_for_package(
        script,
        name="pkg-dep",
        version="1",
    )
    create_basic_wheel_for_package(
        script,
        name="pkg-root",
        version="1",
        # Refer the dependency by URL to prioritise it as much as possible,
        # to test that Requires-Python is *still* inspected first.
        depends=[f"pkg-dep@{pathlib.Path(pkg_dep).as_uri()}"],
        requires_python=incompatible_python,
    )

    result = script.pip(
        "install",
        "--no-cache-dir",
        "--no-index",
        "--find-links",
        script.scratch_path,
        "pkg-root",
        expect_error=True,
    )

    # Resolution should fail because of pkg-root's Requires-Python.
    # This is done before dependencies so pkg-dep should never be pulled.
    assert incompatible_python in result.stderr, str(result)
    # Setuptools produces wheels with normalized names.
    assert "pkg_dep" not in result.stderr, str(result)
    assert "pkg_dep" not in result.stdout, str(result)


def test_new_resolver_no_versions_available_hint(script: PipTestEnvironment) -> None:
    """
    Test hint that no package candidate is available at all,
    when ResolutionImpossible occurs.
    """
    wheel_house = script.scratch_path.joinpath("wheelhouse")
    wheel_house.mkdir()

    incompatible_dep_wheel = make_wheel(
        name="incompatible-dep",
        version="1.0.0",
        wheel_metadata_updates={"Tag": ["py3-none-fakeplat"]},
    )
    incompatible_dep_wheel.save_to(
        wheel_house.joinpath("incompatible_dep-1.0.0-py3-none-fakeplat.whl")
    )

    # Create multiple versions of a package that depend on the incompatible dependency
    requesting_pkg_v1 = make_wheel(
        name="requesting-pkg",
        version="1.0.0",
        metadata_updates={"Requires-Dist": ["incompatible-dep==1.0.0"]},
    )
    requesting_pkg_v1.save_to(
        wheel_house.joinpath("requesting_pkg-1.0.0-py2.py3-none-any.whl")
    )

    requesting_pkg_v2 = make_wheel(
        name="requesting-pkg",
        version="2.0.0",
        metadata_updates={"Requires-Dist": ["incompatible-dep==1.0.0"]},
    )
    requesting_pkg_v2.save_to(
        wheel_house.joinpath("requesting_pkg-2.0.0-py2.py3-none-any.whl")
    )

    # Attempt to install the requesting package
    result = script.pip(
        "install",
        "--no-cache-dir",
        "--no-index",
        "--find-links",
        str(wheel_house),
        "requesting-pkg",
        expect_error=True,
    )

    # Check that ResolutionImpossible error occurred
    assert "ResolutionImpossible" in result.stderr, str(result)

    # Check that the new hint message is present
    assert (
        "Additionally, some packages in these conflicts have no "
        "matching distributions available for your environment:\n"
        "    incompatible-dep\n" in result.stdout
    ), str(result)


def test_new_resolver_conflict_reports_parent_chains(
    script: PipTestEnvironment,
) -> None:
    """
    Each conflicting branch is explained with the parent dependency chain
    leading back to the root requirement that introduced it.
    """
    create_basic_wheel_for_package(script, "base", "1.0")
    create_basic_wheel_for_package(script, "base", "2.0")
    create_basic_wheel_for_package(script, "pkga", "1.0", depends=["base==1.0"])
    create_basic_wheel_for_package(script, "pkgb", "1.0", depends=["base==2.0"])
    create_basic_wheel_for_package(script, "mida", "1.0", depends=["pkga"])
    create_basic_wheel_for_package(script, "midb", "1.0", depends=["pkgb"])

    result = script.pip(
        "install",
        "--no-cache-dir",
        "--no-index",
        "--find-links",
        script.scratch_path,
        "mida",
        "midb",
        expect_error=True,
    )

    assert "ResolutionImpossible" in result.stderr, str(result)
    # Both genuinely conflicting branches, each traced back to its root.
    assert "pkga 1.0 depends on base==1.0" in result.stdout, str(result)
    assert "mida depends on pkga" in result.stdout, str(result)
    assert "The user requested mida" in result.stdout, str(result)
    assert "pkgb 1.0 depends on base==2.0" in result.stdout, str(result)
    assert "midb depends on pkgb" in result.stdout, str(result)
    assert "The user requested midb" in result.stdout, str(result)


def test_new_resolver_conflict_separates_requires_python_skips(
    script: PipTestEnvironment,
) -> None:
    """
    Releases rejected only because of Requires-Python are listed separately
    instead of being reported as missing distributions.
    """
    create_basic_wheel_for_package(script, "base", "1.0")
    create_basic_wheel_for_package(script, "base", "2.0")
    create_basic_wheel_for_package(script, "base", "3.0", requires_python="<0.1")
    create_basic_wheel_for_package(script, "pkga", "1.0", depends=["base<2"])
    create_basic_wheel_for_package(script, "pkgb", "1.0", depends=["base>=2"])

    result = script.pip(
        "install",
        "--no-cache-dir",
        "--no-index",
        "--find-links",
        script.scratch_path,
        "pkga",
        "pkgb",
        expect_error=True,
    )

    assert "ResolutionImpossible" in result.stderr, str(result)
    assert (
        "The following versions were ignored because they require a "
        "different Python version:"
    ) in result.stdout, str(result)
    assert "base: 3.0 Requires-Python <0.1" in result.stdout, str(result)
    # base has usable releases, so it is not "missing".
    assert "no matching distributions available" not in result.stdout, str(result)


def test_new_resolver_conflict_ignores_unmatched_conditions(
    script: PipTestEnvironment,
) -> None:
    """
    Platform-incompatible wheels and marker-gated dependencies that do not
    match are excluded from the conflict attribution.
    """
    create_basic_wheel_for_package(script, "base", "1.0")
    create_basic_wheel_for_package(script, "base", "2.0")
    create_basic_wheel_for_package(script, "pkga", "1.0", depends=["base==1.0"])
    create_basic_wheel_for_package(script, "pkgb", "1.0", depends=["base==2.0"])

    # A package whose only other release is for an incompatible platform.
    platform_wheel = make_wheel(
        name="distractor",
        version="9.0",
        wheel_metadata_updates={"Tag": ["py3-none-fakeplat"]},
    )
    platform_wheel.save_to(
        script.scratch_path.joinpath("distractor-9.0-py3-none-fakeplat.whl")
    )
    create_basic_wheel_for_package(script, "distractor", "1.0")

    # A dependency gated behind a marker that cannot match this platform.
    marker_wheel = make_wheel(
        name="markerpkg",
        version="1.0",
        metadata_updates={
            "Requires-Dist": ["base==42; platform_machine == 'no-such-arch'"]
        },
    )
    marker_wheel.save_to(
        script.scratch_path.joinpath("markerpkg-1.0-py2.py3-none-any.whl")
    )

    result = script.pip(
        "install",
        "--no-cache-dir",
        "--no-index",
        "--find-links",
        script.scratch_path,
        "pkga",
        "pkgb",
        "distractor",
        "markerpkg",
        expect_error=True,
    )

    assert "ResolutionImpossible" in result.stderr, str(result)
    # The causal chain contains only the two genuinely conflicting branches.
    assert "pkga 1.0 depends on base==1.0" in result.stdout, str(result)
    assert "pkgb 1.0 depends on base==2.0" in result.stdout, str(result)
    assert "base==42" not in result.stdout, str(result)
    _, _, conflict_detail = result.stdout.partition("The conflict is caused by:")
    assert "distractor" not in conflict_detail, str(result)


def test_new_resolver_conflict_ignores_unrequested_extras(
    script: PipTestEnvironment,
) -> None:
    """
    Dependencies introduced by extras the user did not request never appear
    in the conflict's causal chain.
    """
    create_basic_wheel_for_package(script, "base", "1.0")
    create_basic_wheel_for_package(script, "base", "2.0")
    create_basic_wheel_for_package(
        script,
        "pkga",
        "1.0",
        depends=["base==1.0"],
        extras={"feature": ["base==9.9"]},
    )
    create_basic_wheel_for_package(script, "pkgb", "1.0", depends=["base==2.0"])

    result = script.pip(
        "install",
        "--no-cache-dir",
        "--no-index",
        "--find-links",
        script.scratch_path,
        "pkga",
        "pkgb",
        expect_error=True,
    )

    assert "ResolutionImpossible" in result.stderr, str(result)
    assert "pkga 1.0 depends on base==1.0" in result.stdout, str(result)
    assert "pkgb 1.0 depends on base==2.0" in result.stdout, str(result)
    assert "base==9.9" not in result.stdout, str(result)
    assert "feature" not in result.stdout, str(result)
