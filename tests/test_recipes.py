"""Test diskcache.recipes."""

import multiprocessing
import os
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


def test_averager(cache):
    nums = dc.Averager(cache, 'nums')
    for i in range(10):
        nums.add(i)
    assert nums.get() == 4.5
    assert nums.pop() == 4.5
    for i in range(20):
        nums.add(i)
    assert nums.get() == 9.5
    assert nums.pop() == 9.5


def test_lock(cache):
    state = {'num': 0}
    lock = dc.Lock(cache, 'demo')

    def worker():
        state['num'] += 1
        with lock:
            assert lock.locked()
            state['num'] += 1
            time.sleep(0.1)

    with lock:
        thread = threading.Thread(target=worker)
        thread.start()
        time.sleep(0.1)
        assert state['num'] == 1
    thread.join()
    assert state['num'] == 2


def test_rlock(cache):
    state = {'num': 0}
    rlock = dc.RLock(cache, 'demo')

    def worker():
        state['num'] += 1
        with rlock:
            with rlock:
                state['num'] += 1
                time.sleep(0.1)

    with rlock:
        thread = threading.Thread(target=worker)
        thread.start()
        time.sleep(0.1)
        assert state['num'] == 1
    thread.join()
    assert state['num'] == 2


def test_semaphore(cache):
    state = {'num': 0}
    semaphore = dc.BoundedSemaphore(cache, 'demo', value=3)

    def worker():
        state['num'] += 1
        with semaphore:
            state['num'] += 1
            time.sleep(0.1)

    semaphore.acquire()
    semaphore.acquire()
    with semaphore:
        thread = threading.Thread(target=worker)
        thread.start()
        time.sleep(0.1)
        assert state['num'] == 1
    thread.join()
    assert state['num'] == 2
    semaphore.release()
    semaphore.release()


def test_memoize_stampede(cache):
    state = {'num': 0}

    @dc.memoize_stampede(cache, 0.1)
    def worker(num):
        time.sleep(0.01)
        state['num'] += 1
        return num

    start = time.time()
    while (time.time() - start) < 1:
        worker(100)
    assert state['num'] > 0


def test_soft_expiring_cache_token_protocol(cache):
    soft = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=300)

    assert soft.peek('key') is None
    assert soft.get('key') is None
    assert soft.get('key', default='missing') == 'missing'

    # Fresh values are computed once and reused.
    calls = []

    def fetch():
        calls.append(1)
        return 'v1'

    assert soft.get_or_compute('key', fetch) == 'v1'
    assert soft.get_or_compute('key', fetch) == 'v1'
    assert len(calls) == 1

    entry = soft.peek('key')
    assert entry.value == 'v1'
    assert entry.generation == 1
    assert entry.soft_expire_time < entry.hard_expire_time

    # The refresh marker is exclusive and the token is generational.
    token = soft.begin_refresh('key')
    assert token is not None
    assert token.generation == 1
    assert soft.refreshing('key')
    assert soft.begin_refresh('key') is None

    # Only the holder can release the marker, and only with its own token.
    forged = dc.RefreshToken('key', token.generation, 'forged-nonce')
    assert soft.abort_refresh(forged) is False
    assert soft.refreshing('key')

    assert soft.commit_refresh(token, 'v2')
    assert soft.get('key') == 'v2'
    assert not soft.refreshing('key')

    # A token naming a superseded generation cannot overwrite the newer value.
    assert soft.commit_refresh(token, 'late') is False
    assert soft.get('key') == 'v2'

    # An unconditional set advances the generation, invalidating stale tokens.
    token = soft.begin_refresh('key')
    assert soft.set('key', 'v3') == 3
    assert soft.commit_refresh(token, 'late-again') is False
    assert soft.get('key') == 'v3'
    assert soft.peek('key').generation == 3

    # Aborting releases only the caller's own marker.
    token = soft.begin_refresh('key')
    assert soft.abort_refresh(token)
    assert not soft.refreshing('key')


def test_soft_expiring_cache_argument_validation(cache):
    with pytest.raises(ValueError):
        dc.SoftExpiringCache(cache, soft_expire=-1, hard_expire=10)

    with pytest.raises(ValueError):
        dc.SoftExpiringCache(cache, soft_expire=10, hard_expire=10)

    with pytest.raises(ValueError):
        dc.SoftExpiringCache(cache, soft_expire=1, hard_expire=10, lease=0)

    # A hard deadline of None means the value is never removed.
    soft = dc.SoftExpiringCache(cache, soft_expire=0.1, hard_expire=None)
    assert soft.get_or_compute('key', lambda: 'v1') == 'v1'
    time.sleep(0.2)
    assert soft.get_or_compute('key', lambda: 'v2') == 'v2'

    entry = soft.peek('key')
    assert entry.value == 'v2'
    assert entry.hard_expire_time is None


