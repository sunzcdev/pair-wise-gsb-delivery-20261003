import asyncio
import unittest
from dataclasses import dataclass
from typing import Any, Protocol

import cachetools
import cachetools.keys


class Gate:
    """Control when a lookup coroutine is allowed to complete."""

    def __init__(self):
        self.started = asyncio.Event()
        self.release = asyncio.Event()

    async def wait(self) -> None:
        self.started.set()
        await self.release.wait()


@dataclass
class Counter:
    """Mutable call counter shared between a coroutine and its tests."""

    value: int = 0


def make_coroutine(counter: Counter, gate: Gate | None = None, error: bool = False):
    async def coroutine(*args: Any, **kwargs: Any) -> Any:
        counter.value += 1
        if gate is not None:
            await gate.wait()
        if error:
            raise ValueError("test error")
        return (counter.value, args, tuple(sorted(kwargs.items())))

    return coroutine


class CachedCoroAssertions(Protocol):
    """The unittest assertion helpers used by the shared test mixin."""

    def assertEqual(self, first: Any, second: Any) -> None: ...
    def assertNotEqual(self, first: Any, second: Any) -> None: ...
    def assertTrue(self, expr: Any) -> None: ...
    def assertFalse(self, expr: Any) -> None: ...
    def assertIs(self, expr1: object, expr2: object) -> None: ...
    def assertIn(self, member: Any, container: Any) -> None: ...
    def assertRaises(self, expected_exception: type[BaseException]) -> Any: ...
    def assertRaisesRegex(
        self, expected_exception: type[BaseException], expected_regex: str
    ) -> Any: ...


