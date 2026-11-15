import time_machine
from datetime import datetime, timedelta, timezone

from requests_cache.backends import BaseCache
from requests_cache.models import CachedResponse
from requests_cache.policy import (
    NEVER_EXPIRE,
    get_age_adjusted_expiration,
    get_corrected_initial_age,
)


T0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)


def test_no_age_headers_leaves_relative_expiration_unchanged():
    with time_machine.travel(T0):
        # A client-side expire_after with no HTTP age information must behave exactly as before
        assert get_age_adjusted_expiration({}, 600) == T0 + timedelta(seconds=600)
        assert get_age_adjusted_expiration({}, timedelta(minutes=10)) == T0 + timedelta(
            seconds=600
        )
        assert get_corrected_initial_age({}) == 0


def test_never_expire_and_none_pass_through():
    assert get_age_adjusted_expiration({'Age': '999'}, None) is None
    assert get_age_adjusted_expiration({'Age': '999'}, NEVER_EXPIRE) is None


def test_invalid_and_negative_age_do_not_extend_life():
    with time_machine.travel(T0):
        # Invalid/negative Age parse to zero: no lifetime extension
        assert get_corrected_initial_age({'Age': 'not-a-number'}) == 0
        assert get_corrected_initial_age({'Age': '-100'}) == 0
        assert get_age_adjusted_expiration({'Age': '-100'}, 600) == T0 + timedelta(seconds=600)
        # A valid positive Age shortens life
        assert get_age_adjusted_expiration({'Age': '100'}, 600) == T0 + timedelta(seconds=500)


def test_future_date_clamped_to_zero_age():
    with time_machine.travel(T0):
        slight_future = {'Date': 'Sun, 15 Nov 2026 12:00:30 GMT'}
        far_future = {'Date': 'Sun, 15 Nov 2026 13:00:00 GMT'}
        assert get_corrected_initial_age(slight_future) == 0
        assert get_corrected_initial_age(far_future) == 0
        # No negative age means no extension beyond the requested lifetime
        assert get_age_adjusted_expiration(slight_future, 600) == T0 + timedelta(seconds=600)


def test_clock_skew_boundary_is_always_clamped_to_zero():
    # Any future Date (within or beyond the skew tolerance) yields a non-negative age
    with time_machine.travel(T0):
        at_limit = {'Date': 'Sun, 15 Nov 2026 12:01:00 GMT'}  # exactly 60s in the future
        beyond = {'Date': 'Sun, 15 Nov 2026 12:02:00 GMT'}  # beyond the 60s tolerance
        assert get_corrected_initial_age(at_limit) == 0
        assert get_corrected_initial_age(beyond) == 0
        # Apparent age still reflects real past Dates
        past = {'Date': 'Sun, 15 Nov 2026 11:59:00 GMT'}
        assert get_corrected_initial_age(past) == 60


def test_missing_date_uses_age_header_only():
    with time_machine.travel(T0):
        assert get_corrected_initial_age({'Age': '250'}) == 250
        assert get_age_adjusted_expiration({'Age': '250'}, 600) == T0 + timedelta(seconds=350)


def test_expires_absolute_is_fixed_point():
    with time_machine.travel(T0):
        absolute = T0 + timedelta(seconds=999)
        # Absolute datetimes are never shifted by local age adjustment
        assert get_age_adjusted_expiration({'Age': '500'}, absolute) == absolute


def test_stored_default_age_is_zero_for_legacy_entries():
    entry = CachedResponse()
    assert entry.age_at_creation == 0
    with time_machine.travel(T0):
        assert entry.age >= 0
