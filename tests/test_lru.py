import unittest

from cachetools import LRUCache

from . import CacheTestMixin


class LRUCacheTest(unittest.TestCase, CacheTestMixin):
    Cache = LRUCache

    def test_lru(self):
        import copy

        cache = LRUCache[int, int](maxsize=2)

        cache[1] = 1
        cache[2] = 2
        cache[3] = 3

        self.assertEqual(len(cache), 2)
        self.assertEqual(cache[2], 2)
        self.assertEqual(cache[3], 3)
        self.assertNotIn(1, cache)

        cache[2]
        # a shallow copy evicts in the same order, on its own
        self.assertEqual((3, 3), copy.copy(cache).popitem())
        cache[4] = 4
        self.assertEqual(len(cache), 2)
        self.assertEqual(cache[2], 2)
        self.assertEqual(cache[4], 4)
        self.assertNotIn(3, cache)

        cache[5] = 5
        self.assertEqual(len(cache), 2)
        self.assertEqual(cache[4], 4)
        self.assertEqual(cache[5], 5)
        self.assertNotIn(2, cache)

    def test_lru_getsizeof(self):
        cache = LRUCache[int, int](maxsize=3, getsizeof=lambda x: x)

        cache[1] = 1
        cache[2] = 2

        self.assertEqual(len(cache), 2)
        self.assertEqual(cache[1], 1)
        self.assertEqual(cache[2], 2)

        cache[3] = 3

        self.assertEqual(len(cache), 1)
        self.assertEqual(cache[3], 3)
        self.assertNotIn(1, cache)
        self.assertNotIn(2, cache)

        with self.assertRaises(ValueError):
            cache[4] = 4
        self.assertEqual(len(cache), 1)
        self.assertEqual(cache[3], 3)

    def test_lru_update_existing(self):
        cache = LRUCache[int, int | str](maxsize=2)

        cache[1] = 1
        cache[2] = 2
        cache[1] = "updated"
        cache[3] = 3

        self.assertEqual(cache[1], "updated")
        self.assertIn(3, cache)
        self.assertNotIn(2, cache)

    def test_lru_clear(self):
        cache = LRUCache[int, int](maxsize=2)

        cache[1] = 1
        cache[2] = 2
        cache.clear()

        self.assertEqual(0, len(cache))
        self.assertEqual(0, cache.currsize)

        # verify LRU order is reset after clear
        cache[3] = 3
        cache[4] = 4
        cache[3]  # access 3 to make it most recently used
        cache[5] = 5  # should evict 4 (least recently used)

        self.assertEqual(2, len(cache))
        self.assertIn(3, cache)
        self.assertIn(5, cache)
        self.assertNotIn(4, cache)

    def test_lru_resize_order(self):
        cache = LRUCache[int, int](maxsize=5)
        cache.update({1: 1, 2: 2, 3: 3, 4: 4, 5: 5})
        # establish a known least-to-most recently used order
        cache[1]
        cache[3]
        cache[5]

        # shrinking follows the cache's own LRU eviction order; note
        # that list(cache) shows insertion order, not LRU order
        def eviction_order():
            order = []
            while cache:
                order.append(cache.popitem()[0])
            return order

        self.assertEqual([2, 4, 1, 3, 5], eviction_order())

    def test_lru_resize_shrink(self):
        cache = LRUCache[int, int](maxsize=5)
        cache.update({1: 1, 2: 2, 3: 3, 4: 4, 5: 5})
        # establish a known least-to-most recently used order
        cache[1]
        cache[3]
        cache[5]

        # shrinking follows the cache's own LRU eviction order
        self.assertEqual([2, 4, 1], cache.resize(2))
        self.assertEqual(2, cache.maxsize)
        self.assertEqual(2, cache.currsize)
        self.assertEqual({3, 5}, set(cache))

        # shrinking to the limit keeps the most recently used items
        self.assertEqual([3], cache.resize(1))
        self.assertEqual([5], list(cache))
        self.assertEqual(1, len(cache))

        # growing back does not bring evicted keys back
        self.assertEqual([], cache.resize(5))
        self.assertEqual([5], list(cache))
        cache[6] = 6
        self.assertEqual({5, 6}, set(cache))

    def test_lru_resize_getsizeof(self):
        cache = LRUCache[int, int](maxsize=10, getsizeof=lambda x: x)
        cache[1] = 1
        cache[2] = 2
        cache[3] = 3
        cache[4] = 4
        self.assertEqual(10, cache.currsize)
        self.assertEqual([1, 2, 3, 4], list(cache))

        # weights, not item counts, drive how much is evicted
        self.assertEqual([1, 2, 3], cache.resize(4))
        self.assertEqual([4], list(cache))
        self.assertEqual(4, cache.currsize)
        self.assertEqual(4, cache.maxsize)

        # the new weighted limit is enforced for later inserts
        with self.assertRaises(ValueError):
            cache[5] = 5
        self.assertEqual([4], list(cache))
