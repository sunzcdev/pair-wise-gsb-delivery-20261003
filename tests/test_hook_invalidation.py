"""Tests for type-dependency-aware hook cache invalidation."""

from __future__ import annotations

from typing import List, Optional

import attrs
import pytest

from cattrs import BaseConverter, Converter


@attrs.define
class Inner:
    x: int


@attrs.define
class Outer:
    inner: Inner


@attrs.define
class ContainerOuter:
    items: list[Inner]
    maybe: Optional[Inner] = None


@attrs.define
class Recursive:
    children: List[Recursive]
    val: int = 0


@pytest.fixture
def converter():
    return Converter()


def test_structure_hook_reregistration_invalidates_dependent(converter):
    """A generated outer hook stops capturing the old field-type hook."""
    converter.get_structure_hook(Outer)

    converter.register_structure_hook(Inner, lambda v, t: Inner(x=v["x"] + 1000))

    assert converter.structure({"inner": {"x": 1}}, Outer) == Outer(Inner(1001))


def test_unstructure_hook_reregistration_invalidates_dependent(converter):
    """The same rule applies in the unstructuring direction."""
    converter.get_unstructure_hook(Outer)

    converter.register_unstructure_hook(Inner, lambda i: {"x": i.x + 2000})

    assert converter.unstructure(Outer(Inner(1))) == {"inner": {"x": 2001}}


def test_nested_container_dependencies(converter):
    """Container hooks depend on their element and value types."""
    before = converter.get_structure_hook(list[Inner])

    converter.register_structure_hook(Inner, lambda v, t: Inner(x=v["x"] + 1))

    after = converter.get_structure_hook(list[Inner])
    assert before is not after
    assert converter.structure([{"x": 1}, {"x": 2}], list[Inner]) == [
        Inner(2),
        Inner(3),
    ]

    map_before = converter.get_structure_hook(dict[str, Inner])
    converter.register_structure_hook(Inner, lambda v, t: Inner(x=v["x"] + 10))
    assert converter.get_structure_hook(dict[str, Inner]) is not map_before
    assert converter.structure({"a": {"x": 1}}, dict[str, Inner]) == {"a": Inner(11)}


def test_generic_arguments_and_transitive_dependencies(converter):
    """Nested generics invalidate through the whole dependency chain."""
    before = converter.get_structure_hook(ContainerOuter)

    converter.register_structure_hook(Inner, lambda v, t: Inner(x=v["x"] + 5))

    assert converter.get_structure_hook(ContainerOuter) is not before
    assert converter.structure(
        {"items": [{"x": 1}], "maybe": {"x": 2}}, ContainerOuter
    ) == ContainerOuter([Inner(6)], Inner(7))


def test_recursive_types_stay_supported(converter):
    original = Recursive([Recursive([], 1)], 0)
    raw = converter.unstructure(original)
    assert converter.structure(raw, Recursive) == original

    converter.register_structure_hook(int, lambda v, t: int(v))
    assert converter.structure(raw, Recursive) == original


def test_unrelated_hooks_are_not_invalidated(converter):
    """Registering one type's hook leaves unrelated cached hooks in place."""

    @attrs.define
    class Other:
        s: str

    converter.get_structure_hook(Outer)
    other_hook = converter.get_structure_hook(Other)

    converter.register_structure_hook(Inner, lambda v, t: Inner(x=v["x"]))

    assert converter.get_structure_hook(Other) is other_hook
    assert Other not in converter.invalidate_structure_hook(Inner)


def test_held_hooks_keep_captured_behavior(converter):
    """Hooks already handed to callers are not mutated out from under them."""
    held_structure = converter.get_structure_hook(Outer)
    held_unstructure = converter.get_unstructure_hook(Outer)

    converter.register_structure_hook(Inner, lambda v, t: Inner(x=v["x"] + 1))
    converter.register_unstructure_hook(Inner, lambda i: {"x": i.x + 1})

    assert held_structure({"inner": {"x": 1}}, Outer) == Outer(Inner(1))
    assert held_unstructure(Outer(Inner(1))) == {"inner": {"x": 1}}

    # Fresh hooks observe the new registrations.
    assert converter.structure({"inner": {"x": 1}}, Outer) == Outer(Inner(2))


