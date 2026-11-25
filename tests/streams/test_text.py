from __future__ import annotations

import platform
import sys
from contextlib import AbstractContextManager, nullcontext

import pytest

from anyio import EndOfStream, create_memory_object_stream
from anyio.abc import ObjectStream, ObjectStreamConnectable
from anyio.streams.stapled import StapledObjectStream
from anyio.streams.text import (
    TextConnectable,
    TextReceiveStream,
    TextSendStream,
    TextStream,
)


async def test_receive() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    text_stream = TextReceiveStream(receive_stream)
    await send_stream.send(b"\xc3\xa5\xc3\xa4\xc3")  # ends with half of the "ö" letter
    assert await text_stream.receive() == "åä"

    # Send the missing byte for "ö"
    await send_stream.send(b"\xb6")
    assert await text_stream.receive() == "ö"

    send_stream.close()
    receive_stream.close()


@pytest.mark.parametrize(
    "errors, expected",
    [
        pytest.param("strict", pytest.raises(UnicodeDecodeError), id="strict"),
        pytest.param("replace", nullcontext(), id="replace"),
        pytest.param("ignore", pytest.raises(EndOfStream), id="ignore"),
    ],
)
async def test_incomplete_character_at_eof(
    errors: str, expected: AbstractContextManager[object]
) -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    text_stream = TextReceiveStream(receive_stream, errors=errors)
    await send_stream.send(b"\xc3")  # first half of a two-byte character
    send_stream.close()
    with expected:
        assert await text_stream.receive() == "\ufffd"

    with pytest.raises(EndOfStream):
        await text_stream.receive()

    receive_stream.close()


async def test_switch_encoding_after_decode_error_preserves_read_ahead_bytes() -> None:
    payload = b"linha\xc3\xa9\n\xff\xfeA\x00"
    expected = "linhaé\nA"

    for mask in range(1 << (len(payload) - 1)):
        cuts = [0]
        cuts.extend(i for i in range(1, len(payload)) if mask & (1 << (i - 1)))
        cuts.append(len(payload))
        chunks = [
            payload[cuts[index] : cuts[index + 1]] for index in range(len(cuts) - 1)
        ]

        send_stream, receive_stream = create_memory_object_stream[bytes](
            len(chunks) + 1
        )
        text_stream = TextReceiveStream(receive_stream)
        for chunk in chunks:
            await send_stream.send(chunk)

        await send_stream.aclose()

        decoded: list[str] = []
        switched = False
        while True:
            try:
                decoded.append(await text_stream.receive())
            except UnicodeDecodeError:
                assert not switched
                await text_stream.switch_encoding("utf-16")
                switched = True
            except EndOfStream:
                break

        assert switched
        assert "".join(decoded) == expected
        await receive_stream.aclose()


async def test_switch_encoding_at_byte_boundary_preserves_read_ahead_bytes() -> None:
    payload = b"linha\xc3\xa9\n\xff\xfeA\x00"
    expected = "linhaé\nA"

    for mask in range(1 << (len(payload) - 1)):
        cuts = [0]
        cuts.extend(i for i in range(1, len(payload)) if mask & (1 << (i - 1)))
        cuts.append(len(payload))
        chunks = [
            payload[cuts[index] : cuts[index + 1]] for index in range(len(cuts) - 1)
        ]

        send_stream, receive_stream = create_memory_object_stream[bytes](
            len(chunks) + 1
        )
        text_stream = TextReceiveStream(receive_stream)
        await text_stream.switch_encoding("utf-16", boundary=b"\n")

        for chunk in chunks:
            await send_stream.send(chunk)

        await send_stream.aclose()

        decoded: list[str] = []
        while True:
            try:
                decoded.append(await text_stream.receive())
            except EndOfStream:
                break

        assert "".join(decoded) == expected
        await receive_stream.aclose()


async def test_failed_encoding_switch_leaves_original_decoder_usable() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    text_stream = TextReceiveStream(receive_stream)

    await send_stream.send(b"linha\xc3")
    assert await text_stream.receive() == "linha"

    with pytest.raises(UnicodeDecodeError, match="unexpected end of data"):
        await text_stream.switch_encoding("utf-16")

    await send_stream.send(b"\xa9\n\xff")
    with pytest.raises(UnicodeDecodeError, match="invalid start byte"):
        await text_stream.receive()

    # The valid old-encoding prefix and the read-ahead new-encoding bytes survive.
    text_stream.switch_encoding("utf-16")
    assert await text_stream.receive() == "é\n"

    await send_stream.send(b"\xfeA\x00")
    assert await text_stream.receive() == "A"

    await send_stream.aclose()
    with pytest.raises(EndOfStream):
        await text_stream.receive()

    await receive_stream.aclose()


async def test_send() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    text_stream = TextSendStream(send_stream)
    await text_stream.send("åäö")
    assert await receive_stream.receive() == b"\xc3\xa5\xc3\xa4\xc3\xb6"

    send_stream.close()
    receive_stream.close()


@pytest.mark.xfail(
    platform.python_implementation() == "PyPy" and sys.pypy_version_info < (7, 3, 2),  # type: ignore[attr-defined]
    reason="PyPy has a bug in its incremental UTF-8 decoder (#3274)",
)
async def test_receive_encoding_error() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    text_stream = TextReceiveStream(receive_stream, errors="replace")
    await send_stream.send(b"\xe5\xe4\xf6")  # "åäö" in latin-1
    assert await text_stream.receive() == "���"

    send_stream.close()
    receive_stream.close()


async def test_send_encoding_error() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    text_stream = TextSendStream(send_stream, encoding="iso-8859-1", errors="replace")
    await text_stream.send("€")
    assert await receive_stream.receive() == b"?"

    send_stream.close()
    receive_stream.close()


@pytest.mark.parametrize("encoding", ["utf-16", "utf-32", "utf-8-sig"])
async def test_send_bom_emitted_only_once(encoding: str) -> None:
    """
    Encodings that emit a byte order mark must not repeat it on every send, otherwise
    the peer receives spurious U+FEFF characters (or duplicate BOM bytes).
    """
    send_stream, receive_stream = create_memory_object_stream[bytes](8)
    with send_stream, receive_stream:
        text_send = TextSendStream(send_stream, encoding=encoding)
        text_receive = TextReceiveStream(receive_stream, encoding=encoding)
        await text_send.send("hello")
        await text_send.send("world")
        assert await text_receive.receive() == "hello"
        assert await text_receive.receive() == "world"


async def test_bidirectional_stream() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    stapled_stream = StapledObjectStream(send_stream, receive_stream)
    text_stream = TextStream(stapled_stream)

    await text_stream.send("åäö")
    assert await receive_stream.receive() == b"\xc3\xa5\xc3\xa4\xc3\xb6"

    await send_stream.send(b"\xc3\xa6\xc3\xb8")
    assert await text_stream.receive() == "æø"
    assert text_stream.extra_attributes == {}

    send_stream.close()
    receive_stream.close()


async def test_text_connectable() -> None:
    send_stream, receive_stream = create_memory_object_stream[bytes](1)
    memory_stream = StapledObjectStream(send_stream, receive_stream)

    class MemoryConnectable(ObjectStreamConnectable[bytes]):
        async def connect(self) -> ObjectStream[bytes]:
            return memory_stream

    connectable = TextConnectable(MemoryConnectable())
    async with await connectable.connect() as stream:
        assert isinstance(stream, TextStream)
        await stream.send("hello")
        assert await stream.receive() == "hello"
