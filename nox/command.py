# Copyright 2016 Alethea Katherine Flowers
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

__lazy_modules__ = {"nox.logger", "pathlib", "shlex", "shutil"}

import os
import shlex
import shutil
import subprocess
import sys
from pathlib import Path
from typing import TYPE_CHECKING, Literal, overload

from nox.logger import logger
from nox.popen import DEFAULT_INTERRUPT_TIMEOUT, DEFAULT_TERMINATE_TIMEOUT, popen

if TYPE_CHECKING:
    from collections.abc import Iterable, Mapping, Sequence
    from typing import IO

__all__ = ["CommandFailed", "ExternalType", "run", "which"]

_PLATFORM = sys.platform


def __dir__() -> list[str]:
    return __all__


ExternalType = Literal["error", True, False]


class CommandFailed(Exception):
    """Raised when an executed command returns a non-success status code."""

    def __init__(
        self, reason: str | None = None, *, return_code: int | None = None
    ) -> None:
        super().__init__(reason)
        self.reason = reason
        self.return_code = return_code


def _which(
    program: str | os.PathLike[str], paths: Sequence[str | os.PathLike[str]] | None
) -> str | None:
    """Finds the full path to an executable, or returns ``None``."""
    if paths is not None:
        full_path = shutil.which(program, path=os.pathsep.join(str(p) for p in paths))
        if full_path:
            return os.fspath(full_path)

    full_path = shutil.which(program)
    if full_path:
        return os.fspath(full_path)

    return None


def which(
    program: str | os.PathLike[str], paths: Sequence[str | os.PathLike[str]] | None
) -> str:
    """Finds the full path to an executable."""
    full_path = _which(program, paths)

    if full_path is not None:
        return full_path

    logger.error(f"Program {program} not found.")
    msg = f"Program {program} not found"
    raise CommandFailed(msg)


def _clean_env(env: Mapping[str, str | None] | None = None) -> dict[str, str] | None:
    if env is None:
        return None

    clean_env = {k: v for k, v in env.items() if v is not None}

    # Ensure systemroot is passed down, otherwise Windows will explode.
    if _PLATFORM.startswith("win"):
        clean_env.setdefault("SYSTEMROOT", os.environ.get("SYSTEMROOT", ""))

    return clean_env


def _shlex_join(args: Sequence[str | os.PathLike[str]]) -> str:
    return " ".join(shlex.quote(os.fspath(arg)) for arg in args)


@overload
def run(
    args: Sequence[str | os.PathLike[str]],
    *,
    env: Mapping[str, str | None] | None = ...,
    silent: Literal[True],
    paths: Sequence[str | os.PathLike[str]] | None = ...,
    fallback: str | os.PathLike[str] | None = ...,
    success_codes: Iterable[int] | None = ...,
    log: bool = ...,
    external: ExternalType = ...,
    stdout: int | IO[str] | None = ...,
    stderr: int | IO[str] | None = ...,
    interrupt_timeout: float | None = ...,
    terminate_timeout: float | None = ...,
) -> str: ...


@overload
def run(
    args: Sequence[str | os.PathLike[str]],
    *,
    env: Mapping[str, str | None] | None = ...,
    silent: Literal[False] = ...,
    paths: Sequence[str | os.PathLike[str]] | None = ...,
    fallback: str | os.PathLike[str] | None = ...,
    success_codes: Iterable[int] | None = ...,
    log: bool = ...,
    external: ExternalType = ...,
    stdout: int | IO[str] | None = ...,
    stderr: int | IO[str] | None = ...,
    interrupt_timeout: float | None = ...,
    terminate_timeout: float | None = ...,
) -> bool: ...


@overload
def run(
    args: Sequence[str | os.PathLike[str]],
    *,
    env: Mapping[str, str | None] | None = ...,
    silent: bool,
    paths: Sequence[str | os.PathLike[str]] | None = ...,
    fallback: str | os.PathLike[str] | None = ...,
    success_codes: Iterable[int] | None = ...,
    log: bool = ...,
    external: ExternalType = ...,
    stdout: int | IO[str] | None = ...,
    stderr: int | IO[str] | None = ...,
    interrupt_timeout: float | None = ...,
    terminate_timeout: float | None = ...,
) -> str | bool: ...


