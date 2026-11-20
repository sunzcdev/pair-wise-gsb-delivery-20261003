from __future__ import annotations

import abc
import logging
from collections.abc import Iterable
from contextlib import AbstractContextManager as ContextManager
from typing import TYPE_CHECKING, Literal, Protocol

from pip._vendor.packaging.version import Version

from pip._internal.locations import get_scheme
from pip._internal.metadata import get_default_environment, get_environment
from pip._internal.utils.packaging import get_requirement

if TYPE_CHECKING:
    from pip._internal.req.req_install import InstallRequirement


logger = logging.getLogger(__name__)

BuildIsolationMode = Literal["off", "virtual", "venv"]

# The two phases in which build dependencies are installed into a build
# environment:
# - "overlay": the static ``build-system.requires`` from pyproject.toml, which
#   are installed before the backend can even be imported;
# - "normal": the dynamic requirements returned by the backend's
#   ``get_requires_for_build_*`` hooks, which are only known after the overlay
#   phase has completed.
BuildRequirementPhase = Literal["overlay", "normal"]


def _dedup(a: str, b: str) -> tuple[str] | tuple[str, str]:
    return (a, b) if a != b else (a,)


class Prefix:
    # TODO: simplify this data model when the legacy subprocess installer is removed
    def __init__(self, path: str, *, venv_executable: str | None = None) -> None:
        self.path = path
        self.setup = False
        scheme = get_scheme("", prefix=path)
        self.bin_dir = scheme.scripts
        self.lib_dirs = _dedup(scheme.purelib, scheme.platlib)
        self.venv_executable = venv_executable


class BuildEnvironmentInstaller(Protocol):
    """
    Interface for installing build dependencies into an isolated build
    environment.
    """

    def install(
        self,
        requirements: Iterable[str],
        prefix: Prefix,
        *,
        kind: str,
        for_req: InstallRequirement | None,
    ) -> None: ...


class BuildEnvironment(ContextManager[None], metaclass=abc.ABCMeta):
    """Creates and manages an isolated environment to install build deps"""

    lib_dirs: list[str]
    python_executable: str

    @abc.abstractmethod
    def __init__(self, installer: BuildEnvironmentInstaller): ...

    def check_requirements(
        self, reqs: Iterable[str]
    ) -> tuple[set[tuple[str, str]], set[str]]:
        """Return 2 sets:
        - conflicting requirements: set of (installed, wanted) reqs tuples
        - missing requirements: set of reqs
        """
        missing = set()
        conflicting = set()
        if reqs:
            env = (
                get_environment(self.lib_dirs)
                if hasattr(self, "lib_dirs")
                else get_default_environment()
            )
            for req_str in reqs:
                req = get_requirement(req_str)
                # We're explicitly evaluating with an empty extra value, since build
                # environments are not provided any mechanism to select specific extras.
                if req.marker is not None and not req.marker.evaluate({"extra": ""}):
                    continue
                dist = env.get_distribution(req.name)
                if not dist:
                    missing.add(req_str)
                    continue
                if isinstance(dist.version, Version):
                    installed_req_str = f"{req.name}=={dist.version}"
                else:
                    installed_req_str = f"{req.name}==={dist.version}"
                if not req.specifier.contains(dist.version, prereleases=True):
                    conflicting.add((installed_req_str, req_str))
                # FIXME: Consider direct URL?
        return conflicting, missing

    def check_requirements_for_phase(
        self,
        reqs: Iterable[str],
        phase: BuildRequirementPhase,
    ) -> tuple[set[tuple[str, str]], set[str]]:
        """Check requirements requested for a given build dependency phase.

        The first phase ("overlay") installs the static build requirements
        declared in pyproject.toml; the second phase ("normal") installs the
        requirements dynamically reported by the backend hooks. Before running
        either phase, the requirements must be checked against what is already
        present in the build environment, which may include a package selected
        by the previous phase.

        The returned conflict set maps each already selected version
        (``installed``) to the new constraint (``wanted``) that clashes with
        it, so the caller can report both explicitly. The caller is expected
        to abort on any conflict rather than install over the selection.

        When both the overlay and normal prefixes provide the same package,
        the metadata environment resolves it to the overlay (higher priority)
        one, so legitimate overlay priority is preserved.
        """
        conflicting, missing = self.check_requirements(reqs)
        for installed, wanted in sorted(conflicting):
            # The full, user-facing explanation (including both the selected
            # version and the clashing constraint) is raised by the caller;
            # here we only record the phase boundary at debug level.
            logger.debug(
                "Build dependency conflict in the %s phase: %s is already "
                "selected in the build environment and is incompatible with "
                "the new constraint %s.",
                phase,
                installed,
                wanted,
            )
        return conflicting, missing

    @abc.abstractmethod
    def install_requirements(
        self,
        requirements: Iterable[str],
        prefix_as_string: str,
        *,
        kind: str,
        for_req: InstallRequirement | None = None,
    ) -> None: ...

    def reset_install(self, prefix_as_string: str) -> None:
        """Discard the state recorded for an installation phase.

        This is called when an installation into the given prefix fails, so
        that a subsequent build cannot inherit the half-finished state of this
        attempt (for instance, a prefix marked as set up while only some of
        its packages were installed).
        """
        return None
