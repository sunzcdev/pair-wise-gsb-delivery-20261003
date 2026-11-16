"""Active revalidation of previously cached responses.

This module provides a bounded-concurrency entry point for proactively revalidating the
contents of a cache. Unlike a normal (possibly cached) request, revalidation:

* iterates over the cache's *entities* (cache keys), so distinct ``Vary`` variants stored under
  different keys are revalidated independently even when they share a URL;
* sends a conditional request for every selected entity that carries a validator
  (``ETag`` / ``If-None-Match`` and/or ``Last-Modified`` / ``If-Modified-Since``);
* collects an independent result for every entity, so a single failure never discards the
  results of the other entities;
* does not dispatch normal response hooks;
* can be cancelled, after which no new entities are admitted and in-flight requests are allowed
  to finish ("graceful" cancellation);
* never removes the previously cached response on failure, so it remains readable afterwards.
"""

from concurrent.futures import FIRST_COMPLETED, ThreadPoolExecutor, wait
from contextlib import contextmanager
from enum import Enum
from logging import getLogger
from threading import Event, RLock
from typing import Dict, Iterable, List, Optional

from attrs import define, field
from requests import PreparedRequest
from requests.exceptions import RequestException

from .backends import BaseCache
from .models import CachedResponse
from .policy.directives import CacheDirectives

__all__ = [
    'CacheRevalidator',
    'RevalidationReport',
    'RevalidationResult',
    'RevalidationStatus',
]

logger = getLogger(__name__)

# Status codes that indicate the previously cached representation is gone
_INVALIDATING_STATUS_CODES = (404, 410)


class RevalidationStatus(str, Enum):
    """Outcome of revalidating a single cached entity."""

    NOT_MODIFIED = 'not_modified'
    """The server answered ``304 Not Modified``; the cached response was refreshed in place."""

    UPDATED = 'updated'
    """The server answered ``200 OK`` with a new representation; the cache entry was replaced."""

    INVALIDATED = 'invalidated'
    """The server answered with a permanent-gone status (``404``/``410``); the entry was removed."""

    SKIPPED = 'skipped'
    """No revalidation request was sent (e.g. no validator, or the run was cancelled)."""

    FAILED = 'failed'
    """The revalidation request raised an error or got an unexpected response; the cache is unchanged."""


@define
class RevalidationResult:
    """The result of revalidating a single cache key."""

    cache_key: str
    status: Optional[RevalidationStatus] = None
    url: Optional[str] = None
    method: Optional[str] = None
    status_code: Optional[int] = None
    error: Optional[str] = None
    reason: Optional[str] = None
    cached_response: Optional[CachedResponse] = field(default=None, repr=False)
    new_response: Optional[CachedResponse] = field(default=None, repr=False)

    @property
    def ok(self) -> bool:
        """Whether revalidation completed without a failure or a skip."""
        return self.status in (
            RevalidationStatus.NOT_MODIFIED,
            RevalidationStatus.UPDATED,
            RevalidationStatus.INVALIDATED,
        )

    @property
    def unverified(self) -> bool:
        """Whether the entity's current state could not be confirmed with the origin."""
        return self.status in (RevalidationStatus.SKIPPED, RevalidationStatus.FAILED)


