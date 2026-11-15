"""Main classes to add caching features to :py:class:`requests.Session`"""

from contextlib import contextmanager, nullcontext
from logging import getLogger
from threading import RLock, Thread
from typing import TYPE_CHECKING, Iterable, Mapping, Optional, Union

from requests import PreparedRequest
from requests import Session as OriginalSession
from requests.hooks import dispatch_hook
from requests.structures import CaseInsensitiveDict

from ._utils import get_valid_kwargs, patch_form_boundary
from .backends import BackendSpecifier, StrOrPath, init_backend
from .models import (
    AnyResponse,
    CachedHTTPResponse,
    CachedRequest,
    CachedResponse,
    OriginalResponse,
)
from .policy import (
    DEFAULT_CACHE_NAME,
    DEFAULT_IGNORED_PARAMS,
    DEFAULT_METHODS,
    DEFAULT_STATUS_CODES,
    CacheActions,
    CacheDirectives,
    CacheSettings,
    ExpirationPatterns,
    ExpirationTime,
    FilterCallback,
    KeyCallback,
    can_freshen_from_head,
    set_request_headers,
    utcnow,
)
from .serializers import SerializerType

__all__ = ['CachedSession', 'CacheMixin']
if TYPE_CHECKING:
    MIXIN_BASE = OriginalSession
else:
    MIXIN_BASE = object

logger = getLogger(__name__)


