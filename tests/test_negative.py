import unittest
from threading import Condition, Event, Lock, Thread

from cachetools import LRUCache, cached, cachedmethod, keys


class Timer:
    """Manually controlled fake clock."""

    def __init__(self):
        self.time = 0

    def __call__(self):
        return self.time

    def tick(self, seconds=1):
        self.time += seconds


class NotFound(LookupError):
    """Dedicated exception for a lookup against a missing route/key."""


class Forbidden(Exception):
    """Second exception type with its own retention time."""


class TransientError(Exception):
    """An ordinary error that must never be cached as negative."""


class Counted:
    """Backend whose real executions are counted."""

    def __init__(self):
        self.calls = 0
        self.available = False

    def get(self, value):
        self.calls += 1
        if self.available:
            return value * 2
        raise NotFound(f"not found: {value}")

    def fail(self):
        self.calls += 1
        raise TransientError("transient")


POLICY = {NotFound: 5, Forbidden: 2}


class NegativeCachedTest(unittest.TestCase):
    def make(self, timer, **kwargs):
        backend = Counted()
        wrapper = cached(LRUCache(maxsize=10), negative=POLICY, timer=timer, **kwargs)(
            backend.get
        )
        return backend, wrapper

    def assert_raises_not_found(self, wrapper, value=7):
        with self.assertRaises(NotFound) as cm:
            wrapper(value)
        return cm.exception

    def test_short_term_negative_then_expiry_with_fake_clock(self):
        # The scenario from the issue, driven entirely by a fake clock.
        timer = Timer()
        backend, wrapper = self.make(timer)

        # first lookup really executes and raises the dedicated error
        exc = self.assert_raises_not_found(wrapper)
        self.assertEqual(backend.calls, 1)
        self.assertEqual(exc.args, ("not found: 7",))

        # repeated lookups are served from the negative entry while live
        timer.tick(4)
        self.assert_raises_not_found(wrapper)
        self.assert_raises_not_found(wrapper)
        self.assertEqual(backend.calls, 1)
        self.assertEqual(len(wrapper.cache), 0)  # nothing stored as a value

        # immediately after expiry the backend is hit again
        timer.tick(1)  # now at 5, exactly at the retention boundary
        self.assert_raises_not_found(wrapper)
        self.assertEqual(backend.calls, 2)

    def test_normal_result_visible_once_backend_recovers(self):
        timer = Timer()
        backend, wrapper = self.make(timer)

        self.assert_raises_not_found(wrapper)
        self.assertEqual(backend.calls, 1)

        # a negative entry suppresses even a recovered backend until it expires
        backend.available = True
        with self.assertRaises(NotFound):
            wrapper(7)
        self.assertEqual(backend.calls, 1)

        timer.tick(5)
        self.assertEqual(wrapper(7), 14)
        self.assertEqual(backend.calls, 2)
        # the normal result is now cached and no longer re-executes
        self.assertEqual(wrapper(7), 14)
        self.assertEqual(backend.calls, 2)
        self.assertIn(keys.hashkey(7), wrapper.cache)

    def test_negative_hit_raises_same_type_without_old_traceback(self):
        timer = Timer()
        _backend, wrapper = self.make(timer)

        original = self.assert_raises_not_found(wrapper)
        try:
            wrapper(7)
            self.fail("NotFound was not raised")
        except NotFound as replayed:
            # capture the traceback while its frames are still alive
            frames = traceback_frames(replayed.__traceback__)
            replayed_tb = replayed.__traceback__
            replayed_exc = replayed

        self.assertIs(type(original), type(replayed_exc))
        self.assertEqual(original.args, replayed_exc.args)
        self.assertIsNot(original, replayed_exc)
        # the replay is a fresh exception: no chained/carried context
        self.assertIsNone(replayed_exc.__cause__)
        self.assertTrue(replayed_exc.__suppress_context__)
        # its traceback originates in the replay path, not the wrapped function
        self.assertIn("raise_result", frames)
        self.assertIn("__call__", frames)
        self.assertNotIn("get", frames)
        # a genuinely new traceback object is attached, not the stored one
        self.assertIsNotNone(replayed_tb)

    def test_unregistered_exception_is_never_cached(self):
        timer = Timer()
        backend = Counted()
        wrapper = cached(LRUCache(maxsize=10), negative=POLICY, timer=timer)(
            backend.fail
        )
        for _ in range(3):
            with self.assertRaises(TransientError):
                wrapper()
        self.assertEqual(backend.calls, 3)

    def test_distinct_exceptions_keep_their_own_retention(self):
        timer = Timer()

        class Multi(Counted):
            def forbidden(self, value):
                self.calls += 1
                raise Forbidden("denied")

        backend = Multi()
        wrap_nf = cached(LRUCache(maxsize=10), negative=POLICY, timer=timer)(
            backend.get
        )
        wrap_fb = cached(LRUCache(maxsize=10), negative=POLICY, timer=timer)(
            backend.forbidden
        )

        with self.assertRaises(NotFound):
            wrap_nf(1)
        with self.assertRaises(Forbidden):
            wrap_fb(1)
        self.assertEqual(backend.calls, 2)

        timer.tick(3)  # Forbidden (ttl 2) has expired, NotFound (ttl 5) has not
        with self.assertRaises(NotFound):
            wrap_nf(1)
        self.assertEqual(backend.calls, 2)  # still served from negative entry
        with self.assertRaises(Forbidden):
            wrap_fb(1)
        self.assertEqual(backend.calls, 3)  # re-executed after expiry

    def test_subclass_matches_declared_exception(self):
        timer = Timer()
        backend = Counted()

        class SpecialNotFound(NotFound):
            pass

        def raising():
            backend.calls += 1
            raise SpecialNotFound("special")

        wrapper = cached(LRUCache(maxsize=10), negative=POLICY, timer=timer)(raising)
        with self.assertRaises(NotFound):
            wrapper()
        with self.assertRaises(NotFound):
            wrapper()  # negative hit, subclass matches the declared type
        self.assertEqual(backend.calls, 1)

    def test_cache_clear_removes_negative_entries(self):
        timer = Timer()
        backend, wrapper = self.make(timer)

        self.assert_raises_not_found(wrapper)
        self.assertEqual(backend.calls, 1)
        wrapper.cache_clear()
        self.assert_raises_not_found(wrapper)
        self.assertEqual(backend.calls, 2)

    def test_no_policy_means_exceptions_propagate_uncached(self):
        backend = Counted()
        wrapper = cached(LRUCache(maxsize=10))(backend.get)
        for _ in range(2):
            with self.assertRaises(NotFound):
                wrapper(7)
        self.assertEqual(backend.calls, 2)

    def test_default_timer_is_accepted(self):
        # without an explicit timer the policy still works using time.monotonic
        backend = Counted()
        wrapper = cached(LRUCache(maxsize=10), negative=POLICY)(backend.get)
        with self.assertRaises(NotFound):
            wrapper(7)
        with self.assertRaises(NotFound):
            wrapper(7)
        self.assertEqual(backend.calls, 1)


