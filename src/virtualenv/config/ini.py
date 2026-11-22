from __future__ import annotations

__lazy_modules__ = [
    "configparser",
    "pathlib",
    "platformdirs",
    f"{__spec__.parent}.convert",  # ty: ignore[missing-union-attribute]  # imported modules have a spec
]

import logging
import os
from configparser import ConfigParser, Error
from pathlib import Path
from typing import TYPE_CHECKING, ClassVar, Final, NamedTuple

from platformdirs import user_config_dir

from .convert import convert

if TYPE_CHECKING:
    from collections.abc import Mapping
    from typing import Any

    from .convert import TypeData

_LOGGER: Final[logging.Logger] = logging.getLogger(__name__)


class IniValue(NamedTuple):
    """What the config file contributes to an option, including when it contributes nothing.

    :param name: the key that matched, which differs from the requested key for a legacy hyphenated one
    :param status: ``set``, ``invalid`` or ``unset``
    :param value: the converted value, ``None`` unless the status is ``set``
    :param raw: the raw value, ``None`` when the key is not set
    :param problem: why the value is invalid, ``None`` unless the status is ``invalid``
    :param legacy: ``True`` when the key used the historical hyphenated spelling

    """

    name: str
    status: str
    value: Any  # simplint: ignore[imprecise-annotation]  # the type depends on the converter
    raw: str | None
    problem: str | None
    legacy: bool


class IniConfig:
    """The ``virtualenv`` section of the config file, found through the environment or the default location.

    :param env: the environment variables, ``None`` for the process environment

    :var VIRTUALENV_CONFIG_FILE_ENV_VAR: the environment variable naming the config file
    :var STATE: the description of the config file state for each state value
    :var section: the name of the section read from the config file
    :var is_env_var: ``True`` when the config file was named by the environment variable
    :var config_file: the path of the config file
    :var has_config_file: whether the config file exists and was read
    :var has_virtualenv_section: whether the config file has a ``virtualenv`` section
    :var config_parser: the parser of the config file

    """

    VIRTUALENV_CONFIG_FILE_ENV_VAR: ClassVar[str] = "VIRTUALENV_CONFIG_FILE"
    STATE: ClassVar[dict[bool | None, str]] = {None: "failed to parse", True: "active", False: "missing"}

    section = "virtualenv"

    def __init__(self, env: Mapping[str, str] | None = None) -> None:
        env = os.environ if env is None else env
        # an empty value counts as unset, as it does for the VIRTUALENV_<OPTION> variables, instead of naming the cwd
        config_file = env.get(self.VIRTUALENV_CONFIG_FILE_ENV_VAR) or None
        self.is_env_var = config_file is not None
        if config_file is None:
            config_file = Path(user_config_dir(appname="virtualenv", appauthor="pypa")) / "virtualenv.ini"
        else:
            config_file = Path(config_file)
        self.config_file = config_file
        self._cache = {}

        self.has_config_file: bool | None = None
        self.has_virtualenv_section = False
        # no interpolation: values are literal, as env vars and flags are, so a % in a prompt or path survives
        self.config_parser: Final[ConfigParser] = ConfigParser(interpolation=None)
        exception = None
        try:
            self.has_config_file = self.config_file.exists()
            if self.has_config_file:
                self.config_file = self.config_file.resolve()
                self._load()
                self.has_virtualenv_section = self.config_parser.has_section(self.section)
        except (OSError, UnicodeError, Error) as exc:
            self.has_config_file = None
            exception = exc
        if exception is not None:
            _LOGGER.error("failed to read config file %s because %r", config_file, exception)

    def _load(self) -> None:
        with self.config_file.open("rt", encoding="utf-8-sig") as file_handler:
            return self.config_parser.read_file(file_handler)

    def __bool__(self) -> bool:
        return bool(self.has_config_file) and bool(self.has_virtualenv_section)

    @property
    def epilog(self) -> str:
        """The help text line that describes the config file and whether it is active."""
        return (
            f"\nconfig file {self.config_file} {self.STATE[self.has_config_file]} "
            f"(change{'d' if self.is_env_var else ''} via env var {self.VIRTUALENV_CONFIG_FILE_ENV_VAR})"
        )

    def get(
        self, key: str, as_type: TypeData
    ) -> tuple[Any, str] | None:  # simplint: ignore[imprecise-annotation]  # type known at run time
        """Return the value of a key from the config file, reading the file once.

        :param key: the option name
        :param as_type: the converter to apply to the value

        :returns: the converted value and the source it came from, ``None`` when the key is not set

        """
        result = self.read(key, as_type)
        if result.status == "set":
            return result.value, "file"
        return None

    def read(self, key: str, as_type: TypeData) -> IniValue:
        """Read a key and report whether and why it contributed a value.

        A key the file does not hold is reported as ``unset`` and one whose value cannot be converted as ``invalid``,
        so a configuration audit can tell a missing key apart from a broken one.

        :param key: the option name
        :param as_type: the converter to apply to the value

        :returns: what the config file contributes

        """
        cache_key = key, as_type
        if cache_key in self._cache:
            return self._cache[cache_key]
        result = self._read(key, as_type)
        self._cache[cache_key] = result
        return result

    def _read(self, key: str, as_type: TypeData) -> IniValue:
        if not self:
            return IniValue(key, "unset", None, None, None, False)
        matched, legacy = self._resolve_key(key)
        if matched is None:
            return IniValue(key, "unset", None, None, None, False)
        # an option without a value, as configparser sees `key =` followed by another key, reads as an empty string,
        # which stays a value: an empty list option means no items, and a boolean one then fails to convert
        raw_value = self.config_parser.get(self.section, matched)
        source = f"config file {self.config_file} key {matched}"
        try:
            value = convert(raw_value, as_type, source)
        # a value that fails conversion reads as unset instead of failing the run, but the audit still names it
        except Exception as exception:  # ruff:ignore[blind-except]
            return IniValue(matched, "invalid", None, raw_value, str(exception) or type(exception).__name__, legacy)
        return IniValue(matched, "set", value, raw_value, None, legacy)

    def _resolve_key(self, key: str) -> tuple[str | None, bool]:
        """Return the key the file holds for the option, accepting the historical hyphenated spelling.

        :param key: the option name as the parser knows it, with underscores

        :returns: the stored key and whether it is the historical hyphenated one, or ``None`` when the file has neither

        """
        # configparser lower-cases option names on read, so compare in lower case too
        stored = self.config_parser.options(self.section) if self.has_virtualenv_section else []
        normal = key.lower()
        if normal in stored:
            return normal, False
        legacy = normal.replace("_", "-")
        if legacy in stored:
            return legacy, True
        return None, False


__all__ = [
    "IniConfig",
    "IniValue",
]