class CacheMixin(MIXIN_BASE):
    """Mixin class that extends :py:class:`requests.Session` with caching features.
    See :py:class:`.CachedSession` for usage details.
    """

    def __init__(
        self,
        cache_name: StrOrPath = DEFAULT_CACHE_NAME,
        backend: Optional[BackendSpecifier] = None,
        serializer: Optional[SerializerType] = None,
        expire_after: ExpirationTime = -1,
        urls_expire_after: Optional[ExpirationPatterns] = None,
        cache_control: bool = False,
        content_root_key: Optional[str] = None,
        allowable_codes: Iterable[int] = DEFAULT_STATUS_CODES,
        allowable_methods: Iterable[str] = DEFAULT_METHODS,
        always_revalidate: bool = False,
        ignored_parameters: Iterable[str] = DEFAULT_IGNORED_PARAMS,
        match_headers: Union[Iterable[str], bool] = False,
        filter_fn: Optional[FilterCallback] = None,
        head_coordination: Union[bool, str] = True,
        key_fn: Optional[KeyCallback] = None,
        read_only: bool = False,
        stale_if_error: Union[bool, int] = False,
        autoclose: bool = True,
        **kwargs,
    ):
        for alias in ('head_policy', 'head_cache_policy', 'head_coordination_policy'):
            if alias in kwargs:
                head_coordination = kwargs.pop(alias)

        self.cache = init_backend(cache_name, backend, serializer=serializer, **kwargs)
        self.settings = CacheSettings.from_kwargs(
            expire_after=expire_after,
            urls_expire_after=urls_expire_after,
            cache_control=cache_control,
            content_root_key=content_root_key,
            allowable_codes=allowable_codes,
            allowable_methods=allowable_methods,
            always_revalidate=always_revalidate,
            ignored_parameters=ignored_parameters,
            match_headers=match_headers,
            filter_fn=filter_fn,
            head_coordination=head_coordination,
            key_fn=key_fn,
            read_only=read_only,
            stale_if_error=stale_if_error,
            autoclose=autoclose,
            **kwargs,
        )
        self._lock = RLock()

        # If the mixin superclass is a custom Session, pass along any valid kwargs
        super().__init__(**get_valid_kwargs(super().__init__, kwargs))

    @classmethod
    def wrap(cls, original_session: OriginalSession, **kwargs) -> 'CacheMixin':
        """Add caching to an existing :py:class:`~requests.Session` object, while retaining all
        original session settings.

        Args:
            original_session: Session object to wrap
            kwargs: Keyword arguments for :py:class:`.CachedSession`
        """
        session = cls(**kwargs)
        session.adapters = original_session.adapters
        session.auth = original_session.auth
        session.cert = original_session.cert
        session.cookies = original_session.cookies
        session.headers = original_session.headers
        session.hooks = original_session.hooks
        session.max_redirects = original_session.max_redirects
        session.params = original_session.params
        session.proxies = original_session.proxies
        session.stream = original_session.stream
        session.trust_env = original_session.trust_env
        session.verify = original_session.verify
        return session

    @property
    def settings(self) -> CacheSettings:
        """Settings that affect cache behavior"""
        return self.cache._settings

    @settings.setter
    def settings(self, value: CacheSettings):
        self.cache._settings = value

    # For backwards-compatibility
    @property
    def expire_after(self) -> ExpirationTime:
        return self.settings.expire_after

    @expire_after.setter
    def expire_after(self, value: ExpirationTime):
        self.settings.expire_after = value

    # Wrapper methods to add return type hints
    def get(self, url: str, params=None, **kwargs) -> AnyResponse:  # type: ignore
        kwargs.setdefault('allow_redirects', True)
        return self.request('GET', url, params=params, **kwargs)

    def options(self, url: str, **kwargs) -> AnyResponse:  # type: ignore
        kwargs.setdefault('allow_redirects', True)
        return self.request('OPTIONS', url, **kwargs)

    def head(self, url: str, **kwargs) -> AnyResponse:  # type: ignore
        kwargs.setdefault('allow_redirects', False)
        return self.request('HEAD', url, **kwargs)

    def post(self, url: str, data=None, **kwargs) -> AnyResponse:  # type: ignore
        return self.request('POST', url, data=data, **kwargs)

    def put(self, url: str, data=None, **kwargs) -> AnyResponse:  # type: ignore
        return self.request('PUT', url, data=data, **kwargs)

    def patch(self, url: str, data=None, **kwargs) -> AnyResponse:  # type: ignore
        return self.request('PATCH', url, data=data, **kwargs)

    def delete(self, url: str, **kwargs) -> AnyResponse:  # type: ignore
        return self.request('DELETE', url, **kwargs)

    def request(  # type: ignore
        self,
        method: str,
        url: str,
        *args,
        headers: Optional[Mapping[str, str]] = None,
        expire_after: ExpirationTime = None,
        only_if_cached: bool = False,
        refresh: bool = False,
        force_refresh: bool = False,
        **kwargs,
    ) -> AnyResponse:
        """This method prepares and sends a request while automatically performing any necessary
        caching operations. This will be called by any other method-specific ``requests`` functions
        (get, post, etc.). This is not used by :py:class:`~requests.PreparedRequest` objects, which
        are handled by :py:meth:`send()`.

        See :py:meth:`requests.Session.request` for base parameters. Additional parameters:

        Args:
            expire_after: Expiration time to set only for this request. See :ref:`expiration` for
                details.
            only_if_cached: Only return results from the cache. If not cached, return a 504 response
                instead of sending a new request.
            refresh: Revalidate with the server before using a cached response, and refresh if needed
                (e.g., a "soft refresh," like F5 in a browser)
            force_refresh: Always make a new request, and overwrite any previously cached response
                (e.g., a "hard refresh", like Ctrl-F5 in a browser))

        Returns:
            Either a new or cached response
        """
        headers = set_request_headers(headers, expire_after, only_if_cached, refresh, force_refresh)
        with patch_form_boundary() if kwargs.get('files') else nullcontext():
            return super().request(method, url, *args, headers=headers, **kwargs)  # type: ignore

    def send(
        self,
        request: PreparedRequest,
        expire_after: ExpirationTime = None,
        only_if_cached: bool = False,
        refresh: bool = False,
        force_refresh: bool = False,
        **kwargs,
    ) -> AnyResponse:
        """Send a prepared request, with caching. See :py:meth:`requests.Session.send` for base
        parameters, and see :py:meth:`.request` for extra parameters.

        **Order of operations:** For reference, a request will pass through the following methods:

        1. :py:func:`requests.get`, :py:meth:`CachedSession.get`, etc. (optional)
        2. :py:meth:`.CachedSession.request`
        3. :py:meth:`requests.Session.request`
        4. :py:meth:`.CachedSession.send`
        5. :py:meth:`.BaseCache.get_response`
        6. :py:meth:`requests.Session.send` (if not using a cached response)
        7. :py:meth:`.BaseCache.save_response` (if not using a cached response)
        """
        # Determine which actions to take based on settings and request info
        request.headers = set_request_headers(
            request.headers, expire_after, only_if_cached, refresh, force_refresh
        )
        actions = CacheActions.from_request(
            self.cache.create_key(request, **kwargs), request, self.settings
        )

        # Attempt to fetch a cached response
        cached_response: Optional[CachedResponse] = None
        if not actions.skip_read:
            cached_response = self.cache.get_response(actions.cache_key)
        actions.update_from_cached_response(cached_response, self.cache.create_key, **kwargs)

        # Secondary Vary lookup: if the primary entry didn't match on Vary,
        # try a Vary-qualified key before going to the origin server.
        if actions.vary_cache_key:
            vary_key = actions.vary_cache_key
            vary_cached = self.cache.get_response(vary_key)
            # Use the Vary-qualified key for any future storage (hit or miss)
            actions.cache_key = vary_key
            if vary_cached is not None:
                # Reset decision flags from the failed primary Vary check
                actions.send_request = False
                actions.resend_request = False
                actions.resend_async = False
                actions.error_504 = False
                actions.vary_cache_key = None
                actions._validation_headers = {}
                # Re-evaluate freshness/expiry with the Vary-matched response
                actions.update_from_cached_response(vary_cached, self.cache.create_key, **kwargs)
                cached_response = vary_cached

        # Handle missing and expired responses based on settings and headers
        if actions.error_504:
            response: AnyResponse = get_504_response(request)
        elif actions.resend_async:
            self._resend_async(request, actions, cached_response, **kwargs)
            response = cached_response  # type: ignore
        elif actions.resend_request:
            response = self._resend(request, actions, cached_response, **kwargs)  # type: ignore
            if isinstance(response, OriginalResponse) and actions._vary_matched:
                response.update_content_changed(cached_response)
        elif actions.send_request:
            response = self._send_and_cache(request, actions, cached_response, **kwargs)
        else:
            response = cached_response  # type: ignore  # Guaranteed to be non-None by this point
            if (request.method or '').upper() == 'HEAD' and self.settings.head_coordination:
                head_get_key = self.cache.create_key(_as_get_request(request), **kwargs)
                self._coordinate_head_response(
                    head_get_key, request, response, actions, **kwargs
                )

        # If the request has been filtered out and was previously cached, delete it
        if self.settings.filter_fn is not None and not self.settings.filter_fn(response):
            logger.debug(f'Deleting filtered response for URL: {response.url}')
            self.cache.delete(actions.cache_key)
            return response

        # Dispatch any hooks here, because they are removed during serialization
        return dispatch_hook('response', request.hooks, response, **kwargs)

    def _send_and_cache(
        self,
        request: PreparedRequest,
        actions: CacheActions,
        cached_response: Optional[CachedResponse] = None,
        **kwargs,
    ) -> AnyResponse:
        """Send a request and cache the response, unless disabled by settings or headers.
        If applicable, also handle conditional requests.
        """
        head_get_key = None
        if (request.method or '').upper() == 'HEAD' and self.settings.head_coordination:
            head_get_key = self.cache.create_key(_as_get_request(request), **kwargs)

        request = actions.update_request(request)
        response = super().send(request, **kwargs)
        actions.update_from_response(response)

        if head_get_key is not None and not isinstance(response, CachedResponse):
            self._coordinate_head_response(
                head_get_key, request, response, actions, **kwargs
            )

        if not actions.skip_write:
            self.cache.save_response(response, actions.cache_key, actions.expires)
        elif cached_response is not None and response.status_code == 304:
            cached_response = actions.update_revalidated_response(
                response, CachedResponse.from_response(cached_response)
            )
            cached_response.cache_key = actions.cache_key
            if head_get_key is not None:
                self._coordinate_head_response(
                    head_get_key, request, cached_response, actions, **kwargs
                )
            if not actions.skip_write:
                self.cache.save_response(cached_response, actions.cache_key, actions.expires)  # type: ignore[unreachable]
            return cached_response
        else:
            logger.debug(f'Skipping cache write for URL: {request.url}')

        # This is possible if the original request is a cache miss, but updating its validation
        # headers results in redirecting to a different URL that is a cache hit
        if isinstance(response, CachedResponse):
            return response
        else:
            return OriginalResponse.wrap_response(response, actions)

    def _coordinate_head_response(
        self,
        get_key: str,
        head_request: PreparedRequest,
        head_response: AnyResponse,
        actions: CacheActions,
        **kwargs,
    ):
        """Use a HEAD response to freshen or invalidate a stored GET response.

        The GET body is never modified here. Only its non-representation metadata and freshness
        can be updated, and only when validators (and, when provided, Content-Length) prove that
        the entity is unchanged. If the response proves that it changed, the GET entry is deleted.
        """
        if not self.settings.head_coordination:
            return
        if getattr(head_response, 'from_cache', False) and head_response.is_expired:
            return

        get_request = _as_get_request(head_request)
        candidates = self._get_head_candidates(get_key, get_request, **kwargs)
        if not candidates:
            return

        # A response that cannot be reused for this request (or a missing resource) makes the
        # selected GET variants stale. Other Vary variants are left alone.
        if head_response.status_code in (404, 410) or _vary_star(head_response):
            for key, cached_response in candidates:
                self._invalidate_head_candidate(
                    get_key, key, cached_response, actions, head_response
                )
            return
        elif not 200 <= head_response.status_code < 300:
            return

        for key, cached_response in candidates:
            can_freshen = can_freshen_from_head(cached_response, head_response)
            if can_freshen is True:
                self._freshen_head_candidate(key, cached_response, head_response, actions)
            elif can_freshen is False:
                self._invalidate_head_candidate(
                    get_key, key, cached_response, actions, head_response
                )

    def _get_head_candidates(
        self, get_key: str, get_request: PreparedRequest, **kwargs
    ) -> list:
        """Get the stored GET response(s) that could be selected by the HEAD request.

        This checks the primary cache key and, when its ``Vary`` headers don't match, the
        Vary-qualified secondary key. A different Vary variant is never touched.
        """
        candidates = []
        cached_response = self.cache.get_response(get_key)
        if cached_response is not None and _is_get_response(cached_response):
            vary_matches, vary_key = self._get_vary_status(cached_response, get_request, **kwargs)
            if vary_matches:
                candidates.append((cached_response.cache_key or get_key, cached_response))
            elif vary_key:
                vary_response = self.cache.get_response(vary_key)
                if vary_response is not None and _is_get_response(vary_response):
                    vary_matches, _ = self._get_vary_status(vary_response, get_request, **kwargs)
                    if vary_matches:
                        candidates.append(
                            (vary_response.cache_key or vary_key, vary_response)
                        )
        return candidates

    def _get_vary_status(
        self, cached_response: CachedResponse, get_request: PreparedRequest, **kwargs
    ):
        """Check Vary matching and get the secondary cache key, if any."""
        actions = CacheActions.from_request(
            self.cache.create_key(get_request, **kwargs), get_request, self.settings
        )
        actions.update_from_cached_response(
            cached_response, self.cache.create_key, **kwargs
        )
        return actions._vary_matched, actions.vary_cache_key

    def _freshen_head_candidate(
        self,
        key: str,
        cached_response: CachedResponse,
        head_response: AnyResponse,
        actions: CacheActions,
    ):
        """Update a stored GET response's headers and expiration, but not its body."""
        if not self._can_modify_cache(actions, head_response):
            return

        updated_response = CachedResponse.from_response(cached_response)
        updated_response.headers = CaseInsensitiveDict(cached_response.headers)
        updated_response.expires = actions.expires
        updated_response.headers.update(head_response.headers)
        if (
            'Content-Length' in updated_response.headers
            and 'Transfer-Encoding' in updated_response.headers
        ):
            del updated_response.headers['Content-Length']
        updated_response.revalidated = True
        updated_response.has_content_changed = False
        updated_response.raw = CachedHTTPResponse.from_cached_response(updated_response)
        self.cache.save_response(updated_response, key, actions.expires)
        logger.debug(f'Freshened cached GET response for URL: {cached_response.url}')

    def _invalidate_head_candidate(
        self,
        get_key: str,
        key: str,
        cached_response: CachedResponse,
        actions: CacheActions,
        response: Optional[AnyResponse] = None,
    ):
        """Invalidate a stored GET response that the HEAD response shows is stale."""
        if not self._can_modify_cache(actions, response):
            return
        logger.debug(f'Invalidating cached GET response for key: {key}')
        if key == get_key and _has_vary(cached_response):
            # Keep the primary variant as an expired anchor so other Vary variants remain
            # discoverable through the normal primary -> secondary lookup.
            updated_response = CachedResponse.from_response(cached_response)
            updated_response.headers = CaseInsensitiveDict(cached_response.headers)
            for header in ('ETag', 'Last-Modified'):
                updated_response.headers.pop(header, None)
            updated_response.expires = utcnow()
            updated_response.has_content_changed = True
            self.cache.save_response(updated_response, key, updated_response.expires)
        else:
            self.cache.delete(key)

    def _can_modify_cache(
        self, actions: CacheActions, response: Optional[AnyResponse] = None
    ) -> bool:
        if self.settings.read_only or self.settings.disabled or actions._directives.no_store:
            return False
        if response is not None:
            return not CacheDirectives.from_headers(response.headers).no_store
        return True

    def _resend(
        self,
        request: PreparedRequest,
        actions: CacheActions,
        cached_response: CachedResponse,
        **kwargs,
    ) -> AnyResponse:
        """Handle a stale cached response by attempting to resend the request and cache a fresh
        response
        """
        logger.debug('Stale response; attempting to re-send request')
        try:
            response = self._send_and_cache(request, actions, cached_response, **kwargs)
            if (
                actions._stale_if_error
                and response.status_code not in self.settings.allowable_codes
            ):
                response.raise_for_status()
            return response
        except Exception:
            return self._handle_error(cached_response, actions)

    def _resend_async(self, *args, **kwargs):
        """Send a non-blocking request to refresh a cached response"""
        logger.debug('Using stale response while revalidating')
        thread = Thread(target=self._send_and_cache, args=args, kwargs=kwargs)
        thread.start()

    def _handle_error(self, cached_response: CachedResponse, actions: CacheActions) -> AnyResponse:
        """Handle a request error based on settings:
        * Default behavior: re-raise the error
        * stale-if-error: Ignore the error and and return the stale cache item
        """
        if actions.is_usable(cached_response, error=True):
            logger.warning(
                f'Request for URL {cached_response.request.url} failed; using cached response',
                exc_info=True,
            )
            return cached_response
        else:
            raise

    @contextmanager
    def cache_disabled(self):
        """
        Context manager for temporary disabling the cache

        .. warning:: This method is not thread-safe.

        Example:

            >>> s = CachedSession()
            >>> with s.cache_disabled():
            ...     s.get('https://httpbin.org/ip')

        """
        if self.settings.disabled:
            yield
        else:
            self.settings.disabled = True
            try:
                yield
            finally:
                self.settings.disabled = False

    def close(self):
        """Close the session and any open backend connections"""
        super().close()
        if self.settings.autoclose:
            self.cache.close()

    def __getstate__(self):
        # Unlike requests.Session, CachedSession may contain backend connection objects that can't
        # be pickled. Support for this could be added if necessary, but for now it's explicitly
        # disabled to avoid confusing errors upon unpickling.
        raise NotImplementedError('CachedSession cannot be pickled')

    def __repr__(self):
        return f'<CachedSession(cache={repr(self.cache)}, settings={self.settings})>'


