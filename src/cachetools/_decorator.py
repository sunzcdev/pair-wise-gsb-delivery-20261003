"""Mixin helpers.

At least for now, the implementation prefers clarity and performance
over ease of maintenance, thus providing separate wrappers for all
valid combinations of parameters lock, condition and info.

"""

# pyright: reportAttributeAccessIssue=false

__all__ = ()

import threading

_deadlock_message = "deadlock detected: circular wait for pending cached calls"

_thread_local = threading.local()


class _WaitState:
    """Per-thread state used to detect and report pending call cycles."""

    __slots__ = ("waiting", "cond", "failed")

    def __init__(self):
        self.waiting = None  # the pending call the thread is waiting for
        self.cond = None  # the condition variable it is waiting on
        self.failed = False  # set if the thread is part of a cycle


class _Pending:
    """A pending cache key, and the state of the thread computing it."""

    __slots__ = ("state",)

    def __init__(self):
        self.state = _thread_state()


def _thread_state():
    """Return the wait state of the calling thread."""
    state = getattr(_thread_local, "state", None)
    if state is None:
        state = _thread_local.state = _WaitState()
    return state


def _fail(pendings, state):
    """Mark the threads waiting for one of `pendings` as failed."""
    for pending in pendings:
        other = pending.state
        if other is not state and other.waiting is not None:
            other.failed = True
            if other.cond is not None:
                try:
                    other.cond.notify_all()
                except RuntimeError:
                    pass  # condition not associated with the held lock


def _wait(cond, pending, key):
    """Wait until there is no pending call for `key` anymore.

    While waiting, a thread records the pending call it is waiting for,
    so that wait cycles can be detected, i.e. chains of pending calls
    whose computing threads wait for each other, including a thread
    that waits for a call it is computing itself.  Such calls can never
    complete, so a `RuntimeError` is raised instead of blocking forever.
    The other threads waiting for a call on the chain are marked as
    failed and notified, so that they raise as well instead of waiting
    for a value that will never be computed.  Notifying another
    condition variable is safe, since it must guard the same lock;
    later calls will just recompute the values.

    Must be called with the cache lock held.

    """
    state = _thread_state()
    state.failed = False  # drop any stale mark left by an earlier cycle
    entry = pending[key]
    while entry is not None:
        outer = state.waiting, state.cond
        state.waiting, state.cond = entry, cond
        try:
            seen = set()
            curr = entry
            while curr is not None and curr not in seen:
                if curr.state is state:
                    # The chain of waiting threads leads back to this
                    # thread, so none of these calls can ever complete.
                    _fail(seen, state)
                    raise RuntimeError(_deadlock_message)
                seen.add(curr)
                curr = curr.state.waiting
            cond.wait()
        finally:
            state.waiting, state.cond = outer
        # Marked by the thread that detected the cycle.  Checked with
        # the lock held, as cond.wait() reacquires it.
        failed, state.failed = state.failed, False
        if failed:
            raise RuntimeError(_deadlock_message)
        entry = pending.get(key)


class _BasicMixin:
    def __init__(self, func):
        self.__func = func

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)

        try:
            return cache[key]
        except KeyError:
            pass  # key not found
        val = self.__func(*args, **kwargs)
        try:
            cache[key] = val
        except ValueError:
            pass  # value too large
        return val

    def cache_clear(self):
        self.cache.clear()


class _LockedMixin:
    def __init__(self, func):
        self.__func = func

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)
        lock = self.cache_lock

        with lock:
            try:
                return cache[key]
            except KeyError:
                pass  # key not found
        val = self.__func(*args, **kwargs)
        with lock:
            try:
                # In case of a race condition, i.e. if another thread
                # stored a value for this key while we were calling
                # self.__func(), prefer the cached value.
                return cache.setdefault(key, val)
            except ValueError:
                return val  # value too large

    def cache_clear(self):
        with self.cache_lock:
            self.cache.clear()


class _ConditionMixin:
    def __init__(self, func):
        self.__func = func
        self.__pending = {}

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)
        lock = self.cache_lock
        cond = self.cache_condition
        pending = self.__pending

        with lock:
            if key in pending:
                _wait(cond, pending, key)
            try:
                return cache[key]
            except KeyError:
                pending[key] = _Pending()
        try:
            val = self.__func(*args, **kwargs)
            with lock:
                try:
                    cache[key] = val
                except ValueError:
                    pass  # value too large
                return val
        finally:
            with lock:
                del pending[key]
                cond.notify_all()

    def cache_clear(self):
        with self.cache_lock:
            self.cache.clear()


class _InfoMixin:
    def __init__(self, func, info):
        self.__func = func
        self.__info = info
        self.__hits = self.__misses = 0

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)

        try:
            result = cache[key]
            self.__hits += 1
            return result
        except KeyError:
            self.__misses += 1
        val = self.__func(*args, **kwargs)
        try:
            cache[key] = val
        except ValueError:
            pass  # value too large
        return val

    def cache_clear(self):
        self.cache.clear()
        self.__hits = self.__misses = 0

    def cache_info(self):
        return self.__info(self.cache, self.__hits, self.__misses)


class _LockedInfoMixin:
    def __init__(self, func, info):
        self.__func = func
        self.__info = info
        self.__hits = self.__misses = 0

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)
        lock = self.cache_lock

        with lock:
            try:
                result = cache[key]
                self.__hits += 1
                return result
            except KeyError:
                self.__misses += 1
        val = self.__func(*args, **kwargs)
        with lock:
            try:
                # In case of a race condition, i.e. if another thread
                # stored a value for this key while we were calling
                # self.__func(), prefer the cached value.
                return cache.setdefault(key, val)
            except ValueError:
                return val  # value too large

    def cache_clear(self):
        with self.cache_lock:
            self.cache.clear()
            self.__hits = self.__misses = 0

    def cache_info(self):
        with self.cache_lock:
            return self.__info(self.cache, self.__hits, self.__misses)


class _ConditionInfoMixin:
    def __init__(self, func, info):
        self.__func = func
        self.__info = info
        self.__hits = self.__misses = 0
        self.__pending = {}

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)
        lock = self.cache_lock
        cond = self.cache_condition
        pending = self.__pending

        with lock:
            if key in pending:
                _wait(cond, pending, key)
            try:
                result = cache[key]
                self.__hits += 1
                return result
            except KeyError:
                pending[key] = _Pending()
                self.__misses += 1
        try:
            val = self.__func(*args, **kwargs)
            with lock:
                try:
                    cache[key] = val
                except ValueError:
                    pass  # value too large
                return val
        finally:
            with lock:
                del pending[key]
                cond.notify_all()

    def cache_clear(self):
        with self.cache_lock:
            self.cache.clear()
            self.__hits = self.__misses = 0

    def cache_info(self):
        with self.cache_lock:
            return self.__info(self.cache, self.__hits, self.__misses)
