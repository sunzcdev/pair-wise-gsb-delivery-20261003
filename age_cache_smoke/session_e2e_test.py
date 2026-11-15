import time_machine
from datetime import datetime, timedelta, timezone
from io import BytesIO
from unittest.mock import patch

from requests import Request, Session
from requests.models import Response
from urllib3.response import HTTPResponse

from requests_cache import CachedSession


T0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)
URL = 'http://example.com/resource'


def _mock_response(headers, status_code=200, content=b'body'):
    response = Response()
    response.status_code = status_code
    response.headers.update(headers)
    response._content = content
    response.url = URL
    response.request = Request(method='GET', url=URL).prepare()
    response.raw = HTTPResponse(
        body=BytesIO(content),
        headers=headers,
        status=status_code,
        preload_content=True,
    )
    return response


def test_end_to_end_age_then_304_revalidation():
    session = CachedSession(
        backend='memory',
        cache_control=True,
    )
    initial_headers = {
        'Cache-Control': 'max-age=600',
        'ETag': '"v1"',
        'Age': '300',
        'Date': 'Sun, 15 Nov 2026 12:00:00 GMT',
    }

    with time_machine.travel(T0):
        with patch.object(Session, 'send', return_value=_mock_response(initial_headers)):
            first = session.get(URL)
        assert first.from_cache is False
        key = session.cache.create_key(Request(method='GET', url=URL).prepare())
        cached = session.cache.get_response(key)
        assert cached.age_at_creation == 300
        assert cached.expires_delta == 300

    # Before expiry, a second request is a cache hit and never contacts the origin
    with time_machine.travel(T0 + timedelta(seconds=100)):
        with patch.object(Session, 'send', side_effect=AssertionError('should not send')):
            second = session.get(URL)
        assert second.from_cache is True

    # After expiry, a 304 revalidation preserves the old body age
    not_modified = _mock_response(
        {
            'Cache-Control': 'max-age=600',
            'ETag': '"v1"',
            'Date': 'Sun, 15 Nov 2026 12:10:00 GMT',
            'Age': '0',
        },
        status_code=304,
    )
    with time_machine.travel(T0 + timedelta(seconds=600)):
        with patch.object(Session, 'send', return_value=not_modified):
            third = session.get(URL)
        assert third.from_cache is True
        revalidated = session.cache.get_response(key)
        # Old body age (300 upstream + 600 local) is retained; it is not treated as fresh content
        assert revalidated.age == 900
        assert revalidated.age_at_creation == 900

    session.close()

