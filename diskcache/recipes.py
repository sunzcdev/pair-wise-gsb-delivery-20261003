"""Disk Cache Recipes
"""

import functools
import math
import os
import random
import threading
import time
from collections import namedtuple

from .core import ENOVAL, Timeout, args_to_key, full_name


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


SoftEntry = namedtuple(
    'SoftEntry',
    ('value', 'soft_expire_time', 'hard_expire_time', 'generation'),
)


class RefreshToken(namedtuple('RefreshToken', ('key', 'generation', 'nonce'))):
    """Exclusive generational token for a soft-expiry refresh.

    Tokens are created by :meth:`SoftExpiringCache.begin_refresh` and name the
    single thread or process that is allowed to refresh a key. The token must
    be passed back to :meth:`SoftExpiringCache.commit_refresh` or
    :meth:`SoftExpiringCache.abort_refresh`.

    The `generation` is the generation of the cached value the refresher
    intends to replace and the `nonce` distinguishes refreshers from each
    other. A commit is rejected when the cached value has advanced past the
    token's generation, so a late refresher cannot overwrite a newer result.

    """

    __slots__ = ()

    @property
    def marker(self):
        """Return the value used to identify the refresh marker."""
        return (self.generation, self.nonce)


class SoftExpiringCache:
    """Recipe for soft-expiring values with single-flight refresh.

    Every cached value has two deadlines. The *soft* deadline marks when the
    value becomes stale: it can still be read but should be refreshed. The
    *hard* deadline marks when the value is evicted entirely. Between the two
    deadlines readers get the old value while exactly one thread or process
    refreshes it.

    The refresher receives an exclusive :class:`RefreshToken` which records
    the generation of the value being replaced. Computations always run
    outside of any cache write transaction. A commit only succeeds while the
    token still names the newest generation, so a slow refresher cannot
    overwrite a newer result. A failed refresh leaves the old value untouched:
    it remains readable until its hard deadline but the deadline is never
    extended, so repeated failures cannot keep a value alive forever.

    When no value has been stored yet, callers that did not win the refresh
    marker wait for the winner to commit. Waiters may give up after
    `wait_timeout` seconds by raising :exc:`diskcache.Timeout` (or by
    returning `default` when one is given) and never delete another
    refresher's marker.

    Assumes keys will not be evicted by the eviction policy before their hard
    deadline. Set the eviction policy to 'none' on the cache to guarantee keys
    are not evicted early.

    >>> import diskcache
    >>> cache = diskcache.Cache()
    >>> soft = SoftExpiringCache(cache, soft_expire=60, hard_expire=300)
    >>> calls = []
    >>> def fetch():
    ...     calls.append(1)
    ...     return 'v1'
    >>> soft.get_or_compute('meta', fetch)
    'v1'
    >>> soft.get_or_compute('meta', fetch)  # Fresh value, no recompute.
    'v1'
    >>> len(calls)
    1

    The refresh marker is exclusive and the token it hands out is
    generational:

    >>> token = soft.begin_refresh('meta')
    >>> soft.begin_refresh('meta') is None
    True
    >>> (token.generation, soft.commit_refresh(token, 'v2'))
    (1, True)
    >>> soft.get('meta')
    'v2'

    A token naming a superseded generation cannot overwrite a newer result:

    >>> soft.commit_refresh(token, 'late')
    False
    >>> soft.get('meta')
    'v2'

    """

    def __init__(
        self,
        cache,
        soft_expire,
        hard_expire,
        lease=None,
        wait_timeout=10.0,
        poll_interval=0.05,
        name=None,
        tag=None,
        time_func=time.time,
        sleep_func=time.sleep,
    ):
        """Initialize soft-expiring cache recipe.

        :param cache: cache to store values and refresh markers
        :param float soft_expire: seconds until a value becomes stale
        :param float hard_expire: seconds until a value is evicted
            (default None, never evicted)
        :param float lease: seconds the refresh marker is held before another
            refresher may take over (default None, same as `hard_expire`
            when set, else `soft_expire`); should exceed the slowest
            expected computation
        :param float wait_timeout: seconds a waiter on a missing value waits
            before giving up (default 10, None waits forever)
        :param float poll_interval: seconds between polls while waiting
            (default 0.05)
        :param str name: namespace for the stored keys (default None,
            automatic)
        :param str tag: text to associate with stored values (default None)
        :param time_func: function returning the current time in seconds
        :param sleep_func: function sleeping a number of seconds

        """
        if soft_expire is None or soft_expire < 0:
            raise ValueError('soft_expire must be non-negative')

        if hard_expire is not None:
            if hard_expire <= soft_expire:
                raise ValueError(
                    'hard_expire must be greater than soft_expire'
                )

        if lease is not None and lease <= 0:
            raise ValueError('lease must be positive')

        self._cache = cache
        self._soft_expire = soft_expire
        self._hard_expire = hard_expire

        if lease is None:
            lease = soft_expire if hard_expire is None else hard_expire

        self._lease = lease
        self._wait_timeout = wait_timeout
        self._poll_interval = poll_interval
        prefix = (SoftExpiringCache.__name__,) if name is None else (name,)
        self._prefix = prefix
        self._tag = tag
        self._time_func = time_func
        self._sleep_func = sleep_func

    def _value_key(self, key):
        return self._prefix + ('value', key)

    def _refresh_key(self, key):
        return self._prefix + ('refresh', key)

    def peek(self, key):
        """Return :class:`SoftEntry` for `key`, or `None` if missing.

        The entry includes the current value, the absolute soft deadline, the
        absolute hard deadline and the current generation.

        """
        entry, expire_time = self._cache.get(
            self._value_key(key),
            default=ENOVAL,
            expire_time=True,
            retry=True,
        )
        if entry is ENOVAL:
            return None
        value, soft_expire_time, generation = entry
        return SoftEntry(value, soft_expire_time, expire_time, generation)

    def get(self, key, default=None):
        """Return value for `key`, whether fresh or stale, else `default`."""
        entry = self._cache.get(
            self._value_key(key), default=ENOVAL, retry=True
        )
        return default if entry is ENOVAL else entry[0]

    def set(self, key, value):
        """Unconditionally store `value` for `key` and reset the soft deadline.

        The stored generation advances so any in-flight refresher's commit is
        rejected rather than overwriting this value. Returns the new
        generation.

        """
        cache = self._cache
        value_key = self._value_key(key)

        with cache.transact(retry=True):
            entry = cache.get(value_key, default=ENOVAL)
            generation = 0 if entry is ENOVAL else entry[2]
            cache.set(
                value_key,
                (value, self._time_func() + self._soft_expire, generation + 1),
                expire=self._hard_expire,
                tag=self._tag,
            )

        return generation + 1

    def begin_refresh(self, key):
        """Try to become the exclusive refresher for `key`.

        Returns a :class:`RefreshToken` on success and `None` when another
        thread or process already holds the refresh marker. The marker is
        held for at most `lease` seconds so a crashed refresher cannot block
        refreshes forever.

        """
        cache = self._cache
        nonce = '{}-{}'.format(os.getpid(), os.urandom(8).hex())

        with cache.transact(retry=True):
            entry = cache.get(self._value_key(key), default=ENOVAL)
            generation = 0 if entry is ENOVAL else entry[2]
            token = RefreshToken(key, generation, nonce)
            added = cache.add(
                self._refresh_key(key),
                token.marker,
                expire=self._lease,
                retry=True,
            )

        return token if added else None

    def refreshing(self, key):
        """Return `True` when a refresher currently holds the marker."""
        return self._refresh_key(key) in self._cache

    def commit_refresh(self, token, value):
        """Commit `value` for the key named by `token`.

        Returns `True` when the value was committed. Returns `False` when a
        newer generation was already committed or when another refresher
        took over the refresh marker, so a late refresher cannot overwrite a
        newer result. The refresh marker is released only while it is still
        held by `token`.

        """
        cache = self._cache
        value_key = self._value_key(token.key)
        refresh_key = self._refresh_key(token.key)

        with cache.transact(retry=True):
            marker = cache.get(refresh_key, default=ENOVAL, retry=True)

            if marker is not ENOVAL and marker != token.marker:
                return False  # Another refresher took over the marker.

            ours = marker == token.marker
            entry = cache.get(value_key, default=ENOVAL, retry=True)

            if entry is not ENOVAL and entry[2] != token.generation:
                # A newer result was already committed by someone else.
                if ours:
                    cache.delete(refresh_key, retry=True)
                return False

            generation = token.generation if entry is ENOVAL else entry[2]
            cache.set(
                value_key,
                (
                    value,
                    self._time_func() + self._soft_expire,
                    generation + 1,
                ),
                expire=self._hard_expire,
                tag=self._tag,
                retry=True,
            )

            if ours:
                cache.delete(refresh_key, retry=True)

            return True

    def abort_refresh(self, token):
        """Release the refresh marker named by `token` after a failure.

        Returns `True` when the marker was released. A marker acquired by
        another refresher is never deleted.

        """
        cache = self._cache
        refresh_key = self._refresh_key(token.key)

        with cache.transact(retry=True):
            marker = cache.get(refresh_key, default=ENOVAL, retry=True)

            if marker != token.marker:
                return False

            cache.delete(refresh_key, retry=True)
            return True

    def get_or_compute(self, key, compute, default=ENOVAL, wait_timeout=None):
        """Return value for `key`, refreshing it with `compute` when needed.

        A fresh value is returned immediately. When the value is stale, one
        caller recomputes it while every other caller keeps reading the stale
        value. When no value exists, one caller computes it while the others
        wait up to `wait_timeout` seconds. `compute` is always called outside
        of any cache write transaction.

        When `compute` raises, the exception is re-raised unless a stale value
        is still readable; in that case the stale value is returned and is
        preserved until its hard deadline. When a waiter times out, `default`
        is returned if one is given, else :exc:`diskcache.Timeout` is raised.
        Waiters never delete another refresher's marker.

        :param key: key to look up
        :param compute: callable returning a new value
        :param default: value returned when a waiter times out (default
            ENOVAL, raise :exc:`diskcache.Timeout`)
        :param float wait_timeout: seconds to wait for a missing value
            (default None, use the instance value)
        :return: value for key

        """
        cache = self._cache
        value_key = self._value_key(key)
        wait = self._wait_timeout if wait_timeout is None else wait_timeout
        start = self._time_func()
        stale = ENOVAL

        while True:
            entry = cache.get(value_key, default=ENOVAL, retry=True)
            now = self._time_func()

            if entry is not ENOVAL:
                value, soft_expire_time, _ = entry

                if now < soft_expire_time:
                    return value  # Cache hit, value is fresh.

                stale = value

            token = self.begin_refresh(key)

            if token is None:
                # Another thread or process is refreshing the key.
                if stale is not ENOVAL:
                    return stale  # Serve the stale value meanwhile.

                if wait is not None and (now - start) >= wait:
                    # Re-check once: the winner may have just committed.
                    entry = cache.get(value_key, default=ENOVAL, retry=True)

                    if entry is not ENOVAL:
                        return entry[0]

                    if default is not ENOVAL:
                        return default

                    raise Timeout(
                        'timed out waiting for value of key: %r' % (key,)
                    )

                self._sleep_func(self._poll_interval)
                continue

            try:
                value = compute()
            except Exception:
                # Keep the old value untouched but never renew it: it is
                # gone as soon as its hard deadline passes.
                self.abort_refresh(token)
                entry = cache.get(value_key, default=ENOVAL, retry=True)

                if entry is not ENOVAL:
                    return entry[0]

                # The old value is gone, so a failed refresh must raise.
                raise
            except BaseException:
                self.abort_refresh(token)
                raise

            if self.commit_refresh(token, value):
                return value

            # The commit was rejected: a newer result exists or the marker
            # was taken over. Prefer the newest committed value.
            entry = cache.get(value_key, default=ENOVAL, retry=True)

            if entry is not ENOVAL:
                return entry[0]

            return value
