from unittest.mock import MagicMock

from requests import Request

from requests_cache.policy import (
    EXPIRE_IMMEDIATELY,
    NEVER_EXPIRE,
    CacheActions,
    CacheSettings,
)


BASIC_REQUEST = Request(method='GET', url='https://img.site.com/base/img.jpg')
HTTPDATE = 'Fri, 16 APR 2021 21:13:00 GMT'


def _actions():
    return CacheActions.from_request('key', BASIC_REQUEST, CacheSettings(cache_control=True))


def test_response_directive_expire_after_values():
    cases = [
        ({}, None),
        ({'Cache-Control': 'no-cache'}, None),
        ({'Cache-Control': 'max-age=0'}, 0),
        ({'Cache-Control': 'max-age=60'}, 60),
        ({'Cache-Control': 'immutable'}, -1),
        ({'Expires': HTTPDATE}, HTTPDATE),
        ({'Expires': HTTPDATE, 'Cache-Control': 'max-age=60'}, 60),
        ({'Expires': 'garbage'}, 0),
        ({'Expires': 'garbage', 'Cache-Control': 'max-age=60'}, 60),
    ]
    for headers, expected in cases:
        actions = _actions()
        actions.update_from_response(MagicMock(headers=headers, request=BASIC_REQUEST))
        assert actions.expire_after == expected, headers


def test_immutable_sets_never_expire():
    actions = _actions()
    actions.update_from_response(
        MagicMock(
            headers={'Cache-Control': 'immutable, max-age=60'}, request=BASIC_REQUEST
        )
    )
    assert actions.expire_after == NEVER_EXPIRE


def test_expire_immediately_sentinel_unchanged():
    assert EXPIRE_IMMEDIATELY == 0
