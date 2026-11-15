from unittest.mock import patch

import pytest
from requests import Request, Session as OriginalSession
from requests.exceptions import RequestException

from requests_cache import CachePlan, CachePolicyError, DO_NOT_CACHE
from tests.conftest import MOCKED_URL, MOCKED_URL_ETAG


def prepare(url=MOCKED_URL, headers=None):
    return Request('GET', url, headers=headers or {}).prepare()


def test_cache_plan__default(mock_session):
    """By default, all cache operations are allowed, and a new request will be sent"""
    plan = mock_session.cache_plan(prepare())
    assert isinstance(plan, CachePlan)
    assert plan.read_cache is True
    assert plan.contact_origin is True
    assert plan.write_cache is True
    assert plan.send_request is True
    assert plan.error_504 is False
    assert plan.cache_key


def test_cache_plan__cached(mock_session):
    """For a fresh cached response, no request will be sent"""
    mock_session.get(MOCKED_URL)
    plan = mock_session.cache_plan(prepare())
    assert plan.read_cache is True
    assert plan.send_request is False
    assert plan.resend_request is False
    assert plan.resend_async is False


def test_cache_plan__only_if_cached(mock_session):
    """only_if_cached forbids contacting the origin server, and results in a 504 if uncached"""
    plan = mock_session.cache_plan(prepare(), only_if_cached=True)
    assert plan.read_cache is True
    assert plan.contact_origin is False
    assert plan.error_504 is True
    assert any('only-if-cached' in reason for reason in plan.reasons)

    # With a usable cached response, no 504 and still no origin contact
    mock_session.get(MOCKED_URL)
    plan = mock_session.cache_plan(prepare(), only_if_cached=True)
    assert plan.contact_origin is False
    assert plan.error_504 is False


def test_cache_plan__only_if_cached__via_settings(mock_session):
    mock_session.settings.only_if_cached = True
    plan = mock_session.cache_plan(prepare())
    assert plan.contact_origin is False
    assert plan.error_504 is True


def test_cache_plan__only_if_cached__precedence_over_refresh(mock_session):
    """only_if_cached takes precedence over refresh: no revalidation is performed"""
    mock_session.get(MOCKED_URL)
    plan = mock_session.cache_plan(prepare(), only_if_cached=True, refresh=True)
    assert plan.contact_origin is False
    assert plan.resend_request is False
    assert plan.resend_async is False


def test_cache_plan__refresh(mock_session):
    """refresh revalidates with the server before using a cached response"""
    mock_session.get(MOCKED_URL_ETAG, expire_after=60)
    plan = mock_session.cache_plan(prepare(MOCKED_URL_ETAG), refresh=True)
    assert plan.read_cache is True
    assert plan.contact_origin is True
    assert plan.write_cache is True
    assert plan.resend_request is True
    assert any('refresh' in reason for reason in plan.reasons)


def test_cache_plan__force_refresh(mock_session):
    """force_refresh skips the cache read, always sends a request, and overwrites the cache"""
    mock_session.get(MOCKED_URL)
    plan = mock_session.cache_plan(prepare(), force_refresh=True)
    assert plan.read_cache is False
    assert plan.contact_origin is True
    assert plan.write_cache is True
    assert plan.send_request is True


def test_cache_plan__force_refresh__precedence_over_refresh(mock_session):
    """force_refresh takes precedence over refresh (a hard refresh implies a soft one)"""
    mock_session.get(MOCKED_URL)
    plan = mock_session.cache_plan(prepare(), refresh=True, force_refresh=True)
    assert plan.read_cache is False
    assert plan.send_request is True


def test_cache_plan__no_store(mock_session):
    """no-store forbids both reading from and writing to the cache"""
    plan = mock_session.cache_plan(prepare(headers={'Cache-Control': 'no-store'}))
    assert plan.read_cache is False
    assert plan.contact_origin is True
    assert plan.write_cache is False
    assert any('no-store' in reason for reason in plan.reasons)


def test_cache_plan__do_not_cache(mock_session):
    plan = mock_session.cache_plan(prepare(), expire_after=DO_NOT_CACHE)
    assert plan.read_cache is False
    assert plan.write_cache is False


def test_cache_plan__read_only(mock_session):
    mock_session.settings.read_only = True
    plan = mock_session.cache_plan(prepare())
    assert plan.read_cache is True
    assert plan.write_cache is False
    assert any('read_only' in reason for reason in plan.reasons)


def test_cache_plan__disabled(mock_session):
    mock_session.settings.disabled = True
    plan = mock_session.cache_plan(prepare())
    assert plan.read_cache is False
    assert plan.contact_origin is True
    assert plan.write_cache is False


def test_cache_plan__conflict(mock_session):
    """only_if_cached + force_refresh is contradictory and rejected with an explainable reason"""
    with pytest.raises(CachePolicyError, match='only-if-cached'):
        mock_session.cache_plan(prepare(), only_if_cached=True, force_refresh=True)


def test_cache_plan__conflict__via_headers(mock_session):
    """The same conflict is detected when expressed via Cache-Control request headers"""
    with pytest.raises(CachePolicyError):
        mock_session.cache_plan(prepare(headers={'Cache-Control': 'no-cache, only-if-cached'}))


def test_cache_plan__conflict__via_settings(mock_session):
    """The same conflict is detected across sources (session setting + per-request option)"""
    mock_session.settings.only_if_cached = True
    with pytest.raises(CachePolicyError):
        mock_session.cache_plan(prepare(), force_refresh=True)


def test_cache_plan__error_is_not_a_network_error():
    """Decision errors are distinguishable from real network failures"""
    assert not issubclass(CachePolicyError, RequestException)


def test_cache_plan__does_not_send(mock_session):
    """Computing a plan never sends a request"""
    with patch.object(OriginalSession, 'send', side_effect=AssertionError('request sent')):
        mock_session.cache_plan(prepare())


def test_cache_plan__does_not_modify_request(mock_session):
    """Computing a plan does not modify the PreparedRequest"""
    request = prepare(headers={'Accept': 'application/json'})
    headers_before = dict(request.headers)
    mock_session.cache_plan(request, only_if_cached=True, expire_after=60)
    assert dict(request.headers) == headers_before
    assert 'Cache-Control' not in request.headers


def test_cache_plan__does_not_modify_cache(mock_session):
    """Computing a plan does not add, update, or delete cached responses"""
    mock_session.get(MOCKED_URL)
    cached_before = next(mock_session.cache.filter())
    plan = mock_session.cache_plan(prepare(), force_refresh=True)
    assert plan.write_cache is True
    cached_after = mock_session.cache.get_response(plan.cache_key)
    assert cached_after is not None
    assert cached_after.created_at == cached_before.created_at
    assert len(mock_session.cache.urls()) == 1

    mock_session.cache_plan(prepare('http+mock://requests-cache.com/uncached'))
    assert len(mock_session.cache.urls()) == 1
