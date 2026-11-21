"""
Asynchronous Shared-Memory Scheduler for Dask Graphs.

This scheduler coordinates several workers to execute tasks in a dask graph in
parallel.  It depends on a ``concurrent.futures.Executor``
and a corresponding Queue for worker-to-scheduler communication.

It tries to execute tasks in an order which maintains a small memory footprint
throughout execution.  It does this by running tasks that allow us to release
data resources.


Task Selection Policy
=====================

When we complete a task we add more data in to our set of available data; this
new data makes new tasks available.  We preferentially choose tasks that were
just made available in a last-in-first-out fashion.  We implement this as a
simple stack.  This results in more depth-first rather than breadth first
behavior which encourages us to process batches of data to completion before
starting in on new data when possible.

When the addition of new data readies multiple tasks simultaneously we add
tasks to the stack in sorted order so that tasks with greater keynames are run
first.  This can be handy to break ties in a predictable fashion.


State
=====

Many functions pass around a ``state`` variable that holds the current state of
the computation.  This variable consists of several other dictionaries and
sets, explained below.

Constant state
--------------

1.  dependencies: {x: [a, b ,c]} a,b,c, must be run before x
2.  dependents: {a: [x, y]} a must run before x or y

Changing state
--------------

### Data

1.  cache: available concrete data.  {key: actual-data}
2.  released: data that we've seen, used, and released because it is no longer
    needed

### Jobs

1.  ready: A fifo stack of ready-to-run tasks
2.  running: A set of tasks currently in execution
3.  finished: A set of finished tasks
4.  waiting: which tasks are still waiting on others :: {key: {keys}}
    Real-time equivalent of dependencies
5.  waiting_data: available data to yet-to-be-run-tasks :: {key: {keys}}
    Real-time equivalent of dependents


Examples
--------

>>> import pprint  # doctest: +SKIP
>>> inc = lambda x: x + 1
>>> add = lambda x, y: x + y
>>> dsk = {'x': 1, 'y': 2, 'z': (inc, 'x'), 'w': (add, 'z', 'y')}  # doctest: +SKIP
>>> pprint.pprint(start_state_from_dask(dsk))  # doctest: +SKIP
{'cache': {'x': 1, 'y': 2},
 'dependencies': {'w': {'z', 'y'}, 'x': set(), 'y': set(), 'z': {'x'}},
 'dependents': defaultdict(None, {'w': set(), 'x': {'z'}, 'y': {'w'}, 'z': {'w'}}),
 'finished': set(),
 'ready': ['z'],
 'released': set(),
 'running': set(),
 'waiting': {'w': {'z'}},
 'waiting_data': {'x': {'z'}, 'y': {'w'}, 'z': {'w'}}}

Optimizations
=============

We build this scheduler with out-of-core array operations in mind.  To this end
we have encoded some particular optimizations.

Compute to release data
-----------------------

When we choose a new task to execute we often have many options.  Policies at
this stage are cheap and can significantly impact performance.  One could
imagine policies that expose parallelism, drive towards a particular output,
etc..

Our current policy is to run tasks that were most recently made available.


Inlining computations
---------------------

We hold on to intermediate computations either in memory or on disk.

For very cheap computations that may emit new copies of the data, like
``np.transpose`` or possibly even ``x + 1`` we choose not to store these as
separate pieces of data / tasks.  Instead we combine them with the computations
that require them.  This may result in repeated computation but saves
significantly on space and computation complexity.

See the function ``inline_functions`` for more information.
"""

from __future__ import annotations

import os
from collections import defaultdict
from collections.abc import Mapping, Sequence
from concurrent.futures import Executor, Future
from functools import partial
from queue import Empty, Queue

from dask import config
from dask._task_spec import DataNode, convert_legacy_graph
from dask.callbacks import local_callbacks, unpack_callbacks
from dask.core import flatten, get_dependencies
from dask.order import order
from dask.typing import Key

if os.name == "nt":
    # Python 3 windows Queue.get doesn't handle interrupts properly. To
    # workaround this we poll at a sufficiently large interval that it
    # shouldn't affect performance, but small enough that users trying to kill
    # an application shouldn't care.
    def queue_get(q):
        while True:
            try:
                return q.get(block=True, timeout=0.1)
            except Empty:
                pass

else:

    def queue_get(q):
        return q.get()


