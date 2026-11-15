import time_machine
from datetime import datetime, timedelta, timezone

from requests_cache.backends import BaseCache
from requests_cache.models import CachedResponse


T0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)


def test_legacy_positional_expires_still_accepted():
    cache = BaseCache('legacy')
    response = CachedResponse(url='http://example.com/legacy', status_code=200)
    absolute = T0 + timedelta(seconds=123)
    with time_machine.travel(T0):
        # Old calling convention: positional absolute datetime, no expire_after kwarg
        cache.save_response(response, 'legacy-key', absolute)
    stored = cache.get_response('legacy-key')
    assert stored.expires == absolute


def test_relative_expire_after_keyword_without_age_headers():
    cache = BaseCache('relative')
    response = CachedResponse(url='http://example.com/rel', status_code=200)
    with time_machine.travel(T0):
        cache.save_response(response, 'rel-key', expire_after=600)
    stored = cache.get_response('rel-key')
    assert stored.expires == T0 + timedelta(seconds=600)
    assert stored.age_at_creation == 0
