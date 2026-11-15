"""The cache_keys module is mostly covered indirectly via other tests.
This just contains tests for some extra edge cases not covered elsewhere.
"""

from io import BytesIO
import json

import pytest
from requests import Request, Response
from urllib3 import HTTPResponse
from unittest.mock import patch

from requests_cache.cache_keys import (
    MAX_NORM_BODY_SIZE,
    create_key,
    normalize_headers,
    normalize_request,
    redact_request,
    redact_response,
)
from requests_cache._utils import decode

CACHE_KEY = 'e25f7e6326966e82'


@pytest.mark.parametrize(
    'url, params',
    [
        ('https://example.com?foo=bar&param=1', None),
        ('https://example.com?foo=bar&param=1', {}),
        ('https://example.com/?foo=bar&param=1', {}),
        ('https://example.com?foo=bar&param=1&', {}),
        ('https://example.com?param=1&foo=bar', {}),
        ('https://example.com?param=1', {'foo': 'bar'}),
        ('https://example.com?foo=bar', {'param': '1'}),
        ('https://example.com', {'foo': 'bar', 'param': '1'}),
        ('https://example.com', {'param': '1', 'foo': 'bar'}),
        ('https://example.com', {'foo': 'bar', 'param': 1}),
        ('https://example.com?', {'foo': 'bar', 'param': '1'}),
    ],
)
def test_create_key__normalize_url_params(url, params):
    """All of the above variations should produce the same cache key"""
    request = Request(
        method='GET',
        url=url,
        params=params,
    )
    assert create_key(request) == CACHE_KEY


def test_create_key__normalize_key_only_params():
    request_1 = Request(method='GET', url='https://img.site.com/base/img.jpg?param_1')
    request_2 = Request(method='GET', url='https://img.site.com/base/img.jpg?param_2')
    assert create_key(request_1) != create_key(request_2)

    request_1 = Request(method='GET', url='https://img.site.com/base/img.jpg?k=v&param_1')
    request_2 = Request(method='GET', url='https://img.site.com/base/img.jpg?param_1&k=v')
    assert create_key(request_1) == create_key(request_2)


def test_create_key__normalize_duplicate_params():
    request_1 = Request(method='GET', url='https://img.site.com/base/img.jpg?param_1=a&param_1=b')
    request_2 = Request(method='GET', url='https://img.site.com/base/img.jpg?param_1=a')
    request_3 = Request(method='GET', url='https://img.site.com/base/img.jpg?param_1=b')
    assert create_key(request_1) != create_key(request_2) != create_key(request_3)

    request_1 = Request(
        method='GET', url='https://img.site.com/base/img.jpg?param_1=a&param_1=b&k=v'
    )
    request_2 = Request(
        method='GET',
        url='https://img.site.com/base/img.jpg?param_1=b&param_1=a',
        params={'k': 'v'},
    )
    assert create_key(request_1) == create_key(request_2)


def test_create_key__fips_hash_fallback():
    """Test that if blake2b fails due to FIPS mode, it creates a valid key using a different
    hash function. Fallback on TypeError and ValueError - both are possible in FIPS mode.
    """
    request = Request(method='GET', url='https://example.com')
    key_1 = create_key(request)

    with patch('requests_cache.cache_keys.blake2b') as mock_blake2b:
        mock_blake2b.side_effect = TypeError
        key_2 = create_key(request)
        mock_blake2b.side_effect = ValueError
        key_3 = create_key(request)

    assert key_1 != key_2
    assert key_2 == key_3  # Fallback to the same algorithm


def test_redact_response__escaped_params():
    """Test that redact_response() handles urlescaped request parameters"""
    url = 'https://img.site.com/base/img.jpg?where=code%3D123'
    request = Request(method='GET', url=url).prepare()
    response = Response()
    response.url = url
    response.request = request
    response.raw = HTTPResponse(request_url=url)
    redacted_response = redact_response(response, [])
    assert redacted_response.url == 'https://img.site.com/base/img.jpg?where=code%3D123'
    assert redacted_response.request.url == 'https://img.site.com/base/img.jpg?where=code%3D123'
    assert redacted_response.request.path_url == '/base/img.jpg?where=code%3D123'
    assert (
        redacted_response.raw._request_url == 'https://img.site.com/base/img.jpg?where=code%3D123'
    )
    if hasattr(redacted_response.raw, 'url'):
        assert redacted_response.raw.url == 'https://img.site.com/base/img.jpg?where=code%3D123'


