import re
from datetime import datetime, timedelta, timezone
from unittest.mock import patch

import pytest

from requests_cache.policy.expiration import (
    DO_NOT_CACHE,
    EXPIRE_IMMEDIATELY,
    NEVER_EXPIRE,
    get_age_adjusted_expiration,
    get_age_header,
    get_corrected_initial_age,
    get_expiration_datetime,
    get_revalidated_expiration,
    get_url_expiration,
    utcnow,
)
from requests_cache.policy.directives import set_request_headers
from tests.conftest import HTTPDATE_DATETIME, HTTPDATE_STR, skip_pypy, time_travel

# A fixed local receive time, used with the controlled clock in tests/conftest.py
AGE_T0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)
AGE_T0_STR = 'Sun, 15 Nov 2026 12:00:00 GMT'
AGED_HEADERS = {
    'Cache-Control': 'max-age=600',
    'Age': '300',
    'Date': AGE_T0_STR,
}


@patch('requests_cache.expiration.datetime')
def test_get_expiration_datetime__no_expiration(mock_datetime):
    assert get_expiration_datetime(None) is None
    assert get_expiration_datetime(-1) is None
    assert get_expiration_datetime(EXPIRE_IMMEDIATELY) == mock_datetime.now(timezone.utc)
    # test 'Expires: 0' (str)
    assert get_expiration_datetime('0') is mock_datetime.now(timezone.utc)
    # test 'Expires: -1' (str) - Azure silliness
    assert get_expiration_datetime('-1') is mock_datetime.now(timezone.utc)


@pytest.mark.parametrize(
    'expire_after, expected_expiration_delta',
    [
        (timedelta(seconds=60), timedelta(seconds=60)),
        (60, timedelta(seconds=60)),
        (33.3, timedelta(seconds=33.3)),
    ],
)
def test_get_expiration_datetime__relative(expire_after, expected_expiration_delta):
    expires = get_expiration_datetime(expire_after)
    expected_expiration = utcnow() + expected_expiration_delta
    # Instead of mocking datetime (which adds some complications), check for approximate value
    assert abs((expires - expected_expiration).total_seconds()) <= 1


def test_get_expiration_datetime__tzinfo():
    tz = timezone(-timedelta(hours=5))
    dt = datetime(2021, 2, 1, 7, 0, tzinfo=tz)
    assert get_expiration_datetime(dt) == datetime(2021, 2, 1, 12, 0, tzinfo=timezone.utc)


def test_get_expiration_datetime__httpdate():
    assert get_expiration_datetime(HTTPDATE_STR) == HTTPDATE_DATETIME
    assert get_expiration_datetime('P12Y34M56DT78H90M12.345S', ignore_invalid_httpdate=True) is None
    with pytest.raises(ValueError):
        get_expiration_datetime('P12Y34M56DT78H90M12.345S')


def test_get_expiration_datetime__empty_string():
    """An empty Expires header should raise ValueError, not IndexError"""
    assert get_expiration_datetime('', ignore_invalid_httpdate=True) is None
    with pytest.raises(ValueError):
        get_expiration_datetime('')


@pytest.mark.parametrize(
    'url, expected_expire_after',
    [
        ('img.site_1.com', 60 * 60),
        ('http://img.site_1.com/base/img.jpg', 60 * 60),
        ('https://img.site_2.com/base/img.jpg', None),
        ('site_2.com/resource_1', 60 * 60 * 2),
        ('http://site_2.com/resource_1/index.html', 60 * 60 * 2),
        ('http://site_2.com/resource_2/', 60 * 60 * 24),
        ('http://site_2.com/static/', -1),
        ('http://site_2.com/api/resource/123', 60 * 60 * 24 * 7),
        ('http://site_2.com/api/resource/xyz', None),
        ('http://site_2.com/static/img.jpg', -1),
        ('site_2.com', None),
        ('some_other_site.com', None),
        (None, None),
    ],
)
def test_get_url_expiration(url, expected_expire_after, mock_session):
    urls_expire_after = {
        '*.site_1.com': 60 * 60,
        'site_2.com/resource_1': 60 * 60 * 2,
        'site_2.com/resource_2': 60 * 60 * 24,
        re.compile(r'site_2\.com/api/resource/\d+'): 60 * 60 * 24 * 7,
        'site_2.com/static': -1,
    }
    assert get_url_expiration(url, urls_expire_after) == expected_expire_after


@pytest.mark.parametrize(
    'url, expected_expire_after',
    [
        ('https://img.site_1.com/image.jpeg', 60 * 60),
        ('https://img.site_1.com/resource/1', 60 * 60 * 2),
        ('https://site_2.com', 1),
        ('https://any_other_site.com', 1),
    ],
)
def test_get_url_expiration__evaluation_order(url, expected_expire_after):
    """If there are multiple matches, the first match should be used in the order defined"""
    urls_expire_after = {
        '*.site_1.com/resource': 60 * 60 * 2,
        '*.site_1.com': 60 * 60,
        '*': 1,
    }
    assert get_url_expiration(url, urls_expire_after) == expected_expire_after


def test_set_request_headers():
    headers = set_request_headers(
        {'ETag': '123456'},
        expire_after=60,
        only_if_cached=True,
        refresh=True,
        force_refresh=True,
    )
    assert headers['Cache-Control'] == 'max-age=60,must-revalidate,no-cache,only-if-cached'
    assert headers['ETag'] == '123456'