class CachedSession(CacheMixin, OriginalSession):
    """Session class that extends :py:class:`requests.Session` with caching features.

    See individual :py:mod:`backend classes <requests_cache.backends>` for additional
    backend-specific arguments. Also see :ref:`user-guide` for more details and examples on how the
    following arguments affect cache behavior.

    Args:
        cache_name: Used as a cache path, prefix, or namespace, depending on the backend
        backend: Cache backend name or instance; name may be one of
            ``['sqlite', 'filesystem', 'mongodb', 'gridfs', 'redis', 'dynamodb', 'memory']``
        serializer: Serializer name or instance; name may be one of
            ``['pickle', 'json', 'yaml', 'bson']``.
        expire_after: Time after which cached items will expire. See :ref:`expiration` for details.
        urls_expire_after: Expiration times to apply for different URL patterns
        cache_control: Use Cache-Control and other response headers to set expiration
        allowable_codes: Only cache responses with one of these status codes
        allowable_methods: Cache only responses for one of these HTTP methods
        always_revalidate: Revalidate with the server for every request, even if the cached response
            is not expired
        match_headers: Request headers to match, when `Vary` response header is not available. May
            be a list of headers, or ``True`` to match all.
        head_coordination: Use successful HEAD responses to freshen metadata (but not content)
            for matching cached GET responses. Enabled by default; set to ``False`` to treat
            HEAD requests with ordinary, independent caching behavior.
        ignored_parameters: Request parameters, headers, and/or JSON body params to exclude from both
            request matching and cached request data
        read_only: Read existing cached responses, but do not write any new responses to the cache
        stale_if_error: Return a stale response if a new request raises an exception. Optionally
            accepts a time value representing maximum staleness to accept.
        stale_while_revalidate: Return a stale response initially, while a non-blocking request is
            sent to refresh the response for the next time it's requested
        autoclose: Close the cache backend when the session is closed
        filter_fn: Response filtering function that indicates whether or not a given response should
            be cached. See :ref:`custom-filtering` for details.
        key_fn: Request matching function for generating custom cache keys. See
            :ref:`custom-matching` for details.
    """


