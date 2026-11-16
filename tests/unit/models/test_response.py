from datetime import datetime, timedelta
from io import BytesIO
from json import loads
from time import sleep

import pytest
from requests import Response
from urllib3.response import HTTPResponse

from requests_cache.models import CachedRequest, CachedResponse
from requests_cache.models.response import OriginalResponse, format_file_size
from requests_cache.policy import CacheActions, utcnow
from tests.conftest import MOCKED_URL


def test_basic_attrs(mock_session):
    response = CachedResponse.from_response(mock_session.get(MOCKED_URL))

    assert response.from_cache is True
    assert response.url == MOCKED_URL
    assert response.status_code == 200
    assert response.reason is None
    assert response.encoding == 'ISO-8859-1'
    assert response.headers['Content-Type'] == 'text/plain'
    assert response.text == 'mock response'
    assert isinstance(response.created_at, datetime)
    assert response.expires is None
    assert response.is_expired is False


@pytest.mark.parametrize(
    'response',
    [
        OriginalResponse(),
        OriginalResponse.wrap_response(Response(), CacheActions()),
    ],
)
def test_original_response(response):
    """Test both instantiating directly and wrapping a `requests.Response`"""
    response = OriginalResponse()
    assert response._content is False
    assert response._content_consumed is False
    assert response.status_code is None
    assert isinstance(response.created_at, datetime)
    assert response.expires is None
    assert response.revalidated is False
    assert response.cache_key == ''


def test_update_content_changed__without_cached_response():
    response = OriginalResponse()

    response.update_content_changed(None)

    assert response.has_content_changed is None
    assert response._content_consumed is False


@pytest.mark.parametrize('content, changed', [(b'not json', False), (b'other invalid json', True)])
def test_update_content_changed__legacy_json_error(content, changed, monkeypatch):
    # Requests <2.27 lets the standard-library JSONDecodeError propagate.
    monkeypatch.setattr(Response, 'json', lambda self: loads(self.text))
    cached_response = CachedResponse(
        content=b'not json', headers={'Content-Type': 'application/json'}
    )
    response = OriginalResponse()
    response._content = content
    response._content_consumed = True
    response.headers['Content-Type'] = 'application/json'

    response.update_content_changed(cached_response)

    assert response.has_content_changed is changed


def test_history(mock_session):
    original_response = mock_session.get(MOCKED_URL)
    original_response.history = [mock_session.get(MOCKED_URL)] * 3
    response = CachedResponse.from_response(original_response)
    assert len(response.history) == 3
    assert all(isinstance(r, CachedResponse) for r in response.history)


def test_from_response__isolated(mock_session):
    """A cached response should not share mutable attributes with the original response"""
    original_response = mock_session.get(MOCKED_URL)
    response = CachedResponse.from_response(original_response)
    original_response.headers['X-Hook'] = '1'
    original_response.cookies.set('hook', '1')
    original_response.request.headers['X-Hook'] = '1'

    assert 'X-Hook' not in response.headers
    assert 'hook' not in response.cookies
    assert 'X-Hook' not in response.request.headers


def test_copy__isolated(mock_session):
    """Copies of a cached response should not share mutable attributes with the original, either
    directly or via its history
    """
    response = CachedResponse.from_response(mock_session.get(MOCKED_URL))
    response.headers['X-Hook'] = '1'
    response.cookies.set('hook', '1')
    response.request.headers['X-Hook'] = '1'
    response.history = [CachedResponse.from_response(mock_session.get(MOCKED_URL))]
    response.history[0].headers['X-Hook'] = '1'
    response.history[0].cookies.set('hook', '1')
    response.history[0].request.headers['X-Hook'] = '1'

    response_copy = response.copy()
    response_copy.headers['X-Hook'] = '2'
    response_copy.cookies.set('hook', '2')
    response_copy.request.headers['X-Hook'] = '2'
    response_copy.history[0].headers['X-Hook'] = '2'
    response_copy.history[0].cookies.set('hook', '2')
    response_copy.history[0].request.headers['X-Hook'] = '2'

    assert response.headers['X-Hook'] == '1'
    assert response.cookies['hook'] == '1'
    assert response.request.headers['X-Hook'] == '1'
    assert response.history[0].headers['X-Hook'] == '1'
    assert response.history[0].cookies['hook'] == '1'
    assert response.history[0].request.headers['X-Hook'] == '1'


def test_copy__body_not_duplicated():
    """Copying a response should share the (immutable) response body instead of duplicating it,
    but each copy should have its own body read position
    """
    response = CachedResponse(content=b'mock response', headers={'Content-Type': 'text/plain'})
    response_copy = response.copy()

    assert response_copy._content is response._content
    assert response_copy.raw is not response.raw
    assert response.raw.read(4) == response_copy.raw.read(4) == b'mock'


