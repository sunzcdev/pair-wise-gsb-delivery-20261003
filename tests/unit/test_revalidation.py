"""Tests for active, bounded-concurrency cache revalidation"""

from threading import Barrier, Event

import pytest
from requests import Request, Response
from requests.adapters import HTTPAdapter

from requests_cache import (
    CachedSession,
    CacheRevalidator,
    RevalidationReport,
    RevalidationResult,
    RevalidationStatus,
)
from tests.conftest import MOCKED_URL, mount_mock_adapter


# Test endpoints
ETAG_URL = f'{MOCKED_URL}/revalidate/etag'
PLAIN_URL = f'{MOCKED_URL}/revalidate/plain'
VARY_URL = f'{MOCKED_URL}/revalidate/vary'
GONE_URL = f'{MOCKED_URL}/revalidate/gone'
ERROR_URL = f'{MOCKED_URL}/revalidate/error'

ETAG = '"etag-1"'


def _has_conditional_header(request):
    """requests-mock matcher: only match revalidation (conditional) requests."""
    return 'If-None-Match' in request.headers or 'If-Modified-Since' in request.headers


def _no_conditional_header(request):
    return not _has_conditional_header(request)


class _RevalidationAdapter(HTTPAdapter):
    """Deterministic, thread-safe mock adapter with a small requests-mock-like API.

    Requests-mock matchers are LIFO and shared across worker threads, which makes concurrent
    revalidation against the same URL non-deterministic. This adapter routes each request from
    its own headers instead, so the outcome never depends on registration order or timing.
    """

    def __init__(self):
        super().__init__()
        import threading

        self._lock = threading.Lock()
        self.request_history = []
        self._overrides = {}
        self._exceptions = {}

    def register_uri(self, method, url, additional_matcher=None, exc=None, **kwargs):
        """Register an override for a URL, optionally gated by a request matcher."""
        entry = {'matcher': additional_matcher, 'kwargs': kwargs}
        with self._lock:
            if exc is not None:
                self._exceptions.setdefault(url, []).append(
                    {'matcher': additional_matcher, 'exc': exc}
                )
            else:
                self._overrides.setdefault(url, []).append(entry)

    def send(self, request, *args, **kwargs):
        with self._lock:
            self.request_history.append(request)

            for entry in self._exceptions.get(request.url, []):
                if entry['matcher'] is None or entry['matcher'](request):
                    raise entry['exc']

            for entry in reversed(self._overrides.get(request.url, [])):
                if entry['matcher'] is None or entry['matcher'](request):
                    return self._build_response(request, entry['kwargs'])

        return self._default_response(request)

    def _build_response(self, request, kwargs):
        body = kwargs['text'].encode('utf-8') if 'text' in kwargs else b''
        response = self._new_response(request, body)
        response.status_code = kwargs.get('status_code', 200)
        for key, value in (kwargs.get('headers') or {}).items():
            response.headers[key] = value
        if 'text' in kwargs:
            response._content = body
        return response

    @staticmethod
    def _new_response(request, body=b''):
        from io import BytesIO

        from urllib3 import HTTPResponse

        response = Response()
        response.request = request
        response.url = request.url
        response._content = body
        response.raw = HTTPResponse(
            body=BytesIO(body),
            headers={},
            status=200,
            preload_content=False,
            request_url=request.url,
        )
        return response

    def _default_response(self, request):
        response = self._new_response(request)
        conditional = _has_conditional_header(request)
        url = request.url.rstrip('/')

        if url.endswith('/gone'):
            response.status_code = 404 if conditional else 200
            if not conditional:
                response.headers['ETag'] = ETAG
        elif url.endswith('/error'):
            response.status_code = 500 if conditional else 200
            if not conditional:
                response.headers['ETag'] = ETAG
        elif url.endswith('/vary'):
            response.status_code = 304 if conditional else 200
            response.headers['ETag'] = ETAG
            response.headers['Vary'] = 'Accept'
        else:
            response.status_code = 304 if conditional else 200
            if not url.endswith('/plain'):
                response.headers['ETag'] = ETAG
        return response


