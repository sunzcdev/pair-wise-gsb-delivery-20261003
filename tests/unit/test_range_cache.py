"""Tests for caching behavior of Range requests and 206 (Partial Content) responses"""

import os
import tempfile
from uuid import uuid4

import pytest
from requests_mock import Adapter

from requests_cache import CachedSession

CONTENT = b'0123456789'
ETAG = '"range-etag-v1"'
LAST_MODIFIED = 'Thu, 05 Jul 2012 15:31:30 GMT'

RANGE_URL = 'http+mock://requests-cache.com/range'
RANGE_URL_UNKNOWN_TOTAL = 'http+mock://requests-cache.com/range-unknown-total'
RANGE_URL_INVALID_CR = 'http+mock://requests-cache.com/range-invalid-cr'
RANGE_URL_NO_VALIDATOR = 'http+mock://requests-cache.com/range-no-validator'
RANGE_URL_IGNORES_RANGE = 'http+mock://requests-cache.com/range-ignored'


def _parse_single_range(range_header):
    # For a multi-range request, the mock server just responds with the first range
    first_range = range_header.replace('bytes=', '').split(',')[0]
    start_s, _, end_s = first_range.partition('-')
    if start_s and end_s:
        return int(start_s), int(end_s)
    elif start_s:
        return int(start_s), len(CONTENT) - 1
    else:
        return len(CONTENT) - int(end_s), len(CONTENT) - 1


def _range_callback(request, context):
    """Mock server with Range and If-Range support"""
    context.headers['ETag'] = ETAG
    if_range = request.headers.get('If-Range')
    range_header = request.headers.get('Range')
    if range_header and (not if_range or if_range == ETAG):
        start, end = _parse_single_range(range_header)
        context.status_code = 206
        context.headers['Content-Range'] = f'bytes {start}-{end}/{len(CONTENT)}'
        return CONTENT[start : end + 1]
    # No Range, or If-Range didn't match: send the full response
    context.status_code = 200
    return CONTENT


def _unknown_total_callback(request, context):
    context.status_code = 206
    context.headers['ETag'] = ETAG
    context.headers['Content-Range'] = 'bytes 0-3/*'
    return CONTENT[0:4]


def _invalid_content_range_callback(request, context):
    context.status_code = 206
    context.headers['ETag'] = ETAG
    context.headers['Content-Range'] = 'garbage'
    return CONTENT[0:4]


def _no_validator_callback(request, context):
    context.status_code = 206
    context.headers['Content-Range'] = f'bytes 0-3/{len(CONTENT)}'
    return CONTENT[0:4]


def _full_response_callback(request, context):
    """Mock server that ignores Range headers"""
    context.status_code = 200
    context.headers['ETag'] = ETAG
    return CONTENT


def _make_session(cache_name, range_policy):
    session = CachedSession(cache_name=cache_name, backend='sqlite', range_policy=range_policy)
    adapter = Adapter()
    adapter.register_uri('GET', RANGE_URL, content=_range_callback)
    adapter.register_uri('GET', RANGE_URL_UNKNOWN_TOTAL, content=_unknown_total_callback)
    adapter.register_uri('GET', RANGE_URL_INVALID_CR, content=_invalid_content_range_callback)
    adapter.register_uri('GET', RANGE_URL_NO_VALIDATOR, content=_no_validator_callback)
    adapter.register_uri('GET', RANGE_URL_IGNORES_RANGE, content=_full_response_callback)
    session.mount('http+mock://', adapter)
    return session


@pytest.fixture(scope='function')
def db_path():
    """Get a unique tempfile path for a SQLite cache db"""
    yield os.path.join(tempfile.gettempdir(), f'{uuid4()}.db')


@pytest.fixture(scope='function')
def isolate_session(db_path):
    yield _make_session(db_path, range_policy='isolate')


@pytest.fixture(scope='function')
def range_session(db_path):
    yield _make_session(db_path, range_policy='cache')


class TestIsolatePolicy:
    """Default policy: range responses are isolated from the cache entirely"""

    def test_206_not_cached(self, isolate_session):
        response = isolate_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        assert response.status_code == 206
        assert response.content == CONTENT[0:4]
        assert response.from_cache is False
        assert len(isolate_session.cache.responses) == 0

    def test_range_request_does_not_read_full_entry(self, isolate_session):
        isolate_session.get(RANGE_URL)
        response = isolate_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        assert response.from_cache is False
        assert response.status_code == 206

    def test_interleaved_full_and_range_reads(self, isolate_session):
        """Full reads and different ranges of the same resource must not share cache entries"""
        full = isolate_session.get(RANGE_URL)
        range_1 = isolate_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        range_2 = isolate_session.get(RANGE_URL, headers={'Range': 'bytes=4-7'})
        full_again = isolate_session.get(RANGE_URL)

        assert full.status_code == 200 and full.content == CONTENT
        assert range_1.status_code == 206 and range_1.content == CONTENT[0:4]
        assert range_2.status_code == 206 and range_2.content == CONTENT[4:8]
        assert not any(r.from_cache for r in (range_1, range_2))

        # The full-entity cache entry is unaffected by the intermediate range requests
        assert full_again.from_cache is True
        assert full_again.status_code == 200
        assert full_again.content == CONTENT
        assert len(isolate_session.cache.responses) == 1

    def test_ignored_range_cached_as_full_entity(self, isolate_session):
        """A 200 response to a range request (server ignored Range) follows full-entity rules"""
        response = isolate_session.get(RANGE_URL_IGNORES_RANGE, headers={'Range': 'bytes=0-3'})
        assert response.status_code == 200
        assert response.content == CONTENT

        cached = isolate_session.get(RANGE_URL_IGNORES_RANGE)
        assert cached.from_cache is True
        assert cached.status_code == 200
        assert cached.content == CONTENT