@define
class RevalidationReport:
    """Aggregate outcome of a revalidation run, with one result per cache key."""

    results: List[RevalidationResult] = field(factory=list)

    @property
    def not_modified(self) -> List[RevalidationResult]:
        """Entities confirmed still current by a ``304`` response."""
        return [r for r in self.results if r.status == RevalidationStatus.NOT_MODIFIED]

    @property
    def updated(self) -> List[RevalidationResult]:
        """Entities replaced by a fresh ``200`` response."""
        return [r for r in self.results if r.status == RevalidationStatus.UPDATED]

    @property
    def invalidated(self) -> List[RevalidationResult]:
        """Entities removed because the origin reported them as gone."""
        return [r for r in self.results if r.status == RevalidationStatus.INVALIDATED]

    @property
    def failed(self) -> List[RevalidationResult]:
        """Entities that could not be revalidated because of an error."""
        return [r for r in self.results if r.status == RevalidationStatus.FAILED]

    @property
    def skipped(self) -> List[RevalidationResult]:
        """Entities for which no revalidation request was sent."""
        return [r for r in self.results if r.status == RevalidationStatus.SKIPPED]

    @property
    def unverified(self) -> List[RevalidationResult]:
        """Entities whose current state could not be confirmed with the origin.

        This includes skips (for example entities without a validator, or entities not admitted
        because the run was cancelled) and failures.
        """
        return self.skipped + self.failed

    @property
    def total(self) -> int:
        """Number of entities included in the run."""
        return len(self.results)

    def counts(self) -> Dict[str, int]:
        """Return the number of results grouped by status."""
        counts = dict.fromkeys(RevalidationStatus, 0)
        for result in self.results:
            counts[result.status] += 1
        return {status.value: n for status, n in counts.items()}

    def get(self, cache_key: str) -> Optional[RevalidationResult]:
        """Get the result for a specific cache key, if present."""
        for result in self.results:
            if result.cache_key == cache_key:
                return result
        return None

    def __iter__(self):
        return iter(self.results)

    def __len__(self) -> int:
        return len(self.results)