def test_soft_expiring_cache_stale_is_served_during_refresh(cache):
    soft = dc.SoftExpiringCache(
        cache,
        soft_expire=0.1,
        hard_expire=60,
        poll_interval=0.01,
    )
    assert soft.get_or_compute('key', lambda: 'v1') == 'v1'
    time.sleep(0.2)  # Value is stale but still readable.

    started = threading.Event()
    release = threading.Event()
    calls = []

    def compute():
        calls.append(1)
        started.set()
        assert release.wait(30)
        return 'v2'

    results = {}

    def worker(name):
        results[name] = soft.get_or_compute('key', compute)

    refresher = threading.Thread(target=worker, args=('refresher',))
    refresher.start()
    assert started.wait(30)

    readers = [
        threading.Thread(target=worker, args=('reader-%d' % num,))
        for num in range(3)
    ]

    for reader in readers:
        reader.start()

    for reader in readers:
        reader.join(30)

    # Readers get the old value instead of queuing on the refresh.
    assert results['reader-0'] == 'v1'
    assert results['reader-1'] == 'v1'
    assert results['reader-2'] == 'v1'

    release.set()
    refresher.join(30)

    assert results['refresher'] == 'v2'
    assert len(calls) == 1  # Exactly one computation for the whole refresh.
    assert soft.get('key') == 'v2'


def test_soft_expiring_cache_compute_outside_transaction(cache):
    soft = dc.SoftExpiringCache(cache, soft_expire=60, hard_expire=300)
    observed = {}

    def compute():
        # No write transaction may be held while the value is computed.
        observed['txn_id'] = cache._txn_id
        cache.set('side-effect', 'done', retry=True)
        return 'value'

    assert soft.get_or_compute('key', compute) == 'value'
    assert observed['txn_id'] is None
    assert cache['side-effect'] == 'done'


def test_soft_expiring_cache_failure_keeps_hard_deadline(cache):
    soft = dc.SoftExpiringCache(
        cache,
        soft_expire=0.1,
        hard_expire=0.6,
        poll_interval=0.01,
    )
    assert soft.get_or_compute('key', lambda: 'v1') == 'v1'
    hard_expire_time = soft.peek('key').hard_expire_time
    time.sleep(0.2)  # Stale but before the hard deadline.

    def fail():
        raise RuntimeError('boom')

    # Failed refreshes fall back to the stale value instead of propagating.
    for _ in range(3):
        assert soft.get_or_compute('key', fail) == 'v1'
        assert soft.peek('key').hard_expire_time == hard_expire_time
        assert not soft.refreshing('key')
        time.sleep(0.05)

    # The hard deadline is never extended by failed refreshes.
    time.sleep(max(0.0, hard_expire_time - time.time()) + 0.1)
    assert soft.get('key') is None

    # Without a stale value the computation error surfaces.
    with pytest.raises(RuntimeError):
        soft.get_or_compute('key', fail)
    assert not soft.refreshing('key')

    # A later successful computation recovers the key.
    assert soft.get_or_compute('key', lambda: 'v2') == 'v2'


def test_soft_expiring_cache_waiter_timeout_keeps_marker(cache):
    soft = dc.SoftExpiringCache(
        cache,
        soft_expire=60,
        hard_expire=300,
        wait_timeout=0.2,
        poll_interval=0.01,
    )
    started = threading.Event()
    release = threading.Event()
    calls = []

    def slow():
        calls.append(1)
        started.set()
        assert release.wait(30)
        return 'winner'

    refresher = threading.Thread(
        target=soft.get_or_compute, args=('key', slow)
    )
    refresher.start()
    assert started.wait(30)

    # The waiter gives up without touching the refresher's marker.
    with pytest.raises(dc.Timeout):
        soft.get_or_compute('key', lambda: 'waiter')
    assert soft.get('key', default='missing') == 'missing'
    assert soft.refreshing('key')

    # A default may be requested instead of an exception.
    assert soft.get_or_compute('key', lambda: 'waiter', default='tired') == (
        'tired'
    )
    assert soft.refreshing('key')

    release.set()
    refresher.join(30)

    assert len(calls) == 1
    assert soft.get('key') == 'winner'
    assert not soft.refreshing('key')