def start_state_from_dask(dsk, cache=None, sortkey=None, keys=None):
    """Start state from a dask

    Examples
    --------
    >>> inc = lambda x: x + 1
    >>> add = lambda x, y: x + y
    >>> dsk = {'x': 1, 'y': 2, 'z': (inc, 'x'), 'w': (add, 'z', 'y')}  # doctest: +SKIP
    >>> from pprint import pprint  # doctest: +SKIP
    >>> pprint(start_state_from_dask(dsk))  # doctest: +SKIP
    {'cache': {'x': 1, 'y': 2},
     'dependencies': {'w': {'z', 'y'}, 'x': set(), 'y': set(), 'z': {'x'}},
     'dependents': defaultdict(None, {'w': set(), 'x': {'z'}, 'y': {'w'}, 'z': {'w'}}),
     'finished': set(),
     'ready': ['z'],
     'released': set(),
     'running': set(),
     'waiting': {'w': {'z'}},
     'waiting_data': {'x': {'z'}, 'y': {'w'}, 'z': {'w'}}}
    """
    if sortkey is None:
        sortkey = order(dsk).get
    if cache is None:
        cache = config.get("cache", None)
    if cache is None:
        cache = dict()
    if keys is None:
        keys = list(set(dsk) - set(cache))
    dsk = convert_legacy_graph(dsk, all_keys=set(dsk) | set(cache))
    stack = list(keys)
    dependencies = defaultdict(set)
    dependents = defaultdict(set)
    waiting = defaultdict(set)
    waiting_data = defaultdict(set)
    ready_set = set()
    seen = set()
    while stack:
        key = stack.pop()
        if key in seen:
            continue
        seen.add(key)
        dependents[key]
        waiting_data[key]
        dependencies[key]
        task = dsk.get(key, None)
        if task is None:
            if dependents[key] and not cache.get(key):
                raise ValueError(
                    f"Missing dependency {key} for dependents {dependents[key]}"
                )
            continue
        elif isinstance(task, DataNode):
            cache[key] = task()
            dependencies[key]
            for d in dependents[key]:
                if d in waiting:
                    waiting[d].remove(key)
                    if not waiting[d]:
                        del waiting[d]
                        ready_set.add(d)
                else:
                    ready_set.add(d)
        else:
            _wait = task.dependencies - set(cache)
            if not _wait:
                ready_set.add(key)
            else:
                waiting[key] = set(_wait)
            for dep in task.dependencies:
                dependencies[key].add(dep)
                dependents[dep].add(key)
                waiting_data[dep].add(key)
                stack.append(dep)

    ready = sorted(ready_set, key=sortkey, reverse=True)

    state = {
        "dependencies": dict(dependencies),
        "dependents": dict(dependents),
        "waiting": dict(waiting),
        "waiting_data": dict(waiting_data),
        "cache": cache,
        "ready": ready,
        "running": set(),
        "finished": set(),
        "released": set(),
    }

    return state


"""
Running tasks
-------------

When we execute tasks we both

1.  Perform the actual work of collecting the appropriate data and calling the function
2.  Manage administrative state to coordinate with the scheduler

Stage attribution
-----------------

A batch of tasks is dispatched to a worker in a single call when
``chunksize > 1`` (see ``get_async``).  An exception escaping such a batch, or
an exception that is not raised by the task function itself, would otherwise
only be attributable to the whole batch.  Every task therefore runs through
:func:`execute_task`, which records the stage that was executing when the
exception occurred and annotates the exception with:

``dask_stage``
    One of ``serialize-task``, ``deserialize-task``, ``execute``,
    ``serialize-result`` or ``deserialize-result`` (or ``batch`` when the
    failure can only be attributed to the batch as a whole).
``dask_task_key``
    The key of the task that failed, when it is known.
``dask_task_keys``
    The keys of the batch, if the failure could not be narrowed down to a
    single task.

The exception that reaches the user keeps its original type and traceback (in
the multiprocessing scheduler it is still wrapped in ``RemoteException``, whose
attribute access forwards to the original exception).  The exception chain is
stored in the exception's state before it is serialized and restored on the
scheduler side, since ``pickle`` only transfers ``__dict__`` and would otherwise
drop ``__cause__``/``__context__``.
"""

