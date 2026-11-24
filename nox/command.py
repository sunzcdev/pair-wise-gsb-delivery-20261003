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

__lazy_modules__ = {"contextvars", "dataclasses", "locale", "nox.logger", "pathlib", "shlex", "shutil"}

import contextvars
import dataclasses
import locale
import os
import shlex
import shutil
import subprocess
import sys
from pathlib import Path
from typing import TYPE_CHECKING, Literal, overload

from nox.logger import logger
from nox.popen import (
    DEFAULT_INTERRUPT_TIMEOUT,
    DEFAULT_OUTPUT_LIMIT,
    DEFAULT_TERMINATE_TIMEOUT,
    PopenResult,
    popen,
)

if TYPE_CHECKING:
    from collections.abc import Iterable, Mapping, Sequence
    from typing import IO

__all__ = ["CommandFailed", "CommandResult", "ExternalType", "command_records", "run", "which"]

_PLATFORM = sys.platform


def __dir__() -> list[str]:
    return __all__


ExternalType = Literal["error", True, False]


@dataclasses.dataclass(frozen=True)
class CommandResult:
    """What happened when one command line was run.

    The two tails are the bounded end of each stream, decoded as one piece so
    a multibyte character split across writes survives intact. ``truncated``
    states, per stream, how many bytes were discarded and retained; ``limit``
    is the configured per-stream cap. The real process exit code is always
    recorded, even on failure.
    """

    command: str
    return_code: int
    stdout_tail: str
    stderr_tail: str
    truncated: dict[str, dict[str, int]]
    limit: int


# Set by a SessionRunner for the duration of a session; command.run appends a
# CommandResult per invocation so the JSON report can describe each command
# without session.run having to grow a new public parameter. A context variable
# keeps parallel sessions and test runs isolated automatically.
command_records: contextvars.ContextVar[list[CommandResult] | None] = contextvars.ContextVar(
    "nox_command_records", default=None
)


class CommandFailed(Exception):
    """Raised when an executed command returns a non-success status code."""

    def __init__(
        self,
        reason: str | None = None,
        *,
        return_code: int | None = None,
        result: CommandResult | None = None,
    ) -> None:
        super().__init__(reason)
        self.reason = reason
        self.return_code = return_code
        self.result = result


def which(
    program: str | os.PathLike[str], paths: Sequence[str | os.PathLike[str]] | None
) -> str:
    """Finds the full path to an executable."""
    if paths is not None:
        full_path = shutil.which(program, path=os.pathsep.join(str(p) for p in paths))
        if full_path:
            return os.fspath(full_path)

    full_path = shutil.which(program)
    if full_path:
        return os.fspath(full_path)

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
    success_codes: Iterable[int] | None = ...,
    log: bool = ...,
    external: ExternalType = ...,
    stdout: int | IO[str] | None = ...,
    stderr: int | IO[str] | None = ...,
    interrupt_timeout: float | None = ...,
    terminate_timeout: float | None = ...,
    output_limit: int = ...,
) -> str: ...


@overload
def run(
    args: Sequence[str | os.PathLike[str]],
    *,
    env: Mapping[str, str | None] | None = ...,
    silent: Literal[False] = ...,
    paths: Sequence[str | os.PathLike[str]] | None = ...,
    success_codes: Iterable[int] | None = ...,
    log: bool = ...,
    external: ExternalType = ...,
    stdout: int | IO[str] | None = ...,
    stderr: int | IO[str] | None = ...,
    interrupt_timeout: float | None = ...,
    terminate_timeout: float | None = ...,
    output_limit: int = ...,
) -> bool: ...


@overload
def run(
    args: Sequence[str | os.PathLike[str]],
    *,
    env: Mapping[str, str | None] | None = ...,
    silent: bool,
    paths: Sequence[str | os.PathLike[str]] | None = ...,
    success_codes: Iterable[int] | None = ...,
    log: bool = ...,
    external: ExternalType = ...,
    stdout: int | IO[str] | None = ...,
    stderr: int | IO[str] | None = ...,
    interrupt_timeout: float | None = ...,
    terminate_timeout: float | None = ...,
    output_limit: int = ...,
) -> str | bool: ...


