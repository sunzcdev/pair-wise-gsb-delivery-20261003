"""
Request extensions provide an untyped space where additional information,
such as tracing or routing state, may be associated with a request.

When a client derives a new request from an existing one, such as when
following a redirect or re-sending a request within an authentication
flow, the extensions are carried across using an explicit copy protocol:

* Keys reserved for `httpx` internals, such as `"timeout"`, have fixed
  rules. They are always inherited as-is, and user-supplied copy
  callbacks cannot override them.
* Values implementing `__httpx_extension_copy__()` provide a derived
  copy of themselves, so that mutable state declared as independent is
  not accidentally shared between the original request and the new one.
* Plain immutable values are inherited directly.
* Anything else raises `ExtensionCopyError`, rather than silently
  sharing mutable state across requests.
"""

from __future__ import annotations

import functools
import typing

from ._exceptions import ExtensionCopyError
from ._types import RequestExtensions

__all__ = ["copy_request_extensions"]


RESERVED_REQUEST_EXTENSIONS = frozenset(
    {"timeout", "trace", "sni_hostname", "target"}
)

ExtensionCopyCallback = typing.Callable[[typing.Any], typing.Any]


def _is_immutable(value: typing.Any) -> bool:
    if isinstance(value, (str, bytes, int, float, complex, bool, type(None))):
        return True
    if isinstance(value, (tuple, frozenset)):
        return all(_is_immutable(item) for item in value)
    return False


def _derive_copy(key: str, derive: typing.Callable[[], typing.Any]) -> typing.Any:
    try:
        return derive()
    except ExtensionCopyError:
        raise
    except Exception as exc:
        raise ExtensionCopyError(
            f"Failed to derive a copy of request extension {key!r}."
        ) from exc


def copy_request_extensions(
    extensions: RequestExtensions,
    copy_callbacks: typing.Mapping[str, ExtensionCopyCallback] | None = None,
) -> dict[str, typing.Any]:
    """
    Return a copy of request `extensions`, suitable for a new request that
    has been derived from an existing one, such as a redirect follow-up
    or an authentication retry.

    Reserved keys are always inherited as-is. Other values are copied by
    an optional per-key `copy_callbacks` entry, or by the value's own
    `__httpx_extension_copy__()` method, or inherited directly if the
    value is immutable. Mutable values that provide no way to derive a
    copy raise `ExtensionCopyError`.
    """
    callbacks = {} if copy_callbacks is None else dict(copy_callbacks)
    reserved = RESERVED_REQUEST_EXTENSIONS.intersection(callbacks)
    if reserved:
        raise ValueError(
            "Copy callbacks may not be registered for reserved request "
            f"extension keys: {sorted(reserved)!r}."
        )

    copied: dict[str, typing.Any] = {}
    for key, value in extensions.items():
        if key in RESERVED_REQUEST_EXTENSIONS:
            copied[key] = value
        elif key in callbacks:
            copied[key] = _derive_copy(key, functools.partial(callbacks[key], value))
        elif hasattr(value, "__httpx_extension_copy__"):
            copied[key] = _derive_copy(key, value.__httpx_extension_copy__)
        elif _is_immutable(value):
            copied[key] = value
        else:
            raise ExtensionCopyError(
                f"Request extension {key!r} holds a mutable value that does "
                "not implement '__httpx_extension_copy__()'. Mutable extension "
                "values must declare how they are copied, so that derived "
                "requests do not accidentally share state."
            )
    return copied
