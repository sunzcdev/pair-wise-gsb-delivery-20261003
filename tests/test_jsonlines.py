"""
Tests for the jsonlines library.
"""

import codecs
import collections
import io
import json
import tempfile

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


# ---------------------------------------------------------------------------
# Explicit, strict format modes
# ---------------------------------------------------------------------------

BOM = codecs.BOM_UTF8.decode()
RS = jsonlines.jsonlines.RECORD_SEPARATOR


def test_format_enum_is_public_api() -> None:
    assert jsonlines.Format is jsonlines.jsonlines.Format
    assert {member.name for member in jsonlines.Format} == {
        "JSON_LINES",
        "RFC7464_TEXT_SEQUENCES",
    }


def test_reader_format_is_keyword_only() -> None:
    with pytest.raises(TypeError):
        jsonlines.Reader(io.StringIO(), jsonlines.Format.JSON_LINES)  # type: ignore


def test_lenient_mode_remains_default() -> None:
    # The historical behaviour is unchanged when no format is selected.
    with jsonlines.Reader(io.StringIO(RS + '"a"\n' + BOM + "2\n")) as reader:
        assert list(reader) == ["a", 2]


def test_lenient_mode_does_not_strip_in_string_chars() -> None:
    # Only the very first character of a line is ever inspected, so
    # characters embedded in JSON strings survive untouched.
    with jsonlines.Reader(io.StringIO('"a' + RS + 'b"\n')) as reader:
        with pytest.raises(jsonlines.InvalidLineError):
            reader.read()


