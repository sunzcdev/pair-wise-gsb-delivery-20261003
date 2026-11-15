import time_machine
from datetime import datetime, timedelta, timezone
from unittest.mock import MagicMock

from requests import Request

from requests_cache.models import CachedResponse
from requests_cache.policy import CacheActions, CacheSettings


T0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)
URL = 'https://img.site.com/base/img.jpg'


def test_304_without_date_or_age_preserves_stored_age():
    request = Request(method='GET', url=URL).prepare()
    with time_machine.travel(T0):
        cached = CachedResponse(
            url=URL,
            status_code=200,
            headers={'ETag': '"v1"'},
            age_at_creation=200,
            created_at=T0,
            expires=T0,
        )

    with time_machine.travel(T0 + timedelta(seconds=300)):
        actions = CacheActions.from_request('k', request, CacheSettings(cache_control=True))
        not_modified = MagicMock(
            status_code=304,
            request=request,
            headers={'ETag': '"v1"', 'Cache-Control': 'max-age=600'},
        )
        actions.update_from_response(not_modified)
        result = actions.update_revalidated_response(
            not_modified, CachedResponse.from_response(cached)
        )
        # Stored current age (200 + 300) is retained even though the 304 carries no age info
        assert result.age_at_creation == 500
        assert result.age == 500
        # New max-age=600 measured against 500s-old content -> only 100s fresh
        assert result.expires_delta == 100


def test_304_immutable_keeps_never_expire_semantics():
    request = Request(method='GET', url=URL).prepare()
    with time_machine.travel(T0):
        cached = CachedResponse(
            url=URL,
            status_code=200,
            headers={'ETag': '"v1"'},
            age_at_creation=100,
            created_at=T0,
            expires=None,
        )

    with time_machine.travel(T0 + timedelta(seconds=300)):
        actions = CacheActions.from_request('k', request, CacheSettings(cache_control=True))
        not_modified = MagicMock(
            status_code=304,
            request=request,
            headers={'ETag': '"v1"', 'Cache-Control': 'immutable'},
        )
        actions.update_from_response(not_modified)
        result = actions.update_revalidated_response(
            not_modified, CachedResponse.from_response(cached)
        )
        assert result.expires is None
        assert result.age == 400