@pytest.fixture
def session(tmp_path):
    """A mocked session pre-populated with several cached entities."""
    cached_session = CachedSession(tmp_path / 'revalidate', backend='sqlite')
    adapter = _RevalidationAdapter()
    for protocol in ('mock://', 'http+mock://', 'https+mock://'):
        cached_session.mount(protocol, adapter)
    cached_session.mock_adapter = adapter

    cached_session.get(ETAG_URL)
    cached_session.get(PLAIN_URL)
    cached_session.get(VARY_URL, headers={'Accept': 'application/json'})
    cached_session.get(VARY_URL, headers={'Accept': 'text/html'})
    cached_session.get(GONE_URL)
    cached_session.get(ERROR_URL)
    return cached_session


def _key(session, url, headers=None):
    request = Request('GET', url, headers=headers or {}).prepare()
    return session.cache.create_key(request)


# Basic report structure
# -----------------------------------------------------


def test_report_empty(mock_session):
    report = mock_session.revalidate()
    assert isinstance(report, RevalidationReport)
    assert report.total == 0
    assert len(report) == 0
    assert list(report) == []
    assert report.counts() == {
        'not_modified': 0,
        'updated': 0,
        'invalidated': 0,
        'skipped': 0,
        'failed': 0,
    }


def test_report_status_grouping(session):
    report = session.revalidate()
    counts = report.counts()
    assert counts['not_modified'] == 3  # etag + 2 vary variants
    assert counts['skipped'] == 1  # plain, no validator
    assert counts['invalidated'] == 1
    assert counts['failed'] == 1
    assert report.total == 6

    assert {r.status for r in report.not_modified} == {RevalidationStatus.NOT_MODIFIED}
    assert len(report.updated) == 0
    assert len(report.invalidated) == 1
    assert len(report.failed) == 1
    assert len(report.skipped) == 1
    # Everything except not-modified/invalidated is unverified
    assert len(report.unverified) == 2


def test_report_result_metadata(session):
    etag_key = _key(session, ETAG_URL)
    plain_key = _key(session, PLAIN_URL)
    report = session.revalidate()
    result = report.get(etag_key)
    assert isinstance(result, RevalidationResult)
    assert result.status is RevalidationStatus.NOT_MODIFIED
    # A 304 is surfaced as the refreshed cached response with its original status code
    assert result.status_code == 200
    assert result.new_response is not None and result.new_response.revalidated is True
    assert result.url == ETAG_URL
    assert result.method == 'GET'
    assert result.ok is True
    assert result.unverified is False
    assert result.cached_response is not None
    assert report.get('unknown-key') is None

    skipped = report.get(plain_key)
    assert skipped.ok is False and skipped.unverified is True


# Per-entity outcomes
# -----------------------------------------------------


def test_not_modified_refreshes_cached_response(session):
    etag_key = _key(session, ETAG_URL)
    report = session.revalidate(keys=[etag_key])
    result = report.get(etag_key)
    assert result.status is RevalidationStatus.NOT_MODIFIED
    # The returned response reflects the successful 304 revalidation
    assert result.new_response is not None and result.new_response.revalidated is True
    # The cached entry still exists and remains readable
    cached = session.cache.get_response(etag_key)
    assert cached is not None and cached.status_code == 200
    # Conditional headers were actually sent
    conditional_requests = [
        r for r in session.mock_adapter.request_history if 'If-None-Match' in r.headers
    ]
    assert conditional_requests
    assert conditional_requests[-1].headers['If-None-Match'] == ETAG


def test_updated_replaces_cached_entity(session):
    etag_key = _key(session, ETAG_URL)
    session.mock_adapter.register_uri(
        'GET',
        ETAG_URL,
        additional_matcher=_has_conditional_header,
        text='brand new content',
        headers={'ETag': '"etag-2"'},
        status_code=200,
    )
    report = session.revalidate(keys=[etag_key])
    result = report.get(etag_key)
    assert result.status is RevalidationStatus.UPDATED
    assert result.status_code == 200
    assert session.cache.get_response(etag_key).text == 'brand new content'


