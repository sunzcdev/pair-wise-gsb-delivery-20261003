from copy import copy
from http.cookiejar import CookieJar
from logging import getLogger
from typing import Union
from urllib.parse import urlsplit

from attrs import define, field, fields_dict
from requests import PreparedRequest
from requests.cookies import RequestsCookieJar, cookiejar_from_dict
from requests.structures import CaseInsensitiveDict

from ..cache_keys import encode
from . import RichMixin

logger = getLogger(__name__)

CookieJarType = Union[RequestsCookieJar, CookieJar, dict, None]


def copy_cookie_jar(jar: CookieJarType) -> RequestsCookieJar:
    """Copy cookies into a new :py:class:`~requests.cookies.RequestsCookieJar`, so that adding,
    removing, or modifying cookies on one object won't affect the other.
    """
    if isinstance(jar, RequestsCookieJar):
        return jar.copy()
    elif isinstance(jar, CookieJar):
        new_jar = copy(jar)
        new_jar.clear()
        for cookie in jar:
            new_jar.set_cookie(copy(cookie))
        return new_jar
    else:
        return cookiejar_from_dict(dict(jar or {}))


def copy_headers(headers) -> CaseInsensitiveDict:
    """Copy headers into a new :py:class:`~requests.structures.CaseInsensitiveDict`"""
    return CaseInsensitiveDict(headers or {})


@define(repr=False)
class CachedRequest(RichMixin):
    """A serializable dataclass that emulates :py:class:`requests.PreparedResponse`"""

    body: bytes = field(default=None, converter=encode)
    cookies: RequestsCookieJar = field(factory=RequestsCookieJar)
    headers: CaseInsensitiveDict = field(factory=CaseInsensitiveDict)
    method: str = field(default=None)
    url: str = field(default=None)

    @classmethod
    def from_request(cls, original_request: PreparedRequest) -> 'CachedRequest':
        """Create a CachedRequest based on an original request object. Mutable request attributes
        are copied, so temporary headers or cookies can't be written back to the original request.
        """
        kwargs = {k: getattr(original_request, k, None) for k in fields_dict(cls).keys()}
        kwargs['cookies'] = copy_cookie_jar(getattr(original_request, '_cookies', None))
        kwargs['headers'] = copy_headers(getattr(original_request, 'headers', None))
        return cls(**kwargs)  # type: ignore  # False positive in mypy 0.920+?

    @property
    def path_url(self):
        p = urlsplit(self.url)
        url = p.path or '/'
        url += f'?{p.query}' if p.query else ''
        return url

    def copy(self) -> 'CachedRequest':
        """Return a copy of the CachedRequest, including copies of its mutable attributes"""
        return self.__class__(
            body=self.body,
            cookies=copy_cookie_jar(self.cookies),
            headers=copy_headers(self.headers),
            method=self.method,
            url=self.url,
        )

    def prepare(self) -> PreparedRequest:
        """Convert the CachedRequest back into a PreparedRequest. Mutable attributes are copied, so
        changes to the prepared request won't be written back to this object (or the cache).
        """
        prepared_request = PreparedRequest()
        prepared_request.prepare(
            cookies=copy_cookie_jar(self.cookies),
            data=self.body,
            headers=copy_headers(self.headers),
            method=self.method,
            url=self.url,
        )
        return prepared_request

    @property
    def _cookies(self):
        """For compatibility with PreparedRequest, which has an attribute named '_cookies', and a
        keyword argument named 'cookies'.
        """
        return self.cookies

    def __str__(self):
        return f'{self.method} {self.url}'
