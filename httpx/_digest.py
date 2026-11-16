"""
Optional digest verification for downloaded response content.

When a server declares a content digest, for example through a
`Content-Digest` header, the download can be verified incrementally by
wrapping the raw response stream with `SyncDigestStream` or
`AsyncDigestStream`.

The digest accumulates as the raw body streams, and is only confirmed
once the stream reaches EOF. Bytes that have already been delivered to
the caller before verification completes cannot be retracted, so this
is not an atomic download mechanism.
"""

from __future__ import annotations

import hashlib
import hmac
import typing

from ._exceptions import (
    DigestFormatError,
    DigestInterruptedError,
    DigestMismatchError,
    DigestUnverifiedError,
)
from ._types import AsyncByteStream, SyncByteStream

__all__ = ["AsyncDigestStream", "DigestVerifier", "SyncDigestStream"]


class DigestVerifier:
    """
    Incrementally verifies the digest of a downloaded byte stream.

    The caller explicitly specifies the `algorithm` (any name accepted by
    `hashlib.new()`, such as `"sha256"`) and the `expected` digest, either
    as a hexadecimal string or as raw bytes.

    Bytes are accumulated with `update()` as they stream in, and the digest
    is only confirmed by `finalize()` once the stream reaches EOF. Closing
    the stream early leaves the verifier in an unverified state: a partially
    read prefix of the content must never be treated as verified.

    A single verifier instance is shared by any read style, so reading the
    response to completion and iterating over it chunk-by-chunk both feed
    the same digest computation.
    """

    def __init__(self, algorithm: str, expected: str | bytes) -> None:
        try:
            self._hasher = hashlib.new(algorithm)
        except (TypeError, ValueError) as exc:
            raise DigestFormatError(
                f"Unsupported digest algorithm {algorithm!r}."
            ) from exc

        self._algorithm = algorithm
        self._expected = self._normalize_expected(expected)
        self._finalized = False
        self._verified = False
        self._closed_early = False
        self._interrupted_by: BaseException | None = None

    def _normalize_expected(self, expected: str | bytes) -> bytes:
        if isinstance(expected, str):
            try:
                digest = bytes.fromhex(expected)
            except ValueError:
                raise DigestFormatError(
                    f"Expected digest {expected!r} is not valid hexadecimal."
                ) from None
        elif isinstance(expected, (bytes, bytearray)):
            digest = bytes(expected)
        else:
            raise DigestFormatError(
                "The expected digest must be a hexadecimal string or bytes, "
                f"got {type(expected).__name__!r}."
            )

        if len(digest) != self._hasher.digest_size:
            raise DigestFormatError(
                f"Expected digest for {self._algorithm!r} must be "
                f"{self._hasher.digest_size} bytes "
                f"({self._hasher.digest_size * 2} hex characters), "
                f"got {len(digest)} bytes."
            )
        return digest

    @property
    def algorithm(self) -> str:
        """
        The digest algorithm in use, as passed to `hashlib.new()`.
        """
        return self._algorithm

    @property
    def expected_digest(self) -> str:
        """
        The expected digest, as a lowercase hexadecimal string.
        """
        return self._expected.hex()

    @property
    def verified(self) -> bool:
        """
        `True` only if the stream reached EOF and the computed digest
        matched the expected digest. An early-closed or interrupted
        stream is never verified, no matter how many bytes matched so far.
        """
        return self._verified

    def update(self, chunk: bytes) -> None:
        """
        Accumulate a chunk of the raw byte stream into the digest.
        """
        if self._finalized:
            raise RuntimeError(
                "Cannot update a digest verifier after the stream has reached EOF."
            )
        self._hasher.update(chunk)

    def finalize(self) -> None:
        """
        Confirm the digest now that the stream has reached EOF.

        Raises `DigestMismatchError` if the computed digest does not match
        the expected digest.
        """
        if self._finalized:
            return
        self._finalized = True
        actual = self._hasher.digest()
        if not hmac.compare_digest(actual, self._expected):
            raise DigestMismatchError(
                f"Digest mismatch using {self._algorithm}: expected "
                f"{self._expected.hex()}, but downloaded content digests "
                f"to {actual.hex()}."
            )
        self._verified = True

    def interrupt(self, exc: BaseException) -> None:
        """
        Record that the transfer was interrupted by an underlying error.
        """
        if not self._finalized:
            self._interrupted_by = exc

    def abandon(self) -> None:
        """
        Record that the stream was closed before reaching EOF.
        """
        if not self._finalized:
            self._closed_early = True

    def verify(self) -> None:
        """
        Raise unless the stream was fully consumed and the digest matched.

        Raises `DigestInterruptedError` if the transfer was interrupted by
        an underlying error, or `DigestUnverifiedError` if the stream was
        closed before EOF. Returns `None` if the digest was verified.
        """
        if self._verified:
            return
        if self._interrupted_by is not None:
            raise DigestInterruptedError(
                "The transfer was interrupted before EOF, so the digest "
                "could not be verified."
            ) from self._interrupted_by
        raise DigestUnverifiedError(
            "The stream was closed before EOF, so the digest is unverified. "
            "The bytes read so far are only a prefix of the content and "
            "must not be treated as verified."
        )


