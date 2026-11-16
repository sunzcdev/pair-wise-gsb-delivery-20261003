import hashlib
import typing

import pytest

import httpx

BODY = b"The quick brown fox jumps over the lazy dog" * 16
CHUNK_SIZE = 64


def body_digest(algorithm: str = "sha256") -> str:
    return hashlib.new(algorithm, BODY).hexdigest()


def chunk_body(body: bytes = BODY, chunk_size: int = CHUNK_SIZE) -> list[bytes]:
    return [body[i : i + chunk_size] for i in range(0, len(body), chunk_size)]


class SyncStream(httpx.SyncByteStream):
    def __init__(self, chunks: list[bytes]) -> None:
        self._chunks = chunks
        self.closed = False

    def __iter__(self) -> typing.Iterator[bytes]:
        yield from self._chunks

    def close(self) -> None:
        self.closed = True


class FailingSyncStream(httpx.SyncByteStream):
    def __init__(self, chunks: list[bytes]) -> None:
        self._chunks = chunks

    def __iter__(self) -> typing.Iterator[bytes]:
        yield from self._chunks
        raise httpx.ReadError("Connection dropped mid-transfer.")


class AsyncStream(httpx.AsyncByteStream):
    def __init__(self, chunks: list[bytes]) -> None:
        self._chunks = chunks
        self.closed = False

    async def __aiter__(self) -> typing.AsyncIterator[bytes]:
        for chunk in self._chunks:
            yield chunk

    async def aclose(self) -> None:
        self.closed = True


class FailingAsyncStream(httpx.AsyncByteStream):
    def __init__(self, chunks: list[bytes]) -> None:
        self._chunks = chunks

    async def __aiter__(self) -> typing.AsyncIterator[bytes]:
        for chunk in self._chunks:
            yield chunk
        raise httpx.ReadError("Connection dropped mid-transfer.")


# Format validation...


def test_unknown_algorithm_is_a_format_error():
    with pytest.raises(httpx.DigestFormatError):
        httpx.DigestVerifier(algorithm="not-a-real-algorithm", expected="00")


def test_malformed_hex_digest_is_a_format_error():
    with pytest.raises(httpx.DigestFormatError):
        httpx.DigestVerifier(algorithm="sha256", expected="not-hex!")


def test_wrong_length_digest_is_a_format_error():
    with pytest.raises(httpx.DigestFormatError):
        httpx.DigestVerifier(algorithm="sha256", expected="ab" * 16)


def test_wrong_type_digest_is_a_format_error():
    with pytest.raises(httpx.DigestFormatError):
        httpx.DigestVerifier(algorithm="sha256", expected=123)  # type: ignore[arg-type]


def test_format_error_is_also_a_value_error():
    with pytest.raises(ValueError):
        httpx.DigestVerifier(algorithm="sha256", expected="zz")


def test_expected_digest_may_be_bytes_or_uppercase_hex():
    expected = hashlib.sha256(BODY).digest()
    verifier = httpx.DigestVerifier(algorithm="sha256", expected=expected)
    assert verifier.expected_digest == expected.hex()
    verifier = httpx.DigestVerifier(algorithm="sha256", expected=expected.hex().upper())
    assert verifier.expected_digest == expected.hex()


# Synchronous streaming...


def test_sync_stream_verified_at_eof():
    stream = httpx.SyncDigestStream(
        SyncStream(chunk_body()), algorithm="sha256", expected=body_digest()
    )
    assert not stream.verified
    chunks = list(stream)
    assert b"".join(chunks) == BODY
    assert stream.verified
    stream.verify()  # Does not raise.
    stream.close()


def test_sync_stream_mismatch_reported_only_at_eof():
    stream = httpx.SyncDigestStream(
        SyncStream(chunk_body()), algorithm="sha256", expected=hashlib.sha256(b"not the body").hexdigest()
    )
    received = b""
    with pytest.raises(httpx.DigestMismatchError):
        for chunk in stream:
            # Every chunk is still delivered as it arrives. The mismatch
            # can only be reported once the stream reaches EOF.
            received += chunk
            assert not stream.verified
    # Bytes already delivered before the failure cannot be retracted.
    assert received == BODY
    assert not stream.verified
    with pytest.raises(httpx.DigestUnverifiedError):
        stream.verify()


def test_sync_stream_early_close_is_unverified():
    stream = httpx.SyncDigestStream(
        SyncStream(chunk_body()), algorithm="sha256", expected=body_digest()
    )
    iterator = iter(stream)
    next(iterator)
    # Even though the prefix read so far matches the digest, closing
    # before EOF must not count as a successful verification.
    stream.close()
    assert not stream.verified
    with pytest.raises(httpx.DigestUnverifiedError):
        stream.verify()