def _mock_response(url, headers=None, body=None):
    request = Request(method='GET', url=url, headers=headers, data=body).prepare()
    response = Response()
    response.url = url
    response.request = request
    response.raw = HTTPResponse(request_url=url)
    return response


def test_redact_response__redacted_parameters():
    """redacted_parameters should be replaced with irreversible placeholders in persisted data"""
    url = 'https://img.site.com/base/img.jpg?token=secret-value-1&param=1'
    response = _mock_response(url, headers={'X-Token': 'secret-value-1'})
    redacted_response = redact_response(response, [], ['token', 'X-Token'])

    assert 'secret-value-1' not in redacted_response.url
    assert 'secret-value-1' not in redacted_response.request.url
    assert 'token=REDACTED-' in redacted_response.request.url
    assert 'param=1' in redacted_response.request.url
    assert redacted_response.request.headers['X-Token'].startswith('REDACTED-')


def test_redact_response__redacted_parameters__distinct_placeholders():
    """Different sensitive values must get different placeholders, and identical values must get
    identical placeholders"""
    url_1 = 'https://img.site.com/base/img.jpg?token=secret-value-1'
    url_2 = 'https://img.site.com/base/img.jpg?token=secret-value-2'
    redacted_1 = redact_response(_mock_response(url_1), [], ['token'])
    redacted_2 = redact_response(_mock_response(url_2), [], ['token'])
    redacted_1_again = redact_response(_mock_response(url_1), [], ['token'])

    assert redacted_1.request.url != redacted_2.request.url
    assert redacted_1.request.url == redacted_1_again.request.url


def test_redact_response__redacted_parameters__history():
    """Sensitive values must not be persisted in redirect history"""
    prev_response = _mock_response('https://img.site.com/redirect?token=secret-value-1')
    response = _mock_response('https://img.site.com/base/img.jpg?token=secret-value-1')
    response.history = [prev_response]

    redacted_response = redact_response(response, [], ['token'])
    assert 'secret-value-1' not in redacted_response.history[0].url
    assert 'secret-value-1' not in redacted_response.history[0].request.url
    assert 'token=REDACTED-' in redacted_response.history[0].request.url


def test_redact_request__json_body():
    request = Request(
        method='POST',
        url='https://img.site.com/base/img.jpg',
        data=b'{"token": "secret-value-1", "param": "1"}',
        headers={'Content-Type': 'application/json'},
    ).prepare()
    redact_request(request, ['token'])
    body = json.loads(request.body)
    assert body['token'].startswith('REDACTED-')
    assert body['param'] == '1'


def test_redact_request__form_body():
    request = Request(
        method='POST',
        url='https://img.site.com/base/img.jpg',
        data={'token': 'secret-value-1', 'param': '1'},
    ).prepare()
    redact_request(request, ['token'])
    body = decode(request.body)
    assert 'secret-value-1' not in body
    assert 'token=REDACTED-' in body
    assert 'param=1' in body


def test_redact_request__no_redacted_parameters():
    request = Request(
        method='GET',
        url='https://img.site.com/base/img.jpg?token=secret-value-1',
    ).prepare()
    assert redact_request(request, None).url == request.url


@pytest.mark.parametrize(
    'content_type',
    [
        'application/json',
        'application/json; charset=utf-8',
        'application/vnd.api+json; charset=utf-8',
        'application/any_string+json',
    ],
)
@pytest.mark.parametrize(
    'data',
    [
        b'{"param_1": "value_1", "param_2": "value_2"}',
        b'["param_3", "param_2", "param_1"',
    ],
)
def test_normalize_request__json_body(data, content_type):
    request = Request(
        method='GET',
        url='https://img.site.com/base/img.jpg',
        data=b'{"param_1": "value_1", "param_2": "value_2"}',
        headers={'Content-Type': content_type},
    )
    norm_request = normalize_request(request, ignored_parameters=['param_2'])
    assert norm_request.body == b'{"param_1": "value_1", "param_2": "REDACTED"}'


