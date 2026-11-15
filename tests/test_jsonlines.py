"""
Tests for the jsonlines library.
"""

import codecs
import collections
import io
import json
import tempfile
from typing import Iterable

import attr
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


def test_rejection_reasons_on_read() -> None:
    """
    ``read()`` attaches a machine readable reason to the exception.
    """
    with jsonlines.Reader([b"\xff\xff"]) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
        assert excinfo.value.reason is jsonlines.RejectionReason.DECODE

    with jsonlines.Reader(["{bad"]) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
        assert excinfo.value.reason is jsonlines.RejectionReason.SYNTAX

    with jsonlines.Reader(["null"]) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
        assert excinfo.value.reason is jsonlines.RejectionReason.NULL

    with jsonlines.Reader(["true"]) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read(type=int)
        assert excinfo.value.reason is jsonlines.RejectionReason.TYPE


def test_rejection_preview_is_truncated() -> None:
    """
    The preview is bounded, even for huge rejected lines.
    """
    long_line = "[" + "x" * (jsonlines.jsonlines.REJECTION_PREVIEW_LIMIT * 2)
    with jsonlines.Reader([long_line]) as reader:
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            reader.read()
        exc = excinfo.value

    assert exc.line == long_line
    assert exc.preview.startswith("[xxx")
    assert len(exc.preview) == jsonlines.jsonlines.REJECTION_PREVIEW_LIMIT + 1
    assert exc.preview.endswith("…")


def test_on_reject_observation() -> None:
    """
    Rejected lines are reported with line number, reason, and preview,
    while iteration still only yields valid lines.
    """
    data = b'{"ok": 1}\n\xff\xff\n{bad\nnull\n12\n'
    seen: list[jsonlines.RejectedLine] = []
    reader = jsonlines.Reader(io.BytesIO(data))
    values = list(reader.iter(skip_invalid=True, on_reject=seen.append))

    assert values == [{"ok": 1}, 12]
    assert [(r.lineno, r.reason) for r in seen] == [
        (2, jsonlines.RejectionReason.DECODE),
        (3, jsonlines.RejectionReason.SYNTAX),
        (4, jsonlines.RejectionReason.NULL),
    ]
    assert [r.preview for r in seen] == [
        "\\xff\\xff",
        "{bad",
        "null",
    ]
    assert all(r.preview == r.preview.strip() for r in seen)

    report = reader.rejection_report
    assert report is not None
    assert report.total == 3
    assert report.count(jsonlines.RejectionReason.DECODE) == 1
    assert report.count(jsonlines.RejectionReason.SYNTAX) == 1
    assert report.count(jsonlines.RejectionReason.NULL) == 1
    assert report.count(jsonlines.RejectionReason.TYPE) == 0
    assert report.max_consecutive == 3
    assert report.consecutive == 0
    assert not report.incomplete
    assert report.stop_reason is None


def test_rejection_report_as_callback() -> None:
    """
    A report instance can be passed directly as the observer.
    """
    report = jsonlines.RejectionReport()
    reader = jsonlines.Reader(["1", "nope", "2"])
    assert list(reader.iter(skip_invalid=True, on_reject=report)) == [1, 2]
    assert report.total == 1
    assert report.counts[jsonlines.RejectionReason.SYNTAX] == 1

    # The reader's own report is a separate instance for this run.
    assert reader.rejection_report is not report
    assert reader.rejection_report is not None
    assert reader.rejection_report.total == 1


def test_rejection_report_absent_without_iter() -> None:
    reader = jsonlines.Reader(["1"])
    assert reader.rejection_report is None


def test_observer_does_not_change_skipping() -> None:
    """
    With ``skip_invalid=False`` the observer sees the rejection, but the
    error is still raised.
    """
    seen: list[jsonlines.RejectedLine] = []
    reader = jsonlines.Reader(["1", "nope", "2"])
    it = reader.iter(on_reject=seen.append)

    assert next(it) == 1
    with pytest.raises(jsonlines.InvalidLineError):
        next(it)
    assert [(r.lineno, r.reason) for r in seen] == [
        (2, jsonlines.RejectionReason.SYNTAX)
    ]


def test_observer_error_terminates_iteration() -> None:
    """
    A failing observer terminates iteration; its exception propagates
    unchanged and the report is marked incomplete.
    """
    seen: list[jsonlines.RejectedLine] = []

    def failing_observer(rejected: jsonlines.RejectedLine) -> None:
        seen.append(rejected)
        raise RuntimeError("observer failed")

    reader = jsonlines.Reader(["1", "nope", "2", "nope", "3"])
    it = reader.iter(skip_invalid=True, on_reject=failing_observer)

    assert next(it) == 1
    with pytest.raises(RuntimeError, match="observer failed"):
        next(it)
    with pytest.raises(StopIteration):
        next(it)

    assert len(seen) == 1
    report = reader.rejection_report
    assert report is not None
    assert report.total == 1
    assert report.incomplete
    assert report.stop_reason == "observer_error"


