from __future__ import annotations

import collections
import logging
import os
from collections.abc import Container, Generator, Iterable
from dataclasses import dataclass, field
from typing import NamedTuple

from pip._vendor.packaging.utils import NormalizedName, canonicalize_name
from pip._vendor.packaging.version import InvalidVersion

from pip._internal.exceptions import BadCommand, InstallationError
from pip._internal.metadata import BaseDistribution, get_environment
from pip._internal.models.direct_url import DirectUrl
from pip._internal.req.constructors import (
    install_req_from_editable,
    install_req_from_line,
)
from pip._internal.req.req_file import COMMENT_RE
from pip._internal.utils.direct_url_helpers import direct_url_as_pep440_direct_reference
from pip._internal.utils.urls import url_to_path

logger = logging.getLogger(__name__)


class _EditableInfo(NamedTuple):
    requirement: str
    comments: list[str]


def _relative_path_for_workspace(path: str, workspace: str | None) -> str | None:
    """Return *path* as a relative path if it lies inside *workspace*.

    The result uses forward slashes and is prefixed with ``./`` when needed,
    so that it is recognizable as a relative path in a requirements file.
    Return ``None`` if *path* is outside *workspace* (or on a different
    drive, on Windows).
    """
    if workspace is None:
        return None
    path = os.path.normcase(os.path.abspath(path))
    workspace = os.path.normcase(os.path.abspath(workspace))
    try:
        rel = os.path.relpath(path, workspace)
    except ValueError:
        # Different drives on Windows.
        return None
    if rel == os.pardir or rel.startswith(os.pardir + os.sep) or os.path.isabs(rel):
        return None
    rel = rel.replace(os.sep, "/")
    if not rel.startswith("."):
        rel = "./" + rel
    return rel


def _relativize_url(url: str, workspace: str | None) -> str:
    """Rewrite a local ``file:`` URL inside *workspace* as a relative path."""
    if workspace is None or not url.startswith("file:"):
        return url
    try:
        path = url_to_path(url)
    except ValueError:
        return url
    rel = _relative_path_for_workspace(path, workspace)
    if rel is None:
        return url
    return rel


def _relativize_requirement(
    req: str, direct_url: DirectUrl, workspace: str | None
) -> str:
    """Rewrite the URL of a PEP 610 direct reference requirement string.

    Only local ``file:`` URLs that point inside *workspace* are rewritten as
    relative paths; anything else (remote URLs, locations outside the
    workspace) is left untouched.  VCS revisions, ``subdirectory=`` and hash
    fragments are preserved, and percent-encoded characters are decoded back
    to their literal form (``url_to_path``/``path_to_url`` round-trip).
    """
    new_url = _relativize_url(direct_url.url, workspace)
    if new_url == direct_url.url:
        return req
    # The URL is emitted verbatim by direct_url_as_pep440_direct_reference()
    # (as ``name @ url``, ``name @ url#fragments`` or
    # ``name @ vcs+url@commit``), so the first occurrence is the URL itself.
    return req.replace(direct_url.url, new_url, 1)


def _relativize_vcs_requirement(req: str, workspace: str | None) -> str:
    """Relativize the URL of a VCS requirement string.

    Handles requirements such as ``git+file:///path/to/repo@rev#egg=name``
    as produced for editable installs under version control, rewriting the
    URL as a relative path when it points inside *workspace*.  The VCS
    prefix, revision and fragments (e.g. ``egg=``, ``subdirectory=``) are
    preserved.
    """
    if workspace is None:
        return req
    vcs_prefix, plus, rest = req.partition("+")
    if not plus:
        return req
    url_rev, hash_sep, fragment = rest.partition("#")
    url, at, rev = url_rev.rpartition("@")
    if not at:
        url = url_rev
    new_url = _relativize_url(url, workspace)
    if new_url == url:
        return req
    result = f"{vcs_prefix}+{new_url}"
    if at:
        result += f"@{rev}"
    if hash_sep:
        result += f"#{fragment}"
    return result


