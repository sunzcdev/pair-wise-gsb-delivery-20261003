"""Strategy for deterministically unstructuring sets and frozensets.

Sets are unordered, so their iteration order depends on the (potentially
process-randomized) hash values of their elements. Archiving the raw
unstructured output of a set can therefore produce different wire formats for
the same data across runs. This strategy removes that source of
nondeterminism by sorting the *unstructured* elements of every set and
frozenset using a key provided by the caller.
"""

from __future__ import annotations

from collections.abc import Callable
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import NOTHING, NothingType
from typing_extensions import NoDefault

from ..cols import is_any_set
from ..errors import DeterministicSetError

if TYPE_CHECKING:
    from ..converters import BaseConverter

__all__ = ["configure_deterministic_sets"]


def _element_unstructure_hook(
    converter: BaseConverter, cl: Any
) -> Callable[[Any], Any]:
    """Resolve the unstructure hook used for the set's element type."""
    type_arg: Any = Any
    args = getattr(cl, "__args__", None)
    if args:
        type_arg = args[0]
        if isinstance(type_arg, TypeVar):
            type_arg = getattr(type_arg, "__default__", Any)
            if type_arg is NoDefault:
                type_arg = Any
    return converter.get_unstructure_hook(type_arg, cache_result=False)


def configure_deterministic_sets(
    converter: BaseConverter,
    sort_key: Callable[[Any], Any] | NothingType = NOTHING,
) -> None:
    """Deterministically unstructure sets and frozensets into sorted lists.

    The elements of every :class:`set`, :class:`frozenset` and abstract
    ``collections.abc`` set are first unstructured using the converter, and
    the resulting wire-format values are then sorted into a :class:`list`.

    :param converter: The converter to apply the strategy to. Both
        :class:`cattrs.BaseConverter` and :class:`cattrs.Converter` are
        supported, including the preconfigured converters.
    :param sort_key: A required one-argument callable applied to each
        *unstructured* element to compute its ordering key, exactly like the
        ``key`` argument of :func:`sorted`.

    Ordering rules, all independent of set iteration order:

    * Elements are ordered by ``sort_key(unstructured_element)``.
    * When two elements produce the same key (a key collision) the tie is
      broken by comparing their unstructured wire values themselves.
    * If the keys, or the wire values involved in a tie, are not mutually
      comparable, :class:`cattrs.DeterministicSetError` is raised. There is
      no fallback to :func:`repr`.

    Each concrete (generic) set type gets its own hook and the strategy keeps
    no shared mutable state between them. Empty sets unstructure to empty
    lists. Structuring is left untouched, so element type conversion and
    nested error locations are preserved.

    ..  versionadded:: NEXT
    """
    if sort_key is NOTHING:
        raise TypeError(
            "configure_deterministic_sets() requires a 'sort_key' callable "
            "that maps an unstructured element to an ordering key."
        )

    def make_deterministic_set_hook(cl: Any) -> Callable[[Any], list[Any]]:
        element_hook = _element_unstructure_hook(converter, cl)

        def unstructure_set(
            value: Any, _element_hook=element_hook, _key=sort_key
        ) -> list[Any]:
            unstructured = [_element_hook(element) for element in value]
            try:
                # Decorate with (key, wire value). Sorting the tuples orders by
                # the caller key first and deterministically breaks key ties by
                # the unstructured wire values. No repr-based fallback exists:
                # an unorderable comparison propagates as a TypeError.
                decorated = sorted((_key(item), item) for item in unstructured)
            except TypeError as exc:
                raise DeterministicSetError(
                    "Cannot deterministically order the unstructured elements "
                    f"of {cl!r}: the sort keys (or, on a key collision, the "
                    "wire values) are not mutually comparable. Provide a "
                    "'sort_key' producing mutually comparable values.",
                    exc,
                ) from exc
            return [item for _, item in decorated]

        return unstructure_set

    converter.register_unstructure_hook_factory(
        is_any_set, make_deterministic_set_hook
    )
