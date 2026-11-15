import time_machine
from datetime import datetime, timedelta, timezone

from requests_cache.policy import get_age_adjusted_expiration, get_corrected_initial_age


T0 = datetime(2026, 11, 15, 12, 0, 0, tzinfo=timezone.utc)


def test_date_in_past_drives_apparent_age_without_age_header():
    with time_machine.travel(T0):
        # Origin generated this 200s ago; no Age header. max-age=600 => 400s remain.
        headers = {'Date': 'Sun, 15 Nov 2026 11:56:40 GMT'}
        assert get_corrected_initial_age(headers) == 200
        expires = get_age_adjusted_expiration(headers, 600)
        assert expires == T0 + timedelta(seconds=400)


def test_age_header_wins_over_apparent_age():
    with time_machine.travel(T0):
        # Apparent age from Date is 100s, but upstream Age claims 400s -> use 400
        headers = {
            'Date': 'Sun, 15 Nov 2026 11:58:20 GMT',
            'Age': '400',
        }
        assert get_corrected_initial_age(headers) == 400
        assert get_age_adjusted_expiration(headers, 600) == T0 + timedelta(seconds=200)


def test_invalid_date_falls_back_to_age_header():
    with time_machine.travel(T0):
        headers = {'Date': 'not-a-date', 'Age': '150'}
        assert get_corrected_initial_age(headers) == 150
        assert get_age_adjusted_expiration(headers, 600) == T0 + timedelta(seconds=450)


def test_invalid_date_and_no_age_means_zero_initial_age():
    with time_machine.travel(T0):
        assert get_corrected_initial_age({'Date': 'garbage'}) == 0
        assert get_age_adjusted_expiration({'Date': 'garbage'}, 600) == T0 + timedelta(
            seconds=600
        )
