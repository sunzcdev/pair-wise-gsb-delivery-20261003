"""Lazy ZIP over HTTP"""

from __future__ import annotations

__all__ = [
    "HTTPRangeRequestUnsupported",
    "LazyWheelContentChangedError",
    "dist_from_wheel_url",
]

import logging
from bisect import bisect_left, bisect_right
from collections.abc import Generator
from contextlib import contextmanager
from tempfile import NamedTemporaryFile
from typing import Any
from zipfile import BadZipFile, ZipFile

from pip._vendor.packaging.utils import NormalizedName
from pip._vendor.requests.models import CONTENT_CHUNK_SIZE, Response

from pip._internal.metadata import BaseDistribution, MemoryWheel, get_wheel_distribution
from pip._internal.network.session import PipSession
from pip._internal.network.utils import HEADERS, raise_for_status, response_chunks

logger = logging.getLogger(__name__)


class HTTPRangeRequestUnsupported(Exception):
    pass


class LazyWheelContentChangedError(Exception):
    """Raised when range responses cannot be proven to come from one file.

    The wheel on the server changed while its bytes were being fetched with
    multiple range requests.  Validators (ETag / Last-Modified) show that at
    least one response describes a different representation, so the partially
    downloaded bytes cannot be safely stitched together.
    """


def dist_from_wheel_url(
    name: NormalizedName,
    url: str,
    session: PipSession,
    chunk_size: int = CONTENT_CHUNK_SIZE,
) -> BaseDistribution:
    """Return a distribution object from the given wheel URL.

    This uses HTTP range requests to only fetch the portion of the wheel
    containing metadata, just enough for the object to be constructed.
    If such requests are not supported, HTTPRangeRequestUnsupported
    is raised.

    A wheel is read through several independent range requests.  When the
    server provides validators, every response is checked against the
    representation described by the initial response so that a wheel swapped
    on the mirror mid-read can never be assembled from two different files.
    A mismatch discards the partial read and falls back to downloading the
    whole wheel in a single request; the returned distribution then reflects
    whichever complete representation the server serves for that fallback.
    """
    try:
        with LazyZipOverHTTP(url, session, chunk_size=chunk_size) as zf:
            # For read-only ZIP files, ZipFile only needs methods read,
            # seek, seekable and tell, not the whole IO protocol.
            wheel = MemoryWheel(zf.name, zf)  # type: ignore
            # After context manager exit, wheel.name
            # is an invalid file by intention.
            return get_wheel_distribution(wheel, name)
    except LazyWheelContentChangedError as exc:
        logger.debug("Lazy wheel fetch restarted from full download: %s", exc)
        return _dist_from_full_wheel_download(name, url, session)


def _dist_from_full_wheel_download(
    name: NormalizedName, url: str, session: PipSession
) -> BaseDistribution:
    """Download the whole wheel in one request and build the distribution.

    This is the explicit fallback used when range responses cannot be proven
    to describe a single, unchanged representation.  A single response carries
    one self-consistent set of bytes, so no stitching across versions is
    possible; whatever representation the server serves now is the product
    whose metadata is returned.
    """
    response = session.get(url, headers=HEADERS, stream=True)
    raise_for_status(response)
    with NamedTemporaryFile() as f:
        for chunk in response_chunks(response, CONTENT_CHUNK_SIZE):
            f.write(chunk)
        f.flush()
        wheel = MemoryWheel(f.name, f)
        return get_wheel_distribution(wheel, name)