def run(
    args: Sequence[str | os.PathLike[str]],
    *,
    env: Mapping[str, str | None] | None = None,
    silent: bool = False,
    paths: Sequence[str | os.PathLike[str]] | None = None,
    success_codes: Iterable[int] | None = None,
    log: bool = True,
    external: ExternalType = False,
    stdout: int | IO[str] | None = None,
    stderr: int | IO[str] | None = subprocess.STDOUT,
    interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
    terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
    output_limit: int = DEFAULT_OUTPUT_LIMIT,
) -> str | bool:
    """Run a command-line program.

    ``output_limit`` bounds, per stream, how many tail bytes are recorded for
    the JSON report. The terminal always receives the complete output live;
    the cap only affects what Nox retains.
    """
    if output_limit < 0:
        msg = "output_limit must be a non-negative integer"
        raise ValueError(msg)

    if success_codes is None:
        success_codes = [0]

    cmd, args = args[0], args[1:]
    full_cmd = f"{cmd} {_shlex_join(args)}"

    cmd_path = which(os.fspath(cmd), paths)
    str_args = [os.fspath(arg) for arg in args]

    if log:
        logger.info(full_cmd)

    abs_cmd_path = Path(os.path.abspath(cmd_path))
    is_external_tool = paths is not None and not any(
        abs_cmd_path.is_relative_to(os.path.abspath(path)) for path in paths
    )
    if is_external_tool:
        if external == "error":
            logger.error(
                f"Error: {cmd} is not installed into the virtualenv, it is located"
                f" at {cmd_path}. Pass external=True into run() to explicitly allow"
                " this."
            )
            msg = "External program disallowed."
            raise CommandFailed(msg)
        if external is False and log:
            logger.warning(
                f"Warning: {cmd} is not installed into the virtualenv, it is"
                f" located at {cmd_path}. This might cause issues! Pass"
                " external=True into run() to silence this message."
            )

    env = _clean_env(env)

    try:
        popen_result: PopenResult = popen(
            [cmd_path, *str_args],
            silent=silent,
            env=env,
            stdout=stdout,
            stderr=stderr,
            interrupt_timeout=interrupt_timeout,
            terminate_timeout=terminate_timeout,
            output_limit=output_limit,
        )
        return_code = popen_result.return_code
        output = popen_result.output

        command_result = CommandResult(
            command=full_cmd,
            return_code=return_code,
            stdout_tail=_decode_tail(popen_result.stdout_tail),
            stderr_tail=_decode_tail(popen_result.stderr_tail),
            truncated=popen_result.truncated,
            limit=output_limit,
        )
        records = command_records.get()
        if records is not None:
            records.append(command_result)

        if return_code not in success_codes:
            suffix = ":" if (silent and output) else ""
            logger.error(
                f"Command {full_cmd} failed with exit code {return_code}{suffix}"
            )

            if silent and output:
                logger.error(output)

            msg = f"Returned code {return_code}"
            raise CommandFailed(msg, return_code=return_code, result=command_result)

        if output:
            logger.output(output)

    except KeyboardInterrupt:
        logger.error("Interrupted...")
        raise

    return output if silent else True


def _decode_tail(tail: bytes) -> str:
    """Decode a retained stream tail, tolerating a legacy non-UTF-8 encoding."""
    if not tail:
        return ""
    try:
        return tail.decode("utf-8")
    except UnicodeDecodeError:
        second_encoding = locale.getpreferredencoding()
        if second_encoding.casefold() in {"utf8", "utf-8"}:
            # Never raise while building a report: keep the undecodable bytes
            # visible instead of losing the failure context.
            return tail.decode("utf-8", errors="replace")
        return tail.decode(second_encoding, errors="replace")
