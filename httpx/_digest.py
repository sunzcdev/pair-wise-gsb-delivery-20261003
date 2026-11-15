from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import typing
from enum import Enum

from ._exceptions import (
    DigestMismatch,
    IncompleteDigest,
    InvalidDigest,
    StreamClosed,
    StreamConsumed,
)
from ._types import AsyncByteStream, SyncByteStream

__all__ = [
    "AsyncDigestVerificationStream",
    "DigestStatus",
    "DigestVerification",
    "DigestVerificationStream",
    "DigestVerifier",
    "SyncDigestVerificationStream",
    "create_digest_verifier",
]


class DigestStatus(str, Enum):
    """
    The state of a digest verification.

    `UNVERIFIED` means the body did not reach EOF, so no success can be
    claimed.  This includes responses closed early and interrupted transfers.
    `VERIFIED` is only set after the entire stream has been consumed and the
    computed digest matched.
    `FAILED` is set when EOF was reached but the computed digest did not match.
    """

    UNVERIFIED = "unverified"
    VERIFIED = "verified"
    FAILED = "failed"


_ALGORITHM_ALIASES = {
    "md-5": "md5",
    "sha-1": "sha1",
    "sha-224": "sha224",
    "sha-256": "sha256",
    "sha-384": "sha384",
    "sha-512": "sha512",
    "sha-512-224": "sha512_224",
    "sha-512-256": "sha512_256",
    "sha3-224": "sha3_224",
    "sha3-256": "sha3_256",
    "sha3-384": "sha3_384",
    "sha3-512": "sha3_512",
    "blake2b": "blake2b",
    "blake2s": "blake2s",
}


def _normalize_algorithm(algorithm: str) -> str:
    if not isinstance(algorithm, str):
        raise InvalidDigest(
            f"Digest algorithm must be a string, not {type(algorithm).__name__}."
        )
    value = algorithm.strip().lower()
    if not value:
        raise InvalidDigest("Digest algorithm must not be empty.")
    candidates = (
        _ALGORITHM_ALIASES.get(value),
        value,
        value.replace("-", "_"),
        value.replace("-", "").replace("_", ""),
    )
    normalized: str | None = None
    digest_size = 0
    for candidate in candidates:
        if candidate is None:
            continue
        try:
            digest_size = hashlib.new(candidate).digest_size
        except (TypeError, ValueError):
            continue
        normalized = candidate
        break
    else:
        raise InvalidDigest(f"Unsupported digest algorithm {algorithm!r}.")
    assert normalized is not None
    if not digest_size:
        raise InvalidDigest(
            f"Digest algorithm {algorithm!r} has a variable-length output."
        )
    return normalized


def _decode_base64(value: str, digest_size: int) -> bytes:
    padding = "=" * (-len(value) % 4)
    encoded = value + padding
    try:
        decoded = base64.b64decode(encoded, validate=True)
    except (binascii.Error, ValueError):
        decoded = b""
    if len(decoded) == digest_size:
        return decoded
    try:
        decoded = base64.urlsafe_b64decode(encoded)
    except (binascii.Error, ValueError):
        decoded = b""
    if len(decoded) == digest_size:
        return decoded
    raise InvalidDigest("Digest value is not valid base64 for the chosen algorithm.")


def _parse_expected_digest(algorithm: str, expected: str | bytes) -> bytes:
    digest_size = hashlib.new(algorithm).digest_size

    if isinstance(expected, (bytearray, memoryview)):
        expected = bytes(expected)
    if not isinstance(expected, (str, bytes)):
        raise InvalidDigest(
            "Expected digest must be a string or bytes, "
            f"not {type(expected).__name__}."
        )

    if isinstance(expected, bytes):
        if len(expected) == digest_size:
            return expected
        try:
            text = expected.decode("ascii")
        except UnicodeDecodeError:
            raise InvalidDigest(
                "Binary digest values must have exactly the algorithm's digest size."
            ) from None
        expected = text

    value = expected.strip()
    if not value:
        raise InvalidDigest("Expected digest must not be empty.")
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
        value = value[1:-1].strip()

    # Accept a complete field value, such as `sha-256=:base64:` or
    # `sha-256=base64`, as well as a bare digest.  A trailing `=` in a
    # base64 value is padding, not a field separator.
    if "=" in value:
        field_name, field_value = value.split("=", 1)
        field_name = field_name.strip()
        field_value = field_value.strip()
        if field_name and field_value:
            try:
                field_algorithm = _normalize_algorithm(field_name)
            except InvalidDigest:
                field_algorithm = None
            if field_algorithm is not None:
                if field_algorithm != algorithm:
                    raise InvalidDigest(
                        f"Digest field algorithm {field_name!r} does not match "
                        f"the requested algorithm {algorithm!r}."
                    )
                value = field_value

    # RFC 9530 structured-field byte sequences are wrapped in colons.
    if value.startswith(":") or value.endswith(":"):
        if not (value.startswith(":") and value.endswith(":")):
            raise InvalidDigest("Malformed structured-field digest value.")
        value = value[1:-1]
        return _decode_base64(value, digest_size)

    # Allow an explicit `algorithm:value` form.
    if ":" in value:
        field_name, field_value = value.split(":", 1)
        if _normalize_algorithm(field_name.strip()) != algorithm:
            raise InvalidDigest(
                f"Digest prefix {field_name!r} does not match "
                f"the requested algorithm {algorithm!r}."
            )
        value = field_value.strip()

    if len(value) == digest_size * 2:
        try:
            return bytes.fromhex(value)
        except ValueError:
            pass

    return _decode_base64(value, digest_size)


