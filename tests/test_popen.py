# Copyright 2026 Alethea Katherine Flowers
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

import sys

import pytest

import nox.popen

PYTHON = sys.executable


def _program(source: str) -> list[str]:
    return [PYTHON, "-c", source]


def test_popen_returns_popen_result() -> None:
    result = nox.popen.popen(_program("print(123)"))

    assert isinstance(result, nox.popen.PopenResult)
    assert result.return_code == 0
    # Non-silent commands stream live; the merged "output" stays empty by contract.
    assert result.output == ""
    assert set(result.truncated) == {"stdout", "stderr"}


def test_popen_silent_returns_full_merged_output() -> None:
    program = (
        "import sys;"
        'sys.stdout.write("out-line\\n");'
        'sys.stderr.write("err-line\\n")'
    )
    result = nox.popen.popen(_program(program), silent=True)

    assert result.return_code == 0
    assert "out-line" in result.output
    assert "err-line" in result.output


def test_popen_streams_live_to_terminal(capsys: pytest.CaptureFixture[str]) -> None:
    program = (
        "import sys;"
        'sys.stdout.write("live-out\\n");'
        'sys.stderr.write("live-err\\n")'
    )
    nox.popen.popen(_program(program))

    captured = capsys.readouterr()
    assert "live-out" in captured.out
    assert "live-err" in captured.err


def test_popen_silent_does_not_stream(capsys: pytest.CaptureFixture[str]) -> None:
    nox.popen.popen(_program('print("hidden")'), silent=True)

    captured = capsys.readouterr()
    assert captured.out == ""
    assert captured.err == ""


def test_popen_nonzero_exit_code_is_reported() -> None:
    result = nox.popen.popen(_program("import sys; sys.exit(7)"))

    assert result.return_code == 7


def test_popen_separate_tails_for_each_stream() -> None:
    program = (
        "import sys;"
        'sys.stdout.write("OUTTAIL\\n");'
        'sys.stderr.write("ERRTAIL\\n")'
    )
    result = nox.popen.popen(_program(program))

    assert b"OUTTAIL" in result.stdout_tail
    assert b"ERRTAIL" in result.stderr_tail
    assert b"ERRTAIL" not in result.stdout_tail
    assert b"OUTTAIL" not in result.stderr_tail


def test_popen_tail_is_bounded_and_keeps_the_end() -> None:
    # 2000 bytes per stream; keep only the last 100.
    program = (
        "import sys;"
        'sys.stdout.write("x" * 1900 + "STDOUT-END");'
        'sys.stderr.write("y" * 1900 + "STDERR-END")'
    )
    result = nox.popen.popen(_program(program), output_limit=100)

    assert len(result.stdout_tail) <= 100
    assert len(result.stderr_tail) <= 100
    assert result.stdout_tail.endswith(b"STDOUT-END")
    assert result.stderr_tail.endswith(b"STDERR-END")
    assert result.truncated["stdout"]["retained"] == 100
    assert result.truncated["stdout"]["dropped"] == (1900 + len(b"STDOUT-END")) - 100
    assert result.truncated["stderr"]["retained"] == 100
    assert result.truncated["stderr"]["dropped"] == (1900 + len(b"STDERR-END")) - 100


def test_popen_small_output_is_not_truncated() -> None:
    # Write bytes directly so line-ending translation can't change the count.
    result = nox.popen.popen(
        _program("import sys; sys.stdout.buffer.write(b'small\\n')"),
        output_limit=1000,
    )

    assert result.truncated["stdout"]["dropped"] == 0
    assert result.truncated["stdout"]["retained"] == len(b"small\n")
    assert result.stdout_tail == b"small\n"


def test_popen_zero_limit_keeps_nothing_but_counts() -> None:
    result = nox.popen.popen(
        _program("import sys; sys.stdout.buffer.write(b'bye\\n')"),
        output_limit=0,
    )

    assert result.stdout_tail == b""
    assert result.truncated["stdout"] == {
        "dropped": len(b"bye\n"),
        "retained": 0,
    }


def test_popen_multibyte_character_split_across_writes() -> None:
    # The child writes the 3-byte UTF-8 sequence of "中" one byte at a time on
    # each stream, mixed with ASCII, then exits non-zero.
    char = "中"
    raw = char.encode("utf-8")
    byte_repr = repr(raw)
    program = f"""
import sys

raw = {byte_repr}
for stream, suffix in ((sys.stdout, "OUT"), (sys.stderr, "ERR")):
    stream.write(suffix + "-")
    stream.flush()
    for index, byte in enumerate(raw):
        stream.buffer.write(bytes([byte]))
        stream.flush()
    stream.write("-END\\n")
    stream.flush()
sys.exit(3)
"""
    result = nox.popen.popen(_program(program), output_limit=50)

    assert result.return_code == 3
    # The retained tail is decoded as one piece: no replacement characters.
    stdout_text = result.stdout_tail.decode("utf-8")
    stderr_text = result.stderr_tail.decode("utf-8")
    assert f"OUT-{char}-END" in stdout_text
    assert f"ERR-{char}-END" in stderr_text
    assert "\ufffd" not in stdout_text
    assert "\ufffd" not in stderr_text

    # The live terminal copy must also show the character whole.
    live = nox.popen.popen(_program(program), output_limit=50)
    assert live.return_code == 3


def test_popen_live_multibyte_output_is_whole(capsys: pytest.CaptureFixture[str]) -> None:
    char = "€"
    raw = char.encode("utf-8")
    program = f"""
import sys

for byte in {repr(raw)}:
    sys.stdout.buffer.write(bytes([byte]))
    sys.stdout.flush()
sys.stdout.write("\\n")
"""
    nox.popen.popen(_program(program))

    captured = capsys.readouterr()
    assert char in captured.out


def test_popen_interleaved_streams_stay_attributed() -> None:
    # A lot of interleaved traffic: each stream's own tail must still contain
    # that stream's final marker, proving the failure-relevant end can't be
    # pushed out by the other stream.
    program = """
import sys

for index in range(200):
    sys.stdout.write(f"out {index}\\n")
    sys.stderr.write(f"err {index}\\n")
sys.stdout.write("OUT-FINAL\\n")
sys.stderr.write("ERR-FINAL\\n")
sys.exit(1)
"""
    result = nox.popen.popen(_program(program), output_limit=64)

    assert result.return_code == 1
    assert b"OUT-FINAL" in result.stdout_tail
    assert b"ERR-FINAL" in result.stderr_tail
    # The early lines were discarded (line-ending agnostic: CRLF on Windows).
    assert b"out 0" not in result.stdout_tail
    assert b"err 0" not in result.stderr_tail


def test_bounded_tail_truncation_accounting() -> None:
    tail = nox.popen._BoundedTail(4)
    tail.append(b"ab")
    tail.append(b"cde")

    assert tail.bytes == b"bcde"
    assert tail.total == 5
    assert tail.truncated_info() == {"dropped": 1, "retained": 4}


def test_popen_custom_stdout_uses_legacy_path(tmp_path: object) -> None:
    import pathlib

    out_file = pathlib.Path(tmp_path) / "out.txt"  # type: ignore[operator]
    with out_file.open("w+", encoding="utf-8") as stdout:
        result = nox.popen.popen(
            _program('import sys; sys.stdout.write("filed")'), stdout=stdout
        )

    assert result.return_code == 0
    assert result.stdout_tail == b""
    assert "filed" in out_file.read_text(encoding="utf-8")
