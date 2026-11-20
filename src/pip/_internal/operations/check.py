"""Validation of dependencies of packages"""

from __future__ import annotations

import logging
from collections.abc import Callable, Generator, Iterable, Iterator
from contextlib import suppress
from email.parser import Parser
from functools import reduce
from typing import (
    Any,
    Literal,
    NamedTuple,
)

from pip._vendor.packaging.requirements import Requirement
from pip._vendor.packaging.tags import Tag, parse_tag
from pip._vendor.packaging.utils import NormalizedName, canonicalize_name
from pip._vendor.packaging.version import Version

from pip._internal.distributions import make_distribution_for_install_requirement
from pip._internal.metadata import get_default_environment
from pip._internal.metadata.base import BaseDistribution
from pip._internal.req.req_install import InstallRequirement

logger = logging.getLogger(__name__)


class PackageDetails(NamedTuple):
    version: Version
    dependencies: list[Requirement]


# Shorthands
PackageSet = dict[NormalizedName, PackageDetails]
Missing = tuple[NormalizedName, Requirement]
Conflicting = tuple[NormalizedName, Version, Requirement]

MissingDict = dict[NormalizedName, list[Missing]]
ConflictingDict = dict[NormalizedName, list[Conflicting]]
CheckResult = tuple[MissingDict, ConflictingDict]

# A ``missing`` relation is one where the required project is not installed at
# all; a ``conflicting`` one is where it is installed at a version the
# requirement rejects.
ConflictKind = Literal["missing", "conflicting"]
RelationKey = tuple[ConflictKind, NormalizedName, NormalizedName]


class BrokenRelation(NamedTuple):
    """One unsatisfied dependency edge of a package set.

    A relation is identified by the depending project, the required project and
    the way it is broken -- deliberately *not* by the version or the specifier
    involved.  This keeps a long-standing breakage classified as such when an
    upgrade merely edits the constraint or changes the version that happens to
    violate it.
    """

    kind: ConflictKind
    project_name: NormalizedName
    project_version: Version
    dependency_name: NormalizedName
    #: Every active requirement from ``project_name`` on ``dependency_name``
    #: that ``check_package_set`` flagged.  Usually exactly one.
    requirements: tuple[Requirement, ...]
    #: The version found for ``dependency_name``, or ``None`` when missing.
    installed_version: Version | None

    @property
    def key(self) -> RelationKey:
        return (self.kind, self.project_name, self.dependency_name)


class CheckDiff(NamedTuple):
    """Broken relations grouped by how an installation changed them."""

    introduced: tuple[BrokenRelation, ...]
    resolved: tuple[BrokenRelation, ...]
    already_present: tuple[BrokenRelation, ...]


class ConflictDetails(NamedTuple):
    #: The package set as it would be after the installation.
    package_set: PackageSet
    #: Broken relations worth warning about, i.e. those reachable from the
    #: packages being installed.  This keeps the existing console behaviour.
    check_result: CheckResult
    #: Broken relations of the whole environment, before vs. after the
    #: installation.  This is what the ``--report`` output exposes, so that it
    #: can be compared with ``pip check`` run after the installation.
    diff: CheckDiff


def create_package_set_from_installed() -> tuple[PackageSet, bool]:
    """Converts a list of distributions into a PackageSet."""
    package_set = {}
    problems = False
    env = get_default_environment()
    for dist in env.iter_installed_distributions(local_only=False, skip=()):
        name = dist.canonical_name
        try:
            dependencies = list(dist.iter_dependencies())
            package_set[name] = PackageDetails(dist.version, dependencies)
        except (OSError, ValueError) as e:
            # Don't crash on unreadable or broken metadata.
            logger.warning("Error parsing dependencies of %s: %s", name, e)
            problems = True
    return package_set, problems