class CachedCoroTestMixin(CachedCoroAssertions):
    def cache(self, minsize: int) -> Any:
        raise NotImplementedError

    async def test_decorator(self):
        cache = self.cache(2)
        calls = Counter()
        wrapper = cachetools.cachedcoro(cache)(make_coroutine(calls))

        self.assertEqual(len(cache), 0)
        self.assertEqual((await wrapper(0))[1], (0,))
        self.assertEqual(len(cache), 1)
        self.assertIn(cachetools.keys.hashkey(0), cache)

        self.assertEqual((await wrapper(1))[1], (1,))
        self.assertEqual(len(cache), 2)

        # the cached value is returned, and the wrapped function is not
        # called again
        first = await wrapper(1)
        self.assertEqual(await wrapper(1), first)
        self.assertEqual(await wrapper(1.0), first)
        self.assertEqual(calls.value, 2)

    async def test_decorator_typed(self):
        cache = self.cache(3)
        calls = Counter()
        wrapper = cachetools.cachedcoro(cache, key=cachetools.keys.typedkey)(
            make_coroutine(calls)
        )

        await wrapper(1)
        await wrapper(1.0)
        self.assertIn(cachetools.keys.typedkey(1), cache)
        self.assertIn(cachetools.keys.typedkey(1.0), cache)
        self.assertEqual(calls.value, 2)

    async def test_concurrent_single_lookup(self):
        # concurrent calls with the same key trigger exactly one lookup,
        # and every waiter receives the same value
        gate = Gate()
        calls = Counter()
        func = make_coroutine(calls, gate)
        wrapper = cachetools.cachedcoro(self.cache(2))(func)

        task = asyncio.ensure_future(
            asyncio.gather(wrapper("x"), wrapper("x"), wrapper("x"))
        )
        await gate.started.wait()
        gate.release.set()
        results = await task
        self.assertEqual(calls.value, 1)
        self.assertTrue(all(r == results[0] for r in results))
        self.assertEqual(len(wrapper.cache), 1)

    async def test_concurrent_distinct_keys(self):
        # different keys still result in independent lookups
        gate = Gate()
        calls = Counter()
        func = make_coroutine(calls, gate)
        wrapper = cachetools.cachedcoro(self.cache(4))(func)

        task = asyncio.ensure_future(asyncio.gather(wrapper("a"), wrapper("b")))
        await gate.started.wait()
        gate.release.set()
        a, b = await task
        self.assertEqual(calls.value, 2)
        self.assertNotEqual(a, b)
        self.assertEqual(await wrapper("a"), a)
        self.assertEqual(calls.value, 2)

    async def test_waiter_cancellation(self):
        # one of several waiters is canceled while the lookup is pending;
        # the other waiters and the lookup itself must still complete
        gate = Gate()
        calls = Counter()
        func = make_coroutine(calls, gate)
        wrapper = cachetools.cachedcoro(self.cache(2))(func)

        first = asyncio.ensure_future(wrapper("x"))
        second = asyncio.ensure_future(wrapper("x"))
        third = asyncio.ensure_future(wrapper("x"))
        await gate.started.wait()
        third.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await third
        gate.release.set()
        results = await asyncio.gather(first, second)

        self.assertEqual(calls.value, 1)
        self.assertEqual(results[0], results[1])
        # the completed value is now cached
        self.assertEqual(await wrapper("x"), results[0])
        self.assertEqual(calls.value, 1)

    async def test_leader_cancellation(self):
        # canceling the call that started the lookup must not cancel the
        # shared lookup task, so other waiters still finish
        gate = Gate()
        calls = Counter()
        func = make_coroutine(calls, gate)
        wrapper = cachetools.cachedcoro(self.cache(2))(func)

        leader = asyncio.ensure_future(wrapper("x"))
        waiter = asyncio.ensure_future(wrapper("x"))
        await gate.started.wait()
        leader.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await leader
        gate.release.set()
        result = await waiter

        self.assertEqual(calls.value, 1)
        self.assertEqual(await wrapper("x"), result)
        self.assertEqual(calls.value, 1)

    async def test_error_not_cached(self):
        # a failed lookup is not cached, so the next call looks up again
        calls = Counter()
        func = make_coroutine(calls, error=True)
        wrapper = cachetools.cachedcoro(self.cache(2))(func)

        with self.assertRaises(ValueError):
            await wrapper(0)
        self.assertEqual(calls.value, 1)
        with self.assertRaises(ValueError):
            await wrapper(0)
        self.assertEqual(calls.value, 2)

    async def test_error_concurrent_waiters(self):
        # all waiters of a failing lookup see the exception, and a later
        # call starts a fresh lookup
        gate = Gate()
        calls = Counter()
        func = make_coroutine(calls, gate, error=True)
        wrapper = cachetools.cachedcoro(self.cache(2))(func)

        task = asyncio.ensure_future(
            asyncio.gather(wrapper(0), wrapper(0), wrapper(0), return_exceptions=True)
        )
        await gate.started.wait()
        gate.release.set()
        results = await task
        self.assertEqual(calls.value, 1)
        self.assertEqual(len(results), 3)
        self.assertTrue(all(isinstance(r, ValueError) for r in results))

        ok_calls = Counter()
        ok = make_coroutine(ok_calls)
        ok_wrapper = cachetools.cachedcoro(self.cache(2))(ok)
        self.assertEqual((await ok_wrapper(0))[1], (0,))
        self.assertEqual(ok_calls.value, 1)

    async def test_error_canceled_waiter(self):
        # a waiter leaving early while the lookup fails must not keep the
        # pending entry around, nor break other waiters or a retry
        gate = Gate()
        calls = Counter()
        func = make_coroutine(calls, gate, error=True)
        wrapper = cachetools.cachedcoro(self.cache(2))(func)

        first = asyncio.ensure_future(wrapper(0))
        second = asyncio.ensure_future(wrapper(0))
        third = asyncio.ensure_future(wrapper(0))
        await gate.started.wait()
        third.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await third
        gate.release.set()

        results = await asyncio.gather(first, second, return_exceptions=True)
        self.assertTrue(all(isinstance(r, ValueError) for r in results))
        self.assertEqual(calls.value, 1)

        with self.assertRaises(ValueError):
            await wrapper(0)
        self.assertEqual(calls.value, 2)

    async def test_zero_size_cache(self):
        cache = self.cache(0)
        calls = Counter()
        func = make_coroutine(calls)
        wrapper = cachetools.cachedcoro(cache)(func)

        self.assertEqual(len(cache), 0)
        await wrapper(0)
        if isinstance(cache, cachetools.Cache):
            # a zero-size Cache rejects every store, so each call looks
            # up again
            self.assertEqual(len(cache), 0)
            await wrapper(0)
            self.assertEqual(calls.value, 2)
        else:
            # a plain mapping has no size limit, so the value is stored
            self.assertEqual(len(cache), 1)
            await wrapper(0)
            self.assertEqual(calls.value, 1)

    async def test_decorator_attributes(self):
        cache = self.cache(2)
        wrapper = cachetools.cachedcoro(cache)(make_coroutine(Counter()))

        self.assertIs(wrapper.cache, cache)
        self.assertIs(wrapper.cache_key, cachetools.keys.hashkey)
        self.assertFalse(hasattr(wrapper, "cache_info"))
        self.assertTrue(callable(wrapper.cache_clear))

    async def test_decorator_wrapped(self):
        func = make_coroutine(Counter())
        wrapper = cachetools.cachedcoro(self.cache(2))(func)

        self.assertIs(wrapper.__wrapped__, func)
        self.assertEqual(wrapper.__name__, "coroutine")
        self.assertIn("coroutine", wrapper.__qualname__)
        self.assertEqual(repr(wrapper), f"<cachedcoro {func!r}>")

    async def test_decorator_clear(self):
        cache = self.cache(2)
        calls = Counter()
        func = make_coroutine(calls)
        wrapper = cachetools.cachedcoro(cache)(func)

        await wrapper(0)
        self.assertEqual(len(cache), 1)
        wrapper.cache_clear()
        self.assertEqual(len(cache), 0)
        await wrapper(0)
        self.assertEqual(calls.value, 2)

    async def test_sync_function_rejected(self):
        def sync_func():
            return 1

        with self.assertRaisesRegex(TypeError, "coroutine"):
            cachetools.cachedcoro(self.cache(2))(sync_func)  # type: ignore[arg-type]