class SyncDigestStream(SyncByteStream):
    """
    A sync byte stream wrapper that verifies a digest while streaming.

    Wraps the *raw* response stream, so the digest is computed over the
    transferred bytes, before any HTTP content decoding such as `gzip`:

    ```python
    with client.stream("GET", url) as response:
        response.stream = httpx.SyncDigestStream(
            response.stream, algorithm="sha256", expected=expected_hex
        )
        content = response.read()  # Raises `DigestMismatchError` at EOF on mismatch.
        assert response.stream.verified
    ```

    Chunks are delivered to the caller as they arrive. Verification is only
    confirmed once the stream reaches EOF, so any bytes consumed before a
    `DigestMismatchError` is raised cannot be retracted.
    """

    def __init__(
        self, stream: SyncByteStream, algorithm: str, expected: str | bytes
    ) -> None:
        self._stream = stream
        self.verifier = DigestVerifier(algorithm, expected)

    @property
    def verified(self) -> bool:
        """
        `True` only if the stream reached EOF and the digest matched.
        """
        return self.verifier.verified

    def verify(self) -> None:
        """
        Raise unless the stream was fully consumed and the digest matched.
        """
        self.verifier.verify()

    def __iter__(self) -> typing.Iterator[bytes]:
        iterator = iter(self._stream)
        try:
            for chunk in iterator:
                self.verifier.update(chunk)
                yield chunk
        except Exception as exc:
            # The transfer was interrupted. The original error propagates
            # unchanged, and the verifier records it so that `.verify()`
            # reports an interrupted transfer rather than a clean close.
            self.verifier.interrupt(exc)
            raise
        finally:
            # Ensure the underlying iterator is released if the consumer
            # does not iterate this stream to completion.
            close = getattr(iterator, "close", None)
            if close is not None:
                close()
        self.verifier.finalize()

    def close(self) -> None:
        try:
            self._stream.close()
        finally:
            # Closing before EOF marks the digest as unverified. A partially
            # read prefix of the content must not be treated as verified.
            self.verifier.abandon()


class AsyncDigestStream(AsyncByteStream):
    """
    An async byte stream wrapper that verifies a digest while streaming.

    Wraps the *raw* response stream, so the digest is computed over the
    transferred bytes, before any HTTP content decoding such as `gzip`:

    ```python
    async with client.stream("GET", url) as response:
        response.stream = httpx.AsyncDigestStream(
            response.stream, algorithm="sha256", expected=expected_hex
        )
        content = await response.aread()  # Raises `DigestMismatchError` at EOF on mismatch.
        assert response.stream.verified
    ```

    Chunks are delivered to the caller as they arrive. Verification is only
    confirmed once the stream reaches EOF, so any bytes consumed before a
    `DigestMismatchError` is raised cannot be retracted.
    """

    def __init__(
        self, stream: AsyncByteStream, algorithm: str, expected: str | bytes
    ) -> None:
        self._stream = stream
        self.verifier = DigestVerifier(algorithm, expected)

    @property
    def verified(self) -> bool:
        """
        `True` only if the stream reached EOF and the digest matched.
        """
        return self.verifier.verified

    def verify(self) -> None:
        """
        Raise unless the stream was fully consumed and the digest matched.
        """
        self.verifier.verify()

    async def __aiter__(self) -> typing.AsyncIterator[bytes]:
        iterator = self._stream.__aiter__()
        try:
            async for chunk in iterator:
                self.verifier.update(chunk)
                yield chunk
        except Exception as exc:
            # The transfer was interrupted. The original error propagates
            # unchanged, and the verifier records it so that `.verify()`
            # reports an interrupted transfer rather than a clean close.
            self.verifier.interrupt(exc)
            raise
        finally:
            # Ensure the underlying iterator is released if the consumer
            # does not iterate this stream to completion.
            aclose = getattr(iterator, "aclose", None)
            if aclose is not None:
                await aclose()
        self.verifier.finalize()

    async def aclose(self) -> None:
        try:
            await self._stream.aclose()
        finally:
            # Closing before EOF marks the digest as unverified. A partially
            # read prefix of the content must not be treated as verified.
            self.verifier.abandon()
