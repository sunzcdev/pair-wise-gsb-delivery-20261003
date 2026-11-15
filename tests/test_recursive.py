"""Test un/structuring recursive class graphs."""

from __future__ import annotations

from typing import Generic, List, Optional, TypeVar

import pytest
from attr import define, resolve_types
from typing_extensions import Self

from cattr import Converter
from cattrs import Converter as GenConverter
from cattrs.gen._consts import already_generating


@define
class A:
    inner: List[A]


def test_simple_recursive():
    c = Converter()

    orig = A([A([])])
    unstructured = c.unstructure(orig)

    assert unstructured == {"inner": [{"inner": []}]}

    assert c.structure(unstructured, A) == orig


T = TypeVar("T")


@define
class Node(Generic[T]):
    """A recursive generic class; children keep the same type parameter."""

    val: T
    children: List[Node[T]]


@define
class SelfNode(Generic[T]):
    """A recursive generic class, referring to itself through `Self`."""

    val: T
    children: List[Self]


@define
class NestedNode(Generic[T]):
    """A recursive generic class through nested containers."""

    val: T
    children: List[List[NestedNode[T]]]


@define
class MutualA(Generic[T]):
    """One half of a mutually-recursive generic pair."""

    val: T
    bs: List[MutualB[T]]


@define
class MutualB(Generic[T]):
    """The other half of a mutually-recursive generic pair."""

    val: T
    a: Optional[MutualA[T]] = None


@pytest.mark.parametrize("node_cl", [Node, SelfNode])
def test_recursive_generic(genconverter: GenConverter, node_cl):
    """Hooks for recursive generics terminate and keep the type parameter."""
    orig = node_cl(1, [node_cl(2, [node_cl(3, [])])])

    unstructured = genconverter.unstructure(orig, node_cl[int])
    assert unstructured == {
        "val": 1,
        "children": [{"val": 2, "children": [{"val": 3, "children": []}]}],
    }

    structured = genconverter.structure(unstructured, node_cl[int])
    assert structured == orig
    assert type(structured.children[0].children[0].val) is int


@pytest.mark.parametrize("node_cl", [Node, SelfNode])
def test_recursive_generic_interleaved(genconverter: GenConverter, node_cl):
    """Hooks for different type parameters must not be mixed up.

    Interleaving conversions of the same payload as `Node[int]` and
    `Node[str]` must produce leaves of the actual type; the recursive
    fields must not be demoted to `Any`.
    """
    int_hook = genconverter.get_structure_hook(node_cl[int])
    str_hook = genconverter.get_structure_hook(node_cl[str])
    assert int_hook is not str_hook

    raw = {"val": "1", "children": [{"val": "2", "children": []}]}

    # The `int` hook must actually convert the leaves.
    as_int = int_hook(raw, node_cl[int])
    assert as_int == node_cl(1, [node_cl(2, [])])
    assert type(as_int.val) is int
    assert type(as_int.children[0].val) is int

    # The `str` hook must keep the leaves strings.
    as_str = str_hook(raw, node_cl[str])
    assert as_str == node_cl("1", [node_cl("2", [])])
    assert type(as_str.val) is str
    assert type(as_str.children[0].val) is str

    # And once more, interleaved, to ensure nothing was cached wrongly.
    as_int_again = genconverter.structure(raw, node_cl[int])
    assert type(as_int_again.val) is int
    assert type(as_int_again.children[0].val) is int

    as_str_again = genconverter.structure({"val": 1, "children": []}, node_cl[str])
    assert type(as_str_again.val) is str


def test_recursive_generic_mutual(genconverter: GenConverter):
    """Two mutually-referencing generic models are supported."""
    orig = MutualA(1, [MutualB(2, MutualA(3, [])), MutualB(4)])

    unstructured = genconverter.unstructure(orig, MutualA[int])
    assert unstructured == {
        "val": 1,
        "bs": [
            {"val": 2, "a": {"val": 3, "bs": []}},
            {"val": 4, "a": None},
        ],
    }

    structured = genconverter.structure(unstructured, MutualA[int])
    assert structured == orig
    assert type(structured.bs[0].a.val) is int

    as_str = genconverter.structure(
        genconverter.unstructure(MutualA("a", [MutualB("b")]), MutualA[str]),
        MutualA[str],
    )
    assert type(as_str.bs[0].val) is str


def test_recursive_generic_nested_containers(genconverter: GenConverter):
    """Recursive generics behind nested containers are supported."""
    orig = NestedNode(1, [[NestedNode(2, []), NestedNode(3, [])], [NestedNode(4, [])]])

    unstructured = genconverter.unstructure(orig, NestedNode[int])
    assert unstructured == {
        "val": 1,
        "children": [
            [{"val": 2, "children": []}, {"val": 3, "children": []}],
            [{"val": 4, "children": []}],
        ],
    }

    structured = genconverter.structure(unstructured, NestedNode[int])
    assert structured == orig
    assert type(structured.children[0][1].val) is int


def test_recursive_generic_failed_forward_ref(genconverter: GenConverter):
    """A failed hook preparation leaves no unfinished placeholder behind.

    If resolving a forward reference fails, the converter must be left in
    a clean state so preparing the hook again (after the reference becomes
    resolvable) works.
    """

    @define
    class ForwardA(Generic[T]):
        val: T
        bs: List[ForwardB[T]]  # noqa: F821

    with pytest.raises(NameError):
        genconverter.get_structure_hook(ForwardA[int])
    with pytest.raises(NameError):
        genconverter.get_unstructure_hook(ForwardA[int])

    # The unfinished placeholders must have been removed.
    assert not hasattr(already_generating, "working_set")

    @define
    class ForwardB(Generic[T]):
        val: T
        a: Optional[ForwardA[T]] = None

    # Fix the registration and prepare again.
    globalns = {**globals(), "ForwardA": ForwardA, "ForwardB": ForwardB}
    resolve_types(ForwardA, globalns=globalns)
    resolve_types(ForwardB, globalns=globalns)

    orig = ForwardA(1, [ForwardB(2, ForwardA(3, []))])
    unstructured = genconverter.unstructure(orig, ForwardA[int])
    structured = genconverter.structure(unstructured, ForwardA[int])

    assert structured == orig
    assert type(structured.bs[0].a.val) is int
