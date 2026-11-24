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
    assert not buff.evict_exceptions


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
    # that stays in fast due to callback raising. The failure is recorded in
    # evict_exceptions instead of propagating out of __setitem__.
    buff["w"] = 11

    assert f2s == ["x", "y", "z"]
    assert s2f == []
    assert a == {"w": 11}
    assert b == {"x": 1, "y": 2, "z": 8}
    assert set(buff.evict_exceptions) == {"w"}
    assert isinstance(buff.evict_exceptions["w"], MyError)
    assert buff.fast.evict_exceptions is buff.evict_exceptions


def test_evict_unserializable_stays_in_fast_others_spill():
    """A value that the slow layer refuses (e.g. it can't be serialized) must stay in
    fast and be recorded in evict_exceptions. Eviction must roll the failed item back,
    keep reducing the fast weight by spilling the other candidates, and re-evaluate the
    key once the same-named value is successfully replaced.
    """

    class NotSerializable:
        """A value that the slow layer refuses to store"""

    class Slow(UserDict):
        def __setitem__(self, key, value):
            if isinstance(value[0], NotSerializable):
                raise TypeError(f"{key}: value is not serializable")
            super().__setitem__(key, value)

    a = {}
    b = Slow()

    def weight(k, v):
        return v[1]

    buff = Buffer(a, b, n=10, weight=weight)

    bad = NotSerializable()
    # The two large values sit on the oldest side of the LRU. x already carries an
    # unserializable value in place, so insertion order is [x, y, z]
    buff.set_noevict("x", (bad, 6))  # oldest; writing it to slow will fail
    buff.set_noevict("y", (0, 6))
    buff.set_noevict("z", (0, 1))  # newest
    assert list(buff.fast.order) == ["x", "y", "z"]
    assert buff.fast.total_weight == 13

    # First eviction, all the way down: x fails to write to slow and stays in fast.
    # Eviction rolls x back and keeps going, spilling the other two candidates instead,
    # so that the only weight left in fast is the one that genuinely cannot be moved.
    buff.evict_until_below_target(0)

    # Fast still holds the unserializable item; slow received the other two
    assert set(a) == {"x"}
    assert a["x"] == (bad, 6)
    assert set(b.data) == {"y", "z"}
    assert b.data == {"y": (0, 6), "z": (0, 1)}
    assert buff.fast.total_weight == 6

    # The failure is explainable and points at the offending key and exception
    assert set(buff.evict_exceptions) == {"x"}
    assert isinstance(buff.evict_exceptions["x"], TypeError)
    assert "not serializable" in str(buff.evict_exceptions["x"])

    # Re-running eviction does not retry the still-bad key and does not raise
    buff.evict_until_below_target(0)
    assert set(a) == {"x"}
    assert set(b.data) == {"y", "z"}
    assert set(buff.evict_exceptions) == {"x"}

    # Explicitly evicting the failing key retries it and re-raises
    with pytest.raises(TypeError, match="not serializable"):
        buff.fast.evict("x")
    assert set(a) == {"x"}
    assert set(buff.evict_exceptions) == {"x"}

    # Replace the first item with a serializable value with the same key
    buff["x"] = (1, 6)
    assert not buff.evict_exceptions
    assert set(a) == {"x"}
    assert a["x"] == (1, 6)

    # Second eviction: the replacement can now land on the slow layer
    buff.evict_until_below_target(0)
    assert not a
    assert set(b.data) == {"x", "y", "z"}
    assert b.data["x"] == (1, 6)
    assert not buff.evict_exceptions


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