def test_converters_have_independent_caches():
    """Invalidating one converter never touches another converter."""
    first, second = Converter(), Converter()

    first_hook = first.get_structure_hook(Outer)
    second_hook = second.get_structure_hook(Outer)

    first.register_structure_hook(Inner, lambda v, t: Inner(x=v["x"] + 1))

    assert second.get_structure_hook(Outer) is second_hook
    assert first.get_structure_hook(Outer) is not first_hook


def test_invalidate_structure_hook_public_api(converter):
    before = converter.get_structure_hook(ContainerOuter)

    affected = converter.invalidate_structure_hook(Inner)

    assert Inner in affected
    assert ContainerOuter in affected
    assert int not in affected
    assert converter.get_structure_hook(ContainerOuter) is not before


def test_invalidate_unstructure_hook_public_api(converter):
    before = converter.get_unstructure_hook(ContainerOuter)

    affected = converter.invalidate_unstructure_hook(Inner)

    assert Inner in affected
    assert ContainerOuter in affected
    assert converter.get_unstructure_hook(ContainerOuter) is not before


def test_superclass_registration_invalidates_subclass_hooks():
    """singledispatch-style registration must still take priority for subclasses."""

    @attrs.define
    class Base:
        i: int

    @attrs.define
    class Sub(Base):
        pass

    converter = Converter()
    assert converter.structure({"i": 1}, Sub) == Sub(1)

    converter.register_structure_hook(Base, lambda o, t: t(o["i"] + 1))

    assert converter.structure({"i": 1}, Sub) == Sub(2)


def test_base_converter_uses_late_binding():
    """The BaseConverter dispatches field types at call time, so new hooks are
    always picked up even through hooks already handed to the caller."""
    converter = BaseConverter()
    held = converter.get_structure_hook(Outer)

    converter.register_structure_hook(Inner, lambda v, t: Inner(x=v["x"] + 3))

    assert held({"inner": {"x": 1}}, Outer) == Outer(Inner(4))


def test_direct_collection_hook_is_regenerated_after_invalidation():
    """Generated hooks that register directly (mapping and iterable factories)
    must regenerate when their element type changes, and the replacement must
    be stored directly without discarding itself on creation."""
    converter = Converter()
    first = converter.get_structure_hook(dict[str, Inner])
    assert dict[str, Inner] in converter._structure_func._direct_dispatch

    converter.register_structure_hook(Inner, lambda v, t: Inner(x=v["x"] + 1))

    second = converter.get_structure_hook(dict[str, Inner])
    assert first is not second
    # The regenerated hook is stored directly again.
    assert converter._structure_func._direct_dispatch[dict[str, Inner]] is second
    assert converter.structure({"a": {"x": 1}}, dict[str, Inner]) == {"a": Inner(2)}

    # Unstructure iterable hooks take the direct path as well.
    first_u = converter.get_unstructure_hook(list[Inner])
    assert list[Inner] in converter._unstructure_func._direct_dispatch
    converter.register_unstructure_hook(Inner, lambda i: {"x": i.x + 9})
    second_u = converter.get_unstructure_hook(list[Inner])
    assert first_u is not second_u
    assert converter._unstructure_func._direct_dispatch[list[Inner]] is second_u


def test_repeated_lookups_reuse_cached_hook(converter):
    """Unchanged types keep serving the same cached hook object."""
    first = converter.get_structure_hook(ContainerOuter)
    second = converter.get_structure_hook(ContainerOuter)
    assert first is second

    # Invalidating an unrelated type does not force regeneration.
    @attrs.define
    class Unrelated:
        flag: bool

    unrelated_hook = converter.get_structure_hook(Unrelated)
    converter.invalidate_structure_hook(Unrelated)
    assert converter.get_structure_hook(ContainerOuter) is first
    assert converter.get_structure_hook(Unrelated) is not unrelated_hook
