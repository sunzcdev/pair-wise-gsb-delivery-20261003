import math
import unittest

from cachetools import LRUCache, TTLCache

from . import CacheTestMixin


class Timer:
    def __init__(self, auto=False):
        self.auto = auto
        self.time = 0

    def __call__(self):
        if self.auto:
            self.time += 1
        return self.time

    def tick(self):
        self.time += 1


class TTLTestCache(TTLCache):
    def __init__(self, maxsize, ttl=math.inf, **kwargs):
        TTLCache.__init__(self, maxsize, ttl=ttl, timer=Timer(), **kwargs)


class TTLCacheTest(unittest.TestCase, CacheTestMixin):
    Cache = TTLTestCache

    def test_ttl(self):
        cache = TTLCache[int, int, int](maxsize=2, ttl=2, timer=Timer())
        self.assertEqual(0, cache.timer())
        self.assertEqual(2, cache.ttl)

        cache[1] = 1
        self.assertEqual(1, cache[1])
        self.assertEqual(1, len(cache))
        self.assertEqual({1}, set(cache))

        cache.timer.tick()
        self.assertEqual(1, cache[1])
        self.assertEqual(1, len(cache))
        self.assertEqual({1}, set(cache))

        cache[2] = 2
        self.assertEqual(1, cache[1])
        self.assertEqual(2, cache[2])
        self.assertEqual(2, len(cache))
        self.assertEqual({1, 2}, set(cache))

        cache.timer.tick()
        self.assertNotIn(1, cache)
        self.assertEqual(2, cache[2])
        self.assertEqual(1, len(cache))
        self.assertEqual({2}, set(cache))

        cache[3] = 3
        self.assertNotIn(1, cache)
        self.assertEqual(2, cache[2])
        self.assertEqual(3, cache[3])
        self.assertEqual(2, len(cache))
        self.assertEqual({2, 3}, set(cache))

        cache.timer.tick()
        self.assertNotIn(1, cache)
        self.assertNotIn(2, cache)
        self.assertEqual(3, cache[3])
        self.assertEqual(1, len(cache))
        self.assertEqual({3}, set(cache))

        cache.timer.tick()
        self.assertNotIn(1, cache)
        self.assertNotIn(2, cache)
        self.assertNotIn(3, cache)

        with self.assertRaises(KeyError):
            del cache[1]
        with self.assertRaises(KeyError):
            cache.pop(2)
        with self.assertRaises(KeyError):
            del cache[3]

        self.assertEqual(0, len(cache))
        self.assertEqual(set(), set(cache))

    def test_ttl_timer(self):
        cache = TTLCache[int, int, int](maxsize=2, ttl=2, timer=Timer())
        self.assertEqual(cache.timer.time, 0)
        self.assertFalse(cache.timer.auto)

        cache[1] = 1
        cache.timer.tick()
        self.assertEqual(cache.timer.time, 1)
        self.assertEqual(1, cache[1])

        cache.timer.tick()
        self.assertEqual(cache.timer.time, 2)
        self.assertNotIn(1, cache)

    def test_ttl_lru(self):
        cache = TTLCache[int, int, int](maxsize=2, ttl=1, timer=Timer())

        cache[1] = 1
        cache[2] = 2
        cache[3] = 3

        self.assertEqual(len(cache), 2)
        self.assertNotIn(1, cache)
        self.assertEqual(cache[2], 2)
        self.assertEqual(cache[3], 3)

        cache[2]
        cache[4] = 4
        self.assertEqual(len(cache), 2)
        self.assertNotIn(1, cache)
        self.assertEqual(cache[2], 2)
        self.assertNotIn(3, cache)
        self.assertEqual(cache[4], 4)

        cache[5] = 5
        self.assertEqual(len(cache), 2)
        self.assertNotIn(1, cache)
        self.assertNotIn(2, cache)
        self.assertNotIn(3, cache)
        self.assertEqual(cache[4], 4)
        self.assertEqual(cache[5], 5)

    def test_ttl_expire(self):
        cache = TTLCache[int, int, int](maxsize=3, ttl=3, timer=Timer())
        with cache.timer as time:
            self.assertEqual(time, cache.timer())
        self.assertEqual(3, cache.ttl)

        cache[1] = 1
        cache.timer.tick()
        cache[2] = 2
        cache.timer.tick()
        cache[3] = 3
        self.assertEqual(2, cache.timer())

        self.assertEqual({1, 2, 3}, set(cache))
        self.assertEqual(3, len(cache))
        self.assertEqual(1, cache[1])
        self.assertEqual(2, cache[2])
        self.assertEqual(3, cache[3])

        items = cache.expire()
        self.assertEqual(set(), set(items))
        self.assertEqual({1, 2, 3}, set(cache))
        self.assertEqual(3, len(cache))
        self.assertEqual(1, cache[1])
        self.assertEqual(2, cache[2])
        self.assertEqual(3, cache[3])

        items = cache.expire(3)
        self.assertEqual({(1, 1)}, set(items))
        self.assertEqual({2, 3}, set(cache))
        self.assertEqual(2, len(cache))
        self.assertNotIn(1, cache)
        self.assertEqual(2, cache[2])
        self.assertEqual(3, cache[3])

        items = cache.expire(4)
        self.assertEqual({(2, 2)}, set(items))
        self.assertEqual({3}, set(cache))
        self.assertEqual(1, len(cache))
        self.assertNotIn(1, cache)
        self.assertNotIn(2, cache)
        self.assertEqual(3, cache[3])

        items = cache.expire(5)
        self.assertEqual({(3, 3)}, set(items))
        self.assertEqual(set(), set(cache))
        self.assertEqual(0, len(cache))
        self.assertNotIn(1, cache)
        self.assertNotIn(2, cache)
        self.assertNotIn(3, cache)

    def test_ttl_iter_expired(self):
        cache = TTLCache[int, int, int](maxsize=2, ttl=2, timer=Timer())
        cache[1] = 1
        cache.timer.tick()
        cache[2] = 2
        cache.timer.tick()
        # a bare for loop skips expired items without removing them
        self.assertEqual([2], [key for key in cache])
        self.assertEqual({(1, 1)}, set(cache.expire()))
        self.assertEqual([2], [key for key in cache])

    def test_ttl_atomic(self):
        cache = TTLCache[int, int | None, int](maxsize=1, ttl=2, timer=Timer(auto=True))
        cache[1] = 1
        self.assertEqual(1, cache[1])
        cache[1] = 1
        self.assertEqual(1, cache.get(1))
        cache[1] = 1
        self.assertEqual(1, cache.pop(1))
        cache[1] = 1
        self.assertEqual(1, cache.setdefault(1))  # implicit default=None
        cache[1] = 1
        cache.clear()
        self.assertEqual(0, len(cache))

    def test_ttl_tuple_key(self):
        cache = TTLCache[tuple[int, ...], int, int](maxsize=1, ttl=1, timer=Timer())
        self.assertEqual(1, cache.ttl)

        cache[(1, 2, 3)] = 42
        self.assertEqual(42, cache[(1, 2, 3)])
        cache.timer.tick()
        with self.assertRaises(KeyError):
            cache[(1, 2, 3)]
        self.assertNotIn((1, 2, 3), cache)

    def test_ttl_datetime(self):
        from datetime import datetime, timedelta

        cache = TTLCache[int, int, datetime](
            maxsize=1, ttl=timedelta(days=1), timer=datetime.now
        )

        cache[1] = 1
        self.assertEqual(1, len(cache))
        items = cache.expire(datetime.now())
        self.assertEqual([], list(items))
        self.assertEqual(1, len(cache))
        items = cache.expire(datetime.now() + timedelta(days=1))
        self.assertEqual([(1, 1)], list(items))
        self.assertEqual(0, len(cache))

    def test_ttl_datetime_overflow(self):
        from datetime import datetime, timedelta

        cache = TTLCache[str, int, datetime](
            maxsize=1, ttl=timedelta(days=1), timer=lambda: datetime.max
        )

        with self.assertRaises(OverflowError):
            cache["overflow"] = 1

        self.assertNotIn("overflow", cache)
        self.assertEqual(0, len(cache))
        self.assertEqual(0, cache.currsize)
        self.assertEqual([], list(cache))
        self.assertEqual([], cache.expire())

    def test_ttl_clear(self):
        cache = TTLCache[int, int, int](maxsize=2, ttl=2, timer=Timer())

        cache[1] = 1
        cache[2] = 2
        cache.clear()

        self.assertEqual(0, len(cache))
        self.assertEqual(0, cache.currsize)

        # verify LRU eviction order is reset after clear
        cache[3] = 3
        cache[4] = 4
        cache[3]  # access 3 to make it most recently used
        cache[5] = 5  # should evict 4 (least recently used)

        self.assertEqual(2, len(cache))
        self.assertIn(3, cache)
        self.assertIn(5, cache)
        self.assertNotIn(4, cache)

        # verify TTL expiry still works after clear
        cache[42] = 42
        cache.timer.tick()
        cache.timer.tick()
        cache.timer.tick()  # past TTL
        self.assertNotIn(42, cache)

    def test_ttl_default_timer_stubs(self):
        # type stubs must infer key and value types with the default timer
        cache: TTLCache[str, int] = TTLCache(maxsize=2, ttl=math.inf)

        cache["a"] = 1
        self.assertEqual(1, cache["a"])
        self.assertEqual(1, len(cache))
        self.assertIn("a", cache)

        sized: TTLCache[str, str] = TTLCache(maxsize=10, ttl=math.inf, getsizeof=len)

        sized["abc"] = "abc"
        self.assertEqual("abc", sized["abc"])
        self.assertEqual(3, sized.currsize)

    def test_ttl_copy(self):
        import copy

        source = TTLCache[int, int, int](maxsize=3, ttl=2, timer=Timer())
        source[1] = 1
        source[2] = 2
        source.timer.tick()
        source[3] = 3
        self.assertEqual(1, source[1])  # least recently used is now 2

        cache = copy.copy(source)
        self.assertEqual([1, 2, 3], list(cache))

        # the copy evicts as its source would, on its own
        cache[4] = 4
        self.assertEqual([1, 3, 4], list(cache))
        self.assertEqual([1, 2, 3], list(source))

        # the timer is shared, but not the caches' use of it
        self.assertIsNot(source.timer, cache.timer)
        with source.timer:
            cache.timer.tick()
            self.assertEqual(1, source.timer())
            self.assertEqual(2, cache.timer())
        self.assertEqual(2, source.timer())

        # items expire in the copy as in its source
        self.assertEqual([3, 4], list(cache))
        self.assertEqual([3], list(source))

    def test_ttl_deepcopy_timer(self):
        import copy

        class DeepCopyTimer(Timer):
            def __deepcopy__(self, memo):
                return copy.copy(self)

        source = TTLCache[int, int, int](maxsize=2, ttl=1, timer=DeepCopyTimer())
        source[1] = 1

        # a timer may define __deepcopy__ itself
        cache = copy.deepcopy(source)
        self.assertEqual([1], list(cache))

        # the copy has a timer of its own
        cache.timer.tick()
        self.assertEqual([], list(cache))
        self.assertEqual([1], list(source))

    def test_ttl_iter_modify(self):
        cache = TTLCache[int, int, int](maxsize=6, ttl=2, timer=Timer())
        cache.update({1: 1, 2: 2, 3: 3})

        # items inserted while iterating are not visited
        keys = []
        for key in cache:
            keys.append(key)
            cache[key + 10] = key
        self.assertEqual([1, 2, 3], keys)
        self.assertEqual([1, 2, 3, 11, 12, 13], list(cache))

        # items removed while iterating are skipped
        keys = []
        for key in cache:
            keys.append(key)
            cache.clear()
        self.assertEqual([1], keys)

    def test_ttl_resize_expired_first(self):
        cache = TTLCache[int, int, int](maxsize=3, ttl=2, timer=Timer())

        cache[1] = 1
        cache.timer.tick()
        cache[2] = 2
        cache[3] = 3  # key 1 expires at time 2; keys 2 and 3 stay live
        cache.timer.tick()

        # the expired item is removed before any live item is evicted
        removed = cache.resize(2)
        self.assertEqual([1], removed)
        self.assertEqual({2, 3}, set(cache))
        self.assertEqual(2, cache.maxsize)
        self.assertEqual(2, cache.currsize)

    def test_ttl_resize_expired_frees_space(self):
        cache = TTLCache[int, int, int](maxsize=4, ttl=2, timer=Timer())

        cache[1] = 1
        cache[2] = 2
        cache.timer.tick()
        cache[3] = 3
        cache[4] = 4  # keys 1, 2 expire at time 2; keys 3, 4 stay live
        cache.timer.tick()

        # the two expired entries already satisfy the smaller limit
        removed = cache.resize(2)
        self.assertEqual([1, 2], removed)
        self.assertEqual([3, 4], list(cache))
        self.assertEqual(2, cache.currsize)

    def test_ttl_resize_lru_order(self):
        cache = TTLCache[int, int, int](maxsize=4, ttl=10, timer=Timer())

        cache[1] = 1
        cache[2] = 2
        cache[3] = 3
        cache[4] = 4
        cache[1]  # make key 1 most recently used, key 2 least recently used

        # nothing expires within this short test, so LRU order applies
        removed = cache.resize(2)
        self.assertEqual([2, 3], removed)
        self.assertEqual({4, 1}, set(cache))
        self.assertEqual(2, cache.currsize)

        # further shrinking keeps the most recently used survivor
        self.assertEqual([4], cache.resize(1))
        self.assertEqual([1], list(cache))

    def test_ttl_resize_negative(self):
        cache = TTLCache[int, int, int](maxsize=2, ttl=2, timer=Timer())
        cache[1] = 1

        with self.assertRaises(ValueError):
            cache.resize(-1)

        # a failed resize must neither change the limit nor expire items
        self.assertEqual(2, cache.maxsize)
        self.assertEqual(1, cache.currsize)
        self.assertIn(1, cache)

    def test_ttl_resize_getsizeof(self):
        cache = TTLCache[int, int, int](
            maxsize=10, ttl=10, timer=Timer(), getsizeof=lambda x: x
        )
        cache[1] = 1
        cache[2] = 2
        cache[3] = 3
        cache[4] = 4
        self.assertEqual(10, cache.currsize)

        # the limit is checked against total weight, not item count
        removed = cache.resize(4)
        self.assertEqual([1, 2, 3], removed)
        self.assertEqual([4], list(cache))
        self.assertEqual(4, cache.currsize)

        with self.assertRaises(ValueError):
            cache[5] = 5
        self.assertEqual([4], list(cache))

    def test_ttl_resize_getsizeof_expired(self):
        cache = TTLCache[int, int, int](
            maxsize=10, ttl=2, timer=Timer(), getsizeof=lambda x: x
        )
        cache[1] = 1
        cache[2] = 2
        cache.timer.tick()
        cache[3] = 3
        cache[4] = 4
        cache.timer.tick()  # keys 1 and 2 expire; no writes happen afterwards

        # expired weight is reported first, then live items are evicted
        # by weight until the remaining total fits the new limit
        removed = cache.resize(4)
        self.assertEqual([1, 2, 3], removed)
        self.assertEqual([4], list(cache))
        self.assertEqual(4, cache.currsize)

    def test_ttl_and_lru_resize_same_data(self):
        # The same data distribution goes into a TTL and an LRU cache.
        ttl = TTLCache[int, int, int](maxsize=5, ttl=2, timer=Timer())
        lru = LRUCache[int, int](maxsize=5)
        ttl[1] = lru[1] = 1
        ttl[2] = lru[2] = 2
        ttl.timer.tick()
        ttl[3] = lru[3] = 3
        ttl[4] = lru[4] = 4
        ttl[5] = lru[5] = 5
        # make key 1 most recently used, key 2 least recently used in LRU
        lru[1]
        ttl.timer.tick()  # keys 1 and 2 expire in the TTL cache only

        # moderate shrink: the TTL cache loses its expired entries and
        # needs no further eviction, while the LRU cache evicts live ones
        ttl_removed = ttl.resize(3)
        lru_removed = lru.resize(3)

        self.assertEqual([1, 2], ttl_removed)
        self.assertEqual({3, 4, 5}, set(ttl))
        self.assertEqual(3, len(ttl))
        self.assertEqual([2, 3], lru_removed)
        self.assertEqual({1, 4, 5}, set(lru))
        self.assertEqual(3, len(lru))
        self.assertEqual(3, lru.maxsize)
        self.assertEqual(3, ttl.maxsize)

        # shrink to the limit and compare what each cache really keeps:
        # the TTL cache reports expiry order first, the LRU cache pure LRU
        ttl_removed = ttl.resize(0)
        lru_removed = lru.resize(0)

        self.assertEqual([3, 4, 5], ttl_removed)
        self.assertEqual([4, 5, 1], lru_removed)
        self.assertEqual(0, len(ttl))
        self.assertEqual(0, len(lru))
        self.assertEqual(0, ttl.currsize)
        self.assertEqual(0, lru.currsize)
