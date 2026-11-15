"""
Tests for jsonlines.FollowReader.
"""

import io
import os
from pathlib import Path

import jsonlines
import pytest


class AppendableBytesIO(io.BytesIO):
    """
    In-memory file stand-in that mimics appending to a growing file.

    Unlike a plain ``write()`` (which moves the current position to the
    end), :py:meth:`append` inserts data at the end while leaving the
    read position untouched, just like a separate writer process
    appending to a real file.
    """

    def append(self, data: bytes) -> None:
        position = self.tell()
        self.seek(0, io.SEEK_END)
        self.write(data)
        self.seek(position)


def test_follow_reads_complete_lines() -> None:
    fp = AppendableBytesIO(b'{"a": 1}\n{"b": 2}\n')
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read() == {"a": 1}
        assert reader.read() == {"b": 2}
        with pytest.raises(EOFError):
            reader.read()


def test_follow_buffers_unterminated_trailing_line() -> None:
    fp = AppendableBytesIO(b'{"a": 1}\n{"par')
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read() == {"a": 1}
        # The half line must not be parsed as a permanent bad record.
        with pytest.raises(EOFError):
            reader.read()
        assert reader._pending == b'{"par'
        # Repeated calls keep returning EOF without busy work or errors.
        with pytest.raises(EOFError):
            reader.read()
        assert reader._pending == b'{"par'


def test_follow_completes_buffered_line_on_refresh() -> None:
    fp = AppendableBytesIO(b'{"a": 1}\n{"par')
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read() == {"a": 1}
        with pytest.raises(EOFError):
            reader.read()

        fp.append(b'tial": 2}\n{"b": 3}\n')
        assert reader.read() == {"partial": 2}
        assert reader.read() == {"b": 3}
        with pytest.raises(EOFError):
            reader.read()


def test_follow_growth_without_newline_stays_buffered() -> None:
    fp = AppendableBytesIO(b'{"par')
    with jsonlines.FollowReader(fp) as reader:
        with pytest.raises(EOFError):
            reader.read()
        fp.append(b'tial": 2')
        # Still no newline, so still buffered rather than parsed.
        with pytest.raises(EOFError):
            reader.read()
        assert reader._pending == b'{"partial": 2'
        fp.append(b"}\n")
        assert reader.read() == {"partial": 2}


def test_follow_finish_processes_final_tail_line() -> None:
    fp = AppendableBytesIO(b"1\n2\n3")
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read() == 1
        assert reader.read() == 2
        with pytest.raises(EOFError):
            reader.read()
        # Caller confirms the source ended: final tail line is parsed.
        assert reader.finish() == 3
        # Nothing buffered afterwards.
        with pytest.raises(EOFError):
            reader.finish()


def test_follow_finish_without_buffered_line() -> None:
    fp = AppendableBytesIO(b"1\n2\n")
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read() == 1
        assert reader.read() == 2
        with pytest.raises(EOFError):
            reader.finish()


def test_follow_finish_skip_empty_whitespace_tail() -> None:
    fp = AppendableBytesIO(b"1\n   ")
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read() == 1
        with pytest.raises(EOFError):
            reader.read()
        assert reader._pending == b"   "
        with pytest.raises(EOFError):
            reader.finish(skip_empty=True)
        # The skipped tail line is consumed.
        assert reader._pending is None


def test_follow_finish_invalid_tail_line_reports_lineno() -> None:
    fp = AppendableBytesIO(b"1\n2\nnope")
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read() == 1
        assert reader.read() == 2
        with pytest.raises(EOFError):
            reader.read()
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.finish()
        assert excinfo.value.lineno == 3


def test_follow_continuous_physical_line_numbers() -> None:
    fp = AppendableBytesIO(b"1\n2\n3")
    with jsonlines.FollowReader(fp) as reader:
        reader.read()
        reader.read()
        # The buffered third line already "occupies" physical line 3.
        with pytest.raises(EOFError):
            reader.read()
        assert reader._lineno == 3
        reader.finish()
        assert reader._lineno == 4