class TestCachePolicy:
    """Opt-in policy: exact single-range 206 responses are cached under range-qualified keys"""

    def test_single_range_cached_and_reused(self, range_session):
        response = range_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        assert response.status_code == 206
        assert response.from_cache is False

        cached = range_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        assert cached.from_cache is True
        assert cached.status_code == 206
        assert cached.content == CONTENT[0:4]

    def test_open_and_suffix_ranges(self, range_session):
        for range_header, expected in [('bytes=6-', CONTENT[6:]), ('bytes=-4', CONTENT[-4:])]:
            response = range_session.get(RANGE_URL, headers={'Range': range_header})
            assert response.status_code == 206
            cached = range_session.get(RANGE_URL, headers={'Range': range_header})
            assert cached.from_cache is True
            assert cached.status_code == 206
            assert cached.content == expected

    def test_different_ranges_isolated(self, range_session):
        range_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        range_session.get(RANGE_URL, headers={'Range': 'bytes=4-7'})

        cached_1 = range_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        cached_2 = range_session.get(RANGE_URL, headers={'Range': 'bytes=4-7'})
        assert cached_1.from_cache is True and cached_1.content == CONTENT[0:4]
        assert cached_2.from_cache is True and cached_2.content == CONTENT[4:8]

    def test_interleaved_full_and_range_reads(self, range_session):
        """Full reads and different ranges of the same resource must not share content or status"""
        full = range_session.get(RANGE_URL)
        range_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        range_session.get(RANGE_URL, headers={'Range': 'bytes=4-7'})

        full_cached = range_session.get(RANGE_URL)
        range_1 = range_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        range_2 = range_session.get(RANGE_URL, headers={'Range': 'bytes=4-7'})

        assert full.status_code == 200 and full.content == CONTENT
        assert full_cached.from_cache is True
        assert full_cached.status_code == 200
        assert full_cached.content == CONTENT
        assert range_1.from_cache is True
        assert range_1.status_code == 206
        assert range_1.content == CONTENT[0:4]
        assert range_2.from_cache is True
        assert range_2.status_code == 206
        assert range_2.content == CONTENT[4:8]
        assert len(range_session.cache.responses) == 3

    def test_multi_range_request_rejected(self, range_session):
        response = range_session.get(RANGE_URL, headers={'Range': 'bytes=0-3,6-9'})
        assert response.status_code == 206
        assert response.from_cache is False
        assert len(range_session.cache.responses) == 0

    def test_unknown_total_length_rejected(self, range_session):
        response = range_session.get(RANGE_URL_UNKNOWN_TOTAL, headers={'Range': 'bytes=0-3'})
        assert response.status_code == 206
        assert len(range_session.cache.responses) == 0

    def test_invalid_content_range_rejected(self, range_session):
        response = range_session.get(RANGE_URL_INVALID_CR, headers={'Range': 'bytes=0-3'})
        assert response.status_code == 206
        assert len(range_session.cache.responses) == 0

    def test_missing_validator_rejected(self, range_session):
        response = range_session.get(RANGE_URL_NO_VALIDATOR, headers={'Range': 'bytes=0-3'})
        assert response.status_code == 206
        assert len(range_session.cache.responses) == 0

    def test_if_range_match_uses_cached_range(self, range_session):
        range_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        response = range_session.get(
            RANGE_URL, headers={'Range': 'bytes=0-3', 'If-Range': ETAG}
        )
        assert response.from_cache is True
        assert response.status_code == 206
        assert response.content == CONTENT[0:4]

    def test_if_range_mismatch_fetches_full_response(self, range_session):
        """If-Range mismatch: the full 200 response follows full-entity caching rules"""
        range_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})

        response = range_session.get(
            RANGE_URL, headers={'Range': 'bytes=0-3', 'If-Range': '"stale-etag"'}
        )
        assert response.from_cache is False
        assert response.status_code == 200
        assert response.content == CONTENT

        # The full response is cached under the full-entity key
        full = range_session.get(RANGE_URL)
        assert full.from_cache is True
        assert full.status_code == 200
        assert full.content == CONTENT

        # The previously cached range entry is still intact and separate
        partial = range_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        assert partial.from_cache is True
        assert partial.status_code == 206
        assert partial.content == CONTENT[0:4]

    def test_if_range_date_without_last_modified_does_not_match(self, range_session):
        range_session.get(RANGE_URL, headers={'Range': 'bytes=0-3'})
        response = range_session.get(
            RANGE_URL, headers={'Range': 'bytes=0-3', 'If-Range': LAST_MODIFIED}
        )
        # The cached entry has an ETag but no Last-Modified, so the date can't match
        assert response.from_cache is False