class LazyZipOverHTTP:
    """File-like object mapped to a ZIP file over HTTP.

    This uses HTTP range requests to lazily fetch the file's content,
    which is supposed to be fed to ZipFile.  If such requests are not
    supported by the server, raise HTTPRangeRequestUnsupported
    during initialization.
    """

    def __init__(
        self, url: str, session: PipSession, chunk_size: int = CONTENT_CHUNK_SIZE
    ) -> None:
        head = session.head(url, headers=HEADERS)
        raise_for_status(head)
        assert head.status_code == 200
        self._session, self._url, self._chunk_size = session, url, chunk_size
        self._length = int(head.headers["Content-Length"])
        # Validators identifying the representation described by this HEAD
        # response.  Every subsequent range response must describe the same
        # one, otherwise the locally assembled file would mix bytes from two
        # different wheels.  An empty tuple means the server offers no
        # validator, in which case no extra check is possible.
        self._validators = _response_validators(head)
        self._file = NamedTemporaryFile()
        self.truncate(self._length)
        self._left: list[int] = []
        self._right: list[int] = []
        if "bytes" not in head.headers.get("Accept-Ranges", "none"):
            raise HTTPRangeRequestUnsupported("range request is not supported")
        self._check_zip()

    @property
    def mode(self) -> str:
        """Opening mode, which is always rb."""
        return "rb"

    @property
    def name(self) -> str:
        """Path to the underlying file."""
        return self._file.name

    def seekable(self) -> bool:
        """Return whether random access is supported, which is True."""
        return True

    def close(self) -> None:
        """Close the file."""
        self._file.close()

    @property
    def closed(self) -> bool:
        """Whether the file is closed."""
        return self._file.closed

    def read(self, size: int = -1) -> bytes:
        """Read up to size bytes from the object and return them.

        As a convenience, if size is unspecified or -1,
        all bytes until EOF are returned.  Fewer than
        size bytes may be returned if EOF is reached.
        """
        download_size = max(size, self._chunk_size)
        start, length = self.tell(), self._length
        stop = length if size < 0 else min(start + download_size, length)
        start = max(0, stop - download_size)
        self._download(start, stop - 1)
        return self._file.read(size)

    def readable(self) -> bool:
        """Return whether the file is readable, which is True."""
        return True

    def seek(self, offset: int, whence: int = 0) -> int:
        """Change stream position and return the new absolute position.

        Seek to offset relative position indicated by whence:
        * 0: Start of stream (the default).  pos should be >= 0;
        * 1: Current position - pos may be negative;
        * 2: End of stream - pos usually negative.
        """
        return self._file.seek(offset, whence)

    def tell(self) -> int:
        """Return the current position."""
        return self._file.tell()

    def truncate(self, size: int | None = None) -> int:
        """Resize the stream to the given size in bytes.

        If size is unspecified resize to the current position.
        The current stream position isn't changed.

        Return the new file size.
        """
        return self._file.truncate(size)

    def writable(self) -> bool:
        """Return False."""
        return False

    def __enter__(self) -> LazyZipOverHTTP:
        self._file.__enter__()
        return self

    def __exit__(self, *exc: Any) -> None:
        self._file.__exit__(*exc)

    @contextmanager
    def _stay(self) -> Generator[None, None, None]:
        """Return a context manager keeping the position.

        At the end of the block, seek back to original position.
        """
        pos = self.tell()
        try:
            yield
        finally:
            self.seek(pos)

    def _check_zip(self) -> None:
        """Check and download until the file is a valid ZIP."""
        end = self._length - 1
        for start in reversed(range(0, end, self._chunk_size)):
            self._download(start, end)
            with self._stay():
                try:
                    # For read-only ZIP files, ZipFile only needs
                    # methods read, seek, seekable and tell.
                    ZipFile(self)
                except BadZipFile:
                    pass
                else:
                    break

    def _stream_response(
        self, start: int, end: int, base_headers: dict[str, str] = HEADERS
    ) -> Response:
        """Return HTTP response to a range request from start to end."""
        headers = base_headers.copy()
        headers["Range"] = f"bytes={start}-{end}"
        # TODO: Get range requests to be correctly cached
        headers["Cache-Control"] = "no-cache"
        return self._session.get(self._url, headers=headers, stream=True)

    def _merge(
        self, start: int, end: int, left: int, right: int
    ) -> Generator[tuple[int, int], None, None]:
        """Return a generator of intervals to be fetched.

        Args:
            start (int): Start of needed interval
            end (int): End of needed interval
            left (int): Index of first overlapping downloaded data
            right (int): Index after last overlapping downloaded data
        """
        lslice, rslice = self._left[left:right], self._right[left:right]
        i = start = min([start] + lslice[:1])
        end = max([end] + rslice[-1:])
        for j, k in zip(lslice, rslice):
            if j > i:
                yield i, j - 1
            i = k + 1
        if i <= end:
            yield i, end
        self._left[left:right], self._right[left:right] = [start], [end]

    def _download(self, start: int, end: int) -> None:
        """Download bytes from start to end inclusively.

        Before any received byte is written, the response validators are
        compared against the ones captured from the initial response.  On a
        mismatch the partially assembled file is left untouched by this
        response and LazyWheelContentChangedError is raised so the caller can
        discard the whole partial read and fall back to a full download.
        """
        with self._stay():
            left = bisect_left(self._right, start)
            right = bisect_right(self._left, end)
            for start, end in self._merge(start, end, left, right):
                response = self._stream_response(start, end)
                response.raise_for_status()
                self._raise_if_representation_changed(response)
                self.seek(start)
                for chunk in response_chunks(response, self._chunk_size):
                    self._file.write(chunk)

    def _raise_if_representation_changed(self, response: Response) -> None:
        """Ensure a range response still describes the original wheel.

        Only the validators advertised by the initial response are compared:
        an ETag, if present, must match byte-for-byte, and likewise for
        Last-Modified.  A server that gave no validator cannot be checked here
        and keeps the previous best-effort behavior.
        """
        if not self._validators:
            return
        current = dict(_response_validators(response))
        for header, expected in self._validators:
            actual = current.get(header)
            if actual is None or actual != expected:
                raise LazyWheelContentChangedError(
                    f"{self._url} changed while being read with range "
                    f"requests: {header} was {expected!r}, now {actual!r}"
                )


def _response_validators(response: Response) -> tuple[tuple[str, str], ...]:
    """Return the (header, value) validators carried by a response.

    ETag is the strong, opaque validator and is preferred; Last-Modified is
    kept as a weaker fallback for servers that only emit it.  Header names
    are returned in their canonical request/response spelling.
    """
    validators: list[tuple[str, str]] = []
    etag = response.headers.get("ETag")
    if etag:
        validators.append(("ETag", etag))
    last_modified = response.headers.get("Last-Modified")
    if last_modified:
        validators.append(("Last-Modified", last_modified))
    return tuple(validators)
