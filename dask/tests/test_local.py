from __future__ import annotations

import pickle

import pytest

import dask
from dask._task_spec import Task, TaskRef
from dask.local import (
    annotate_batch_exception,
    annotate_exception,
    batch_execute_tasks,
    finish_task,
    get_sync,
    preserve_exception_chain,
    restore_exception_chain,
    sortkey,
    start_state_from_dask,
)
from dask.order import order
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


def _pack(exc, dumps):
    """``pack_exception`` that serializes like the multiprocessing scheduler"""
    return dumps((exc, "traceback-body"))


def _raise_zero_division(data):
    return 1 / 0


def _return_corrupt(data):
    return "corrupt"


def _corrupting_loads(data):
    value = pickle.loads(data)
    if isinstance(value, tuple) and value and value[0] == "corrupt":
        raise ValueError("cannot decode result payload")
    return value


class TestStageAttribution:
    def _run(self, key, task_info, dumps=pickle.dumps, loads=pickle.loads):
        return batch_execute_tasks(
            [(key, task_info, dumps, loads, int, _pack)]
        )[0]

    def test_execute_stage(self):
        key, result, failed = self._run(
            "x", pickle.dumps((_raise_zero_division, {}))
        )
        assert failed
        exc, tb = pickle.loads(result)
        assert key == "x"
        assert isinstance(exc, ZeroDivisionError)
        assert exc.dask_stage == "execute"
        assert exc.dask_task_key == "x"

    def test_deserialize_task_stage(self):
        # The task payload is unreadable
        key, result, failed = self._run("x", b"not a pickle")
        assert failed
        exc, tb = pickle.loads(result)
        assert exc.dask_stage == "deserialize-task"
        assert exc.dask_task_key == "x"

    def test_deserialize_result_stage(self):
        # ``dumps`` worked but the payload cannot be read back on the
        # scheduler side (this happens in ``get_async``, not in the worker)
        with pytest.raises(ValueError, match="cannot decode result payload") as e:
            get_sync(
                {"x": (_return_corrupt, 1)},
                "x",
                dumps=pickle.dumps,
                loads=_corrupting_loads,
            )
        assert e.value.dask_stage == "deserialize-result"
        assert e.value.dask_task_key == "x"

    def test_annotate_exception_adds_note(self):
        exc = ValueError("boom")
        annotate_exception(exc, "execute", "x")
        assert exc.dask_stage == "execute"
        assert exc.dask_task_key == "x"
        assert any("'x'" in note and "'execute'" in note for note in exc.__notes__)

    def test_annotate_batch_exception(self):
        exc = ValueError("boom")
        annotate_batch_exception(exc, ["a", "b"])
        assert exc.dask_stage == "batch"
        assert exc.dask_task_keys == ("a", "b")
        assert not hasattr(exc, "dask_task_key")

    def test_annotate_batch_exception_keeps_task_attribution(self):
        exc = annotate_exception(ValueError("boom"), "execute", "a")
        annotate_batch_exception(exc, ["a", "b"])
        assert exc.dask_stage == "execute"
        assert exc.dask_task_key == "a"
        assert not hasattr(exc, "dask_task_keys")

    def test_exception_chain_survives_pickle(self):
        root = KeyError("root-cause")
        wrapper = RuntimeError("wrapped")
        wrapper.__cause__ = root

        preserve_exception_chain(wrapper)
        unpickled = pickle.loads(pickle.dumps(wrapper))
        # pickle drops ``__cause__`` but keeps the serialized reference
        assert unpickled.__cause__ is None
        restore_exception_chain(unpickled)
        assert isinstance(unpickled.__cause__, KeyError)
        assert "root-cause" in str(unpickled.__cause__)
        assert "dask_cause" not in unpickled.__dict__

    def test_sync_stage_attribution(self):
        """The same staging applies through the synchronous scheduler"""
        with pytest.raises(ZeroDivisionError) as e:
            get_sync({"x": (_raise_zero_division, 1)}, "x")
        assert e.value.dask_stage == "execute"
        assert e.value.dask_task_key == "x"
