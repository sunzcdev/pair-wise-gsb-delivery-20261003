import re
from collections.abc import Iterable, Iterator
from typing import Any

from pip._vendor.packaging.dependency_groups import (
    DependencyGroupInclude,
    DependencyGroupResolver,
)
from pip._vendor.packaging.errors import ExceptionGroup
from pip._vendor.packaging.requirements import Requirement

from pip._internal.exceptions import InstallationError
from pip._internal.models.dependency_group import (
    DependencyGroupProvenance,
    ResolvedDependencyGroupRequirement,
)
from pip._internal.utils.compat import tomllib


def parse_dependency_groups(
    groups: list[tuple[str, str]],
) -> list[ResolvedDependencyGroupRequirement]:
    """
    Parse dependency groups data as provided via the CLI, in a `[path:]group` syntax.

    Each returned requirement carries a
    :class:`~pip._internal.models.dependency_group.DependencyGroupProvenance`
    describing the selected pyproject, the chain of included groups that was
    followed, and the marker in effect for the concrete requirement.

    Raises InstallationErrors if anything goes wrong.
    """
    resolvers = _build_resolvers(path for (path, _) in groups)
    return list(_resolve_all_groups(resolvers, groups))


def _resolve_all_groups(
    resolvers: dict[str, DependencyGroupResolver], groups: list[tuple[str, str]]
) -> Iterator[ResolvedDependencyGroupRequirement]:
    """
    Run all resolution, converting any error from `DependencyGroupResolver` into
    an InstallationError.
    """
    for path, groupname in groups:
        resolver = resolvers[path]
        try:
            yield from _expand_group(
                resolver,
                path=path,
                requested_group=groupname,
                include_chain=(groupname,),
                seen=set(),
            )
        except ExceptionGroup as eg:
            # Convert ExceptionGroup to a single InstallationError with all messages
            messages = [str(e) for e in eg.exceptions]
            raise InstallationError(
                f"[dependency-groups] resolution failed for '{groupname}' "
                f"from '{path}': {'; '.join(messages)}"
            ) from eg


def _expand_group(
    resolver: DependencyGroupResolver,
    *,
    path: str,
    requested_group: str,
    include_chain: tuple[str, ...],
    seen: set[str],
) -> Iterator[ResolvedDependencyGroupRequirement]:
    """
    Walk one resolved group, following ``include-group`` references recursively
    so that the full include chain is recorded on every emitted requirement.

    ``resolver.resolve()`` is used first so that malformed data, missing groups,
    and cyclic includes surface exactly as they did before; ``lookup()`` then
    provides the un-flattened structure needed for the provenance.
    """
    # Validates the whole expansion (including cycles) using packaging's own
    # logic before we attempt our own traversal.
    resolver.resolve(requested_group)

    normalized = _normalize_group_name(requested_group)
    if normalized in seen:
        # Defensive: resolver.resolve() above already reports cycles, so this
        # should be unreachable for user data.
        raise LookupError(
            f"Dependency group '{requested_group}' was revisited while "
            f"expanding {' -> '.join(include_chain)}"
        )
    seen = seen | {normalized}

    for item in resolver.lookup(requested_group):
        if isinstance(item, Requirement):
            provenance = DependencyGroupProvenance(
                source=path,
                requested_group=include_chain[0],
                include_chain=include_chain,
                marker=str(item.marker) if item.marker is not None else None,
            )
            yield ResolvedDependencyGroupRequirement(
                requirement=str(item), provenance=provenance
            )
        elif isinstance(item, DependencyGroupInclude):
            yield from _expand_group(
                resolver,
                path=path,
                requested_group=item.include_group,
                include_chain=(*include_chain, item.include_group),
                seen=seen,
            )
        else:  # pragma: no cover
            raise NotImplementedError(
                f"Invalid dependency group item after parse: {item}"
            )


def _build_resolvers(paths: Iterable[str]) -> dict[str, Any]:
    resolvers = {}
    for path in paths:
        if path in resolvers:
            continue

        pyproject = _load_pyproject(path)
        if "dependency-groups" not in pyproject:
            raise InstallationError(
                f"[dependency-groups] table was missing from '{path}'. "
                "Cannot resolve '--group' option."
            )
        raw_dependency_groups = pyproject["dependency-groups"]
        if not isinstance(raw_dependency_groups, dict):
            raise InstallationError(
                f"[dependency-groups] table was malformed in {path}. "
                "Cannot resolve '--group' option."
            )

        try:
            resolvers[path] = DependencyGroupResolver(raw_dependency_groups)
        except ExceptionGroup as eg:
            # Handle ExceptionGroup from resolver initialization
            messages = [str(e) for e in eg.exceptions]
            raise InstallationError(
                f"[dependency-groups] data was invalid in {path}: {'; '.join(messages)}"
            ) from eg

    return resolvers


def _normalize_group_name(name: str) -> str:
    # Match the normalization applied internally by packaging (PEP 735): runs
    # of "-", "_" and "." collapse to a single "-", lower-cased.
    return _NORMALIZE_PATTERN.sub("-", name).lower()


_NORMALIZE_PATTERN = re.compile(r"[-_.]+")


def _load_pyproject(path: str) -> dict[str, Any]:
    """
    This helper loads a pyproject.toml as TOML.

    It raises an InstallationError if the operation fails.
    """
    try:
        with open(path, "rb") as fp:
            return tomllib.load(fp)
    except FileNotFoundError:
        raise InstallationError(f"{path} not found. Cannot resolve '--group' option.")
    except tomllib.TOMLDecodeError as e:
        raise InstallationError(f"Error parsing {path}: {e}") from e
    except OSError as e:
        raise InstallationError(f"Error reading {path}: {e}") from e
