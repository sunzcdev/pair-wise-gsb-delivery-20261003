import unittest

from cachetools import LFUCache

from . import CacheTestMixin


class LFUCacheTest(unittest.TestCase, CacheTestMixin):
    Cache = LFUCache

    def test_lfu(self):
        cache = LFUCache[int, int](maxsize=2)

        cache[1] = 1
        cache[1]
        cache[2] = 2
        cache[3] = 3

        self.assertEqual(len(cache), 2)
        self.assertEqual(cache[1], 1)
        self.assertNotIn(2, cache)
        self.assertEqual(cache[3], 3)

        cache[3]
        cache[4] = 4
        self.assertEqual(len(cache), 2)
        self.assertNotIn(1, cache)
        self.assertEqual(cache[3], 3)
        self.assertEqual(cache[4], 4)

    def test_lfu_tie(self):
        cache = LFUCache[int, int](maxsize=3)

        cache[1] = 1
        cache[2] = 2
        cache[3] = 3

        cache[3]
        cache[2]
        cache[1]

        # do not use self.assertEqual(cache[1], 1) here, since that
        # would mess up usage counts
        self.assertEqual(len(cache), 3)
        self.assertIn(1, cache)
        self.assertIn(2, cache)
        self.assertIn(3, cache)

        cache[4] = 4
        self.assertEqual(len(cache), 3)
        self.assertIn(1, cache)
        self.assertIn(2, cache)
        self.assertNotIn(3, cache)
        self.assertIn(4, cache)

        cache[4]
        cache[5] = 5
        self.assertEqual(len(cache), 3)
        self.assertIn(1, cache)
        self.assertNotIn(2, cache)
        self.assertNotIn(3, cache)
        self.assertIn(4, cache)
        self.assertIn(5, cache)

    def test_lfu_tie_insert_only(self):
        cache = LFUCache[int, int](maxsize=3)

        cache[1] = 1
        cache[2] = 2
        cache[3] = 3

        # items never retrieved are tied at use count 1, so they are
        # evicted in insertion order; the set-based implementation
        # used up to v7.2 only fails this from key 17 on
        for key in range(4, 20):
            cache[key] = key
            self.assertEqual(len(cache), 3)
            self.assertNotIn(key - 3, cache)
            self.assertIn(key - 2, cache)
            self.assertIn(key - 1, cache)
            self.assertIn(key, cache)

    def test_lfu_decay(self):
        # fixed access sequence: build up old hot keys, then switch
        # to a new working set
        def run(cache):
            for key in (1, 2, 3):
                cache[key] = key
            for _ in range(15):
                for key in (1, 2, 3):
                    cache[key]
            for _ in range(16):
                for key in (4, 5, 6):
                    cache[key] = key
            return list(cache)

        # without decay, the old hot keys keep their high use counts,
        # so the new working set cannot move in
        self.assertEqual([2, 3, 6], run(LFUCache(maxsize=3)))
        # with decay, the old use counts fade away and the new working
        # set gradually replaces the old hot keys
        decaying = LFUCache[int, int](maxsize=3, decay_interval=8)
        self.assertEqual([5, 6, 4], run(decaying))
        # with decay_interval larger than the number of accesses,
        # decay never kicks in and behavior matches the default
        self.assertEqual([2, 3, 6], run(LFUCache(maxsize=3, decay_interval=1000)))

    def test_lfu_decay_tie(self):
        cache = LFUCache[int, int](maxsize=4, decay_interval=3)

        cache[1] = 1
        cache[2] = 2
        cache[3] = 3  # third use triggers decay, links merge in order
        cache[1]
        cache[2]
        cache[4] = 4  # third use triggers decay, counts 2 decay to 1

        # merged links keep their relative order, so ties are still
        # broken by least recent use
        self.assertEqual([3, 1, 2, 4], [cache.popitem()[0] for _ in range(4)])

    def test_lfu_decay_params(self):
        with self.assertRaises(ValueError):
            LFUCache(maxsize=1, decay_interval=0)
        with self.assertRaises(ValueError):
            LFUCache(maxsize=1, decay_interval=1, decay_factor=0)
        with self.assertRaises(ValueError):
            LFUCache(maxsize=1, decay_interval=1, decay_factor=1)

        # decay_factor is ignored without decay_interval
        cache = LFUCache[int, int](maxsize=1, decay_factor=2)
        cache[1] = 1
        self.assertEqual([1], list(cache))

    def test_lfu_decay_clear(self):
        cache = LFUCache[int, int](maxsize=2, decay_interval=2)

        cache[1] = 1
        cache.clear()

        # the use counter is reset, so the first use after clear()
        # does not trigger decay
        cache[1] = 1
        cache[1]
        cache[2] = 2
        cache[3] = 3

        self.assertEqual([1, 3], list(cache))

    def test_lfu_decay_copy_pickle(self):
        import copy
        import pickle

        source = LFUCache[int, int](maxsize=2, decay_interval=2)
        source[1] = 1
        source[2] = 2
        source[2]

        for cache in (
            copy.copy(source),
            copy.deepcopy(source),
            pickle.loads(pickle.dumps(source)),
        ):
            cache[3] = 3
            cache[4] = 4
            self.assertEqual([3, 4], list(cache))

        self.assertEqual([1, 2], list(source))

    def test_lfu_getsizeof(self):
        cache = LFUCache[int, int](maxsize=3, getsizeof=lambda x: x)

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

    def test_lfu_update_existing(self):
        cache = LFUCache[int, int | str](maxsize=2)

        cache[1] = 1
        cache[2] = 2
        cache[1] = "updated"
        cache[3] = 3

        self.assertEqual(cache[1], "updated")
        self.assertIn(3, cache)
        self.assertNotIn(2, cache)

    def test_lfu_clear(self):
        cache = LFUCache[int, int](maxsize=2)

        cache[1] = 1
        cache[1]  # increment frequency
        cache[1]  # increment frequency again
        cache[2] = 2

        cache.clear()

        self.assertEqual(0, len(cache))
        self.assertEqual(0, cache.currsize)

        # verify LFU frequency tracking is reset after clear
        cache[3] = 3
        cache[4] = 4
        cache[3]  # access 3 to increment frequency
        cache[5] = 5  # should evict 4 (least frequently used)

        self.assertEqual(2, len(cache))
        self.assertIn(3, cache)
        self.assertIn(5, cache)
        self.assertNotIn(4, cache)

    def test_lfu_copy_use_counts(self):
        """Regression test for recursion errors from copying and
        pickling LFUCache with many use counts."""
        import copy
        import pickle
        import sys

        n = sys.getrecursionlimit()
        source = LFUCache[int, int](maxsize=n + 1)
        for key in range(n):
            source[key] = key
            for _ in range(key):
                source[key]  # key ends up with use count key + 1
        source[n] = n  # tied with key 0 at use count 1

        for cache in (
            copy.copy(source),
            copy.deepcopy(source),
            pickle.loads(pickle.dumps(source)),
        ):
            # compare keys only, since items() would count as a use
            self.assertEqual(list(source), list(cache))
            # the copy evicts by use count, then by age
            order = [cache.popitem()[0] for _ in range(len(cache))]
            self.assertEqual([0, n, *range(1, n)], order)

        self.assertEqual(n + 1, len(source))
