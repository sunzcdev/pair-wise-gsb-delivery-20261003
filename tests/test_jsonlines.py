"""
Tests for the jsonlines library.
"""

import codecs
import collections
import io
import json
import tempfile
from typing import Any

import jsonlines
import pytest


SAMPLE_BYTES = b'{"a": 1}\n{"b": 2}\n'
SAMPLE_TEXT = SAMPLE_BYTES.decode("utf-8")


def is_json_decode_error(exc: object) -> bool:
    if type(exc).__module__ == "ujson":
        # The ujson package has its own ujson.JSONDecodeError; because of the
        # line above this function also works if it's not installed.
        import ujson

        return isinstance(exc, ujson.JSONDecodeError)
    else:
        # Otherwise, this should be a stdlib json.JSONDecodeError, which also
        # works for orjson since orjson.JSONDecodeError inherits from it.
        return isinstance(exc, json.JSONDecodeError)


class PartialWriteTextStream(io.StringIO):
    """
    Text stream that accepts only a limited number of chars per write() call.

    Mimics a custom collection output stream that performs short writes.
    """

    def __init__(self, limit: int) -> None:
        super().__init__()
        self.limit = limit
        self.flushes = 0

    def write(self, data: Any) -> int:
        chunk = data[: self.limit]
        super().write(chunk)
        return len(chunk)

    def flush(self) -> None:
        self.flushes += 1


class PartialWriteBinaryStream(io.BytesIO):
    """
    Binary version of :py:class:`PartialWriteTextStream`.
    """

    def __init__(self, limit: int) -> None:
        super().__init__()
        self.limit = limit
        self.flushes = 0

    def write(self, data: Any) -> int:
        chunk = bytes(data)[: self.limit]
        super().write(chunk)
        return len(chunk)

    def flush(self) -> None:
        self.flushes += 1


class BrokenWriteStream(io.BytesIO):
    """
    Binary stream whose write() returns an invalid result from the start.
    """

    def __init__(self, result: object) -> None:
        super().__init__()
        self.result = result

    def write(self, data: Any) -> Any:
        super().write(bytes(data))
        return self.result

    def flush(self) -> None:
        raise AssertionError("flush() must not be called")


class DyingWriteStream(io.BytesIO):
    """
    Binary stream that accepts a few bytes and then reports no progress.
    """

    def __init__(self, accept: int) -> None:
        super().__init__()
        self.accept = accept
        self.flushes = 0

    def write(self, data: Any) -> int:
        if len(self.getvalue()) >= self.accept:
            return 0
        chunk = bytes(data)[: self.accept - len(self.getvalue())]
        super().write(chunk)
        return len(chunk)

    def flush(self) -> None:
        self.flushes += 1


class SeekSpyStream(io.BytesIO):
    """
    Seekable stream that records (and forbids) any attempt to roll back.
    """

    def __init__(self, accept: int) -> None:
        super().__init__()
        self.accept = accept

    def write(self, data: Any) -> int:
        if len(self.getvalue()) >= self.accept:
            return 0
        chunk = bytes(data)[: self.accept - len(self.getvalue())]
        super().write(chunk)
        return len(chunk)

    def seek(self, *args: Any, **kwargs: Any) -> int:
        raise AssertionError("partial writes must not be rolled back via seek()")

    def truncate(self, *args: Any, **kwargs: Any) -> int:
        raise AssertionError("partial writes must not be rolled back via truncate()")


def test_reader() -> None:
    fp = io.BytesIO(SAMPLE_BYTES)
    with jsonlines.Reader(fp) as reader:
        it = iter(reader)
        assert next(it) == {"a": 1}
        assert next(it) == {"b": 2}
        with pytest.raises(StopIteration):
            next(it)
        with pytest.raises(EOFError):
            reader.read()


def test_reading_from_iterable() -> None:
    with jsonlines.Reader(["1", b"{}"]) as reader:
        assert list(reader) == [1, {}]
    assert "wrapping <list at " in repr(reader)


def test_reader_rfc7464_text_sequences() -> None:
    fp = io.BytesIO(b'\x1e"a"\x0a\x1e"b"\x0a')
    with jsonlines.Reader(fp) as reader:
        assert list(reader) == ["a", "b"]