###############################################################################
# Two-process scenarios with controllable compute callbacks.
#
# A worker opens the cache directory itself and calls `get_or_compute` with a
# compute callback that is driven by files in a control directory:
#
# * ``<name>.started`` -- written when compute begins.
# * ``<name>.release`` -- written by the parent to unblock compute.
# * ``<name>.fail``    -- if present when released, compute raises.
# * ``<name>.value``   -- contents returned by compute.
# * ``<name>.result``  -- JSON value returned by ``get_or_compute``.
# * ``<name>.timeout`` -- written when the waiter raised `diskcache.Timeout`.
# * ``<name>.error``   -- exception type name when `get_or_compute` raised.
#
# The number of compute calls is counted across processes with `Cache.incr`.
###############################################################################

COUNT_KEY = ('compute-count',)


def _write_text(path, text):
    temp_path = '%s.%d.tmp' % (path, os.getpid())

    with open(temp_path, 'w', encoding='utf-8') as writer:
        writer.write(text)

    deadline = time.time() + 10.0

    while True:
        try:
            os.replace(temp_path, path)  # Atomic: no empty files are seen.
            return
        except PermissionError:
            # Windows may briefly deny sharing on a freshly read file.
            if time.time() >= deadline:
                raise
            time.sleep(0.01)


def _read_text(path):
    deadline = time.time() + 10.0

    while True:
        try:
            with open(path, encoding='utf-8') as reader:
                return reader.read()
        except PermissionError:
            # Windows may briefly deny sharing on a freshly created file.
            if time.time() >= deadline:
                raise
            time.sleep(0.01)


def _wait_path(path, timeout=120.0):
    deadline = time.time() + timeout

    while not os.path.exists(path):
        if time.time() >= deadline:
            raise TimeoutError('timed out waiting for %s' % (path,))
        time.sleep(0.01)


def _soft_worker(args):
    (cache_dir, control_dir, key, name, soft_expire, hard_expire, lease,
     wait_timeout) = args

    def control(suffix):
        return os.path.join(control_dir, '%s.%s' % (name, suffix))

    cache = dc.Cache(cache_dir)

    try:
        soft = dc.SoftExpiringCache(
            cache,
            soft_expire,
            hard_expire,
            lease=lease,
            wait_timeout=wait_timeout,
            poll_interval=0.01,
        )

        def compute():
            cache.incr(COUNT_KEY, 1, retry=True)
            _write_text(control('started'), name)
            _wait_path(control('release'))
            if os.path.exists(control('fail')):
                raise RuntimeError('compute failed')
            return _read_text(control('value'))

        try:
            result = soft.get_or_compute(key, compute)
        except dc.Timeout:
            _write_text(control('timeout'), name)
        except Exception as exc:  # pylint: disable=broad-except
            _write_text(control('error'), type(exc).__name__)
        else:
            _write_text(control('result'), result)
    finally:
        cache.close()


class SoftWorkers:
    """Spawn and drive worker processes for SoftExpiringCache tests."""

    def __init__(self, tmp_path, cache_dir):
        self.control_dir = str(tmp_path / 'control')
        os.makedirs(self.control_dir)
        self.cache_dir = cache_dir
        self.context = multiprocessing.get_context('spawn')
        self.processes = []

    def control(self, name, suffix):
        return os.path.join(self.control_dir, '%s.%s' % (name, suffix))

    def spawn(
        self,
        name,
        key='metadata',
        soft_expire=60.0,
        hard_expire=300.0,
        lease=None,
        wait_timeout=10.0,
    ):
        args = (
            self.cache_dir,
            self.control_dir,
            key,
            name,
            soft_expire,
            hard_expire,
            lease,
            wait_timeout,
        )
        process = self.context.Process(target=_soft_worker, args=(args,))
        process.start()
        self.processes.append(process)
        return process

    def started(self, name, timeout=120.0):
        _wait_path(self.control(name, 'started'), timeout)

    def release(self, name, value=None, fail=False):
        if value is not None:
            _write_text(self.control(name, 'value'), value)
        if fail:
            _write_text(self.control(name, 'fail'), '1')
        _write_text(self.control(name, 'release'), '1')

    def wait(self, name, process, timeout=120.0):
        process.join(timeout)
        assert process.exitcode == 0, 'worker %s failed' % (name,)

    def result(self, name):
        _wait_path(self.control(name, 'result'))
        return _read_text(self.control(name, 'result'))

    def raised(self, name, kind):
        path = self.control(name, kind)
        _wait_path(path)
        return _read_text(path)

    def close(self):
        for process in self.processes:
            if process.is_alive():
                process.terminate()
                process.join(10)


@pytest.fixture
def workers(tmp_path):
    group = SoftWorkers(tmp_path, str(tmp_path / 'cache'))

    with dc.Cache(group.cache_dir) as cache:
        yield cache, group

    group.close()
    shutil.rmtree(group.cache_dir, ignore_errors=True)


