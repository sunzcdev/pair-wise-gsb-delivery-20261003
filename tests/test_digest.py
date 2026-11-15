from __future__ import annotations

import base64
import gzip
import hashlib
import typing

import pytest

import httpx


def body_chunks() -> typing.Iterator[bytes]:
    yield b"Hello, "
    yield b"world!"


async def async_body_chunks() -> typing.AsyncIterator[bytes]:
    yield b"Hello, "
    yield b"world!"


class TrackedStream(httpx.SyncByteStream):
    def __init__(self, chunks: typing.Iterable[bytes]) -> None:
        self.chunks = list(chunks)
        self.closed = False

    def __iter__(self) -> typing.Iterator[bytes]:
        yield from self.chunks

    def close(self) -> None:
        self.closed = True


class AsyncTrackedStream(httpx.AsyncByteStream):
    def __init__(self, chunks: typing.Iterable[bytes]) -> None:
        self.chunks = list(chunks)
        self.index = 0
        self.closed = False

    def __aiter__(self) -> AsyncTrackedStream:
        return self

    async def __anext__(self) -> bytes:
        try:
            chunk = self.chunks[self.index]
        except IndexError:
            raise StopAsyncIteration from None
        self.index += 1
        return chunk

    async def aclose(self) -> None:
        self.closed = True


def test_digest_verifier_accumulates_chunks():
    expected = hashlib.sha256(b"Hello, world!").hexdigest()
    verifier = httpx.DigestVerifier("sha-256", expected)

    verifier.update(b"Hello, ")
    verifier.update(b"world!")

    assert verifier.status is httpx.DigestStatus.UNVERIFIED
    assert not verifier.verified
    assert verifier.bytes_read == 13

    assert verifier.finish() is httpx.DigestStatus.VERIFIED
    assert verifier.verified
    assert verifier.is_verified
    assert verifier.verify() is httpx.DigestStatus.VERIFIED


@pytest.mark.parametrize(
    "expected",
    [
        hashlib.sha256(b"Hello, world!").digest(),
        hashlib.sha256(b"Hello, world!").hexdigest(),
        hashlib.sha256(b"Hello, world!").hexdigest().upper(),
        base64.b64encode(hashlib.sha256(b"Hello, world!").digest()).decode(),
        "sha-256="
        + base64.b64encode(hashlib.sha256(b"Hello, world!").digest()).decode(),
        "sha-256=:"
        + base64.b64encode(hashlib.sha256(b"Hello, world!").digest()).decode()
        + ":",
    ],
)
def test_digest_expected_formats(expected):
    verifier = httpx.DigestVerifier("sha256", expected)
    verifier.update(b"Hello, world!")

    assert verifier.finish() is httpx.DigestStatus.VERIFIED


@pytest.mark.parametrize(
    "algorithm,expected",
    [
        ("unknown-hash", "00"),
        ("sha256", ""),
        ("sha256", "not-a-digest"),
        ("sha256", "00"),
        ("sha256", "sha-512=" + "00" * 64),
        ("sha256", "sha256=:not-base64!:"),
    ],
)
def test_invalid_digest_is_reported_separately(algorithm, expected):
    with pytest.raises(httpx.InvalidDigest) as exc_info:
        httpx.DigestVerifier(algorithm, expected)

    assert isinstance(exc_info.value, ValueError)
    assert not isinstance(exc_info.value, httpx.DigestMismatch)


def test_response_verifies_raw_body_at_eof():
    body = b"Hello, world!"
    response = httpx.Response(
        200,
        stream=body_chunks(),
        digest=("sha256", hashlib.sha256(body).hexdigest()),
    )

    assert response.digest_status is httpx.DigestStatus.UNVERIFIED
    assert response.read() == body
    assert response.digest_status is httpx.DigestStatus.VERIFIED
    assert response.is_digest_verified
    assert response.digest_verified


def test_response_read_accepts_digest_options():
    body = b"Hello, world!"
    response = httpx.Response(200, stream=body_chunks())

    assert (
        response.read(
            digest_algorithm="sha-256",
            digest_expected=hashlib.sha256(body).hexdigest(),
        )
        == body
    )
    assert response.is_digest_verified

    # Reading the cached content again does not restart the calculation.
    verifier = response.digest_verifier
    assert verifier is not None
    assert response.read() == body
    assert verifier.bytes_read == len(body)
    assert verifier.status is httpx.DigestStatus.VERIFIED


