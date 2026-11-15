"""
Count the deep ``equal`` comparisons that ``uniq`` performs.

Structural fingerprints group candidates first, so deep comparisons only
happen inside a candidate bucket rather than across every pair of elements.
Fingerprints are hashable structural tuples and frozensets -- never strings
built from ``repr``/``json.dumps`` -- and every bucket is adjudicated with
the JSON-aware :func:`jsonschema._utils.equal`.

Run directly to print how the number of comparisons scales for several
nested distributions::

    python -m jsonschema.benchmarks.unique_items_comparisons
"""

import itertools

from jsonschema import _utils
from jsonschema._utils import equal as real_equal
from jsonschema._utils import unbool


class Unhashable:
    __hash__ = None

    def __init__(self, value):
        self.value = value

    def __eq__(self, other):
        return (
            isinstance(other, Unhashable) and self.value == other.value
        )


class OtherUnhashable:
    __hash__ = None

    def __init__(self, value):
        self.value = value

    def __eq__(self, other):
        return (
            isinstance(other, OtherUnhashable)
            and self.value == other.value
        )


def distinct_nested_objects(size):
    return [
        {
            "a": [index, index + 1, {"b": (index, False)}],
            "c": {"d": [index, 2, 3]},
        }
        for index in range(size)
    ]


def shared_shape_distinct_values(size):
    return [
        {"x": index, "y": [index, index], "z": {"k": index}}
        for index in range(size)
    ]


def half_structural_duplicates(size):
    base = [{"a": [1, 2, {"b": True}]} for _ in range(size // 2)]
    other = [
        {"a": [1, 2, {"b": True}], "i": index}
        for index in range(size - size // 2)
    ]
    return base + other


def nan_bearing_objects(size):
    nan = float("nan")
    return [{"v": [index, nan]} for index in range(size)]


def unhashable_single_type(size):
    return [Unhashable(index) for index in range(size)]


def unhashable_mixed_types(size):
    half = size // 2
    return [Unhashable(index) for index in range(half)] + [
        OtherUnhashable(index) for index in range(size - half)
    ]


DISTRIBUTIONS = {
    "distinct nested objects": distinct_nested_objects,
    "shared shape, distinct values": shared_shape_distinct_values,
    "half structural duplicates": half_structural_duplicates,
    "NaN nested in objects": nan_bearing_objects,
    "unhashable, single type": unhashable_single_type,
    "unhashable, mixed types": unhashable_mixed_types,
}
SIZES = (64, 128, 256, 512)


def _brute_force_uniq(container, equal):
    seen = []
    for element in container:
        for previous in seen:
            if equal(previous, element):
                return False
        seen.append(element)
    return True


def _legacy_uniq(container, equal):
    """Sort-then-compare strategy, with an O(n^2) brute-force fallback."""
    try:
        sort = sorted(unbool(element) for element in container)
        sliced = itertools.islice(sort, 1, None)
        for one, two in zip(sort, sliced):
            if equal(one, two):
                return False
    except (NotImplementedError, TypeError):
        seen = []
        for element in container:
            element = unbool(element)
            for previous in seen:
                if equal(previous, element):
                    return False
            seen.append(element)
    return True


def _count_equal_calls(implementation, elements, use_module_global):
    calls = []

    def counting_equal(one, two):
        calls.append((one, two))
        return real_equal(one, two)

    if use_module_global:
        original = _utils.equal
        _utils.equal = counting_equal
        try:
            implementation(elements)
        finally:
            _utils.equal = original
    else:
        implementation(elements, counting_equal)
    return len(calls)


def main():
    header = (
        f"{'distribution':<30}{'n':>6}"
        f"{'brute O(n^2)':>14}{'legacy':>10}{'now':>8}"
    )
    print(header)
    print("-" * len(header))

    for label, factory in DISTRIBUTIONS.items():
        for size in SIZES:
            elements = factory(size)
            brute = _count_equal_calls(
                _brute_force_uniq,
                elements,
                use_module_global=False,
            )
            legacy = _count_equal_calls(
                _legacy_uniq,
                elements,
                use_module_global=False,
            )
            now = _count_equal_calls(
                _utils.uniq,
                elements,
                use_module_global=True,
            )
            print(
                f"{label:<30}{size:>6}{brute:>14}{legacy:>10}{now:>8}",
            )
        print()


if __name__ == "__main__":
    main()