def test_invalidated_removes_entity_but_keeps_report_copy(session):
    gone_key = _key(session, GONE_URL)
    report = session.revalidate(keys=[gone_key])
    result = report.get(gone_key)
    assert result.status is RevalidationStatus.INVALIDATED
    assert result.status_code == 404
    assert not session.cache.contains(key=gone_key)
    # The original response remains readable through the report
    assert result.cached_response is not None
    assert result.cached_response.status_code == 200


@pytest.mark.parametrize('status_code', [404, 410])
def test_gone_status_is_invalidated(session, status_code):
    url = f'{MOCKED_URL}/revalidate/gone-{status_code}'
    session.mock_adapter.register_uri('GET', url, headers={'ETag': ETAG}, status_code=200)
    session.get(url)
    session.mock_adapter.register_uri(
        'GET', url, additional_matcher=_has_conditional_header, status_code=status_code
    )
    key = _key(session, url)
    report = session.revalidate(keys=[key])
    assert report.get(key).status is RevalidationStatus.INVALIDATED
    assert not session.cache.contains(key=key)


def test_failed_keeps_cached_entity_readable(session):
    error_key = _key(session, ERROR_URL)
    report = session.revalidate(keys=[error_key])
    result = report.get(error_key)
    assert result.status is RevalidationStatus.FAILED
    assert result.status_code == 500
    assert '500' in result.error
    # Cache is untouched and still readable
    cached = session.cache.get_response(error_key)
    assert cached is not None and cached.status_code == 200


def test_failed_on_connection_error(session):
    from requests.exceptions import ConnectionError

    # Exceptions must propagate out of send() rather than being swallowed by stale-if-error
    session.settings.stale_if_error = False
    etag_key = _key(session, ETAG_URL)
    failure_url = f'{ETAG_URL}/connection-failure'
    session.mock_adapter.register_uri(
        'GET', failure_url, exc=ConnectionError('connection timed out')
    )
    # Point the cached entity's request at the failing endpoint
    cached = session.cache.get_response(etag_key)
    cached.request.url = failure_url
    session.cache.responses[etag_key] = cached

    report = session.revalidate(keys=[etag_key])
    result = report.get(etag_key)
    assert result.status is RevalidationStatus.FAILED
    assert result.error
    assert session.cache.contains(key=etag_key)


def test_one_failure_does_not_lose_other_results(session):
    etag_key = _key(session, ETAG_URL)
    error_key = _key(session, ERROR_URL)
    report = session.revalidate(keys=[etag_key, error_key], max_workers=1)
    assert report.get(error_key).status is RevalidationStatus.FAILED
    assert report.get(etag_key).status is RevalidationStatus.NOT_MODIFIED


def test_entity_without_validator_is_skipped(session):
    plain_key = _key(session, PLAIN_URL)
    history_before = len(session.mock_adapter.request_history)
    report = session.revalidate(keys=[plain_key])
    result = report.get(plain_key)
    assert result.status is RevalidationStatus.SKIPPED
    assert result.reason
    assert result in report.unverified
    # No request was sent for it
    assert len(session.mock_adapter.request_history) == history_before


def test_force_refresh_revalidates_without_validator(session):
    plain_key = _key(session, PLAIN_URL)
    session.mock_adapter.register_uri(
        'GET',
        PLAIN_URL,
        additional_matcher=_no_conditional_header,
        text='refreshed body',
        status_code=200,
    )
    report = session.revalidate(keys=[plain_key], force_refresh=True)
    result = report.get(plain_key)
    assert result.status is RevalidationStatus.UPDATED
    assert session.cache.get_response(plain_key).text == 'refreshed body'
    # An unconditional request carries no validator headers
    last_request = session.mock_adapter.request_history[-1]
    assert 'If-None-Match' not in last_request.headers


# Vary variants are distinct entities
# -----------------------------------------------------


