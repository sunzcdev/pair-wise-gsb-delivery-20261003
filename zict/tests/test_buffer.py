import random
import threading
from collections import UserDict
from concurrent.futures import ThreadPoolExecutor

import pytest

from zict import Buffer
from zict.tests import utils_test


def test_simple():
    a = {}
    b = {}
    buff = Buffer(a, b, n=10, weight=lambda k, v: v)

    buff["x"] = 1
    buff["y"] = 2

    assert buff["x"] == 1
    assert buff["y"] == 2
    assert a == {"x": 1, "y": 2}
    assert buff.fast.total_weight == 3

    buff["z"] = 8
    assert a == {"y": 2, "z": 8}
    assert b == {"x": 1}

    assert buff["x"] == 1
    assert a == {"x": 1, "z": 8}
    assert b == {"y": 2}

    assert "x" in buff
    assert "y" in buff
    assert "missing" not in buff

    buff["y"] = 1
    assert a == {"x": 1, "y": 1, "z": 8}
    assert buff.fast.total_weight == 10
    assert b == {}

    del buff["z"]
    assert a == {"x": 1, "y": 1}
    assert buff.fast.total_weight == 2
    assert b == {}

    del buff["y"]
    assert a == {"x": 1}
    assert buff.fast.total_weight == 1
    assert b == {}

    assert "y" not in buff

    buff["a"] = 5
    assert set(buff) == set(buff.keys()) == {"a", "x"}

    fast_keys = set(buff.fast)
    slow_keys = set(buff.slow)
    assert not (fast_keys & slow_keys)
    assert fast_keys | slow_keys == set(buff)

    # Overweight element stays in slow mapping
    buff["b"] = 1000
    assert "b" in buff.slow
    assert set(buff.fast) == fast_keys
    assert set(buff.slow) == {"b"} | slow_keys
    assert "b" in buff
    assert buff["b"] == 1000


def test_setitem_avoid_fast_slow_duplicate():
    a = {}
    b = {}
    buff = Buffer(a, b, n=10, weight=lambda k, v: v)
    for first, second in [(1, 12), (12, 1)]:
        buff["a"] = first
        assert buff["a"] == first
        buff["a"] = second
        assert buff["a"] == second

        fast_keys = set(buff.fast)
        slow_keys = set(buff.slow)
        assert not (fast_keys & slow_keys)
        assert fast_keys | slow_keys == set(buff)

        del buff["a"]
        assert "a" not in buff
        assert "a" not in a
        assert "a" not in b


def test_mapping():
    """
    Test mapping interface for Buffer().
    """
    a = {}
    b = {}
    buff = Buffer(a, b, n=2)
    utils_test.check_mapping(buff)
    utils_test.check_closing(buff)

    buff.clear()
    assert not buff.slow
    assert not buff._cancel_restore
    assert not buff.fast
    assert not buff.fast.d
    assert not buff.fast.weights
    assert not buff.fast.total_weight
    assert not buff.fast._cancel_evict


def test_callbacks():
    f2s = []

    def f2s_cb(k, v):
        f2s.append(k)

    s2f = []

    def s2f_cb(k, v):
        s2f.append(k)

    a = {}
    b = {}
    buff = Buffer(
        a,
        b,
        n=10,
        weight=lambda k, v: v,
        fast_to_slow_callbacks=f2s_cb,
        slow_to_fast_callbacks=s2f_cb,
    )

    buff["x"] = 1
    buff["y"] = 2

    assert buff["x"] == 1
    assert buff["y"] == 2
    assert not f2s
    assert not s2f

    buff["z"] = 8

    assert f2s == ["x"]
    assert s2f == []
    buff["z"]

    assert f2s == ["x"]
    assert s2f == []

    buff["x"]
    assert f2s == ["x", "y"]
    assert s2f == ["x"]


