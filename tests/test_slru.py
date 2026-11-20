import copy
import unittest

from cachetools import SLRUCache

from . import CacheTestMixin


def eviction_order(cache):
    """The keys popitem() would return, without touching the cache."""
    clone = copy.copy(cache)
    return [clone.popitem()[0] for _ in range(len(cache))]


def segments(cache):
    """The (probationary, protected) segment keys."""
    return (
        list(cache._SLRUCache__probation),
        list(cache._SLRUCache__protected),
    )


class SLRUCacheTest(unittest.TestCase, CacheTestMixin):
    Cache = SLRUCache

    def test_slru_default_maxprotected(self):
        cache = SLRUCache[int, int](maxsize=10)
        self.assertEqual(5, cache.maxprotected)

    def test_slru_maxprotected_negative(self):
        with self.assertRaises(ValueError):
            SLRUCache(maxsize=10, maxprotected=-1)

    def test_slru_promotion_and_demotion(self):
        cache = SLRUCache[int, int](maxsize=4, maxprotected=2)

        cache[1] = 1
        cache[2] = 2
        cache[1]  # repeated hit: 1 is promoted to the protected segment

        cache[3] = 3
        cache[2]  # 2 is promoted; the protected segment is now full
        cache[4] = 4  # evicts probation LRU 3 to make room

        # probationary items (3, 4) are all discarded before protected
        # hot items (1, 2)
        self.assertEqual([3, 4, 1, 2], eviction_order(cache))

        # promoting 4 demotes protected LRU 1 back to the probationary
        # segment's MRU end, ahead of the one-off key 3
        self.assertEqual(4, cache[4])
        self.assertEqual([3, 1, 2, 4], eviction_order(cache))
        self.assertEqual(4, len(cache))

        # the one-off key is evicted before the demoted hot key
        self.assertEqual((3, 3), cache.popitem())
        self.assertEqual((1, 1), cache.popitem())
        self.assertEqual([2, 4], eviction_order(cache))

    def test_slru_popitem_evicts_probation_first(self):
        cache = SLRUCache[int, int](maxsize=4, maxprotected=2)

        cache[1] = 1
        cache[2] = 2
        cache[1]
        cache[2]
        cache[3] = 3
        cache[4] = 4

        self.assertEqual((3, 3), cache.popitem())
        self.assertEqual((4, 4), cache.popitem())
        self.assertEqual((1, 1), cache.popitem())
        self.assertEqual((2, 2), cache.popitem())
        with self.assertRaises(KeyError):
            cache.popitem()

    def test_slru_hot_keys_survive_scan(self):
        # service warm-up scenario: a few hot keys, then a stream of
        # brand-new one-off keys must not flush the protected segment
        cache = SLRUCache[str, int](maxsize=10, maxprotected=4)

        for key in ("hot-a", "hot-b", "hot-c", "hot-d"):
            cache[key] = 1
            cache[key]  # repeated hit: promote to the protected segment

        for i in range(20):
            cache["scan-%d" % i] = i

        self.assertEqual(10, len(cache))
        for key in ("hot-a", "hot-b", "hot-c", "hot-d"):
            self.assertIn(key, cache)

        # all six probationary scan keys are evicted before any hot key
        order = eviction_order(cache)
        self.assertEqual(
            ["scan-14", "scan-15", "scan-16", "scan-17", "scan-18", "scan-19"],
            order[:6],
        )
        self.assertEqual(
            ["hot-a", "hot-b", "hot-c", "hot-d"],
            order[6:],
        )

    def test_slru_key_lives_in_one_segment_only(self):
        cache = SLRUCache[int, int](maxsize=4, maxprotected=2)

        cache[1] = 1
        self.assertEqual(([1], []), segments(cache))

        cache[1]  # promotion removes the key from probation first
        self.assertEqual(([], [1]), segments(cache))

        cache[2] = 2
        cache[2]  # protected segment is now full
        self.assertEqual(([], [1, 2]), segments(cache))

        cache[3] = 3
        cache[3]  # promoting 3 demotes protected LRU 1
        self.assertEqual(([1], [2, 3]), segments(cache))

        cache[1]  # re-promotion swaps the segments back
        self.assertEqual(([2], [3, 1]), segments(cache))

        # no key is ever counted in both segments
        probation, protected = segments(cache)
        self.assertFalse(set(probation) & set(protected))
        self.assertEqual(len(cache), len(probation) + len(protected))

    def test_slru_repeated_hits_reorder_protected(self):
        cache = SLRUCache[int, int](maxsize=4, maxprotected=3)

        cache[1] = 1
        cache[2] = 2
        cache[3] = 3
        for key in (1, 2, 3):
            cache[key]

        cache[1]  # 1 becomes most recently used in the protected segment
        cache[4] = 4
        cache[4]  # promoting 4 demotes protected LRU 2, not 1

        self.assertIn(1, cache)
        self.assertIn(2, cache)  # demoted to probation
        self.assertIn(3, cache)
        self.assertIn(4, cache)
        self.assertEqual((2, 2), cache.popitem())

    def test_slru_maxprotected_zero(self):
        cache = SLRUCache[int, int](maxsize=2, maxprotected=0)

        cache[1] = 1
        cache[2] = 2
        cache[1]  # no protected segment: behaves like a plain LRU cache
        cache[3] = 3

        self.assertNotIn(2, cache)
        self.assertEqual(1, cache[1])
        self.assertEqual(3, cache[3])
        self.assertEqual([1, 3], eviction_order(cache))

    def test_slru_getsizeof(self):
        cache = SLRUCache[int, int](maxsize=20, maxprotected=7, getsizeof=lambda x: x)

        cache[1] = 1
        cache[2] = 2
        cache[3] = 3
        cache[1]
        cache[2]  # protected segment now holds sizes 1 and 2

        cache[5] = 5
        cache[5]  # sizes 3 + 5 would exceed 7: demote protected LRU 1
        self.assertIn(1, cache)  # demoted to probation, not discarded
        self.assertEqual(11, cache.currsize)

        cache[4] = 4  # total 15 still fits the overall maxsize
        # promoting 4 (size 4) demotes protected items 2 and 5, in order
        cache[4]
        self.assertIn(1, cache)
        self.assertIn(2, cache)
        self.assertIn(5, cache)
        self.assertEqual([3, 1, 2, 5, 4], eviction_order(cache))
        self.assertEqual(3, cache.popitem()[0])

        with self.assertRaises(ValueError):
            cache[21] = 21

    def test_slru_getsizeof_update_weight_once(self):
        # updating a protected key's weight must count it once only
        cache = SLRUCache[int, int](maxsize=10, maxprotected=10, getsizeof=lambda x: x)

        cache[2] = 2
        cache[2]
        self.assertEqual(2, cache.currsize)

        cache[2] = 5
        self.assertEqual(5, cache.currsize)
        self.assertEqual(5, cache[2])

        cache[2] = 1
        self.assertEqual(1, cache.currsize)
        # the key still lives in exactly one segment
        self.assertEqual(1, len(cache))

    def test_slru_update_existing(self):
        cache = SLRUCache[int, int | str](maxsize=4, maxprotected=2)

        cache[1] = 1
        cache[2] = 2
        cache[1]  # promote 1
        cache[1] = "updated"  # stays protected, stays counted once

        self.assertEqual("updated", cache[1])
        self.assertEqual(2, len(cache))

        cache[3] = 3
        cache[4] = 4
        self.assertEqual([2, 3, 4, 1], eviction_order(cache))

    def test_slru_clear(self):
        cache = SLRUCache[int, int](maxsize=4, maxprotected=2)

        cache[1] = 1
        cache[2] = 2
        cache[1]
        cache.clear()

        self.assertEqual(0, len(cache))
        self.assertEqual(0, cache.currsize)

        # both segments work again after clear
        cache[3] = 3
        cache[4] = 4
        cache[3]
        cache[5] = 5
        self.assertIn(3, cache)
        self.assertIn(4, cache)
        self.assertIn(5, cache)

    def test_slru_copy(self):
        source = SLRUCache[int, int](maxsize=4, maxprotected=2)
        source.update({1: 1, 2: 2, 3: 3})
        source[1]
        source[2]  # protected: 1, 2; probation: 3

        cache = copy.copy(source)
        self.assertEqual(source, cache)
        self.assertEqual(source.maxprotected, cache.maxprotected)
        self.assertEqual(eviction_order(source), eviction_order(cache))

        # the copy evicts on its own, in the same order
        cache[4] = 4  # full cache: protected LRU 1 is demoted, then
        # probation LRU 3 is evicted to make room
        self.assertEqual([1, 4, 2, 3], eviction_order(cache))
        self.assertEqual((1, 1), cache.popitem())

        # the source keeps all its items and segment membership
        self.assertEqual(3, len(source))
        self.assertEqual([1, 2, 3], eviction_order(source))

        # promotions on the copy leave the source untouched
        cache[3] = 3
        cache[3]
        self.assertIn(3, source)
        self.assertEqual([1, 2, 3], eviction_order(source))