class DictTest(unittest.IsolatedAsyncioTestCase, CachedCoroTestMixin):
    def cache(self, minsize: int) -> Any:
        return {}


class CacheTest(unittest.IsolatedAsyncioTestCase, CachedCoroTestMixin):
    def cache(self, minsize: int) -> Any:
        return cachetools.Cache(maxsize=minsize)


class LRUCacheTest(unittest.IsolatedAsyncioTestCase, CachedCoroTestMixin):
    def cache(self, minsize: int) -> Any:
        return cachetools.LRUCache(maxsize=minsize)


class InfoTest(unittest.IsolatedAsyncioTestCase):
    async def test_cache_info(self):
        cache = cachetools.LRUCache(maxsize=2)
        calls = Counter()

        async def func(n: int) -> int:
            calls.value += 1
            return n

        wrapper = cachetools.cachedcoro(cache, info=True)(func)
        self.assertEqual(wrapper.cache_info(), (0, 0, 2, 0))

        await wrapper(1)
        self.assertEqual(wrapper.cache_info(), (0, 1, 2, 1))
        await wrapper(1)
        self.assertEqual(wrapper.cache_info(), (1, 1, 2, 1))
        await wrapper(2)
        self.assertEqual(wrapper.cache_info(), (1, 2, 2, 2))
        self.assertEqual(calls.value, 2)

        wrapper.cache_clear()
        self.assertEqual(wrapper.cache_info(), (0, 0, 2, 0))

    async def test_cache_info_concurrent(self):
        # one miss is counted for the single shared lookup, not one per
        # waiter; later calls are cache hits
        gate = Gate()
        calls = Counter()

        async def func(n: int) -> int:
            calls.value += 1
            await gate.wait()
            return n

        wrapper = cachetools.cachedcoro(cachetools.Cache(maxsize=2), info=True)(func)
        task = asyncio.ensure_future(asyncio.gather(wrapper(1), wrapper(1), wrapper(1)))
        await gate.started.wait()
        gate.release.set()
        results = await task
        self.assertEqual(results, [1, 1, 1])
        self.assertEqual(calls.value, 1)
        self.assertEqual(wrapper.cache_info(), (0, 1, 2, 1))

        await wrapper(1)
        self.assertEqual(wrapper.cache_info(), (1, 1, 2, 1))

    async def test_cache_info_error(self):
        calls = Counter()

        async def error_func() -> None:
            calls.value += 1
            raise RuntimeError("boom")

        cache = cachetools.Cache(maxsize=2)
        wrapper = cachetools.cachedcoro(cache, info=True)(error_func)

        with self.assertRaises(RuntimeError):
            await wrapper()
        self.assertEqual(wrapper.cache_info(), (0, 1, 2, 0))
        with self.assertRaises(RuntimeError):
            await wrapper()
        self.assertEqual(wrapper.cache_info(), (0, 2, 2, 0))
        self.assertEqual(calls.value, 2)


class InvalidCacheTest(unittest.TestCase):
    def test_none_cache(self):
        with self.assertRaisesRegex(TypeError, "None"):
            cachetools.cachedcoro(None)  # type: ignore[arg-type]


class MethodTest(unittest.IsolatedAsyncioTestCase):
    async def test_decorated_method(self):
        # like @cached, @cachedcoro may be used on methods as well
        cache = cachetools.LRUCache(maxsize=2)

        class Config:
            def __init__(self):
                self.calls = 0

            @cachetools.cachedcoro(cache, key=cachetools.keys.methodkey)
            async def get(self, value: int) -> int:
                self.calls += 1
                return value * 2

        config = Config()
        self.assertEqual(await config.get(1), 2)
        self.assertEqual(await config.get(1), 2)
        self.assertEqual(config.calls, 1)
        self.assertEqual(len(cache), 1)
        config.get.cache_clear()
        self.assertEqual(len(cache), 0)


@cachetools.cachedcoro(cachetools.LRUCache(maxsize=2), info=True)
async def pickled_coro(n: int) -> int:
    return n


class PickleTest(unittest.IsolatedAsyncioTestCase):
    def test_pickle_and_copy(self):
        import copy
        import pickle

        self.assertIs(pickle.loads(pickle.dumps(pickled_coro)), pickled_coro)
        self.assertIs(copy.copy(pickled_coro), pickled_coro)
        self.assertIs(copy.deepcopy(pickled_coro), pickled_coro)

    async def test_self_keyword(self):
        # the receiver is positional-only inside the wrapper, so a wrapped
        # coroutine may take a keyword argument named "self"
        async def func(**kwargs: Any) -> dict[str, Any]:
            return kwargs

        wrapper = cachetools.cachedcoro(cachetools.Cache(maxsize=2))(func)
        self.assertEqual(await wrapper(self=1), {"self": 1})
        self.assertEqual(await wrapper(self=1), {"self": 1})
        self.assertIn(cachetools.keys.hashkey(self=1), wrapper.cache)


if __name__ == "__main__":
    unittest.main()