def test_follow_each_record_emitted_once() -> None:
    fp = AppendableBytesIO(b"1\n2\n")
    with jsonlines.FollowReader(fp) as reader:
        assert list(reader.iter()) == [1, 2]
        # A second pass finds no new complete records.
        assert list(reader.iter()) == []
        fp.append(b"3\n")
        assert list(reader.iter()) == [3]
        # The partial line is not re-emitted.
        fp.append(b"4")
        assert list(reader.iter()) == []
        fp.append(b"\n")
        assert list(reader.iter()) == [4]


def test_follow_iter_does_not_wait_for_data() -> None:
    fp = AppendableBytesIO(b"1\n2\npar")
    with jsonlines.FollowReader(fp) as reader:
        iterator = reader.iter()
        assert next(iterator) == 1
        assert next(iterator) == 2
        with pytest.raises(StopIteration):
            next(iterator)
        assert reader._pending == b"par"


def test_follow_iter_skip_invalid() -> None:
    fp = AppendableBytesIO(b"1\nbad\n2\n")
    with jsonlines.FollowReader(fp) as reader:
        assert list(reader.iter(skip_invalid=True)) == [1, 2]


def test_follow_skip_empty() -> None:
    fp = AppendableBytesIO(b"1\n\n2\n")
    with jsonlines.FollowReader(fp) as reader:
        assert list(reader.iter(skip_empty=True)) == [1, 2]


def test_follow_typed_reads() -> None:
    fp = AppendableBytesIO(b"12\ntrue\n")
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read(type=int) == 12
        with pytest.raises(jsonlines.InvalidLineError):
            reader.read(type=int)


def test_follow_allow_none() -> None:
    fp = AppendableBytesIO(b"null\n")
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read(allow_none=True) is None


def test_follow_text_stream() -> None:
    fp = io.StringIO('{"a": 1}\n{"par')
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read() == {"a": 1}
        with pytest.raises(EOFError):
            reader.read()
        assert reader._pending == '{"par'


def test_follow_binary_utf8_partial_character() -> None:
    # A multibyte UTF-8 character split across refreshes must not cause
    # a UnicodeDecodeError while buffered.
    fp = AppendableBytesIO(b'{"v": "\xe2\x82')
    with jsonlines.FollowReader(fp) as reader:
        with pytest.raises(EOFError):
            reader.read()
        fp.append(b'\xac"}\n')  # euro sign U+20AC
        assert reader.read() == {"v": "€"}


def test_follow_invalid_complete_line_still_raises() -> None:
    fp = AppendableBytesIO(b"1\n[1, 2\n")
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read() == 1
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
        assert excinfo.value.lineno == 2


def test_follow_closed_reader_raises() -> None:
    reader = jsonlines.FollowReader(AppendableBytesIO(b"1\n"))
    reader.close()
    with pytest.raises(RuntimeError):
        reader.read()
    with pytest.raises(RuntimeError):
        reader.finish()


def test_follow_passed_in_file_is_not_closed() -> None:
    fp = AppendableBytesIO(b"1\n")
    reader = jsonlines.FollowReader(fp)
    reader.close()
    assert not fp.closed


def test_follow_opened_path_is_closed(tmp_path: Path) -> None:
    path = tmp_path / "log.jsonl"
    path.write_bytes(b"1\n")
    reader = jsonlines.FollowReader(path)
    assert reader._should_close_fp
    reader.close()
    assert reader._fp is not None
    with pytest.raises(ValueError):
        reader._fp.read()


def test_follow_custom_loads() -> None:
    fp = AppendableBytesIO(b"whatever\n")
    with jsonlines.FollowReader(fp, loads=lambda line: "decoded") as reader:
        assert reader.read() == "decoded"


def test_follow_repr() -> None:
    fp = AppendableBytesIO(b"1\n")
    reader = jsonlines.FollowReader(fp)
    text = repr(reader)
    assert "FollowReader" in text
    assert "AppendableBytesIO" in text


def test_follow_invalid_type() -> None:
    reader = jsonlines.FollowReader(AppendableBytesIO(b"1\n"))
    with pytest.raises(ValueError):
        # The runtime rejects types that are not in VALID_TYPES even
        # though the static signature accepts any Type[Any].
        reader.read(type=set)


