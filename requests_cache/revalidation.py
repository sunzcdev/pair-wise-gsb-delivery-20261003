"""Bounded-concurrency revalidation of cached responses via conditional requests.

This actively checks cached resources against the origin server, instead of waiting for them to be
requested again. Each cached response is revalidated individually, using its original (cached)
request, so URL variants negotiated via ``Vary`` are checked separately. Responses are grouped into
a :py:class:`RevalidationReport` by outcome: unmodified, updated, stale, or failed.
"""

from concurrent.futures import FIRST_COMPLETED, Future, ThreadPoolExecutor, wait
from enum import Enum
from logging import getLogger
from threading import Event
from typing import TYPE_CHECKING, Dict, Iterable, Iterator, List, Optional

from attrs import define, field
from requests import PreparedRequest, Response
from requests import Session as OriginalSession

from .models import CachedRequest, CachedResponse, RichMixin
from .policy import CacheActions, CacheDirectives

if TYPE_CHECKING:
    from .session import CacheMixin

__all__ = [
    'RevalidationReport',
    'RevalidationResult',
    'RevalidationStatus',
    'revalidate_cache',
]

logger = getLogger(__name__)

# Status codes indicating that the cached resource no longer exists on the server
STALE_STATUS_CODES = (404, 410)


class RevalidationStatus(Enum):
    """Possible outcomes of revalidating a single cached response"""

    UNMODIFIED = 'unmodified'  # Server responded with 304; cached response is still valid
    UPDATED = 'updated'  # Server responded with new content; cache has been updated
    STALE = 'stale'  # Server responded with 404/410; cached response is no longer valid
    FAILED = 'failed'  # Revalidation could not be completed (request error or unexpected status)
    PENDING = 'pending'  # Not yet revalidated (e.g., cancelled before it started)


@define(repr=False)
class RevalidationResult(RichMixin):
    """The outcome of revalidating a single cached response.

    Args:
        cache_key: Cache key identifying the cached response (unique per ``Vary`` variant)
        status: Outcome of the revalidation
        request: The original cached request, identifying which negotiated variant was checked
        response: The new or updated response, if one was received
        error: The exception that occurred, if revalidation failed with a request error
    """

    cache_key: str = field(default=None)
    status: RevalidationStatus = field(default=None)
    request: Optional[CachedRequest] = field(default=None)
    response: Optional[Response] = field(default=None, repr=False)
    error: Optional[Exception] = field(default=None, repr=False)

    @property
    def url(self) -> Optional[str]:
        """URL of the cached request, for convenience; note that multiple results may share the
        same URL if the response was negotiated with ``Vary``"""
        return self.request.url if self.request else None


@define(repr=False)
class RevalidationReport(RichMixin):
    """Aggregated results of revalidating cached responses.

    Args:
        results: Completed revalidation results
        pending: Cached responses that have not been revalidated (e.g., due to cancellation)
    """

    results: List[RevalidationResult] = field(factory=list)
    pending: List[RevalidationResult] = field(factory=list)

    @property
    def unmodified(self) -> List[RevalidationResult]:
        """Responses confirmed unmodified by the server (304)"""
        return self._by_status(RevalidationStatus.UNMODIFIED)

    @property
    def updated(self) -> List[RevalidationResult]:
        """Responses replaced with new content from the server"""
        return self._by_status(RevalidationStatus.UPDATED)

    @property
    def stale(self) -> List[RevalidationResult]:
        """Responses whose resource no longer exists on the server (404/410)"""
        return self._by_status(RevalidationStatus.STALE)

    @property
    def failed(self) -> List[RevalidationResult]:
        """Responses that could not be revalidated due to an error"""
        return self._by_status(RevalidationStatus.FAILED)

    @property
    def cancelled(self) -> bool:
        """Whether revalidation was cancelled before all cached responses were checked"""
        return bool(self.pending)

    def _by_status(self, status: RevalidationStatus) -> List[RevalidationResult]:
        return [r for r in self.results if r.status is status]

    def __iter__(self) -> Iterator[RevalidationResult]:
        return iter(self.results)

    def __len__(self) -> int:
        return len(self.results)


