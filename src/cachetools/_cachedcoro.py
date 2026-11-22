"""Asynchronous function decorator helpers."""

# pyright: reportArgumentType=false, reportAttributeAccessIssue=false

__all__ = ()

import functools
import inspect
import types

from ._decorator_async import _BasicMixin, _InfoMixin


class _WrapperBase:
    """Wrapper base class providing the public cache attributes."""

    def __init__(self, func, cache, key):
        if not inspect.iscoroutinefunction(func):
            raise TypeError("cachedcoro can only wrap coroutine functions") from None
        # Update first, so func.__dict__ cannot override wrapper state,
        # e.g. when stacking decorators.
        functools.update_wrapper(self, func)
        self.cache = cache
        self.cache_key = key

    # descriptor, copy and pickle support as for functools.lru_cache (C version)
    def __get__(self, obj, objtype=None):
        return self if obj is None else types.MethodType(self, obj)

    def __copy__(self):
        return self

    def __deepcopy__(self, memo):
        return self

    def __reduce__(self):
        return self.__qualname__

    def __repr__(self):
        return f"<cachedcoro {self.__wrapped__!r}>"


class _BasicWrapper(_WrapperBase, _BasicMixin):
    def __init__(self, func, cache, key):
        _WrapperBase.__init__(self, func, cache, key)
        _BasicMixin.__init__(self, func)


class _InfoWrapper(_WrapperBase, _InfoMixin):
    def __init__(self, func, cache, key, info):
        _WrapperBase.__init__(self, func, cache, key)
        _InfoMixin.__init__(self, func, info)


def _wrapper(func, cache, key, info=None):
    if info is not None:
        return _InfoWrapper(func, cache, key, info)
    return _BasicWrapper(func, cache, key)
