"""Tests for isolating responses returned from the cache"""

from copy import deepcopy

import pytest
from requests.cookies import RequestsCookieJar

from requests_cache.backends import BaseCache, SQLiteCache
from requests_cache.models import CachedRequest, CachedResponse

BODY = b'0123456789' * 1000


def _make_response() -> CachedResponse:
    redirect = CachedResponse(
        status_code=302,
        url='http://example.com/redirect',
        headers={'Location': 'http://example.com'},
        cookies=RequestsCookieJar(),
        request=CachedRequest(method='GET', url='http://example.com/redirect'),
    )
    return CachedResponse(
        status_code=200,
        url='http://example.com',
        headers={'Content-Type': 'text/plain', 'X-Final': '1'},
        cookies=RequestsCookieJar(),
        request=CachedRequest(
            method='GET',
            url='http://example.com',
            headers={'X-Request': '1'},
        ),
        history=[redirect],
        content=BODY,
    )


@pytest.fixture(params=['memory', 'sqlite'])
def cache(request):
    cache = BaseCache() if request.param == 'memory' else SQLiteCache(':memory:')
    cache.responses['key'] = _make_response()
    yield cache
    cache.close()


def test_get_response_isolation(cache):
    """Cache hits should not share mutable data or raw response read position"""
    response_1 = cache.get_response('key')
    response_2 = cache.get_response('key')

    assert response_1 is not response_2
    assert response_1.headers is not response_2.headers
    assert response_1.cookies is not response_2.cookies
    assert response_1.history is not response_2.history
    assert response_1.history[0] is not response_2.history[0]
    assert response_1.request is not response_2.request
    assert response_1.request.headers is not response_2.request.headers
    assert response_1.raw is not response_2.raw
    assert response_1.raw._fp is not response_2.raw._fp

    response_1.headers['X-Hit'] = '1'
    response_1.cookies.set('hit', '1')
    response_1.history.append(CachedResponse(status_code=299))
    response_1.history[0].headers['X-History'] = '1'
    response_1.request.headers['X-Temporary'] = '1'
    response_1.raw.read(10)

    assert 'X-Hit' not in response_2.headers
    assert 'hit' not in response_2.cookies
    assert len(response_2.history) == 1
    assert 'X-History' not in response_2.history[0].headers
    assert 'X-Temporary' not in response_2.request.headers
    assert response_2.raw.read() == BODY


def test_copy_shares_immutable_body():
    """Copying a response should not copy its body bytes"""
    response = _make_response()
    copied = response.copy()
    deep_copied = deepcopy(response)

    assert copied._content is response._content
    assert copied.raw._body is response._content
    assert deep_copied._content is response._content
    assert deep_copied.raw._body is response._content


def test_request_from_request_isolation():
    """A cached request should not alias mutable request headers or cookies"""
    original_request = CachedRequest(
        method='GET',
        url='http://example.com',
        headers={'X-Request': '1'},
        cookies={'cookie': 'value'},
    ).prepare()
    request = CachedRequest.from_request(original_request)

    assert request.headers is not original_request.headers
    assert request.cookies is not original_request._cookies

    request.headers['X-Temporary'] = '1'
    request.cookies.set('temporary', '1')

    assert 'X-Temporary' not in original_request.headers
    assert 'temporary' not in original_request._cookies