def revalidate_cache(
    session: 'CacheMixin',
    keys: Optional[Iterable[str]] = None,
    max_workers: int = 4,
    cancel_event: Optional[Event] = None,
    **send_kwargs,
) -> RevalidationReport:
    """Revalidate cached responses with the origin server, using conditional requests where
    validators (``ETag``/``Last-Modified``) are available.

    Each cached response is checked individually using its original request, so multiple ``Vary``
    variants of the same URL are revalidated separately. A failure for one item does not affect
    the remaining items. Normal cache-hit behavior (including response hooks) is bypassed, and
    cached responses remain readable during and after revalidation.

    Args:
        session: The cached session to revalidate
        keys: Specific cache keys to revalidate; defaults to all cached responses
        max_workers: Maximum number of concurrent requests
        cancel_event: When set, stop accepting new items and wait for in-flight requests to
            finish; unchecked items are returned in ``report.pending``
        send_kwargs: Additional keyword arguments for :py:meth:`requests.Session.send`
            (e.g., ``timeout``)
    """
    if max_workers < 1:
        raise ValueError('max_workers must be at least 1')
    cancel_event = cancel_event or Event()
    all_keys = (
        list(dict.fromkeys(keys)) if keys is not None else list(session.cache.responses.keys())
    )
    key_iter = iter(all_keys)
    results: List[RevalidationResult] = []

    # Submit items lazily (up to max_workers at a time) so that cancellation stops accepting new
    # items immediately, while in-flight requests are allowed to finish
    with ThreadPoolExecutor(
        max_workers=max_workers, thread_name_prefix='requests_cache_revalidate'
    ) as executor:
        in_flight: Dict[Future, str] = {}
        exhausted = False

        def submit_next():
            nonlocal exhausted
            if exhausted or cancel_event.is_set():
                return
            try:
                key = next(key_iter)
            except StopIteration:
                exhausted = True
                return
            future = executor.submit(_revalidate_response, session, key, **send_kwargs)
            in_flight[future] = key

        for _ in range(min(max_workers, len(all_keys))):
            submit_next()

        while in_flight:
            done, _ = wait(list(in_flight), return_when=FIRST_COMPLETED)
            for future in done:
                key = in_flight.pop(future)
                try:
                    results.append(future.result())
                except Exception as e:  # Defensive; _revalidate_response() should not raise
                    results.append(
                        RevalidationResult(cache_key=key, status=RevalidationStatus.FAILED, error=e)
                    )
                submit_next()

    return RevalidationReport(results=results, pending=_pending_results(session, key_iter))


def _pending_results(
    session: 'CacheMixin', remaining_keys: Iterator[str]
) -> List[RevalidationResult]:
    """Build results for items that were never submitted, so the caller can tell which cached
    resources have not been verified yet"""
    pending = []
    for key in remaining_keys:
        cached_response = session.cache.responses.get(key)
        request = cached_response.request if cached_response is not None else None
        pending.append(
            RevalidationResult(cache_key=key, status=RevalidationStatus.PENDING, request=request)
        )
    return pending


def _revalidate_response(
    session: 'CacheMixin', cache_key: str, **send_kwargs
) -> RevalidationResult:
    """Revalidate a single cached response. Never raises; errors are reported in the result."""
    cached_response = session.cache.responses.get(cache_key)
    if cached_response is None:
        return RevalidationResult(
            cache_key=cache_key,
            status=RevalidationStatus.FAILED,
            error=ValueError('Cached response is missing or cannot be deserialized'),
        )
    cached_request = cached_response.request

    try:
        request = _prepare_conditional_request(cached_response)
        # Bypass CachedSession.send() so no cache lookup, cache write side effects, or
        # response hooks are triggered by the revalidation request itself
        response = OriginalSession.send(session, request, allow_redirects=False, **send_kwargs)
    except Exception as e:
        logger.warning(f'Revalidation failed for {cached_request}: {e}')
        return RevalidationResult(
            cache_key=cache_key,
            status=RevalidationStatus.FAILED,
            request=cached_request,
            error=e,
        )

    return _handle_revalidation_response(session, cache_key, cached_response, response)


def _prepare_conditional_request(cached_response: CachedResponse) -> PreparedRequest:
    """Rebuild the original request, with conditional headers based on the cached response's
    validators. Replaying the original request (including its headers) ensures that the same
    ``Vary`` variant is checked, rather than just the same URL.
    """
    request = cached_response.request.prepare()
    request.hooks = {'response': []}

    # Replace any stale validation headers from a previous revalidation
    request.headers.pop('If-None-Match', None)
    request.headers.pop('If-Modified-Since', None)
    directives = CacheDirectives.from_headers(cached_response.headers)
    if directives.etag:
        request.headers['If-None-Match'] = directives.etag
    if directives.last_modified:
        request.headers['If-Modified-Since'] = directives.last_modified
    return request


def _handle_revalidation_response(
    session: 'CacheMixin',
    cache_key: str,
    cached_response: CachedResponse,
    response: Response,
) -> RevalidationResult:
    """Classify the server's response and update the cache if needed"""
    cached_request = cached_response.request
    result_kwargs = {'cache_key': cache_key, 'request': cached_request}

    if response.status_code == 304:
        updated_response = _update_cached_response(session, cache_key, cached_response, response)
        return RevalidationResult(
            status=RevalidationStatus.UNMODIFIED, response=updated_response, **result_kwargs
        )
    elif 200 <= response.status_code < 300:
        session.cache.save_response(response, cache_key)
        return RevalidationResult(
            status=RevalidationStatus.UPDATED, response=response, **result_kwargs
        )
    elif response.status_code in STALE_STATUS_CODES:
        # Leave the cached response untouched so it remains readable
        return RevalidationResult(
            status=RevalidationStatus.STALE, response=response, **result_kwargs
        )
    else:
        logger.warning(
            f'Unexpected status {response.status_code} when revalidating {cached_request}'
        )
        return RevalidationResult(
            status=RevalidationStatus.FAILED, response=response, **result_kwargs
        )


def _update_cached_response(
    session: 'CacheMixin', cache_key: str, cached_response: CachedResponse, response: Response
) -> CachedResponse:
    """Update a cached response's headers and expiration after a 304, without modifying the
    original response object"""
    actions = CacheActions.from_request(cache_key, response.request, session.settings)
    updated_response = actions.update_revalidated_response(
        response, CachedResponse.from_response(cached_response)
    )
    updated_response.cache_key = cache_key
    if not actions.skip_write:
        session.cache.save_response(updated_response, cache_key, actions.expires)
    return updated_response
