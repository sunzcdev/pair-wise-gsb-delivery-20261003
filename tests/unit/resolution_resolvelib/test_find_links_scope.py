"""Unit tests for find-links scope enforcement during resolution."""

from __future__ import annotations

import os
from pathlib import Path
from types import SimpleNamespace

import pytest

from pip._internal.exceptions import FindLinksScopeError
from pip._internal.models.candidate import InstallationCandidate
from pip._internal.models.link import Link
from pip._internal.req.constructors import install_req_from_line
from pip._internal.req.req_install import InstallRequirement
from pip._internal.resolution.resolvelib.factory import (
    Factory,
    _link_is_within_find_links,
)
from pip._internal.utils.urls import path_to_url


def make_candidate(name: str, version: str, path: Path) -> InstallationCandidate:
    return InstallationCandidate(name, version, Link(path_to_url(os.fspath(path))))


class TestLinkIsWithinFindLinks:
    def test_local_directory(self, tmp_path: Path) -> None:
        wheelhouse = tmp_path / "wheelhouse"
        wheelhouse.mkdir()
        wheel = wheelhouse / "pkg-1.0-py3-none-any.whl"
        wheel.write_bytes(b"")
        link = Link(path_to_url(os.fspath(wheel)))

        assert _link_is_within_find_links(link, os.fspath(wheelhouse))
        assert _link_is_within_find_links(link, path_to_url(os.fspath(wheelhouse)))
        assert not _link_is_within_find_links(link, os.fspath(tmp_path))

    def test_http_url(self) -> None:
        link = Link("https://wheelhouse.example.com/packages/pkg-1.0.tar.gz")
        assert _link_is_within_find_links(
            link, "https://wheelhouse.example.com/packages"
        )
        assert _link_is_within_find_links(
            link, "https://wheelhouse.example.com/packages/"
        )
        assert not _link_is_within_find_links(
            link, "https://wheelhouse.example.com/other"
        )
        assert not _link_is_within_find_links(
            link, "https://wheelhouse.example.com/packages2"
        )


def make_scoped_ireq(
    name: str, scope: tuple[tuple[str, str], ...]
) -> InstallRequirement:
    ireq = install_req_from_line(name)
    ireq.find_links_scope = scope
    return ireq


class TestCheckFindLinksScopes:
    def check(
        self,
        candidates: list[InstallationCandidate],
        ireqs: list[InstallRequirement],
    ) -> None:
        finder = SimpleNamespace(
            find_all_candidates=lambda project_name: candidates
        )
        Factory._check_find_links_scopes(
            SimpleNamespace(_finder=finder),  # type: ignore[arg-type]
            "shared",
            ireqs,  # type: ignore[arg-type]
        )

    def test_no_scopes_is_a_noop(self) -> None:
        self.check([], [install_req_from_line("shared")])

    def test_scope_satisfied_by_candidate(self, tmp_path: Path) -> None:
        wheelhouse = tmp_path / "wheelhouse"
        wheelhouse.mkdir()
        candidate = make_candidate(
            "shared", "1.0", wheelhouse / "shared-1.0-py3-none-any.whl"
        )
        ireq = make_scoped_ireq(
            "shared",
            ((os.fspath(wheelhouse), "line 1 of requirements.txt"),),
        )
        self.check([candidate], [ireq])

    def test_unsatisfiable_scope_reports_introduction_location(
        self, tmp_path: Path
    ) -> None:
        wheelhouse_a = tmp_path / "wheelhouse-a"
        wheelhouse_a.mkdir()
        wheelhouse_b = tmp_path / "wheelhouse-b"
        wheelhouse_b.mkdir()
        candidate = make_candidate(
            "shared", "1.0", wheelhouse_a / "shared-1.0-py3-none-any.whl"
        )
        scope_a = (os.fspath(wheelhouse_a), "line 1 of requirements.txt")
        scope_b = (os.fspath(wheelhouse_b), "line 4 of requirements.txt")
        ireqs = [
            make_scoped_ireq("shared", (scope_a,)),
            make_scoped_ireq("shared", (scope_b,)),
        ]

        with pytest.raises(FindLinksScopeError) as exc_info:
            self.check([candidate], ireqs)

        message = str(exc_info.value)
        assert "'shared'" in message
        # The unsatisfiable restriction is reported with the location where
        # it was introduced; the satisfiable one is not.
        assert os.fspath(wheelhouse_b) in message
        assert "line 4 of requirements.txt" in message
        assert os.fspath(wheelhouse_a) not in message
