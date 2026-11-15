"""Tests for the deterministic sets strategy."""

from collections.abc import Set as AbcSet
from typing import FrozenSet, Set

import pytest
from attrs import frozen

from cattrs import BaseConverter, DeterministicSetError
from cattrs.errors import IterableValidationError, IterableValidationNote
from cattrs.strategies import configure_deterministic_sets


@frozen
class Item:
    name: str
    rank: int


@frozen
class Box:
    items: Set[int]


def test_requires_sort_key() -> None:
    """The strategy is opt-in and demands an explicit sort key."""
    converter = BaseConverter()
    with pytest.raises(TypeError):
        configure_deterministic_sets(converter)


def test_set_unstructured_as_sorted_list(converter: BaseConverter) -> None:
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    assert converter.unstructure({3, 1, 2}, unstructure_as=Set[int]) == [1, 2, 3]


def test_frozenset_unstructured_as_sorted_list(converter: BaseConverter) -> None:
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    result = converter.unstructure(
        frozenset({3, 1, 2}), unstructure_as=FrozenSet[int]
    )
    assert result == [1, 2, 3]
    assert isinstance(result, list)


def test_abstract_set_unstructured_as_sorted_list(
    converter: BaseConverter,
) -> None:
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    assert (
        converter.unstructure({3, 1, 2}, unstructure_as=AbcSet[int]) == [1, 2, 3]
    )


def test_bare_set_unstructured_as_sorted_list(converter: BaseConverter) -> None:
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    assert converter.unstructure({3, 1, 2}) == [1, 2, 3]


def test_empty_sets_do_not_share_state(converter: BaseConverter) -> None:
    """Empty sets unstructure to fresh, independent lists."""
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    first = converter.unstructure(set(), unstructure_as=Set[int])
    second = converter.unstructure(set(), unstructure_as=Set[int])
    assert first == [] and second == []
    assert first is not second
    first.append(1)
    assert converter.unstructure(set(), unstructure_as=Set[int]) == []


def test_sorts_converted_wire_values_with_caller_key(
    converter: BaseConverter,
) -> None:
    """The key runs on unstructured values, not on the original elements."""
    configure_deterministic_sets(converter, sort_key=lambda d: d["rank"])
    result = converter.unstructure(
        {Item("b", 2), Item("a", 1)}, unstructure_as=Set[Item]
    )
    assert result == [{"name": "a", "rank": 1}, {"name": "b", "rank": 2}]


def test_key_collision_tie_broken_by_wire_value(
    converter: BaseConverter,
) -> None:
    """Equal keys are resolved by comparing the wire values."""
    configure_deterministic_sets(converter, sort_key=lambda t: t[0])
    result = converter.unstructure(
        {(1, "z"), (1, "a"), (0, "m")}, unstructure_as=Set[tuple]
    )
    # BaseConverter keeps tuples, Converter unstructures them into lists,
    # but the collision resolution order is identical in both wire formats.
    assert result[0] in ((0, "m"), [0, "m"])
    assert result[1] in ((1, "a"), [1, "a"])
    assert result[2] in ((1, "z"), [1, "z"])


def test_incomparable_keys_raise_dedicated_error(converter: BaseConverter) -> None:
    """Unorderable keys raise DeterministicSetError instead of a repr fallback."""
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    with pytest.raises(DeterministicSetError) as exc_info:
        converter.unstructure({1, "a"}, unstructure_as=Set[object])
    assert isinstance(exc_info.value.cause, TypeError)
    assert exc_info.value.__cause__ is exc_info.value.cause


def test_incomparable_wire_values_on_collision_raise(
    converter: BaseConverter,
) -> None:
    """A key tie where wire values are unorderable also raises."""
    configure_deterministic_sets(converter, sort_key=lambda d: d["rank"])
    with pytest.raises(DeterministicSetError):
        converter.unstructure(
            {Item("z", 1), Item("a", 1)}, unstructure_as=Set[Item]
        )