#: Stages a task can fail in.  These are attached to the exception raised by
#: the scheduler as the ``dask_stage`` attribute.
TASK_SERIALIZE_TASK = "serialize-task"
TASK_DESERIALIZE_TASK = "deserialize-task"
TASK_EXECUTE = "execute"
TASK_SERIALIZE_RESULT = "serialize-result"
TASK_DESERIALIZE_RESULT = "deserialize-result"
TASK_BATCH = "batch"

#: Attributes used to transfer the exception chain through serialization.
_CHAIN_ATTRS = {"dask_cause": "__cause__", "dask_context": "__context__"}


def _set_exception_attr(exc, name, value):
    try:
        setattr(exc, name, value)
    except (AttributeError, TypeError):  # pragma: no cover - exotic exceptions
        pass


def _add_exception_note(exc, note):
    # PEP 678 notes are rendered by the traceback module, which makes the
    # attribution visible to the user without altering ``str(exc)``.
    add_note = getattr(exc, "add_note", None)
    if add_note is not None:
        try:
            add_note(note)
        except Exception:  # pragma: no cover - exotic exceptions
            pass


def annotate_exception(exc, stage, key=None):
    """Annotate *exc* with the execution stage (and task key) that failed.

    Returns the exception so that it can be used inline, e.g.
    ``raise annotate_exception(err, TASK_EXECUTE, key)``.
    """
    if key is not None:
        _set_exception_attr(exc, "dask_task_key", key)
        _add_exception_note(exc, f"Task {key!r} failed during the {stage!r} stage")
    else:
        _add_exception_note(exc, f"Task failed during the {stage!r} stage")
    _set_exception_attr(exc, "dask_stage", stage)
    return exc


def preserve_exception_chain(exc):
    """Store ``__cause__``/``__context__`` in the exception state.

    ``pickle`` (and therefore most executors) only transfers ``__dict__``, so an
    explicit ``raise ... from ...`` chain is lost between the worker and the
    scheduler.  Call this right before serializing an exception, so that
    :func:`restore_exception_chain` can rebuild the chain on the other side.

    This is only needed when the exception actually crosses a process boundary;
    in-process schedulers raise the original exception object, chain included.
    """
    seen = set()
    stack = [exc]
    while stack:
        current = stack.pop()
        if id(current) in seen:
            continue
        seen.add(id(current))
        state = getattr(current, "__dict__", None)
        if not isinstance(state, dict):
            continue
        for src, dst in _CHAIN_ATTRS.items():
            linked = getattr(current, dst, None)
            if linked is not None and src not in state:
                try:
                    state[src] = linked
                    stack.append(linked)
                except Exception:  # pragma: no cover - exotic exceptions
                    pass
    return exc


def restore_exception_chain(exc):
    """Reverse of :func:`preserve_exception_chain`.

    Restores ``__cause__``/``__context__`` after deserialization so that the
    re-raised exception still shows the original chain.
    """
    state = getattr(exc, "__dict__", None)
    if not isinstance(state, dict):
        return exc
    for src, dst in _CHAIN_ATTRS.items():
        linked = state.pop(src, None)
        if linked is not None and getattr(exc, dst, None) is None:
            try:
                setattr(exc, dst, linked)
            except (AttributeError, TypeError):  # pragma: no cover
                pass
    return exc


def annotate_batch_exception(exc, keys):
    """Attribute an exception that escaped a whole batch of tasks to that batch.

    Exceptions raised by an individual task are annotated by
    :func:`execute_task` before they travel back to the scheduler.  Anything
    else - e.g. a worker process dying - only carries batch level information,
    so all keys of the batch are attached instead.
    """
    if getattr(exc, "dask_task_key", None) is not None:
        # Already pinpointed to a single task by execute_task
        return exc
    keys = tuple(keys)
    if keys:
        _set_exception_attr(exc, "dask_task_keys", keys)
        _set_exception_attr(exc, "dask_stage", TASK_BATCH)
        _add_exception_note(
            exc,
            f"Task batch failed as a whole, the failing task is one of {list(keys)!r}",
        )
    return exc


