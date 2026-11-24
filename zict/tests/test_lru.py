import random
import threading
import time
from concurrent.futures import ThreadPoolExecutor

import pytest

from zict import LRU
from zict.tests import utils_test


def test_simple():
    d = {}
    lru = LRU(2, d)

    lru["x"] = 1
    lru["y"] = 2

    assert lru["x"] == 1
    assert lru["y"] == 2
    assert d == {"x": 1, "y": 2}

    lru["z"] = 3
    assert len(d) == 2
    assert len(lru) == 2
    assert "z" in d
    assert "z" in lru
    assert "x" not in d
    assert "y" in d

    del lru["y"]
    assert "y" not in d
    assert "y" not in lru

    lru["a"] = 5
    assert set(lru) == {"z", "a"}


def test_str():
    d = {}
    lru = LRU(2, d)

    lru["x"] = 1
    lru["y"] = 2

    assert str(lru.total_weight) in str(lru)
    assert str(lru.total_weight) in repr(lru)
    assert str(lru.n) in str(lru)
    assert str(lru.n) in repr(lru)
    assert "dict" in str(lru)
    assert "dict" in repr(lru)


def test_mapping():
    """
    Test mapping interface for LRU().
    """
    d = {}
    # 100 is more than the max length when running check_mapping()
    lru = LRU(100, d)
    utils_test.check_mapping(lru)
    utils_test.check_closing(lru)

    lru.clear()
    assert not lru.d
    assert not lru.weights
    assert not lru.total_weight
    assert not lru._cancel_evict
    assert not lru.evict_exceptions


def test_overwrite():
    d = {}
    lru = LRU(2, d)

    lru["x"] = 1
    lru["y"] = 2
    lru["y"] = 3

    assert set(lru) == {"x", "y"}

    lru.update({"y": 4})

    assert set(lru) == {"x", "y"}


def test_callbacks():
    count = [0]

    def cb(k, v):
        count[0] += 1

    L = []
    d = {}
    lru = LRU(2, d, on_evict=[lambda k, v: L.append((k, v)), cb])

    lru["x"] = 1
    lru["y"] = 2
    lru["z"] = 3

    assert L == [("x", 1)]
    assert count[0] == len(L)


def test_cb_exception_keep_on_lru():
    class MyError(Exception):
        pass

    def cb(k, v):
        raise MyError()

    a = []
    b = []
    d = {}
    lru = LRU(
        2,
        d,
        on_evict=[
            lambda k, v: a.append((k, v)),
            cb,
            lambda k, v: b.append((k, v)),
        ],
    )

    lru["x"] = 1
    lru["y"] = 2

    # No exception escapes: every failing key is rolled back into the LRU and recorded
    lru["z"] = 3

    # exception was raised in a later callback, once per candidate
    assert a == [("x", 1), ("y", 2), ("z", 3)]
    # no callback after the failing one ever ran
    assert b == []
    assert lru.total_weight == 3
    assert lru.weights == {"x": 1, "y": 1, "z": 1}

    assert set(lru) == {"x", "y", "z"}

    assert lru.d == {"x": 1, "y": 2, "z": 3}
    assert list(lru.order) == ["x", "y", "z"]

    # The eviction result is explainable: every failing key maps to its exception
    assert set(lru.evict_exceptions) == {"x", "y", "z"}
    assert all(isinstance(exc, MyError) for exc in lru.evict_exceptions.values())

    # Failing keys are skipped by automatic eviction
    lru.evict_until_below_target()
    assert set(lru) == {"x", "y", "z"}
    assert a == [("x", 1), ("y", 2), ("z", 3)]

    # Explicitly evicting a failing key retries it and re-raises
    with pytest.raises(MyError):
        lru.evict("x")
    assert set(lru) == {"x", "y", "z"}
    assert isinstance(lru.evict_exceptions["x"], MyError)

    # Overwriting a failing key with a new value clears its record and makes it
    # eligible for eviction again
    lru.set_noevict("x", 1)
    assert "x" not in lru.evict_exceptions
    assert set(lru.evict_exceptions) == {"y", "z"}

    def cb_ok(k, v):
        pass

    # With a working callback and every key replaced, nothing is blocked anymore
    lru.on_evict = [cb_ok]
    lru.set_noevict("y", 2)
    lru.set_noevict("z", 3)
    assert not lru.evict_exceptions
    lru.evict_until_below_target(0)
    assert not lru
    assert lru.total_weight == 0


