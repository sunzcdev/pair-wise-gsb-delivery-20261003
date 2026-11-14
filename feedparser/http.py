# Copyright 2010-2025 Kurt McKee <contactme@kurtmckee.org>
# Copyright 2002-2008 Mark Pilgrim
# All rights reserved.
#
# This file is a part of feedparser.
#
# Redistribution and use in source and binary forms, with or without
# modification, are permitted provided that the following conditions are met:
#
# * Redistributions of source code must retain the above copyright notice,
#   this list of conditions and the following disclaimer.
# * Redistributions in binary form must reproduce the above copyright notice,
#   this list of conditions and the following disclaimer in the documentation
#   and/or other materials provided with the distribution.
#
# THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS 'AS IS'
# AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
# IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
# ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
# LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
# CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
# SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
# INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
# CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
# ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
# POSSIBILITY OF SUCH DAMAGE.

from __future__ import annotations

import http.client
import typing
import zlib

import requests
import urllib3

from .datetimes import _parse_date
from .exceptions import BodyTooLargeError, ContentDecodingError, FeedparserError

# HTTP "Accept" header to send to servers when downloading feeds.
ACCEPT_HEADER: str = (
    "application/atom+xml"
    ",application/rdf+xml"
    ",application/rss+xml"
    ",application/x-netcdf"
    ",application/xml"
    ";q=0.9,text/xml"
    ";q=0.2,*/*"
    ";q=0.1"
)


# Chunk size used when reading a response body incrementally.
_CHUNK_SIZE = 64 * 1024


class BodyBudget:
    """Record received and decoded body byte counts against an optional limit.

    ``received`` counts the bytes read from the response
    (after transfer decoding, before content decoding);
    ``decoded`` counts the bytes after content decoding (e.g. decompression).
    The limit applies to the decoded byte count.
    """

    def __init__(self, limit: int | None = None) -> None:
        self.limit = limit
        self.received = 0
        self.decoded = 0

    def add_received(self, count: int) -> None:
        self.received += count

    def add_decoded(self, count: int) -> None:
        self.decoded += count
        if self.limit is not None and self.decoded > self.limit:
            raise BodyTooLargeError(
                f"the decoded response body exceeds the {self.limit} byte limit"
            )


class _DeflateDecoder:
    """Decode "deflate" data, whether zlib-wrapped or raw."""

    def __init__(self) -> None:
        self._obj = zlib.decompressobj()
        self._buffer = b""
        self._first_chunk = True

    def decompress(self, data: bytes) -> bytes:
        if not self._first_chunk:
            return self._obj.decompress(data)
        self._buffer += data
        try:
            decoded = self._obj.decompress(data)
        except zlib.error:
            # The data is not zlib-wrapped; retry as a raw deflate stream.
            self._obj = zlib.decompressobj(-zlib.MAX_WBITS)
            self._first_chunk = False
            buffer, self._buffer = self._buffer, b""
            return self._obj.decompress(buffer)
        if decoded:
            self._first_chunk = False
            self._buffer = b""
        return decoded

    def flush(self) -> bytes:
        return self._obj.flush()

    @property
    def eof(self) -> bool:
        return self._obj.eof


def _get_decoder(
    content_encoding: str,
) -> zlib.Decompress | _DeflateDecoder | None:
    encoding = content_encoding.lower().strip()
    if encoding in ("gzip", "x-gzip"):
        return zlib.decompressobj(16 + zlib.MAX_WBITS)
    if encoding == "deflate":
        return _DeflateDecoder()
    return None


def _read_body(response: requests.Response, budget: BodyBudget) -> bytes:
    """Read a response body in chunks, enforcing the decoded size limit.

    The actual bytes read from the response are counted and decoded,
    so the limit is enforced even for chunked responses
    or responses without a Content-Length header.
    """

    decoder = _get_decoder(response.headers.get("Content-Encoding", ""))
    chunks: list[bytes] = []
    while True:
        try:
            chunk = response.raw.read(_CHUNK_SIZE, decode_content=False)
        except (
            http.client.HTTPException,
            urllib3.exceptions.HTTPError,
            OSError,
        ) as error:
            raise requests.ConnectionError(error) from error
        if not chunk:
            break
        budget.add_received(len(chunk))
        if decoder is not None:
            try:
                chunk = decoder.decompress(chunk)
            except zlib.error as error:
                raise ContentDecodingError(
                    "the response body could not be decompressed"
                ) from error
        budget.add_decoded(len(chunk))
        chunks.append(chunk)

    if decoder is not None and budget.received:
        if not decoder.eof:
            raise ContentDecodingError("the compressed response body is truncated")
        try:
            chunk = decoder.flush()
        except zlib.error as error:
            raise ContentDecodingError(
                "the response body could not be decompressed"
            ) from error
        budget.add_decoded(len(chunk))
        chunks.append(chunk)

    return b"".join(chunks)


def get(
    url: str,
    result: dict[str, typing.Any],
    session: requests.Session | None = None,
    size_limit: int | None = None,
) -> bytes:
    """Fetch a URL and record response metadata in *result*.

    :param session:
        An optional requests session held by the caller.
        When provided, only the response is closed;
        the session remains open and reusable.
        Otherwise, a temporary session is created and closed.
    :param size_limit:
        An optional limit, in bytes, on the decoded response body size.
        The body is read incrementally and the limit is enforced
        on the actual decoded bytes, regardless of the response headers.
        If the limit is exceeded, the compressed data is corrupt,
        or the request is interrupted,
        ``bozo`` and ``bozo_exception`` are set in *result*
        and no partial body is returned.
    """

    if session is None:
        session = requests.Session()
        own_session = True
    else:
        own_session = False
    try:
        try:
            response = session.get(
                url,
                headers={"Accept": ACCEPT_HEADER},
                timeout=10,
                stream=True,
            )
        except requests.RequestException as exception:
            result["bozo"] = True
            result["bozo_exception"] = exception
            return b""

        try:
            # Lowercase the HTTP header keys for comparisons per RFC 2616.
            result["headers"] = {k.lower(): v for k, v in response.headers.items()}

            # save HTTP headers
            if "etag" in result["headers"]:
                result["etag"] = result["headers"]["etag"]
            if "last-modified" in result["headers"]:
                modified = result["headers"]["last-modified"]
                if modified:
                    result["modified"] = modified
                    result["modified_parsed"] = _parse_date(modified)
            result["href"] = response.url
            result["status"] = response.status_code

            if size_limit is None:
                data = response.content
            else:
                data = _read_body(response, BodyBudget(size_limit))
        except (requests.RequestException, FeedparserError) as exception:
            result["bozo"] = True
            result["bozo_exception"] = exception
            return b""
        finally:
            response.close()
        return data
    finally:
        if own_session:
            session.close()
