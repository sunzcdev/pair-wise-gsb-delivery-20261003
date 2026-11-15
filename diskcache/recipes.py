"""Disk Cache Recipes
"""

import functools
import math
import os
import random
import threading
import time
import uuid

from collections import namedtuple

from .core import ENOVAL, args_to_key, full_name


class Averager:
    """Recipe for calculating a running average.

    Sometimes known as "online statistics," the running average maintains the
    total and count. The average can then be calculated at any time.

    Assumes the key will not be evicted. Set the eviction policy to 'none' on
    the cache to guarantee the key is not evicted.

    >>> import diskcache
    >>> cache = diskcache.FanoutCache()
    >>> ave = Averager(cache, 'latency')
    >>> ave.add(0.080)
    >>> ave.add(0.120)
    >>> ave.get()
    0.1
    >>> ave.add(0.160)
    >>> ave.pop()
    0.12
    >>> print(ave.get())
    None

    """

    def __init__(self, cache, key, expire=None, tag=None):
        self._cache = cache
        self._key = key
        self._expire = expire
        self._tag = tag

    def add(self, value):
        """Add `value` to average."""
        with self._cache.transact(retry=True):
            total, count = self._cache.get(self._key, default=(0.0, 0))
            total += value
            count += 1
            self._cache.set(
                self._key,
                (total, count),
                expire=self._expire,
                tag=self._tag,
            )

    def get(self):
        """Get current average or return `None` if count equals zero."""
        total, count = self._cache.get(self._key, default=(0.0, 0), retry=True)
        return None if count == 0 else total / count

    def pop(self):
        """Return current average and delete key."""
        total, count = self._cache.pop(self._key, default=(0.0, 0), retry=True)
        return None if count == 0 else total / count


class Lock:
    """Recipe for cross-process and cross-thread lock.

    Assumes the key will not be evicted. Set the eviction policy to 'none' on
    the cache to guarantee the key is not evicted.

    >>> import diskcache
    >>> cache = diskcache.Cache()
    >>> lock = Lock(cache, 'report-123')
    >>> lock.acquire()
    >>> lock.release()
    >>> with lock:
    ...     pass

    """

    def __init__(self, cache, key, expire=None, tag=None):
        self._cache = cache
        self._key = key
        self._expire = expire
        self._tag = tag

    def acquire(self):
        """Acquire lock using spin-lock algorithm."""
        while True:
            added = self._cache.add(
                self._key,
                None,
                expire=self._expire,
                tag=self._tag,
                retry=True,
            )
            if added:
                break
            time.sleep(0.001)

    def release(self):
        """Release lock by deleting key."""
        self._cache.delete(self._key, retry=True)

    def locked(self):
        """Return true if the lock is acquired."""
        return self._key in self._cache

    def __enter__(self):
        self.acquire()

    def __exit__(self, *exc_info):
        self.release()


class RLock:
    """Recipe for cross-process and cross-thread re-entrant lock.

    Assumes the key will not be evicted. Set the eviction policy to 'none' on
    the cache to guarantee the key is not evicted.

    >>> import diskcache
    >>> cache = diskcache.Cache()
    >>> rlock = RLock(cache, 'user-123')
    >>> rlock.acquire()
    >>> rlock.acquire()
    >>> rlock.release()
    >>> with rlock:
    ...     pass
    >>> rlock.release()
    >>> rlock.release()
    Traceback (most recent call last):
      ...
    AssertionError: cannot release un-acquired lock

    """

    def __init__(self, cache, key, expire=None, tag=None):
        self._cache = cache
        self._key = key
        self._expire = expire
        self._tag = tag

    def acquire(self):
        """Acquire lock by incrementing count using spin-lock algorithm."""
        pid = os.getpid()
        tid = threading.get_ident()
        pid_tid = '{}-{}'.format(pid, tid)

        while True:
            with self._cache.transact(retry=True):
                value, count = self._cache.get(self._key, default=(None, 0))
                if pid_tid == value or count == 0:
                    self._cache.set(
                        self._key,
                        (pid_tid, count + 1),
                        expire=self._expire,
                        tag=self._tag,
                    )
                    return
            time.sleep(0.001)

    def release(self):
        """Release lock by decrementing count."""
        pid = os.getpid()
        tid = threading.get_ident()
        pid_tid = '{}-{}'.format(pid, tid)

        with self._cache.transact(retry=True):
            value, count = self._cache.get(self._key, default=(None, 0))
            is_owned = pid_tid == value and count > 0
            assert is_owned, 'cannot release un-acquired lock'
            self._cache.set(
                self._key,
                (value, count - 1),
                expire=self._expire,
                tag=self._tag,
            )

    def __enter__(self):
        self.acquire()

    def __exit__(self, *exc_info):
        self.release()