def test_cb_exception_keep_on_lru_weights():
    class MyError(Exception):
        pass

    def cb(k, v):
        if v >= 3:
            raise MyError()

    a = []
    b = []
    d = {}
    lru = LRU(
        2,
        d,
        on_evict=[
            lambda k, v: a.append((k, v)),
            cb,
            lambda k, v: b.append((k, v)),
        ],
        weight=lambda k, v: v,
    )

    lru["x"] = 1

    # value is individually heavier than n; the exception is recorded, not raised
    lru["y"] = 3

    # tried to evict y and failed, then evicted x and succeeded
    assert a == [("y", 3), ("x", 1)]
    assert b == [("x", 1)]
    assert lru.total_weight == 3
    assert set(lru) == {"y"}

    assert lru.d == {"y": 3}
    assert list(lru.order) == ["y"]
    assert set(lru.evict_exceptions) == {"y"}
    assert isinstance(lru.evict_exceptions["y"], MyError)

    # value is individually heavier than n
    lru["z"] = 4

    # z fails and is recorded; y is not retried, as it already failed and hasn't been
    # overwritten
    assert a == [("y", 3), ("x", 1), ("z", 4)]
    # tried to evict and raised exception
    assert b == [("x", 1)]
    assert lru.total_weight == 7
    assert set(lru) == {"y", "z"}

    assert lru.d == {"y": 3, "z": 4}
    assert list(lru.order) == ["y", "z"]
    assert set(lru.evict_exceptions) == {"y", "z"}
    assert all(isinstance(exc, MyError) for exc in lru.evict_exceptions.values())

    # Deleting a failing key clears its record
    del lru["y"]
    assert set(lru.evict_exceptions) == {"z"}

    # Overwriting a failing key clears its record and re-enables eviction
    lru["z"] = 1
    assert not lru.evict_exceptions
    assert lru.d == {"z": 1}
    assert lru.total_weight == 1


def test_evict_failure_skips_and_retries_other_candidates():
    """A failing candidate is rolled back and recorded; eviction keeps going on the
    other keys, and a replaced value is re-evaluated afterwards.
    """
    class MyError(Exception):
        pass

    evicted = []

    def cb(k, v):
        if k == "x":
            raise MyError(k)
        evicted.append(k)

    lru = LRU(
        10,
        {},
        on_evict=cb,
        weight=lambda k, v: v,
    )
    # Oldest on the left; two large weights, then a small one
    lru.set_noevict("x", 6)  # oldest, will fail
    lru.set_noevict("y", 6)
    lru.set_noevict("z", 1)  # newest
    assert list(lru.order) == ["x", "y", "z"]

    lru.evict_until_below_target(0)

    # x failed and stayed in the LRU; y and z were evicted instead
    assert set(lru.d) == {"x"}
    assert evicted == ["y", "z"]
    assert set(lru.evict_exceptions) == {"x"}
    assert isinstance(lru.evict_exceptions["x"], MyError)
    assert lru.evict_exceptions["x"].args == ("x",)
    assert lru.total_weight == 6

    # Failing key is skipped: nothing more can be evicted automatically
    lru.evict_until_below_target(0)
    assert set(lru.d) == {"x"}

    # Explicit eviction retries and re-raises
    with pytest.raises(MyError):
        lru.evict("x")
    assert set(lru.d) == {"x"}

    # Replace the bad value with a serializable one: the key is re-evaluated
    lru.set_noevict("x", 6)
    lru.on_evict = [lambda k, v: evicted.append(k)]
    assert not lru.evict_exceptions
    lru.evict_until_below_target(0)
    assert not lru.d
    assert evicted == ["y", "z", "x"]
    assert not lru.evict_exceptions