def check_package_set(
    package_set: PackageSet, should_ignore: Callable[[str], bool] | None = None
) -> CheckResult:
    """Check if a package set is consistent

    If should_ignore is passed, it should be a callable that takes a
    package name and returns a boolean.
    """

    missing = {}
    conflicting = {}

    for package_name, package_detail in package_set.items():
        # Info about dependencies of package_name
        missing_deps: set[Missing] = set()
        conflicting_deps: set[Conflicting] = set()

        if should_ignore and should_ignore(package_name):
            continue

        for req in package_detail.dependencies:
            name = canonicalize_name(req.name)

            # Check if it's missing
            if name not in package_set:
                missed = True
                if req.marker is not None:
                    missed = req.marker.evaluate({"extra": ""})
                if missed:
                    missing_deps.add((name, req))
                continue

            # Check if there's a conflict
            version = package_set[name].version
            if not req.specifier.contains(version, prereleases=True):
                conflicting_deps.add((name, version, req))

        if missing_deps:
            missing[package_name] = sorted(missing_deps, key=str)
        if conflicting_deps:
            conflicting[package_name] = sorted(conflicting_deps, key=str)

    return missing, conflicting


def _iter_broken_relations(
    package_set: PackageSet, check_result: CheckResult
) -> Iterator[BrokenRelation]:
    """Enumerate the broken dependency edges of an already checked package set.

    Every ``(kind, project, dependency)`` triple is reported once, together with
    all the requirements that flagged it.  Neither the specifier nor the version
    that happens to violate it takes part in the identity of a relation, so
    editing a constraint or bumping a version keeps an old breakage classified
    as old.  ``missing`` and ``conflicting`` stay distinct: a dependency going
    from absent to installed-but-incompatible is reported as one relation being
    fixed and another one appearing, which is what changed for the user.
    """
    missing, conflicting = check_result

    missing_by_edge: dict[
        tuple[NormalizedName, NormalizedName], list[Requirement]
    ] = {}
    for project_name, missing_deps in missing.items():
        for dependency_name, req in missing_deps:
            missing_by_edge.setdefault((project_name, dependency_name), []).append(req)

    for (project_name, dependency_name), reqs in missing_by_edge.items():
        yield BrokenRelation(
            kind="missing",
            project_name=project_name,
            project_version=package_set[project_name].version,
            dependency_name=dependency_name,
            requirements=tuple(sorted(set(reqs), key=str)),
            installed_version=None,
        )

    conflicting_by_edge: dict[
        tuple[NormalizedName, NormalizedName], list[Requirement]
    ] = {}
    for project_name, conflicting_deps in conflicting.items():
        for dependency_name, _installed_version, req in conflicting_deps:
            conflicting_by_edge.setdefault((project_name, dependency_name), []).append(
                req
            )

    for (project_name, dependency_name), reqs in conflicting_by_edge.items():
        yield BrokenRelation(
            kind="conflicting",
            project_name=project_name,
            project_version=package_set[project_name].version,
            dependency_name=dependency_name,
            requirements=tuple(sorted(set(reqs), key=str)),
            installed_version=package_set[dependency_name].version,
        )


def _relation_sort_key(relation: BrokenRelation) -> tuple[str, str, str]:
    return (relation.project_name, relation.dependency_name, relation.kind)


def diff_check_results(
    before_package_set: PackageSet,
    before_check_result: CheckResult,
    after_package_set: PackageSet,
    after_check_result: CheckResult,
) -> CheckDiff:
    """Compare two checked package sets edge by edge.

    ``after`` is expected to be the result of applying the planned installation
    on top of ``before``.  Both sides must be described by the same package set
    contents, i.e. checked without a ``should_ignore`` filter, otherwise the two
    relation sets are not comparable.
    """
    before = {
        relation.key: relation
        for relation in _iter_broken_relations(before_package_set, before_check_result)
    }
    after = {
        relation.key: relation
        for relation in _iter_broken_relations(after_package_set, after_check_result)
    }

    return CheckDiff(
        # Relations only present once the plan is applied: described with the
        # versions and requirements the user is about to get.
        introduced=tuple(
            sorted(
                (after[key] for key in after.keys() - before.keys()),
                key=_relation_sort_key,
            )
        ),
        # Relations only present before: described with the versions and
        # requirements that were installed, since the plan removes them.
        resolved=tuple(
            sorted(
                (before[key] for key in before.keys() - after.keys()),
                key=_relation_sort_key,
            )
        ),
        # Relations present on both sides, described with the planned state.
        already_present=tuple(
            sorted(
                (after[key] for key in after.keys() & before.keys()),
                key=_relation_sort_key,
            )
        ),
    )