class BoundedSemaphore:
    """Recipe for cross-process and cross-thread bounded semaphore.

    Assumes the key will not be evicted. Set the eviction policy to 'none' on
    the cache to guarantee the key is not evicted.

    >>> import diskcache
    >>> cache = diskcache.Cache()
    >>> semaphore = BoundedSemaphore(cache, 'max-cons', value=2)
    >>> semaphore.acquire()
    >>> semaphore.acquire()
    >>> semaphore.release()
    >>> with semaphore:
    ...     pass
    >>> semaphore.release()
    >>> semaphore.release()
    Traceback (most recent call last):
      ...
    AssertionError: cannot release un-acquired semaphore

    """

    def __init__(self, cache, key, value=1, expire=None, tag=None):
        self._cache = cache
        self._key = key
        self._value = value
        self._expire = expire
        self._tag = tag

    def acquire(self):
        """Acquire semaphore by decrementing value using spin-lock algorithm."""
        while True:
            with self._cache.transact(retry=True):
                value = self._cache.get(self._key, default=self._value)
                if value > 0:
                    self._cache.set(
                        self._key,
                        value - 1,
                        expire=self._expire,
                        tag=self._tag,
                    )
                    return
            time.sleep(0.001)

    def release(self):
        """Release semaphore by incrementing value."""
        with self._cache.transact(retry=True):
            value = self._cache.get(self._key, default=self._value)
            assert self._value > value, 'cannot release un-acquired semaphore'
            value += 1
            self._cache.set(
                self._key,
                value,
                expire=self._expire,
                tag=self._tag,
            )

    def __enter__(self):
        self.acquire()

    def __exit__(self, *exc_info):
        self.release()


RefreshToken = namedtuple('RefreshToken', ('generation', 'nonce'))


