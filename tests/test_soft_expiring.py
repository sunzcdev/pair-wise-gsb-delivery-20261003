"""Test SoftExpiringCache recipe with two local processes.

Compute callbacks are controllable: a shared gate event blocks computation
until the test allows it, a fail flag forces recomputation errors, and every
computation increments a counter stored in the cache itself so tests can
observe duplicate compute counts across processes.
"""

import multiprocessing as mp
import shutil
import time

import pytest

import diskcache as dc
from diskcache.recipes import SoftExpiringCache

KEY = 'metadata'
COUNT_KEY = ('soft-expiring-test', 'compute-count')


@pytest.fixture
def directory(tmp_path):
    path = str(tmp_path / 'cache')
    yield path
    shutil.rmtree(path, ignore_errors=True)


def make_soft_cache(directory, soft=0.3, hard=30.0, refresh=None):
    cache = dc.Cache(directory)
    return cache, SoftExpiringCache(cache, soft, hard, refresh_expire=refresh)


def compute_count(directory):
    with dc.Cache(directory) as cache:
        return cache.get(COUNT_KEY, default=0, retry=True)


def wait_for(predicate, timeout=15.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.05)
    return False


def reader_worker(directory, soft, hard, refresh, wait_timeout, gate, fail, value, queue):
    """Call get_or_compute with a controllable compute callback."""
    cache = dc.Cache(directory)
    soft_cache = SoftExpiringCache(cache, soft, hard, refresh_expire=refresh)

    def compute():
        cache.incr(COUNT_KEY, retry=True)
        gate.wait(30)
        if fail:
            raise RuntimeError('compute failed')
        return value

    try:
        result = soft_cache.get_or_compute(KEY, compute, wait_timeout=wait_timeout)
    except BaseException as exc:  # pylint: disable=broad-except
        queue.put(('error', repr(exc)))
    else:
        queue.put(('ok', result))
    finally:
        cache.close()


def late_commit_worker(directory, refresh, acquired, proceed, value, queue):
    """Acquire a refresh token, then commit only when told to (too late)."""
    cache = dc.Cache(directory)
    soft_cache = SoftExpiringCache(cache, 60.0, 60.0, refresh_expire=refresh)
    token = soft_cache.try_acquire_refresh(KEY)
    queue.put(('acquired', token is not None))
    acquired.set()
    proceed.wait(30)
    committed = soft_cache.commit_refresh(KEY, token, value)
    queue.put(('committed', committed))
    cache.close()


def spawn(ctx, target, args):
    proc = ctx.Process(target=target, args=args)
    proc.daemon = True
    proc.start()
    return proc


def drain(queue, count, timeout=15.0):
    results = []
    deadline = time.monotonic() + timeout
    for _ in range(count):
        remaining = deadline - time.monotonic()
        assert remaining > 0, 'timed out waiting for worker results'
        results.append(queue.get(timeout=max(remaining, 0.1)))
    return results


def test_cold_miss_two_processes_compute_once(directory):
    """Two processes race a cold miss; the value is computed exactly once."""
    ctx = mp.get_context('spawn')
    gate = ctx.Event()
    queue = ctx.Queue()
    args = (directory, 60.0, 300.0, None, 30.0, gate, False, 'v1', queue)
    procs = [spawn(ctx, reader_worker, args) for _ in range(2)]

    assert wait_for(lambda: compute_count(directory) == 1)
    time.sleep(0.5)  # Give the loser a chance to duplicate the computation.
    assert compute_count(directory) == 1  # Loser is waiting, not computing.

    gate.set()
    assert sorted(drain(queue, 2)) == [('ok', 'v1'), ('ok', 'v1')]
    for proc in procs:
        proc.join(15)
    assert compute_count(directory) == 1


