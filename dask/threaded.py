"""
A threaded shared-memory scheduler

See local.py
"""

from __future__ import annotations

import atexit
import contextvars
import multiprocessing.pool
import sys
import threading
from collections import defaultdict
from collections.abc import Mapping, Sequence
from concurrent.futures import Executor, ThreadPoolExecutor
from threading import Lock, current_thread

from dask import config
from dask.local import MultiprocessingPoolExecutor, get_async
from dask.system import CPU_COUNT
from dask.typing import Key


def _thread_get_id():
    return current_thread().ident


main_thread = current_thread()
default_pool: Executor | None = None
pools: defaultdict[threading.Thread, dict[int, Executor]] = defaultdict(dict)
pools_lock = Lock()
# Number of computations currently running on each dask-created pool.
# Pools supplied by the caller are never tracked here.
_active_pools: dict[Executor, int] = {}


def pack_exception(e, dumps):
    return e, sys.exc_info()[2]


class ContextAwareThreadPoolExecutor(ThreadPoolExecutor):
    """Variant ThreadPoolExecutor that propagates contextvars
    from the submitting thread to the worker threads.

    With a vanilla ThreadPoolExecutor, contextvars are automatically propagated on
    CPython 3.14t (noGIL) if and only if they are set before the worker threads are
    started.
    This variant propagates contextvars on all Python interpreters and also when the
    worker threads are already warm when the variables are set in the submitting thread.

    This also affects catching warnings, which on 3.14t use contextvars.

    See Also
    --------
    https://docs.python.org/3/using/cmdline.html#envvar-PYTHON_THREAD_INHERIT_CONTEXT
    https://docs.python.org/3/using/cmdline.html#envvar-PYTHON_CONTEXT_AWARE_WARNINGS
    """

    def submit(self, fn, /, *args, **kwargs):
        ctx = contextvars.copy_context()
        return super().submit(ctx.run, fn, *args, **kwargs)


def get(
    dsk: Mapping,
    keys: Sequence[Key] | Key,
    cache=None,
    num_workers=None,
    pool=None,
    **kwargs,
):
    """Threaded cached implementation of dask.get

    Parameters
    ----------

    dsk: dict
        A dask dictionary specifying a workflow
    keys: key or list of keys
        Keys corresponding to desired data
    num_workers: integer of thread count
        The number of threads to use in the ThreadPool that will actually execute tasks
    cache: dict-like (optional)
        Temporary storage of results

    Examples
    --------
    >>> inc = lambda x: x + 1
    >>> add = lambda x, y: x + y
    >>> dsk = {'x': 1, 'y': 2, 'z': (inc, 'x'), 'w': (add, 'z', 'y')}
    >>> get(dsk, 'w')
    4
    >>> get(dsk, ['w', 'y'])
    (4, 2)
    """
    global default_pool
    pool = pool or config.get("pool", None)
    num_workers = num_workers or config.get("num_workers", None)
    thread = current_thread()
    dask_pool = False

    with pools_lock:
        if pool is None:
            dask_pool = True
            if num_workers is None and thread is main_thread:
                if default_pool is None:
                    default_pool = ContextAwareThreadPoolExecutor(CPU_COUNT)
                    atexit.register(default_pool.shutdown)
                pool = default_pool
            elif thread in pools and num_workers in pools[thread]:
                pool = pools[thread][num_workers]
            else:
                pool = ContextAwareThreadPoolExecutor(num_workers)
                atexit.register(pool.shutdown)
                pools[thread][num_workers] = pool
            _active_pools[pool] = _active_pools.get(pool, 0) + 1
        elif isinstance(pool, multiprocessing.pool.Pool):
            pool = MultiprocessingPoolExecutor(pool)

    try:
        results = get_async(
            pool.submit,
            pool._max_workers,
            dsk,
            keys,
            cache=cache,
            get_id=_thread_get_id,
            pack_exception=pack_exception,
            **kwargs,
        )
    finally:
        if dask_pool:
            with pools_lock:
                n = _active_pools[pool] - 1
                if n:
                    _active_pools[pool] = n
                else:
                    del _active_pools[pool]

    # Cleanup pools associated to dead threads
    with pools_lock:
        active_threads = set(threading.enumerate())
        if thread is not main_thread:
            for t in list(pools):
                if t not in active_threads:
                    for num_workers, p in list(pools[t].items()):
                        if p not in _active_pools:
                            p.shutdown()
                            del pools[t][num_workers]
                    if not pools[t]:
                        del pools[t]

    return results


def shutdown_pools():
    """Shut down idle thread pools created by dask.

    This shuts down the default pool and any pools created on behalf of
    short-lived threads, but only those that are not currently running a
    computation. Pools that are still in use keep running and are left
    untouched, as are any pools supplied by the caller through the ``pool``
    keyword argument or the ``pool`` config option; those keep their own
    lifecycle.

    New pools will be transparently recreated on subsequent calls to
    :func:`get` as needed.
    """
    global default_pool
    with pools_lock:
        if default_pool is not None and default_pool not in _active_pools:
            default_pool.shutdown()
            default_pool = None
        for thread in list(pools):
            for num_workers, pool in list(pools[thread].items()):
                if pool not in _active_pools:
                    pool.shutdown()
                    del pools[thread][num_workers]
            if not pools[thread]:
                del pools[thread]