def test_failing_key_callable_raises_dedicated_error(
    converter: BaseConverter,
) -> None:
    def bad_key(value):
        raise TypeError("boom")

    configure_deterministic_sets(converter, sort_key=bad_key)
    with pytest.raises(DeterministicSetError) as exc_info:
        converter.unstructure({1, 2}, unstructure_as=Set[int])
    assert str(exc_info.value.cause) == "boom"


def test_element_unstructure_error_propagates(converter: BaseConverter) -> None:
    """Errors raised while converting elements are not swallowed or rewrapped."""

    class Broken:
        pass

    def raise_when_unstructuring(_value: Broken) -> None:
        raise ValueError("unstructure failed")

    converter.register_unstructure_hook(Broken, raise_when_unstructuring)
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    with pytest.raises(ValueError, match="unstructure failed"):
        converter.unstructure({Broken()}, unstructure_as=Set[Broken])


def test_structuring_untouched(converter: BaseConverter) -> None:
    """The strategy only affects unstructuring; structure behavior is default."""
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    structured = converter.structure([1, 2, 3], Set[int])
    assert structured == {1, 2, 3}
    assert isinstance(structured, set)

    frozen = converter.structure([1, 2, 3], FrozenSet[int])
    assert frozen == frozenset({1, 2, 3})
    assert isinstance(frozen, frozenset)


def test_element_conversion_and_nested_error_location_preserved(
    converter: BaseConverter,
) -> None:
    """Structuring still converts elements and reports the element position."""
    if not converter.detailed_validation:
        pytest.skip("Detailed validation is disabled.")
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    with pytest.raises(IterableValidationError) as exc_info:
        converter.structure(["a", 2], Set[int])

    error = exc_info.value.exceptions[0]
    note = next(
        note
        for note in getattr(error, "__notes__", [])
        if isinstance(note, IterableValidationNote)
    )
    assert note.index == 0
    assert note.type is int


def test_distinct_generic_instances_get_independent_hooks(
    converter: BaseConverter,
) -> None:
    """Hooks are per concrete generic type with no shared mutable state."""
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    int_hook = converter.get_unstructure_hook(Set[int])
    frozen_hook = converter.get_unstructure_hook(FrozenSet[int])
    assert int_hook is not frozen_hook
    # Repeated lookups return the cached hook for the same type.
    assert converter.get_unstructure_hook(Set[int]) is int_hook


def test_nested_sets_in_attrs_are_sorted(converter: BaseConverter) -> None:
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    assert converter.unstructure(Box({3, 1, 2})) == {"items": [1, 2, 3]}
    assert converter.structure({"items": [1, 2, 3]}, Box) == Box({1, 2, 3})


def test_disabled_keeps_existing_container_policy() -> None:
    """Without the strategy, sets and frozensets keep their own types."""
    converter = BaseConverter()
    assert converter.unstructure({1, 2, 3}, unstructure_as=Set[int]) == {1, 2, 3}
    assert isinstance(
        converter.unstructure({1, 2, 3}, unstructure_as=Set[int]), set
    )
    assert isinstance(
        converter.unstructure(
            frozenset({1, 2, 3}), unstructure_as=FrozenSet[int]
        ),
        frozenset,
    )


def test_strategy_survives_copy() -> None:
    """The registered factory is carried over by Converter.copy()."""
    converter = BaseConverter()
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    copied = converter.copy()
    assert copied.unstructure({3, 1, 2}, unstructure_as=Set[int]) == [1, 2, 3]


def test_output_is_independent_of_input_order(converter: BaseConverter) -> None:
    """Different element orders unstructure to the same sorted payload."""
    configure_deterministic_sets(converter, sort_key=lambda v: v)
    assert converter.unstructure({2, 1, 3}, unstructure_as=Set[int]) == (
        converter.unstructure({3, 2, 1}, unstructure_as=Set[int])
    )