class NegativeVariantsTest(unittest.TestCase):
    """The policy must apply to every lock/condition/info wrapper variant."""

    def _check(self, **kwargs):
        timer = Timer()
        backend = Counted()
        wrapper = cached(LRUCache(maxsize=10), negative=POLICY, timer=timer, **kwargs)(
            backend.get
        )
        with self.assertRaises(NotFound):
            wrapper(7)
        with self.assertRaises(NotFound):
            wrapper(7)
        self.assertEqual(backend.calls, 1)
        timer.tick(5)
        with self.assertRaises(NotFound):
            wrapper(7)
        self.assertEqual(backend.calls, 2)
        return wrapper

    def test_basic(self):
        self._check()

    def test_lock(self):
        self._check(lock=Lock())

    def test_condition(self):
        self._check(condition=Condition())

    def test_info(self):
        wrapper = self._check(info=True)
        # two real executions are misses, the in-window replay is a hit
        hits, misses, _, currsize = wrapper.cache_info()
        self.assertEqual((hits, misses, currsize), (1, 2, 0))

    def test_lock_info(self):
        wrapper = self._check(lock=Lock(), info=True)
        hits, misses, _, _ = wrapper.cache_info()
        self.assertEqual((hits, misses), (1, 2))

    def test_condition_info(self):
        wrapper = self._check(condition=Condition(), info=True)
        hits, misses, _, _ = wrapper.cache_info()
        self.assertEqual((hits, misses), (1, 2))

    def test_lock_condition_info(self):
        lock = Lock()
        wrapper = self._check(lock=lock, condition=Condition(lock), info=True)
        hits, misses, _, _ = wrapper.cache_info()
        self.assertEqual((hits, misses), (1, 2))