def freeze(
    requirement: list[str] | None = None,
    local_only: bool = False,
    user_only: bool = False,
    paths: list[str] | None = None,
    isolated: bool = False,
    exclude_editable: bool = False,
    skip: Container[str] = (),
    workspace: str | None = None,
) -> Generator[str, None, None]:
    installations: dict[str, FrozenRequirement] = {}

    dists = get_environment(paths).iter_installed_distributions(
        local_only=local_only,
        skip=(),
        user_only=user_only,
    )
    for dist in dists:
        req = FrozenRequirement.from_dist(dist, workspace=workspace)
        if exclude_editable and req.editable:
            continue
        installations[req.canonical_name] = req

    if requirement:
        # the options that don't get turned into an InstallRequirement
        # should only be emitted once, even if the same option is in multiple
        # requirements files, so we need to keep track of what has been emitted
        # so that we don't emit it again if it's seen again
        emitted_options: set[str] = set()
        # keep track of which files a requirement is in so that we can
        # give an accurate warning if a requirement appears multiple times.
        req_files: dict[str, list[str]] = collections.defaultdict(list)
        for req_file_path in requirement:
            with open(req_file_path) as req_file:
                for line in req_file:
                    if (
                        not line.strip()
                        or line.strip().startswith("#")
                        or line.startswith(
                            (
                                "-r",
                                "--requirement",
                                "-f",
                                "--find-links",
                                "-i",
                                "--index-url",
                                "--pre",
                                "--trusted-host",
                                "--process-dependency-links",
                                "--extra-index-url",
                                "--use-feature",
                            )
                        )
                    ):
                        line = line.rstrip()
                        if line not in emitted_options:
                            emitted_options.add(line)
                            yield line
                        continue

                    if line.startswith(("-e", "--editable")):
                        if line.startswith("-e"):
                            line = line[2:].strip()
                        else:
                            line = line[len("--editable") :].strip().lstrip("=")
                        line_req = install_req_from_editable(
                            line,
                            isolated=isolated,
                        )
                    else:
                        line_req = install_req_from_line(
                            COMMENT_RE.sub("", line).strip(),
                            isolated=isolated,
                        )

                    if not line_req.name:
                        logger.info(
                            "Skipping line in requirement file [%s] because "
                            "it's not clear what it would install: %s",
                            req_file_path,
                            line.strip(),
                        )
                        logger.info(
                            "  (add #egg=PackageName to the URL to avoid"
                            " this warning)"
                        )
                    else:
                        line_req_canonical_name = canonicalize_name(line_req.name)
                        if line_req_canonical_name not in installations:
                            # either it's not installed, or it is installed
                            # but has been processed already
                            if not req_files[line_req.name]:
                                logger.warning(
                                    "Requirement file [%s] contains %s, but "
                                    "package %r is not installed",
                                    req_file_path,
                                    COMMENT_RE.sub("", line).strip(),
                                    line_req.name,
                                )
                            else:
                                req_files[line_req.name].append(req_file_path)
                        else:
                            yield str(installations[line_req_canonical_name]).rstrip()
                            del installations[line_req_canonical_name]
                            req_files[line_req.name].append(req_file_path)

        # Warn about requirements that were included multiple times (in a
        # single requirements file or in different requirements files).
        for name, files in req_files.items():
            if len(files) > 1:
                logger.warning(
                    "Requirement %s included multiple times [%s]",
                    name,
                    ", ".join(sorted(set(files))),
                )

        yield ("## The following requirements were added by pip freeze:")
    for installation in sorted(installations.values(), key=lambda x: x.name.lower()):
        if installation.canonical_name not in skip:
            yield str(installation).rstrip()


def _format_as_name_version(dist: BaseDistribution) -> str:
    try:
        dist_version = dist.version
    except InvalidVersion:
        # legacy version
        return f"{dist.raw_name}==={dist.raw_version}"
    else:
        return f"{dist.raw_name}=={dist_version}"


def _get_editable_info(
    dist: BaseDistribution, workspace: str | None = None
) -> _EditableInfo:
    """
    Compute and return values (req, comments) for use in
    FrozenRequirement.from_dist().
    """
    editable_project_location = dist.editable_project_location
    assert editable_project_location
    location = os.path.normcase(os.path.abspath(editable_project_location))
    # If the editable location is inside the workspace, refer to it with a
    # relative path so the output stays valid when the workspace is moved.
    display_location = _relative_path_for_workspace(location, workspace) or location

    from pip._internal.vcs import RemoteNotFoundError, RemoteNotValidError, vcs

    vcs_backend = vcs.get_backend_for_dir(location)

    if vcs_backend is None:
        display = _format_as_name_version(dist)
        logger.debug(
            'No VCS found for editable requirement "%s" in: %r',
            display,
            location,
        )
        return _EditableInfo(
            requirement=display_location,
            comments=[f"# Editable install with no version control ({display})"],
        )

    vcs_name = type(vcs_backend).__name__

    try:
        req = vcs_backend.get_src_requirement(location, dist.raw_name)
    except RemoteNotFoundError:
        display = _format_as_name_version(dist)
        return _EditableInfo(
            requirement=display_location,
            comments=[f"# Editable {vcs_name} install with no remote ({display})"],
        )
    except RemoteNotValidError as ex:
        display = _format_as_name_version(dist)
        return _EditableInfo(
            requirement=display_location,
            comments=[
                f"# Editable {vcs_name} install ({display}) with either a deleted "
                f"local remote or invalid URI:",
                f"# '{ex.url}'",
            ],
        )
    except BadCommand:
        logger.warning(
            "cannot determine version of editable source in %s "
            "(%s command not found in path)",
            location,
            vcs_backend.name,
        )
        return _EditableInfo(requirement=display_location, comments=[])
    except InstallationError as exc:
        logger.warning("Error when trying to get requirement for VCS system %s", exc)
    else:
        return _EditableInfo(
            requirement=_relativize_vcs_requirement(req, workspace), comments=[]
        )

    logger.warning("Could not determine repository location of %s", location)

    return _EditableInfo(
        requirement=display_location,
        comments=["## !! Could not determine repository location"],
    )


@dataclass(frozen=True)
class FrozenRequirement:
    name: str
    req: str
    editable: bool
    comments: Iterable[str] = field(default_factory=tuple)

    @property
    def canonical_name(self) -> NormalizedName:
        return canonicalize_name(self.name)

    @classmethod
    def from_dist(
        cls, dist: BaseDistribution, workspace: str | None = None
    ) -> FrozenRequirement:
        editable = dist.editable
        if editable:
            req, comments = _get_editable_info(dist, workspace=workspace)
        else:
            comments = []
            direct_url = dist.direct_url
            if direct_url:
                # if PEP 610 metadata is present, use it
                req = direct_url_as_pep440_direct_reference(direct_url, dist.raw_name)
                req = _relativize_requirement(req, direct_url, workspace)
            else:
                # name==version requirement
                req = _format_as_name_version(dist)

        return cls(dist.raw_name, req, editable, comments=comments)

    def __str__(self) -> str:
        req = self.req
        if self.editable:
            req = f"-e {req}"
        return "\n".join(list(self.comments) + [str(req)]) + "\n"