def test_soft_expiry_two_processes_single_flight(workers):
    cache, group = workers
    key = 'metadata'
    soft = dc.SoftExpiringCache(
        cache,
        soft_expire=0.2,
        hard_expire=60,
        lease=30,
        wait_timeout=30,
    )
    assert soft.get_or_compute(key, lambda: 'v1') == 'v1'
    time.sleep(0.3)  # Stale but far from the hard deadline.

    refresher = group.spawn('refresher', key=key, soft_expire=0.2,
                            hard_expire=60, lease=30, wait_timeout=30)
    group.started('refresher')

    waiter = group.spawn('waiter', key=key, soft_expire=0.2,
                         hard_expire=60, lease=30, wait_timeout=30)

    # The waiter must not compute; it reads the stale value and exits.
    assert group.result('waiter') == 'v1'
    group.wait('waiter', waiter)
    assert cache[COUNT_KEY] == 1

    group.release('refresher', value='v2')
    assert group.result('refresher') == 'v2'
    group.wait('refresher', refresher)

    # Exactly one computation happened across both processes.
    assert cache[COUNT_KEY] == 1
    assert soft.get(key) == 'v2'
    assert not soft.refreshing(key)


def test_soft_expiry_two_processes_waiter_timeout(workers):
    cache, group = workers
    key = 'metadata'
    soft = dc.SoftExpiringCache(
        cache,
        soft_expire=60,
        hard_expire=300,
        lease=30,
        wait_timeout=30,
    )
    refresher = group.spawn('refresher', key=key, soft_expire=60,
                            hard_expire=300, lease=30, wait_timeout=30)
    group.started('refresher')

    # No value exists yet: the waiter times out with the marker intact.
    waiter = group.spawn('waiter', key=key, soft_expire=60, hard_expire=300,
                         lease=30, wait_timeout=0.5)
    assert group.raised('waiter', 'timeout') == 'waiter'
    group.wait('waiter', waiter)
    assert soft.refreshing(key)
    assert soft.get(key) is None

    # The refresher still commits and releases its own marker.
    group.release('refresher', value='winner')
    assert group.result('refresher') == 'winner'
    group.wait('refresher', refresher)

    assert cache[COUNT_KEY] == 1
    assert soft.get(key) == 'winner'
    assert not soft.refreshing(key)


def test_soft_expiry_two_processes_failure_fallback(workers):
    cache, group = workers
    key = 'metadata'
    soft = dc.SoftExpiringCache(
        cache,
        soft_expire=0.2,
        hard_expire=1.2,
        lease=30,
        wait_timeout=30,
    )
    assert soft.get_or_compute(key, lambda: 'v1') == 'v1'
    hard_expire_time = soft.peek(key).hard_expire_time
    time.sleep(0.3)

    failing = group.spawn('failing', key=key, soft_expire=0.2,
                          hard_expire=1.2, lease=30, wait_timeout=30)
    group.started('failing')
    group.release('failing', fail=True)

    # The stale value survives the failed refresh and is returned as-is.
    assert group.result('failing') == 'v1'
    group.wait('failing', failing)

    assert cache[COUNT_KEY] == 1
    assert soft.get(key) == 'v1'
    assert soft.peek(key).hard_expire_time == hard_expire_time
    assert not soft.refreshing(key)

    # The hard deadline still applies; failures do not renew the value.
    time.sleep(max(0.0, hard_expire_time - time.time()) + 0.1)
    assert soft.get(key) is None


def test_soft_expiry_two_processes_late_commit_rejected(workers):
    cache, group = workers
    key = 'metadata'
    soft = dc.SoftExpiringCache(
        cache,
        soft_expire=0.2,
        hard_expire=120,
        lease=0.6,
        wait_timeout=60,
    )
    assert soft.get_or_compute(key, lambda: 'v1') == 'v1'
    time.sleep(0.3)

    # The late refresher takes the marker and computes slowly.
    late = group.spawn('late', key=key, soft_expire=0.2, hard_expire=120,
                       lease=0.6, wait_timeout=60)
    group.started('late')

    # After the lease expires another process takes over and commits first.
    time.sleep(0.8)
    early = group.spawn('early', key=key, soft_expire=0.2, hard_expire=120,
                        lease=0.6, wait_timeout=60)
    group.started('early')
    group.release('early', value='from-early')
    assert group.result('early') == 'from-early'
    group.wait('early', early)

    # The late refresher's commit is rejected and the newer value wins.
    group.release('late', value='from-late')
    assert group.result('late') == 'from-early'
    group.wait('late', late)

    assert cache[COUNT_KEY] == 2
    assert soft.get(key) == 'from-early'
    assert soft.peek(key).generation == 2
    assert not soft.refreshing(key)