class NegativeCachedMethodTest(unittest.TestCase):
    def test_method_negative_caching_with_fake_clock(self):
        timer = Timer()

        class Service:
            def __init__(self):
                self.cache = LRUCache(maxsize=10)
                self.calls = 0

            @cachedmethod(lambda self: self.cache, negative=POLICY, timer=timer)
            def get(self, value):
                self.calls += 1
                raise NotFound("missing")

        service = Service()
        with self.assertRaises(NotFound):
            service.get(1)
        with self.assertRaises(NotFound):
            service.get(1)
        self.assertEqual(service.calls, 1)
        timer.tick(5)
        with self.assertRaises(NotFound):
            service.get(1)
        self.assertEqual(service.calls, 2)

    def test_method_lock_info_variant(self):
        timer = Timer()

        class Service:
            def __init__(self):
                self.cache = LRUCache(maxsize=10)
                self.lock = Lock()
                self.calls = 0

            @cachedmethod(
                lambda self: self.cache,
                lock=lambda self: self.lock,
                info=True,
                negative=POLICY,
                timer=timer,
            )
            def get(self, value):
                self.calls += 1
                raise NotFound("missing")

        service = Service()
        with self.assertRaises(NotFound):
            service.get(1)
        with self.assertRaises(NotFound):
            service.get(1)
        self.assertEqual(service.calls, 1)
        hits, misses, _, _ = service.get.cache_info()
        self.assertEqual((hits, misses), (1, 1))
        service.get.cache_clear()
        with self.assertRaises(NotFound):
            service.get(1)
        self.assertEqual(service.calls, 2)


