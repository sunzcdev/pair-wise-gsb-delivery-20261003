import os
import sys
import urllib.parse
from pathlib import Path

import pytest

from pip._internal.utils.urls import (
    path_to_url,
    resolve_relative_path_url,
    url_to_path,
)


@pytest.mark.skipif("sys.platform == 'win32'")
def test_path_to_url_unix() -> None:
    assert path_to_url("/tmp/file") == "file:///tmp/file"
    path = os.path.join(os.getcwd(), "file")
    assert path_to_url("file") == "file://" + urllib.parse.quote(path)


@pytest.mark.skipif("sys.platform != 'win32'")
@pytest.mark.parametrize(
    "path, url",
    [
        pytest.param("C:/tmp/file", "file:///C:/tmp/file", id="posix-path"),
        pytest.param("C:\\tmp\\file", "file:///C:/tmp/file", id="nt-path"),
    ],
)
def test_path_to_url_win(path: str, url: str) -> None:
    assert path_to_url(path) == url


@pytest.mark.skipif("sys.platform != 'win32'")
def test_unc_path_to_url_win() -> None:
    # The two and four slash forms are both acceptable for our purposes. CPython's
    # behaviour has changed several times here, so blindly accept either.
    # - https://github.com/python/cpython/issues/78457
    # - https://github.com/python/cpython/issues/126205
    url = path_to_url(r"\\unc\as\path")
    assert url in ["file://unc/as/path", "file:////unc/as/path"]


@pytest.mark.skipif("sys.platform != 'win32'")
def test_relative_path_to_url_win() -> None:
    path = os.path.join(os.getcwd(), "file").replace("\\", "/")
    assert path_to_url("file") == "file:///" + urllib.parse.quote(path, safe="/:")


@pytest.mark.parametrize(
    "url,win_expected,non_win_expected",
    [
        ("file:tmp", "tmp", "tmp"),
        ("file:C:/path/to/file", r"C:\path\to\file", "C:/path/to/file"),
        ("file:/path/to/file", r"\path\to\file", "/path/to/file"),
        ("file://localhost/tmp/file", r"\tmp\file", "/tmp/file"),
        ("file://localhost/C:/tmp/file", r"C:\tmp\file", "/C:/tmp/file"),
        ("file://somehost/tmp/file", r"\\somehost\tmp\file", None),
        ("file:///tmp/file", r"\tmp\file", "/tmp/file"),
        ("file:///C:/tmp/file", r"C:\tmp\file", "/C:/tmp/file"),
    ],
)
def test_url_to_path(url: str, win_expected: str, non_win_expected: str) -> None:
    if sys.platform == "win32":
        expected_path = win_expected
    else:
        expected_path = non_win_expected

    if expected_path is None:
        with pytest.raises(ValueError):
            url_to_path(url)
    else:
        assert url_to_path(url) == expected_path


@pytest.mark.skipif("sys.platform != 'win32'")
def test_url_to_path_path_to_url_symmetry_win() -> None:
    path = r"C:\tmp\file"
    assert url_to_path(path_to_url(path)) == path

    unc_path = r"\\unc\share\path"
    assert url_to_path(path_to_url(unc_path)) == unc_path


class TestResolveRelativePathUrl:
    def test_non_relative_urls_unchanged(self) -> None:
        for url in (
            "https://example.com/pkg-1.0.tar.gz",
            "file:///abs/path/pkg",
            "git+https://example.com/repo@abc123#egg=pkg",
            "git+file:///abs/path/repo@abc123",
            "pkg==1.0",
        ):
            assert resolve_relative_path_url(url) == url

    def test_unknown_vcs_prefix_unchanged(self) -> None:
        assert resolve_relative_path_url("foo+./repo@abc") == "foo+./repo@abc"

    def test_relative_path(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.chdir(tmp_path)
        assert resolve_relative_path_url("./pkg") == path_to_url(
            os.fspath(tmp_path / "pkg")
        )

    def test_relative_parent_path(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        subdir = tmp_path / "sub"
        subdir.mkdir()
        monkeypatch.chdir(subdir)
        assert resolve_relative_path_url("../pkg") == path_to_url(
            os.fspath(tmp_path / "pkg")
        )

    def test_relative_archive_with_hash_fragment(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.chdir(tmp_path)
        assert resolve_relative_path_url("./dist/pkg-1.0.tar.gz#sha256=abc") == (
            path_to_url(os.fspath(tmp_path / "dist" / "pkg-1.0.tar.gz"))
            + "#sha256=abc"
        )

    def test_vcs_relative_with_rev_and_subdirectory(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.chdir(tmp_path)
        resolved = resolve_relative_path_url(
            "git+./repo@abc123#subdirectory=sub%dir"
        )
        assert resolved == (
            "git+"
            + path_to_url(os.fspath(tmp_path / "repo"))
            + "@abc123#subdirectory=sub%dir"
        )

    def test_vcs_relative_without_rev(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.chdir(tmp_path)
        assert resolve_relative_path_url("git+../repo") == (
            "git+" + path_to_url(os.fspath(tmp_path.parent / "repo"))
        )

    def test_vcs_only_skips_plain_paths(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.chdir(tmp_path)
        assert resolve_relative_path_url("./pkg", vcs_only=True) == "./pkg"
        assert resolve_relative_path_url("git+./repo@abc", vcs_only=True) == (
            "git+" + path_to_url(os.fspath(tmp_path / "repo")) + "@abc"
        )

    def test_percent_characters_round_trip(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.chdir(tmp_path)
        resolved = resolve_relative_path_url("./edit%pkg")
        assert "%25" in resolved
        assert url_to_path(resolved) == os.path.join(
            os.fspath(tmp_path), "edit%pkg"
        )