def test_stale_refresh_serves_old_value_and_computes_once(directory):
    """During refresh the old value is served; only one process recomputes."""
    cache, soft_cache = make_soft_cache(directory, soft=0.3, hard=30.0)
    assert soft_cache.get_or_compute(KEY, lambda: 'old') == 'old'
    time.sleep(0.5)  # Value is now stale but within the hard deadline.

    ctx = mp.get_context('spawn')
    gate = ctx.Event()
    queue = ctx.Queue()
    args = (directory, 0.3, 30.0, None, 30.0, gate, False, 'new', queue)
    procs = [spawn(ctx, reader_worker, args) for _ in range(2)]

    assert wait_for(lambda: compute_count(directory) == 1)
    # The loser does not wait for the refresh; it briefly reads the old value.
    assert drain(queue, 1) == [('ok', 'old')]
    time.sleep(0.5)
    assert compute_count(directory) == 1  # No duplicate recomputation.

    gate.set()
    assert drain(queue, 1) == [('ok', 'new')]
    for proc in procs:
        proc.join(15)
    assert compute_count(directory) == 1
    assert soft_cache.get(KEY) == 'new'
    cache.close()


def test_refresh_failure_keeps_old_value_until_hard_deadline(directory):
    """Failed refresh falls back to the old value; hard expiry still applies."""
    cache, soft_cache = make_soft_cache(directory, soft=0.3, hard=2.0)
    assert soft_cache.get_or_compute(KEY, lambda: 'old') == 'old'
    time.sleep(0.5)  # Stale but within the hard deadline.

    ctx = mp.get_context('spawn')
    gate = ctx.Event()
    gate.set()  # Do not block the computation; let it fail immediately.
    queue = ctx.Queue()
    args = (directory, 0.3, 2.0, None, 30.0, gate, True, 'ignored', queue)
    proc = spawn(ctx, reader_worker, args)

    assert drain(queue, 1) == [('ok', 'old')]  # Failure falls back to old value.
    proc.join(15)
    assert compute_count(directory) == 1

    # The failed refresher released its own marker, so another may refresh.
    token = soft_cache.try_acquire_refresh(KEY)
    assert token is not None
    assert soft_cache.abort_refresh(KEY, token)

    # The old value survives until the hard deadline but is not revived.
    assert soft_cache.get(KEY) == 'old'
    time.sleep(1.8)  # Past the hard deadline (2.0s since the commit).
    assert soft_cache.get(KEY) is None
    cache.close()


def test_late_committer_cannot_overwrite_newer_result(directory):
    """A refresher whose token expired cannot overwrite a newer commit."""
    ctx = mp.get_context('spawn')
    acquired = ctx.Event()
    proceed = ctx.Event()
    queue = ctx.Queue()
    # Refresh tokens live only 0.5s; the worker commits long after that.
    proc = spawn(ctx, late_commit_worker, (directory, 0.5, acquired, proceed, 'late', queue))

    assert acquired.wait(15)
    assert drain(queue, 1) == [('acquired', True)]

    time.sleep(0.8)  # Worker's token has now expired.
    cache, soft_cache = make_soft_cache(directory, soft=60.0, hard=60.0, refresh=0.5)
    token = soft_cache.try_acquire_refresh(KEY)
    assert token is not None
    assert soft_cache.commit_refresh(KEY, token, 'fresh')

    proceed.set()  # Late worker now tries to commit its stale result.
    assert drain(queue, 1) == [('committed', False)]
    proc.join(15)
    assert soft_cache.get(KEY) == 'fresh'
    cache.close()


def test_cold_miss_waiter_times_out_without_deleting_marker(directory):
    """A waiter on a cold miss may time out and must not delete the marker."""
    ctx = mp.get_context('spawn')
    gate = ctx.Event()
    queue = ctx.Queue()
    args = (directory, 60.0, 300.0, None, 30.0, gate, False, 'v1', queue)
    proc = spawn(ctx, reader_worker, args)

    cache, soft_cache = make_soft_cache(directory, soft=60.0, hard=300.0)
    assert wait_for(lambda: soft_cache.refreshing(KEY))  # Worker holds token.

    def forbidden_compute():
        raise AssertionError('waiter must not compute while token is held')

    start = time.monotonic()
    result = soft_cache.get_or_compute(KEY, forbidden_compute, wait_timeout=0.5)
    elapsed = time.monotonic() - start
    assert result is None  # Timed-out waiter exits with the default.
    assert 0.4 < elapsed < 5.0
    assert soft_cache.refreshing(KEY)  # Marker belongs to the worker still.

    gate.set()
    assert drain(queue, 1) == [('ok', 'v1')]
    proc.join(15)
    assert soft_cache.get(KEY) == 'v1'
    cache.close()