def run(
    args: Sequence[str | os.PathLike[str]],
    *,
    env: Mapping[str, str | None] | None = None,
    silent: bool = False,
    paths: Sequence[str | os.PathLike[str]] | None = None,
    fallback: str | os.PathLike[str] | None = None,
    success_codes: Iterable[int] | None = None,
    log: bool = True,
    external: ExternalType = False,
    stdout: int | IO[str] | None = None,
    stderr: int | IO[str] | None = subprocess.STDOUT,
    interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
    terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
) -> str | bool:
    """Run a command-line program.

    The program is resolved from *paths* (typically the virtualenv's bin
    directory) first. When *fallback* is given, it is only used if the
    preferred program cannot be found, or disappears after resolution but
    before the process starts. The fallback shares the same arguments,
    working directory and environment as the preferred program.
    """

    if success_codes is None:
        success_codes = [0]

    cmd, args = args[0], args[1:]
    full_cmd = f"{cmd} {_shlex_join(args)}"

    cmd_path = _which(os.fspath(cmd), paths)
    str_args = [os.fspath(arg) for arg in args]

    if log:
        logger.info(full_cmd)

    if cmd_path is None and fallback is not None:
        logger.warning(
            f"Program {cmd} not found, using fallback executable {fallback} instead."
        )

    resolved_path = cmd_path if cmd_path is not None else fallback
    if resolved_path is not None:
        abs_cmd_path = Path(os.path.abspath(os.fspath(resolved_path)))
        is_external_tool = paths is not None and not any(
            abs_cmd_path.is_relative_to(os.path.abspath(path)) for path in paths
        )
        if is_external_tool:
            if external == "error":
                logger.error(
                    f"Error: {cmd} is not installed into the virtualenv, it is"
                    f" located at {resolved_path}. Pass external=True into run()"
                    " to explicitly allow this."
                )
                msg = "External program disallowed."
                raise CommandFailed(msg)
            if external is False and log:
                logger.warning(
                    f"Warning: {cmd} is not installed into the virtualenv, it is"
                    f" located at {resolved_path}. This might cause issues! Pass"
                    " external=True into run() to silence this message."
                )

    env = _clean_env(env)

    candidates: list[str] = []
    if cmd_path is not None:
        candidates.append(cmd_path)
    if fallback is not None:
        candidates.append(os.fspath(fallback))

    if not candidates:
        logger.error(f"Program {cmd} not found.")
        msg = f"Program {cmd} not found"
        raise CommandFailed(msg)

    try:
        return_code = 0
        output = ""
        for index, candidate in enumerate(candidates):
            is_fallback = candidate != cmd_path
            try:
                return_code, output = popen(
                    [candidate, *str_args],
                    silent=silent,
                    env=env,
                    stdout=stdout,
                    stderr=stderr,
                    interrupt_timeout=interrupt_timeout,
                    terminate_timeout=terminate_timeout,
                )
            except FileNotFoundError:
                # The executable vanished after resolution but before the
                # process started. Only the explicitly provided fallback may
                # take over, and it only gets a single attempt.
                if index == len(candidates) - 1:
                    if is_fallback:
                        logger.error(
                            f"Fallback executable {fallback} could not be started."
                        )
                        msg = f"Fallback program {fallback} not found"
                    else:
                        logger.error(f"Program {cmd} not found.")
                        msg = f"Program {cmd} not found"
                    raise CommandFailed(msg) from None
                logger.warning(
                    f"Program {cmd} at {cmd_path} disappeared before it could"
                    f" start, using fallback executable {fallback} instead."
                )
                continue

            if is_fallback:
                logger.warning(
                    f"Ran fallback executable {fallback} instead of the preferred"
                    f" {cmd} ({cmd_path})."
                )
            break

        if return_code not in success_codes:
            suffix = ":" if (silent and output) else ""
            logger.error(
                f"Command {full_cmd} failed with exit code {return_code}{suffix}"
            )

            if silent and output:
                logger.error(output)

            msg = f"Returned code {return_code}"
            raise CommandFailed(msg, return_code=return_code)

        if output:
            logger.output(output)

    except KeyboardInterrupt:
        logger.error("Interrupted...")
        raise

    return output if silent else True
