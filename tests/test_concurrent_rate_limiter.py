from __future__ import annotations

import time
from concurrent.futures import ThreadPoolExecutor

from dramatiq.rate_limits import ConcurrentRateLimiter, RateLimitExceeded, RateLimitResult


def test_limiters_without_a_retry_boundary_still_report_results(rate_limiter_backend):
    # Given that I have a distributed mutex
    mutex = ConcurrentRateLimiter(rate_limiter_backend, "sequential-test", limit=1)

    # When I acquire it, asking for the richer result
    with mutex.acquire(raise_on_failure=False, with_retry_at=True) as result:
        # Then I get a truthy result without a retry hint
        assert result == RateLimitResult(True)
        assert result.retry_at is None

        # And a refused attempt is falsy, also without a hint, because
        # this limiter can't estimate one
        with mutex.acquire(raise_on_failure=False, with_retry_at=True) as refused:
            assert not refused
            assert refused.acquired is False
            assert refused.retry_at is None


def test_concurrent_rate_limiter_releases_the_lock_after_each_call(rate_limiter_backend):
    # Given that I have a distributed mutex and a call database
    mutex = ConcurrentRateLimiter(rate_limiter_backend, "sequential-test", limit=1)
    calls = 0

    # And I acquire it multiple times sequentially
    for _ in range(8):
        with mutex.acquire(raise_on_failure=False) as acquired:
            if not acquired:
                continue

            calls += 1

    # I expect it to have succeeded that number of times
    assert calls == 8


def test_concurrent_rate_limiter_can_act_as_a_mutex(rate_limiter_backend):
    # Given that I have a distributed mutex and a call database
    mutex = ConcurrentRateLimiter(rate_limiter_backend, "concurrent-test", limit=1)
    calls = []

    # And a function that adds calls to the database after acquiring the mutex
    def work():
        with mutex.acquire(raise_on_failure=False) as acquired:
            if not acquired:
                return

            calls.append(1)
            time.sleep(0.3)

    # If I execute multiple workers concurrently
    with ThreadPoolExecutor(max_workers=8) as e:
        futures = []
        for _ in range(8):
            futures.append(e.submit(work))

        for future in futures:
            future.result()

    # I expect only one call to have succeeded
    assert sum(calls) == 1


def test_concurrent_rate_limiter_limits_concurrency(rate_limiter_backend):
    # Given that I have a distributed rate limiter and a call database
    mutex = ConcurrentRateLimiter(rate_limiter_backend, "concurrent-test", limit=4)
    calls = []

    # And a function that adds calls to the database after acquiring the rate limit
    def work():
        try:
            with mutex.acquire():
                calls.append(1)
                time.sleep(0.3)
        except RateLimitExceeded:
            pass

    # If I execute multiple workers concurrently
    with ThreadPoolExecutor(max_workers=32) as e:
        futures = []
        for _ in range(32):
            futures.append(e.submit(work))

        for future in futures:
            future.result()

    # I expect at most 4 calls to have succeeded
    assert 3 <= sum(calls) <= 4
