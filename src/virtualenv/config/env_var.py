from __future__ import annotations

__lazy_modules__ = [
    f"{__spec__.parent}.convert",  # ty: ignore[missing-union-attribute]  # imported modules have a spec
]

from typing import TYPE_CHECKING, NamedTuple

from .convert import convert

if TYPE_CHECKING:
    from collections.abc import Mapping
    from typing import Any

    from .convert import TypeData


class EnvVarValue(NamedTuple):
    """What an environment variable contributes to an option, including when it contributes nothing.

    :param name: the full name of the variable, such as ``VIRTUALENV_NO_PERIODIC_UPDATE``
    :param status: ``set`` when the variable holds a usable value, ``empty`` when it is set but blank,
        ``invalid`` when the value cannot be converted, and ``unset`` when the variable is not present
    :param value: the converted value, ``None`` unless the status is ``set``
    :param raw: the raw value, ``None`` when the variable is unset
    :param problem: why the value is invalid, ``None`` unless the status is ``invalid``

    """

    name: str
    status: str
    value: Any  # simplint: ignore[imprecise-annotation]  # the type depends on the converter
    raw: str | None
    problem: str | None


def read_env_var(key: str, as_type: TypeData, env: Mapping[str, str]) -> EnvVarValue:
    """Read an environment variable option and report whether and why it contributed a value.

    An empty variable is reported as ``empty`` rather than silently ignored, so a configuration audit can tell it apart
    from a variable that is not set at all.

    :param key: the config key requested
    :param as_type: the type we would like to convert it to
    :param env: environment variables to use

    :returns: what the variable contributes

    """
    environ_key = f"VIRTUALENV_{key.upper()}"
    if environ_key not in env:
        return EnvVarValue(environ_key, "unset", None, None, None)
    raw = env[environ_key]
    if not raw:
        return EnvVarValue(environ_key, "empty", None, raw, None)
    try:
        value = convert(raw, as_type, f"env var {environ_key}")
    except Exception as exception:  # ruff:ignore[blind-except]  # the converter already logged the warning
        return EnvVarValue(environ_key, "invalid", None, raw, str(exception) or type(exception).__name__)
    return EnvVarValue(environ_key, "set", value, raw, None)


def get_env_var(
    key: str, as_type: TypeData, env: Mapping[str, str]
) -> tuple[Any, str] | None:  # simplint: ignore[imprecise-annotation]  # type known at run time
    """Get the environment variable option.

    :param key: the config key requested
    :param as_type: the type we would like to convert it to
    :param env: environment variables to use

    :returns: the converted value and source, or None if not set

    """
    result = read_env_var(key, as_type, env)
    if result.status == "set":
        return result.value, f"env var {result.name}"
    return None


__all__ = [
    "EnvVarValue",
    "get_env_var",
    "read_env_var",
]