def execute_task(key, task_info, dumps, loads, get_id, pack_exception):
    """
    Compute task and handle all administration

    The stage that failed is annotated on the exception (see
    :func:`annotate_exception`) before it is passed to ``pack_exception``, so
    that the scheduler can tell "the task could not be sent", "the function
    raised" and "the result could not be unpickled" apart.

    See Also
    --------
    _execute_task : actually execute task
    """
    stage = TASK_DESERIALIZE_TASK
    try:
        task, data = loads(task_info)
        stage = TASK_EXECUTE
        result = task(data)
        stage = TASK_SERIALIZE_RESULT
        id = get_id()
        result = dumps((result, id))
        failed = False
    except BaseException as e:
        annotate_exception(e, stage, key)
        result = pack_exception(e, dumps)
        failed = True
    return key, result, failed


def batch_execute_tasks(it):
    """
    Batch computing of multiple tasks with `execute_task`
    """
    return [execute_task(*a) for a in it]


def release_data(key, state, delete=True):
    """Remove data from temporary storage

    See Also
    --------
    finish_task
    """
    if key in state["waiting_data"]:
        assert not state["waiting_data"][key]
        del state["waiting_data"][key]

    state["released"].add(key)

    if delete:
        del state["cache"][key]


def finish_task(
    dsk, key, state, results, sortkey, delete=True, release_data=release_data
):
    """
    Update execution state after a task finishes

    Mutates.  This should run atomically (with a lock).
    """
    for dep in sorted(state["dependents"][key], key=sortkey, reverse=True):
        s = state["waiting"][dep]
        s.remove(key)
        if not s:
            del state["waiting"][dep]
            state["ready"].append(dep)

    for dep in state["dependencies"][key]:
        if dep in state["waiting_data"]:
            s = state["waiting_data"][dep]
            s.remove(key)
            if not s and dep not in results:
                release_data(dep, state, delete=delete)
        elif delete and dep not in results:
            release_data(dep, state, delete=delete)

    state["finished"].add(key)
    state["running"].remove(key)

    return state


def nested_get(ind, coll):
    """Get nested index from collection

    Examples
    --------

    >>> nested_get(1, 'abc')
    'b'
    >>> nested_get([1, 0], 'abc')
    ('b', 'a')
    >>> nested_get([[1, 0], [0, 1]], 'abc')
    (('b', 'a'), ('a', 'b'))
    """
    if isinstance(ind, list):
        return tuple(nested_get(i, coll) for i in ind)
    else:
        return coll[ind]


def default_get_id():
    """Default get_id"""
    return None


def default_pack_exception(e, dumps):
    raise e


def reraise(exc, tb=None):
    if exc.__traceback__ is not tb:
        raise exc.with_traceback(tb)
    raise exc


def identity(x):
    """Identity function. Returns x.

    >>> identity(3)
    3
    """
    return x


"""
Task Selection
--------------

We often have a choice among many tasks to run next.  This choice is both
cheap and can significantly impact performance.

We currently select tasks that have recently been made ready.  We hope that
this first-in-first-out policy reduces memory footprint
"""

"""
`get`
-----

The main function of the scheduler.  Get is the main entry point.
"""