def test_vary_variants_revalidated_independently(session):
    # Vary variants are stored under separate (Vary-qualified) keys even though they share a URL
    vary_keys = [
        response.cache_key
        for response in session.cache.responses.values()
        if response.url == VARY_URL
    ]
    assert len(vary_keys) == 2
    json_key, html_key = vary_keys

    report = session.revalidate(keys=vary_keys)
    assert report.get(json_key).status is RevalidationStatus.NOT_MODIFIED
    assert report.get(html_key).status is RevalidationStatus.NOT_MODIFIED

    # Both variants remain in the cache, under their own keys
    assert session.cache.contains(key=json_key)
    assert session.cache.contains(key=html_key)

    # The conditional requests preserved each variant's negotiated Accept header
    vary_requests = [r for r in session.mock_adapter.request_history if r.url.startswith(VARY_URL)]
    accept_headers = {r.headers.get('Accept') for r in vary_requests}
    assert {'application/json', 'text/html'}.issubset(accept_headers)


def test_revalidate_iterates_entities_not_urls(session):
    """All Vary variants of one URL must appear as separate results, even though the URL matches."""
    report = session.revalidate()
    vary_results = [r for r in report if r.url == VARY_URL]
    assert len(vary_results) == 2
    assert len({r.cache_key for r in vary_results}) == 2


# Hooks
# -----------------------------------------------------


def test_revalidation_does_not_dispatch_hooks(session):
    # Attach a hook directly to the session; revalidation must suppress it via skip_hooks.
    calls = []

    def hook(response, *args, **kwargs):
        calls.append(response)
        return response

    session.hooks['response'].append(hook)
    # A normal request dispatches the session-level hook exactly once
    session.get(PLAIN_URL)
    assert len(calls) == 1

    # Bulk revalidation never invokes it
    calls.clear()
    session.revalidate()
    assert calls == []


def test_normal_send_still_dispatches_hooks(mock_session):
    """The skip_hooks path must not change ordinary request behavior."""
    url = f'{MOCKED_URL}/hooks-check'
    mock_session.mock_adapter.register_uri('GET', url, status_code=200)
    calls = []

    def hook(response, *args, **kwargs):
        calls.append(response)
        return response

    # Populate the cache, then verify a normal cache-hit request still runs the hook once
    mock_session.get(url)
    mock_session.get(url, hooks={'response': [hook]})
    assert len(calls) == 1


# Cancellation & concurrency
# -----------------------------------------------------


def test_preset_cancel_event_skips_everything(session):
    event = Event()
    event.set()
    report = session.revalidate(cancel_event=event)
    assert report.total == 6
    assert all(r.status is RevalidationStatus.SKIPPED for r in report)
    assert all(r.reason == 'revalidation cancelled' for r in report)
    assert len(report.unverified) == report.total


def test_cancel_stops_admitting_new_entities(session):
    all_keys = list(session.cache.responses.keys())
    revalidator = CacheRevalidator(cache=session.cache, send=session.send, max_workers=2)
    revalidator.cancel()
    report = revalidator.run(all_keys)
    assert all(r.status is RevalidationStatus.SKIPPED for r in report)


class _BlockingAdapter(HTTPAdapter):
    """Adapter that blocks inside send() until released, to exercise bounded concurrency."""

    def __init__(self, max_concurrent):
        super().__init__()
        self.barrier = Barrier(max_concurrent)
        self.max_concurrent = max_concurrent
        self.observed_max = 0
        self._active = 0

    def send(self, request, *args, **kwargs):
        import threading

        with threading.Lock():
            self._active += 1
            self.observed_max = max(self.observed_max, self._active)
        try:
            # Wait until max_concurrent requests are simultaneously in flight.
            self.barrier.wait(timeout=10)
            response = _RevalidationAdapter._new_response(request)
            response.status_code = 304
            response.headers['ETag'] = ETAG
            return response
        finally:
            with threading.Lock():
                self._active -= 1


def test_max_workers_bounds_concurrency(mock_session):
    n_items = 6
    max_workers = 3
    adapter = _BlockingAdapter(max_concurrent=max_workers)
    for i in range(n_items):
        url = f'{MOCKED_URL}/concurrent/{i}'
        mock_session.mock_adapter.register_uri('GET', url, headers={'ETag': ETAG}, status_code=200)
        mock_session.get(url)
    mock_session.mount('http+mock://', adapter)

    report = mock_session.revalidate(max_workers=max_workers)
    assert report.total == n_items
    assert all(r.status is RevalidationStatus.NOT_MODIFIED for r in report)
    assert adapter.observed_max == max_workers


