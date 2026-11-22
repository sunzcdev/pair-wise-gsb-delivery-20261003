"""Asynchronous mixin helpers.

Unlike the synchronous wrappers, these do not need `lock` or
`condition` variants:  an asyncio event loop runs its tasks
cooperatively, and the pending futures below provide both mutual
exclusion and the notification of waiters.

"""

# pyright: reportAttributeAccessIssue=false

__all__ = ()

import asyncio


def _store(future, cache, key, pending):
    # Runs when the shared lookup task completes.  A done callback
    # cannot be canceled, so the result is stored exactly once even if
    # the call that started the lookup was canceled while awaiting it.
    pending.pop(key, None)
    if future.cancelled():
        return
    exc = future.exception()
    if exc is None:
        try:
            cache[key] = future.result()
        except ValueError:
            pass  # value too large
    # On failure the exception has now been marked as retrieved, which
    # also prevents a "task exception was never retrieved" warning, and
    # nothing is cached so the next call looks up again.


class _BasicMixin:
    def __init__(self, func):
        self.__func = func
        self.__pending = {}

    async def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)

        try:
            return cache[key]
        except KeyError:
            pass  # key not found
        future = self.__pending.get(key)
        if future is None:
            # The lookup runs in its own task, awaited through a shield
            # below.  Canceling any caller only cancels its own shield,
            # never the shared task, so concurrent calls share one
            # lookup and all remaining waiters still finish.
            future = asyncio.Task(self.__func(*args, **kwargs))
            self.__pending[key] = future
            future.add_done_callback(
                lambda future: _store(future, cache, key, self.__pending)
            )
        return await asyncio.shield(future)

    def cache_clear(self):
        self.cache.clear()


class _InfoMixin:
    def __init__(self, func, info):
        self.__func = func
        self.__info = info
        self.__hits = self.__misses = 0
        self.__pending = {}

    async def __call__(self, /, *args, **kwargs):
        cache = self.cache
        key = self.cache_key(*args, **kwargs)

        try:
            result = cache[key]
            self.__hits += 1
            return result
        except KeyError:
            pass  # key not found
        future = self.__pending.get(key)
        if future is None:
            # Only the call that actually starts a lookup counts as a
            # miss; waiters joining it neither hit nor miss.
            self.__misses += 1
            future = asyncio.Task(self.__func(*args, **kwargs))
            self.__pending[key] = future
            future.add_done_callback(
                lambda future: _store(future, cache, key, self.__pending)
            )
        return await asyncio.shield(future)

    def cache_clear(self):
        self.cache.clear()
        self.__hits = self.__misses = 0

    def cache_info(self):
        return self.__info(self.cache, self.__hits, self.__misses)