def test_set_request_headers__do_not_cache():
    headers = set_request_headers(
        {},
        expire_after=DO_NOT_CACHE,
        only_if_cached=False,
        refresh=False,
        force_refresh=False,
    )
    assert 'X-ACTUAL-NO-CACHE' in headers
    assert 'Cache-Control' not in headers


# Age header / response age
# -------------------------


@pytest.mark.parametrize(
    'headers, expected_age',
    [
        ({}, 0),
        ({'Age': '300'}, 300),
        ({'Age': '0'}, 0),
        ({'Age': 'not-a-number'}, 0),
        ({'Age': ''}, 0),
        ({'Age': '-100'}, 0),  # Negative ages must never extend the lifetime
        ({'Age': '  120  '}, 120),
    ],
)
def test_get_age_header(headers, expected_age):
    assert get_age_header(headers) == expected_age


@pytest.mark.parametrize(
    'headers, expected_age',
    [
        ({}, 0),
        # Age only
        ({'Age': '250'}, 250),
        # Date only; apparent age is measured from the receive time
        ({'Date': 'Sun, 15 Nov 2026 11:56:40 GMT'}, 200),
        # The larger of the two wins
        ({'Date': 'Sun, 15 Nov 2026 11:58:20 GMT', 'Age': '400'}, 400),
        ({'Date': 'Sun, 15 Nov 2026 11:56:40 GMT', 'Age': '100'}, 200),
        # Invalid dates fall back to the Age header
        ({'Date': 'not-a-date', 'Age': '150'}, 150),
        ({'Date': 'garbage'}, 0),
        # Invalid/negative Age is ignored, not treated as a negative offset
        ({'Date': 'Sun, 15 Nov 2026 11:58:20 GMT', 'Age': '-400'}, 100),
    ],
)
@skip_pypy
def test_get_corrected_initial_age(headers, expected_age):
    with time_travel(AGE_T0):
        assert get_corrected_initial_age(headers) == expected_age


@pytest.mark.parametrize(
    'date_header',
    [
        'Sun, 15 Nov 2026 12:00:30 GMT',  # Within the clock skew tolerance
        'Sun, 15 Nov 2026 13:00:00 GMT',  # Far in the future
    ],
)
@skip_pypy
def test_get_corrected_initial_age__future_date_clamped(date_header):
    """A future Date (clock skew) must never produce a negative age"""
    with time_travel(AGE_T0):
        assert get_corrected_initial_age({'Date': date_header}) == 0


@pytest.mark.parametrize(
    'expire_after, expected_delta',
    [
        (600, 300),
        (60, -240),
        (timedelta(seconds=600), 300),
        (600.5, 300.5),
        (EXPIRE_IMMEDIATELY, 0),
        (NEVER_EXPIRE, None),
        (DO_NOT_CACHE, None),
        (None, None),
    ],
)
@skip_pypy
def test_get_age_adjusted_expiration(expire_after, expected_delta):
    """A relative lifetime is shortened by the response's corrected initial age"""
    with time_travel(AGE_T0):
        result = get_age_adjusted_expiration(AGED_HEADERS, expire_after)
    if expected_delta is None:
        assert result is None
    else:
        assert result == AGE_T0 + timedelta(seconds=expected_delta)


@pytest.mark.parametrize('headers', [{}, {'Age': '-100'}, {'Age': 'junk'}, {'Date': 'garbage'}])
@skip_pypy
def test_get_age_adjusted_expiration__no_usable_age(headers):
    """With no usable age information, a client-side expire_after keeps its full lifetime"""
    with time_travel(AGE_T0):
        assert get_age_adjusted_expiration(headers, 600) == AGE_T0 + timedelta(seconds=600)


@skip_pypy
def test_get_age_adjusted_expiration__absolute_is_fixed_point():
    """An absolute expiration (datetime or Expires httpdate) is not shifted by current age"""
    absolute = AGE_T0 + timedelta(seconds=999)
    with time_travel(AGE_T0):
        assert get_age_adjusted_expiration(AGED_HEADERS, absolute) == absolute
        httpdate = 'Sun, 15 Nov 2026 12:16:39 GMT'
        assert get_age_adjusted_expiration(AGED_HEADERS, httpdate) == absolute


@pytest.mark.parametrize(
    'expire_after, corrected_age, expected_delta',
    [
        (600, 500, 100),
        (600, 900, -300),
        (600, 0, 600),
        (timedelta(seconds=600), 500, 100),
        (EXPIRE_IMMEDIATELY, 500, 0),
        (NEVER_EXPIRE, 500, None),
        (DO_NOT_CACHE, 500, None),
        (None, 500, None),
    ],
)
@skip_pypy
def test_get_revalidated_expiration(expire_after, corrected_age, expected_delta):
    with time_travel(AGE_T0):
        result = get_revalidated_expiration(expire_after, corrected_age)
    if expected_delta is None:
        assert result is None
    else:
        assert result == AGE_T0 + timedelta(seconds=expected_delta)


@skip_pypy
def test_get_revalidated_expiration__absolute_is_fixed_point():
    absolute = AGE_T0 + timedelta(seconds=999)
    with time_travel(AGE_T0):
        assert get_revalidated_expiration(absolute, 500) == absolute
