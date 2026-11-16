from io import BytesIO
from logging import getLogger
from typing import TYPE_CHECKING, Optional

from attrs import define, field, fields_dict
from requests import Response
from urllib3.response import (  # type: ignore  # import location false positive
    HTTPHeaderDict,
    HTTPResponse,
    is_fp_closed,
)

from . import RichMixin

logger = getLogger(__name__)


if TYPE_CHECKING:
    from . import CachedResponse


class BytesReader:
    """A minimal, read-only file-like wrapper around an immutable ``bytes`` object.

    This is used instead of ``io.BytesIO`` so that each copy of a cached response can have its own
    independent read position without duplicating the (potentially large) response body in memory.
    Only the subset of file-like behavior used by :py:class:`.CachedHTTPResponse` and urllib3 is
    implemented.
    """

    def __init__(self, body: bytes = b''):
        self._body = body
        self._pos = 0
        self._closed = False

    @property
    def closed(self) -> bool:
        return self._closed

    def isclosed(self) -> bool:
        return self._closed

    def close(self):
        self._closed = True

    def readable(self) -> bool:
        return True

    def seekable(self) -> bool:
        return True

    def tell(self) -> int:
        return self._pos

    def seek(self, pos: int, whence: int = 0) -> int:
        if self._closed:
            raise ValueError('I/O operation on closed file')
        if whence == 0:
            new_pos = pos
        elif whence == 1:
            new_pos = self._pos + pos
        else:
            new_pos = len(self._body) + pos
        self._pos = min(max(new_pos, 0), len(self._body))
        return self._pos

    def read(self, amt: Optional[int] = None) -> bytes:
        """Read ``amt`` bytes, or all remaining bytes if ``amt`` is ``None`` or negative"""
        if self._closed:
            raise ValueError('I/O operation on closed file')
        if amt is None or amt < 0:
            amt = len(self._body) - self._pos
        data = self._body[self._pos : self._pos + amt]
        self._pos += len(data)
        return data

    def read1(self, amt: Optional[int] = None) -> bytes:
        return self.read(amt)

    def __len__(self) -> int:
        return len(self._body)


@define(auto_attribs=False, repr=False, slots=False)
class CachedHTTPResponse(RichMixin, HTTPResponse):
    """A wrapper class that emulates :py:class:`~urllib3.response.HTTPResponse`.

    This enables consistent behavior for streaming requests and generator usage in the following
    cases:
    * On an original response, after reading its content to write to the cache
    * On a cached response
    """

    decode_content: bool = field(default=None)
    headers: HTTPHeaderDict = field(factory=HTTPHeaderDict)
    reason: str = field(default=None)
    request_url: str = field(default=None)
    status: int = field(default=0)
    version: int = field(default=0)

    def __init__(self, body: Optional[bytes] = None, **kwargs):
        """First initialize via HTTPResponse.__init__, then via __attrs_init__"""
        kwargs = {k: v for k, v in kwargs.items() if v is not None}
        body = body or b''
        super().__init__(body=BytesReader(body), preload_content=False, **kwargs)
        self._body = body
        self._fp_bytes_read = 0
        self.length_remaining = len(body)
        self.__attrs_init__(**kwargs)  # type: ignore # False positive in mypy 0.920+?

    @classmethod
    def from_response(cls, response: Response) -> 'CachedHTTPResponse':
        """Create a CachedHTTPResponse based on an original response, and restore the response to
        its previous state.
        """
        if TYPE_CHECKING:
            assert isinstance(response.raw, HTTPResponse)
        # Get basic attributes
        kwargs = {k: getattr(response.raw, k, None) for k in fields_dict(cls).keys()}
        # Init kwarg for HTTPResponse._request_url has no leading '_'
        kwargs['request_url'] = response.raw._request_url  # type: ignore[attr-defined]
        # Copy headers, so the cached response can't be affected by changes to the original response
        kwargs['headers'] = HTTPHeaderDict(getattr(response.raw, 'headers', None) or {})
        kwargs['body'] = _copy_body(response)
        return cls(**kwargs)  # type: ignore[arg-type]  # False positive in mypy 0.920+?

    @classmethod
    def from_cached_response(cls, response: 'CachedResponse'):
        """Create a CachedHTTPResponse based on a cached response"""
        obj = cls(
            headers=HTTPHeaderDict(response.headers),
            reason=response.reason,
            status=response.status_code,
            request_url=response.request.url,
        )
        obj.reset(response._content)
        return obj

    @property
    def _request_url(self) -> str:
        """For compatibility with urllib3"""
        return self.request_url

    @_request_url.setter
    def _request_url(self, value: str):
        self.request_url = value

    def release_conn(self):
        """No-op for compatibility"""

    def read(self, amt=None, decode_content=None, **kwargs):
        """Simplified reader for cached content that emulates
        :py:meth:`urllib3.response.HTTPResponse.read()`, but does not need to read from a socket
        or decode content.
        """
        if 'Content-Encoding' in self.headers and decode_content is False:
            logger.warning('read(decode_content=False) is not supported for cached responses')
        if is_fp_closed(self._fp):
            return b''

        data = self._fp.read(amt)
        if data:
            self._fp_bytes_read += len(data)
            if self.length_remaining is not None:
                self.length_remaining -= len(data)
        # "close" the file to inform consumers to stop reading from it
        else:
            self._fp.close()
        return data

    def reset(self, body: Optional[bytes] = None):
        """Reset raw response file pointer, and optionally update content"""
        body = body or self._body or b''
        self._body = body
        self._fp_bytes_read = 0
        # Use a new reader over the same immutable body, so the read position is independent of
        # any other copy of this response (and no body data is copied)
        self._fp = BytesReader(body)
        self.length_remaining = len(body)

    def stream(self, amt=None, **kwargs):
        """Simplified generator over cached content that emulates
        :py:meth:`urllib3.response.HTTPResponse.stream()`
        """
        while not self._fp.closed:
            yield self.read(amt=amt, **kwargs)


def _copy_body(response: Response) -> Optional[bytes]:
    """Read and copy raw response data, and then restore response object to its previous state.
    This is necessary so streaming responses behave consistently with or without the cache.
    """
    if TYPE_CHECKING:
        assert isinstance(response.raw, HTTPResponse)
    # File pointer is missing or closed; nothing to do
    if not getattr(response.raw, '_fp', None) or is_fp_closed(response.raw._fp):  # type: ignore[attr-defined]
        return None
    # Body has already been read & decoded by requests
    elif getattr(response.raw, '_has_decoded_content', False):
        body = response.content
    # Body has not yet been read
    else:
        body = response.raw.read(decode_content=False)
        _reset_fp(response.raw, body)
        _ = response.content  # This property reads, decodes, and stores response content

    # After reading, reset file pointer once more so client can still read it as a stream
    _reset_fp(response.raw, body)
    return body


def _reset_fp(raw: HTTPResponse, body: Optional[bytes] = None):
    """Set content and reset raw response file pointer.

    This is used for the raw response of an original (non-cached) response, so it keeps using a
    standard file-like object. Cached responses use :py:class:`.BytesReader` instead, so their
    copies can share the same body without sharing a read position.
    """
    body = body or b''
    raw._body = body  # type: ignore[attr-defined]
    raw._fp_bytes_read = 0  # type: ignore[attr-defined]
    raw._fp = BytesIO(body)  # type: ignore[attr-defined]
    raw.length_remaining = len(body)
