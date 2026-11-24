# Copyright 2017 Alethea Katherine Flowers
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

__lazy_modules__ = {"codecs", "contextlib", "dataclasses", "itertools", "locale", "threading"}

import codecs
import contextlib
import dataclasses
import itertools
import locale
import os
import subprocess
import sys
import threading
from typing import IO, TYPE_CHECKING

if TYPE_CHECKING:
    from collections.abc import Mapping, Sequence

__all__ = [
    "DEFAULT_INTERRUPT_TIMEOUT",
    "DEFAULT_OUTPUT_LIMIT",
    "DEFAULT_TERMINATE_TIMEOUT",
    "PopenResult",
    "decode_output",
    "popen",
    "shutdown_process",
    "shutdown_streaming_process",
]


def __dir__() -> list[str]:
    return __all__


DEFAULT_INTERRUPT_TIMEOUT = 0.3
DEFAULT_TERMINATE_TIMEOUT = 0.2

# Per-stream (stdout and stderr, independently) number of tail bytes retained
# for the JSON report when a command produces a lot of output. The terminal
# still receives everything live; only the recorded copy is bounded.
DEFAULT_OUTPUT_LIMIT = 10_000

_CMD_META = frozenset("&<>^|()")

# Variables cmd.exe resolves dynamically or defines itself, so a reference to
# one expands even when it is absent from the child process environment.
_CMD_AUTO_VARS = frozenset(
    {
        "__APPDIR__",
        "__CD__",
        "CD",
        "CMDCMDLINE",
        "CMDEXTVERSION",
        "COMSPEC",
        "DATE",
        "ERRORLEVEL",
        "HIGHESTNUMANODENUMBER",
        "PATHEXT",
        "PROMPT",
        "RANDOM",
        "TIME",
    }
)


def _expandable_reference(arg: str, env: Mapping[str, str]) -> str | None:
    """Find a ``%VAR%`` reference that cmd.exe would expand, if there is one.

    cmd.exe scans the text between each pair of ``%`` characters; when it
    names a defined (or dynamic) variable - optionally followed by ``:``
    modifiers, the value is substituted, even inside double quotes. An
    undefined name is left alone and the scan resumes at the trailing ``%``.
    """
    defined = {name.upper() for name in env}
    percents = [index for index, char in enumerate(arg) if char == "%"]
    for start, end in itertools.pairwise(percents):
        name = arg[start + 1 : end].partition(":")[0].upper()
        if name and (
            name in defined
            or name in _CMD_AUTO_VARS
            # hidden variables such as the per-drive working directory "=C:"
            or name.startswith("=")
        ):
            return arg[start : end + 1]
    return None


def _windows_batch_command(
    args: Sequence[str], env: Mapping[str, str] | None = None
) -> str:
    """Build a command line that protects the cmd.exe metacharacters ``&<>^|()``.

    Arguments cmd.exe would corrupt anyway are rejected: ``%VAR%`` references
    are expanded even inside double quotes, and quote characters cannot be
    escaped. cmd.exe doesn't have backslash escapes, it only counts quotes.
    """
    if env is None:
        env = os.environ

    quoted_args = []
    for arg in args:
        reference = _expandable_reference(arg, env)
        if reference is not None:
            msg = (
                "Cannot escape argument for batch script, cmd.exe would"
                f" expand {reference}: {arg}"
            )
            raise ValueError(msg)
        if _CMD_META.isdisjoint(arg):
            quoted_args.append(subprocess.list2cmdline([arg]))
        elif len(arg) >= 2 and arg[0] == arg[-1] == '"' and '"' not in arg[1:-1]:
            # A pre-quoted argument such as '"urllib3<1.25"' (the historical
            # workaround): its outer quotes already protect the
            # metacharacters, so pass it through unchanged.
            quoted_args.append(arg)
        elif '"' in arg:
            # list2cmdline escapes embedded quotes with backslashes, but
            # cmd.exe doesn't have backslash escapes.
            msg = f"Cannot escape argument for batch script: {arg}"
            raise ValueError(msg)
        else:
            # Force list2cmdline to quote the argument, then remove the
            # temporary leading space without changing the argument itself:
            # ' requests<99' -> '" requests<99"' -> '"requests<99"'
            quoted = subprocess.list2cmdline([f" {arg}"])
            quoted_args.append(f"{quoted[0]}{quoted[2:]}")
    return " ".join(quoted_args)