def test_evict_failure_heavy_key_does_not_block_lighter_keys():
    class MyError(Exception):
        pass

    evicted = []

    def cb(k, v):
        if v >= 10:
            raise MyError(k)
        evicted.append(k)

    lru = LRU(5, {}, on_evict=cb, weight=lambda k, v: v)
    lru.set_noevict("x", 1)
    lru.set_noevict("y", 2)
    # z is a heavy key, so it would normally be evicted first
    lru.set_noevict("z", 10)
    assert list(lru.heavy) == ["z"]
    assert list(lru.order) == ["x", "y", "z"]

    lru.evict_until_below_target()

    # Heavy z fails and gets out of the heavy set and behind x and y, which are then
    # evicted successfully; the total still exceeds n, but no evictable key is left
    assert set(lru.d) == {"z"}
    assert evicted == ["x", "y"]
    assert set(lru.evict_exceptions) == {"z"}
    assert not lru.heavy
    assert list(lru.order) == ["z"]


def test_weight():
    d = {}
    weight = lambda k, v: v
    lru = LRU(10, d, weight=weight)

    lru["x"] = 5
    assert lru.total_weight == 5

    lru["y"] = 4
    assert lru.total_weight == 9

    lru["z"] = 3
    assert d == {"y": 4, "z": 3}
    assert lru.total_weight == 7

    del lru["z"]
    assert lru.total_weight == 4

    lru["a"] = 10000
    assert "a" not in lru
    assert d == {"y": 4}


def test_manual_eviction():
    a = []
    lru = LRU(100, {}, weight=lambda k, v: v, on_evict=lambda k, v: a.append(k))
    lru.set_noevict("x", 70)
    lru.set_noevict("y", 50)
    lru.set_noevict("z", 110)
    assert lru.total_weight == 70 + 50 + 110
    assert lru.heavy == {"z"}
    assert list(lru.order) == ["x", "y", "z"]
    assert a == []

    lru.evict_until_below_target()
    assert dict(lru) == {"y": 50}
    assert a == ["z", "x"]
    assert lru.weights == {"y": 50}
    assert lru.order == {"y"}
    assert not lru.heavy

    lru.evict_until_below_target()  # No-op
    assert dict(lru) == {"y": 50}
    lru.evict_until_below_target(50)  # Custom target
    assert dict(lru) == {"y": 50}
    lru.evict_until_below_target(0)  # 0 != None
    assert not lru
    assert not lru.order
    assert not lru.weights
    assert a == ["z", "x", "y"]


def test_explicit_evict():
    d = {}
    lru = LRU(10, d)

    lru["x"] = 1
    lru["y"] = 2
    lru["z"] = 3

    assert set(d) == {"x", "y", "z"}

    assert lru.evict() == ("x", 1, 1)
    assert set(d) == {"y", "z"}
    assert lru.evict("z") == ("z", 3, 1)
    assert set(d) == {"y"}
    assert lru.evict() == ("y", 2, 1)
    with pytest.raises(KeyError, match=r"'evict\(\): dictionary is empty'"):
        lru.evict()

    # evict() with explicit key
    lru["v"] = 4
    lru["w"] = 5
    assert lru.evict("w") == ("w", 5, 1)
    with pytest.raises(KeyError, match="notexist"):
        lru.evict("notexist")


def test_init_not_empty():
    lru1 = LRU(100, {}, weight=lambda k, v: v * 2)
    lru1.set_noevict(1, 10)
    lru1.set_noevict(2, 20)
    lru1.set_noevict(3, 30)
    lru1.set_noevict(4, 60)
    lru2 = LRU(100, {1: 10, 2: 20, 3: 30, 4: 60}, weight=lambda k, v: v * 2)
    assert lru1.d == lru2.d == {1: 10, 2: 20, 3: 30, 4: 60}
    assert lru1.weights == lru2.weights == {1: 20, 2: 40, 3: 60, 4: 120}
    assert lru1.total_weight == lru2.total_weight == 240
    assert list(lru1.order) == list(lru2.order) == [1, 2, 3, 4]
    assert list(lru1.heavy) == list(lru2.heavy) == [4]


def test_get_all_or_nothing():
    lru = LRU(100, {"x": 1, "y": 2, "z": 3})
    assert list(lru.order) == ["x", "y", "z"]
    with pytest.raises(KeyError, match="w"):
        lru.get_all_or_nothing(["x", "w", "y"])
    assert list(lru.order) == ["x", "y", "z"]
    assert lru.get_all_or_nothing(["y", "x"]) == {"y": 2, "x": 1}
    assert list(lru.order) == ["z", "y", "x"]


