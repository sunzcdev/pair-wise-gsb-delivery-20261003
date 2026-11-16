"""
Tests for the jsonlines library.
"""

import codecs
import collections
import io
import json
import os
import tempfile
import zlib

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

def gzip_member(data: bytes) -> bytes:
    compressor = zlib.compressobj(9, zlib.DEFLATED, 16 | zlib.MAX_WBITS)
    return compressor.compress(data) + compressor.flush()


def test_open_gzip_archive_single_member() -> None:
    fp = io.BytesIO(gzip_member(b'{"a": 1}\n{"b": 2}\n'))
    with jsonlines.open_gzip_archive(fp) as reader:
        assert list(reader) == [{"a": 1}, {"b": 2}]


def test_open_gzip_archive_concatenated_members() -> None:
    archive = gzip_member(b"1\n2\n") + gzip_member(b"3\n") + gzip_member(b"4\n5\n")
    fp = io.BytesIO(archive)
    with jsonlines.open_gzip_archive(fp) as reader:
        assert list(reader) == [1, 2, 3, 4, 5]


def test_open_gzip_archive_member_boundary_is_not_a_line_break() -> None:
    # The last line of a member is continued by the next member.
    archive = gzip_member(b'{"a":') + gzip_member(b" 1}\n") + gzip_member(b'{"b": 2}\n')
    fp = io.BytesIO(archive)
    with jsonlines.open_gzip_archive(fp) as reader:
        assert list(reader) == [{"a": 1}, {"b": 2}]


def test_open_gzip_archive_final_line_without_newline() -> None:
    fp = io.BytesIO(gzip_member(b"1\n2"))
    with jsonlines.open_gzip_archive(fp) as reader:
        assert list(reader) == [1, 2]


def test_open_gzip_archive_empty() -> None:
    fp = io.BytesIO(b"")
    with jsonlines.open_gzip_archive(fp) as reader:
        assert list(reader) == []


def test_open_gzip_archive_reader_semantics() -> None:
    # The returned reader behaves like any other jsonlines.Reader.
    archive = gzip_member(b"1\n\nnonsense\n2\n")
    fp = io.BytesIO(archive)
    with jsonlines.open_gzip_archive(fp) as reader:
        assert list(reader.iter(skip_empty=True, skip_invalid=True)) == [1, 2]
    fp = io.BytesIO(gzip_member(b"{}\n"))
    with jsonlines.open_gzip_archive(fp) as reader:
        assert reader.read(type=dict) == {}
        with pytest.raises(EOFError):
            reader.read()


def test_open_gzip_archive_corrupt_member() -> None:
    archive = gzip_member(b"1\n2\n") + gzip_member(b"3\n") + b"not a gzip member"
    fp = io.BytesIO(archive)
    with jsonlines.open_gzip_archive(fp) as reader:
        with pytest.raises(jsonlines.InvalidArchiveError) as excinfo:
            list(reader.iter(skip_invalid=True))
    exc = excinfo.value
    assert exc.member_index == 2
    assert exc.lines_read == 3
    assert isinstance(exc, jsonlines.Error)
    assert not isinstance(exc, jsonlines.InvalidLineError)


def test_open_gzip_archive_corrupt_member_midway() -> None:
    good = gzip_member(b"1\n")
    corrupt = bytearray(gzip_member(b"2\n3\n"))
    corrupt[-8:-4] = b"\xff\xff\xff\xff"  # corrupt the crc32
    fp = io.BytesIO(good + bytes(corrupt))
    with jsonlines.open_gzip_archive(fp) as reader:
        with pytest.raises(jsonlines.InvalidArchiveError) as excinfo:
            list(reader)
    assert excinfo.value.member_index == 1


def test_open_gzip_archive_truncated_member() -> None:
    # The second member is missing its gzip trailer, so all of its
    # content lines are still delivered before the truncation error.
    archive = gzip_member(b"1\n") + gzip_member(b"2\n3\n")[:15]
    fp = io.BytesIO(archive)
    with jsonlines.open_gzip_archive(fp) as reader:
        with pytest.raises(jsonlines.InvalidArchiveError) as excinfo:
            list(reader.iter(skip_invalid=True))
    exc = excinfo.value
    assert exc.member_index == 1
    assert exc.lines_read == 3
    assert "truncated" in str(exc)


def test_open_gzip_archive_not_gzip_at_all() -> None:
    fp = io.BytesIO(b"1\n2\n")
    with jsonlines.open_gzip_archive(fp) as reader:
        with pytest.raises(jsonlines.InvalidArchiveError) as excinfo:
            list(reader)
    assert excinfo.value.member_index == 0
    assert excinfo.value.lines_read == 0


def test_open_gzip_archive_by_filename(tmp_path) -> None:
    path = tmp_path / "archive.jsonl.gz"
    path.write_bytes(gzip_member(b"1\n") + gzip_member(b"2\n"))
    with jsonlines.open_gzip_archive(str(path)) as reader:
        assert list(reader) == [1, 2]
    assert reader._fp.closed


def test_open_gzip_archive_custom_loads() -> None:
    fp = io.BytesIO(gzip_member(b'"a"\n'))
    loads = lambda s: json.loads(s).upper()  # noqa: E731
    with jsonlines.open_gzip_archive(fp, loads=loads) as reader:
        assert list(reader) == ["A"]


def test_open_gzip_archive_incremental_decompression() -> None:
    # Decompression must happen lazily, without reading the whole
    # archive up front. Use incompressible data so that the compressed
    # size exceeds the read chunk size.
    reads = []

    class TrackingReader(io.BytesIO):
        def read(self, n=-1):
            data = super().read(n)
            reads.append(len(data))
            return data

    archive = gzip_member(os.urandom(128 * 1024)) + gzip_member(b"2\n")
    fp = TrackingReader(archive)
    it = jsonlines.jsonlines._iter_gzip_archive_lines(fp)
    next(it)
    assert sum(reads) < len(archive)  # second member not read yet
    # The first member's final partial line is continued by the second
    # member, so the last line ends with the second member's content.
    assert list(it)[-1].endswith(b"2\n")