def shutdown_process(
    proc: subprocess.Popen[bytes],
    interrupt_timeout: float | None,
    terminate_timeout: float | None,
) -> tuple[bytes, bytes]:
    """Gracefully shutdown a child process."""
    with contextlib.suppress(subprocess.TimeoutExpired):
        return proc.communicate(timeout=interrupt_timeout)

    proc.terminate()

    with contextlib.suppress(subprocess.TimeoutExpired):
        return proc.communicate(timeout=terminate_timeout)

    proc.kill()

    return proc.communicate()


def shutdown_streaming_process(
    proc: subprocess.Popen[bytes],
    interrupt_timeout: float | None,
    terminate_timeout: float | None,
) -> None:
    """Stop a child whose pipes are drained by Nox's own reader threads.

    Unlike :func:`shutdown_process` this never calls ``communicate()`` (the
    pipes already have readers), it only escalates wait/terminate/kill; the
    readers see EOF and finish on their own.
    """
    with contextlib.suppress(subprocess.TimeoutExpired):
        proc.wait(timeout=interrupt_timeout)
        return

    proc.terminate()

    with contextlib.suppress(subprocess.TimeoutExpired):
        proc.wait(timeout=terminate_timeout)
        return

    proc.kill()
    proc.wait()


def decode_output(output: bytes) -> str:
    """Try to decode the given bytes with encodings from the system.

    :param output: output to decode
    :raises UnicodeDecodeError: if all encodings fail
    :return: decoded string
    """
    try:
        return output.decode("utf-8")
    except UnicodeDecodeError:
        second_encoding = locale.getpreferredencoding()
        if second_encoding.casefold() in {"utf8", "utf-8"}:
            raise

        return output.decode(second_encoding)


@dataclasses.dataclass(frozen=True)
class PopenResult:
    """Everything :func:`popen` learned while running a command.

    ``output`` keeps the historical contract: merged stdout/stderr text for a
    ``silent`` command (used as :meth:`session.run`'s return value), an empty
    string otherwise.

    The ``stdout_tail``/``stderr_tail`` pair is the bounded end of each stream
    as raw bytes, so a multibyte character split across two reads is never
    truncated mid-character: the retained bytes are decoded as one piece.
    ``truncated`` reports, per stream, how many bytes were discarded and how
    many are retained, so a report can state exactly how much was cut.
    """

    return_code: int
    output: str
    stdout_tail: bytes = b""
    stderr_tail: bytes = b""
    truncated: dict[str, int] = dataclasses.field(default_factory=dict)


class _BoundedTail:
    """Keep only the last ``limit`` bytes while remembering the full length."""

    def __init__(self, limit: int) -> None:
        self.limit = limit
        self._buf = bytearray()
        self.total = 0

    def append(self, chunk: bytes) -> None:
        self.total += len(chunk)
        if self.limit <= 0:
            return
        self._buf.extend(chunk)
        overflow = len(self._buf) - self.limit
        if overflow > 0:
            del self._buf[:overflow]

    @property
    def bytes(self) -> bytes:
        return bytes(self._buf)

    def truncated_info(self) -> dict[str, int]:
        dropped = self.total - len(self._buf)
        return {"dropped": dropped, "retained": len(self._buf)}


def _stream_reader(
    pipe: IO[bytes],
    *,
    tail: _BoundedTail,
    live_stream: IO[str] | None,
    full_output: bytearray | None,
    output_lock: threading.Lock,
) -> None:
    """Drain one child pipe until EOF.

    Every chunk is appended to the bounded tail and, when the caller wants it,
    to the merged full-output buffer. When a live terminal stream is given, the
    chunk is also decoded incrementally and forwarded immediately, so output
    appears while the command runs and a multibyte character split across reads
    is still emitted whole (the decoder holds the partial sequence until the
    rest of it arrives).
    """
    decoder = codecs.getincrementaldecoder("utf-8")(errors="replace")
    while True:
        chunk = pipe.read(1024)
        if not chunk:
            break
        tail.append(chunk)
        if full_output is not None:
            with output_lock:
                full_output.extend(chunk)
        if live_stream is not None:
            _write_live(live_stream, decoder.decode(chunk))
    if live_stream is not None:
        final = decoder.decode(b"", final=True)
        if final:
            _write_live(live_stream, final)


