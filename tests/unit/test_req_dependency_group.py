import errno
import textwrap
from pathlib import Path
from typing import Any

import pytest

from pip._internal.exceptions import InstallationError
from pip._internal.models.dependency_group import (
    DependencyGroupProvenance,
    ResolvedDependencyGroupRequirement,
)
from pip._internal.req.req_dependency_group import parse_dependency_groups


def test_parse_simple_dependency_groups(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    pyproject = tmp_path.joinpath("pyproject.toml")
    pyproject.write_text(textwrap.dedent("""\
            [dependency-groups]
            foo = ["bar"]
            """))
    monkeypatch.chdir(tmp_path)

    result = list(parse_dependency_groups([("pyproject.toml", "foo")]))

    assert len(result) == 1, result
    entry = result[0]
    assert isinstance(entry, ResolvedDependencyGroupRequirement)
    assert entry.requirement == "bar"
    assert entry.provenance == DependencyGroupProvenance(
        source="pyproject.toml",
        requested_group="foo",
        include_chain=("foo",),
        marker=None,
    )
    assert entry.provenance.comes_from() == (
        "dependency group 'foo' from 'pyproject.toml'"
    )


def test_parse_dependency_groups_preserves_include_chain_and_marker(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    pyproject = tmp_path.joinpath("pyproject.toml")
    pyproject.write_text(textwrap.dedent("""\
            [dependency-groups]
            dev = [
                "bar==1.0",
                {include-group = "test"},
            ]
            test = [
                'six; python_version < "3.12"',
                {include-group = "base"},
            ]
            base = ["wheel"]
            """))
    monkeypatch.chdir(tmp_path)

    result = list(parse_dependency_groups([("pyproject.toml", "dev")]))

    assert [entry.requirement for entry in result] == [
        "bar==1.0",
        'six; python_version < "3.12"',
        "wheel",
    ]

    by_name = {entry.requirement: entry.provenance for entry in result}
    assert by_name["bar==1.0"].include_chain == ("dev",)
    assert by_name["bar==1.0"].marker is None

    six_provenance = by_name['six; python_version < "3.12"']
    assert six_provenance.source == "pyproject.toml"
    assert six_provenance.requested_group == "dev"
    assert six_provenance.include_chain == ("dev", "test")
    assert six_provenance.direct_group == "test"
    assert six_provenance.marker == 'python_version < "3.12"'
    assert six_provenance.comes_from() == (
        'dependency group \'dev\' from \'pyproject.toml\' '
        "(included via 'dev -> test'); "
        'marker \'python_version < "3.12"\''
    )

    wheel_provenance = by_name["wheel"]
    assert wheel_provenance.include_chain == ("dev", "test", "base")
    assert wheel_provenance.direct_group == "base"
    assert "included via 'dev -> test -> base'" in wheel_provenance.comes_from()


def test_parse_same_named_groups_keep_their_own_sources(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project_a = tmp_path.joinpath("proj_a")
    project_b = tmp_path.joinpath("proj_b")
    project_a.mkdir()
    project_b.mkdir()
    project_a.joinpath("pyproject.toml").write_text(textwrap.dedent("""\
            [dependency-groups]
            test = ["simple==1.0", {include-group = "shared"}]
            shared = ["six"]
            """))
    project_b.joinpath("pyproject.toml").write_text(textwrap.dedent("""\
            [dependency-groups]
            test = ["simple==2.0"]
            """))
    monkeypatch.chdir(tmp_path)

    result = list(
        parse_dependency_groups(
            [
                ("proj_a/pyproject.toml", "test"),
                ("proj_b/pyproject.toml", "test"),
            ]
        )
    )

    assert [entry.requirement for entry in result] == [
        "simple==1.0",
        "six",
        "simple==2.0",
    ]
    sources = [entry.provenance.source for entry in result]
    assert sources == [
        "proj_a/pyproject.toml",
        "proj_a/pyproject.toml",
        "proj_b/pyproject.toml",
    ]
    assert result[0].provenance.comes_from() == (
        "dependency group 'test' from 'proj_a/pyproject.toml'"
    )
    assert result[2].provenance.comes_from() == (
        "dependency group 'test' from 'proj_b/pyproject.toml'"
    )
    # The shared requirement still points back at the originally selected group.
    assert result[1].provenance.include_chain == ("test", "shared")
    assert result[1].provenance.requested_group == "test"


def test_dependency_group_provenance_validation() -> None:
    DependencyGroupProvenance(
        source="pyproject.toml",
        requested_group="dev",
        include_chain=("dev",),
        marker=None,
    )

    with pytest.raises(ValueError, match="at least the requested group"):
        DependencyGroupProvenance(
            source="pyproject.toml",
            requested_group="dev",
            include_chain=(),
            marker=None,
        )

    with pytest.raises(ValueError, match="must start with the requested group"):
        DependencyGroupProvenance(
            source="pyproject.toml",
            requested_group="dev",
            include_chain=("test",),
            marker=None,
        )


def test_parse_cyclic_dependency_groups(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    pyproject = tmp_path.joinpath("pyproject.toml")
    pyproject.write_text(textwrap.dedent("""\
            [dependency-groups]
            foo = [{include-group="bar"}]
            bar = [{include-group="foo"}]
            """))
    monkeypatch.chdir(tmp_path)

    with pytest.raises(
        InstallationError,
        match=(
            r"\[dependency-groups\] resolution failed for "
            r"'foo' from 'pyproject\.toml':"
        ),
    ) as excinfo:
        parse_dependency_groups([("pyproject.toml", "foo")])

    exception = excinfo.value
    assert (
        "Cyclic dependency group include while resolving foo: foo -> bar, bar -> foo"
    ) in str(exception)


def test_parse_with_no_dependency_groups_defined(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    pyproject = tmp_path.joinpath("pyproject.toml")
    pyproject.write_text("")
    monkeypatch.chdir(tmp_path)

    with pytest.raises(
        InstallationError,
        match=(r"\[dependency-groups\] table was missing from 'pyproject\.toml'\."),
    ):
        parse_dependency_groups([("pyproject.toml", "foo")])


def test_parse_with_no_pyproject_file(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.chdir(tmp_path)

    with pytest.raises(InstallationError, match=r"pyproject\.toml not found\."):
        parse_dependency_groups([("pyproject.toml", "foo")])


def test_parse_with_malformed_pyproject_file(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    pyproject = tmp_path.joinpath("pyproject.toml")
    pyproject.write_text(textwrap.dedent("""\
            [dependency-groups  # no closing bracket
            foo = ["bar"]
            """))
    monkeypatch.chdir(tmp_path)

    with pytest.raises(InstallationError, match=r"Error parsing pyproject\.toml"):
        parse_dependency_groups([("pyproject.toml", "foo")])


def test_parse_gets_unexpected_oserror(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    pyproject = tmp_path.joinpath("pyproject.toml")
    pyproject.write_text(textwrap.dedent("""\
            [dependency-groups]
            foo = ["bar"]
            """))
    monkeypatch.chdir(tmp_path)

    # inject an implementation of `tomli.load()` which emits an 'OSError(EPIPE, ...)'
    # as though we were loading from a fifo or other unusual filetype
    def epipe_toml_load(*args: Any, **kwargs: Any) -> None:
        raise OSError(errno.EPIPE, "Broken pipe")

    monkeypatch.setattr(
        "pip._internal.req.req_dependency_group.tomllib.load", epipe_toml_load
    )

    with pytest.raises(InstallationError, match=r"Error reading pyproject\.toml"):
        parse_dependency_groups([("pyproject.toml", "foo")])
