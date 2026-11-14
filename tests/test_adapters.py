import asyncio
import ijson
import pytest


def test_from_iter_read0_does_not_consume():
    chunks = [b'{"key":', b'"value"}']
    file_obj = ijson.from_iter(iter(chunks))
    assert file_obj.read(0) == b""
    assert file_obj.read(1) == b"{"
    assert file_obj.read(8) == b'"key":'
    assert file_obj.read(64) == b'"value"}'
    assert file_obj.read(1) == b""


def test_from_iter_read_respects_n_and_keeps_remainder():
    file_obj = ijson.from_iter(iter([b"abcdef", b"ghij"]))
    assert file_obj.read(2) == b"ab"
    assert file_obj.read(3) == b"cde"
    assert file_obj.read(10) == b"f"
    assert file_obj.read(10) == b"ghij"
    assert file_obj.read(1) == b""


def test_from_iter_skips_empty_chunks():
    chunks = [b"", b"{", b"", b"", b'"key"', b"", b": 1", b"", b"}", b""]
    file_obj = ijson.from_iter(iter(chunks))
    assert file_obj.read(64) == b"{"
    assert file_obj.read(64) == b'"key"'
    assert file_obj.read(64) == b": 1"
    assert file_obj.read(64) == b"}"
    assert file_obj.read(64) == b""


def test_from_iter_empty_chunks_between_tokens_parse():
    chunks = [b"[1", b"", b"2", b"", b",", b"", b'"a\\', b"", b'nb"', b"", b"]"]
    assert list(ijson.items(ijson.from_iter(iter(chunks)), "item")) == [12, "a\nb"]


def test_from_iter_empty_chunks_within_number():
    chunks = [b"1", b"", b"2", b"", b"3"]
    assert list(ijson.items(ijson.from_iter(iter(chunks)), "")) == [123]


def test_from_iter_empty_chunks_within_structure():
    chunks = [b"{", b"", b'"a"', b"", b":", b"", b"[", b"", b"]", b"", b"}"]
    assert list(ijson.kvitems(ijson.from_iter(iter(chunks)), "")) == [("a", [])]


def test_from_iter_multibyte_utf8_split_across_chunks():
    data = '"héllo wörld"'.encode("utf-8")
    chunks = [data[:3], b"", data[3:4], data[4:]]
    file_obj = ijson.from_iter(iter(chunks))
    collected = b""
    while True:
        piece = file_obj.read(2)
        if not piece:
            break
        collected += piece
    assert collected == data
    assert list(ijson.items(ijson.from_iter(iter(chunks)), "")) == ["héllo wörld"]


def test_from_iter_source_exception_propagates():
    def chunks():
        yield b"[1"
        raise ValueError("boom")

    file_obj = ijson.from_iter(chunks())
    assert file_obj.read(64) == b"[1"
    with pytest.raises(ValueError, match="boom"):
        file_obj.read(64)


def test_from_iter_rejects_invalid_chunk_type():
    file_obj = ijson.from_iter(iter([b"[", 1, b"]"]))
    assert file_obj.read(64) == b"["
    with pytest.raises(TypeError):
        file_obj.read(64)


@pytest.mark.parametrize(
    "chunks_factory",
    [list, tuple, iter, lambda chunks: (chunk for chunk in chunks)],
    ids=["list", "tuple", "iter", "generator"],
)
def test_from_iter_accepts_iterable(chunks_factory):
    chunks = chunks_factory([b'{"key":', b'"value"}'])
    file_obj = ijson.from_iter(chunks)
    assert file_obj.read(64) == b'{"key":'
    assert file_obj.read(64) == b'"value"}'
    assert file_obj.read(1) == b""


@pytest.mark.parametrize(
    "chunks_factory",
    [list, tuple, iter, lambda chunks: (chunk for chunk in chunks)],
    ids=["list", "tuple", "iter", "generator"],
)
def test_from_iter_accepts_string_and_warns(chunks_factory):
    chunks = chunks_factory(['{"key":', '"value"}'])
    file_obj = ijson.from_iter(chunks)
    with pytest.deprecated_call():
        assert file_obj.read(64) == b'{"key":'
    assert file_obj.read(64) == b'"value"}'
    assert file_obj.read(1) == b""


def test_from_iter_empty_string_chunks_skipped_without_warning_spam():
    chunks = ["", "[", "", "1", "", "]"]
    file_obj = ijson.from_iter(iter(chunks))
    with pytest.deprecated_call():
        assert file_obj.read(64) == b"["
    assert file_obj.read(64) == b"1"
    assert file_obj.read(64) == b"]"
    assert file_obj.read(64) == b""


def test_from_iter_accepts_aiterable():
    async def chunks():
        yield b'{"key":'
        yield b'"value"}'

    async def main():
        file_obj = ijson.from_iter(chunks())
        assert await file_obj.read(0) == b""
        assert await file_obj.read(64) == b'{"key":'
        assert await file_obj.read(64) == b'"value"}'
        assert await file_obj.read(1) == b""

    asyncio.run(main())


def test_from_iter_accepts_async_string_chunks_and_warns():
    async def chunks():
        yield '{"key":'
        yield '"value"}'

    async def main():
        file_obj = ijson.from_iter(chunks())
        with pytest.deprecated_call():
            assert await file_obj.read(64) == b'{"key":'
        assert await file_obj.read(64) == b'"value"}'
        assert await file_obj.read(1) == b""

    asyncio.run(main())


def test_from_iter_async_read_respects_n_and_keeps_remainder():
    async def chunks():
        yield b"abcdef"
        yield b"ghij"

    async def main():
        file_obj = ijson.from_iter(chunks())
        assert await file_obj.read(2) == b"ab"
        assert await file_obj.read(3) == b"cde"
        assert await file_obj.read(10) == b"f"
        assert await file_obj.read(10) == b"ghij"
        assert await file_obj.read(1) == b""

    asyncio.run(main())


def test_from_iter_async_skips_empty_chunks():
    async def chunks():
        yield b"[1"
        yield b""
        yield b"2,"
        yield b""
        yield b'"a\\'
        yield b""
        yield b'nb"]'
        yield b""

    async def main():
        file_obj = ijson.from_iter(chunks())
        assert await file_obj.read(64) == b"[1"
        assert await file_obj.read(64) == b"2,"
        assert await file_obj.read(64) == b'"a\\'
        assert await file_obj.read(64) == b'nb"]'
        assert await file_obj.read(64) == b""

    asyncio.run(main())


def test_from_iter_async_empty_chunks_parse():
    async def chunks():
        for part in [b"[1", b"", b"2,", b"", b'"a\\', b"", b'nb"]', b""]:
            yield part

    async def main():
        results = []
        async for item in ijson.items_async(ijson.from_iter(chunks()), "item"):
            results.append(item)
        return results

    assert asyncio.run(main()) == [12, "a\nb"]


def test_from_iter_async_source_exception_propagates():
    async def chunks():
        yield b"[1"
        raise ValueError("boom")

    async def main():
        file_obj = ijson.from_iter(chunks())
        assert await file_obj.read(64) == b"[1"
        with pytest.raises(ValueError, match="boom"):
            await file_obj.read(64)

    asyncio.run(main())


def test_from_iter_async_rejects_invalid_chunk_type():
    async def chunks():
        yield b"["
        yield 1

    async def main():
        file_obj = ijson.from_iter(chunks())
        assert await file_obj.read(64) == b"["
        with pytest.raises(TypeError):
            await file_obj.read(64)

    asyncio.run(main())