def test_digest_value_can_include_the_algorithm_field():
    body = b"Hello, world!"
    response = httpx.Response(200, stream=body_chunks())

    assert response.read(digest="sha-256=" + hashlib.sha256(body).hexdigest()) == body
    assert response.is_digest_verified


def test_response_iteration_uses_configured_verifier():
    body = b"Hello, world!"
    response = httpx.Response(200, stream=body_chunks())
    verifier = response.verify_digest("sha256", hashlib.sha256(body).hexdigest())

    chunks = list(response.iter_bytes(chunk_size=4))

    assert b"".join(chunks) == body
    assert chunks == [b"Hell", b"o, w", b"orld", b"!"]
    assert verifier.status is httpx.DigestStatus.VERIFIED
    assert verifier.bytes_read == len(body)


def test_iteration_configures_digest_before_the_first_chunk():
    body = b"Hello, world!"
    response = httpx.Response(200, stream=body_chunks())
    iterator = response.iter_raw(
        digest=("sha256", hashlib.sha256(body).hexdigest())
    )

    assert response.digest_status is httpx.DigestStatus.UNVERIFIED
    response.close()
    assert response.digest_status is httpx.DigestStatus.UNVERIFIED
    iterator.close()


def test_digest_covers_raw_bytes_before_content_decoding():
    body = b"Hello, world!" * 10
    compressed = gzip.compress(body)
    response = httpx.Response(
        200,
        headers={"content-encoding": "gzip"},
        stream=httpx.ByteStream(compressed),
        digest=("sha256", hashlib.sha256(compressed).hexdigest()),
    )

    assert response.read() == body
    verifier = response.digest_verifier
    assert verifier is not None
    assert verifier.bytes_read == len(compressed)
    assert verifier.status is httpx.DigestStatus.VERIFIED


def test_client_get_can_verify_the_download():
    body = b"Hello, world!"

    def app(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=body)

    transport = httpx.MockTransport(app)
    with httpx.Client(transport=transport) as client:
        response = client.get(
            "http://testserver/",
            digest=("sha256", hashlib.sha256(body).hexdigest()),
        )

    assert response.content == body
    assert response.digest_status is httpx.DigestStatus.VERIFIED


def test_client_stream_can_verify_the_download():
    body = b"Hello, world!"

    def app(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=body)

    transport = httpx.MockTransport(app)
    with httpx.Client(transport=transport) as client:
        with client.stream(
            "GET",
            "http://testserver/",
            digest=("sha256", hashlib.sha256(body).hexdigest()),
        ) as response:
            assert response.read() == body

    assert response.digest_status is httpx.DigestStatus.VERIFIED


def test_digest_mismatch_is_raised_only_after_all_chunks():
    response = httpx.Response(
        200,
        stream=body_chunks(),
        digest=("sha256", "00" * 32),
    )
    delivered = []

    with pytest.raises(httpx.DigestMismatch) as exc_info:
        for chunk in response.iter_raw():
            delivered.append(chunk)

    assert delivered == [b"Hello, ", b"world!"]
    assert exc_info.value.algorithm == "sha256"
    assert exc_info.value.bytes_read == 13
    assert exc_info.value.actual == hashlib.sha256(b"Hello, world!").digest()
    assert response.digest_status is httpx.DigestStatus.FAILED
    assert not response.is_digest_verified


def test_early_close_is_unverified_and_not_a_success():
    stream = TrackedStream([b"Hello, ", b"world!"])
    response = httpx.Response(
        200,
        stream=stream,
        digest=("sha256", hashlib.sha256(b"Hello, world!").hexdigest()),
    )
    iterator = response.iter_raw()

    assert next(iterator) == b"Hello, "
    response.close()

    assert stream.closed
    assert response.digest_status is httpx.DigestStatus.UNVERIFIED
    assert not response.is_digest_verified
    verifier = response.digest_verifier
    assert verifier is not None
    assert verifier.is_incomplete
    assert verifier.bytes_read == len(b"Hello, ")
    with pytest.raises(httpx.IncompleteDigest):
        verifier.verify()


def test_close_before_reading_is_unverified():
    stream = TrackedStream([b"Hello, world!"])
    response = httpx.Response(
        200,
        stream=stream,
        digest=("sha256", hashlib.sha256(b"Hello, world!").hexdigest()),
    )

    response.close()

    assert response.digest_status is httpx.DigestStatus.UNVERIFIED
    assert response.digest_verifier is not None
    assert response.digest_verifier.bytes_read == 0
    assert response.digest_verifier.is_incomplete