def test_callbacks_exception_catch():
    class MyError(Exception):
        pass

    f2s = []

    def f2s_cb(k, v):
        if v > 10:
            raise MyError()
        f2s.append(k)

    s2f = []

    def s2f_cb(k, v):
        s2f.append(k)

    a = {}
    b = {}
    buff = Buffer(
        a,
        b,
        n=10,
        weight=lambda k, v: v,
        fast_to_slow_callbacks=f2s_cb,
        slow_to_fast_callbacks=s2f_cb,
    )

    buff["x"] = 1
    buff["y"] = 2

    assert buff["x"] == 1
    assert buff["y"] == 2
    assert not f2s
    assert not s2f
    assert a == {"x": 1, "y": 2}  # keys are in fast/memory
    assert not b

    # Add key < n but total weight > n this will move x out of fast
    buff["z"] = 8

    assert f2s == ["x"]
    assert s2f == []
    assert a == {"y": 2, "z": 8}
    assert b == {"x": 1}

    # Add key > n, again total weight > n this will move everything to slow except w
    # that stays in fast due to callback raising
    with pytest.raises(MyError):
        buff["w"] = 11

    assert f2s == ["x", "y", "z"]
    assert s2f == []
    assert a == {"w": 11}
    assert b == {"x": 1, "y": 2, "z": 8}


def test_n_offset():
    buff = Buffer({}, {}, n=5)
    assert buff.n == 5
    assert buff.fast.n == 5
    buff.n = 3
    assert buff.fast.n == 3
    assert buff.offset == 0
    assert buff.fast.offset == 0
    buff.offset = 2
    assert buff.offset == 2
    assert buff.fast.offset == 2


def test_set_noevict():
    a = {}
    b = {}
    f2s = []
    s2f = []
    buff = Buffer(
        a,
        b,
        n=5,
        weight=lambda k, v: v,
        fast_to_slow_callbacks=lambda k, v: f2s.append(k),
        slow_to_fast_callbacks=lambda k, v: s2f.append(k),
    )
    buff.set_noevict("x", 3)
    buff.set_noevict("y", 3)  # Would cause x to move to slow
    buff.set_noevict("z", 6)  # >n; would be immediately evicted

    assert a == {"x": 3, "y": 3, "z": 6}
    assert b == {}
    assert f2s == s2f == []

    buff.evict_until_below_target()
    assert a == {"y": 3}
    assert b == {"z": 6, "x": 3}
    assert f2s == ["z", "x"]
    assert s2f == []

    # set_noevict clears slow
    f2s.clear()
    buff.set_noevict("x", 1)
    assert a == {"y": 3, "x": 1}
    assert b == {"z": 6}
    assert f2s == s2f == []

    # Custom target; 0 != None
    buff.evict_until_below_target(0)
    assert a == {}
    assert b == {"z": 6, "x": 1, "y": 3}
    assert f2s == ["y", "x"]
    assert s2f == []


def test_evict_restore_during_iter():
    """Test that __iter__ won't be disrupted if another thread evicts or restores a key"""
    buff = Buffer({"x": 1, "y": 2}, {"z": 3}, n=5)
    assert list(buff) == ["x", "y", "z"]
    it = iter(buff)
    assert next(it) == "x"
    buff.fast.evict("x")
    assert next(it) == "y"
    assert buff["x"] == 1
    assert next(it) == "z"
    with pytest.raises(StopIteration):
        next(it)


@pytest.mark.parametrize("event", ("set", "set_noevict", "del"))
@pytest.mark.parametrize("when", ("before", "after"))
def test_cancel_evict(event, when):
    """See also:

    test_cancel_restore
    test_lru.py::test_cancel_evict
    """
    ev1 = threading.Event()
    ev2 = threading.Event()

    class Slow(UserDict):
        def __setitem__(self, k, v):
            if when == "before":
                ev1.set()
                assert ev2.wait(timeout=5)
                super().__setitem__(k, v)
            else:
                super().__setitem__(k, v)
                ev1.set()
                assert ev2.wait(timeout=5)

    buff = Buffer({}, Slow(), n=100, weight=lambda k, v: v)
    buff.set_noevict("x", 1)
    with ThreadPoolExecutor(1) as ex:
        fut = ex.submit(buff.fast.evict)
        assert ev1.wait(timeout=5)
        # cb is running

        if event == "set":
            buff["x"] = 2
        elif event == "set_noevict":
            buff.set_noevict("x", 2)
        else:
            assert event == "del"
            del buff["x"]
        ev2.set()
        assert fut.result() == (None, None, 0)

    if event in ("set", "set_noevict"):
        assert buff.fast == {"x": 2}
        assert not buff.slow
        assert buff.fast.weights == {"x": 2}
        assert list(buff.fast.order) == ["x"]
    else:
        assert not buff.fast
        assert not buff.slow
        assert not buff.fast.weights
        assert not buff.fast.order

    assert not buff.fast._cancel_evict