class CacheRevalidator:
    """Runs bounded-concurrency, conditional revalidation over cached entities.

    One task is created per cache key; each task rebuilds the request that produced the cached
    response (preserving its negotiated headers, so distinct ``Vary`` variants are handled
    separately) and sends it as a refresh. Normal response hooks are not dispatched.

    Args:
        cache: The cache backend to revalidate.
        send: Callable sending a prepared request with caching. Typically
            :py:meth:`.CachedSession.send`.
        max_workers: Maximum number of concurrent revalidation requests.
        cancel_event: Optional event that, when set, stops admitting new entities; in-flight
            requests are still awaited.
        force_refresh: Revalidate entities without validators with an unconditional request
            instead of skipping them.
        request_timeout: Optional per-request timeout, forwarded to the underlying transport.
    """

    def __init__(
        self,
        cache: BaseCache,
        send,
        max_workers: int = 5,
        cancel_event: Optional[Event] = None,
        force_refresh: bool = False,
        request_timeout: Optional[float] = None,
    ):
        if max_workers < 1:
            raise ValueError('max_workers must be >= 1')
        self.cache = cache
        self._send = send
        self.max_workers = max_workers
        self.cancel_event = cancel_event or Event()
        self.force_refresh = force_refresh
        self.request_timeout = request_timeout
        self._cancelled = False
        # Some backends (notably the default SQLite setup) share one connection per storage and
        # cannot safely interleave cache reads/writes across threads. This re-entrant lock guards
        # the cache's storage entry points for the duration of a run; it is never held while a
        # worker waits on network I/O, so requests still run with up to ``max_workers`` in flight.
        self._storage_lock = RLock()

    @contextmanager
    def _serialized_storage(self):
        """Temporarily serialize all cache storage calls made by worker threads.

        Only the high-level :class:`~requests_cache.backends.BaseCache` methods exercised by
        :py:meth:`.CachedSession.send` are wrapped, so network requests stay concurrent.
        """
        names = ('get_response', 'save_response', 'delete', 'contains', 'create_key', 'filter')
        originals = {name: getattr(self.cache, name) for name in names}
        lock = self._storage_lock

        def make_wrapper(func):
            def wrapper(*args, **kwargs):
                with lock:
                    return func(*args, **kwargs)

            return wrapper

        for name, func in originals.items():
            setattr(self.cache, name, make_wrapper(func))
        try:
            yield
        finally:
            for name, func in originals.items():
                setattr(self.cache, name, func)

    def cancel(self):
        """Cancel the run. No new entities will be admitted; already submitted requests finish."""
        self._cancelled = True
        self.cancel_event.set()

    @property
    def cancelled(self) -> bool:
        return self._cancelled or self.cancel_event.is_set()

    def run(self, keys: Optional[Iterable[str]] = None) -> RevalidationReport:
        """Revalidate the given cache keys, or every entity in the cache if not specified.

        Returns a :class:`RevalidationReport` containing an independent result for every entity.
        """
        all_keys = list(self.cache.responses.keys()) if keys is None else list(keys)
        report = RevalidationReport()
        if not all_keys:
            return report

        # Bound both the number of worker threads and the number of requests admitted at once.
        # Cache storage access is serialized via _serialized_storage(), while network waits stay
        # concurrent.
        with self._serialized_storage(), ThreadPoolExecutor(
            max_workers=self.max_workers, thread_name_prefix='cache-revalidate'
        ) as executor:
            futures = set()
            for key in all_keys:
                if self.cancelled:
                    report.results.append(self._skip(key, 'revalidation cancelled'))
                    continue
                future = executor.submit(self._revalidate, key)
                futures.add(future)
                # Back-pressure: never have more than max_workers requests outstanding, so a
                # cancellation is observed before any additional work is admitted.
                if len(futures) >= self.max_workers:
                    futures = self._collect(futures, report)

            while futures:
                futures = self._collect(futures, report)

        return report

    def _collect(self, futures, report: RevalidationReport):
        """Wait for at least one in-flight task and record its result."""
        done, pending = wait(futures, return_when=FIRST_COMPLETED)
        for future in done:
            report.results.append(future.result())
        return set(pending)

    def _skip(self, key: str, reason: str) -> RevalidationResult:
        cached = self.cache.get_response(key)
        return RevalidationResult(
            cache_key=key,
            status=RevalidationStatus.SKIPPED,
            url=getattr(cached, 'url', None),
            method=getattr(getattr(cached, 'request', None), 'method', None),
            reason=reason,
            cached_response=cached,
        )

    def _revalidate(self, key: str) -> RevalidationResult:
        cached = self.cache.get_response(key)
        if cached is None:
            return RevalidationResult(
                cache_key=key,
                status=RevalidationStatus.FAILED,
                error='cached response is missing or could not be deserialized',
            )

        url = cached.url
        method = cached.request.method
        base = RevalidationResult(cache_key=key, url=url, method=method, cached_response=cached)

        if not self.force_refresh and not self._has_validator(cached):
            base.status = RevalidationStatus.SKIPPED
            base.reason = 'cached response has no validator (ETag or Last-Modified)'
            return base

        try:
            request = self._prepare_request(cached)
            kwargs = {'timeout': self.request_timeout} if self.request_timeout is not None else {}
            response = self._send(
                request,
                refresh=not self.force_refresh,
                force_refresh=self.force_refresh,
                skip_hooks=True,
                **kwargs,
            )
        except RequestException as e:
            base.status = RevalidationStatus.FAILED
            base.error = str(e)
            return base
        except Exception as e:  # Defensive: a single bad entity must not sink the whole run
            logger.warning(f'Revalidation failed for {method} {url}', exc_info=True)
            base.status = RevalidationStatus.FAILED
            base.error = str(e)
            return base

        base.status_code = response.status_code
        base.new_response = response if isinstance(response, CachedResponse) else None

        # A 304 is returned as the refreshed cached response, which keeps its original status code
        # (see CacheActions.update_revalidated_response); detect it via the per-response
        # `revalidated` flag, which is safe to read from multiple worker threads.
        if getattr(response, 'revalidated', False):
            base.status = RevalidationStatus.NOT_MODIFIED
        elif response.status_code == 200:
            base.status = RevalidationStatus.UPDATED
        elif response.status_code in _INVALIDATING_STATUS_CODES:
            base.status = RevalidationStatus.INVALIDATED
            self.cache.delete(key)
        else:
            base.status = RevalidationStatus.FAILED
            base.error = f'unexpected response status: {response.status_code}'
        return base

    @staticmethod
    def _has_validator(cached: CachedResponse) -> bool:
        return CacheDirectives.from_headers(cached.headers).has_validator

    @staticmethod
    def _prepare_request(cached: CachedResponse) -> PreparedRequest:
        """Rebuild the original request, keeping negotiated headers (e.g. for Vary variants)."""
        request = cached.request.prepare()
        # Hooks must not run during bulk revalidation; they are also not part of the cache key.
        request.hooks = {'response': []}
        return request
