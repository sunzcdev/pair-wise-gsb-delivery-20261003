from __future__ import annotations

from collections import defaultdict

import pytest

import dask
from dask._task_spec import Task, TaskRef
from dask.local import finish_task, get_sync, sortkey, start_state_from_dask
from dask.order import order
from dask.threaded import get as get_threaded
from dask.utils_test import GetFunctionTestMixin, add, inc

fib_dask = {"f0": 0, "f1": 1, "f2": 1, "f3": 2, "f4": 3, "f5": 5, "f6": 8}


def test_start_state():
    dsk = {"x": 1, "y": 2, "z": (inc, "x"), "w": (add, "z", "y")}
    result = start_state_from_dask(dsk)

    expected = {
        "cache": {"x": 1, "y": 2},
        "dependencies": {
            "w": {"y", "z"},
            "x": set(),
            "y": set(),
            "z": {"x"},
        },
        "dependents": {"w": set(), "x": {"z"}, "y": {"w"}, "z": {"w"}},
        "finished": set(),
        "released": set(),
        "running": set(),
        "ready": ["z"],
        "waiting": {"w": {"z"}},
        "waiting_data": {"w": set(), "x": {"z"}, "y": {"w"}, "z": {"w"}},
    }
    assert result == expected


def test_start_state_looks_at_cache():
    dsk = {"b": (inc, "a")}
    cache = {"a": 1}
    result = start_state_from_dask(dsk, cache)
    assert result["dependencies"]["b"] == {"a"}
    assert result["ready"] == ["b"]


def test_start_state_with_redirects():
    dsk = {"x": 1, "y": "x", "z": (inc, "y")}
    result = start_state_from_dask(dsk)
    assert result["cache"] == {"x": 1}


def test_start_state_with_independent_but_runnable_tasks():
    assert start_state_from_dask({"x": (inc, 1)})["ready"] == ["x"]


def test_start_state_with_tasks_no_deps():
    dsk = {"a": [1, (inc, 2)], "b": [1, 2, 3, 4], "c": (inc, 3)}
    state = start_state_from_dask(dsk)
    assert list(state["cache"].keys()) == ["b"]
    assert "a" in state["ready"] and "c" in state["ready"]
    deps = {k: set() for k in "abc"}
    assert state["dependencies"] == deps
    assert state["dependents"] == deps


def test_finish_task():
    dsk = {"x": 1, "y": 2, "z": (inc, "x"), "w": (add, "z", "y")}
    sortkey = order(dsk).get
    state = start_state_from_dask(dsk)
    state["ready"].remove("z")
    state["running"] = {"z", "other-task"}
    task = "z"
    result = 2

    state["cache"]["z"] = result
    finish_task(dsk, task, state, set(), sortkey)

    assert state == {
        "cache": {"y": 2, "z": 2},
        "dependencies": {
            "w": {"y", "z"},
            "x": set(),
            "y": set(),
            "z": {"x"},
        },
        "finished": {"z"},
        "released": {"x"},
        "running": {"other-task"},
        "dependents": {"w": set(), "x": {"z"}, "y": {"w"}, "z": {"w"}},
        "ready": ["w"],
        "waiting": {},
        "waiting_data": {"w": set(), "y": {"w"}, "z": {"w"}},
    }


class TestGetAsync(GetFunctionTestMixin):
    get = staticmethod(get_sync)

    def test_get_sync_num_workers(self):
        self.get({"x": (inc, "y"), "y": 1}, "x", num_workers=2)


def test_cache_options():
    cache = {}

    def inc2(x):
        assert "y" in cache
        return x + 1

    with dask.config.set(cache=cache):
        get_sync({"x": (inc2, "y"), "y": 1}, "x")


def test_sort_key():
    L = ["x", ("x", 1), ("z", 0), ("x", 0)]
    assert sorted(L, key=sortkey) == ["x", ("x", 0), ("x", 1), ("z", 0)]


def test_callback():
    f = lambda x: x + 1
    dsk = {"a": (f, 1)}
    from dask.threaded import get

    def start_callback(key, d, state):
        assert key == "a" or key is None
        assert d == dsk
        assert isinstance(state, dict)

    def end_callback(key, value, d, state, worker_id):
        assert key == "a" or key is None
        assert value == 2 or value is None
        assert d == dsk
        assert isinstance(state, dict)

    get(dsk, "a", start_callback=start_callback, end_callback=end_callback)


def test_exceptions_propagate():
    class MyException(Exception):
        def __init__(self, a, b):
            self.a = a
            self.b = b

        def __str__(self):
            return "My Exception!"

    def f():
        raise MyException(1, 2)

    from dask.threaded import get

    try:
        get({"x": (f,)}, "x")
        assert False
    except MyException as e:
        assert "My Exception!" in str(e)
        assert "a" in dir(e)
        assert e.a == 1
        assert e.b == 2


def test_ordering():
    L = []

    def append(i):
        L.append(i)

    dsk = {("x", i): (append, i) for i in range(10)}
    x_keys = sorted(dsk)
    dsk["y"] = (lambda *args: None, list(x_keys))

    get_sync(dsk, "y")

    assert L == sorted(L, reverse=True)


