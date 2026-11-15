"""Tests for bounded-concurrency cache revalidation"""

from threading import Event, Lock
from time import sleep

import pytest
import requests
from requests_mock import ANY as ANY_METHOD

from requests_cache import RevalidationStatus
from tests.conftest import (
    ETAG,
    MOCKED_URL,
    MOCKED_URL_200_404,
    MOCKED_URL_ETAG,
    MOCKED_URL_JSON,
    MOCKED_URL_VARY,
)


def test_revalidate_unmodified(mock_session):
    """A 304 response should mark the cached response as unmodified and keep it readable"""
    mock_session.get(MOCKED_URL_ETAG)
    mock_session.mock_adapter.register_uri(ANY_METHOD, MOCKED_URL_ETAG, status_code=304)

    report = mock_session.revalidate()

    assert len(report.unmodified) == 1
    assert not (report.updated or report.stale or report.failed or report.pending)
    response = mock_session.get(MOCKED_URL_ETAG)
    assert response.from_cache and response.status_code == 200


def test_revalidate_sends_conditional_headers(mock_session):
    """Validators from the cached response should be sent as conditional request headers"""
    mock_session.get(MOCKED_URL_ETAG)
    mock_session.mock_adapter.register_uri(ANY_METHOD, MOCKED_URL_ETAG, status_code=304)

    mock_session.revalidate()

    last_request = mock_session.mock_adapter.request_history[-1]
    assert last_request.headers['If-None-Match'] == ETAG


def test_revalidate_updated(mock_session):
    """A 200 response should replace the cached response"""
    mock_session.get(MOCKED_URL)
    mock_session.mock_adapter.register_uri(
        ANY_METHOD, MOCKED_URL, text='updated response', status_code=200
    )

    report = mock_session.revalidate()

    assert len(report.updated) == 1
    assert mock_session.get(MOCKED_URL).text == 'updated response'


def test_revalidate_stale(mock_session):
    """A 404 response should be reported as stale, and the cached response should be kept"""
    mock_session.get(MOCKED_URL_200_404)

    report = mock_session.revalidate()  # Second request returns 404

    assert len(report.stale) == 1
    response = mock_session.get(MOCKED_URL_200_404)
    assert response.from_cache and response.status_code == 200


def test_revalidate_failed_status(mock_session):
    """An unexpected status code should be reported as failed, leaving the cache untouched"""
    mock_session.get(MOCKED_URL)
    mock_session.mock_adapter.register_uri(ANY_METHOD, MOCKED_URL, status_code=500)

    report = mock_session.revalidate()

    assert len(report.failed) == 1
    assert mock_session.get(MOCKED_URL).from_cache


def test_revalidate_failed_request_error(mock_session):
    """A request exception should be reported as failed, leaving the cache untouched"""
    mock_session.get(MOCKED_URL)
    mock_session.mock_adapter.register_uri(ANY_METHOD, MOCKED_URL, exc=requests.ConnectionError)

    report = mock_session.revalidate()

    assert len(report.failed) == 1
    assert isinstance(report.failed[0].error, requests.ConnectionError)
    assert mock_session.get(MOCKED_URL).from_cache


def test_revalidate_partial_failure(mock_session):
    """A failure for one item should not affect results for other items"""
    mock_session.get(MOCKED_URL)
    mock_session.get(MOCKED_URL_ETAG)
    mock_session.mock_adapter.register_uri(ANY_METHOD, MOCKED_URL, exc=requests.ConnectionError)
    mock_session.mock_adapter.register_uri(ANY_METHOD, MOCKED_URL_ETAG, status_code=304)

    report = mock_session.revalidate()

    assert len(report.failed) == 1
    assert len(report.unmodified) == 1


