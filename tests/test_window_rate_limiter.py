from __future__ import annotations

import time
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor

import pytest

from dramatiq.rate_limits import RateLimitExceeded, RateLimitResult, WindowRateLimiter
from dramatiq.rate_limits.backend import retry_delay_from_counts
from dramatiq.rate_limits.backends import StubBackend

from .common import skip_in_ci


@skip_in_ci
def test_window_rate_limiter_limits_per_window(rate_limiter_backend):
    # Given that I have a bucket rate limiter and a call database
    limiter = WindowRateLimiter(rate_limiter_backend, "window-test", limit=2, window=5)
    calls = defaultdict(lambda: 0)

    # And a function that increments keys over the span of 20 seconds
    def work():
        for _ in range(20):
            for _ in range(8):
                with limiter.acquire(raise_on_failure=False) as acquired:
                    if not acquired:
                        continue

                    calls[int(time.time())] += 1

            time.sleep(1)

    # If I run that function multiple times concurrently
    with ThreadPoolExecutor(max_workers=8) as e:
        futures = []
        for _ in range(8):
            futures.append(e.submit(work))

        for future in futures:
            future.result()

    # I expect between 8 and 10 calls to have been made in total
    assert 8 <= sum(calls.values()) <= 10


class FakeClock:
    """A stand-in for the time module that only moves when told to."""

    def __init__(self, now):
        self.now = now

    def time(self):
        return self.now

    def advance(self, seconds):
        self.now += seconds


@pytest.fixture()
def fake_clock(monkeypatch):
    clock = FakeClock(1_000_000.0)
    monkeypatch.setattr(time, "time", clock.time)
    return clock


def acquire_result(limiter):
    with limiter.acquire(raise_on_failure=False, with_retry_at=True) as result:
        return result


def test_retry_delay_from_counts_drops_the_oldest_buckets_first():
    # Buckets are ordered newest to oldest, so the oldest ones drop out
    # of the window first.
    assert retry_delay_from_counts([1, 1, 1], 1, 1) == 3
    assert retry_delay_from_counts([1, 1, 1], 1, 2) == 2
    assert retry_delay_from_counts([2, 0, 0, 0], 1, 2) == 4
    assert retry_delay_from_counts([0, 0, 2], 1, 2) == 1


def test_retry_delay_from_counts_is_at_least_one_bucket():
    # A rejection always points at a future bucket, even if the sum with
    # the requested amount fits (eg. when the single bucket is full).
    assert retry_delay_from_counts([0], 1, 1) == 1
    assert retry_delay_from_counts([1], 2, 2) == 1


def test_rate_limit_result_behaves_like_a_boolean():
    acquired = RateLimitResult(True)
    rejected = RateLimitResult(False, retry_at=1_000_003.0)

    # Both directions of the usual checks keep working
    assert acquired
    assert not rejected
    assert bool(acquired) is True
    assert bool(rejected) is False
    assert acquired == RateLimitResult(True)
    assert rejected == RateLimitResult(False, retry_at=1_000_003.0)
    assert rejected != acquired

    # And comparisons written against the old boolean still hold
    assert acquired == True  # noqa: E712
    assert rejected == False  # noqa: E712
    assert acquired != False  # noqa: E712
    assert rejected != True  # noqa: E712
    assert hash(acquired) == hash(True)

    # And the hint is readable for inspection and logging
    assert rejected.acquired is False
    assert rejected.retry_at == 1_000_003.0
    assert "retry_at=1000003.0" in repr(rejected)


def test_window_rate_limiter_can_raise_with_reserved_options_unset(fake_clock):
    # Given that I have a limiter that has exhausted its quota
    limiter = WindowRateLimiter(StubBackend(), "raising", limit=1, window=2)
    assert acquire_result(limiter)

    # When I use the raising API, the original exception is still raised
    # and none of the new options are required
    with pytest.raises(RateLimitExceeded):
        with limiter.acquire():
            pass


def test_window_rate_limiter_defaults_to_plain_booleans(fake_clock):
    # Given that I have a rate limiter with room for a single operation
    limiter = WindowRateLimiter(StubBackend(), "bools", limit=1, window=2)

    # When I acquire it with the original API
    with limiter.acquire(raise_on_failure=False) as acquired:
        # Then I get a plain bool back
        assert acquired is True

        # And a second attempt is refused with a plain bool
        with limiter.acquire(raise_on_failure=False) as refused:
            assert refused is False


