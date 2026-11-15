import time_machine
from datetime import datetime, timedelta, timezone

from requests_cache.backends import BaseCache
from requests_cache.models import CachedResponse


T0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)


def test_client_expire_after_unaffected_when_no_http_age_context():
    # No Age/Date headers: a client expire_after keeps exactly its requested lifetime
    cache = BaseCache('client')
    response = CachedResponse(url='http://example.com/x', status_code=200, headers={})
    with time_machine.travel(T0):
        cache.save_response(response, 'k', expire_after=600)
    entry = cache.get_response('k')
    assert entry.expires == T0 + timedelta(seconds=600)
    assert entry.age_at_creation == 0


def test_age_header_still_applies_even_without_cache_control():
    # Age adjustment is a transport-layer freshness correction, not a cache_control policy gate
    cache = BaseCache('age-transport')
    response = CachedResponse(
        url='http://example.com/y',
        status_code=200,
        headers={'Age': '300', 'Date': 'Sun, 15 Nov 2026 12:00:00 GMT'},
    )
    with time_machine.travel(T0):
        cache.save_response(response, 'k', expire_after=600)
    entry = cache.get_response('k')
    assert entry.age_at_creation == 300
    assert entry.expires == T0 + timedelta(seconds=300)
