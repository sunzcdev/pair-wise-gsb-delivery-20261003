"""Test diskcache.recipes SoftExpiringCache."""

import multiprocessing
import shutil
import threading
import time

import pytest

import diskcache as dc


@pytest.fixture
def cache():
    with dc.Cache() as cache:
        yield cache
    shutil.rmtree(cache.directory, ignore_errors=True)


def test_set_get_peek(cache):
    sec = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=120)
    assert sec.peek('a') == (None, 'miss')
    assert sec.get('a') is None
    sec.set('a', 1)
    assert sec.get('a') == 1
    assert sec.peek('a') == (1, 'fresh')
    sec.delete('a')
    assert sec.peek('a') == (None, 'miss')


def test_soft_and_hard_expiry(cache):
    sec = dc.SoftExpiringCache(cache, soft_expire=0.05, hard_expire=0.2)
    sec.set('a', 1)
    assert sec.peek('a') == (1, 'fresh')
    time.sleep(0.1)
    # Old value is still returned between soft and hard expiration times.
    assert sec.peek('a') == (1, 'stale')
    assert sec.get('a') == 1
    time.sleep(0.2)
    # Hard deadline passed; value is discarded.
    assert sec.peek('a') == (None, 'miss')


def test_get_or_compute_fresh(cache):
    sec = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=120)
    sec.set('a', 1)
    calls = []
    assert sec.get_or_compute('a', lambda: calls.append(1)) == 1
    assert calls == []


def test_get_or_compute_miss(cache):
    sec = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=120)
    assert sec.get_or_compute('a', lambda: 2) == 2
    assert sec.get('a') == 2


def test_token_exclusive_and_late_commit(cache):
    sec = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=120)
    sec.set('a', 1)
    token = sec.begin_refresh('a')
    assert token is not None
    assert sec.begin_refresh('a') is None  # lease is exclusive
    assert sec.commit_refresh('a', token, 2)
    # Late commit with the same token cannot overwrite the newer result.
    assert sec.commit_refresh('a', token, 3) is False
    assert sec.get('a') == 2


def test_late_commit_after_lease_expiry(cache):
    sec = dc.SoftExpiringCache(
        cache, soft_expire=60, hard_expire=120, refresh_expire=0.05
    )
    sec.set('a', 1)
    token1 = sec.begin_refresh('a')
    time.sleep(0.1)  # lease expires while token1 is still "computing"
    token2 = sec.begin_refresh('a')
    assert token2 is not None
    assert sec.commit_refresh('a', token2, 2)
    # The late refresher's token no longer matches the lease or generation.
    assert sec.commit_refresh('a', token1, 3) is False
    assert sec.get('a') == 2


def test_abort_keeps_value_and_deadline(cache):
    sec = dc.SoftExpiringCache(cache, soft_expire=0, hard_expire=60)
    sec.set('a', 1)
    data_key = sec._data_key('a')
    _, expire_before = cache.get(data_key, expire_time=True)
    token = sec.begin_refresh('a')
    assert sec.abort_refresh('a', token)
    assert sec.get('a') == 1
    _, expire_after = cache.get(data_key, expire_time=True)
    # Failure does not extend the hard deadline (no infinite renewal).
    assert expire_before == expire_after


def test_abort_not_holder(cache):
    sec = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=120)
    token = sec.begin_refresh('a')
    other = dc.recipes.RefreshToken(token.generation, 'not-the-nonce')
    assert sec.abort_refresh('a', other) is False
    assert sec.begin_refresh('a') is None  # marker untouched
    assert sec.abort_refresh('a', token)


def test_failure_backoff_delays_retry(cache):
    sec = dc.SoftExpiringCache(
        cache, soft_expire=0, hard_expire=60, failure_backoff=0.2
    )
    sec.set('a', 1)
    token = sec.begin_refresh('a')
    assert sec.abort_refresh('a', token)
    assert sec.begin_refresh('a') is None  # backoff holds the lease
    time.sleep(0.3)
    assert sec.begin_refresh('a') is not None


def test_failed_compute_returns_stale(cache):
    sec = dc.SoftExpiringCache(cache, soft_expire=0, hard_expire=60)
    sec.set('a', 1)

    def boom():
        raise ValueError('boom')

    assert sec.get_or_compute('a', boom) == 1
    assert sec.get('a') == 1