def test_json_lines_basic() -> None:
    fp = io.StringIO("1\n2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.JSON_LINES) as reader:
        assert list(reader) == [1, 2]


def test_json_lines_one_value_per_physical_line() -> None:
    # Multiple values on a single physical line are not valid JSON Lines.
    fp = io.StringIO("1 2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.JSON_LINES) as reader:
        with pytest.raises(jsonlines.InvalidLineError):
            reader.read()


def test_json_lines_bom_only_at_stream_start_text() -> None:
    fp = io.StringIO(BOM + "1\n2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.JSON_LINES) as reader:
        assert list(reader) == [1, 2]


def test_json_lines_bom_only_at_stream_start_bytes() -> None:
    fp = io.BytesIO(codecs.BOM_UTF8 + b"1\n2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.JSON_LINES) as reader:
        assert list(reader) == [1, 2]


def test_json_lines_bom_mid_stream_is_rejected() -> None:
    fp = io.StringIO("1\n" + BOM + "2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.JSON_LINES) as reader:
        assert reader.read() == 1  # first line is fine
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
    exc = excinfo.value
    assert "BOM" in str(exc)
    assert exc.lineno == 2
    assert exc.column == 1
    assert exc.position == 2


def test_json_lines_record_separator_is_rejected_with_position() -> None:
    fp = io.StringIO("1\n" + RS + "2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.JSON_LINES) as reader:
        assert reader.read() == 1
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
    exc = excinfo.value
    assert "record separator" in str(exc)
    assert exc.lineno == 2
    assert exc.column == 1
    assert exc.position == 2


def test_json_lines_record_separator_inside_string_is_not_stripped() -> None:
    # A raw RS inside a JSON string must not be treated as a separator;
    # it makes the line invalid JSON, and the error points at its position.
    fp = io.StringIO('"a' + RS + 'b"\n')
    with jsonlines.Reader(fp, format=jsonlines.Format.JSON_LINES) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
    exc = excinfo.value
    assert "record separator" in str(exc)
    assert exc.column == 3
    assert exc.position == 2
    # The RS was not removed from the offending line.
    assert RS in exc.line


def test_json_lines_bom_inside_string_is_preserved() -> None:
    fp = io.StringIO(json.dumps("x" + BOM + "y", ensure_ascii=False) + "\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.JSON_LINES) as reader:
        assert reader.read() == "x" + BOM + "y"


def test_json_lines_empty_line_handling() -> None:
    fp = io.StringIO("1\n\n2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.JSON_LINES) as reader:
        assert reader.read() == 1
        with pytest.raises(jsonlines.InvalidLineError):
            reader.read()
    with jsonlines.Reader(
        io.StringIO("1\n\n2\n"), format=jsonlines.Format.JSON_LINES
    ) as reader:
        assert list(reader.iter(skip_empty=True)) == [1, 2]


def test_rfc7464_basic_records() -> None:
    fp = io.StringIO(RS + '"a"\n' + RS + '"b"\n')
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        assert list(reader) == ["a", "b"]


def test_rfc7464_records_from_bytes() -> None:
    fp = io.BytesIO(b'\x1e"a"\x0a\x1e"b"\x0a')
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        assert list(reader) == ["a", "b"]


def test_rfc7464_record_can_span_physical_lines() -> None:
    data = RS + '{\n  "x": 1\n}\n' + RS + "2\n"
    fp = io.StringIO(data)
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        assert list(reader) == [{"x": 1}, 2]


def test_rfc7464_consecutive_records_without_line_feed() -> None:
    # RS immediately following an RS starts the next record.
    fp = io.StringIO(RS + "1" + RS + "2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        assert reader.read() == 1
        assert reader.read() == 2


def test_rfc7464_empty_record_is_invalid_by_default() -> None:
    fp = io.StringIO(RS + "1\n" + RS + RS + "2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        assert reader.read() == 1
        with pytest.raises(jsonlines.InvalidLineError):
            reader.read()


def test_rfc7464_empty_record_can_be_skipped() -> None:
    fp = io.StringIO(RS + "1\n" + RS + RS + "2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        assert list(reader.iter(skip_empty=True)) == [1, 2]


def test_rfc7464_trailing_record_separator() -> None:
    fp = io.StringIO(RS + "1\n" + RS)
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        assert reader.read() == 1
        with pytest.raises(jsonlines.InvalidLineError):
            reader.read()


def test_rfc7464_requires_leading_record_separator() -> None:
    fp = io.StringIO("1\n" + RS + "2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
    exc = excinfo.value
    assert "record separator" in str(exc)
    assert exc.lineno == 1
    assert exc.column == 1
    assert exc.position == 0


def test_rfc7464_leading_bom_is_accepted() -> None:
    fp = io.StringIO(BOM + RS + "1\n" + RS + "2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        assert list(reader) == [1, 2]


def test_rfc7464_bom_inside_record_is_rejected() -> None:
    fp = io.StringIO(RS + "1" + BOM + "\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
    exc = excinfo.value
    assert "BOM" in str(exc)
    assert exc.position == 2


def test_rfc7464_record_separator_inside_string_is_preserved() -> None:
    # An escaped (valid) RS inside a JSON string survives as data and is
    # never mistaken for a record delimiter.
    data = RS + json.dumps("a" + RS + "b") + "\n" + RS + "2\n"
    fp = io.StringIO(data)
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        assert reader.read() == "a" + RS + "b"
        assert reader.read() == 2


def test_rfc7464_typed_and_allow_none_options() -> None:
    fp = io.StringIO(RS + "1\n" + RS + "null\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        assert reader.read(type=int) == 1
        assert reader.read(allow_none=True) is None


def test_mixed_format_json_lines_input_in_rfc_mode_is_rejected() -> None:
    # JSON Lines framing (no RS) is invalid for RFC 7464 parsing.
    fp = io.StringIO("1\n2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.RFC7464_TEXT_SEQUENCES) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
    assert "record separator" in str(excinfo.value)


def test_mixed_format_rfc_input_in_json_lines_mode_is_rejected() -> None:
    fp = io.StringIO(RS + "1\n" + RS + "2\n")
    with jsonlines.Reader(fp, format=jsonlines.Format.JSON_LINES) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
    assert "record separator" in str(excinfo.value)


def test_invalid_line_error_position_attributes_default_to_none() -> None:
    exc = jsonlines.InvalidLineError("boom", "line", 1)
    assert exc.column is None
    assert exc.position is None
    assert str(exc) == "boom (line 1)"
