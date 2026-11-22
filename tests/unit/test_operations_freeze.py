"""Tests for the workspace-relative references emitted by ``pip freeze``."""

import os
from pathlib import Path

from pip._internal.models.direct_url import ArchiveInfo, DirectUrl, DirInfo, VcsInfo
from pip._internal.operations.freeze import (
    _relative_path_for_workspace,
    _relativize_requirement,
    _relativize_url,
    _relativize_vcs_requirement,
)
from pip._internal.utils.direct_url_helpers import direct_url_as_pep440_direct_reference
from pip._internal.utils.urls import path_to_url


class TestRelativePathForWorkspace:
    def test_inside(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        assert (
            _relative_path_for_workspace(
                os.fspath(workspace / "sub" / "pkg"), os.fspath(workspace)
            )
            == "./sub/pkg"
        )

    def test_workspace_root(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        assert (
            _relative_path_for_workspace(os.fspath(workspace), os.fspath(workspace))
            == "."
        )

    def test_outside(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        assert (
            _relative_path_for_workspace(
                os.fspath(tmp_path / "other"), os.fspath(workspace)
            )
            is None
        )

    def test_sibling_with_common_prefix(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        sibling = tmp_path / "ws2"
        assert (
            _relative_path_for_workspace(os.fspath(sibling), os.fspath(workspace))
            is None
        )

    def test_no_workspace(self, tmp_path: Path) -> None:
        assert _relative_path_for_workspace(os.fspath(tmp_path), None) is None


class TestRelativizeUrl:
    def test_file_url_inside(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        url = path_to_url(os.fspath(workspace / "pkg"))
        assert _relativize_url(url, os.fspath(workspace)) == "./pkg"

    def test_file_url_outside(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        url = path_to_url(os.fspath(tmp_path / "pkg"))
        assert _relativize_url(url, os.fspath(workspace)) == url

    def test_remote_url_unchanged(self, tmp_path: Path) -> None:
        url = "https://example.com/pkg-1.0.tar.gz"
        assert _relativize_url(url, os.fspath(tmp_path)) == url

    def test_no_workspace(self) -> None:
        url = "file:///somewhere/pkg"
        assert _relativize_url(url, None) == url

    def test_percent_characters_decoded(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        url = path_to_url(os.fspath(workspace / "edit%pkg"))
        assert "%25" in url
        assert _relativize_url(url, os.fspath(workspace)) == "./edit%pkg"


class TestRelativizeRequirement:
    def test_archive_with_hash(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        direct_url = DirectUrl(
            url=path_to_url(os.fspath(workspace / "dist" / "pkg-1.0.tar.gz")),
            archive_info=ArchiveInfo(hashes={"sha256": "abcdef"}),
        )
        req = direct_url_as_pep440_direct_reference(direct_url, "pkg")
        assert _relativize_requirement(req, direct_url, os.fspath(workspace)) == (
            "pkg @ ./dist/pkg-1.0.tar.gz#sha256=abcdef"
        )

    def test_vcs_revision_subdirectory_and_percent_round_trip(
        self, tmp_path: Path
    ) -> None:
        workspace = tmp_path / "ws"
        direct_url = DirectUrl(
            url=path_to_url(os.fspath(workspace / "repo")),
            vcs_info=VcsInfo(vcs="git", commit_id="abc123"),
            subdirectory="sub%dir",
        )
        req = direct_url_as_pep440_direct_reference(direct_url, "pkg")
        assert _relativize_requirement(req, direct_url, os.fspath(workspace)) == (
            "pkg @ git+./repo@abc123#subdirectory=sub%dir"
        )

    def test_directory(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        direct_url = DirectUrl(
            url=path_to_url(os.fspath(workspace / "pkg")), dir_info=DirInfo()
        )
        req = direct_url_as_pep440_direct_reference(direct_url, "pkg")
        assert (
            _relativize_requirement(req, direct_url, os.fspath(workspace))
            == "pkg @ ./pkg"
        )

    def test_outside_workspace_unchanged(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        direct_url = DirectUrl(
            url=path_to_url(os.fspath(tmp_path / "pkg")), dir_info=DirInfo()
        )
        req = direct_url_as_pep440_direct_reference(direct_url, "pkg")
        assert (
            _relativize_requirement(req, direct_url, os.fspath(workspace)) == req
        )

    def test_remote_url_unchanged(self, tmp_path: Path) -> None:
        direct_url = DirectUrl(
            url="https://example.com/repo",
            vcs_info=VcsInfo(vcs="git", commit_id="abc123"),
        )
        req = direct_url_as_pep440_direct_reference(direct_url, "pkg")
        assert (
            _relativize_requirement(req, direct_url, os.fspath(tmp_path)) == req
        )

    def test_no_workspace(self, tmp_path: Path) -> None:
        direct_url = DirectUrl(
            url=path_to_url(os.fspath(tmp_path / "pkg")), dir_info=DirInfo()
        )
        req = direct_url_as_pep440_direct_reference(direct_url, "pkg")
        assert _relativize_requirement(req, direct_url, None) == req


class TestRelativizeVcsRequirement:
    def test_local_remote_inside_workspace(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        url = path_to_url(os.fspath(workspace / "remotes" / "repo.git"))
        req = f"git+{url}@abc123#egg=pkg"
        assert _relativize_vcs_requirement(req, os.fspath(workspace)) == (
            "git+./remotes/repo.git@abc123#egg=pkg"
        )

    def test_subdirectory_fragment_preserved(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        url = path_to_url(os.fspath(workspace / "repo"))
        req = f"git+{url}@abc123#egg=pkg&subdirectory=sub%dir"
        assert _relativize_vcs_requirement(req, os.fspath(workspace)) == (
            "git+./repo@abc123#egg=pkg&subdirectory=sub%dir"
        )

    def test_remote_url_unchanged(self, tmp_path: Path) -> None:
        req = "git+https://example.com/repo@abc123#egg=pkg"
        assert _relativize_vcs_requirement(req, os.fspath(tmp_path)) == req

    def test_no_revision(self, tmp_path: Path) -> None:
        workspace = tmp_path / "ws"
        url = path_to_url(os.fspath(workspace / "repo"))
        req = f"git+{url}#egg=pkg"
        assert _relativize_vcs_requirement(req, os.fspath(workspace)) == (
            "git+./repo#egg=pkg"
        )

    def test_no_workspace(self) -> None:
        req = "git+file:///somewhere/repo@abc123#egg=pkg"
        assert _relativize_vcs_requirement(req, None) == req