def _algorithm_from_field(value: str | bytes) -> str | None:
    if isinstance(value, bytes):
        try:
            value = value.decode("ascii")
        except UnicodeDecodeError:
            return None
    text = value.strip()
    if len(text) >= 2 and text[0] == text[-1] and text[0] in "\"'":
        text = text[1:-1].strip()

    separator = "=" if "=" in text else ":" if ":" in text else None
    if separator is None:
        return None
    name, field_value = text.split(separator, 1)
    if not name.strip() or not field_value.strip():
        return None
    try:
        return _normalize_algorithm(name.strip())
    except InvalidDigest:
        return None


class DigestVerification:
    """
    Incrementally compute a digest and compare it at EOF.

    A verifier may be updated with any number of byte chunks.  `status` remains
    `UNVERIFIED` until `finish()` has been called after the complete stream was
    consumed.  Calling `finish()` compares against the expected value and sets
    `VERIFIED` or `FAILED`.  `verify()` additionally rejects an unfinished
    calculation, which makes an early close distinguishable from a successful
    verification.
    """

    def __init__(self, algorithm: str, expected: str | bytes) -> None:
        self.algorithm = _normalize_algorithm(algorithm)
        self.expected = _parse_expected_digest(self.algorithm, expected)
        self._hash = hashlib.new(self.algorithm)
        self._status = DigestStatus.UNVERIFIED
        self._bytes_read = 0
        self._finished = False
        self._incomplete = False
        self._interrupted: BaseException | None = None
        self._failure: DigestMismatch | None = None
        self._incomplete_reason: str | None = None

    @property
    def status(self) -> DigestStatus:
        return self._status

    @property
    def is_verified(self) -> bool:
        return self._status is DigestStatus.VERIFIED

    @property
    def verified(self) -> bool:
        return self.is_verified

    @property
    def is_failed(self) -> bool:
        return self._status is DigestStatus.FAILED

    @property
    def is_unverified(self) -> bool:
        return self._status is DigestStatus.UNVERIFIED

    @property
    def is_finished(self) -> bool:
        return self._finished

    @property
    def is_incomplete(self) -> bool:
        return self._incomplete

    @property
    def is_interrupted(self) -> bool:
        return self._interrupted is not None

    @property
    def interrupted(self) -> BaseException | None:
        return self._interrupted

    @property
    def incomplete_reason(self) -> str | None:
        return self._incomplete_reason

    @property
    def failure(self) -> DigestMismatch | None:
        return self._failure

    @property
    def error(self) -> DigestMismatch | BaseException | None:
        return self._failure or self._interrupted

    @property
    def bytes_read(self) -> int:
        return self._bytes_read

    @property
    def digest_size(self) -> int:
        return self._hash.digest_size

    @property
    def digest(self) -> bytes:
        return self._hash.digest()

    @property
    def actual(self) -> bytes:
        return self.digest

    @property
    def hexdigest(self) -> str:
        return self._hash.hexdigest()

    @property
    def actual_hex(self) -> str:
        return self.hexdigest

    @property
    def expected_hex(self) -> str:
        return self.expected.hex()

    def update(self, data: bytes) -> None:
        if self._finished:
            raise RuntimeError("Cannot update a digest after it has been verified.")
        if not isinstance(data, (bytes, bytearray, memoryview)):
            raise TypeError("Digest data must be bytes.")
        chunk = bytes(data)
        self._hash.update(chunk)
        self._bytes_read += len(chunk)

    def finish(self) -> DigestStatus:
        if self._finished:
            if self._failure is not None:
                raise self._failure
            return self._status

        self._finished = True
        actual = self._hash.digest()
        if hmac.compare_digest(actual, self.expected):
            self._status = DigestStatus.VERIFIED
            return self._status

        self._status = DigestStatus.FAILED
        self._failure = DigestMismatch(
            algorithm=self.algorithm,
            expected=self.expected,
            actual=actual,
            bytes_read=self._bytes_read,
        )
        raise self._failure

    def verify(self) -> DigestStatus:
        """
        Check the result, rejecting calls that did not consume the whole body.
        """
        if not self._finished:
            raise IncompleteDigest(
                "Digest verification did not reach EOF: "
                f"read {self._bytes_read} bytes before verification was requested."
            )
        return self.finish()

    def mark_incomplete(self, reason: str = "stream closed before EOF") -> None:
        if not self._finished:
            self._incomplete = True
            self._incomplete_reason = reason

    def mark_interrupted(self, exc: BaseException) -> None:
        if not self._finished:
            self._incomplete = True
            self._interrupted = exc
            self._incomplete_reason = "stream interrupted before EOF"

    def __bool__(self) -> bool:
        return self.is_verified

    def __repr__(self) -> str:
        return (
            f"<{self.__class__.__name__} {self.algorithm!r} "
            f"status={self._status.value!r} bytes_read={self._bytes_read}>"
        )


