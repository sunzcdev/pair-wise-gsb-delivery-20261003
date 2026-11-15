"""Tests for type-dependency-based generated hook invalidation."""

from __future__ import annotations

from typing import Generic, TypeVar

from attrs import define

from cattrs import Converter

T = TypeVar("T")


@define
class Inner:
    a: int


@define
class Outer:
    inner: Inner


@define
class Unrelated:
    x: int


@define
class Holder:
    items: list[Inner]


@define
class Box(Generic[T]):
    value: T


@define
class Node:
    value: int
    child: Node | None = None


def test_structure_hook_invalidates_dependents():
    converter = Converter()
    converter.structure({"inner": {"a": 1}}, Outer)
    converter.structure({"x": 2}, Unrelated)

    converter.register_structure_hook(Inner, lambda v, _: Inner(v["a"] + 100))

    # The dependent hook is regenerated, the unrelated one is kept cached.
    assert Outer not in converter._structure_func._dispatch_cache
    assert Unrelated in converter._structure_func._dispatch_cache
    assert converter.structure({"inner": {"a": 1}}, Outer) == Outer(Inner(101))


def test_unstructure_hook_invalidates_dependents():
    converter = Converter()
    converter.unstructure(Outer(Inner(1)))
    converter.unstructure(Unrelated(2))

    converter.register_unstructure_hook(Inner, lambda i: {"a": i.a + 1000})

    assert Outer not in converter._unstructure_func._dispatch_cache
    assert Unrelated in converter._unstructure_func._dispatch_cache
    assert converter.unstructure(Outer(Inner(1))) == {"inner": {"a": 1001}}


def test_nested_container_is_a_dependency():
    converter = Converter()
    converter.structure({"items": [{"a": 1}]}, Holder)
    converter.unstructure(Holder([Inner(1)]))

    converter.register_structure_hook(Inner, lambda v, _: Inner(v["a"] + 7))
    converter.register_unstructure_hook(Inner, lambda i: {"a": i.a + 9})

    assert Holder not in converter._structure_func._dispatch_cache
    assert converter.structure({"items": [{"a": 1}]}, Holder) == Holder([Inner(8)])
    assert converter.unstructure(Holder([Inner(1)])) == {
        "items": [{"a": 10}]
    }


def test_generic_arguments_are_dependencies():
    converter = Converter()
    alias = Box[Inner]
    converter.structure({"value": {"a": 1}}, alias)
    converter.unstructure(Box(Inner(1)))

    converter.register_structure_hook(Inner, lambda v, _: Inner(v["a"] + 3))
    converter.register_unstructure_hook(Inner, lambda i: {"a": i.a + 5})

    assert alias not in converter._structure_func._dispatch_cache
    assert converter.structure({"value": {"a": 1}}, alias) == Box(Inner(4))
    assert converter.unstructure(Box(Inner(1))) == {"value": {"a": 6}}


def test_recursive_types_are_dependencies():
    converter = Converter()
    data = {"value": 1, "child": {"value": 2, "child": None}}
    converter.structure(data, Node)
    converter.unstructure(Node(1, Node(2)))

    def structure_node(v, _):
        child = (
            converter.structure(v["child"], Node) if v["child"] is not None else None
        )
        return Node(v["value"] + 50, child)

    def unstructure_node(n):
        child = converter.unstructure(n.child) if n.child is not None else None
        return {"value": n.value + 60, "child": child}

    converter.register_structure_hook(Node, structure_node)
    converter.register_unstructure_hook(Node, unstructure_node)

    structured = converter.structure(data, Node)
    assert structured.value == 51
    assert structured.child.value == 52
    assert converter.unstructure(Node(1, Node(2))) == {
        "value": 61,
        "child": {"value": 62, "child": None},
    }


def test_taken_hooks_keep_captured_behavior():
    converter = Converter()
    taken_structure = converter.get_structure_hook(Outer)
    taken_unstructure = converter.get_unstructure_hook(Outer)

    converter.register_structure_hook(Inner, lambda v, _: Inner(v["a"] + 100))
    converter.register_unstructure_hook(Inner, lambda i: {"a": i.a + 1000})

    # Functions already handed out keep their old, captured behavior.
    assert taken_structure({"inner": {"a": 1}}, Outer) == Outer(Inner(1))
    assert taken_unstructure(Outer(Inner(1))) == {"inner": {"a": 1}}

    # Re-fetching through the public API picks up the new behavior.
    fresh_structure = converter.get_structure_hook(Outer)
    fresh_unstructure = converter.get_unstructure_hook(Outer)
    assert fresh_structure is not taken_structure
    assert fresh_structure({"inner": {"a": 1}}, Outer) == Outer(Inner(101))
    assert fresh_unstructure(Outer(Inner(1))) == {"inner": {"a": 1001}}


def test_independent_converters_do_not_share_invalidation():
    first = Converter()
    second = Converter()
    first.structure({"inner": {"a": 1}}, Outer)
    second.structure({"inner": {"a": 1}}, Outer)

    first.register_structure_hook(Inner, lambda v, _: Inner(v["a"] + 100))

    assert first.structure({"inner": {"a": 1}}, Outer) == Outer(Inner(101))
    assert second.structure({"inner": {"a": 1}}, Outer) == Outer(Inner(1))


def test_superclass_hook_invalidates_subclass_cache():
    @define
    class Base:
        i: int

    @define
    class Sub(Base):
        pass

    converter = Converter()
    assert converter.structure({"i": 1}, Sub) == Sub(1)

    converter.register_structure_hook(Base, lambda o, t: t(o["i"] + 1))

    assert Sub not in converter._structure_func._dispatch_cache
    assert converter.structure({"i": 1}, Sub) == Sub(2)