def test_normalize_request__json_body_list_filtered():
    request = Request(
        method='GET',
        url='https://img.site.com/base/img.jpg',
        data=b'["param_3", "param_2", "param_1"]',
        headers={'Content-Type': 'application/json'},
    )
    norm_request = normalize_request(request, ignored_parameters=['param_2', 'param_1'])
    assert norm_request.body == b'["param_3"]'


def test_normalize_request__json_body_invalid():
    request = Request(
        method='GET',
        url='https://img.site.com/base/img.jpg',
        data=b'invalid JSON!',
        headers={'Content-Type': 'application/json'},
    )
    assert normalize_request(request, ignored_parameters=['param_2']).body == b'invalid JSON!'


def test_normalize_request__json_body_empty():
    request = Request(
        method='GET',
        url='https://img.site.com/base/img.jpg',
        data=b'{}',
        headers={'Content-Type': 'application/json'},
    )
    assert normalize_request(request, ignored_parameters=['param_2']).body == b'{}'


@pytest.mark.parametrize(
    'content_type',
    ['application/octet-stream', None],
)
def test_normalize_request__binary_body(content_type):
    request = Request(
        method='GET',
        url='https://img.site.com/base/img.jpg',
        data=b'some bytes',
        headers={'Content-Type': content_type},
    )
    assert normalize_request(request, ignored_parameters=['param']).body == request.data


def test_normalize_request__oversized_body():
    body = {'param': '1', 'content': '0' * MAX_NORM_BODY_SIZE}
    encoded_body = json.dumps(body).encode('utf-8')

    request = Request(
        method='GET',
        url='https://img.site.com/base/img.jpg',
        json=body,
        headers={'Content-Type': 'application/octet-stream'},
    )
    assert normalize_request(request, ignored_parameters=['param']).body == encoded_body


def test_normalize_request__file_like_body():
    original_body = BytesIO(b'some bytes')
    request = Request(
        method='GET',
        url='https://img.site.com/base/img.jpg',
        data=original_body,
        headers={'Content-Type': 'application/json'},
    )
    assert normalize_request(request).body == b'some bytes'
    assert original_body.read() == b'some bytes'


def test_normalize_request__file_like_reset_fails():
    """Test a request body with a file-like class that doesn't support seek()"""

    class CustomBytesIO:
        def __init__(self, content):
            self.content = content

        def __len__(self):
            return len(self.content)

        def read(self):
            return self.content

    original_body = CustomBytesIO(b'some bytes')
    request = Request(
        method='GET',
        url='https://img.site.com/base/img.jpg',
        data=original_body,
        headers={'Content-Type': 'application/json'},
    )
    assert normalize_request(request).body == b'some bytes'
    assert original_body.read() == b'some bytes'


def test_normalize_headers__single_header_value_as_bytes():
    norm_headers = normalize_headers({'Accept': b'gzip'})
    assert norm_headers == {'Accept': 'gzip'}


def test_normalize_headers__multiple_header_values_as_bytes():
    headers = {'Accept': b'gzip,  deflate,Venmo,  PayPal, '}
    norm_headers = normalize_headers(headers)
    assert norm_headers == {'Accept': 'deflate, gzip, paypal, venmo'}


def test_normalize_headers__single_header_value_as_string():
    norm_headers = normalize_headers({'Accept': 'gzip'})
    assert norm_headers == {'Accept': 'gzip'}


def test_normalize_headers__multiple_header_values_as_string():
    headers = {'Accept': 'gzip,  deflate,Venmo,  PayPal, '}
    norm_headers = normalize_headers(headers)
    assert norm_headers == {'Accept': 'deflate, gzip, paypal, venmo'}


def test_remove_ignored_headers__empty():
    request = Request(
        method='GET',
        url='https://img.site.com/base/img.jpg',
        headers={'foo': 'bar'},
    )
    assert normalize_request(request.prepare(), ignored_parameters=None).headers == request.headers
