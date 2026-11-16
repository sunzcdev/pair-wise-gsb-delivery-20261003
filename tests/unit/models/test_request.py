from requests.cookies import RequestsCookieJar
from requests.utils import default_headers

from requests_cache.models.response import CachedRequest
from tests.conftest import MOCKED_URL


def test_from_request(mock_session):
    response = mock_session.get(MOCKED_URL, data=b'mock request', headers={'foo': 'bar'})
    request = CachedRequest.from_request(response.request)
    expected_headers = {**default_headers(), 'Content-Length': '12', 'foo': 'bar'}

    assert response.request.body == request.body == b'mock request'
    assert response.request.headers == request.headers == expected_headers
    assert response.request.method == request.method == 'GET'
    assert response.request.path_url == request.path_url == '/text'
    assert response.request.url == request.url == MOCKED_URL
    assert response.request._cookies == request._cookies == request.cookies == {}


def test_from_request__isolated(mock_session):
    """A CachedRequest should not share mutable attributes with the original request"""
    response = mock_session.get(MOCKED_URL)
    response.request.headers['foo'] = 'bar'
    response.request._cookies.set('foo', 'bar')
    request = CachedRequest.from_request(response.request)

    request.headers['foo'] = 'modified'
    request.cookies.set('foo', 'modified')

    assert response.request.headers['foo'] == 'bar'
    assert response.request._cookies['foo'] == 'bar'


def test_copy():
    request = CachedRequest(
        body=b'mock request', headers={'foo': 'bar'}, method='GET', url=MOCKED_URL
    )
    request.cookies.set('foo', 'bar')
    request_copy = request.copy()
    request_copy.headers['foo'] = 'modified'
    request_copy.cookies.set('foo', 'modified')

    assert request_copy.body == request.body
    assert request_copy.headers['foo'] == 'modified'
    assert request_copy.cookies['foo'] == 'modified'
    assert request.headers['foo'] == 'bar'
    assert request.cookies['foo'] == 'bar'


def test_prepare__isolated():
    """Preparing a request should not share headers or cookies with the CachedRequest"""
    request = CachedRequest(headers={'foo': 'bar'}, method='GET', url=MOCKED_URL)
    request.cookies.set('foo', 'bar')
    prepared_request = request.prepare()
    prepared_request.headers['foo'] = 'modified'
    prepared_request._cookies.set('foo', 'modified')

    assert request.headers['foo'] == 'bar'
    assert request.cookies['foo'] == 'bar'


def test_prepare__cookies():
    """Cookies should be preserved when converting to and from a CachedRequest"""
    cookies = RequestsCookieJar()
    cookies.set('foo', 'bar')
    request = CachedRequest(cookies=cookies, method='GET', url=MOCKED_URL)

    assert request.prepare()._cookies == cookies


def test_prepare__repeated():
    """Each PreparedRequest should get its own copy of the request data, so temporary changes to
    one prepared request don't get written back to the CachedRequest
    """
    request = CachedRequest(headers={'foo': 'bar'}, method='GET', url=MOCKED_URL)
    prepared_request_1 = request.prepare()
    prepared_request_2 = request.prepare()
    prepared_request_1.headers['foo'] = 'modified'

    assert prepared_request_2.headers['foo'] == 'bar'
    assert request.headers['foo'] == 'bar'