def test_sync_stream_interrupted_transfer_reported_separately():
    stream = httpx.SyncDigestStream(
        FailingSyncStream(chunk_body()), algorithm="sha256", expected=body_digest()
    )
    with pytest.raises(httpx.ReadError):
        list(stream)
    assert not stream.verified
    with pytest.raises(httpx.DigestInterruptedError) as exc_info:
        stream.verify()
    assert isinstance(exc_info.value.__cause__, httpx.ReadError)
    # An interrupted transfer is distinct from a digest mismatch.
    assert not isinstance(exc_info.value, httpx.DigestMismatchError)


def test_sync_response_read_and_iteration_share_one_computation():
    expected = body_digest()
    stream = httpx.SyncDigestStream(
        SyncStream(chunk_body()), algorithm="sha256", expected=expected
    )
    response = httpx.Response(200, stream=stream)
    assert response.read() == BODY
    assert stream.verified
    stream.verify()

    # Iterating the raw response feeds the same digest computation.
    stream = httpx.SyncDigestStream(
        SyncStream(chunk_body()), algorithm="sha256", expected=expected
    )
    response = httpx.Response(200, stream=stream)
    assert b"".join(response.iter_raw()) == BODY
    assert stream.verified


def test_sync_response_read_mismatch():
    stream = httpx.SyncDigestStream(
        SyncStream(chunk_body()), algorithm="sha256", expected=hashlib.sha256(b"not the body").hexdigest()
    )
    response = httpx.Response(200, stream=stream)
    with pytest.raises(httpx.DigestMismatchError):
        response.read()


def test_sync_verifier_rejects_update_after_eof():
    stream = httpx.SyncDigestStream(
        SyncStream(chunk_body()), algorithm="sha256", expected=body_digest()
    )
    list(stream)
    with pytest.raises(RuntimeError):
        stream.verifier.update(b"more data")


# Asynchronous streaming...


@pytest.mark.anyio
async def test_async_stream_verified_at_eof():
    stream = httpx.AsyncDigestStream(
        AsyncStream(chunk_body()), algorithm="sha256", expected=body_digest()
    )
    assert not stream.verified
    chunks = [chunk async for chunk in stream]
    assert b"".join(chunks) == BODY
    assert stream.verified
    stream.verify()  # Does not raise.
    await stream.aclose()


@pytest.mark.anyio
async def test_async_stream_mismatch_reported_only_at_eof():
    stream = httpx.AsyncDigestStream(
        AsyncStream(chunk_body()), algorithm="sha256", expected=hashlib.sha256(b"not the body").hexdigest()
    )
    received = b""
    with pytest.raises(httpx.DigestMismatchError):
        async for chunk in stream:
            received += chunk
            assert not stream.verified
    assert received == BODY
    assert not stream.verified


@pytest.mark.anyio
async def test_async_stream_early_close_is_unverified():
    stream = httpx.AsyncDigestStream(
        AsyncStream(chunk_body()), algorithm="sha256", expected=body_digest()
    )
    iterator = stream.__aiter__()
    await iterator.__anext__()
    # Even though the prefix read so far matches the digest, closing
    # before EOF must not count as a successful verification.
    await iterator.aclose()
    await stream.aclose()
    assert not stream.verified
    with pytest.raises(httpx.DigestUnverifiedError):
        stream.verify()


@pytest.mark.anyio
async def test_async_stream_interrupted_transfer_reported_separately():
    stream = httpx.AsyncDigestStream(
        FailingAsyncStream(chunk_body()), algorithm="sha256", expected=body_digest()
    )
    with pytest.raises(httpx.ReadError):
        [chunk async for chunk in stream]
    assert not stream.verified
    with pytest.raises(httpx.DigestInterruptedError) as exc_info:
        stream.verify()
    assert isinstance(exc_info.value.__cause__, httpx.ReadError)


@pytest.mark.anyio
async def test_async_response_aread_shares_one_computation():
    stream = httpx.AsyncDigestStream(
        AsyncStream(chunk_body()), algorithm="sha256", expected=body_digest()
    )
    response = httpx.Response(200, stream=stream)
    assert await response.aread() == BODY
    assert stream.verified
    stream.verify()


@pytest.mark.anyio
async def test_async_response_aread_mismatch():
    stream = httpx.AsyncDigestStream(
        AsyncStream(chunk_body()), algorithm="sha256", expected=hashlib.sha256(b"not the body").hexdigest()
    )
    response = httpx.Response(200, stream=stream)
    with pytest.raises(httpx.DigestMismatchError):
        await response.aread()
