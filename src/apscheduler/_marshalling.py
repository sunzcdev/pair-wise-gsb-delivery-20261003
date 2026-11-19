from __future__ import annotations

import json
from collections.abc import Callable, Mapping
from datetime import tzinfo
from functools import partial
from inspect import isclass, ismethod, ismethoddescriptor
from typing import TYPE_CHECKING, Any
from zoneinfo import ZoneInfo

from ._exceptions import DeserializationError, SerializationError

if TYPE_CHECKING:
    from .abc import Serializer

_NOT_FOUND = object()
_max_search_depth = 100


def describe_type(value: Any) -> str:
    """Return a ``module.qualified_name`` description of the type of ``value``."""
    cls = type(value)
    return f"{cls.__module__}.{cls.__qualname__}"


def find_unserializable(
    probe: Callable[[Any], Any],
    value: Any,
    _seen: set[int] | None = None,
    _depth: int = 0,
) -> Any:
    """
    Find the innermost value inside ``value`` that ``probe`` cannot handle.

    The probe is first applied to the given value as a whole. If it fails, containers
    are traversed recursively in an attempt to pinpoint the offending item. If no
    offending item can be found (the failure was caused by the container itself, by
    something like a reference cycle, or by exceeding the maximum search depth), the
    container is returned.

    :param probe: a callable that raises an exception when given a value it cannot
        handle
    :param value: the value to inspect
    :return: the offending value, or :data:`_NOT_FOUND` if ``probe`` accepted ``value``

    """
    try:
        probe(value)
    except Exception:
        pass
    else:
        return _NOT_FOUND

    if _seen is None:
        _seen = set()

    if (
        _depth >= _max_search_depth
        or id(value) in _seen
        or not isinstance(value, (Mapping, list, tuple, set, frozenset))
    ):
        return value

    _seen.add(id(value))
    try:
        if isinstance(value, Mapping):
            items: list[tuple[Any, Any]] = list(value.items())
        else:
            items = list(enumerate(value))

        for key, item in items:
            for candidate in (key, item):
                if id(candidate) in _seen:
                    # A reference cycle; report the container currently being examined
                    # rather than one of its ancestors
                    continue

                found = find_unserializable(probe, candidate, _seen, _depth + 1)
                if found is not _NOT_FOUND:
                    return found
    finally:
        _seen.discard(id(value))

    # The container itself could not be handled by the probe
    return value


def _serialization_error(
    owner: str, field: str, offender: Any, *, json_only: bool = False
) -> SerializationError:
    # Deliberately never include the offending value itself (or its repr) in the
    # message, as it may contain sensitive data
    suffix = "JSON serializable" if json_only else "serializable"
    return SerializationError(
        f"Failed to serialize the {owner}'s {field}: value of type "
        f"{describe_type(offender)} is not {suffix}"
    )


def marshal_field(
    serializer: Serializer, owner: str, field: str, value: Any
) -> bytes:
    """
    Serialize a single field of a task, schedule or job.

    Any failure is converted into a :class:`SerializationError` that names the field
    (such as ``args`` or ``kwargs``) and the type of the value that could not be
    serialized, but never includes the value itself. The original exception is retained
    as the cause of the raised exception.

    :param serializer: the serializer to use
    :param owner: a human readable description of the object being serialized (e.g.
        ``"job"``)
    :param field: name of the field being serialized
    :param value: the value to serialize
    :return: the serialized value as a bytestring

    """
    try:
        return serializer.serialize(value)
    except Exception as exc:
        offender = find_unserializable(serializer.serialize, value)
        if offender is _NOT_FOUND:
            offender = value

        raise _serialization_error(owner, field, offender) from exc


def marshal_metadata(owner: str, metadata: dict[str, Any]) -> dict[str, Any]:
    """
    Verify that the given metadata can be stored as JSON.

    Metadata is stored in a JSON column (or a BSON document), so it has to be JSON
    compatible. Any failure is converted into a :class:`SerializationError` that names
    the ``metadata`` field and the type of the value that could not be serialized, but
    never includes the value itself.

    :param owner: a human readable description of the object being serialized (e.g.
        ``"job"``)
    :param metadata: the metadata dictionary to verify
    :return: the unmodified metadata dictionary

    """
    try:
        json.dumps(metadata)
    except Exception as exc:
        offender = find_unserializable(json.dumps, metadata)
        if offender is _NOT_FOUND:
            offender = metadata

        raise _serialization_error(owner, "metadata", offender, json_only=True) from exc

    return metadata


def marshal_object(obj) -> tuple[str, Any]:
    return (
        f"{obj.__class__.__module__}:{obj.__class__.__qualname__}",
        obj.__getstate__(),
    )


def unmarshal_object(ref: str, state: Any) -> Any:
    cls = callable_from_ref(ref)
    if not isinstance(cls, type):
        raise TypeError(f"{ref} is not a class")

    instance = cls.__new__(cls)
    instance.__setstate__(state)
    return instance


def marshal_timezone(value: tzinfo) -> str:
    if isinstance(value, ZoneInfo):
        return value.key
    elif hasattr(value, "zone"):  # pytz timezones
        return value.zone

    raise SerializationError(
        f"Unserializable time zone: {value!r}\n"
        f"Only time zones from the zoneinfo or pytz modules can be serialized."
    )


def unmarshal_timezone(value: str) -> ZoneInfo:
    return ZoneInfo(value)


def callable_to_ref(func: Callable) -> str:
    """
    Return a reference to the given callable.

    :raises SerializationError: if the given object is not callable, is a partial(),
        bound method, lambda or local function or does not have the ``__module__`` and
        ``__qualname__`` attributes

    """
    if isinstance(func, partial):
        raise SerializationError("Cannot create a reference to a partial()")

    if ismethod(func):
        if not isclass(func.__self__):
            raise SerializationError("Cannot create a reference to an instance method")

        return f"{func.__module__}:{func.__self__.__qualname__}.{func.__name__}"

    if ismethoddescriptor(func):
        return f"{func.__objclass__.__module__}:{func.__qualname__}"

    if not hasattr(func, "__module__"):
        raise SerializationError("Callable has no __module__ attribute")

    if not hasattr(func, "__qualname__"):
        raise SerializationError("Callable has no __qualname__ attribute")

    if "<lambda>" in func.__qualname__:
        raise SerializationError("Cannot create a reference to a lambda")

    if "<locals>" in func.__qualname__:
        raise SerializationError("Cannot create a reference to a nested function")

    return f"{func.__module__}:{func.__qualname__}"


def callable_from_ref(ref: str) -> Callable:
    """
    Return the callable pointed to by ``ref``.

    :raises DeserializationError: if the reference could not be resolved or the looked
        up object is not callable

    """
    if ":" not in ref:
        raise ValueError(f"Invalid reference: {ref}")

    modulename, rest = ref.split(":", 1)
    try:
        obj = __import__(modulename, fromlist=[rest])
    except ImportError as exc:
        raise LookupError(
            f"Error resolving reference {ref!r}: could not import module"
        ) from exc

    try:
        for name in rest.split("."):
            obj = getattr(obj, name)
    except Exception as exc:
        raise DeserializationError(
            f"Error resolving reference {ref!r}: error looking up object"
        ) from exc

    if not callable(obj):
        raise DeserializationError(
            f"{ref!r} points to an object of type "
            f"{obj.__class__.__qualname__} which is not callable"
        )

    return obj
