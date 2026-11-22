"""Method decorator helpers.

At least for now, the implementation prefers clarity and performance
over ease of maintenance, thus providing separate wrappers for all
valid combinations of parameters lock, condition and info.

"""

# pyright: reportArgumentType=false, reportAttributeAccessIssue=false

__all__ = ()

import functools
import types

from ._decorator import (
    _BasicMixin,
    _ConditionInfoMixin,
    _ConditionMixin,
    _InfoMixin,
    _LockedInfoMixin,
    _LockedMixin,
)
from ._descriptor import _MethodDescriptor


class _WrapperBase:
    """Wrapper base class providing the public cache attributes as properties."""

    def __init__(self, obj, descriptor, method, cache, key, lock=None, cond=None):
        functools.update_wrapper(self, method)
        self.__obj = obj
        self.__descriptor = descriptor
        self.__cache = cache
        self.__lock = lock if lock is not None else self.__none
        self.__cond = cond if cond is not None else self.__none
        self.cache_key = functools.partial(key, obj)

    def __reduce__(self):
        # Rebuild via the descriptor instead of pickling unpicklable
        # internals, e.g. __wrapped__, lock/cond closures.  Note that
        # this will also reset the hit and miss counts of *Info*
        # wrappers.
        return (self.__descriptor.__get__, (self.__obj,))

    def __repr__(self):
        wrapped = self.__wrapped__
        return f"<cachedmethod {wrapped!r} of {object.__repr__(self.__obj)}>"

    @property
    def cache(self):
        return self.__cache(self.__obj)

    @property
    def cache_lock(self):
        return self.__lock(self.__obj)

    @property
    def cache_condition(self):
        return self.__cond(self.__obj)

    @staticmethod
    def __none(_):
        return None


class _BasicWrapper(_WrapperBase, _BasicMixin):
    def __init__(self, obj, descriptor, method, cache, key, negative=None, timer=None):
        _WrapperBase.__init__(self, obj, descriptor, method, cache, key)
        _BasicMixin.__init__(self, types.MethodType(method, obj), negative, timer)


class _LockedWrapper(_WrapperBase, _LockedMixin):
    def __init__(
        self, obj, descriptor, method, cache, key, lock, negative=None, timer=None
    ):
        _WrapperBase.__init__(self, obj, descriptor, method, cache, key, lock)
        _LockedMixin.__init__(self, types.MethodType(method, obj), negative, timer)


class _ConditionWrapper(_WrapperBase, _ConditionMixin):
    def __init__(
        self,
        obj,
        descriptor,
        method,
        cache,
        key,
        lock,
        cond,
        negative=None,
        timer=None,
    ):
        _WrapperBase.__init__(self, obj, descriptor, method, cache, key, lock, cond)
        _ConditionMixin.__init__(self, types.MethodType(method, obj), negative, timer)


class _InfoWrapper(_WrapperBase, _InfoMixin):
    def __init__(
        self, obj, descriptor, method, cache, key, info, negative=None, timer=None
    ):
        _WrapperBase.__init__(self, obj, descriptor, method, cache, key)
        _InfoMixin.__init__(self, types.MethodType(method, obj), info, negative, timer)


class _LockedInfoWrapper(_WrapperBase, _LockedInfoMixin):
    def __init__(
        self,
        obj,
        descriptor,
        method,
        cache,
        key,
        lock,
        info,
        negative=None,
        timer=None,
    ):
        _WrapperBase.__init__(self, obj, descriptor, method, cache, key, lock)
        _LockedInfoMixin.__init__(
            self, types.MethodType(method, obj), info, negative, timer
        )


class _ConditionInfoWrapper(_WrapperBase, _ConditionInfoMixin):
    def __init__(
        self,
        obj,
        descriptor,
        method,
        cache,
        key,
        lock,
        cond,
        info,
        negative=None,
        timer=None,
    ):
        _WrapperBase.__init__(self, obj, descriptor, method, cache, key, lock, cond)
        _ConditionInfoMixin.__init__(
            self, types.MethodType(method, obj), info, negative, timer
        )


def _wrapper(
    method, cache, key, lock=None, cond=None, info=None, negative=None, timer=None
):
    if info is not None:
        if cond is not None and lock is not None:
            wrapper = lambda obj, descriptor: _ConditionInfoWrapper(
                obj, descriptor, method, cache, key, lock, cond, info, negative, timer
            )
        elif cond is not None:
            wrapper = lambda obj, descriptor: _ConditionInfoWrapper(
                obj, descriptor, method, cache, key, cond, cond, info, negative, timer
            )
        elif lock is not None:
            wrapper = lambda obj, descriptor: _LockedInfoWrapper(
                obj, descriptor, method, cache, key, lock, info, negative, timer
            )
        else:
            wrapper = lambda obj, descriptor: _InfoWrapper(
                obj, descriptor, method, cache, key, info, negative, timer
            )
    else:
        if cond is not None and lock is not None:
            wrapper = lambda obj, descriptor: _ConditionWrapper(
                obj, descriptor, method, cache, key, lock, cond, negative, timer
            )
        elif cond is not None:
            wrapper = lambda obj, descriptor: _ConditionWrapper(
                obj, descriptor, method, cache, key, cond, cond, negative, timer
            )
        elif lock is not None:
            wrapper = lambda obj, descriptor: _LockedWrapper(
                obj, descriptor, method, cache, key, lock, negative, timer
            )
        else:
            wrapper = lambda obj, descriptor: _BasicWrapper(
                obj, descriptor, method, cache, key, negative, timer
            )
    descriptor = _MethodDescriptor(wrapper)
    return functools.update_wrapper(descriptor, method)