def test_follow_truncation_reported(tmp_path: Path) -> None:
    path = tmp_path / "log.jsonl"
    path.write_bytes(b'{"a": 1}\n{"par')
    with jsonlines.FollowReader(open(path, "rb")) as reader:
        assert reader.read() == {"a": 1}
        with pytest.raises(EOFError):
            reader.read()
        # Truncate the file to fewer bytes than the current read position.
        with open(path, "wb") as writer:
            writer.write(b"x\n")
        with pytest.raises(jsonlines.SourceChangedError) as excinfo:
            reader.read()
        assert excinfo.value.reason == "truncated"
        # The old partial line must be discarded, never spliced.
        assert reader._pending is None


def test_follow_truncation_then_regrowth_reported(tmp_path: Path) -> None:
    # A truncated file must be reported even if new data is appended
    # before the reader gets a chance to refresh.
    path = tmp_path / "log.jsonl"
    path.write_bytes(b'{"a": 1}\n{"par')
    with jsonlines.FollowReader(open(path, "rb")) as reader:
        assert reader.read() == {"a": 1}
        with pytest.raises(EOFError):
            reader.read()
        assert reader._fp is not None
        read_position = reader._fp.tell()
        assert read_position > 2
        # Shrink the file (truncation), then append some data while
        # staying below the reader's current position.
        with open(path, "wb") as writer:
            writer.write(b"x")
        with open(path, "ab") as writer:
            writer.write(b"y")
        assert os.path.getsize(path) < read_position
        with pytest.raises(jsonlines.SourceChangedError) as excinfo:
            reader.read()
        assert excinfo.value.reason == "truncated"
        assert reader._pending is None


def test_follow_replacement_reported(tmp_path: Path) -> None:
    path = tmp_path / "log.jsonl"
    path.write_bytes(b'{"a": 1}\n{"par')
    reader = jsonlines.FollowReader(open(path, "rb"))
    assert reader.read() == {"a": 1}
    with pytest.raises(EOFError):
        reader.read()
    old_identity = reader._file_identity

    # Rotate the file: close and remove the original, then create a
    # replacement with a different inode (as log rotation does).
    assert reader._fp is not None
    reader._fp.close()
    path.unlink()
    path.write_bytes(b'{"b": 2}\n')
    reader._fp = open(path, "rb")
    new_identity = reader._get_file_identity(reader._fp)
    assert new_identity != old_identity
    # The reader still remembers the identity from before the rotation,
    # so its next source check must notice the replacement.
    with pytest.raises(jsonlines.SourceChangedError) as excinfo:
        reader.read()
    assert excinfo.value.reason == "replaced"
    assert reader._pending is None
    reader.close()


def test_follow_open_helper(tmp_path: Path) -> None:
    path = tmp_path / "log.jsonl"
    path.write_bytes(b"1\n2")
    with jsonlines.open(path, follow=True) as reader:
        assert isinstance(reader, jsonlines.FollowReader)
        assert reader.read() == 1
        with pytest.raises(EOFError):
            reader.read()
        assert reader.finish() == 2


def test_follow_open_rejects_write_modes(tmp_path: Path) -> None:
    with pytest.raises(ValueError):
        jsonlines.open(tmp_path / "x.jsonl", mode="w", follow=True)


class CountingBytesIO(AppendableBytesIO):
    def __init__(self, data: bytes) -> None:
        super().__init__(data)
        self.readline_calls = 0

    def readline(self, size: int = -1) -> bytes:  # type: ignore[override]
        self.readline_calls += 1
        return super().readline(size)


def test_follow_no_polling_or_busy_wait() -> None:
    # Each read() performs at most one readline(); when nothing new is
    # available it returns control to the caller immediately.
    fp = CountingBytesIO(b"1\n")
    with jsonlines.FollowReader(fp) as reader:
        assert reader.read() == 1
        calls_after_first = fp.readline_calls
        with pytest.raises(EOFError):
            reader.read()
        # Exactly one additional read attempt, then EOF (no looping).
        assert fp.readline_calls == calls_after_first + 1
        with pytest.raises(EOFError):
            reader.read()
        assert fp.readline_calls == calls_after_first + 2


def test_regular_reader_eof_behaviour_unchanged() -> None:
    # A trailing line without newline is still parsed immediately by the
    # regular Reader, and exhaustion raises EOFError as before.
    with jsonlines.Reader(io.StringIO("1\n2")) as reader:
        assert reader.read() == 1
        assert reader.read() == 2
        with pytest.raises(EOFError):
            reader.read()