def test_transport_interruption_is_not_a_digest_mismatch():
    def interrupted_body() -> typing.Iterator[bytes]:
        yield b"Hello, "
        raise httpx.ReadError("connection dropped")

    response = httpx.Response(
        200,
        stream=interrupted_body(),
        digest=("sha256", hashlib.sha256(b"Hello, world!").hexdigest()),
    )

    with pytest.raises(httpx.ReadError) as exc_info:
        response.read()

    assert str(exc_info.value) == "connection dropped"
    assert response.digest_status is httpx.DigestStatus.UNVERIFIED
    verifier = response.digest_verifier
    assert verifier is not None
    assert verifier.is_interrupted
    assert verifier.interrupted is exc_info.value
    assert verifier.failure is None


def test_digest_stream_can_report_mismatch_without_raising():
    stream = httpx.DigestVerificationStream(
        body_chunks(),
        "sha256",
        "00" * 32,
        raise_on_mismatch=False,
    )

    assert list(stream) == [b"Hello, ", b"world!"]
    assert stream.status is httpx.DigestStatus.FAILED
    assert stream.error is not None


def test_empty_body_can_verify():
    expected = hashlib.sha256(b"").hexdigest()
    response = httpx.Response(
        200,
        stream=httpx.ByteStream(b""),
        digest=("sha256", expected),
    )

    assert response.read() == b""
    assert response.is_digest_verified


@pytest.mark.anyio
async def test_async_response_verifies_at_eof():
    body = b"Hello, world!"
    response = httpx.Response(
        200,
        stream=async_body_chunks(),
        digest=("sha256", hashlib.sha256(body).hexdigest()),
    )

    assert await response.aread() == body
    assert response.digest_status is httpx.DigestStatus.VERIFIED


@pytest.mark.anyio
async def test_async_client_can_verify_the_download():
    body = b"Hello, world!"

    async def app(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=body)

    transport = httpx.MockTransport(app)
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.get(
            "http://testserver/",
            digest=("sha256", hashlib.sha256(body).hexdigest()),
        )

    assert response.content == body
    assert response.digest_status is httpx.DigestStatus.VERIFIED


@pytest.mark.anyio
async def test_async_digest_mismatch_is_raised_only_after_all_chunks():
    response = httpx.Response(
        200,
        stream=async_body_chunks(),
        digest=("sha256", "00" * 32),
    )
    delivered = []

    with pytest.raises(httpx.DigestMismatch):
        async for chunk in response.aiter_raw():
            delivered.append(chunk)

    assert delivered == [b"Hello, ", b"world!"]
    assert response.digest_status is httpx.DigestStatus.FAILED


@pytest.mark.anyio
async def test_async_early_close_is_unverified():
    stream = AsyncTrackedStream([b"Hello, ", b"world!"])
    response = httpx.Response(
        200,
        stream=stream,
        digest=("sha256", hashlib.sha256(b"Hello, world!").hexdigest()),
    )
    await response.aclose()

    assert stream.closed
    assert response.digest_status is httpx.DigestStatus.UNVERIFIED
    assert response.digest_verifier is not None
    assert response.digest_verifier.is_incomplete


@pytest.mark.anyio
async def test_async_digest_stream_partial_read_is_unverified():
    stream = AsyncTrackedStream([b"Hello, ", b"world!"])
    digest_stream = httpx.DigestVerificationStream(
        stream,
        "sha256",
        hashlib.sha256(b"Hello, world!").hexdigest(),
    )
    iterator = digest_stream.__aiter__()

    assert await iterator.__anext__() == b"Hello, "
    await iterator.aclose()

    assert digest_stream.status is httpx.DigestStatus.UNVERIFIED
    assert digest_stream.is_unverified
    assert digest_stream.verifier.is_incomplete
    assert digest_stream.bytes_read == len(b"Hello, ")
    await digest_stream.aclose()


@pytest.mark.anyio
async def test_async_transport_interruption_is_not_a_digest_mismatch():
    async def interrupted_body() -> typing.AsyncIterator[bytes]:
        yield b"Hello, "
        raise httpx.ReadError("connection dropped")

    response = httpx.Response(
        200,
        stream=interrupted_body(),
        digest=("sha256", hashlib.sha256(b"Hello, world!").hexdigest()),
    )

    with pytest.raises(httpx.ReadError):
        await response.aread()

    assert response.digest_status is httpx.DigestStatus.UNVERIFIED
    assert response.digest_verifier is not None
    assert response.digest_verifier.is_interrupted
    assert response.digest_verifier.failure is None