class NoPolicyFastPathTest(unittest.TestCase):
    """Without a policy, all wrapper variants keep their original behavior."""

    @staticmethod
    def func():
        return 42

    def _exercise(self, **kwargs):
        wrapper = cached(LRUCache(maxsize=10), **kwargs)(self.func)
        self.assertEqual(wrapper(), 42)
        self.assertEqual(wrapper(), 42)  # second call served from the cache
        info = (
            wrapper.cache_info() if kwargs.get("info") else None
        )  # before cache_clear() resets the counters
        wrapper.cache_clear()
        self.assertEqual(len(wrapper.cache), 0)
        return info

    def test_basic(self):
        self._exercise()

    def test_lock(self):
        self._exercise(lock=Lock())

    def test_condition(self):
        self._exercise(condition=Condition())

    def test_info(self):
        info = self._exercise(info=True)
        assert info is not None
        self.assertEqual((info.hits, info.misses), (1, 1))

    def test_lock_info(self):
        info = self._exercise(lock=Lock(), info=True)
        assert info is not None
        self.assertEqual((info.hits, info.misses), (1, 1))

    def test_condition_info(self):
        info = self._exercise(condition=Condition(), info=True)
        assert info is not None
        self.assertEqual((info.hits, info.misses), (1, 1))

    def test_lock_condition_info(self):
        lock = Lock()
        info = self._exercise(lock=lock, condition=Condition(lock), info=True)
        assert info is not None
        self.assertEqual((info.hits, info.misses), (1, 1))

    def test_error_lock_variant(self):
        backend = Counted()
        wrapper = cached(LRUCache(maxsize=10), lock=Lock())(backend.fail)
        for _ in range(2):
            with self.assertRaises(TransientError):
                wrapper()
        self.assertEqual(backend.calls, 2)  # nothing is cached without a policy

    def test_error_condition_variant(self):
        backend = Counted()
        wrapper = cached(LRUCache(maxsize=10), condition=Condition())(backend.fail)
        for _ in range(2):
            with self.assertRaises(TransientError):
                wrapper()
        self.assertEqual(backend.calls, 2)

    def test_error_lock_info_variant(self):
        backend = Counted()
        wrapper = cached(LRUCache(maxsize=10), lock=Lock(), info=True)(backend.fail)
        for _ in range(2):
            with self.assertRaises(TransientError):
                wrapper()
        self.assertEqual(backend.calls, 2)

    def test_error_condition_info_variant(self):
        backend = Counted()
        wrapper = cached(LRUCache(maxsize=10), condition=Condition(), info=True)(
            backend.fail
        )
        for _ in range(2):
            with self.assertRaises(TransientError):
                wrapper()
        self.assertEqual(backend.calls, 2)

    def test_clear_all_variants_without_policy(self):
        # exercise the cache_clear() fast path with negative unset
        backend = Counted()
        wrappers = [
            cached(LRUCache(maxsize=10))(backend.get),
            cached(LRUCache(maxsize=10), lock=Lock())(backend.get),
            cached(LRUCache(maxsize=10), condition=Condition())(backend.get),
            cached(LRUCache(maxsize=10), info=True)(backend.get),
            cached(LRUCache(maxsize=10), lock=Lock(), info=True)(backend.get),
            cached(LRUCache(maxsize=10), condition=Condition(), info=True)(backend.get),
        ]
        for wrapper in wrappers:
            with self.assertRaises(NotFound):
                wrapper(7)
            wrapper.cache_clear()
            self.assertEqual(len(wrapper.cache), 0)

    def test_basic_and_info_success_and_error_without_policy(self):
        # cover the _InfoMixin success/discard/error lines with no policy
        backend = Counted()
        info_wrapper = cached(LRUCache(maxsize=10), info=True)(backend.fail)
        with self.assertRaises(TransientError):
            info_wrapper()
        basic_wrapper = cached(LRUCache(maxsize=10))(backend.fail)
        with self.assertRaises(TransientError):
            basic_wrapper()
        self.assertEqual(backend.calls, 2)

    def test_success_and_clear_all_variants_without_policy(self):
        # cover the negative-is-None discard and clear branches in every variant
        backend = Counted()
        backend.available = True
        lock = Lock()
        wrappers = [
            cached(LRUCache(maxsize=10))(backend.get),
            cached(LRUCache(maxsize=10), lock=Lock())(backend.get),
            cached(LRUCache(maxsize=10), condition=Condition())(backend.get),
            cached(LRUCache(maxsize=10), info=True)(backend.get),
            cached(LRUCache(maxsize=10), lock=Lock(), info=True)(backend.get),
            cached(LRUCache(maxsize=10), condition=Condition(), info=True)(backend.get),
            cached(
                LRUCache(maxsize=10),
                lock=lock,
                condition=Condition(lock),
                info=True,
            )(backend.get),
        ]
        for wrapper in wrappers:
            self.assertEqual(wrapper(7), 14)
            wrapper.cache_clear()


class NegativePolicyClearTest(unittest.TestCase):
    """Exercise the negative discard/clear lines in every wrapper variant."""

    @staticmethod
    def _factories(timer, lock, cond):
        def make(backend, **kwargs):
            return cached(LRUCache(maxsize=10), negative=POLICY, timer=timer, **kwargs)(
                backend.get
            )

        return [
            lambda b: make(b),
            lambda b: make(b, lock=lock),
            lambda b: make(b, condition=cond),
            lambda b: make(b, info=True),
            lambda b: make(b, lock=lock, info=True),
            lambda b: make(b, condition=cond, info=True),
        ]

    def test_discard_after_recovery_and_clear(self):
        timer = Timer()
        lock = Lock()
        cond = Condition()
        for factory in self._factories(timer, lock, cond):
            backend = Counted()
            wrapper = factory(backend)
            # seed a negative entry, then let it expire and recover a value
            with self.assertRaises(NotFound):
                wrapper(7)
            timer.tick(5)
            backend.available = True
            self.assertEqual(wrapper(7), 14)  # discards the stale negative entry
            self.assertEqual(wrapper(7), 14)  # value now cached
            backend.available = False
            with self.assertRaises(NotFound):
                wrapper(8)  # seed another negative entry
            wrapper.cache_clear()  # clears both the cache and negative store
            self.assertEqual(len(wrapper.cache), 0)
            backend.available = False


