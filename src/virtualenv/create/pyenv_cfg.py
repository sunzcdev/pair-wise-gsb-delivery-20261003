from __future__ import annotations

__lazy_modules__ = [
    "collections",
    "virtualenv.util.path",
    "virtualenv.util.text",
]

import logging
import os
import re
from collections import Counter, OrderedDict
from typing import TYPE_CHECKING, Final

from virtualenv.util.path import write_file
from virtualenv.util.text import collapse_line_boundaries

if TYPE_CHECKING:
    from pathlib import Path

LOGGER: Final[logging.Logger] = logging.getLogger(__name__)  # simplint: ignore[module-variable-prefix]  # existing name

# a plain value such as 3.14 or true would otherwise turn into a path whenever the working directory has an entry so
# named
_PATH_KEYS: Final[frozenset[str]] = frozenset({
    "home",
    "executable",
    "base-prefix",
    "base-exec-prefix",
    "base-executable",
    "venvlauncher_command",
})

#: The byte order mark some tools prepend to the file; kept intact across refreshes.
_BOM: Final[str] = "\ufeff"
#: The line boundaries the interpreter itself recognizes when reading the file (universal newlines).
_LINE_ENDING: Final[re.Pattern[str]] = re.compile(r"\r\n|\r|\n")


class PyEnvCfg:
    """The ``pyvenv.cfg`` file of an environment, read into ordered key-value pairs.

    :param content: the key-value pairs in file order
    :param path: the location of the file

    """

    def __init__(self, content: OrderedDict[str, str], path: Path) -> None:
        self.content = content
        self.path = path

    @classmethod
    def from_folder(cls, folder: Path) -> PyEnvCfg:
        """Load the ``pyvenv.cfg`` of the environment in the given folder.

        :param folder: the environment folder

        :returns: the parsed configuration, empty when the file does not exist

        """
        return cls.from_file(folder / "pyvenv.cfg")

    @classmethod
    def from_file(cls, path: Path) -> PyEnvCfg:
        """Load the ``pyvenv.cfg`` at the given path.

        :param path: the location of the file

        :returns: the parsed configuration, empty when the file does not exist

        """
        return PyEnvCfg(cls._read_values(path) if path.exists() else OrderedDict(), path)

    @staticmethod
    def _read_values(path: Path) -> OrderedDict[str, str]:
        _validate_path(path)
        content = OrderedDict()
        # utf-8-sig so a byte order mark does not become part of the first key
        try:
            text = path.read_text(encoding="utf-8-sig")
        except UnicodeDecodeError as exception:
            msg = f"cannot reliably read {path}: the configuration is not valid UTF-8 ({exception})"
            raise ValueError(msg) from exception
        for line in text.splitlines():
            key, separator, value = line.partition("=")
            if not separator or (key := key.strip()).startswith("#"):
                continue
            value = value.strip()
            if len(value) > 1 and value[0] in {"'", '"'} and value[0] == value[-1]:
                value = value[1:-1]
            content[key] = value
        return content

    def __setitem__(self, key: str, value: str) -> None:
        self.content[key] = value

    def __getitem__(self, key: str) -> str:
        return self.content[key]

    def __contains__(self, item: str) -> bool:
        return item in self.content

    def __repr__(self) -> str:
        return f"{self.__class__.__name__}(path={self.path})"

    def write(self, preserve: bool = False) -> None:
        """Write the key-value pairs to the file, quoting the prompt and absolutizing existing paths.

        :param preserve: keep the text of an existing file - comments, foreign keys, byte order mark and line
            endings - and change only the lines of the keys held here, refreshing every occurrence of a key so
            readers adopting the first or the last one all see the new value, and adding missing keys at the
            end; when the existing file cannot be parsed reliably a :exc:`ValueError` is raised and the file
            is left untouched

        """
        _validate_path(self.path)
        LOGGER.debug("write %s", self.path)
        if preserve and self.path.exists():
            text = self._refreshed_text()
        else:
            text = "".join(f"{self._line(key, value)}\n" for key, value in self.content.items())
        # encoding happens before the file is touched, so a failure leaves any existing file readable
        write_file(self.path, text.encode("utf-8"))

    def _line(self, key: str, value: str) -> str:
        if key == "prompt" and value:
            normalized_value = f'"{value}"'
        elif key in _PATH_KEYS and os.path.exists(value):
            # abspath rather than realpath keeps symlinked prefixes intact, as venv does (#2770)
            normalized_value = os.path.abspath(value)
        else:
            normalized_value = value
        line = f"{collapse_line_boundaries(key)} = {collapse_line_boundaries(normalized_value)}"
        LOGGER.debug("\t%r", line)
        return line

    def _refreshed_text(self) -> str:
        """Render the pairs onto the current file text, keeping every line this configuration does not own.

        :raises ValueError: when the current file is not valid UTF-8 and so cannot be reliably updated

        """
        raw = self.path.read_bytes()
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError as exception:
            msg = f"cannot reliably update {self.path}: the existing configuration is not valid UTF-8 ({exception})"
            raise ValueError(msg) from exception
        bom = text.startswith(_BOM)
        if bom:
            text = text[len(_BOM) :]
        lines = _split_lines(text)
        # consumers disagree on duplicates: getpath adopts the last occurrence of a key while the Windows venv
        # launcher adopts the first, so every occurrence of a key held here is refreshed for the new value to
        # actually take effect
        remaining = OrderedDict(self.content)
        endings = [ending for _body, ending in lines if ending]
        newline = Counter(endings).most_common(1)[0][0] if endings else "\n"
        out: list[str] = []
        for body, ending in lines:
            key = _line_key(body)
            if key is not None and key in self.content:
                remaining.pop(key, None)
                out.append(self._line(key, self.content[key]) + ending)
            else:
                out.append(body + ending)
        if remaining:
            if out and not lines[-1][1]:  # the last line has no ending of its own, give it one before appending
                out[-1] += newline
            out.extend(f"{self._line(key, value)}{newline}" for key, value in remaining.items())
        return (_BOM if bom else "") + "".join(out)

    def refresh(self) -> OrderedDict[str, str]:
        """Re-read the key-value pairs from the file, discarding in-memory changes.

        :returns: the key-value pairs read from the file

        """
        self.content = self._read_values(self.path)
        return self.content

    def update(self, other: dict[str, str]) -> PyEnvCfg:
        """Merge the given pairs into the configuration.

        :param other: the key-value pairs to add or overwrite

        :returns: this configuration, to allow chaining

        """
        self.content.update(other)
        return self


def _validate_path(path: Path) -> None:
    if path.exists() and not path.is_file():
        msg = f"configuration path is not a regular file: {path}"
        raise ValueError(msg)


def _split_lines(text: str) -> list[tuple[str, str]]:
    """Split text into ``(body, ending)`` pairs on the line boundaries the interpreter recognizes."""
    lines: list[tuple[str, str]] = []
    start = 0
    for match in _LINE_ENDING.finditer(text):
        lines.append((text[start : match.start()], match.group()))
        start = match.end()
    if start < len(text):
        lines.append((text[start:], ""))
    return lines


def _line_key(body: str) -> str | None:
    """Return the key a line assigns, ``None`` for comments, blanks and other text."""
    key, separator, _value = body.partition("=")
    if not separator:
        return None
    key = key.strip()
    return None if not key or key.startswith("#") else key


__all__ = [
    "PyEnvCfg",
]