def test_observer_eof_error_is_not_swallowed() -> None:
    """
    An observer raising ``EOFError`` must not be mistaken for the end of
    the input; iteration stops with that error.
    """

    def failing_observer(rejected: jsonlines.RejectedLine) -> None:
        raise EOFError("observer eof")

    reader = jsonlines.Reader(["1", "nope", "2"])
    it = reader.iter(skip_invalid=True, on_reject=failing_observer)

    assert next(it) == 1
    with pytest.raises(EOFError, match="observer eof"):
        next(it)
    with pytest.raises(StopIteration):
        next(it)

    report = reader.rejection_report
    assert report is not None
    assert report.incomplete
    assert report.stop_reason == "observer_error"


def test_source_errors_are_not_swallowed() -> None:
    """
    Errors raised by the underlying source are never treated as
    rejected lines.
    """

    class SourceError(Exception):
        pass

    def failing_lines() -> Iterable[str]:
        yield "1"
        yield "invalid"
        raise SourceError("boom")

    seen: list[jsonlines.RejectedLine] = []
    reader = jsonlines.Reader(failing_lines())
    it = reader.iter(skip_invalid=True, on_reject=seen.append)

    assert next(it) == 1
    with pytest.raises(SourceError, match="boom"):
        next(it)
    assert [(r.lineno, r.reason) for r in seen] == [
        (2, jsonlines.RejectionReason.SYNTAX)
    ]


def test_source_eof_error_is_not_swallowed() -> None:
    """
    An ``EOFError`` raised by the source is an error, not the end of the
    input.
    """

    def failing_lines() -> Iterable[str]:
        yield "1"
        raise EOFError("source eof")

    reader = jsonlines.Reader(failing_lines())
    it = reader.iter(skip_invalid=True)
    assert next(it) == 1
    with pytest.raises(EOFError, match="source eof"):
        next(it)


def test_max_rejects_limit() -> None:
    """
    Exceeding the absolute rejection limit stops iteration and marks
    the report incomplete.
    """
    data = "1\nbad\n2\nbad\n3\nbad\n4\n"
    reader = jsonlines.Reader(io.StringIO(data))
    values = list(reader.iter(skip_invalid=True, max_rejects=2))

    # Two rejections are allowed; the third one stops iteration.
    assert values == [1, 2, 3]
    report = reader.rejection_report
    assert report is not None
    assert report.total == 3
    assert report.incomplete
    assert report.stop_reason == "max_rejects"

    # At the limit itself nothing is truncated.
    reader = jsonlines.Reader(io.StringIO(data))
    values = list(reader.iter(skip_invalid=True, max_rejects=3))
    assert values == [1, 2, 3, 4]
    assert reader.rejection_report is not None
    assert not reader.rejection_report.incomplete


def test_max_consecutive_rejects_limit() -> None:
    """
    Consecutive rejection counting resets on a valid line.
    """
    data = "bad\nbad\n1\nbad\nbad\n2\nbad\nbad\nbad\n3\n"
    reader = jsonlines.Reader(io.StringIO(data))
    values = list(reader.iter(skip_invalid=True, max_consecutive_rejects=2))

    assert values == [1, 2]
    report = reader.rejection_report
    assert report is not None
    assert report.total == 7
    assert report.count(jsonlines.RejectionReason.SYNTAX) == 7
    assert report.max_consecutive == 3
    assert report.incomplete
    assert report.stop_reason == "max_consecutive_rejects"

    reader = jsonlines.Reader(io.StringIO("bad\nbad\n1\n2\n"))
    assert list(reader.iter(skip_invalid=True, max_consecutive_rejects=2)) == [1, 2]
    assert reader.rejection_report is not None
    assert not reader.rejection_report.incomplete


def test_rejection_reason_values() -> None:
    """
    Reasons are plain strings, so they can be logged or serialized.
    """
    assert jsonlines.RejectionReason.DECODE.value == "decode"
    assert jsonlines.RejectionReason.SYNTAX.value == "syntax"
    assert jsonlines.RejectionReason.TYPE.value == "type"
    assert jsonlines.RejectionReason.NULL.value == "null"
    assert issubclass(jsonlines.RejectionReason, str)
    assert json.dumps(jsonlines.RejectionReason.SYNTAX) == '"syntax"'
    same: str = jsonlines.RejectionReason.SYNTAX
    assert same == "syntax"


def test_negative_rejection_limits_are_invalid() -> None:
    reader = jsonlines.Reader([])
    with pytest.raises(ValueError, match="max_rejects"):
        reader.iter(max_rejects=-1)
    with pytest.raises(ValueError, match="max_consecutive_rejects"):
        reader.iter(max_consecutive_rejects=-1)


def test_rejected_lines_are_not_retained() -> None:
    """
    The report only aggregates counts; it does not store rejected lines.
    """
    data = "bad\n" * 1000
    reader = jsonlines.Reader(io.StringIO(data))
    assert list(reader.iter(skip_invalid=True)) == []

    report = reader.rejection_report
    assert report is not None
    assert report.total == 1000
    assert report.max_consecutive == 1000
    # Only counters are kept; there is no per-line collection anywhere.
    for value in attr.asdict(report).values():
        if isinstance(value, dict):
            assert all(isinstance(count, int) for count in value.values())
        else:
            assert value is None or isinstance(value, (int, str))
