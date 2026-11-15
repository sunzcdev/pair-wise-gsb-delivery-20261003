import time_machine
from datetime import datetime, timedelta, timezone

from requests_cache.policy import (
    DO_NOT_CACHE,
    EXPIRE_IMMEDIATELY,
    NEVER_EXPIRE,
    get_age_adjusted_expiration,
    get_revalidated_expiration,
)


T0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)
AGED = {'Age': '300', 'Date': 'Sun, 15 Nov 2026 12:00:00 GMT'}


def test_expire_immediately_is_not_extended_or_broken_by_age():
    with time_machine.travel(T0):
        result = get_age_adjusted_expiration(AGED, EXPIRE_IMMEDIATELY)
        assert result == T0


def test_special_values_return_none():
    assert get_age_adjusted_expiration(AGED, NEVER_EXPIRE) is None
    assert get_age_adjusted_expiration(AGED, DO_NOT_CACHE) is None
    assert get_revalidated_expiration(NEVER_EXPIRE, 300, T0) is None


def test_revalidated_expire_immediately():
    with time_machine.travel(T0):
        assert get_revalidated_expiration(EXPIRE_IMMEDIATELY, 300, T0) == T0


def test_timedelta_lifetime_adjusted_for_age():
    with time_machine.travel(T0):
        result = get_age_adjusted_expiration(AGED, timedelta(seconds=600))
        assert result == T0 + timedelta(seconds=300)


def test_float_seconds_lifetime():
    with time_machine.travel(T0):
        result = get_age_adjusted_expiration(AGED, 600.5)
        assert result == T0 + timedelta(seconds=300.5)