def _relation_to_dict(relation: BrokenRelation) -> dict[str, Any]:
    return {
        "kind": relation.kind,
        "project": relation.project_name,
        "project_version": str(relation.project_version),
        "dependency": relation.dependency_name,
        "requirements": [str(req) for req in relation.requirements],
        "installed_version": (
            None
            if relation.installed_version is None
            else str(relation.installed_version)
        ),
    }


def check_diff_to_dict(
    diff: CheckDiff,
) -> dict[str, list[dict[str, Any]]]:
    """Serialize a :class:`CheckDiff` into a JSON-compatible mapping."""
    return {
        "introduced": [_relation_to_dict(relation) for relation in diff.introduced],
        "resolved": [_relation_to_dict(relation) for relation in diff.resolved],
        "already_present": [
            _relation_to_dict(relation) for relation in diff.already_present
        ],
    }


def _filter_check_result(
    check_result: CheckResult, keep: Callable[[NormalizedName], bool]
) -> CheckResult:
    """Restrict a result to the projects accepted by ``keep``.

    ``check_package_set`` only reports dependencies of the projects it is asked
    not to ignore, so filtering its output on the project name is equivalent to
    running it again with the corresponding ``should_ignore`` predicate.
    """
    missing, conflicting = check_result
    return (
        {name: deps for name, deps in missing.items() if keep(name)},
        {name: deps for name, deps in conflicting.items() if keep(name)},
    )


def check_install_conflicts(to_install: list[InstallRequirement]) -> ConflictDetails:
    """For checking if the dependency graph would be consistent after \
    installing given requirements
    """
    # Start from the current state
    package_set, _ = create_package_set_from_installed()
    # Snapshot the pre-install state before ``_simulate_installation_of``
    # mutates it in place.
    before_package_set = package_set.copy()

    # Install packages
    would_be_installed = _simulate_installation_of(to_install, package_set)

    # Only warn about directly-dependent packages; create a whitelist of them
    whitelist = _create_whitelist(would_be_installed, package_set)

    # The report compares the whole environment, so that its "after" side lines
    # up with what ``pip check`` reports once the installation has happened.
    before_check_result = check_package_set(before_package_set)
    after_check_result = check_package_set(package_set)

    return ConflictDetails(
        package_set=package_set,
        check_result=_filter_check_result(
            after_check_result, lambda name: name in whitelist
        ),
        diff=diff_check_results(
            before_package_set,
            before_check_result,
            package_set,
            after_check_result,
        ),
    )


def check_unsupported(
    packages: Iterable[BaseDistribution],
    supported_tags: Iterable[Tag],
) -> Generator[BaseDistribution, None, None]:
    for p in packages:
        with suppress(FileNotFoundError):
            wheel_file = p.read_text("WHEEL")
            wheel_tags: frozenset[Tag] = reduce(
                frozenset.union,
                map(parse_tag, Parser().parsestr(wheel_file).get_all("Tag", [])),
                frozenset(),
            )
            if wheel_tags.isdisjoint(supported_tags):
                yield p


def _simulate_installation_of(
    to_install: list[InstallRequirement], package_set: PackageSet
) -> set[NormalizedName]:
    """Computes the version of packages after installing to_install."""
    # Keep track of packages that were installed
    installed = set()

    # Modify it as installing requirement_set would (assuming no errors)
    for inst_req in to_install:
        abstract_dist = make_distribution_for_install_requirement(inst_req)
        dist = abstract_dist.get_metadata_distribution()
        name = dist.canonical_name
        package_set[name] = PackageDetails(dist.version, list(dist.iter_dependencies()))

        installed.add(name)

    return installed


def _create_whitelist(
    would_be_installed: set[NormalizedName], package_set: PackageSet
) -> set[NormalizedName]:
    packages_affected = set(would_be_installed)

    for package_name in package_set:
        if package_name in packages_affected:
            continue

        for req in package_set[package_name].dependencies:
            if canonicalize_name(req.name) in would_be_installed:
                packages_affected.add(package_name)
                break

    return packages_affected
