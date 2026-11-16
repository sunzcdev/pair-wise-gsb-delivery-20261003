"""Tests for the deterministic set unstructuring strategy."""

from collections.abc import MutableSet as AbcMutableSet
from collections.abc import Set as AbcSet
from typing import FrozenSet, Set

import pytest
from attrs import define

from cattrs import (
    BaseConverter,
    Converter,
    IterableValidationError,
    IterableValidationNote,
    SetUnstructureError,
)
from cattrs.strategies import configure_set_unstructure, make_set_unstructure_factory


def test_sorts_mutable_sets_and_frozensets(converter: BaseConverter) -> None:
    """Both set and frozenset are unstructured into sorted ordered collections."""
    configure_set_unstructure(converter, key=lambda v: v)

    assert converter.unstructure({3, 1, 2}, unstructure_as=Set[int]) == [1, 2, 3]
    assert converter.unstructure(
        frozenset({3, 1, 2}), unstructure_as=FrozenSet[int]
    ) == [1, 2, 3]
    # Bare (unparameterized) sets are handled as well.
    assert converter.unstructure({3, 1, 2}) == [1, 2, 3]
    assert converter.unstructure(frozenset({3, 1, 2})) == [1, 2, 3]


def test_handles_abc_and_typing_generic_aliases(converter: BaseConverter) -> None:
    """The predicate matches typing-module and collections.abc set aliases."""
    configure_set_unstructure(converter, key=lambda v: v)

    assert converter.unstructure({3, 1, 2}, unstructure_as=AbcSet[int]) == [1, 2, 3]
    assert converter.unstructure({3, 1, 2}, unstructure_as=AbcMutableSet[int]) == [
        1,
        2,
        3,
    ]
    assert converter.unstructure(
        frozenset({3, 1, 2}), unstructure_as=FrozenSet[int]
    ) == [1, 2, 3]


def test_deterministic_across_hash_randomization() -> None:
    """String sets produce the same output regardless of PYTHONHASHSEED.

    This test is a safety net: the subprocess invocations in the test suite
    would exercise the hash seeds, but the ordering guarantee here is what
    matters. The strategy must never rely on set iteration order.
    """
    converter = Converter()
    configure_set_unstructure(converter, key=lambda v: v)

    values = {"cherry", "apple", "banana"}
    first = converter.unstructure(values, unstructure_as=Set[str])
    assert first == ["apple", "banana", "cherry"]
    # Repeated calls in the same process are obviously stable too.
    assert converter.unstructure(values, unstructure_as=Set[str]) == first


def test_key_runs_on_unstructured_wire_values(converter: BaseConverter) -> None:
    """The sort key is applied after element conversion, on wire values."""

    @define(frozen=True)
    class Box:
        n: int

    converter.register_unstructure_hook(Box, lambda box: {"n": box.n})
    configure_set_unstructure(converter, key=lambda wire: wire["n"])

    result = converter.unstructure({Box(3), Box(1), Box(2)}, unstructure_as=Set[Box])
    assert result == [{"n": 1}, {"n": 2}, {"n": 3}]


def test_element_type_conversion_is_preserved(converter: BaseConverter) -> None:
    """Element hooks still run; only the container ordering changes."""

    @define(frozen=True)
    class Item:
        v: int

    converter.register_unstructure_hook(Item, lambda item: item.v * 10)
    configure_set_unstructure(converter, key=lambda v: v)

    assert converter.unstructure({Item(2), Item(1)}, unstructure_as=Set[Item]) == [
        10,
        20,
    ]


def test_empty_sets(converter: BaseConverter) -> None:
    """Empty sets unstructure to empty ordered collections."""
    configure_set_unstructure(converter, key=lambda v: v)

    assert converter.unstructure(set(), unstructure_as=Set[int]) == []
    assert converter.unstructure(frozenset(), unstructure_as=FrozenSet[int]) == []
    assert converter.unstructure(set()) == []


def test_incomparable_keys_raise_public_error(converter: BaseConverter) -> None:
    """Incomparable sort keys raise SetUnstructureError instead of a repr fallback."""
    configure_set_unstructure(converter, key=lambda v: v)

    with pytest.raises(SetUnstructureError) as exc_info:
        converter.unstructure({1, "a"}, unstructure_as=Set[object])

    # The original comparison failure remains available as the cause.
    assert isinstance(exc_info.value.__cause__, TypeError)
    # The values that could not be sorted are publicly exposed.
    assert set(exc_info.value.values) == {1, "a"}
    assert exc_info.value.key is not None
    # The message explicitly rules out a repr-based fallback.
    assert "repr" in str(exc_info.value)
    assert "not" in str(exc_info.value)


def test_key_function_typeerror_is_wrapped(converter: BaseConverter) -> None:
    """A TypeError raised by the key callable itself is reported publicly too."""

    def flaky_key(value: object) -> object:
        if value == 2:
            raise TypeError("boom")
        return value

    configure_set_unstructure(converter, key=flaky_key)

    with pytest.raises(SetUnstructureError) as exc_info:
        converter.unstructure({1, 2, 3}, unstructure_as=Set[int])
    assert isinstance(exc_info.value.__cause__, TypeError)


