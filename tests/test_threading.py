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

    def test_cached_cond_clear(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(1)
                cond = WaitCondition()
                barriers = [Barrier(2, timeout=self.TIMEOUT) for _ in range(2)]
                values = [object(), object(), object()]
                calls = []

                @cached(cache, condition=cond, info=info)
                def func(calls=calls, barriers=barriers, values=values):
                    index = len(calls)
                    calls.append(None)
                    if index < len(barriers):
                        barriers[index].wait()  # tell the test we are running
                        barriers[index].wait()  # wait until the test releases us
                    return values[index]

                self.run_cond_clear_test(
                    func, cond, barriers, values, calls, cache, info
                )

    def test_cachedmethod_cond_clear(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(1)
                cond = WaitCondition()
                barriers = [Barrier(2, timeout=self.TIMEOUT) for _ in range(2)]
                values = [object(), object(), object()]
                calls = []

                class Cached:
                    @cachedmethod(lambda _: cache, condition=lambda _: cond, info=info)
                    def method(self, calls=calls, barriers=barriers, values=values):
                        index = len(calls)
                        calls.append(None)
                        if index < len(barriers):
                            barriers[index].wait()  # tell the test we are running
                            barriers[index].wait()  # wait until it releases us
                        return values[index]

                self.run_cond_clear_test(
                    Cached().method, cond, barriers, values, calls, cache, info
                )

    def test_cached_cond_clear_pending(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(1)
                cond = WaitCondition()
                # a third slot, so a third computation fails an
                # assertion instead of raising out of the thread
                started = [Event(), Event(), Event()]
                release = [Event(), Event(), Event()]
                values = [object(), object(), object()]
                calls = []

                @cached(cache, condition=cond, info=info)
                def func(calls=calls, started=started, release=release, values=values):
                    index = len(calls)
                    calls.append(None)
                    started[index].set()
                    release[index].wait(self.TIMEOUT)
                    return values[index]

                self.run_cond_clear_pending_test(
                    func, cond, started, release, values, calls, cache, info
                )

    def test_cachedmethod_cond_clear_pending(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(1)
                cond = WaitCondition()
                # a third slot, so a third computation fails an
                # assertion instead of raising out of the thread
                started = [Event(), Event(), Event()]
                release = [Event(), Event(), Event()]
                values = [object(), object(), object()]
                calls = []

                class Cached:
                    @cachedmethod(lambda _: cache, condition=lambda _: cond, info=info)
                    def method(
                        self,
                        calls=calls,
                        started=started,
                        release=release,
                        values=values,
                        timeout=self.TIMEOUT,
                    ):
                        index = len(calls)
                        calls.append(None)
                        started[index].set()
                        release[index].wait(timeout)
                        return values[index]

                self.run_cond_clear_pending_test(
                    Cached().method, cond, started, release, values, calls, cache, info
                )

    def test_cached_cond_clear_wait(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(1)
                cond = WaitCondition()
                started = [Event(), Event()]
                release = Event()
                values = [object(), object()]
                calls = []

                @cached(cache, condition=cond, info=info)
                def func(started=started, release=release, values=values, calls=calls):
                    index = len(calls)
                    calls.append(None)
                    started[index].set()
                    if index == 0:
                        release.wait(self.TIMEOUT)
                    return values[index]

                self.run_cond_clear_wait_test(
                    func, cond, started, release, values, calls, cache, info
                )

    def test_cachedmethod_cond_clear_wait(self):
        for info in (False, True):
            with self.subTest(info=info):
                cache = LRUCache(1)
                cond = WaitCondition()
                started = [Event(), Event()]
                release = Event()
                values = [object(), object()]
                calls = []

                class Cached:
                    @cachedmethod(lambda _: cache, condition=lambda _: cond, info=info)
                    def method(
                        self,
                        started=started,
                        release=release,
                        values=values,
                        calls=calls,
                        timeout=self.TIMEOUT,
                    ):
                        index = len(calls)
                        calls.append(None)
                        started[index].set()
                        if index == 0:
                            release.wait(timeout)
                        return values[index]

                self.run_cond_clear_wait_test(
                    Cached().method, cond, started, release, values, calls, cache, info
                )

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

    def run_cond_clear_test(self, func, cond, barriers, values, calls, cache, info):
        # Two calls for the same key are started in order, but the
        # second one is allowed to finish first.  Clearing the cache in
        # between must let the second call store its result, must not
        # let the stale first result overwrite it afterwards, and must
        # not leave the key pending, so a later read sees the cache.
        results = [None] * 3

        def run(index):
            results[index] = func()

        # barrier 1: the first call enters the wrapped function
        first = Thread(target=run, args=[0], daemon=True)
        first.start()
        self.assertIsNotNone(barriers[0].wait(self.TIMEOUT))

        func.cache_clear()
        self.assertEqual(len(cache), 0)

        # barrier 2: clearing must leave no pending key behind
        second = Thread(target=run, args=[1], daemon=True)
        second.start()
        self.assertIsNotNone(barriers[1].wait(self.TIMEOUT))
        self.assertEqual(len(calls), 2)
        self.assertFalse(cond.waiting.is_set())  # no waiting for a stale call

        # both calls are inside the wrapped function now; let the
        # second round finish first, so the rounds complete in reverse
        self.assertIsNotNone(barriers[1].wait(self.TIMEOUT))
        second.join(timeout=self.TIMEOUT)
        self.assertFalse(second.is_alive())
        self.assertIs(results[1], values[1])
        self.assertIs(cache[func.cache_key()], values[1])

        # the stale first call returns its own value, but must neither
        # fill the cache nor withdraw the new pending key
        self.assertIsNotNone(barriers[0].wait(self.TIMEOUT))
        first.join(timeout=self.TIMEOUT)
        self.assertFalse(first.is_alive())
        self.assertIs(results[0], values[0])
        self.assertIs(cache[func.cache_key()], values[1])

        # a third call neither blocks nor recomputes
        results[2] = func()
        self.assertEqual(len(calls), 2)
        self.assertIs(results[2], values[1])
        self.assertIs(cache[func.cache_key()], values[1])

        if info:
            cache_info = func.cache_info()
            # clearing reset the counts, so only the second call's miss
            # and the third call's hit are left
            self.assertEqual(cache_info.hits, 1)
            self.assertEqual(cache_info.misses, 1)
            self.assertEqual(cache_info.currsize, 1)

    def run_cond_clear_pending_test(
        self, func, cond, started, release, values, calls, cache, info
    ):
        # The stale call returns while a new call for the same key is
        # still executing.  It must not withdraw that call's pending
        # key, or a third thread would start yet another computation.
        results = [None] * 3

        def run(index):
            results[index] = func()

        first = Thread(target=run, args=[0], daemon=True)
        first.start()
        self.assertTrue(started[0].wait(self.TIMEOUT))

        func.cache_clear()
        self.assertEqual(len(cache), 0)

        second = Thread(target=run, args=[1], daemon=True)
        second.start()
        self.assertTrue(started[1].wait(self.TIMEOUT))
        self.assertEqual(len(calls), 2)

        # the stale call finishes first, while the new one is executing
        release[0].set()
        first.join(timeout=self.TIMEOUT)
        self.assertFalse(first.is_alive())
        self.assertIs(results[0], values[0])
        self.assertNotIn(func.cache_key(), cache)

        # a third caller joins the pending new call instead of computing
        third = Thread(target=run, args=[2], daemon=True)
        third.start()
        self.assertTrue(cond.waiting.wait(self.TIMEOUT))
        self.assertEqual(len(calls), 2)

        release[1].set()
        for thread in (second, third):
            thread.join(timeout=self.TIMEOUT)
            self.assertFalse(thread.is_alive())
        self.assertIs(results[1], values[1])
        self.assertIs(results[2], values[1])
        self.assertIs(cache[func.cache_key()], values[1])
        self.assertEqual(len(calls), 2)

    def run_cond_clear_wait_test(
        self, func, cond, started, release, values, calls, cache, info
    ):
        # A thread waiting for a pending call must be woken up by
        # cache_clear(), because the call it waits for will not store
        # its result nor notify anyone.
        results = [None] * 3

        def run(index):
            results[index] = func()

        # daemon threads, so a missing wakeup fails instead of hanging
        first = Thread(target=run, args=[0], daemon=True)
        first.start()
        self.assertTrue(started[0].wait(self.TIMEOUT))

        second = Thread(target=run, args=[1], daemon=True)
        second.start()
        try:
            # the second caller blocks on the pending key
            self.assertTrue(cond.waiting.wait(self.TIMEOUT))
            self.assertEqual(len(calls), 1)

            func.cache_clear()
            self.assertEqual(len(cache), 0)
            # the woken-up caller computes the cleared value anew
            self.assertTrue(started[1].wait(self.TIMEOUT))
            self.assertEqual(len(calls), 2)
        finally:
            release.set()  # the stale call returns its own result
        for thread in (first, second):
            thread.join(timeout=self.TIMEOUT)
            self.assertFalse(thread.is_alive())
        self.assertIs(results[0], values[0])
        self.assertIs(results[1], values[1])
        self.assertIs(cache[func.cache_key()], values[1])

        results[2] = func()
        self.assertIs(results[2], values[1])
        self.assertEqual(len(calls), 2)

        if info:
            cache_info = func.cache_info()
            # only the woken-up caller's miss and the last call's hit
            self.assertEqual(cache_info.hits, 1)
            self.assertEqual(cache_info.misses, 1)
            self.assertEqual(cache_info.currsize, 1)

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
