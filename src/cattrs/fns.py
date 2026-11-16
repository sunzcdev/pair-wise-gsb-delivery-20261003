"""Useful internal functions."""

from typing import Any, Callable, NoReturn, TypeVar

from ._compat import TypeAlias
from .errors import StructureHandlerNotFoundError

T = TypeVar("T")

Predicate: TypeAlias = Callable[[Any], bool]
"""A predicate function determines if a type can be handled."""


def identity(obj: T) -> T:
    """The identity function."""
    return obj


def raise_error(_, cl: Any) -> NoReturn:
    """At the bottom of the condition stack, we explode if we can't handle it."""
    msg = f"Unsupported type: {cl!r}. Register a structure hook for it."
    raise StructureHandlerNotFoundError(msg, type_=cl)


def default_unstructure_fallback_factory(_: Any) -> Callable[[T], T]:
    """The default unstructure fallback factory: always the identity hook.

    This is a named function (rather than an inline ``lambda``) so tooling,
    like the planning API, can recognize that a type only resolves to the
    built-in passthrough behavior.
    """
    return identity


def default_structure_fallback_factory(type: Any) -> Callable[..., NoReturn]:
    """The default structure fallback factory.

    No structure hook can be produced for an unsupported type, so this raises
    eagerly (when the hook is requested) rather than returning a hook. It is a
    named function (rather than an inline ``lambda``) so tooling, like the
    planning API, can recognize that a type reached the fallback and therefore
    has no structure handler.
    """
    return raise_error(None, type)