def test_stable_handling_of_equal_keys(converter: BaseConverter) -> None:
    """Equal sort keys collide without error and keep a stable relative order."""
    configure_set_unstructure(converter, key=lambda wire: wire[0])

    values = {("b", 2), ("a", 9), ("a", 1)}
    result = converter.unstructure(values, unstructure_as=Set[tuple[str, int]])

    # The "a" entries come first (equal key), and their pre-sort relative order
    # is retained by Python's stable sort.
    assert result[0][0] == "a" and result[1][0] == "a"
    assert result[2] == ("b", 2)


def test_distinguishing_tuple_key_is_fully_deterministic(
    converter: BaseConverter,
) -> None:
    """A value-level tiebreak in the key removes any collision ambiguity."""
    configure_set_unstructure(converter, key=lambda wire: (wire[0], wire[1]))

    values = {("b", 2), ("a", 9), ("a", 1)}
    assert converter.unstructure(values, unstructure_as=Set[tuple[str, int]]) == [
        ("a", 1),
        ("a", 9),
        ("b", 2),
    ]


def test_unstructure_to_override(converter: BaseConverter) -> None:
    """The sorted result can be emitted into another ordered collection."""
    configure_set_unstructure(converter, key=lambda v: v, unstructure_to=tuple)

    assert converter.unstructure({3, 1, 2}, unstructure_as=Set[int]) == (1, 2, 3)


def test_generic_instances_do_not_share_state(converter: BaseConverter) -> None:
    """Hooks for different parameterized set types are independent."""

    @define(frozen=True)
    class Item:
        v: int

    converter.register_unstructure_hook(Item, lambda item: item.v * 10)
    configure_set_unstructure(converter, key=lambda v: v)

    int_hook = converter.get_unstructure_hook(Set[int])
    item_hook = converter.get_unstructure_hook(Set[Item])

    assert int_hook is not item_hook
    assert int_hook({2, 1}) == [1, 2]
    assert item_hook({Item(2), Item(1)}) == [10, 20]


def test_structuring_and_other_containers_untouched() -> None:
    """The strategy only changes unstructuring; structure hooks stay default."""
    converter = Converter()
    configure_set_unstructure(converter, key=lambda v: v)

    structured = converter.structure([1, 2, 3], Set[int])
    assert structured == {1, 2, 3} and isinstance(structured, set)

    frozen = converter.structure([1, 2, 3], FrozenSet[int])
    assert frozen == frozenset({1, 2, 3}) and isinstance(frozen, frozenset)

    # Lists keep their original ordering policy.
    assert converter.unstructure([3, 1, 2]) == [3, 1, 2]


def test_nested_structure_error_positions_preserved() -> None:
    """Structuring bad elements of a sorted set payload keeps index notes."""
    converter = Converter(detailed_validation=True)
    configure_set_unstructure(converter, key=lambda v: v)

    with pytest.raises(IterableValidationError) as exc_info:
        converter.structure([1, "x", 3], Set[int])

    indices = [
        note.index
        for sub in exc_info.value.exceptions
        for note in getattr(sub, "__notes__", [])
        if isinstance(note, IterableValidationNote)
    ]
    assert 1 in indices


def test_nested_sets_inside_attrs_class() -> None:
    """The strategy applies to sets nested inside attrs classes too."""

    @define
    class Holder:
        items: Set[int]

    converter = Converter()
    configure_set_unstructure(converter, key=lambda v: v)

    assert converter.unstructure(Holder({3, 1, 2})) == {"items": [1, 2, 3]}


def test_default_converter_keeps_existing_container_policy() -> None:
    """Without the strategy, sets still unstructure to sets in iteration order."""
    converter = Converter()

    result = converter.unstructure({3, 1, 2}, unstructure_as=Set[int])
    assert isinstance(result, set) and result == {1, 2, 3}

    frozen_result = converter.unstructure(
        frozenset({3, 1, 2}), unstructure_as=FrozenSet[int]
    )
    assert isinstance(frozen_result, frozenset)


def test_non_callable_key_rejected() -> None:
    """The key must be callable."""
    with pytest.raises(TypeError):
        configure_set_unstructure(Converter(), key=42)  # type: ignore[arg-type]


def test_reapplying_strategy_uses_latest_key() -> None:
    """Registering the strategy again overrides the previous hooks."""
    converter = Converter()
    configure_set_unstructure(converter, key=lambda v: -v)
    configure_set_unstructure(converter, key=lambda v: v)

    assert converter.unstructure({1, 2, 3}, unstructure_as=Set[int]) == [1, 2, 3]


def test_make_factory_directly() -> None:
    """The factory helper is usable on its own."""
    converter = Converter()
    hook = make_set_unstructure_factory(Set[int], converter, key=lambda v: v)

    assert hook({3, 1, 2}) == [1, 2, 3]


def test_unhashable_wire_values_supported() -> None:
    """Wire values need not be hashable; only the sort key has to be orderable."""

    @define(frozen=True)
    class Box:
        n: int

    converter = Converter()
    converter.register_unstructure_hook(Box, lambda box: {"n": box.n})
    configure_set_unstructure(converter, key=lambda wire: wire["n"])

    result = converter.unstructure({Box(2), Box(1)}, unstructure_as=Set[Box])
    assert result == [{"n": 1}, {"n": 2}]
    # The output is a list of dicts, which could never live inside a set.
    assert all(isinstance(item, dict) for item in result)