def test_reader_utf8_bom_bytes() -> None:
    """
    UTF-8 BOM is ignored, even if it occurs in the middle of a stream.
    """
    chunks = [
        codecs.BOM_UTF8,
        b"1\n",
        codecs.BOM_UTF8,
        b"2\n",
    ]
    fp = io.BytesIO(b"".join(chunks))
    with jsonlines.Reader(fp) as reader:
        assert list(reader) == [1, 2]


def test_reader_utf8_bom_text() -> None:
    """
    Text version of ``test_reader_utf8_bom_bytes()``.
    """
    chunks = [
        "1\n",
        codecs.BOM_UTF8.decode(),
        "2\n",
    ]
    fp = io.StringIO("".join(chunks))
    with jsonlines.Reader(fp) as reader:
        assert list(reader) == [1, 2]


def test_reader_utf8_bom_bom_bom() -> None:
    """
    Too many UTF-8 BOM BOM BOM chars cause BOOM 💥 BOOM.
    """
    reader = jsonlines.Reader([codecs.BOM_UTF8.decode() * 3 + "1\n"])
    with pytest.raises(jsonlines.InvalidLineError) as excinfo:
        reader.read()

    exc = excinfo.value
    assert "invalid json" in str(exc)
    assert is_json_decode_error(exc.__cause__)


def test_writer_text() -> None:
    fp = io.StringIO()
    with jsonlines.Writer(fp) as writer:
        writer.write({"a": 1})
        writer.write({"b": 2})
    assert fp.getvalue() == SAMPLE_TEXT


def test_writer_binary() -> None:
    fp = io.BytesIO()
    with jsonlines.Writer(fp) as writer:
        writer.write_all(
            [
                {"a": 1},
                {"b": 2},
            ]
        )
    assert fp.getvalue() == SAMPLE_BYTES


def test_closing() -> None:
    reader = jsonlines.Reader([])
    reader.close()
    with pytest.raises(RuntimeError):
        reader.read()
    writer = jsonlines.Writer(io.BytesIO())
    writer.close()
    writer.close()  # no-op
    with pytest.raises(RuntimeError):
        writer.write(123)


def test_invalid_lines() -> None:
    data = "[1, 2"
    with jsonlines.Reader(io.StringIO(data)) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
        exc = excinfo.value
        assert "invalid json" in str(exc)
        assert exc.line == data
        assert is_json_decode_error(exc.__cause__)


def test_skip_invalid() -> None:
    fp = io.StringIO("12\ninvalid\n34")
    reader = jsonlines.Reader(fp)
    it = reader.iter(skip_invalid=True)
    assert next(it) == 12
    assert next(it) == 34


def test_empty_strings_in_iterable() -> None:
    input = ["123", "", "456"]
    it = iter(jsonlines.Reader(input))
    assert next(it) == 123
    with pytest.raises(jsonlines.InvalidLineError):
        next(it)
    with pytest.raises(StopIteration):
        next(it)
    it = jsonlines.Reader(input).iter(skip_empty=True)
    assert list(it) == [123, 456]


def test_invalid_utf8() -> None:
    with jsonlines.Reader([b"\xff\xff"]) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
        assert "line is not valid utf-8" in str(excinfo.value)


def test_empty_lines() -> None:
    data_with_empty_line = b"1\n\n2\n"
    with jsonlines.Reader(io.BytesIO(data_with_empty_line)) as reader:
        assert reader.read()
        with pytest.raises(jsonlines.InvalidLineError):
            reader.read()
        assert reader.read() == 2
        with pytest.raises(EOFError):
            reader.read()
    with jsonlines.Reader(io.BytesIO(data_with_empty_line)) as reader:
        assert list(reader.iter(skip_empty=True)) == [1, 2]


def test_typed_reads() -> None:
    with jsonlines.Reader(io.StringIO('12\ntrue\n"foo"\n')) as reader:
        assert reader.read(type=int) == 12

        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read(type=int)
        exc = excinfo.value
        assert "does not match requested type" in str(exc)
        assert exc.line == "true"

        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read(type=float)
        exc = excinfo.value
        assert "does not match requested type" in str(exc)
        assert exc.line == '"foo"'


def test_typed_read_invalid_type() -> None:
    reader = jsonlines.Reader([])
    with pytest.raises(ValueError) as excinfo:
        reader.read(type="nope")  # type: ignore[call-overload]
    exc = excinfo.value
    assert str(exc) == "invalid type specified"


