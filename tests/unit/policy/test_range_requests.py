from unittest.mock import MagicMock

import pytest
from requests import PreparedRequest, Response
from requests.structures import CaseInsensitiveDict

from requests_cache.policy.range_requests import (
    get_range_cache_key,
    if_range_matches,
    is_cacheable_range_response,
    parse_content_range,
    parse_range_header,
)

ETAG = '"abc123"'
LAST_MODIFIED = 'Thu, 05 Jul 2012 15:31:30 GMT'


class TestParseRangeHeader:
    @pytest.mark.parametrize(
        'value, expected',
        [
            ('bytes=0-99', [(0, 99)]),
            ('bytes=100-', [(100, None)]),
            ('bytes=-100', [(None, 100)]),
            ('bytes=0-99, 200-299', [(0, 99), (200, 299)]),
            ('bytes=0-99,200-299', [(0, 99), (200, 299)]),
            ('  bytes=0-99  ', [(0, 99)]),
            ('Bytes=0-99', [(0, 99)]),
        ],
    )
    def test_valid(self, value, expected):
        assert parse_range_header(value) == expected

    @pytest.mark.parametrize(
        'value',
        [
            None,
            '',
            'items=0-99',  # non-bytes unit
            'bytes=',  # no range spec
            'bytes=-',  # empty range spec
            'bytes=abc-def',
            'bytes=0-99-',  # invalid spec
            'bytes=0-99, invalid',
        ],
    )
    def test_invalid(self, value):
        assert parse_range_header(value) is None


class TestParseContentRange:
    @pytest.mark.parametrize(
        'value, expected',
        [
            ('bytes 0-99/200', (0, 99, 200)),
            ('bytes 0-99/*', (0, 99, None)),
            ('bytes 100-199/200', (100, 199, 200)),
            ('Bytes 0-99/200', (0, 99, 200)),
        ],
    )
    def test_valid(self, value, expected):
        assert parse_content_range(value) == expected

    @pytest.mark.parametrize(
        'value',
        [
            None,
            '',
            'garbage',
            'bytes 0-99',  # missing total
            'bytes */200',  # unsatisfied-range form has no start/end
            'items 0-99/200',
        ],
    )
    def test_invalid(self, value):
        assert parse_content_range(value) is None


class TestGetRangeCacheKey:
    def test_differs_from_base_key(self):
        assert get_range_cache_key('base', (0, 99)) != 'base'

    def test_same_range_same_key(self):
        assert get_range_cache_key('base', (0, 99)) == get_range_cache_key('base', (0, 99))

    def test_different_ranges_different_keys(self):
        keys = {
            get_range_cache_key('base', (0, 99)),
            get_range_cache_key('base', (100, 199)),
            get_range_cache_key('base', (100, None)),
            get_range_cache_key('base', (None, 100)),
        }
        assert len(keys) == 4

    def test_different_base_keys(self):
        assert get_range_cache_key('base1', (0, 99)) != get_range_cache_key('base2', (0, 99))


def _make_request(range_header=None):
    request = PreparedRequest()
    request.prepare(method='GET', url='http://example.com/file', headers={})
    if range_header:
        request.headers['Range'] = range_header
    return request


def _make_response(
    status_code=206,
    content_range='bytes 0-3/10',
    content_type='application/octet-stream',
    etag=ETAG,
):
    response = Response()
    response.status_code = status_code
    headers = {'Content-Type': content_type}
    if content_range is not None:
        headers['Content-Range'] = content_range
    if etag:
        headers['ETag'] = etag
    response.headers = CaseInsensitiveDict(headers)
    return response


class TestIsCacheableRangeResponse:
    def test_cacheable(self):
        request = _make_request('bytes=0-3')
        assert is_cacheable_range_response(request, _make_response()) is True

    def test_open_range(self):
        request = _make_request('bytes=6-')
        response = _make_response(content_range='bytes 6-9/10')
        assert is_cacheable_range_response(request, response) is True

    def test_suffix_range(self):
        request = _make_request('bytes=-4')
        response = _make_response(content_range='bytes 6-9/10')
        assert is_cacheable_range_response(request, response) is True

    def test_not_206(self):
        request = _make_request('bytes=0-3')
        assert is_cacheable_range_response(request, _make_response(status_code=200)) is False

    def test_no_range_request_header(self):
        request = _make_request()
        assert is_cacheable_range_response(request, _make_response()) is False

    def test_multi_range_request(self):
        request = _make_request('bytes=0-3, 6-9')
        assert is_cacheable_range_response(request, _make_response()) is False

    def test_multipart_response(self):
        request = _make_request('bytes=0-3')
        response = _make_response(content_type='multipart/byteranges; boundary=x')
        assert is_cacheable_range_response(request, response) is False

    def test_unknown_total_length(self):
        request = _make_request('bytes=0-3')
        response = _make_response(content_range='bytes 0-3/*')
        assert is_cacheable_range_response(request, response) is False

    @pytest.mark.parametrize(
        'content_range',
        [
            None,  # missing
            'garbage',  # invalid
            'bytes 3-0/10',  # start > end
            'bytes 0-10/10',  # end >= total
            'bytes 4-7/10',  # doesn't match requested range
        ],
    )
    def test_invalid_content_range(self, content_range):
        request = _make_request('bytes=0-3')
        response = _make_response(content_range=content_range)
        assert is_cacheable_range_response(request, response) is False

    def test_missing_validator(self):
        request = _make_request('bytes=0-3')
        response = _make_response(etag=None)
        assert is_cacheable_range_response(request, response) is False

    def test_last_modified_is_valid_validator(self):
        request = _make_request('bytes=0-3')
        response = _make_response(etag=None)
        response.headers['Last-Modified'] = LAST_MODIFIED
        assert is_cacheable_range_response(request, response) is True


class TestIfRangeMatches:
    def test_etag_match(self):
        assert if_range_matches(ETAG, CaseInsensitiveDict({'ETag': ETAG})) is True

    def test_etag_mismatch(self):
        assert if_range_matches('"other"', CaseInsensitiveDict({'ETag': ETAG})) is False

    def test_weak_etag_never_matches(self):
        assert if_range_matches(f'W/{ETAG}', CaseInsensitiveDict({'ETag': f'W/{ETAG}'})) is False

    def test_date_match(self):
        headers = CaseInsensitiveDict({'Last-Modified': LAST_MODIFIED})
        assert if_range_matches(LAST_MODIFIED, headers) is True

    def test_date_mismatch(self):
        headers = CaseInsensitiveDict({'Last-Modified': LAST_MODIFIED})
        assert if_range_matches('Thu, 05 Jul 2012 15:31:31 GMT', headers) is False

    def test_missing_headers(self):
        assert if_range_matches(ETAG, CaseInsensitiveDict()) is False
        assert if_range_matches(LAST_MODIFIED, CaseInsensitiveDict()) is False

    def test_invalid_date(self):
        headers = CaseInsensitiveDict({'Last-Modified': LAST_MODIFIED})
        assert if_range_matches('not-a-date', headers) is False