DigestVerifier = DigestVerification


class DigestVerificationStream(SyncByteStream, AsyncByteStream):
    """
    A byte-stream wrapper that verifies the body as it is delivered.

    The wrapper never buffers the body and does not hide transport errors.
    Success is only recorded after the underlying stream is exhausted.  If the
    wrapper is closed before EOF, `status` stays `UNVERIFIED`.
    """

    def __init__(
        self,
        stream: SyncByteStream | AsyncByteStream,
        algorithm: str | None = None,
        expected: str | bytes | None = None,
        *,
        verifier: DigestVerification | None = None,
        raise_on_mismatch: bool = True,
    ) -> None:
        if verifier is None and isinstance(algorithm, DigestVerification):
            verifier = algorithm
            algorithm = None
        if verifier is None:
            if algorithm is None or expected is None:
                raise InvalidDigest(
                    "Both an algorithm and an expected digest are required."
                )
            verifier = DigestVerification(algorithm, expected)
        self.stream = stream
        self.verifier = verifier
        self.raise_on_mismatch = raise_on_mismatch
        self._started = False
        self._exhausted = False
        self._closed = False

    @property
    def status(self) -> DigestStatus:
        return self.verifier.status

    @property
    def verified(self) -> bool:
        return self.verifier.verified

    @property
    def is_verified(self) -> bool:
        return self.verifier.is_verified

    @property
    def is_failed(self) -> bool:
        return self.verifier.is_failed

    @property
    def is_unverified(self) -> bool:
        return self.verifier.is_unverified

    @property
    def is_interrupted(self) -> bool:
        return self.verifier.is_interrupted

    @property
    def bytes_read(self) -> int:
        return self.verifier.bytes_read

    @property
    def digest(self) -> bytes:
        return self.verifier.digest

    @property
    def hexdigest(self) -> str:
        return self.verifier.hexdigest

    @property
    def error(self) -> DigestMismatch | BaseException | None:
        return self.verifier.failure or self.verifier.interrupted

    def verify(self) -> DigestStatus:
        return self.verifier.verify()

    def __iter__(self) -> typing.Iterator[bytes]:
        if self._closed:
            raise StreamClosed()
        if self._started:
            raise StreamConsumed()
        if not isinstance(self.stream, SyncByteStream) and not hasattr(
            self.stream, "__iter__"
        ):
            raise RuntimeError("Attempted to iterate an async-only digest stream.")
        stream = typing.cast(SyncByteStream, self.stream)
        self._started = True
        try:
            for chunk in stream:
                self.verifier.update(chunk)
                yield chunk
        except GeneratorExit:
            self.verifier.mark_incomplete()
            raise
        except BaseException as exc:
            self.verifier.mark_interrupted(exc)
            raise
        else:
            self._exhausted = True
            try:
                self.verifier.finish()
            except DigestMismatch:
                if not self.raise_on_mismatch:
                    return
                self.close()
                raise

    async def __aiter__(self) -> typing.AsyncIterator[bytes]:
        if self._closed:
            raise StreamClosed()
        if self._started:
            raise StreamConsumed()
        if not isinstance(self.stream, AsyncByteStream) and not hasattr(
            self.stream, "__aiter__"
        ):
            raise RuntimeError("Attempted to iterate a sync-only digest stream.")
        stream = typing.cast(AsyncByteStream, self.stream)
        self._started = True
        try:
            async for chunk in stream:
                self.verifier.update(chunk)
                yield chunk
        except GeneratorExit:
            self.verifier.mark_incomplete()
            raise
        except BaseException as exc:
            self.verifier.mark_interrupted(exc)
            raise
        else:
            self._exhausted = True
            try:
                self.verifier.finish()
            except DigestMismatch:
                if not self.raise_on_mismatch:
                    return
                await self.aclose()
                raise

    def close(self) -> None:
        self._closed = True
        if not self._exhausted:
            self.verifier.mark_incomplete()
        if not isinstance(self.stream, SyncByteStream) and not hasattr(
            self.stream, "close"
        ):
            raise RuntimeError("Attempted to close an async-only digest stream.")
        typing.cast(SyncByteStream, self.stream).close()

    async def aclose(self) -> None:
        self._closed = True
        if not self._exhausted:
            self.verifier.mark_incomplete()
        if not isinstance(self.stream, AsyncByteStream) and not hasattr(
            self.stream, "aclose"
        ):
            raise RuntimeError("Attempted to close a sync-only digest stream.")
        await typing.cast(AsyncByteStream, self.stream).aclose()

    def __repr__(self) -> str:
        return (
            f"<{self.__class__.__name__} {self.verifier.algorithm!r} "
            f"status={self.verifier.status.value!r}>"
        )


