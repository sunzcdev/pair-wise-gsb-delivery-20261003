"""
Tests for the jsonlines library.
"""

import asyncio
import codecs
import collections
import io
import json
import tempfile

import jsonlines
import pytest


SAMPLE_BYTES = b'{"a": 1}\n{"b": 2}\n'
SAMPLE_TEXT = SAMPLE_BYTES.decode("utf-8")


class AsyncLineSource:
    """
    Async iterable yielding lines, tracking pulls and aclose() calls.
    """

    def __init__(self, lines):
        self.lines = list(lines)
        self.pulls = 0
        self.aclosed = False

    def __aiter__(self):
        return self

    async def __anext__(self):
        if not self.lines:
            raise StopAsyncIteration
        self.pulls += 1
        return self.lines.pop(0)

    async def aclose(self):
        self.aclosed = True


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


def test_async_reader() -> None:
    async def main() -> None:
        source = AsyncLineSource(['{"a": 1}', '{"b": 2}'])
        async with jsonlines.AsyncReader(source) as reader:
            assert await reader.read() == {"a": 1}
            assert await reader.read() == {"b": 2}
            with pytest.raises(EOFError):
                await reader.read()
        with pytest.raises(RuntimeError):
            await reader.read()

    asyncio.run(main())


def test_async_reader_iteration() -> None:
    async def main() -> None:
        source = AsyncLineSource(["1", "2", "3"])
        reader = jsonlines.AsyncReader(source)
        assert [obj async for obj in reader] == [1, 2, 3]
        assert "wrapping <AsyncLineSource at " in repr(reader)

    asyncio.run(main())


def test_async_reader_lazy_pulls() -> None:
    """
    The next line is only requested from the source when asked for.
    """

    async def main() -> None:
        source = AsyncLineSource(["1", "2", "3"])
        reader = jsonlines.AsyncReader(source)
        assert source.pulls == 0
        assert await reader.read() == 1
        assert source.pulls == 1
        assert await reader.read() == 2
        assert source.pulls == 2

    asyncio.run(main())


def test_async_reader_iter_flags() -> None:
    async def main() -> None:
        source = AsyncLineSource(["1", "", "invalid", "null", "2"])
        reader = jsonlines.AsyncReader(source)
        result = [
            obj
            async for obj in reader.iter(
                allow_none=True, skip_empty=True, skip_invalid=True
            )
        ]
        assert result == [1, None, 2]

    asyncio.run(main())


def test_async_reader_typed_reads() -> None:
    async def main() -> None:
        source = AsyncLineSource(["12", "true"])
        reader = jsonlines.AsyncReader(source)
        assert await reader.read(type=int) == 12
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            await reader.read(type=int)
        exc = excinfo.value
        assert "does not match requested type" in str(exc)
        assert exc.line == "true"
        assert exc.lineno == 2

        reader = jsonlines.AsyncReader(AsyncLineSource([]))
        with pytest.raises(ValueError) as excinfo:
            await reader.read(type="nope")  # type: ignore[call-overload]
        assert "invalid type specified" in str(excinfo.value)

    asyncio.run(main())


def test_async_reader_line_numbers_match_sync_reader() -> None:
    """
    Line numbers in errors are identical to the synchronous Reader.
    """

    async def main() -> None:
        lines = ["1", "", "  ", "invalid", "true"]
        sync_reader = jsonlines.Reader(list(lines))
        async_reader = jsonlines.AsyncReader(AsyncLineSource(lines))
        assert sync_reader.read(skip_empty=True) == 1
        assert await async_reader.read(skip_empty=True) == 1
        with pytest.raises(jsonlines.InvalidLineError) as sync_excinfo:
            sync_reader.read(skip_empty=True)
        with pytest.raises(jsonlines.InvalidLineError) as async_excinfo:
            await async_reader.read(skip_empty=True)
        assert async_excinfo.value.lineno == sync_excinfo.value.lineno == 4
        assert async_excinfo.value.line == sync_excinfo.value.line

    asyncio.run(main())


def test_async_reader_bytes_and_bom() -> None:
    async def main() -> None:
        source = AsyncLineSource([codecs.BOM_UTF8 + b"1", b"\xff"])
        reader = jsonlines.AsyncReader(source)
        assert await reader.read() == 1
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            await reader.read()
        assert "line is not valid utf-8" in str(excinfo.value)

    asyncio.run(main())


def test_async_reader_custom_loads_is_called_synchronously() -> None:
    calls = []

    def loads(s):
        calls.append(s)
        return "uh what"

    async def main() -> None:
        reader = jsonlines.AsyncReader(AsyncLineSource(["{}"]), loads=loads)
        assert await reader.read() == "uh what"

    asyncio.run(main())
    assert calls == ["{}"]


def test_async_reader_close_does_not_aclose_source() -> None:
    async def main() -> None:
        source = AsyncLineSource(["1"])
        async with jsonlines.AsyncReader(source) as reader:
            assert await reader.read() == 1
        assert not source.aclosed
        reader.close()
        reader.close()  # no-op
        assert not source.aclosed

    asyncio.run(main())


def test_async_reader_cancellation_propagates() -> None:
    """
    Cancelling a pending read raises CancelledError (it is not
    swallowed) and does not consume a line.
    """

    class BlockingSource:
        def __init__(self):
            self.started = 0
            self.lines = []

        def __aiter__(self):
            return self

        async def __anext__(self):
            if self.lines:
                return self.lines.pop(0)
            self.started += 1
            await asyncio.Event().wait()  # blocks forever
            raise AssertionError("unreachable")  # pragma: no cover

    async def main() -> None:
        source = BlockingSource()
        reader = jsonlines.AsyncReader(source)
        task = asyncio.ensure_future(reader.read())
        await asyncio.sleep(0)  # let the read() start and block on the source
        assert source.started == 1
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        # No line was consumed by the cancelled read; the reader is
        # still usable and the next line gets line number 1.
        source.lines.append("invalid")
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            await reader.read()
        assert excinfo.value.lineno == 1

    asyncio.run(main())