def _write_live(stream: IO[str], text: str) -> None:
    """Forward live output, escaping anything the console encoding cannot hold."""
    if not text:
        return
    try:
        stream.write(text)
    except UnicodeEncodeError:
        encoding = getattr(stream, "encoding", None) or "utf-8"
        stream.write(text.encode(encoding, "backslashreplace").decode(encoding))
    stream.flush()


def _stream_popen(
    proc: subprocess.Popen[bytes],
    *,
    silent: bool,
    output_limit: int,
    interrupt_timeout: float | None,
    terminate_timeout: float | None,
) -> PopenResult:
    """Read both pipes concurrently with bounded per-stream tails."""
    assert proc.stdout is not None
    assert proc.stderr is not None

    # A silent command's full merged output is its documented return value, so
    # it must be accumulated completely. A non-silent command goes to the
    # terminal live and only the bounded tails are kept for the report.
    full_output = bytearray() if silent else None
    output_lock = threading.Lock()
    stdout_tail = _BoundedTail(output_limit)
    stderr_tail = _BoundedTail(output_limit)

    live_out = None if silent else sys.stdout
    live_err = None if silent else sys.stderr
    threads = [
        threading.Thread(
            target=_stream_reader,
            name="nox-stdout-reader",
            args=(proc.stdout,),
            kwargs={
                "tail": stdout_tail,
                "live_stream": live_out,
                "full_output": full_output,
                "output_lock": output_lock,
            },
            daemon=True,
        ),
        threading.Thread(
            target=_stream_reader,
            name="nox-stderr-reader",
            args=(proc.stderr,),
            kwargs={
                "tail": stderr_tail,
                "live_stream": live_err,
                "full_output": full_output,
                "output_lock": output_lock,
            },
            daemon=True,
        ),
    ]
    for thread in threads:
        thread.start()

    try:
        proc.wait()
    except KeyboardInterrupt:
        shutdown_streaming_process(proc, interrupt_timeout, terminate_timeout)
        if proc.returncode != 0:
            raise
    finally:
        for thread in threads:
            thread.join()
        # Closing the Popen also closes both pipes, avoiding ResourceWarnings
        # about inherited file descriptors.
        proc.wait()
        proc.stdout.close()
        proc.stderr.close()

    sys.stdout.flush()
    sys.stderr.flush()

    output = decode_output(bytes(full_output)) if full_output else ""
    return PopenResult(
        return_code=proc.returncode,
        output=output,
        stdout_tail=stdout_tail.bytes,
        stderr_tail=stderr_tail.bytes,
        truncated={
            "stdout": stdout_tail.truncated_info(),
            "stderr": stderr_tail.truncated_info(),
        },
    )


def popen(
    args: Sequence[str],
    *,
    env: Mapping[str, str] | None = None,
    silent: bool = False,
    stdout: int | IO[str] | None = None,
    stderr: int | IO[str] | None = subprocess.STDOUT,
    interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
    terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
    output_limit: int = DEFAULT_OUTPUT_LIMIT,
) -> PopenResult:
    if silent and stdout is not None:
        msg = (
            "Can not specify silent and stdout; passing a custom stdout always silences"
            " the commands output in Nox's log."
        )
        raise ValueError(msg)

    popen_args: Sequence[str] | str = args
    if sys.platform.startswith("win") and args[0].casefold().endswith((".bat", ".cmd")):
        popen_args = _windows_batch_command(args, env)

    if silent or (stdout is None and stderr in (None, subprocess.STDOUT)):
        # Nox owns the pipes: drain them itself so output streams live and the
        # end of each stream is retained within the bounded report limit.
        proc = subprocess.Popen(
            popen_args,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        return _stream_popen(
            proc,
            silent=silent,
            output_limit=output_limit,
            interrupt_timeout=interrupt_timeout,
            terminate_timeout=terminate_timeout,
        )

    # Custom stdout/stderr destinations (files or descriptors) are handed to
    # the child directly; there is nothing for Nox to retain or stream.
    with subprocess.Popen(popen_args, env=env, stdout=stdout, stderr=stderr) as proc:
        try:
            out, _err = proc.communicate()
            sys.stdout.flush()

        except KeyboardInterrupt:
            out, _err = shutdown_process(proc, interrupt_timeout, terminate_timeout)
            if proc.returncode != 0:
                raise

        return_code = proc.wait()

    return PopenResult(
        return_code=return_code,
        output=decode_output(out) if out else "",
    )
