"""Mixin helpers.

At least for now, the implementation prefers clarity and performance
over ease of maintenance, thus providing separate wrappers for all
valid combinations of parameters lock, condition and info.

"""

# pyright: reportAttributeAccessIssue=false

__all__ = ()

from ._negative import _NegativeCache


class _BasicMixin:
    def __init__(self, func, negative=None, timer=None):
        self.__func = func
        self.__negative = _NegativeCache(negative, timer) if negative else None

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)
        negative = self.__negative

        try:
            return cache[key]
        except KeyError:
            pass  # key not found
        if negative is not None:
            result = negative.get(key)
            if result is not None:
                _NegativeCache.raise_result(result)
        try:
            val = self.__func(*args, **kwargs)
        except Exception as exc:
            if negative is not None:
                negative.store(key, exc)
            raise
        if negative is not None:
            negative.discard(key)
        try:
            cache[key] = val
        except ValueError:
            pass  # value too large
        return val

    def cache_clear(self):
        self.cache.clear()
        if self.__negative is not None:
            self.__negative.clear()


class _LockedMixin:
    def __init__(self, func, negative=None, timer=None):
        self.__func = func
        self.__negative = _NegativeCache(negative, timer) if negative else None

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)
        lock = self.cache_lock
        negative = self.__negative

        with lock:
            try:
                return cache[key]
            except KeyError:
                pass  # key not found
            if negative is not None:
                result = negative.get(key)
                if result is not None:
                    _NegativeCache.raise_result(result)
        try:
            val = self.__func(*args, **kwargs)
            error = None
        except Exception as exc:  # noqa: BLE001 - policy decides what is cached
            val = None
            error = exc
        with lock:
            if error is None:
                if negative is not None:
                    negative.discard(key)
                try:
                    # In case of a race condition, i.e. if another thread
                    # stored a value for this key while we were calling
                    # self.__func(), prefer the cached value.
                    return cache.setdefault(key, val)
                except ValueError:
                    return val  # value too large
            if negative is not None:
                negative.store(key, error)
        raise error

    def cache_clear(self):
        with self.cache_lock:
            self.cache.clear()
            if self.__negative is not None:
                self.__negative.clear()


class _ConditionMixin:
    def __init__(self, func, negative=None, timer=None):
        self.__func = func
        self.__negative = _NegativeCache(negative, timer) if negative else None
        self.__pending = set()

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)
        lock = self.cache_lock
        cond = self.cache_condition
        negative = self.__negative

        with lock:
            while key in self.__pending:
                cond.wait()
            try:
                return cache[key]
            except KeyError:
                if negative is not None:
                    result = negative.get(key)
                    if result is not None:
                        _NegativeCache.raise_result(result)
                self.__pending.add(key)
        try:
            val = self.__func(*args, **kwargs)
            error = None
        except Exception as exc:  # noqa: BLE001 - policy decides what is cached
            val = None
            error = exc
        try:
            if error is None:
                with lock:
                    if negative is not None:
                        negative.discard(key)
                    try:
                        cache[key] = val
                    except ValueError:
                        pass  # value too large
                return val
        finally:
            with lock:
                self.__pending.remove(key)
                if error is not None and negative is not None:
                    negative.store(key, error)
                cond.notify_all()
        # only reachable when the wrapped function raised an exception
        raise error  # type: ignore[misc]

    def cache_clear(self):
        with self.cache_lock:
            self.cache.clear()
            if self.__negative is not None:
                self.__negative.clear()


class _InfoMixin:
    def __init__(self, func, info, negative=None, timer=None):
        self.__func = func
        self.__info = info
        self.__negative = _NegativeCache(negative, timer) if negative else None
        self.__hits = self.__misses = 0

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)
        negative = self.__negative

        try:
            result = cache[key]
            self.__hits += 1
            return result
        except KeyError:
            pass  # key not found
        if negative is not None:
            result = negative.get(key)
            if result is not None:
                self.__hits += 1
                _NegativeCache.raise_result(result)
        self.__misses += 1
        try:
            val = self.__func(*args, **kwargs)
        except Exception as exc:
            if negative is not None:
                negative.store(key, exc)
            raise
        if negative is not None:
            negative.discard(key)
        try:
            cache[key] = val
        except ValueError:
            pass  # value too large
        return val

    def cache_clear(self):
        self.cache.clear()
        if self.__negative is not None:
            self.__negative.clear()
        self.__hits = self.__misses = 0

    def cache_info(self):
        return self.__info(self.cache, self.__hits, self.__misses)


class _LockedInfoMixin:
    def __init__(self, func, info, negative=None, timer=None):
        self.__func = func
        self.__info = info
        self.__negative = _NegativeCache(negative, timer) if negative else None
        self.__hits = self.__misses = 0

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)
        lock = self.cache_lock
        negative = self.__negative

        with lock:
            try:
                result = cache[key]
                self.__hits += 1
                return result
            except KeyError:
                pass  # key not found
            if negative is not None:
                result = negative.get(key)
                if result is not None:
                    self.__hits += 1
                    _NegativeCache.raise_result(result)
            self.__misses += 1
        try:
            val = self.__func(*args, **kwargs)
            error = None
        except Exception as exc:  # noqa: BLE001 - policy decides what is cached
            val = None
            error = exc
        with lock:
            if error is None:
                if negative is not None:
                    negative.discard(key)
                try:
                    # In case of a race condition, i.e. if another thread
                    # stored a value for this key while we were calling
                    # self.__func(), prefer the cached value.
                    return cache.setdefault(key, val)
                except ValueError:
                    return val  # value too large
            if negative is not None:
                negative.store(key, error)
        raise error

    def cache_clear(self):
        with self.cache_lock:
            self.cache.clear()
            if self.__negative is not None:
                self.__negative.clear()
            self.__hits = self.__misses = 0

    def cache_info(self):
        with self.cache_lock:
            return self.__info(self.cache, self.__hits, self.__misses)


class _ConditionInfoMixin:
    def __init__(self, func, info, negative=None, timer=None):
        self.__func = func
        self.__info = info
        self.__negative = _NegativeCache(negative, timer) if negative else None
        self.__hits = self.__misses = 0
        self.__pending = set()

    def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)
        lock = self.cache_lock
        cond = self.cache_condition
        negative = self.__negative

        with lock:
            while key in self.__pending:
                cond.wait()
            try:
                result = cache[key]
                self.__hits += 1
                return result
            except KeyError:
                if negative is not None:
                    result = negative.get(key)
                    if result is not None:
                        self.__hits += 1
                        _NegativeCache.raise_result(result)
                self.__pending.add(key)
                self.__misses += 1
        try:
            val = self.__func(*args, **kwargs)
            error = None
        except Exception as exc:  # noqa: BLE001 - policy decides what is cached
            val = None
            error = exc
        try:
            if error is None:
                with lock:
                    if negative is not None:
                        negative.discard(key)
                    try:
                        cache[key] = val
                    except ValueError:
                        pass  # value too large
                return val
        finally:
            with lock:
                self.__pending.remove(key)
                if error is not None and negative is not None:
                    negative.store(key, error)
                cond.notify_all()
        # only reachable when the wrapped function raised an exception
        raise error  # type: ignore[misc]

    def cache_clear(self):
        with self.cache_lock:
            self.cache.clear()
            if self.__negative is not None:
                self.__negative.clear()
            self.__hits = self.__misses = 0

    def cache_info(self):
        with self.cache_lock:
            return self.__info(self.cache, self.__hits, self.__misses)