def test_window_rate_limiter_reports_a_retry_boundary_when_rejected(fake_clock):
    # Given that I have a rate limiter with room for two operations in
    # a three second window
    limiter = WindowRateLimiter(StubBackend(), "hint", limit=2, window=3)

    # And the first two attempts fit in the current second
    assert acquire_result(limiter)
    assert acquire_result(limiter)

    # When the next attempt is refused
    result = acquire_result(limiter)

    # Then it is reported as rejected, with a hint for when to retry
    assert not result
    assert result.acquired is False

    # And the boundary is the whole second at which the two accepted
    # operations leave the window, so all three seconds roll off
    assert result.retry_at == fake_clock.now + 3
    assert result.retry_at == int(result.retry_at)

    # And that is indeed the first second where the request fits
    fake_clock.advance(result.retry_at - fake_clock.now - 1)
    assert not acquire_result(limiter)
    fake_clock.advance(1.0)
    assert acquire_result(limiter)


def test_window_rate_limiter_rejection_does_not_extend_the_window(fake_clock):
    # Given that I have a rate limiter that has exhausted its quota
    backend = StubBackend()
    limiter = WindowRateLimiter(backend, "no-extension", limit=1, window=2)
    assert acquire_result(limiter)
    assert not acquire_result(limiter)

    # When I hammer it while it's still refused
    fake_clock.advance(0.5)
    assert not acquire_result(limiter)
    fake_clock.advance(0.5)

    # Then the boundary stays anchored to the accepted operation, so it
    # is one second away even though a refused attempt just happened
    result = acquire_result(limiter)
    assert result.retry_at == fake_clock.now + 1

    # And once that second is over the slot is available again, proving
    # the refused attempts didn't push the window forward
    fake_clock.advance(1.0)
    assert acquire_result(limiter)


def test_window_rate_limiter_retry_boundary_is_only_a_hint(fake_clock):
    # Given that I have a rate limiter with a single slot per window
    backend = StubBackend()
    limiter = WindowRateLimiter(backend, "not-a-reservation", limit=1, window=2)
    assert acquire_result(limiter)

    # When two callers are both told about the same boundary
    first = acquire_result(limiter)
    second = acquire_result(limiter)
    assert first.retry_at == second.retry_at

    # Then only one of them can actually use it
    fake_clock.advance(first.retry_at - fake_clock.now)
    assert acquire_result(limiter)
    assert not acquire_result(limiter)


def test_window_rate_limiter_retry_boundary_uses_the_same_clock_as_admission(fake_clock):
    # Given that I have a rate limiter that accepts one op per window
    backend = StubBackend()
    limiter = WindowRateLimiter(backend, "same-clock", limit=1, window=3)
    start = fake_clock.now
    assert acquire_result(limiter)

    # Then the hint is relative to the fake clock, not the real one
    result = acquire_result(limiter)
    assert result.retry_at == start + 3

    # And the limiter agrees once the fake clock reaches that second
    fake_clock.advance(result.retry_at - fake_clock.now)
    assert acquire_result(limiter)


def test_window_rate_limiter_admits_the_quota_across_a_tick(fake_clock):
    # Given that I have a limiter with a two second window and room for
    # three operations per window
    backend = StubBackend()
    limiter = WindowRateLimiter(backend, "tick", limit=3, window=2)

    # And two operations were admitted just before a second boundary
    fake_clock.now = 1_000_000.999
    first = acquire_result(limiter)
    second = acquire_result(limiter)
    assert first
    assert second

    # When the clock moves past the boundary and several callers race
    # for the last slot at once
    fake_clock.advance(0.002)
    fake_clock.now = int(fake_clock.now)  # land exactly on the new second

    with ThreadPoolExecutor(max_workers=6) as e:
        results = list(e.map(lambda _: acquire_result(limiter), range(6)))

    admitted = [r for r in results if r]
    rejected = [r for r in results if not r]

    # Then exactly one of the racing callers got the last slot
    assert len(admitted) == 1
    assert len(rejected) == 5

    # And every rejected caller points at the boundary where the two
    # earlier operations roll out, one second after this tick
    for result in rejected:
        assert result.retry_at == fake_clock.now + 1

    # And at that boundary only the two slots freed by the roll-out are
    # available, so the accepted operation didn't leak further ahead
    fake_clock.advance(1.0)
    assert acquire_result(limiter)
    assert acquire_result(limiter)
    assert not acquire_result(limiter)