class SoftExpiringCache:
    """Recipe for cached values with separate soft and hard expiration.

    The soft expiration time separates the period in which a stored value is
    fresh from the period in which it is stale but may still be served. The
    hard expiration time is the deadline after which the value is discarded
    entirely. While a value is stale, a single process or thread is elected
    to refresh it and all others continue to serve the old value.

    Refreshers receive an exclusive generation token from `begin_refresh`.
    The token proves the refresh lease is still held when `commit_refresh`
    stores the computed value. Computation therefore happens outside the
    cache's write transaction and late refreshers cannot overwrite newer
    results. If computation fails, `abort_refresh` releases the lease and
    the old value is kept until the hard expiration time. Failures never
    extend the hard deadline so values are not kept alive indefinitely.

    When no value is stored, `get_or_compute` callers that do not win the
    refresh lease wait for the winner to commit. Waiters may exit early with
    `wait_timeout` and never delete another process's refresh marker.

    Assumes keys will not be evicted. Set the eviction policy to 'none' on
    the cache to guarantee keys are not evicted.

    >>> import diskcache
    >>> cache = diskcache.Cache()
    >>> sec = SoftExpiringCache(cache, soft_expire=60, hard_expire=120)
    >>> sec.set('color', 'blue')
    >>> sec.get('color')
    'blue'
    >>> sec.peek('color')
    ('blue', 'fresh')
    >>> sec.get_or_compute('color', lambda: 'red')  # fresh, not computed
    'blue'

    Only the token holder may commit a refresh. Committing twice fails
    because the first commit releases the lease:

    >>> token = sec.begin_refresh('color')
    >>> sec.commit_refresh('color', token, 'red')
    True
    >>> sec.commit_refresh('color', token, 'green')  # late commit rejected
    False
    >>> sec.get('color')
    'red'

    While another process holds the refresh lease, waiters may time out
    without disturbing the lease:

    >>> other = sec.begin_refresh('missing')
    >>> sec.get_or_compute('missing', lambda: 'v', default='timeout', wait_timeout=0)
    'timeout'
    >>> sec.begin_refresh('missing') is None  # lease still held by `other`
    True
    >>> sec.abort_refresh('missing', other)
    True

    Failed computations keep the old value until the hard expiration time:

    >>> stale = SoftExpiringCache(cache, soft_expire=0, hard_expire=60, name='stale')
    >>> stale.set('n', 1)
    >>> stale.get_or_compute('n', lambda: 2)
    2
    >>> def boom():
    ...     raise ValueError('boom')
    >>> stale.get_or_compute('n', boom)
    2
    >>> stale.peek('n')
    (2, 'stale')

    :param cache: cache to store values and refresh markers
    :param float soft_expire: seconds until stored value is stale
    :param float hard_expire: seconds until stored value is discarded
    :param float refresh_expire: seconds the refresh lease is held (default
        None, same as `hard_expire`)
    :param float failure_backoff: seconds to delay refresh retries after a
        failed computation (default 0, no delay)
    :param str name: name used to namespace keys (default None)
    :param str tag: text to associate with stored values (default None)

    """

    def __init__(
        self,
        cache,
        soft_expire,
        hard_expire,
        refresh_expire=None,
        failure_backoff=0,
        name=None,
        tag=None,
    ):
        assert 0 <= soft_expire <= hard_expire
        self._cache = cache
        self._soft_expire = soft_expire
        self._hard_expire = hard_expire
        self._refresh_expire = (
            hard_expire if refresh_expire is None else refresh_expire
        )
        self._failure_backoff = failure_backoff
        self._name = name
        self._tag = tag

    def _data_key(self, key):
        return ('diskcache.SoftExpiringCache', self._name, key)

    def _marker_key(self, key):
        return ('diskcache.SoftExpiringCache', self._name, key, ENOVAL)

    def set(self, key, value):
        """Store `value` for `key` and reset soft and hard expiration times.

        :param key: key for item
        :param value: value to store for key

        """
        cache = self._cache
        data_key = self._data_key(key)
        soft_deadline = time.time() + self._soft_expire

        with cache.transact(retry=True):
            record = cache.get(data_key, default=ENOVAL)
            generation = 0 if record is ENOVAL else record[2] + 1
            cache.set(
                data_key,
                (value, soft_deadline, generation),
                expire=self._hard_expire,
                tag=self._tag,
            )

    def get(self, key, default=None):
        """Return stored value for `key`, whether fresh or stale.

        Returns `default` if no value is stored or the hard expiration time
        has passed.

        :param key: key for item
        :param default: value to return if key is missing (default None)
        :return: value for item or default

        """
        record = self._cache.get(self._data_key(key), default=ENOVAL, retry=True)
        return default if record is ENOVAL else record[0]

    def peek(self, key, default=None):
        """Return (value, status) pair for `key`.

        Status is 'fresh' before the soft expiration time, 'stale' between
        the soft and hard expiration times, and 'miss' when no value is
        stored.

        :param key: key for item
        :param default: value to return if key is missing (default None)
        :return: (value, status) pair

        """
        record = self._cache.get(self._data_key(key), default=ENOVAL, retry=True)
        if record is ENOVAL:
            return default, 'miss'
        value, soft_deadline, _ = record
        status = 'fresh' if time.time() < soft_deadline else 'stale'
        return value, status

    def delete(self, key):
        """Delete stored value and refresh marker for `key`.

        :param key: key for item

        """
        with self._cache.transact(retry=True):
            self._cache.delete(self._data_key(key))
            self._cache.delete(self._marker_key(key))

    def begin_refresh(self, key):
        """Begin a refresh of `key` and return an exclusive refresh token.

        Returns None if another process or thread already holds the refresh
        lease for `key`. The lease is held for at most `refresh_expire`
        seconds so a crashed refresher cannot block refreshes forever.

        :param key: key for item
        :return: RefreshToken or None

        """
        cache = self._cache
        record = cache.get(self._data_key(key), default=ENOVAL, retry=True)
        generation = None if record is ENOVAL else record[2]
        nonce = uuid.uuid4().hex
        added = cache.add(
            self._marker_key(key),
            nonce,
            expire=self._refresh_expire,
            retry=True,
        )
        return RefreshToken(generation, nonce) if added else None

    def commit_refresh(self, key, token, value):
        """Commit `value` for `key` using `token` from `begin_refresh`.

        The commit is atomic and succeeds only while the refresh lease is
        still held by `token` and no newer value was committed since the
        refresh began. Late refreshers therefore cannot overwrite newer
        results. A successful commit releases the refresh lease and resets
        both soft and hard expiration times.

        :param key: key for item
        :param token: RefreshToken from `begin_refresh`
        :param value: computed value to store for key
        :return: True if value was committed

        """
        cache = self._cache
        data_key = self._data_key(key)
        marker_key = self._marker_key(key)
        soft_deadline = time.time() + self._soft_expire

        with cache.transact(retry=True):
            nonce = cache.get(marker_key, default=ENOVAL)
            if nonce != token.nonce:
                return False
            record = cache.get(data_key, default=ENOVAL)
            generation = None if record is ENOVAL else record[2]
            if generation != token.generation:
                return False
            cache.set(
                data_key,
                (value, soft_deadline, 0 if generation is None else generation + 1),
                expire=self._hard_expire,
                tag=self._tag,
            )
            cache.delete(marker_key)
            return True

    def abort_refresh(self, key, token):
        """Abort a refresh of `key` using `token` from `begin_refresh`.

        The stored value is not modified and keeps its original hard
        expiration time. The refresh lease is released unless
        `failure_backoff` is set, in which case the lease is held for the
        backoff duration to delay retries.

        :param key: key for item
        :param token: RefreshToken from `begin_refresh`
        :return: True if the refresh lease was held by token

        """
        cache = self._cache
        marker_key = self._marker_key(key)

        with cache.transact(retry=True):
            nonce = cache.get(marker_key, default=ENOVAL)
            if nonce != token.nonce:
                return False
            if self._failure_backoff:
                cache.set(marker_key, nonce, expire=self._failure_backoff)
            else:
                cache.delete(marker_key)
            return True

    def get_or_compute(
        self,
        key,
        compute,
        default=None,
        wait_timeout=None,
        poll_interval=0.01,
    ):
        """Return stored value for `key`, computing and storing it if needed.

        Fresh values are returned immediately. While stale, one caller is
        elected to recompute the value and all others keep serving the old
        value. Computation happens outside the cache's write transaction. If
        computation fails, the old value is returned and kept until the hard
        expiration time.

        When no value is stored, the elected caller computes the value and
        other callers wait for it to be committed. Waiters exit with
        `default` after `wait_timeout` seconds and never delete another
        process's refresh marker.

        :param key: key for item
        :param compute: callable of no arguments that computes the value
        :param default: value to return on wait timeout (default None)
        :param float wait_timeout: seconds to wait for another process to
            commit a missing value (default None, wait forever)
        :param float poll_interval: seconds between polls while waiting
            (default 0.01)
        :return: value for item or default

        """
        cache = self._cache
        data_key = self._data_key(key)
        record = cache.get(data_key, default=ENOVAL, retry=True)

        if record is ENOVAL:
            token = self.begin_refresh(key)

            if token is not None:
                try:
                    value = compute()
                except BaseException:
                    self.abort_refresh(key, token)
                    raise
                self.commit_refresh(key, token, value)
                return value

            # Another process is refreshing. Wait for it to commit. Waiters
            # may time out and never delete the other process's marker.
            deadline = (
                None if wait_timeout is None else time.time() + wait_timeout
            )

            while True:
                record = cache.get(data_key, default=ENOVAL, retry=True)
                if record is not ENOVAL:
                    return record[0]
                if deadline is not None and time.time() >= deadline:
                    return default
                time.sleep(poll_interval)

        value, soft_deadline, _ = record

        if time.time() < soft_deadline:
            return value

        token = self.begin_refresh(key)

        if token is None:
            return value  # Serve stale while another process refreshes.

        try:
            new_value = compute()
        except BaseException:
            self.abort_refresh(key, token)
            return value  # Keep old value until hard expiration time.

        self.commit_refresh(key, token, new_value)
        return new_value