@pytest.mark.parametrize("event", ("set", "set_noevict", "del"))
@pytest.mark.parametrize("when", ("before", "after"))
def test_cancel_restore(event, when):
    """See also:

    test_cancel_evict
    test_lru.py::test_cancel_evict
    """
    ev1 = threading.Event()
    ev2 = threading.Event()

    class Slow(UserDict):
        def __getitem__(self, k):
            if when == "before":
                ev1.set()
                assert ev2.wait(timeout=5)
                return super().__getitem__(k)
            else:
                out = super().__getitem__(k)
                ev1.set()
                assert ev2.wait(timeout=5)
                return out

    buff = Buffer({}, Slow(), n=100, weight=lambda k, v: v)
    buff.set_noevict("x", 1)
    buff.fast.evict()
    assert not buff.fast
    assert set(buff.slow) == {"x"}

    with ThreadPoolExecutor(1) as ex:
        fut = ex.submit(buff.__getitem__, "x")
        assert ev1.wait(timeout=5)
        # cb is running

        if event == "set":
            buff["x"] = 2
        elif event == "set_noevict":
            buff.set_noevict("x", 2)
        else:
            assert event == "del"
            del buff["x"]
        ev2.set()

        with pytest.raises(KeyError, match="x"):
            fut.result()

    if event in ("set", "set_noevict"):
        assert buff.fast == {"x": 2}
        assert not buff.slow
        assert buff.fast.weights == {"x": 2}
        assert list(buff.fast.order) == ["x"]
    else:
        assert not buff.fast
        assert not buff.slow
        assert not buff.fast.weights
        assert not buff.fast.order

    assert not buff._cancel_restore


def test_peek():
    """peek() returns the current value without changing the fast layer or the LRU;
    only a regular __getitem__ promotes a cold key back into fast.
    """
    f2s = []
    s2f = []
    a = {}
    b = {}
    buff = Buffer(
        a,
        b,
        n=10,
        weight=lambda k, v: v,
        fast_to_slow_callbacks=lambda k, v: f2s.append(k),
        slow_to_fast_callbacks=lambda k, v: s2f.append(k),
    )
    buff["x"] = 1
    buff["y"] = 2
    buff["z"] = 8  # overflows: x is evicted to slow as the coldest key
    assert a == {"y": 2, "z": 8}
    assert b == {"x": 1}
    order_before = list(buff.fast.order)

    # Peeking a fast key does not touch the LRU order
    assert buff.peek("z") == 8
    assert list(buff.fast.order) == order_before
    assert a == {"y": 2, "z": 8}
    assert b == {"x": 1}

    # Peeking a slow (cold) key does not promote it: the fast layer, the LRU and the
    # callbacks are all left untouched
    assert buff.peek("x") == 1
    assert a == {"y": 2, "z": 8}
    assert b == {"x": 1}
    assert list(buff.fast.order) == order_before
    assert f2s == ["x"]
    assert s2f == []

    # A genuine miss is reported explicitly
    with pytest.raises(KeyError, match="missing"):
        buff.peek("missing")

    # A regular read still follows the existing rules and promotes the key to fast
    assert buff["x"] == 1
    assert a == {"x": 1, "z": 8}
    assert b == {"y": 2}
    assert s2f == ["x"]

    # After the promotion, peek() keeps reading from fast side-effect free
    order_before = list(buff.fast.order)
    assert buff.peek("x") == 1
    assert list(buff.fast.order) == order_before


