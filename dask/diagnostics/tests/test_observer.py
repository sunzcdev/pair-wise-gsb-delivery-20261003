from __future__ import annotations

import pytest

from dask._task_spec import Task, TaskRef
from dask.diagnostics import ExecutionObserver
from dask.local import get_sync
from dask.threaded import get


def inc(x):
    return x + 1


# One input ``x`` feeds both a short branch (``short``) and a long branch
# (``long-1`` -> ``long-2`` -> ``long-3``); both branch tips are requested.
dsk_shared = {
    "x": 1,
    "short": (inc, "x"),
    "long-1": (inc, "x"),
    "long-2": (inc, "long-1"),
    "long-3": (inc, "long-2"),
}


def test_shared_input_long_and_short_branches():
    with ExecutionObserver() as obs:
        out = get_sync(dsk_shared, ["short", "long-3"])
    assert out == (2, 4)

    # The requested outputs are exactly the two branch tips
    assert obs.requested_outputs == {"short", "long-3"}

    # The shared input was served from the cache to *both* branches
    consumers = {h.consumer for h in obs.cache_hits if h.key == "x"}
    assert consumers == {"short", "long-1"}

    # Requested outputs are held, never released mid-computation
    released = {r.key for r in obs.releases}
    assert "short" not in released
    assert "long-3" not in released
    assert {"x", "long-1", "long-2"} <= released

    # ``x`` is released exactly once, and only after *both* branches are
    # done with it: the release is triggered by whichever of its two
    # consumers finishes last.
    (release,) = [r for r in obs.releases if r.key == "x"]
    assert release.released_by in ("short", "long-1")
    assert release.time >= obs.task_end_times["short"]
    assert release.time >= obs.task_end_times["long-1"]
    last_consumer = max(("short", "long-1"), key=obs.task_end_times.get)
    assert release.released_by == last_consumer

    # Each long-branch intermediate is released by its own consumer
    (release,) = [r for r in obs.releases if r.key == "long-1"]
    assert release.released_by == "long-2"
    (release,) = [r for r in obs.releases if r.key == "long-2"]
    assert release.released_by == "long-3"

    assert not obs.failed
    assert obs.cached_on_failure == set()


def test_threaded_scheduler():
    with ExecutionObserver() as obs:
        out = get(dsk_shared, ["short", "long-3"])
    assert out == (2, 4)
    assert obs.requested_outputs == {"short", "long-3"}
    (release,) = [r for r in obs.releases if r.key == "x"]
    assert release.released_by in ("short", "long-1")
    assert release.time >= obs.task_end_times["short"]
    assert release.time >= obs.task_end_times["long-1"]


def test_cache_hits_from_precached_values():
    dsk = {
        "y": Task("y", inc, TaskRef("x")),
        "z": Task("z", inc, TaskRef("y")),
    }
    cache = {"x": 1}
    with ExecutionObserver() as obs:
        assert get_sync(dsk, "z", cache=cache) == 3
    # ``x`` was reused from the provided cache instead of recomputed
    assert {(h.key, h.consumer) for h in obs.cache_hits} == {("x", "y")}
    assert obs.requested_outputs == {"z"}


def test_requested_output_already_cached():
    with ExecutionObserver() as obs:
        assert get_sync({}, "x", cache={"x": 1}) == 1
    assert obs.requested_outputs == {"x"}
    # No task ran at all; the output itself was a cache hit
    assert {(h.key, h.consumer) for h in obs.cache_hits} == {("x", None)}
    assert obs.releases == []


def test_cached_keys_on_failure():
    def div(x, y):
        return x / y

    dsk = {"x": 1, "y": (inc, "x"), "z": (div, "y", 0)}
    with ExecutionObserver() as obs:
        with pytest.raises(ZeroDivisionError):
            get_sync(dsk, "z")

    assert obs.failed
    # ``y`` was computed but its only consumer ``z`` failed, so it is still
    # sitting in the cache; ``x`` was released as soon as ``y`` finished.
    assert obs.cached_on_failure == {"y"}
    assert any(r.key == "x" and r.released_by == "y" for r in obs.releases)
    assert obs.requested_outputs == set()


def test_clear():
    obs = ExecutionObserver()
    with obs:
        get_sync(dsk_shared, ["short", "long-3"])
    assert obs.requested_outputs
    obs.clear()
    assert obs.requested_outputs == set()
    assert obs.cache_hits == []
    assert obs.releases == []
    assert obs.cached_on_failure == set()
    assert obs.task_end_times == {}