def throttle(
    cache,
    count,
    seconds,
    name=None,
    expire=None,
    tag=None,
    time_func=time.time,
    sleep_func=time.sleep,
):
    """Decorator to throttle calls to function.

    Assumes keys will not be evicted. Set the eviction policy to 'none' on the
    cache to guarantee the keys are not evicted.

    >>> import diskcache, time
    >>> cache = diskcache.Cache()
    >>> count = 0
    >>> @throttle(cache, 2, 1)  # 2 calls per 1 second
    ... def increment():
    ...     global count
    ...     count += 1
    >>> start = time.time()
    >>> while (time.time() - start) <= 2:
    ...     increment()
    >>> count in (6, 7)  # 6 or 7 calls depending on CPU load
    True

    """

    def decorator(func):
        rate = count / float(seconds)
        key = full_name(func) if name is None else name
        now = time_func()
        cache.set(key, (now, count), expire=expire, tag=tag, retry=True)

        @functools.wraps(func)
        def wrapper(*args, **kwargs):
            while True:
                with cache.transact(retry=True):
                    last, tally = cache.get(key)
                    now = time_func()
                    tally += (now - last) * rate
                    delay = 0

                    if tally > count:
                        cache.set(key, (now, count - 1), expire)
                    elif tally >= 1:
                        cache.set(key, (now, tally - 1), expire)
                    else:
                        delay = (1 - tally) / rate

                if delay:
                    sleep_func(delay)
                else:
                    break

            return func(*args, **kwargs)

        return wrapper

    return decorator