def test_complex_ordering():
    pytest.importorskip("numpy")
    da = pytest.importorskip("dask.array")
    from dask.diagnostics import Callback

    actual_order = []

    def track_order(key, dask, state):
        actual_order.append(key)

    x = da.random.normal(size=(20, 20), chunks=(-1, -1))
    res = (x.dot(x.T) - x.mean(axis=0)).std()
    dsk = dict(res.__dask_graph__())
    exp_order_dict = order(dsk)
    exp_order = sorted(exp_order_dict.keys(), key=exp_order_dict.get)
    with Callback(pretask=track_order):
        get_sync(dsk, exp_order[-1])
    assert actual_order == exp_order


def test_ensure_calculate_only_whats_needed():
    counter = 0

    def only_once():
        nonlocal counter
        if counter > 0:
            raise RuntimeError("Should only be called once")
        counter += 1
        return 1

    def add(x, y):
        return x + y

    tasks = [
        Task("x1", only_once),
        (t1 := Task("x2", only_once)),
        Task("y", add, 1, t1.ref()),
    ]
    dsk = {t.key: t for t in tasks}
    assert get_sync(dsk, "y") == 2


def test_detect_malformed_graph():
    dsk = {"y": Task("y", lambda x: x, TaskRef("x"))}
    with pytest.raises(ValueError, match="Missing dependency"):
        get_sync(dsk, "y")
    assert get_sync(dsk, "y", cache={"x": 1}) == 1


class TransientReadError(OSError):
    """Simulates a temporary read error of a pure compute chunk."""


def _two_branch_graph(calls):
    """Two call-counted sibling branches joined by a sum."""

    def flaky_read():
        calls["flaky"] += 1
        if calls["flaky"] == 1:
            raise TransientReadError("temporary read error")
        return 1

    def stable_branch():
        calls["stable"] += 1
        return 2

    return {
        "flaky": Task("flaky", flaky_read),
        "stable": Task("stable", stable_branch),
        "total": Task("total", add, TaskRef("flaky"), TaskRef("stable")),
    }


@pytest.mark.parametrize("get", [get_sync, get_threaded], ids=["sync", "threads"])
def test_retry_only_marked_task(get):
    calls = defaultdict(int)
    dsk = _two_branch_graph(calls)
    assert get(dsk, "total", retry={"flaky"}, max_retries=1) == 3
    # Only the marked, flaky branch was re-executed
    assert calls["flaky"] == 2
    assert calls["stable"] == 1


@pytest.mark.parametrize("get", [get_sync, get_threaded], ids=["sync", "threads"])
def test_retry_disabled_by_default(get):
    calls = defaultdict(int)
    dsk = _two_branch_graph(calls)
    with pytest.raises(TransientReadError):
        get(dsk, "total")
    assert calls["flaky"] == 1


@pytest.mark.parametrize("get", [get_sync, get_threaded], ids=["sync", "threads"])
def test_retry_unmarked_task_propagates(get):
    calls = defaultdict(int)
    dsk = _two_branch_graph(calls)
    # Only the stable branch is marked retryable; the flaky one is not
    with pytest.raises(TransientReadError):
        get(dsk, "total", retry={"stable"}, max_retries=2)
    assert calls["flaky"] == 1


@pytest.mark.parametrize("get", [get_sync, get_threaded], ids=["sync", "threads"])
def test_retry_keeps_dependencies_and_delays_downstream(get):
    calls = defaultdict(int)

    def source():
        calls["source"] += 1
        return 40

    def flaky_inc(x):
        calls["flaky"] += 1
        if calls["flaky"] == 1:
            raise TransientReadError("temporary read error")
        return x + 1

    def downstream(x):
        calls["downstream"] += 1
        return x * 2

    dsk = {
        "source": Task("source", source),
        "flaky": Task("flaky", flaky_inc, TaskRef("source")),
        "out": Task("out", downstream, TaskRef("flaky")),
    }
    # The retry needs the "source" result again: it must not have been
    # released after the first failed attempt. The downstream task must only
    # run once the retry succeeded.
    assert get(dsk, "out", retry={"flaky"}, max_retries=2) == 82
    assert dict(calls) == {"source": 1, "flaky": 2, "downstream": 1}


@pytest.mark.parametrize("get", [get_sync, get_threaded], ids=["sync", "threads"])
def test_retry_exhausted_propagates(get):
    calls = defaultdict(int)

    def always_fails():
        calls["flaky"] += 1
        raise TransientReadError("temporary read error")

    dsk = {"flaky": Task("flaky", always_fails)}
    with pytest.raises(TransientReadError):
        get(dsk, "flaky", retry={"flaky"}, max_retries=2)
    assert calls["flaky"] == 3


@pytest.mark.parametrize("get", [get_sync, get_threaded], ids=["sync", "threads"])
def test_side_effect_task_propagates_without_retry(get):
    calls = defaultdict(int)

    def flaky_read():
        calls["flaky"] += 1
        if calls["flaky"] == 1:
            raise TransientReadError("temporary read error")
        return 1

    def side_effecting(x):
        calls["side-effect"] += 1
        raise TransientReadError("temporary read error")

    dsk = {
        "flaky": Task("flaky", flaky_read),
        # The side-effect task depends on the flaky one so that the retry of
        # "flaky" has completed deterministically before it runs
        "side-effect": Task(
            "side-effect", side_effecting, TaskRef("flaky"), _has_side_effects=True
        ),
    }
    # retry=True retries everything retryable, but the task declared as
    # having side effects propagates the same error directly
    with pytest.raises(TransientReadError):
        get(dsk, "side-effect", retry=True, max_retries=3)
    assert calls["side-effect"] == 1
    # The retryable sibling was still retried
    assert calls["flaky"] == 2
