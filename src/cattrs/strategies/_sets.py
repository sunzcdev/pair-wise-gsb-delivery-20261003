"""Strategy for deterministically unstructuring sets and frozensets.

The default unstructuring of ``set`` and ``frozenset`` preserves the iteration
order of the source container, which depends on the elements' hash values and
therefore on process-wide hash randomization. This module provides an opt-in
strategy that unstructures sets into ordered collections, sorted by a
caller-provided key applied to the *unstructured* (wire-format) elements.
"""

from __future__ import annotations

from collections.abc import Callable, Iterable
from typing import TYPE_CHECKING, Any, TypeVar

from typing_extensions import NoDefault

from .._compat import is_frozenset, is_mutable_set
from ..errors import SetUnstructureError
from ..fns import identity

if TYPE_CHECKING:
    from ..converters import BaseConverter

__all__ = ["configure_set_unstructure", "make_set_unstructure_factory"]


def make_set_unstructure_factory(
    cl: Any,
    converter: BaseConverter,
    key: Callable[[Any], Any],
    unstructure_to: Any = None,
) -> Callable[[Iterable[Any]], Any]:
    """Build an unstructure hook that sorts a set's unstructured elements.

    :param cl: The set type (possibly generic, like ``set[int]``) to build a
        hook for.
    :param converter: The converter used to unstructure the elements.
    :param key: A one-argument callable producing the sort key. It is applied
        to each element *after* the element has been unstructured to its
        wire-format value.
    :param unstructure_to: The collection type holding the sorted result.
        Defaults to ``list``.

    .. versionadded:: NEXT
    """
    if unstructure_to is None:
        unstructure_to = list

    # Resolve an element-specific hook when the set type carries a type
    # argument, mirroring `iterable_unstructure_factory`.
    handler: Callable[[Any], Any] = converter.unstructure
    if getattr(cl, "__args__", None) not in (None, ()):
        type_arg = cl.__args__[0]
        if isinstance(type_arg, TypeVar):
            type_arg = getattr(type_arg, "__default__", Any)
            if type_arg is NoDefault:
                type_arg = Any
        element_hook = converter.get_unstructure_hook(type_arg, cache_result=False)
        if element_hook != identity:
            handler = element_hook

    def unstructure_set(
        obj: Iterable[Any],
        _handler: Callable[[Any], Any] = handler,
        _key: Callable[[Any], Any] = key,
        _seq_cl: Any = unstructure_to,
    ) -> Any:
        # Elements keep their own type conversion; only ordering is changed.
        values = [_handler(element) for element in obj]
        try:
            # Python's sort is stable: elements whose keys compare equal keep
            # the relative order they had before sorting.
            values.sort(key=_key)
        except TypeError as exc:
            # Incomparable keys are an explicit, public failure; never fall
            # back to comparing `repr` strings.
            raise SetUnstructureError(_key, values) from exc
        return _seq_cl(values)

    return unstructure_set


def configure_set_unstructure(
    converter: BaseConverter, key: Callable[[Any], Any], unstructure_to: Any = None
) -> None:
    """Deterministically unstructure ``set`` and ``frozenset`` values.

    By default, cattrs unstructures sets by reusing the source container's
    iteration order. That order derives from the elements' hashes and changes
    between processes for hash-randomized types (such as strings), so the
    resulting wire format is not reproducible. This strategy replaces the set
    and frozenset unstructure hooks so that:

    * every element is first unstructured using its own hook (so element type
      conversion and nested error locations are preserved),
    * the caller-provided ``key`` callable is applied to each *unstructured*
      element to produce a sort key,
    * the elements are sorted by those keys and emitted into an ordered
      collection (a :class:`list` by default).

    :param converter: The converter to configure. Both
        :class:`~cattrs.BaseConverter` and :class:`~cattrs.Converter` (and
        their subclasses, including the preconfigured converters) are
        supported.
    :param key: A one-argument callable returning the sort key for an
        unstructured element. All returned keys must be mutually orderable;
        otherwise a :class:`~cattrs.SetUnstructureError` is raised instead of
        silently falling back to another ordering.
    :param unstructure_to: The collection type used for the sorted result.
        Defaults to :class:`list`; pass, for example, :class:`tuple` to emit
        tuples instead.

    Sorting is *stable*: when two unstructured elements produce equal keys,
    their relative order is left untouched. This makes equal-key collisions
    predictable within a single unstructure call, but to get fully
    reproducible output across processes the ``key`` should distinguish every
    element (for example with a tuple key ending in the value itself).

    The strategy only changes unstructuring; structuring hooks and every other
    container type are left to their existing behavior. A fresh hook is
    generated and cached per distinct generic set type, so different
    parameterizations (such as ``set[int]`` and ``set[str]``) never share
    state, and empty sets simply unstructure to an empty ordered collection.

    .. versionadded:: NEXT
    """
    if not callable(key):
        msg = "The `key` argument must be a one-argument callable."
        raise TypeError(msg)

    def factory(
        cl: Any,
        _converter: BaseConverter = converter,
        _key: Callable[[Any], Any] = key,
        _unstructure_to: Any = unstructure_to,
    ) -> Callable[[Iterable[Any]], Any]:
        return make_set_unstructure_factory(
            cl, _converter, _key, unstructure_to=_unstructure_to
        )

    # A single predicate covers both mutable sets and frozensets, including
    # their typing-module and collections.abc generic forms. The dispatch
    # machinery ignores predicates raising on unrelated types.
    converter.register_unstructure_hook_factory(
        lambda cl: is_mutable_set(cl) or is_frozenset(cl), factory
    )