def test_peek_concurrent_read_and_inspect():
    """A normal read and a read-only inspection hit the coldest key at the same time;
    another cold key is only inspected. Only the normal read affects the fast layer and
    the LRU, while both paths retrieve the current values.
    """
    a = {}
    # A small delay in slow makes the normal read and the inspection realistically
    # interleave while both are fetching the cold key from the slow layer
    b = utils_test.SlowDict(0.001)
    buff = Buffer(a, b, n=10, weight=lambda k, v: v)
    buff["x"] = 1  # coldest key: will end up in slow
    buff["y"] = 2  # second coldest key: also in slow
    buff["z"] = 8
    buff["w"] = 1  # pushes y out of fast too; fast now holds z, w
    assert set(buff.fast) == {"z", "w"}
    assert dict(buff.slow) == {"x": 1, "y": 2}

    fast_before = dict(buff.fast)
    slow_before = dict(buff.slow)
    order_before = list(buff.fast.order)
    barrier = threading.Barrier(2)
    results = {}

    def normal_read():
        barrier.wait()
        results["read"] = buff["x"]

    def read_only_inspect():
        barrier.wait()
        results["peek_x"] = buff.peek("x")

    with ThreadPoolExecutor(2) as ex:
        f1 = ex.submit(normal_read)
        f2 = ex.submit(read_only_inspect)
        f1.result(timeout=5)
        f2.result(timeout=5)

    # Both paths got the current value of the coldest key
    assert results == {"read": 1, "peek_x": 1}

    # Only the normal read moved x from slow to fast. x weighs 1, so it fits below the
    # target and nothing is evicted; the fast LRU just gains x at the top.
    assert set(buff.fast) == {"z", "w", "x"}
    assert list(buff.fast.order) == ["z", "w", "x"]
    assert dict(buff.slow) == {"y": 2}
    assert "x" not in buff.slow

    # The other cold key is only inspected: it must stay in slow and not disturb fast
    assert buff.peek("y") == 2
    assert set(buff.fast) == {"z", "w", "x"}
    assert list(buff.fast.order) == ["z", "w", "x"]
    assert dict(buff.slow) == {"y": 2}

    # Sanity: the slow-only inspection never caused any promotion
    assert fast_before == {"z": 8, "w": 1}
    assert slow_before == {"x": 1, "y": 2}
    assert order_before == ["z", "w"]


def test_peek_missing_during_restore():
    """If the key is being deleted while another thread is restoring it, a peek() that
    cannot find the key anywhere reports the missing item explicitly instead of
    triggering a restore itself.
    """
    ev1 = threading.Event()
    ev2 = threading.Event()

    class Slow(UserDict):
        def __getitem__(self, k):
            if k == "x":
                ev1.set()
                assert ev2.wait(timeout=5)
            return super().__getitem__(k)

    buff = Buffer({}, Slow(), n=100, weight=lambda k, v: v)
    buff.set_noevict("x", 1)
    buff.fast.evict()
    assert not buff.fast
    assert set(buff.slow) == {"x"}

    with ThreadPoolExecutor(1) as ex:
        # A normal read starts a slow-to-fast restore and blocks inside slow.__getitem__
        fut = ex.submit(buff.__getitem__, "x")
        assert ev1.wait(timeout=5)

        # Cancel the restore by deleting the key while it is in flight
        del buff["x"]
        ev2.set()
        with pytest.raises(KeyError, match="x"):
            fut.result()

    # The key is now in neither layer and peek() reports the miss explicitly
    assert not buff.fast
    assert not buff.slow
    with pytest.raises(KeyError, match="x"):
        buff.peek("x")


@pytest.mark.stress
@pytest.mark.repeat(utils_test.REPEAT_STRESS_TESTS)
def test_stress_different_keys_threadsafe():
    # Sometimes x and y can cohexist without triggering eviction
    # Sometimes x and y are individually <n but when they're both in they cause eviction
    # Sometimes x or y are heavy
    buff = Buffer(
        {},
        utils_test.SlowDict(0.001),
        n=1,
        weight=lambda k, v: random.choice([0.4, 0.9, 1.1]),
    )
    utils_test.check_different_keys_threadsafe(buff)
    assert not buff.fast
    assert not buff.slow
    utils_test.check_mapping(buff)


@pytest.mark.stress
@pytest.mark.repeat(utils_test.REPEAT_STRESS_TESTS)
def test_stress_same_key_threadsafe():
    # Sometimes x is heavy
    buff = Buffer(
        {},
        utils_test.SlowDict(0.001),
        n=1,
        weight=lambda k, v: random.choice([0.9, 1.1]),
    )
    utils_test.check_same_key_threadsafe(buff)
    assert not buff.fast
    assert not buff.slow
    utils_test.check_mapping(buff)
