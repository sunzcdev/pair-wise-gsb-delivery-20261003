import functools
import unittest
from threading import Barrier, Condition, Event, Lock, Thread

from cachetools import LRUCache, cached, cachedmethod


class WaitCondition(Condition):
    def __init__(self, lock=None):
        super().__init__(lock)
        self.waiting = Event()

    def wait(self, timeout=None):
        self.waiting.set()
        return super().wait(timeout)


class ThreadingTest(unittest.TestCase):
    NTHREADS = 10

    TIMEOUT = 10

    def test_cached_lock(self):
        for info in (False, True):
            with self.subTest(info=info):
                barrier = Barrier(self.NTHREADS, timeout=self.TIMEOUT)
                cache = LRUCache(1)

                @cached(cache, lock=Lock(), info=info)
                def func(barrier=barrier):
                    result = object()
                    barrier.wait()
                    return result

                self.run_lock_test(func, cache, info)

    def test_cachedmethod_lock(self):
        for info in (False, True):
            with self.subTest(info=info):
                barrier = Barrier(self.NTHREADS, timeout=self.TIMEOUT)
                cache = LRUCache(1)
                lock = Lock()

                class Cached:
                    @cachedmethod(lambda _: cache, lock=lambda _: lock, info=info)
                    def method(self, barrier=barrier):
                        result = object()
                        barrier.wait()
                        return result

                self.run_lock_test(Cached().method, cache, info)

    def test_cached_cond(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(1)
                cond = WaitCondition()
                started = Event()
                release = Event()
                calls = []

                @cached(cache, condition=cond, info=info)
                def func(calls=calls, started=started, release=release):
                    calls.append(None)
                    started.set()
                    release.wait(self.TIMEOUT)
                    return object()

                self.run_cond_wait_test(func, cond, started, release, calls)

    def test_cachedmethod_cond(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(1)
                cond = WaitCondition()
                started = Event()
                release = Event()
                calls = []

                class Cached:
                    @cachedmethod(lambda _: cache, condition=lambda _: cond, info=info)
                    def method(
                        self,
                        calls=calls,
                        started=started,
                        release=release,
                        timeout=self.TIMEOUT,
                    ):
                        calls.append(None)
                        started.set()
                        release.wait(timeout)
                        return object()

                func = Cached().method
                self.run_cond_wait_test(func, cond, started, release, calls)

    def test_cached_lock_cond(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(1)
                lock = Lock()
                cond = WaitCondition(lock)
                started = Event()
                release = Event()
                calls = []

                @cached(cache, lock=lock, condition=cond, info=info)
                def func(calls=calls, started=started, release=release):
                    calls.append(None)
                    started.set()
                    release.wait(self.TIMEOUT)
                    return object()

                self.run_cond_wait_test(func, cond, started, release, calls)

    def test_cachedmethod_lock_cond(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(1)
                lock = Lock()
                cond = WaitCondition(lock)
                started = Event()
                release = Event()
                calls = []

                class Cached:
                    @cachedmethod(
                        lambda _: cache,
                        lock=lambda _: lock,
                        condition=lambda _: cond,
                        info=info,
                    )
                    def method(
                        self,
                        calls=calls,
                        started=started,
                        release=release,
                        timeout=self.TIMEOUT,
                    ):
                        calls.append(None)
                        started.set()
                        release.wait(timeout)
                        return object()

                func = Cached().method
                self.run_cond_wait_test(func, cond, started, release, calls)

    def test_cached_unrelated_lock_cond(self):
        # a condition variable not created with the lock fails on
        # the first cache miss, even without concurrent calls
        for info in (False, True):
            with self.subTest(info=info):

                @cached(LRUCache(1), lock=Lock(), condition=Condition(), info=info)
                def func():
                    return object()

                with self.assertRaises(RuntimeError):
                    func()

    def test_cachedmethod_unrelated_lock_cond(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(1)
                lock = Lock()
                cond = Condition()

                class Cached:
                    @cachedmethod(
                        lambda _: cache,
                        lock=lambda _: lock,
                        condition=lambda _: cond,
                        info=info,
                    )
                    def method(self):
                        return object()

                with self.assertRaises(RuntimeError):
                    Cached().method()

    def test_cached_cond_cycle(self):
        # Two threads computing mutually dependent keys each wait for
        # the other's pending call, which can never complete, so the
        # wait must be reported instead of blocking forever.
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(4)
                lock = Lock()
                cond = Condition(lock)
                barrier = Barrier(2, timeout=self.TIMEOUT)
                nested = [True]
                calls = []
                wrappers = {}

                def func(key):
                    calls.append(key)
                    if not nested[0]:
                        return key
                    barrier.wait()
                    return wrappers["b" if key == "a" else "a"]()

                for key in ("a", "b"):
                    wrappers[key] = cached(
                        cache,
                        key=lambda _key=key: _key,
                        lock=lock,
                        condition=cond,
                        info=info,
                    )(functools.partial(func, key))

                self.run_cycle_test(wrappers, cache, nested, calls)

    def test_cachedmethod_cond_cycle(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(4)
                lock = Lock()
                cond = Condition(lock)
                barrier = Barrier(2, timeout=self.TIMEOUT)
                nested = [True]
                calls = []

                class Cached:
                    @cachedmethod(
                        lambda _: cache,
                        lock=lambda _: lock,
                        condition=lambda _: cond,
                        info=info,
                    )
                    def method(self, key):
                        calls.append(key)
                        if not nested[0]:
                            return key
                        barrier.wait()
                        return self.method("b" if key == "a" else "a")

                obj = Cached()

                self.run_cycle_test(
                    {"a": lambda: obj.method("a"), "b": lambda: obj.method("b")},
                    cache,
                    nested,
                    calls,
                )

    def test_cachedmethod_cond_cycle_shared_cache(self):
        # two instances sharing the same cache, lock and condition
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(4)
                lock = Lock()
                cond = Condition(lock)
                barrier = Barrier(2, timeout=self.TIMEOUT)
                nested = [True]
                calls = []

                class Cached:
                    @cachedmethod(
                        lambda _: cache,
                        lock=lambda _: lock,
                        condition=lambda _: cond,
                        info=info,
                    )
                    def method(self, other, key):
                        calls.append(key)
                        if not nested[0]:
                            return key
                        barrier.wait()
                        return other.method(self, "b" if key == "a" else "a")

                first, second = Cached(), Cached()

                self.run_cycle_test(
                    {
                        "a": lambda: first.method(second, "a"),
                        "b": lambda: second.method(first, "b"),
                    },
                    cache,
                    nested,
                    calls,
                )

    def test_cached_cond_nested_keys(self):
        # Recursion into different keys must still work and be cached;
        # waiting for a pending call that will complete is not a cycle.
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(16)
                lock = Lock()
                cond = Condition(lock)
                calls = []

                @cached(cache, lock=lock, condition=cond, info=info)
                def fib(n):
                    calls.append(n)
                    return n if n < 2 else fib(n - 1) + fib(n - 2)

                self.assertEqual(fib(10), 55)
                self.assertEqual(len(cache), 11)
                calls.clear()
                self.assertEqual(fib(10), 55)
                self.assertEqual(calls, [])

    def run_cycle_test(self, wrappers, cache, nested, calls):
        errors = {}

        def run(name):
            try:
                wrappers[name]()
            except RuntimeError as e:
                errors[name] = e

        threads = [
            Thread(target=run, args=("a",)),
            Thread(target=run, args=("b",)),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=self.TIMEOUT)
            self.assertFalse(thread.is_alive())

        # both callers must report the circular wait, nothing is cached
        self.assertEqual(set(errors), {"a", "b"})
        self.assertIn("deadlock", str(errors["a"]))
        self.assertIn("deadlock", str(errors["b"]))
        self.assertEqual(len(calls), 2)
        self.assertEqual(len(cache), 0)

        # the pending calls are released, so later calls complete
        nested[0] = False
        self.assertEqual(wrappers["a"](), "a")
        self.assertEqual(wrappers["b"](), "b")
        self.assertEqual(len(cache), 2)

    def run_cond_wait_test(self, func, cond, started, release, calls):
        results = [None] * 2

        def run(index):
            results[index] = func()

        first = Thread(target=run, args=[0])
        first.start()
        self.assertTrue(started.wait(self.TIMEOUT))
        second = Thread(target=run, args=[1])
        second.start()
        # second caller must block on the pending key
        self.assertTrue(cond.waiting.wait(self.TIMEOUT))
        release.set()
        for thread in (first, second):
            thread.join(timeout=self.TIMEOUT)
            self.assertFalse(thread.is_alive())
        self.assertEqual(len(calls), 1)
        self.assertIsNotNone(results[0])
        self.assertIs(results[0], results[1])

    def run_lock_test(self, func, cache, info):
        self.run_threading_test(func, cache)

        if info:
            cache_info = func.cache_info()
            self.assertEqual(cache_info.hits + cache_info.misses, self.NTHREADS)
            self.assertGreaterEqual(cache_info.misses, 1)

    def run_threading_test(self, func, cache):
        results = [None] * self.NTHREADS

        def run(index):
            results[index] = func()

        threads = [Thread(target=run, args=[i]) for i in range(self.NTHREADS)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=self.TIMEOUT)
            self.assertFalse(thread.is_alive())
        self.assertEqual(len(cache), 1)

        # multiple threads may compute the value, but the first one
        # should always win and all concurrent invocations should
        # return the same result
        cached_result = next(iter(cache.values()))
        for result in results:
            self.assertIs(result, cached_result)
