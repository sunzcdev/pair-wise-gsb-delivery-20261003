import time_machine
from datetime import datetime, timedelta, timezone
from unittest.mock import MagicMock

from requests import Request

from requests_cache.models import CachedResponse
from requests_cache.policy import (
    CacheActions,
    CacheSettings,
    get_age_adjusted_expiration,
    get_corrected_initial_age,
)


def test_304_keeps_old_body_age():
    t0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)
    url = 'http://example.com/data'
    req = Request(method='GET', url=url).prepare()

    with time_machine.travel(t0):
        cached = CachedResponse(
            url=url,
            status_code=200,
            headers={
                'ETag': '"v1"',
                'Cache-Control': 'max-age=600',
                'Age': '300',
                'Date': 'Sun, 15 Nov 2026 12:00:00 GMT',
            },
            age_at_creation=300,
            created_at=t0,
            expires=t0 + timedelta(seconds=300),
        )
        assert cached.age == 300
        assert cached.expires_delta == 300

    with time_machine.travel(t0 + timedelta(seconds=600)):
        settings = CacheSettings(cache_control=True)
        actions = CacheActions.from_request('k', req, settings)
        not_modified = MagicMock(
            status_code=304,
            request=req,
            headers={
                'ETag': '"v1"',
                'Cache-Control': 'max-age=600',
                'Date': 'Sun, 15 Nov 2026 12:10:00 GMT',
                'Age': '0',
            },
        )
        actions.update_from_response(not_modified)
        assert actions.expire_after == 600
        updated = actions.update_revalidated_response(
            not_modified, CachedResponse.from_response(cached)
        )

        # The 304 carries no body: the old content's age must not reset to zero
        # Total body age = 300 upstream + 600 local residence
        assert updated.age == 900
        assert updated.age_at_creation == 900
        # New max-age=600 cannot revive content already 900s old: expires is revalidation time
        # (12:10) + 600 - 900 = 12:05, i.e. the response is already stale again
        assert updated.expires == t0 + timedelta(seconds=300)
        assert updated.expires_delta == 600 - 900
        assert updated.is_expired is True
        assert updated.created_at == t0 + timedelta(seconds=600)

        # As local time advances, the preserved age keeps accruing
        with time_machine.travel(t0 + timedelta(seconds=600 + 601)):
            assert updated.age == 900 + 601
            assert updated.is_expired is True


def test_304_with_upstream_age_uses_max_age():
    t0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)
    url = 'http://example.com/data'
    req = Request(method='GET', url=url).prepare()

    with time_machine.travel(t0):
        cached = CachedResponse(
            url=url,
            status_code=200,
            headers={'ETag': '"v1"', 'Date': 'Sun, 15 Nov 2026 12:00:00 GMT'},
            age_at_creation=100,
            created_at=t0,
            expires=t0 + timedelta(seconds=200),
        )

    with time_machine.travel(t0 + timedelta(seconds=300)):
        settings = CacheSettings(cache_control=True)
        actions = CacheActions.from_request('k', req, settings)
        # 304 claims the response is already 500s old upstream
        not_modified = MagicMock(
            status_code=304,
            request=req,
            headers={
                'ETag': '"v1"',
                'Cache-Control': 'max-age=600',
                'Date': 'Sun, 15 Nov 2026 12:05:00 GMT',
                'Age': '500',
            },
        )
        actions.update_from_response(not_modified)
        updated = actions.update_revalidated_response(
            not_modified, CachedResponse.from_response(cached)
        )
        # max(old current age 400, 304 initial age 500) = 500
        assert updated.age_at_creation == 500
        assert updated.age == 500
        # max-age=600 minus age 500 -> only 100s of freshness remain
        assert updated.expires_delta == 100


def test_age_adjustment_basic():
    t0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)
    headers = {
        'Cache-Control': 'max-age=600',
        'Age': '300',
        'Date': 'Sun, 15 Nov 2026 12:00:00 GMT',
    }
    expires = get_age_adjusted_expiration(headers, 600, received_at=t0)
    assert expires == t0 + timedelta(seconds=300)
    assert get_corrected_initial_age(headers, t0) == 300