def get_504_response(request: PreparedRequest) -> CachedResponse:
    """Get a 504: Not Cached error response, for use with only-if-cached option"""
    return CachedResponse(
        url=request.url or '',
        status_code=504,
        reason='Not Cached',
        request=request,  # type: ignore
    )


def _as_get_request(request: PreparedRequest) -> PreparedRequest:
    """Make a GET-like request for looking up the stored GET response matching a HEAD request.

    Conditional and range headers are request-specific and should not affect which stored
    representation is selected.
    """
    cached_request = CachedRequest.from_request(request)
    cached_request.method = 'GET'
    for header in (
        'Cache-Control',
        'If-None-Match',
        'If-Modified-Since',
        'If-Range',
        'Range',
        'X-ACTUAL-NO-CACHE',
    ):
        cached_request.headers.pop(header, None)
    return cached_request.prepare()


def _is_get_response(response: CachedResponse) -> bool:
    method = getattr(getattr(response, 'request', None), 'method', None)
    return method is None or method.upper() == 'GET'


def _vary_star(response: AnyResponse) -> bool:
    vary = response.headers.get('Vary', '')
    return any(value.strip() == '*' for value in vary.split(','))


def _has_vary(response: AnyResponse) -> bool:
    """Whether this response participates in Vary-based variant selection."""
    vary = response.headers.get('Vary', '')
    return bool(vary and not _vary_star(response))