SyncDigestVerificationStream = DigestVerificationStream
AsyncDigestVerificationStream = DigestVerificationStream


def _coerce_verifier(
    digest: typing.Any = None,
    algorithm: str | None = None,
    expected: str | bytes | None = None,
    digest_algorithm: str | None = None,
    digest_expected: str | bytes | None = None,
) -> DigestVerification | None:
    if digest_algorithm is not None:
        if algorithm is not None and algorithm != digest_algorithm:
            raise InvalidDigest("Conflicting digest algorithm values were provided.")
        algorithm = digest_algorithm
    if digest_expected is not None:
        if expected is not None and expected != digest_expected:
            raise InvalidDigest("Conflicting expected digest values were provided.")
        expected = digest_expected

    if isinstance(digest, DigestVerification):
        if algorithm is not None or expected is not None:
            raise InvalidDigest(
                "Do not combine a DigestVerification object with algorithm/expected."
            )
        return digest
    if isinstance(digest, (tuple, list)):
        if len(digest) != 2:
            raise InvalidDigest(
                "Digest tuples must contain exactly (algorithm, expected)."
            )
        if algorithm is not None or expected is not None:
            raise InvalidDigest(
                "Do not combine a digest tuple with algorithm/expected."
            )
        algorithm, expected = digest
    elif isinstance(digest, dict):
        if algorithm is not None or expected is not None:
            raise InvalidDigest(
                "Do not combine a digest mapping with algorithm/expected."
            )
        try:
            algorithm = digest["algorithm"]
            expected = digest["expected"]
        except KeyError as exc:
            raise InvalidDigest(
                "Digest mappings must contain 'algorithm' and 'expected'."
            ) from exc
    elif isinstance(digest, (str, bytes)):
        if algorithm is None:
            algorithm = _algorithm_from_field(digest)
            if algorithm is None:
                raise InvalidDigest(
                    "An algorithm is required when the expected digest is passed "
                    "directly, unless the digest value includes an algorithm prefix."
                )
        if expected is not None:
            raise InvalidDigest(
                "Do not pass the expected digest both positionally and by keyword."
            )
        expected = digest
    elif digest is not None:
        raise InvalidDigest(
            "Digest verification must be specified as an (algorithm, expected) "
            "pair, a mapping, a DigestVerification instance, or a digest value "
            "alongside an algorithm."
        )

    if algorithm is None and expected is None:
        return None
    if algorithm is None or expected is None:
        raise InvalidDigest("Both an algorithm and an expected digest are required.")
    return DigestVerification(algorithm, expected)


def create_digest_verifier(
    digest: typing.Any = None,
    algorithm: str | None = None,
    expected: str | bytes | None = None,
    digest_algorithm: str | None = None,
    digest_expected: str | bytes | None = None,
) -> DigestVerification | None:
    """
    Normalize the supported digest option spellings into a verifier.
    """
    return _coerce_verifier(
        digest=digest,
        algorithm=algorithm,
        expected=expected,
        digest_algorithm=digest_algorithm,
        digest_expected=digest_expected,
    )
