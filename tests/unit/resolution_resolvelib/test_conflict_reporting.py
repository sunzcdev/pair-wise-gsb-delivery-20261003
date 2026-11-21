from __future__ import annotations

from typing import TYPE_CHECKING
from unittest.mock import Mock

from pip._internal.req.constructors import install_req_from_req_string
from pip._internal.resolution.resolvelib.requirements import SpecifierRequirement

if TYPE_CHECKING:
    from pip._internal.resolution.resolvelib.factory import Factory


def test_iter_conflict_chain_lines_walks_to_root(factory: Factory) -> None:
    root = install_req_from_req_string("mida", user_supplied=True)
    middle = install_req_from_req_string("pkga==1.0", comes_from=root)
    leaf = install_req_from_req_string("base==1.0", comes_from=middle)
    requirement = SpecifierRequirement(leaf)

    parent = Mock()
    parent.name = "pkga"
    parent.version = "1.0"

    lines = list(factory._iter_conflict_chain_lines(requirement, parent))

    assert lines == [
        "pkga 1.0 depends on base==1.0",
        "mida depends on pkga==1.0",
        "The user requested mida",
    ]


def test_iter_conflict_chain_lines_for_root_requirement(factory: Factory) -> None:
    root = install_req_from_req_string("base==1.0", user_supplied=True)
    requirement = SpecifierRequirement(root)

    lines = list(factory._iter_conflict_chain_lines(requirement, None))

    assert lines == ["The user requested base==1.0"]


def test_collect_requires_python_skips_unknown_project(factory: Factory) -> None:
    # The helper must not raise for a project the finder knows nothing about.
    assert factory._collect_requires_python_skips({"base", "other"}) == {}