def test_copy__raw_position_isolated():
    """Reading one copy of a cached response should not affect any other copy"""
    response = CachedResponse(content=b'mock response', headers={'Content-Type': 'text/plain'})
    response_copy = response.copy()

    assert response_copy.raw.read(None) == b'mock response'
    # The other copy should still be readable from its own position
    assert response.raw.read(None) == b'mock response'


def test_next__isolated():
    """The prepared request for a redirect's next request should not share mutable data with the
    CachedRequest stored in the cache, so it can't be written back to it
    """
    next_request = CachedRequest(headers={'foo': 'bar'}, method='GET', url=MOCKED_URL)
    next_request.cookies.set('foo', 'bar')
    response = CachedResponse(next=next_request)

    prepared_request = response.next
    prepared_request.headers['foo'] = 'modified'
    prepared_request._cookies.set('foo', 'modified')

    assert response._next.headers['foo'] == 'bar'
    assert response._next.cookies['foo'] == 'bar'
    # A second call should also get the original values
    assert response.next.headers['foo'] == 'bar'
    assert response.next._cookies['foo'] == 'bar'


@pytest.mark.parametrize(
    'expires, is_expired',
    [
        (utcnow() + timedelta(days=1), False),
        (utcnow() - timedelta(days=1), True),
    ],
)
def test_is_expired(expires, is_expired, mock_session):
    response = CachedResponse.from_response(mock_session.get(MOCKED_URL), expires=expires)
    assert response.from_cache is True
    assert response.is_expired == is_expired


def test_is_expired__timezone_naive(mock_session):
    response = CachedResponse.from_response(
        mock_session.get(MOCKED_URL),
        expires=datetime.now() - timedelta(days=1),
    )
    assert response.is_expired is True


def test_iterator(mock_session):
    # Set up mock response with streamed content
    url = f'{MOCKED_URL}/stream'
    mock_raw_response = HTTPResponse(
        body=BytesIO(b'mock response'),
        status=200,
        request_method='GET',
        decode_content=False,
        preload_content=False,
    )
    mock_session.mock_adapter.register_uri(
        'GET',
        url,
        status_code=200,
        raw=mock_raw_response,
    )

    # Expect the same chunks of data from the original response and subsequent cached responses
    last_request_chunks = None
    for i in range(3):
        response = mock_session.get(url, stream=True)
        chunks = list(response.iter_lines())
        if i == 0:
            assert response.from_cache is False
        else:
            assert response.from_cache is True
            assert chunks == last_request_chunks
        last_request_chunks = chunks


def test_reset_expiration__extend_expiration(mock_session):
    # Start with an expired response
    response = CachedResponse.from_response(
        mock_session.get(MOCKED_URL),
        expires=utcnow() - timedelta(seconds=0.01),
    )
    assert response.is_expired is True

    # Set expiration in the future
    is_expired = response.reset_expiration(utcnow() + timedelta(seconds=0.01))
    assert is_expired is response.is_expired is False
    sleep(0.1)
    assert response.is_expired is True


def test_reset_expiration__shorten_expiration(mock_session):
    # Start with a non-expired response
    response = CachedResponse.from_response(
        mock_session.get(MOCKED_URL),
        expires=utcnow() + timedelta(seconds=1),
    )
    assert response.is_expired is False

    # Set expiration in the past
    is_expired = response.reset_expiration(utcnow() - timedelta(seconds=1))
    assert is_expired is response.is_expired is True


def test_size(mock_session):
    response = CachedResponse.from_response(mock_session.get(MOCKED_URL))
    response._content = None
    assert response.size == 0
    response._content = b'1' * 1024
    assert response.size == 1024


def test_str(mock_session):
    """Just ensure that a subset of relevant attrs get included in the response str; the format
    may change without breaking the test.
    """
    response = CachedResponse.from_response(mock_session.get(MOCKED_URL))
    response._content = b'1010'
    expected_values = ['GET', MOCKED_URL, 200, '4 bytes', 'created', 'expires', 'fresh']
    assert all(str(v) in str(response) for v in expected_values)


def test_repr(mock_session):
    """Just ensure that a subset of relevant attrs get included in the response repr"""
    response = CachedResponse.from_response(mock_session.get(MOCKED_URL))
    expected_values = ['GET', MOCKED_URL, 200, 'ISO-8859-1', response.headers]
    print(repr(response))
    assert repr(response).startswith('CachedResponse(') and repr(response).endswith(')')
    assert all(str(v) in repr(response) for v in expected_values)


@pytest.mark.parametrize(
    'n_bytes, expected_size',
    [
        (None, '0 bytes'),
        (5, '5 bytes'),
        (3 * 1024, '3.00 KiB'),
        (1024 * 3000, '2.93 MiB'),
        (1024 * 1024 * 5000, '4.88 GiB'),
    ],
)
def test_format_file_size(n_bytes, expected_size):
    assert format_file_size(n_bytes) == expected_size
