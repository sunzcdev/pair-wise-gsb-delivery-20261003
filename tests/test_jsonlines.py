"""
Tests for the jsonlines library.
"""

import asyncio
import codecs
import collections
import io
import json
import tempfile
from typing import AsyncIterator, List, Sequence, Union

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


async def async_iter(
    lines: Sequence[Union[str, bytes]]
) -> AsyncIterator[Union[str, bytes]]:
    for line in lines:
        yield line


def test_async_reader() -> None:
    async def main() -> None:
        reader = jsonlines.AsyncReader(async_iter(SAMPLE_TEXT.splitlines()))
        assert await reader.read() == {"a": 1}
        assert await reader.read() == {"b": 2}
        with pytest.raises(EOFError):
            await reader.read()

    asyncio.run(main())


def test_async_reader_iteration() -> None:
    async def main() -> None:
        reader = jsonlines.AsyncReader(async_iter(["1", "2", "3"]))
        assert [obj async for obj in reader] == [1, 2, 3]

        reader = jsonlines.AsyncReader(async_iter(["1", "invalid", "2"]))
        assert [obj async for obj in reader.iter(skip_invalid=True)] == [1, 2]

        reader = jsonlines.AsyncReader(async_iter(["1", "", "2"]))
        assert [obj async for obj in reader.iter(skip_empty=True)] == [1, 2]

    asyncio.run(main())


def test_async_reader_typed_reads() -> None:
    async def main() -> None:
        reader = jsonlines.AsyncReader(
            async_iter(['12', 'true', '"foo"', "null", "null"])
        )
        assert await reader.read(type=int) == 12
        with pytest.raises(jsonlines.InvalidLineError) as excinfo:
            await reader.read(type=int)
        assert "does not match requested type" in str(excinfo.value)
        with pytest.raises(jsonlines.InvalidLineError):
            await reader.read(type=float)
        with pytest.raises(jsonlines.InvalidLineError):
            await reader.read()
        assert await reader.read(type=dict, allow_none=True) is None

        reader = jsonlines.AsyncReader(async_iter([]))
        with pytest.raises(ValueError):
            await reader.read(type="nope")  # type: ignore[call-overload]

    asyncio.run(main())


def test_async_reader_custom_loads() -> None:
    async def main() -> None:
        calls = []  # type: List[Union[str, bytes]]

        def loads(s: Union[str, bytes]) -> str:
            calls.append(s)
            return "uh what"

        reader = jsonlines.AsyncReader(async_iter(["{}"]), loads=loads)
        assert await reader.read() == "uh what"
        assert calls == ["{}"]

    asyncio.run(main())


def test_async_reader_lineno_matches_sync_reader() -> None:
    """
    Line numbers in errors must be identical for the same input.
    """
    lines = ["1", "", "invalid", "2"]

    async def main() -> None:
        sync_reader = jsonlines.Reader(list(lines))
        async_reader = jsonlines.AsyncReader(async_iter(lines))
        for _ in lines:
            try:
                sync_reader.read()
            except jsonlines.InvalidLineError as sync_exc:
                with pytest.raises(jsonlines.InvalidLineError) as excinfo:
                    await async_reader.read()
                async_exc = excinfo.value
                assert async_exc.lineno == sync_exc.lineno
                assert async_exc.line == sync_exc.line
            else:
                await async_reader.read()

    asyncio.run(main())


def test_async_reader_lazy_pulling() -> None:
    """
    The next line is only requested from the source when needed.
    """
    pulled = []  # type: List[str]

    async def source() -> AsyncIterator[str]:
        for line in ["1", "2", "3"]:
            pulled.append(line)
            yield line

    async def main() -> None:
        reader = jsonlines.AsyncReader(source())
        assert pulled == []
        assert await reader.read() == 1
        assert pulled == ["1"]
        assert await reader.read() == 2
        assert pulled == ["1", "2"]

    asyncio.run(main())


def test_async_reader_cancellation_not_swallowed() -> None:
    """
    A cancelled wait propagates CancelledError and does not consume a line.
    """

    class GatedSource:
        """
        Async iterator that blocks on a gate before yielding further
        lines, and survives cancellation of a pending __anext__().
        """

        def __init__(self, lines: List[str], gate: asyncio.Event) -> None:
            self._lines = lines
            self._gate = gate
            self._index = 0

        def __aiter__(self) -> "GatedSource":
            return self

        async def __anext__(self) -> str:
            if self._index >= len(self._lines):
                raise StopAsyncIteration
            if self._index > 0:
                await self._gate.wait()
            line = self._lines[self._index]
            self._index += 1
            return line

    async def main() -> None:
        gate = asyncio.Event()
        reader = jsonlines.AsyncReader(GatedSource(["1", "2"], gate))
        assert await reader.read() == 1

        task = asyncio.ensure_future(reader.read())
        await asyncio.sleep(0)  # let the task start waiting on the source
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

        # The cancelled read did not consume a line: after unblocking the
        # source, the pending line is still delivered exactly once.
        gate.set()
        assert await reader.read() == 2
        with pytest.raises(EOFError):
            await reader.read()

    asyncio.run(main())


def test_async_reader_close_does_not_close_source() -> None:
    """
    Closing the reader must not aclose() the caller-owned source.
    """
    state = {"closed": False}

    async def source() -> AsyncIterator[str]:
        try:
            yield "1"
            yield "2"
        finally:
            state["closed"] = True

    async def main() -> None:
        gen = source()
        async with jsonlines.AsyncReader(gen) as reader:
            assert await reader.read() == 1
        assert not state["closed"]
        assert [obj async for obj in gen] == ["2"] and state["closed"]

        # Plain context manager works too, and also leaves the source alone.
        state["closed"] = False
        gen = source()
        with jsonlines.AsyncReader(gen) as reader:
            assert await reader.read() == 1
        assert not state["closed"]
        await gen.aclose()

        reader = jsonlines.AsyncReader(async_iter([]))
        reader.close()
        with pytest.raises(RuntimeError):
            await reader.read()

    asyncio.run(main())