def get_async(
    submit,
    num_workers,
    dsk,
    result,
    cache=None,
    get_id=default_get_id,
    rerun_exceptions_locally=None,
    pack_exception=default_pack_exception,
    raise_exception=reraise,
    callbacks=None,
    dumps=identity,
    loads=identity,
    chunksize=None,
    **kwargs,
):
    """Asynchronous get function

    This is a general version of various asynchronous schedulers for dask.  It
    takes a ``concurrent.futures.Executor.submit`` function to form a more
    specific ``get`` method that walks through the dask array with parallel
    workers, avoiding repeat computation and minimizing memory use.

    Parameters
    ----------
    submit : function
        A ``concurrent.futures.Executor.submit`` function
    num_workers : int
        The number of workers that task submissions can be spread over
    dsk : dict
        A dask dictionary specifying a workflow
    result : key or list of keys
        Keys corresponding to desired data
    cache : dict-like, optional
        Temporary storage of results
    get_id : callable, optional
        Function to return the worker id, takes no arguments. Examples are
        `threading.current_thread` and `multiprocessing.current_process`.
    rerun_exceptions_locally : bool, optional
        Whether to rerun failing tasks in local process to enable debugging
        (False by default)
    pack_exception : callable, optional
        Function to take an exception and ``dumps`` method, and return a
        serialized tuple of ``(exception, traceback)`` to send back to the
        scheduler. Default is to just raise the exception.
    raise_exception : callable, optional
        Function that takes an exception and a traceback, and raises an error.
    callbacks : tuple or list of tuples, optional
        Callbacks are passed in as tuples of length 5. Multiple sets of
        callbacks may be passed in as a list of tuples. For more information,
        see the dask.diagnostics documentation.
    dumps: callable, optional
        Function to serialize task data and results to communicate between
        worker and parent.  Defaults to identity.
    loads: callable, optional
        Inverse function of `dumps`.  Defaults to identity.
    chunksize: int, optional
        Size of chunks to use when dispatching work. Defaults to 1.
        If -1, will be computed to evenly divide ready work across workers.

    See Also
    --------
    threaded.get
    """
    chunksize = chunksize or config.get("chunksize", 1)

    queue = Queue()

    if isinstance(result, list):
        result_flat = set(flatten(result))
    else:
        result_flat = {result}
    results = set(result_flat)

    if not isinstance(dsk, Mapping):
        dsk = dsk.__dask_graph__()
    dsk = convert_legacy_graph(dsk)
    with local_callbacks(callbacks) as callbacks:
        _, _, pretask_cbs, posttask_cbs, _ = unpack_callbacks(callbacks)
        started_cbs = []
        succeeded = False
        # if start_state_from_dask fails, we will have something
        # to pass to the final block.
        state = {}
        try:
            for cb in callbacks:
                if cb[0]:
                    cb[0](dsk)
                started_cbs.append(cb)

            keyorder = order(dsk)

            state = start_state_from_dask(
                dsk, keys=results, cache=cache, sortkey=keyorder.get
            )

            for _, start_state, _, _, _ in callbacks:
                if start_state:
                    start_state(dsk, state)

            if rerun_exceptions_locally is None:
                rerun_exceptions_locally = config.get("rerun_exceptions_locally", False)

            if state["waiting"] and not state["ready"]:
                raise ValueError("Found no accessible jobs in dask")

            # Maps an in-flight submission to the keys it contains, so that a
            # failure which is only attributable to the batch as a whole (e.g. a
            # worker process dying) can still name the tasks involved.
            pending_batches: dict[Future, tuple[Key, ...]] = {}

            def queue_batch_results(batch):
                fut = Future()
                fut.set_result(batch)
                queue.put(fut)

            def fire_tasks(chunksize):
                """Fire off a task to the thread pool"""
                # Determine chunksize and/or number of tasks to submit
                nready = len(state["ready"])
                if chunksize == -1:
                    ntasks = nready
                    chunksize = -(ntasks // -num_workers)
                else:
                    used_workers = -(len(state["running"]) // -chunksize)
                    avail_workers = max(num_workers - used_workers, 0)
                    ntasks = min(nready, chunksize * avail_workers)

                # Prep all ready tasks for submission. Tasks whose input cannot
                # be serialized are attributed here; they must not take down the
                # rest of the batch.
                args = []
                failed_args = []
                for _ in range(ntasks):
                    # Get the next task to compute (most recently added)
                    key = state["ready"].pop()
                    # Notify task is running
                    state["running"].add(key)
                    for f in pretask_cbs:
                        f(key, dsk, state)

                    # Prep args to send
                    data = {
                        dep: state["cache"][dep] for dep in state["dependencies"][key]
                    }
                    try:
                        task_info = dumps((dsk[key], data))
                    except BaseException as e:
                        annotate_exception(e, TASK_SERIALIZE_TASK, key)
                        failed_args.append((key, pack_exception(e, dumps), True))
                    else:
                        args.append(
                            (key, task_info, dumps, loads, get_id, pack_exception)
                        )

                # Batch submit
                for i in range(-(len(args) // -chunksize)):
                    each_args = args[i * chunksize : (i + 1) * chunksize]
                    if not each_args:
                        break
                    each_keys = tuple(a[0] for a in each_args)
                    try:
                        fut = submit(batch_execute_tasks, each_args)
                    except BaseException as e:
                        # The batch never made it to a worker at all
                        annotate_batch_exception(e, each_keys)
                        raise
                    pending_batches[fut] = each_keys
                    fut.add_done_callback(queue.put)

                if failed_args:
                    # Queued after the dispatchable work so that its results are
                    # handled first.
                    queue_batch_results(failed_args)

            # Main loop, wait on tasks to finish, insert new ones
            while state["waiting"] or state["ready"] or state["running"]:
                fire_tasks(chunksize)
                fut = queue_get(queue)
                batch_keys = pending_batches.pop(fut, ())
                try:
                    batch = fut.result()
                except BaseException as e:
                    # Nothing came back from the worker, so the failure can
                    # only be attributed to the batch as a whole.
                    annotate_batch_exception(e, batch_keys)
                    raise

                failure = None
                for key, res_info, failed in batch:
                    if failed:
                        try:
                            exc, tb = loads(res_info)
                        except BaseException as e:
                            # The packed exception itself is unreadable
                            annotate_exception(e, TASK_DESERIALIZE_TASK, key)
                            state["running"].discard(key)
                            if failure is None:
                                failure = (key, e, None, None)
                            continue
                        restore_exception_chain(exc)
                        state["running"].discard(key)
                        if failure is None:
                            # Snapshot the inputs now; later tasks in this batch
                            # may release them.
                            data = None
                            if rerun_exceptions_locally and tb is not None:
                                data = {
                                    dep: state["cache"][dep]
                                    for dep in get_dependencies(dsk, key)
                                }
                            failure = (key, exc, tb, data)
                        continue

                    try:
                        res, worker_id = loads(res_info)
                    except BaseException as e:
                        annotate_exception(e, TASK_DESERIALIZE_RESULT, key)
                        state["running"].discard(key)
                        if failure is None:
                            failure = (key, e, None, None)
                        continue

                    state["cache"][key] = res
                    finish_task(dsk, key, state, results, keyorder.get)
                    for f in posttask_cbs:
                        f(key, res, dsk, state, worker_id)

                if failure is not None:
                    # Every other task in the batch has been accounted for
                    # according to how it actually finished above, including
                    # releasing its dependencies, before raising.
                    key, exc, tb, data = failure
                    if tb is None:
                        raise exc
                    if data is not None:
                        dsk[key](data)  # Re-execute locally
                    raise_exception(exc, tb)

            succeeded = True

        finally:
            for _, _, _, _, finish in started_cbs:
                if finish:
                    finish(dsk, state, not succeeded)

    return nested_get(result, state["cache"])


""" Synchronous concrete version of get_async

Usually we supply a ``concurrent.futures.Executor``.  Here we provide a
sequential one.  This is useful for debugging and for code dominated by the
GIL
"""


class SynchronousExecutor(Executor):
    _max_workers = 1

    def submit(self, fn, *args, **kwargs):
        fut = Future()
        try:
            fut.set_result(fn(*args, **kwargs))
        except BaseException as e:
            fut.set_exception(e)
        return fut


synchronous_executor = SynchronousExecutor()


def get_sync(dsk: Mapping, keys: Sequence[Key] | Key, **kwargs):
    """A naive synchronous version of get_async

    Can be useful for debugging.
    """
    kwargs.pop("num_workers", None)  # if num_workers present, remove it
    return get_async(
        synchronous_executor.submit,
        synchronous_executor._max_workers,
        dsk,
        keys,
        **kwargs,
    )


""" Adaptor for ``multiprocessing.Pool`` instances

Usually we supply a ``concurrent.futures.Executor``.  Here we provide a wrapper
class for ``multiprocessing.Pool`` instances so we can treat them like
``concurrent.futures.Executor`` instances instead.

This is mainly useful for legacy use cases or users that prefer
``multiprocessing.Pool``.
"""


class MultiprocessingPoolExecutor(Executor):
    def __init__(self, pool):
        self.pool = pool
        self._max_workers = len(pool._pool)

    def submit(self, fn, *args, **kwargs):
        return submit_apply_async(self.pool.apply_async, fn, *args, **kwargs)


def submit_apply_async(apply_async, fn, *args, **kwargs):
    fut = Future()
    apply_async(fn, args, kwargs, fut.set_result, fut.set_exception)
    return fut


def get_apply_async(apply_async, num_workers, *args, **kwargs):
    return get_async(
        partial(submit_apply_async, apply_async), num_workers, *args, **kwargs
    )


def sortkey(item):
    """Sorting key function that is robust to different types

    Both strings and tuples are common key types in dask graphs.
    However In Python 3 one can not compare strings with tuples directly.
    This function maps many types to a form where they can be compared

    Examples
    --------
    >>> sortkey('Hello')
    ('str', 'Hello')

    >>> sortkey(('x', 1))
    ('tuple', ('x', 1))
    """
    return (type(item).__name__, item)