class NegativeConcurrencyTest(unittest.TestCase):
    TIMEOUT = 5

    def test_condition_coalesces_concurrent_negative_calls(self):
        started = Event()
        release = Event()
        calls = []

        @cached(
            LRUCache(maxsize=10),
            condition=Condition(),
            negative=POLICY,
            timer=Timer(),
        )
        def func():
            calls.append(1)
            started.set()
            release.wait(self.TIMEOUT)
            raise NotFound("boom")

        results: list[NotFound | None] = [None, None]

        def run(index):
            try:
                func()
            except NotFound as exc:
                results[index] = exc

        first = Thread(target=run, args=(0,))
        first.start()
        self.assertTrue(started.wait(self.TIMEOUT))
        second = Thread(target=run, args=(1,))
        second.start()
        release.set()
        for thread in (first, second):
            thread.join(self.TIMEOUT)
            self.assertFalse(thread.is_alive())

        # the wrapped function runs once; the waiting thread replays the
        # negative result and still receives its own fresh exception
        self.assertEqual(len(calls), 1)
        self.assertTrue(all(isinstance(exc, NotFound) for exc in results))
        self.assertIsNot(results[0], results[1])


class InvalidPolicyTest(unittest.TestCase):
    def test_empty_mapping(self):
        with self.assertRaisesRegex(TypeError, "non-empty mapping"):
            cached(LRUCache(maxsize=10), negative={})

    def test_non_mapping(self):
        with self.assertRaisesRegex(TypeError, "non-empty mapping"):
            cached(LRUCache(maxsize=10), negative=[(NotFound, 1)])  # type: ignore

    def test_non_exception_key(self):
        with self.assertRaisesRegex(TypeError, "exception classes"):
            cached(LRUCache(maxsize=10), negative={int: 1})  # type: ignore

    def test_non_callable_timer(self):
        with self.assertRaisesRegex(TypeError, "timer must be callable"):
            cached(LRUCache(maxsize=10), negative=POLICY, timer=5)  # type: ignore

    def test_validation_happens_at_decoration_time(self):
        # the error is raised before the wrapped function is ever invoked
        with self.assertRaises(TypeError):
            cachedmethod(lambda self: LRUCache(maxsize=10), negative={1: 2})  # type: ignore


class NegativeTypingTest(unittest.TestCase):
    """Concrete, fully annotated usages that the stubs must type-check."""

    def test_cached_policy_typing(self):
        timer = Timer()

        @cached(
            LRUCache[str, int](maxsize=10),
            negative={NotFound: 5},
            timer=timer,
            info=True,
        )
        def lookup(value: str) -> int:
            return len(value)

        result: int = lookup("abc")
        self.assertEqual(result, 3)
        self.assertEqual(lookup("abc"), 3)  # second call is a cache hit
        self.assertEqual(lookup.cache_info().hits, 1)

    def test_cachedmethod_policy_typing(self):
        timer = Timer()

        class Service:
            def __init__(self):
                self.cache = LRUCache[int, int](maxsize=10)

            @cachedmethod(
                lambda self: self.cache,
                negative={NotFound: 5},
                timer=timer,
            )
            def get(self, value: int) -> int:
                return value * 2

        service = Service()
        result: int = service.get(3)
        self.assertEqual(result, 6)


def traceback_frames(tb):
    names = set()
    while tb is not None:
        names.add(tb.tb_frame.f_code.co_name)
        tb = tb.tb_next
    return names