def barrier(cache, lock_factory, name=None, expire=None, tag=None):
    """Barrier to calling decorated function.

    Supports different kinds of locks: Lock, RLock, BoundedSemaphore.

    Assumes keys will not be evicted. Set the eviction policy to 'none' on the
    cache to guarantee the keys are not evicted.

    >>> import diskcache, time
    >>> cache = diskcache.Cache()
    >>> @barrier(cache, Lock)
    ... def work(num):
    ...     print('worker started')
    ...     time.sleep(1)
    ...     print('worker finished')
    >>> import multiprocessing.pool
    >>> pool = multiprocessing.pool.ThreadPool(2)
    >>> _ = pool.map(work, range(2))
    worker started
    worker finished
    worker started
    worker finished
    >>> pool.terminate()

    """

    def decorator(func):
        key = full_name(func) if name is None else name
        lock = lock_factory(cache, key, expire=expire, tag=tag)

        @functools.wraps(func)
        def wrapper(*args, **kwargs):
            with lock:
                return func(*args, **kwargs)

        return wrapper

    return decorator


def memoize_stampede(
    cache, expire, name=None, typed=False, tag=None, beta=1, ignore=()
):
    """Memoizing cache decorator with cache stampede protection.

    Cache stampedes are a type of system overload that can occur when parallel
    computing systems using memoization come under heavy load. This behaviour
    is sometimes also called dog-piling, cache miss storm, cache choking, or
    the thundering herd problem.

    The memoization decorator implements cache stampede protection through
    early recomputation. Early recomputation of function results will occur
    probabilistically before expiration in a background thread of
    execution. Early probabilistic recomputation is based on research by
    Vattani, A.; Chierichetti, F.; Lowenstein, K. (2015), Optimal Probabilistic
    Cache Stampede Prevention, VLDB, pp. 886-897, ISSN 2150-8097

    If name is set to None (default), the callable name will be determined
    automatically.

    If typed is set to True, function arguments of different types will be
    cached separately. For example, f(3) and f(3.0) will be treated as distinct
    calls with distinct results.

    The original underlying function is accessible through the `__wrapped__`
    attribute. This is useful for introspection, for bypassing the cache, or
    for rewrapping the function with a different cache.

    >>> from diskcache import Cache
    >>> cache = Cache()
    >>> @memoize_stampede(cache, expire=1)
    ... def fib(number):
    ...     if number == 0:
    ...         return 0
    ...     elif number == 1:
    ...         return 1
    ...     else:
    ...         return fib(number - 1) + fib(number - 2)
    >>> print(fib(100))
    354224848179261915075

    An additional `__cache_key__` attribute can be used to generate the cache
    key used for the given arguments.

    >>> key = fib.__cache_key__(100)
    >>> del cache[key]

    Remember to call memoize when decorating a callable. If you forget, then a
    TypeError will occur.

    :param cache: cache to store callable arguments and return values
    :param float expire: seconds until arguments expire
    :param str name: name given for callable (default None, automatic)
    :param bool typed: cache different types separately (default False)
    :param str tag: text to associate with arguments (default None)
    :param set ignore: positional or keyword args to ignore (default ())
    :return: callable decorator

    """
    # Caution: Nearly identical code exists in Cache.memoize
    def decorator(func):
        """Decorator created by memoize call for callable."""
        base = (full_name(func),) if name is None else (name,)

        def timer(*args, **kwargs):
            """Time execution of `func` and return result and time delta."""
            start = time.time()
            result = func(*args, **kwargs)
            delta = time.time() - start
            return result, delta

        @functools.wraps(func)
        def wrapper(*args, **kwargs):
            """Wrapper for callable to cache arguments and return values."""
            key = wrapper.__cache_key__(*args, **kwargs)
            pair, expire_time = cache.get(
                key,
                default=ENOVAL,
                expire_time=True,
                retry=True,
            )

            if pair is not ENOVAL:
                result, delta = pair
                now = time.time()
                ttl = expire_time - now

                if (-delta * beta * math.log(random.random())) < ttl:
                    return result  # Cache hit.

                # Check whether a thread has started for early recomputation.

                thread_key = key + (ENOVAL,)
                thread_added = cache.add(
                    thread_key,
                    None,
                    expire=delta,
                    retry=True,
                )

                if thread_added:
                    # Start thread for early recomputation.
                    def recompute():
                        with cache:
                            pair = timer(*args, **kwargs)
                            cache.set(
                                key,
                                pair,
                                expire=expire,
                                tag=tag,
                                retry=True,
                            )

                    thread = threading.Thread(target=recompute)
                    thread.daemon = True
                    thread.start()

                return result

            pair = timer(*args, **kwargs)
            cache.set(key, pair, expire=expire, tag=tag, retry=True)
            return pair[0]

        def __cache_key__(*args, **kwargs):
            """Make key for cache given function arguments."""
            return args_to_key(base, args, kwargs, typed, ignore)

        wrapper.__cache_key__ = __cache_key__
        return wrapper

    return decorator
