from io import BytesIO

from requests_cache.models import CachedHTTPResponse
from tests.conftest import MOCKED_URL


def test_from_response(mock_session):
    response = mock_session.get(MOCKED_URL)
    response.raw._fp = BytesIO(b'mock response')
    response.raw._request_url = MOCKED_URL
    raw = CachedHTTPResponse.from_response(response)

    assert dict(response.raw.headers) == dict(raw.headers) == {'Content-Type': 'text/plain'}
    assert raw.read(None) == b'mock response'
    assert response.raw.decode_content is raw.decode_content is False
    assert response.raw.reason is raw.reason is None
    if hasattr(response.raw, '_request_url'):
        assert response.raw._request_url == raw._request_url == MOCKED_URL
    assert response.raw.status == raw.status == 200
    assert response.raw.version == raw.version == 0


def test_read():
    raw = CachedHTTPResponse(body=b'mock response')
    assert raw.read(10) == b'mock respo'
    assert raw.read(None) == b'nse'
    assert raw.read(1) == b''
    assert raw._fp.closed is True


def test_read__always_decode():
    """read() with decode_content=False is not supported"""
    raw = CachedHTTPResponse(body=b'mock response', headers={'Content-Encoding': 'gzip'})
    assert raw.read(decode_content=False) == b'mock response'


def test_close():
    raw = CachedHTTPResponse(body=b'mock response')
    raw.close()
    assert raw._fp.closed is True


def test_reset():
    raw = CachedHTTPResponse(body=b'mock response')
    raw.read(None)
    assert raw.read(1) == b''
    assert raw._fp.closed is True

    raw.reset()
    assert raw.read(None) == b'mock response'


def test_reset__independent_read_position():
    """Each raw response should have its own read position, without copying the body"""
    body = b'mock response'
    raw_1 = CachedHTTPResponse(body=body)
    raw_2 = CachedHTTPResponse(body=body)

    assert raw_1._body is raw_2._body is body
    assert raw_1._fp is not raw_2._fp
    assert raw_1.read(4) == b'mock'
    assert raw_2.read(4) == b'mock'


def test_stream():
    raw = CachedHTTPResponse(body=b'mock response')
    data = b''
    for chunk in raw.stream(1):
        data += chunk
    assert data == b'mock response'
    assert raw._fp.closed


def test_from_response__headers_isolated(mock_session):
    """A raw cached response should not share headers with the original raw response"""
    response = mock_session.get(MOCKED_URL)
    raw = CachedHTTPResponse.from_response(response)
    response.raw.headers['X-Custom'] = 'true'

    assert 'X-Custom' not in raw.headers