def test_close_aborts_eviction():
    evicted = []

    def cb(k, v):
        evicted.append(k)
        if len(evicted) == 3:
            lru.close()

    lru = LRU(100, {}, weight=lambda k, v: v, on_evict=cb)
    lru["a"] = 20
    lru["b"] = 20
    lru["c"] = 20
    lru["d"] = 20
    lru["e"] = 90  # Trigger eviction of a, b, c, d

    assert lru.closed
    assert evicted == ["a", "b", "c"]
    assert dict(lru) == {"d": 20, "e": 90}


def test_flush_close():
    flushed = 0
    closed = False

    class D(utils_test.SimpleDict):
        def flush(self):
            nonlocal flushed
            flushed += 1

        def close(self):
            nonlocal closed
            closed = True

    with LRU(10, D()) as lru:
        lru.flush()

    assert flushed == 1
    assert closed


def test_update_n():
    evicted = []
    z = LRU(10, {}, on_evict=lambda k, v: evicted.append(k), weight=lambda k, v: v)
    z["x"] = 5
    assert not evicted

    # Update n. This also changes what keys are considered heavy
    # (but there isn't a full scan on the weights for already existing keys)
    z.n = 3
    assert not evicted
    assert not z.heavy
    z["y"] = 1
    assert evicted == ["x"]
    z["z"] = 4
    assert evicted == ["x", "z"]


def test_update_offset():
    evicted = []
    z = LRU(5, {}, on_evict=lambda k, v: evicted.append(k), weight=lambda k, v: v)

    z.offset = 2
    z["x"] = 1
    # y would be a heavy key if we had reduced n by 2 instead of increasing offset
    z["y"] = 2.5
    assert evicted == ["x"]
    z["z"] = 5.5  # Still heavy according to n alone
    assert evicted == ["x", "z"]


@pytest.mark.parametrize("event", ("set", "set_noevict", "del"))
def test_cancel_evict(event):
    """See also:

    test_buffer.py::test_cancel_evict
    test_buffer.py::test_cancel_restore
    """
    ev1 = threading.Event()
    ev2 = threading.Event()
    log = []

    def cb(k, v):
        ev1.set()
        assert ev2.wait(timeout=5)

    def cancel_cb(k, v):
        log.append((k, v))

    lru = LRU(100, {}, on_evict=cb, on_cancel_evict=cancel_cb, weight=lambda k, v: v)
    lru.set_noevict("x", 1)
    with ThreadPoolExecutor(1) as ex:
        fut = ex.submit(lru.evict)
        assert ev1.wait(timeout=5)
        # cb is running

        assert lru.evict() == (None, None, 0)
        if event == "set":
            lru["x"] = 2
        elif event == "set_noevict":
            lru.set_noevict("x", 2)
        else:
            assert event == "del"
            del lru["x"]

        ev2.set()
        assert fut.result() == (None, None, 0)

    assert log == [("x", 1)]
    if event in ("set", "set_noevict"):
        assert lru.d == {"x": 2}
        assert lru.weights == {"x": 2}
        assert list(lru.order) == ["x"]
    else:
        assert not lru.d
        assert not lru.weights
        assert not lru.order

    assert not lru._cancel_evict


def slow_cb(k, v):
    time.sleep(0.01)


@pytest.mark.stress
@pytest.mark.repeat(utils_test.REPEAT_STRESS_TESTS)
def test_stress_different_keys_threadsafe():
    # Sometimes x and y can cohexist without triggering eviction
    # Sometimes x and y are individually <n but when they're both in they cause eviction
    # Sometimes x or y are heavy
    lru = LRU(
        1,
        {},
        weight=lambda k, v: random.choice([0.4, 0.9, 1.1]),
        on_evict=slow_cb,
        on_cancel_evict=slow_cb,
    )
    utils_test.check_different_keys_threadsafe(lru, allow_keyerror=True)
    lru.n = 100
    utils_test.check_mapping(lru)


@pytest.mark.stress
@pytest.mark.repeat(utils_test.REPEAT_STRESS_TESTS)
def test_stress_same_key_threadsafe():
    # Sometimes x is heavy
    lru = LRU(
        1,
        {},
        weight=lambda k, v: random.choice([0.9, 1.1]),
        on_evict=slow_cb,
        on_cancel_evict=slow_cb,
    )

    utils_test.check_same_key_threadsafe(lru)
    lru.n = 100
    utils_test.check_mapping(lru)