def test_revalidate_vary_variants(mock_session):
    """Vary variants of the same URL should be revalidated separately, using each variant's
    original request headers"""
    mock_session.get(MOCKED_URL_VARY, headers={'Accept': 'text/plain'})
    mock_session.get(MOCKED_URL_VARY, headers={'Accept': 'application/json'})
    assert len(mock_session.cache.responses.keys()) == 2
    mock_session.mock_adapter.register_uri(ANY_METHOD, MOCKED_URL_VARY, status_code=304)

    report = mock_session.revalidate()

    assert len(report.unmodified) == 2
    assert len({r.cache_key for r in report.unmodified}) == 2
    assert {r.url for r in report.unmodified} == {MOCKED_URL_VARY}
    accept_headers = sorted(r.request.headers['Accept'] for r in report.unmodified)
    assert accept_headers == ['application/json', 'text/plain']


def test_revalidate_specific_keys(mock_session):
    """Only the specified cache keys should be revalidated"""
    mock_session.get(MOCKED_URL)
    mock_session.get(MOCKED_URL_ETAG)
    key = next(k for k, r in mock_session.cache.responses.items() if r.url == MOCKED_URL_ETAG)
    mock_session.mock_adapter.register_uri(ANY_METHOD, MOCKED_URL_ETAG, status_code=304)

    report = mock_session.revalidate(keys=[key])

    assert len(report.unmodified) == 1
    assert report.unmodified[0].cache_key == key
    assert len(mock_session.mock_adapter.request_history) == 3


def test_revalidate_skips_response_hooks(mock_session):
    """Revalidation requests should not trigger response hooks"""
    hook_calls = []
    mock_session.hooks['response'].append(lambda r, *args, **kwargs: hook_calls.append(r))
    mock_session.get(MOCKED_URL)
    previous_hook_calls = len(hook_calls)
    assert previous_hook_calls > 0

    mock_session.revalidate()

    assert len(hook_calls) == previous_hook_calls


def test_revalidate_cancelled_before_start(mock_session):
    """A pre-set cancel event should leave all items pending, without sending any requests"""
    mock_session.get(MOCKED_URL)
    mock_session.get(MOCKED_URL_ETAG)
    cancel_event = Event()
    cancel_event.set()
    previous_request_count = len(mock_session.mock_adapter.request_history)

    report = mock_session.revalidate(cancel_event=cancel_event)

    assert report.cancelled
    assert not report.results
    assert len(report.pending) == 2
    assert {r.url for r in report.pending} == {MOCKED_URL, MOCKED_URL_ETAG}
    assert len(mock_session.mock_adapter.request_history) == previous_request_count


def test_revalidate_cancelled_mid_run(mock_session):
    """Cancellation should stop accepting new items but let in-flight requests finish"""
    cancel_event = Event()
    urls = [MOCKED_URL, MOCKED_URL_JSON, MOCKED_URL_ETAG]
    for url in urls:
        mock_session.get(url)

    def callback(request, context):
        cancel_event.set()
        context.status_code = 200
        return 'mock response'

    for url in urls:
        mock_session.mock_adapter.register_uri(ANY_METHOD, url, text=callback)

    report = mock_session.revalidate(max_workers=1, cancel_event=cancel_event)

    assert report.cancelled
    assert len(report.results) >= 1
    assert len(report.pending) >= 1
    assert len(report.results) + len(report.pending) == len(urls)
    assert all(r.status is RevalidationStatus.PENDING for r in report.pending)


def test_revalidate_bounded_concurrency(mock_session):
    """No more than max_workers requests should be in flight at once"""
    active = 0
    max_active = 0
    lock = Lock()
    urls = [f'http+mock://requests-cache.com/concurrent_{i}' for i in range(6)]

    def callback(request, context):
        nonlocal max_active, active
        with lock:
            active += 1
            max_active = max(max_active, active)
        sleep(0.05)
        with lock:
            active -= 1
        context.status_code = 200
        return 'mock response'

    for url in urls:
        mock_session.mock_adapter.register_uri(ANY_METHOD, url, text='mock response')
        mock_session.get(url)
        mock_session.mock_adapter.register_uri(ANY_METHOD, url, text=callback)

    report = mock_session.revalidate(max_workers=2)

    assert len(report.updated) == len(urls)
    assert 1 < max_active <= 2


def test_revalidate_invalid_max_workers(mock_session):
    with pytest.raises(ValueError):
        mock_session.revalidate(max_workers=0)
