import time_machine
from datetime import datetime, timedelta, timezone
from unittest.mock import MagicMock

from requests import Request

from requests_cache.models import CachedResponse
from requests_cache.policy import CacheActions, CacheSettings


T0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)
URL = 'https://img.site.com/base/img.jpg'


def test_stale_while_revalidate_window_uses_current_age():
    request = Request(method='GET', url=URL).prepare()
    # max-age=100 but response already 80s old on arrival -> only 20s fresh locally
    cached = CachedResponse(
        headers={'Cache-Control': 'stale-while-revalidate=30'},
        age_at_creation=80,
        created_at=T0,
        expires=T0 + timedelta(seconds=20),
    )
    settings = CacheSettings(stale_while_revalidate=30)

    with time_machine.travel(T0 + timedelta(seconds=25)):
        actions = CacheActions.from_request('key', request, settings)
        actions.update_from_cached_response(cached)
        # 5s into the 30s stale window -> still usable
        assert actions.is_usable(cached) is True

    with time_machine.travel(T0 + timedelta(seconds=51)):
        actions = CacheActions.from_request('key', request, settings)
        actions.update_from_cached_response(cached)
        # 31s past expiry -> outside stale-while-revalidate window
        assert actions.is_usable(cached) is False


def test_min_fresh_against_aged_response():
    request = Request(
        method='GET', url=URL, headers={'Cache-Control': 'min-fresh=25'}
    ).prepare()
    cached = CachedResponse(
        age_at_creation=80,
        created_at=T0,
        expires=T0 + timedelta(seconds=20),
    )
    actions = CacheActions.from_request('key', request, CacheSettings())
    with time_machine.travel(T0):
        # Only 20s remains but caller demands 25s of guaranteed freshness -> not usable
        assert actions.is_usable(cached) is False


def test_transfer_encoding_preserved_on_304():
    request = Request(method='GET', url=URL).prepare()
    actions = CacheActions.from_request('key', request, CacheSettings(cache_control=True))
    cached = CachedResponse(
        headers={'ETag': '"v1"', 'Transfer-Encoding': 'chunked'}
    )
    not_modified = MagicMock(
        status_code=304,
        headers={'ETag': '"v1"', 'Content-Length': 0},
        request=request,
    )
    result = actions.update_revalidated_response(not_modified, cached)
    assert result.headers['Transfer-Encoding'] == 'chunked'
    assert 'Content-Length' not in result.headers