def test_typed_iteration() -> None:
    fp = io.StringIO("1\n2\n")
    with jsonlines.Reader(fp) as reader:
        actual = list(reader.iter(type=int))
        assert actual == [1, 2]

    fp = io.StringIO("1\n2\n")
    with jsonlines.Reader(fp) as reader:
        it = reader.iter(type=str)
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            next(it)
        exc = excinfo.value
        assert "does not match requested type" in str(exc)


def test_writer_flags() -> None:
    fp = io.BytesIO()
    with jsonlines.Writer(fp, compact=True, sort_keys=True) as writer:
        writer.write(
            collections.OrderedDict(
                [
                    ("b", 2),
                    ("a", 1),
                ]
            )
        )
    assert fp.getvalue() == b'{"a":1,"b":2}\n'


def test_custom_dumps() -> None:
    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, dumps=lambda obj: "oh hai")
    with writer:
        nbytes = writer.write({})
        assert nbytes == len(b"oh hai\n")

    assert fp.getvalue() == b"oh hai\n"


def test_custom_dumps_bytes() -> None:
    """
    A custom dump function that returns bytes (e.g. ‘orjson’) should work.
    """

    fp = io.BytesIO()
    writer = jsonlines.Writer(fp, dumps=lambda obj: b"some bytes")
    with writer:
        writer.write(123)

    assert fp.getvalue() == b"some bytes\n"


def test_custom_loads() -> None:
    fp = io.BytesIO(b"{}\n")
    with jsonlines.Reader(fp, loads=lambda s: "uh what") as reader:
        assert reader.read() == "uh what"


def test_open_reading() -> None:
    with tempfile.NamedTemporaryFile("wb") as fp:
        fp.write(b"123\n")
        fp.flush()
        with jsonlines.open(fp.name) as reader:
            assert list(reader) == [123]


def test_open_reading_with_utf8_bom() -> None:
    """
    The ``.open()`` helper ignores a UTF-8 BOM.
    """
    with tempfile.NamedTemporaryFile("wb") as fp:
        fp.write(codecs.BOM_UTF8)
        fp.write(b"123\n")
        fp.flush()
        with jsonlines.open(fp.name) as reader:
            assert list(reader) == [123]


def test_open_writing() -> None:
    with tempfile.NamedTemporaryFile("w+b") as fp:
        with jsonlines.open(fp.name, mode="w") as writer:
            writer.write(123)
        assert fp.read() == b"123\n"
    assert fp.name in repr(writer)


def test_open_and_append_writing() -> None:
    with tempfile.NamedTemporaryFile("w+b") as fp:
        with jsonlines.open(fp.name, mode="w") as writer:
            nbytes = writer.write(123)
            assert nbytes == len(str(123)) + 1
        with jsonlines.open(fp.name, mode="a") as writer:
            nbytes = writer.write(456)
            assert nbytes == len(str(456)) + 1
        assert fp.read() == b"123\n456\n"


def test_open_invalid_mode() -> None:
    with pytest.raises(ValueError) as excinfo:
        jsonlines.open("foo", mode="foo")
    assert "mode" in str(excinfo.value)


def test_single_char_stripping() -> None:
    """ "
    Sanity check that a helper constant actually contains single-char strings.
    """
    assert all(len(s) == 1 for s in jsonlines.jsonlines.SKIPPABLE_SINGLE_INITIAL_CHARS)


def test_writer_partial_writes_text() -> None:
    """
    A stream that accepts a single character per call still gets the whole line.
    """
    fp = PartialWriteTextStream(limit=1)
    with jsonlines.Writer(fp) as writer:
        nchars = writer.write({"a": 1})
    assert fp.getvalue() == '{"a": 1}\n'
    assert nchars == len('{"a": 1}\n')


def test_writer_partial_writes_binary() -> None:
    """
    Binary streams count bytes, and the newline is subject to short writes too.
    """
    fp = PartialWriteBinaryStream(limit=3)
    with jsonlines.Writer(fp) as writer:
        nbytes = writer.write({"a": 1})
        nbytes += writer.write({"b": 2})
    assert fp.getvalue() == SAMPLE_BYTES
    assert nbytes == len(SAMPLE_BYTES)


def test_writer_partial_write_newline() -> None:
    """
    A stream that accepts everything except the final newline still gets it.
    """
    line = b'{"a": 1}'
    fp = PartialWriteBinaryStream(limit=len(line))
    writer = jsonlines.Writer(fp)
    assert writer.write({"a": 1}) == len(line) + 1
    assert fp.getvalue() == line + b"\n"