def test_failed_compute_on_miss_raises(cache):
    sec = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=120)

    def boom():
        raise ValueError('boom')

    with pytest.raises(ValueError):
        sec.get_or_compute('a', boom)
    # Lease was released so a retry may proceed.
    assert sec.get_or_compute('a', lambda: 2) == 2


def test_miss_waiter_timeout_keeps_marker(cache):
    sec = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=120)
    token = sec.begin_refresh('a')
    result = sec.get_or_compute('a', lambda: 2, default='timeout', wait_timeout=0.05)
    assert result == 'timeout'
    # Waiter timed out without deleting the other holder's marker.
    assert sec.begin_refresh('a') is None
    assert sec.commit_refresh('a', token, 2)
    assert sec.get('a') == 2


def test_single_flight_threads(cache):
    sec = dc.SoftExpiringCache(cache, soft_expire=0, hard_expire=60)
    sec.set('a', 'old')
    state = {'count': 0}
    barrier = threading.Barrier(3)
    release = threading.Event()
    results = []

    def compute():
        state['count'] += 1
        release.wait(10)
        return 'new'

    def worker():
        barrier.wait()
        results.append(sec.get_or_compute('a', compute))

    threads = [threading.Thread(target=worker) for _ in range(2)]
    for thread in threads:
        thread.start()
    barrier.wait()
    time.sleep(0.2)  # loser observes the lease and serves the stale value
    release.set()
    for thread in threads:
        thread.join()
    assert state['count'] == 1
    assert sorted(results) == ['new', 'old']


def _mp_refresh_worker(directory, started, proceed, counter, queue):
    cache = dc.Cache(directory)
    sec = dc.SoftExpiringCache(cache, soft_expire=0, hard_expire=60)

    def compute():
        with counter.get_lock():
            counter.value += 1
        started.set()
        proceed.wait(30)
        return 'computed'

    queue.put(sec.get_or_compute('key', compute))
    cache.close()


def test_two_processes_single_refresh(tmp_path):
    """Two processes race to refresh a stale key; exactly one computes."""
    ctx = multiprocessing.get_context('spawn')
    directory = str(tmp_path)
    started = ctx.Event()
    proceed = ctx.Event()
    counter = ctx.Value('i', 0)
    queue = ctx.Queue()

    cache = dc.Cache(directory)
    sec = dc.SoftExpiringCache(cache, soft_expire=0, hard_expire=60)
    sec.set('key', 'seed')
    cache.close()

    procs = [
        ctx.Process(
            target=_mp_refresh_worker,
            args=(directory, started, proceed, counter, queue),
        )
        for _ in range(2)
    ]
    for proc in procs:
        proc.start()
    assert started.wait(30)
    time.sleep(0.5)  # loser observes the lease and serves the stale value
    proceed.set()
    for proc in procs:
        proc.join(30)
        assert proc.exitcode == 0

    # Duplicate computation count is one across both processes.
    assert counter.value == 1
    results = sorted(queue.get(timeout=5) for _ in procs)
    assert results == ['computed', 'seed']


def _mp_failing_worker(directory, started, proceed, counter, queue):
    cache = dc.Cache(directory)
    sec = dc.SoftExpiringCache(cache, soft_expire=0, hard_expire=60)

    def compute():
        with counter.get_lock():
            counter.value += 1
        started.set()
        proceed.wait(30)
        raise RuntimeError('boom')

    queue.put(sec.get_or_compute('key', compute))
    cache.close()


def test_two_processes_failure_fallback(tmp_path):
    """Failed refresh keeps the old value without extending its deadline."""
    ctx = multiprocessing.get_context('spawn')
    directory = str(tmp_path)
    started = ctx.Event()
    proceed = ctx.Event()
    counter = ctx.Value('i', 0)
    queue = ctx.Queue()

    cache = dc.Cache(directory)
    sec = dc.SoftExpiringCache(cache, soft_expire=0, hard_expire=60)
    sec.set('key', 'seed')
    data_key = sec._data_key('key')
    _, expire_before = cache.get(data_key, expire_time=True)
    cache.close()

    procs = [
        ctx.Process(
            target=_mp_failing_worker,
            args=(directory, started, proceed, counter, queue),
        )
        for _ in range(2)
    ]
    for proc in procs:
        proc.start()
    assert started.wait(30)
    time.sleep(0.5)
    proceed.set()
    for proc in procs:
        proc.join(30)
        assert proc.exitcode == 0

    # Only one process computed; both fell back to the old value.
    assert counter.value == 1
    assert [queue.get(timeout=5) for _ in procs] == ['seed', 'seed']

    cache = dc.Cache(directory)
    value, expire_after = cache.get(data_key, expire_time=True)
    cache.close()
    assert value[0] == 'seed'
    # Failure did not renew the hard deadline.
    assert expire_after == expire_before


