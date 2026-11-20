"""
Lightweight observation of local scheduler execution.

This module provides :class:`ExecutionObserver`, a small callback-based
diagnostic that complements :class:`dask.diagnostics.Profiler`.  Where the
``Profiler`` records how long each *task* ran, the ``ExecutionObserver``
records how long each *intermediate result* lived in the scheduler cache and
which downstream tasks were keeping it alive -- the information needed when
analyzing the resident memory of a multi-output computation.
"""
from __future__ import annotations

from collections import namedtuple
from timeit import default_timer

from dask.callbacks import Callback

# Records the final release of a cached value: ``key`` was dropped from the
# scheduler cache because the task ``released_by`` -- its last remaining
# dependent -- finished at ``time`` (seconds since the epoch).
ReleaseData = namedtuple("ReleaseData", ("key", "released_by", "time"))

# Records a cache hit: the value of ``key`` was already in the scheduler cache
# when execution started and was reused by ``consumer`` (a task key, or None
# if ``key`` was itself a requested output) instead of being recomputed.
CacheHit = namedtuple("CacheHit", ("key", "consumer", "time"))

class ExecutionObserver(Callback):
    """A lightweight observer of local scheduler execution.

    Records the following information about a computation:

        1. ``requested_outputs``: the outputs that were requested from the
           scheduler and are still held in the cache when the computation
           completes successfully.
        2. ``cache_hits``: values that were already in the scheduler cache
           when execution started and were reused by a task (or requested as
           an output) instead of being recomputed.
        3. ``releases``: the last release of every cached dependency,
           including which finished task triggered it.  A key is released
           exactly once -- when its last remaining downstream consumer
           finishes -- so this pinpoints when a shared intermediate stopped
           being used by *any* branch of the graph.
        4. ``cached_on_failure``: the keys still sitting in the cache if the
           computation fails.

    Only the standard scheduler callbacks are used, so this works with every
    local scheduler (synchronous, threaded, and multiprocessing) and imposes
    no changes on user code or custom callbacks.

    Examples
    --------

    >>> from operator import add, mul
    >>> from dask.threaded import get
    >>> from dask.diagnostics import ExecutionObserver
    >>> dsk = {'x': 1, 'y': (add, 'x', 10), 'z': (mul, 'y', 2)}
    >>> with ExecutionObserver() as obs:
    ...     get(dsk, 'z')
    22

    >>> obs.requested_outputs
    {'z'}
    >>> obs.cache_hits        # doctest: +SKIP
    [CacheHit(key='x', consumer='y', time=...)]
    >>> obs.releases          # doctest: +SKIP
    [ReleaseData(key='x', released_by='y', time=...),
     ReleaseData(key='y', released_by='z', time=...)]

    When one input feeds several branches of different lengths, the release
    log shows when the input is no longer used by *any* branch:

    >>> inc = lambda x: x + 1
    >>> dsk = {'x': 1, 'short': (inc, 'x'),
    ...        'long-1': (inc, 'x'), 'long-2': (inc, 'long-1')}
    >>> with ExecutionObserver() as obs:
    ...     get(dsk, ['short', 'long-2'])
    (2, 3)
    >>> [r for r in obs.releases if r.key == 'x']   # doctest: +SKIP
    [ReleaseData(key='x', released_by='long-1', time=...)]

    ``x`` is only released once both ``short`` and ``long-1`` -- its last
    remaining consumer -- have finished.

    You can activate the observer globally

    >>> obs.register()      # doctest: +SKIP

    If you use the observer globally you will need to clear out old results
    manually.

    >>> obs.clear()         # doctest: +SKIP
    >>> obs.unregister()    # doctest: +SKIP

    See Also
    --------
    dask.diagnostics.CacheProfiler
    """

    def __init__(self):
        self.clear()

    def __enter__(self):
        self.clear()
        self.start_time = default_timer()
        return super().__enter__()

    def __exit__(self, *args):
        self.end_time = default_timer()
        return super().__exit__(*args)

    def _start(self, dsk):
        self._dsk.update(dsk)

    def _start_state(self, dsk, state):
        # Values already in the cache before any task runs.  Anything
        # reachable from the requested outputs that shows up here is served
        # from the cache instead of being recomputed.
        self._precached = set(state["cache"])
        self._seen_released = set(state["released"])

    def _pretask(self, key, dsk, state):
        t = default_timer()
        for dep in state["dependencies"][key]:
            if dep in self._precached:
                # Each task runs exactly once, so each (dep, consumer) pair
                # is recorded exactly once.
                self.cache_hits.append(CacheHit(dep, key, t))

    def _posttask(self, key, result, dsk, state, worker_id):
        t = default_timer()
        self.task_end_times[key] = t
        # ``finish_task`` runs before this callback, so any key that shows up
        # in ``released`` now was dropped from the cache because ``key`` --
        # its last remaining dependent -- just finished.
        newly_released = state["released"] - self._seen_released
        for k in newly_released:
            self.releases.append(ReleaseData(k, key, t))
        self._seen_released |= newly_released

    def _finish(self, dsk, state, failed):
        t = default_timer()
        self.failed = failed
        cache = state.get("cache", ())
        if failed:
            # Keys whose consumers never finished are still resident here.
            self.cached_on_failure = set(cache)
        else:
            # Keys that were visited while walking the graph from the
            # requested outputs and were never released can only be the
            # requested outputs themselves; every other computed or cached
            # key is released once its last dependent finishes.
            dependencies = state.get("dependencies", {})
            self.requested_outputs = {k for k in cache if k in dependencies}
            for k in self.requested_outputs & self._precached:
                self.cache_hits.append(CacheHit(k, None, t))

    def clear(self):
        """Clear out old results from the observer"""
        self.requested_outputs = set()
        self.cache_hits = []
        self.releases = []
        self.cached_on_failure = set()
        self.task_end_times = {}
        self.failed = False
        self._precached = set()
        self._seen_released = set()
        self._dsk = {}
        self.start_time = None
        self.end_time = None