def test_writer_partial_writes_are_not_duplicated() -> None:
    """
    Only the remainder of the line is retried, never already accepted data.
    """
    fp = PartialWriteBinaryStream(limit=4)
    writer = jsonlines.Writer(fp)
    writer.write(12345)
    assert fp.getvalue() == b"12345\n"


@pytest.mark.parametrize("result", [0, -1, 1.0, None, "5", True])
def test_writer_invalid_write_result(result: object) -> None:
    """
    Invalid write() results raise WriteError instead of silently losing data.
    """
    fp = BrokenWriteStream(result)
    writer = jsonlines.Writer(fp)
    with pytest.raises(jsonlines.WriteError) as excinfo:
        writer.write(123)

    exc = excinfo.value
    assert exc.written == 0
    assert exc.total == len(b"123\n")
    assert "stream.write()" in str(exc)
    assert isinstance(exc, jsonlines.Error)
    assert isinstance(exc, OSError)


def test_writer_write_zero_progress_mid_line() -> None:
    """
    A stream that stops accepting data reports how much of the line got through.
    """
    fp = DyingWriteStream(accept=2)
    writer = jsonlines.Writer(fp)
    with pytest.raises(jsonlines.WriteError) as excinfo:
        writer.write(123)

    exc = excinfo.value
    assert fp.getvalue() == b"12"
    assert exc.written == 2
    assert exc.total == len(b"123\n")
    assert "no progress" in str(exc)
    assert fp.flushes == 0


def test_writer_write_error_keeps_partial_output() -> None:
    """
    Output accepted before the failure is kept; it cannot be retracted.
    """
    fp = DyingWriteStream(accept=2)
    writer = jsonlines.Writer(fp)
    with pytest.raises(jsonlines.WriteError):
        writer.write(123)
    # The partial line remains; nothing pretends it was rolled back.
    assert fp.getvalue() == b"12"


def test_writer_partial_write_counts_are_per_stream_kind() -> None:
    """
    Text streams count characters; binary streams count bytes.
    """
    text_fp = PartialWriteTextStream(limit=2)
    binary_fp = PartialWriteBinaryStream(limit=2)

    with jsonlines.Writer(text_fp) as writer:
        assert writer.write("é") == len('"é"') + 1
        assert text_fp.getvalue() == '"é"\n'

    with jsonlines.Writer(binary_fp) as writer:
        assert writer.write("é") == len('"é"\n'.encode("utf-8"))
        assert binary_fp.getvalue() == '"é"\n'.encode("utf-8")


def test_writer_flush_only_after_complete_line() -> None:
    """
    A partial write must not be followed by a flush that claims success.
    """
    fp = DyingWriteStream(accept=1)
    writer = jsonlines.Writer(fp, flush=True)
    with pytest.raises(jsonlines.WriteError):
        writer.write(123)
    assert fp.flushes == 0

    ok_fp = PartialWriteBinaryStream(limit=1)
    with jsonlines.Writer(ok_fp, flush=True) as flushing_writer:
        flushing_writer.write(123)
    assert ok_fp.flushes == 1  # once per completely written line


def test_writer_write_all_stops_at_failing_line() -> None:
    """
    write_all only counts complete lines, and reports the failure.
    """
    fp = DyingWriteStream(accept=len(b"1\n") + len(b"22\n") + 1)
    writer = jsonlines.Writer(fp)
    with pytest.raises(jsonlines.WriteError) as excinfo:
        writer.write_all([1, 22, 333])

    exc = excinfo.value
    # The first two lines were written completely; the third stopped halfway.
    assert fp.getvalue() == b"1\n22\n3"
    assert exc.written == len(b"3")
    assert exc.total == len(b"333\n")


def test_writer_write_all_counts_complete_records() -> None:
    fp = PartialWriteBinaryStream(limit=1)
    writer = jsonlines.Writer(fp)
    assert writer.write_all([1, 2, 3]) == len(b"1\n2\n3\n")
    assert fp.getvalue() == b"1\n2\n3\n"


def test_writer_no_rollback_of_partial_line() -> None:
    """
    A partial line is never retracted, even on a seekable stream.
    """
    fp = SeekSpyStream(accept=1)
    writer = jsonlines.Writer(fp)
    with pytest.raises(jsonlines.WriteError):
        writer.write({"a": 1})

    # The already accepted character stays; no seek()/truncate() was attempted.
    assert fp.getvalue() == b"{"