def _mp_slow_token_holder(directory, refresh_expire, acquired, proceed, queue):
    cache = dc.Cache(directory)
    sec = dc.SoftExpiringCache(
        cache, soft_expire=0, hard_expire=60, refresh_expire=refresh_expire
    )
    token = sec.begin_refresh('key')
    assert token is not None
    acquired.set()
    proceed.wait(30)
    queue.put(sec.commit_refresh('key', token, 'late'))
    cache.close()


def _mp_fast_refresher(directory, queue):
    cache = dc.Cache(directory)
    sec = dc.SoftExpiringCache(cache, soft_expire=0, hard_expire=60)
    token = sec.begin_refresh('key')
    assert token is not None
    queue.put(sec.commit_refresh('key', token, 'fresh'))
    cache.close()


def test_two_processes_late_commit(tmp_path):
    """A late refresher cannot overwrite the newer committed result."""
    ctx = multiprocessing.get_context('spawn')
    directory = str(tmp_path)
    acquired = ctx.Event()
    proceed = ctx.Event()
    queue = ctx.Queue()

    cache = dc.Cache(directory)
    sec = dc.SoftExpiringCache(cache, soft_expire=0, hard_expire=60)
    sec.set('key', 'seed')
    cache.close()

    slow = ctx.Process(
        target=_mp_slow_token_holder,
        args=(directory, 0.3, acquired, proceed, queue),
    )
    slow.start()
    assert acquired.wait(30)
    time.sleep(0.6)  # slow process's lease expires while it "computes"

    fast = ctx.Process(target=_mp_fast_refresher, args=(directory, queue))
    fast.start()
    fast.join(30)
    assert fast.exitcode == 0
    assert queue.get(timeout=5) is True  # fast process committed

    proceed.set()
    slow.join(30)
    assert slow.exitcode == 0
    assert queue.get(timeout=5) is False  # late commit rejected

    cache = dc.Cache(directory)
    assert dc.SoftExpiringCache(cache, 0, 60).get('key') == 'fresh'
    cache.close()


def _mp_first_compute_worker(directory, started, proceed, queue):
    cache = dc.Cache(directory)
    sec = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=120)

    def compute():
        started.set()
        proceed.wait(30)
        return 'v1'

    queue.put(sec.get_or_compute('key', compute))
    cache.close()


def _mp_waiter_worker(directory, queue):
    cache = dc.Cache(directory)
    sec = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=120)

    def compute():
        queue.put('waiter-computed')  # must never run
        return 'bad'

    value = sec.get_or_compute(
        'key', compute, default='timeout', wait_timeout=0.3
    )
    queue.put(value)
    cache.close()


def test_two_processes_miss_waiter_timeout(tmp_path):
    """With no initial value, a waiter times out without deleting the
    refreshing process's marker."""
    ctx = multiprocessing.get_context('spawn')
    directory = str(tmp_path)
    started = ctx.Event()
    proceed = ctx.Event()
    queue = ctx.Queue()

    first = ctx.Process(
        target=_mp_first_compute_worker, args=(directory, started, proceed, queue)
    )
    first.start()
    assert started.wait(30)  # first process holds the lease and computes

    waiter = ctx.Process(target=_mp_waiter_worker, args=(directory, queue))
    waiter.start()
    waiter.join(30)
    assert waiter.exitcode == 0
    assert queue.get(timeout=5) == 'timeout'

    # The waiter's timeout did not delete the first process's marker.
    cache = dc.Cache(directory)
    sec = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=120)
    assert sec.begin_refresh('key') is None
    cache.close()

    proceed.set()
    first.join(30)
    assert first.exitcode == 0
    assert queue.get(timeout=5) == 'v1'

    cache = dc.Cache(directory)
    assert dc.SoftExpiringCache(cache, 60, 120).get('key') == 'v1'
    cache.close()
