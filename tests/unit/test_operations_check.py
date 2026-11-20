from pip._vendor.packaging.requirements import Requirement
from pip._vendor.packaging.utils import NormalizedName, canonicalize_name
from pip._vendor.packaging.version import Version

from pip._internal.operations.check import (
    PackageDetails,
    PackageSet,
    _create_whitelist,
    check_diff_to_dict,
    check_package_set,
    diff_check_results,
)


def package_details(*dependencies: str, version: str = "1") -> PackageDetails:
    return PackageDetails(
        Version(version),
        [Requirement(dependency) for dependency in dependencies],
    )


def installed_package_details(
    *dependencies: str, version: str = "1"
) -> PackageDetails:
    """Build details the way ``BaseDistribution.iter_dependencies`` does.

    Installed metadata is read with ``extras=()``, which drops requirements
    guarded by an extra that was not requested and by a marker that is false in
    the current environment.  Real package sets therefore only ever contain
    requirements that are in effect, which is the rule ``pip check`` relies on.
    """
    active: list[Requirement] = []
    for dependency in dependencies:
        req = Requirement(dependency)
        if req.marker is None or req.marker.evaluate({"extra": ""}):
            active.append(req)
    return PackageDetails(Version(version), active)


def test_create_whitelist_is_not_order_dependent() -> None:
    root = canonicalize_name("root")
    middle = canonicalize_name("middle")
    leaf = canonicalize_name("leaf")
    expected: set[NormalizedName] = {leaf, middle}

    package_set_with_dependent_first: PackageSet = {
        root: package_details("middle"),
        middle: package_details("leaf"),
        leaf: package_details(),
    }
    package_set_with_dependency_first: PackageSet = {
        leaf: package_details(),
        middle: package_details("leaf"),
        root: package_details("middle"),
    }

    assert _create_whitelist({leaf}, package_set_with_dependent_first) == expected
    assert _create_whitelist({leaf}, package_set_with_dependency_first) == expected


def test_diff_reports_newly_introduced_relation() -> None:
    before: PackageSet = {
        "root": package_details("leaf"),
        "leaf": package_details(version="2.0"),
    }
    after: PackageSet = {
        "root": package_details("leaf>=3.0"),
        "leaf": package_details(version="2.0"),
    }

    diff = diff_check_results(
        before, check_package_set(before), after, check_package_set(after)
    )

    assert [r.key for r in diff.introduced] == [
        ("conflicting", "root", "leaf"),
    ]
    assert diff.resolved == ()
    assert diff.already_present == ()
    assert diff.introduced[0].installed_version == Version("2.0")
    assert [str(req) for req in diff.introduced[0].requirements] == ["leaf>=3.0"]


def test_diff_keeps_old_breakage_when_the_constraint_is_edited() -> None:
    before: PackageSet = {
        "root": package_details("leaf<1.0"),
        "leaf": package_details(version="2.0"),
    }
    after: PackageSet = {
        "root": package_details("leaf<1.5"),
        "leaf": package_details(version="2.0"),
    }

    diff = diff_check_results(
        before, check_package_set(before), after, check_package_set(after)
    )

    assert diff.introduced == ()
    assert diff.resolved == ()
    assert [r.key for r in diff.already_present] == [
        ("conflicting", "root", "leaf"),
    ]
    # ``already_present`` is described with the planned state.
    assert [str(req) for req in diff.already_present[0].requirements] == ["leaf<1.5"]


def test_diff_keeps_old_breakage_when_the_broken_version_moves() -> None:
    before: PackageSet = {
        "root": package_details("leaf>=9"),
        "leaf": package_details(version="1"),
    }
    after: PackageSet = {
        "root": package_details("leaf>=9"),
        "leaf": package_details(version="2"),
    }

    diff = diff_check_results(
        before, check_package_set(before), after, check_package_set(after)
    )

    assert diff.introduced == ()
    assert diff.resolved == ()
    assert [r.key for r in diff.already_present] == [
        ("conflicting", "root", "leaf"),
    ]
    assert diff.already_present[0].installed_version == Version("2")


def test_diff_reports_resolved_relation() -> None:
    before: PackageSet = {
        "root": package_details("leaf>=2"),
        "leaf": package_details(version="1"),
    }
    after: PackageSet = {
        "root": package_details("leaf>=2"),
        "leaf": package_details(version="2"),
    }

    diff = diff_check_results(
        before, check_package_set(before), after, check_package_set(after)
    )

    assert [r.key for r in diff.resolved] == [("conflicting", "root", "leaf")]
    # ``resolved`` is described with the pre-install state, since it is gone.
    assert diff.resolved[0].installed_version == Version("1")
    assert diff.introduced == ()
    assert diff.already_present == ()


def test_diff_treats_missing_and_conflicting_as_distinct_relations() -> None:
    # ``leaf`` moves from absent to installed-but-too-old.
    before: PackageSet = {"root": package_details("leaf>=2")}
    after: PackageSet = {
        "root": package_details("leaf>=2"),
        "leaf": package_details(version="1"),
    }

    diff = diff_check_results(
        before, check_package_set(before), after, check_package_set(after)
    )

    assert [r.key for r in diff.resolved] == [("missing", "root", "leaf")]
    assert [r.key for r in diff.introduced] == [("conflicting", "root", "leaf")]
    assert diff.already_present == ()


def test_diff_ignores_inactive_markers_and_extras() -> None:
    # Requirements are filtered by ``iter_dependencies`` before they reach the
    # package set: markers are evaluated with ``extra == ""``, so an optional or
    # inactive dependency is never part of a relation.
    package_set: PackageSet = {
        "root": installed_package_details(
            "leaf>=2",
            'leaf>=3; python_version < "2"',
            'leaf>=4; extra == "test"',
        ),
        "leaf": package_details(version="1"),
    }
    result = check_package_set(package_set)

    diff = diff_check_results(package_set, result, package_set, result)

    assert diff.introduced == ()
    assert diff.resolved == ()
    assert len(diff.already_present) == 1
    relation = diff.already_present[0]
    # Only the requirement actually in effect is kept.
    assert [str(req) for req in relation.requirements] == ["leaf>=2"]


def test_diff_of_identical_package_sets_is_all_already_present() -> None:
    package_set: PackageSet = {"root": package_details("leaf")}
    result = check_package_set(package_set)

    diff = diff_check_results(package_set, result, package_set, result)

    assert [r.key for r in diff.already_present] == [("missing", "root", "leaf")]
    assert diff.already_present[0].installed_version is None
    assert diff.introduced == ()
    assert diff.resolved == ()


def test_diff_to_dict_is_json_shaped() -> None:
    before: PackageSet = {"root": package_details("leaf>=2")}
    after: PackageSet = {
        "root": package_details("leaf>=2"),
        "leaf": package_details(version="1"),
    }
    diff = diff_check_results(
        before, check_package_set(before), after, check_package_set(after)
    )

    assert check_diff_to_dict(diff) == {
        "introduced": [
            {
                "kind": "conflicting",
                "project": "root",
                "project_version": "1",
                "dependency": "leaf",
                "requirements": ["leaf>=2"],
                "installed_version": "1",
            }
        ],
        "resolved": [
            {
                "kind": "missing",
                "project": "root",
                "project_version": "1",
                "dependency": "leaf",
                "requirements": ["leaf>=2"],
                "installed_version": None,
            }
        ],
        "already_present": [],
    }