def test_invalid_max_workers(mock_session):
    with pytest.raises(ValueError):
        mock_session.revalidate(max_workers=0)


class _GatedAdapter(HTTPAdapter):
    """Holds the first request inside send() until ``release`` is set, then answers 304."""

    def __init__(self):
        super().__init__()
        self.entered = Event()
        self.release = Event()
        self.sent = 0

    def send(self, request, *args, **kwargs):
        self.sent += 1
        self.entered.set()
        self.release.wait(timeout=10)
        response = _RevalidationAdapter._new_response(request)
        response.status_code = 304
        response.headers['ETag'] = ETAG
        return response


def test_cancel_during_flight_report_contents(mock_session):
    """After cancellation, in-flight entities finish while the rest are skipped in the report."""
    n_items = 4
    adapter = _GatedAdapter()
    for i in range(n_items):
        url = f'{MOCKED_URL}/cancel-report/{i}'
        mock_session.mock_adapter.register_uri('GET', url, headers={'ETag': ETAG}, status_code=200)
        mock_session.get(url)
    mock_session.mount('http+mock://', adapter)

    cancel_event = Event()
    revalidator = CacheRevalidator(
        cache=mock_session.cache,
        send=mock_session.send,
        max_workers=1,
        cancel_event=cancel_event,
    )

    import threading

    holder = {}

    def run():
        holder['report'] = revalidator.run()

    thread = threading.Thread(target=run)
    thread.start()
    assert adapter.entered.wait(timeout=10)
    # Cancel while one request is blocked in flight
    cancel_event.set()
    adapter.release.set()
    thread.join(timeout=15)
    assert not thread.is_alive()

    report = holder['report']
    assert report.total == n_items
    # Only the single in-flight entity was admitted and finished; the rest were never sent
    assert adapter.sent == 1
    assert len(report.not_modified) == 1
    assert len(report.skipped) == n_items - 1
    assert all(r.reason == 'revalidation cancelled' for r in report.skipped)
    assert len(report.unverified) == n_items - 1


# Misc
# -----------------------------------------------------


def test_missing_key_is_reported_as_failure(session):
    report = session.revalidate(keys=['nonexistent-key'])
    result = report.get('nonexistent-key')
    assert result.status is RevalidationStatus.FAILED
    assert result.error


def test_explicit_key_subset(session):
    etag_key = _key(session, ETAG_URL)
    report = session.revalidate(keys=[etag_key])
    assert report.total == 1
    assert report.get(etag_key).status is RevalidationStatus.NOT_MODIFIED


def test_explicit_keys_keep_one_result_per_key_in_order(session):
    keys = list(session.cache.responses.keys())
    subset = [keys[0], keys[2], keys[0]]  # Duplicate must produce two independent results
    report = session.revalidate(keys=subset, max_workers=1)
    assert [r.cache_key for r in report] == subset
    assert report.total == 3


def test_revalidate_works_with_memory_backend():
    session = mount_mock_adapter(CachedSession(backend='memory'))
    session.mock_adapter.register_uri(
        'GET',
        ETAG_URL,
        [
            {'status_code': 200, 'headers': {'ETag': ETAG}},
        ],
    )
    session.mock_adapter.register_uri(
        'GET',
        ETAG_URL,
        additional_matcher=_has_conditional_header,
        status_code=304,
        headers={'ETag': ETAG},
    )
    session.get(ETAG_URL)
    report = session.revalidate()
    assert report.counts()['not_modified'] == 1


def test_revalidate_non_get_method(mock_session):
    url = f'{MOCKED_URL}/revalidate/post'
    mock_session.mock_adapter.register_uri('POST', url, headers={'ETag': ETAG}, status_code=200)
    mock_session.mock_adapter.register_uri(
        'POST',
        url,
        additional_matcher=_has_conditional_header,
        status_code=304,
        headers={'ETag': ETAG},
    )
    mock_session.post(url)
    report = mock_session.revalidate()
    result = report.results[0]
    assert result.method == 'POST'
    assert result.status is RevalidationStatus.NOT_MODIFIED
