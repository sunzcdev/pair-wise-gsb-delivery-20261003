import copy
import datetime
import inspect
import itertools
import logging
import re
import threading
import time
import traceback
import uuid
import warnings

from collections import deque
from collections import namedtuple
from functools import partial
from functools import wraps

from huey import signals as S
from huey.constants import EmptyData
from huey.consumer import Consumer
from huey.exceptions import CancelExecution
from huey.exceptions import ConfigurationError
from huey.exceptions import RateLimitExceeded
from huey.exceptions import RETRYING_EXCEPTIONS
from huey.exceptions import ResultExpired
from huey.exceptions import ResultPending
from huey.exceptions import ResultTimeout
from huey.exceptions import RetryTask
from huey.exceptions import TaskException
from huey.exceptions import TaskLockedException
from huey.exceptions import TaskTimeout
from huey.exceptions import WaitCancelled
from huey.registry import Registry
from huey.serializer import Serializer
from huey.storage import BlackHoleStorage
from huey.storage import CySqliteStorage
from huey.storage import FileStorage
from huey.storage import MemoryStorage
from huey.storage import PostgresStorage
from huey.storage import PriorityRedisExpireStorage
from huey.storage import PriorityRedisStorage
from huey.storage import RedisExpireStorage
from huey.storage import RedisStorage
from huey.storage import SqliteStorage
from huey.utils import ChordConfig
from huey.utils import Error
from huey.utils import SKIPPED
from huey.utils import noop_context
from huey.utils import normalize_expire_time
from huey.utils import normalize_time
from huey.utils import utcnow


logger = logging.getLogger('huey')
_sentinel = object()


class Huey(object):
    """
    Huey executes tasks by exposing function decorators that cause the function
    call to be enqueued for execution by a separate consumer process.

    :param name: a name for the task queue, e.g. your application's name.
    :param bool results: whether to store task results.
    :param bool store_none: whether to store ``None`` in the result store.
    :param bool store_intermediate_errors: when a task fails but has retries
        remaining, store the intermediate exception in the result store and run
        any ``on_error`` handler. When ``False``, the error is withheld until
        the task's retries are exhausted. Defaults to ``True`` for backwards
        compatibility.
    :param bool multi_waiter: default for the ``multi_waiter`` option of tasks.
        When enabled, tasks return a :py:class:`SharedResult` whose result can
        be read repeatedly by any number of independent waiters.
    :param retention: default retention period (seconds, or a
        ``datetime.timedelta``) for shared (multi-waiter) results. When
        ``None``, shared results are retained until read past their deadline or
        explicitly discarded.
    :param bool utc: use UTC internally by converting from local time.
    :param bool immediate: useful for debugging; causes tasks to be executed
        synchronously in the application.
    :param Serializer serializer: serializer implementation for tasks and
        result data. The default implementation uses pickle.
    :param bool compression: compress tasks and result data (gzip by default).
    :param bool use_zlib: use zlib for compression instead of gzip.
    :param bool immediate_use_memory: automatically switch to a local in-memory
        storage backend when immediate-mode is enabled.
    :param storage_kwargs: arbitrary keyword arguments that will be passed to
        the storage backend for additional configuration.

    Example usage::

        from huey import RedisHuey

        # Create a huey instance.
        huey = RedisHuey('my-app')

        @huey.task()
        def add_numbers(a, b):
            return a + b

        @huey.periodic_task(crontab(minute='0', hour='2'))
        def nightly_report():
            generate_nightly_report()
    """
    storage_class = None

    def __init__(self, name='huey', results=True, store_none=False, utc=True,
                 immediate=False, serializer=None, compression=False,
                 use_zlib=False, immediate_use_memory=True, storage_class=None,
                 store_intermediate_errors=True, multi_waiter=False,
                 result_retention=None, **storage_kwargs):

        self.name = name
        self.results = results
        self.store_none = store_none
        self.store_intermediate_errors = store_intermediate_errors
        self.multi_waiter = multi_waiter
        self.result_retention = normalize_retention(result_retention)
        self.utc = utc
        self._immediate = immediate
        self.immediate_use_memory = immediate_use_memory
        if serializer is None:
            serializer = Serializer(compression, use_zlib=use_zlib)
        self.serializer = serializer

        # Initialize storage.
        self.storage_kwargs = storage_kwargs
        if storage_class is not None:
            self.storage_class = storage_class
        self.storage = self.create_storage()

        # Allow overriding the default TaskWrapper implementation.
        self.task_wrapper_class = self.get_task_wrapper_class()

        self._locks = set()
        self._pre_execute = {}
        self._post_execute = {}
        self._startup = {}
        self._shutdown = {}
        self._registry = Registry()
        self._signal = S.Signal()
        self._tasks_in_flight = set()
        self._timeout_handler = None  # This is consumer-specific.

    def get_task_wrapper_class(self):
        return TaskWrapper

    def create_storage(self):
        # When using immediate mode, the default behavior is to use an
        # in-memory broker rather than a live one like Redis or Sqlite, however
        # this can be overridden by specifying "immediate_use_memory=False"
        # when initializing Huey.
        if self._immediate and self.immediate_use_memory:
            return self.get_immediate_storage()

        return self.get_storage(**self.storage_kwargs)

    def get_immediate_storage(self):
        return MemoryStorage(self.name)

    def get_storage(self, **kwargs):
        if self.storage_class is None:
            warnings.warn('storage_class not specified when initializing '
                          'huey, will default to RedisStorage.')
            Storage = RedisStorage
        else:
            Storage = self.storage_class
        return Storage(self.name, **kwargs)

    @property
    def immediate(self):
        return self._immediate

    @immediate.setter
    def immediate(self, value):
        if self._immediate != value:
            self._immediate = value
            # If we are using different storage engines for immediate-mode
            # versus normal mode, we need to recreate the storage engine.
            if self.immediate_use_memory:
                self.storage = self.create_storage()

    def create_consumer(self, **options):
        return Consumer(self, **options)

    def task(self, retries=0, retry_delay=0, retry_backoff=0, priority=None,
             context=False, name=None, expires=None, timeout=None,
             multi_waiter=None, result_retention=None, **kwargs):
        TaskWrapper = self.task_wrapper_class
        multi_waiter = self.multi_waiter if multi_waiter is None else multi_waiter
        result_retention = (self.result_retention
                            if result_retention is None else result_retention)
        def decorator(func):
            return TaskWrapper(
                self,
                func.func if isinstance(func, TaskWrapper) else func,
                context=context,
                name=name,
                default_retries=retries,
                default_retry_delay=retry_delay,
                default_retry_backoff=retry_backoff,
                default_priority=priority,
                default_expires=expires,
                default_timeout=timeout,
                multi_waiter=multi_waiter,
                result_retention=result_retention,
                **kwargs)
        return decorator

    def periodic_task(self, validate_datetime, retries=0, retry_delay=0,
                      retry_backoff=0, priority=None, context=False, name=None,
                      expires=None, timeout=None, multi_waiter=None,
                      result_retention=None, **kwargs):
        TaskWrapper = self.task_wrapper_class
        multi_waiter = self.multi_waiter if multi_waiter is None else multi_waiter
        result_retention = (self.result_retention
                            if result_retention is None else result_retention)
        def decorator(func):
            def method_validate(self, timestamp):
                return validate_datetime(timestamp)

            return TaskWrapper(
                self,
                func.func if isinstance(func, TaskWrapper) else func,
                context=context,
                name=name,
                default_retries=retries,
                default_retry_delay=retry_delay,
                default_retry_backoff=retry_backoff,
                default_priority=priority,
                default_expires=expires,
                default_timeout=timeout,
                validate_datetime=method_validate,
                task_base=PeriodicTask,
                multi_waiter=multi_waiter,
                result_retention=result_retention,
                **kwargs)

        return decorator

    def context_task(self, obj, as_argument=False, **kwargs):
        def context_decorator(fn):
            @wraps(fn)
            def inner(*a, **k):
                with obj as ctx:
                    if as_argument:
                        return fn(ctx, *a, **k)
                    else:
                        return fn(*a, **k)
            return inner
        def task_decorator(func):
            return self.task(**kwargs)(context_decorator(func))
        return task_decorator

    def pre_execute(self, name=None):
        def decorator(fn):
            self._pre_execute[name or fn.__name__] = fn
            return fn
        return decorator

    def unregister_pre_execute(self, name):
        if not isinstance(name, str):
            # Assume we were given the function itself.
            name = name.__name__
        return self._pre_execute.pop(name, None) is not None

    def post_execute(self, name=None):
        def decorator(fn):
            self._post_execute[name or fn.__name__] = fn
            return fn
        return decorator

    def unregister_post_execute(self, name):
        if not isinstance(name, str):
            # Assume we were given the function itself.
            name = name.__name__
        return self._post_execute.pop(name, None) is not None

    def on_startup(self, name=None):
        def decorator(fn):
            self._startup[name or fn.__name__] = fn
            return fn
        return decorator

    def unregister_on_startup(self, name):
        if not isinstance(name, str):
            # Assume we were given the function itself.
            name = name.__name__
        return self._startup.pop(name, None) is not None

    def on_shutdown(self, name=None):
        def decorator(fn):
            self._shutdown[name or fn.__name__] = fn
            return fn
        return decorator

    def unregister_on_shutdown(self, name=None):
        if not isinstance(name, str):
            # Assume we were given the function itself.
            name = name.__name__
        return self._shutdown.pop(name, None) is not None

    def notify_interrupted_tasks(self):
        while self._tasks_in_flight:
            try:
                task = self._tasks_in_flight.pop()
            except KeyError:
                break  # Task might have raced and finished.
            self._emit(S.SIGNAL_INTERRUPTED, task)

    def signal(self, *signals):
        def decorator(fn):
            self._signal.connect(fn, *signals)
            return fn
        return decorator

    def disconnect_signal(self, receiver, *signals):
        self._signal.disconnect(receiver, *signals)

    def _emit(self, signal, task, *args, **kwargs):
        self._signal.send(signal, task, *args, **kwargs)

    def serialize_task(self, task):
        message = self._registry.create_message(task)
        return self.serializer.serialize(message)

    def deserialize_task(self, data):
        message = self.serializer.deserialize(data)
        return self._registry.create_task(message)

    def enqueue(self, task):
        if isinstance(task, group):
            results = [self.enqueue(t) for t in task.tasks]
            if results and all(isinstance(r, SharedResult) for r in results):
                return SharedResultGroup(results)
            return ResultGroup(results)
        elif isinstance(task, chord):
            return self._enqueue_chord(task)

        # Resolve the expiration time when the task is enqueued.
        if task.expires:
            task.resolve_expires(self.utc)

        # Register the pending marker(s) before the task can run, so "not
        # finished yet" can be told apart from "never registered / expired".
        if task.multi_waiter:
            self._register_pipeline(task, 'register_pending')

        self._emit(S.SIGNAL_ENQUEUED, task)

        if self._immediate:
            self.execute(task)
        else:
            self.storage.enqueue(self.serialize_task(task), task.priority)

        if not self.results:
            return

        results = self._pipeline_results(task)
        if len(results) > 1:
            if all(isinstance(r, SharedResult) for r in results):
                return SharedResultGroup(results)
            return ResultGroup(results)
        return results[0]

    def _enqueue_chord(self, chord_obj):
        cid = str(uuid.uuid4())
        size = len(chord_obj.tasks)
        results = []
        for i, task in enumerate(chord_obj.tasks):
            if isinstance(task, group):
                raise ValueError('cannot use `group` as a chord member - '
                                 'use .then() to convert to a `chord` first.')

            config = ChordConfig(cid, size, i, chord_obj.callback)
            results.append(self._enqueue_chord_member(task, config))

        if not size:
            callback = copy.copy(chord_obj.callback)
            callback.extend_data(([],))
            self.enqueue(callback)

        # Register pending markers for the callback (and its pipeline) so a
        # waiter that starts before the chord completes sees STATE_PENDING.
        if chord_obj.callback.multi_waiter:
            self._register_pipeline(chord_obj.callback, 'register_pending')

        pipeline = self._pipeline_results(chord_obj.callback)
        pipeline_result = None
        if len(pipeline) > 1:
            if all(isinstance(r, SharedResult) for r in pipeline):
                pipeline_result = SharedResultGroup(pipeline)
            else:
                pipeline_result = ResultGroup(pipeline)
        return ChordResult(results, pipeline[0], pipeline_result)

    def _enqueue_chord_member(self, task, config):
        if isinstance(task, chord):
            head = task.callback
        else:
            head = task

        tail = head
        while tail.on_complete is not None:
            tail = tail.on_complete
        tail.chord_config = config

        if isinstance(task, chord):
            self._enqueue_chord(task)
        else:
            self.enqueue(head)

        return self._result_handle(tail)

    def _pipeline_results(self, task):
        results = []
        while task is not None:
            results.append(self._result_handle(task))
            task = task.on_complete
        return results

    def _register_pipeline(self, task, method, *args):
        # Apply the given SharedResult lifecycle method to every task in the
        # pipeline (the task itself plus any on_complete successors).
        while task is not None:
            if task.multi_waiter:
                handle = self._result_handle(task)
                getattr(handle, method)(*args)
            task = task.on_complete

    def dequeue(self):
        data = self.storage.dequeue()
        if data is not None:
            return self.deserialize_task(data)

    def put(self, key, data):
        return self.storage.put_data(key, self.serializer.serialize(data))

    def put_result(self, key, data):
        return self.storage.put_data(key, self.serializer.serialize(data),
                                     is_result=True)

    def put_if_empty(self, key, data, ttl=None):
        return self.storage.put_if_empty(key, self.serializer.serialize(data),
                                         ttl)

    def get_raw(self, key, peek=False):
        if peek:
            return self.storage.peek_data(key)
        else:
            return self.storage.pop_data(key)

    def get(self, key, peek=False):
        data = self.get_raw(key, peek)
        if data is not EmptyData:
            return self.serializer.deserialize(data)

    def delete(self, key):
        return self.storage.delete_data(key)

    def _timeout_context(self, task):
        if not task.timeout or self._timeout_handler is None:
            return noop_context()

        return self._timeout_handler(task.timeout)

    def _get_timestamp(self):
        return (utcnow() if self.utc else
                datetime.datetime.now())

    def execute(self, task, timestamp=None):
        if timestamp is None:
            timestamp = self._get_timestamp()

        if not self.ready_to_run(task, timestamp):
            self.add_schedule(task)
        elif self.is_revoked(task, timestamp, False):
            logger.warning('Task %s was revoked, not executing', task)
            self._emit(S.SIGNAL_REVOKED, task)
            self._abort_chord_member(task, SKIPPED)
        elif task.expires_resolved and task.expires_resolved < timestamp:
            logger.info('Task %s expired, not executing.', task)
            self._emit(S.SIGNAL_EXPIRED, task)
            self._abort_chord_member(task, SKIPPED)
        else:
            logger.info('Executing %s', task)
            self._emit(S.SIGNAL_EXECUTING, task)
            return self._execute(task, timestamp)

    def _execute(self, task, timestamp):
        if self._pre_execute:
            try:
                self._run_pre_execute(task)
            except CancelExecution:
                self._emit(S.SIGNAL_CANCELED, task)
                self._abort_chord_member(task, SKIPPED)
                return

        start = time.monotonic()
        exception = None
        retry_eta = None
        task_value = None

        if task.timeout:
            task._deadline = start + task.timeout

        try:
            self._tasks_in_flight.add(task)
            try:
                with self._timeout_context(task):
                    task_value = task.execute()
            finally:
                # notify_interrupted_tasks() may have already popped it.
                self._tasks_in_flight.discard(task)
                duration = time.monotonic() - start
        except TaskTimeout as exc:
            logger.warning('Task %s timed out after %ss.', task.id,
                           task.timeout)
            exception = exc
            self._emit(S.SIGNAL_TIMEOUT, task)
        except RateLimitExceeded as exc:
            delay = task.retry_delay or exc.delay
            if exc.retry or task.retries:
                logger.info('Task %s rate-limited on "%s", retrying in %s',
                            task.id, exc.key, delay)
                retry_eta = normalize_time(None, delay, self.utc)
                if exc.retry:
                    task.retries += 1
            else:
                logger.info('Task %s rate-limited on "%s"', task.id, exc.key)
            exception = exc
            self._emit(S.SIGNAL_RATE_LIMITED, task)
        except TaskLockedException as exc:
            logger.warning('Task %s not run, %s.', task.id, exc)
            exception = exc
            self._emit(S.SIGNAL_LOCKED, task)
        except RetryTask as exc:
            logger.info('Task %s raised RetryTask, retrying.', task.id)
            task.retries += 1
            if exc.eta or exc.delay is not None:
                retry_eta = normalize_time(exc.eta, exc.delay, self.utc)
            exception = exc
        except CancelExecution as exc:
            if exc.retry or (exc.retry is None and task.retries):
                task.retries = max(task.retries, 1)
                msg = '(task will be retried)'
            else:
                task.retries = 0
                msg = '(aborted, will not be retried)'
            logger.warning('Task %s raised CancelExecution %s.', task.id, msg)
            self._emit(S.SIGNAL_CANCELED, task)
            exception = exc
        except KeyboardInterrupt:
            logger.warning('Received exit signal, %s did not finish.', task.id)
            self._emit(S.SIGNAL_INTERRUPTED, task)
            return
        except Exception as exc:
            logger.exception('Unhandled exception in task %s.', task.id)
            exception = exc
            self._emit(S.SIGNAL_ERROR, task, exc)
        else:
            logger.info('%s executed in %0.3fs', task, duration)

        # Clear the flag if this instance of the task was revoked after it
        # began executing. Expiring storages implement pop_data as a peek.
        if not isinstance(task, PeriodicTask):
            self.delete(task.revoke_id)

        store = self.results and not isinstance(task, PeriodicTask)
        if exception is None:
            if store and (task_value is not None or self.store_none):
                self.put_result(task.id, task_value)
            if store and task.multi_waiter:
                # Shared results are always stored (even for a successful
                # ``None``) in a separate, non-destructive namespace.
                self._result_handle(task).store_success(task_value)
            self._run_post_execute(task, task_value, exception)
            self._emit(S.SIGNAL_COMPLETE, task)
            if task.on_complete:
                task.on_complete.extend_data(task_value)
                self.enqueue(task.on_complete)
            if task.chord_config is not None:
                self._check_chord(task.chord_config, task_value)
        else:
            final = not task.retries
            surface = final or (self.store_intermediate_errors and
                                not isinstance(exception, RETRYING_EXCEPTIONS))
            if surface:
                error = Error(self.build_error_result(task, exception))
                next_task = task if store else None
                while next_task is not None:
                    self.put_result(next_task.id, error)
                    if next_task.multi_waiter:
                        self._result_handle(next_task).store_failure(
                            error.metadata)
                    next_task = next_task.on_complete if final else None
            self._run_post_execute(task, task_value, exception)
            if surface and task.on_error:
                # Copy, as on_error is shared across retries.
                handler = copy.copy(task.on_error)
                handler.extend_data(exception)
                self.enqueue(handler)
            if final:
                self._abort_chord_member(task, error)
            else:
                self._emit(S.SIGNAL_RETRYING, task)
                self._requeue_task(task, self._get_timestamp(), retry_eta)

        return task_value

    def _abort_chord_member(self, task, value):
        # The rest of the pipeline won't run, so report to its chord here.
        while task is not None:
            if task.chord_config is not None:
                return self._check_chord(task.chord_config, value)
            task = task.on_complete

    def _check_chord(self, cc, value):
        chord_key = 'chord:%s' % cc.cid
        result_key = 'chord:%s:%s' % (cc.cid, cc.idx)
        self.put_result(result_key, value)

        if self.storage.incr(chord_key) == cc.size:
            self.storage.delete_counter(chord_key)

            # Expiring storages implement pop_data as a peek.
            results = []
            for idx in range(cc.size):
                key = 'chord:%s:%s' % (cc.cid, idx)
                results.append(self.get(key, peek=True))
                self.delete(key)

            callback = cc.callback
            callback.extend_data((results,))
            self.enqueue(callback)

    def _requeue_task(self, task, timestamp, retry_eta=None):
        task.retries -= 1
        logger.info('Requeueing %s, %s retries', task.id, task.retries)
        if retry_eta is not None:
            task.eta = retry_eta
            self.add_schedule(task)
        elif task.retry_delay:
            delay = datetime.timedelta(seconds=task.retry_delay)
            task.eta = timestamp + delay
            if task.retry_backoff:
                task.retry_delay *= task.retry_backoff
            self.add_schedule(task)
        else:
            self.enqueue(task)

    def _run_pre_execute(self, task):
        for name, callback in self._pre_execute.items():
            logger.debug('Pre-execute hook %s for %s.', name, task)
            try:
                callback(task)
            except CancelExecution:
                logger.warning('Task %s cancelled by %s (pre-execute).',
                               task, name)
                raise
            except Exception:
                logger.exception('Unhandled exception calling pre-execute '
                                 'hook %s for %s.', name, task)

    def _run_post_execute(self, task, task_value, exception):
        for name, callback in self._post_execute.items():
            logger.debug('Post-execute hook %s for %s.', name, task)
            try:
                callback(task, task_value, exception)
            except Exception:
                logger.exception('Unhandled exception calling post-execute '
                                 'hook %s for %s.', name, task)

    def build_error_result(self, task, exception):
        tb = ''.join(traceback.format_exception(
            type(exception), exception, exception.__traceback__))
        if isinstance(exception, TaskException):
            error = exception.metadata.get('error') or repr(exception)
        else:
            error = repr(exception)

        return {
            'error': error,
            'retries': task.retries,
            'traceback': tb,
            'task_id': task.id,
        }

    def _task_key(self, task_class, key):
        if isinstance(task_class, TaskWrapper):
            task_class = task_class.task_class
        return ':'.join((key, self._registry.task_to_string(task_class)))

    def revoke_all(self, task_class, revoke_until=None, revoke_once=False):
        if revoke_until is not None:
            revoke_until = normalize_time(revoke_until, utc=self.utc)
        self.put(self._task_key(task_class, 'rt'), (revoke_until, revoke_once))

    def restore_all(self, task_class):
        return self.delete(self._task_key(task_class, 'rt'))

    def revoke(self, task, revoke_until=None, revoke_once=False):
        if revoke_until is not None:
            revoke_until = normalize_time(revoke_until, utc=self.utc)
        self.put(task.revoke_id, (revoke_until, revoke_once))

    def restore(self, task):
        # Return value indicates whether the task was in fact revoked.
        return self.delete(task.revoke_id)

    def revoke_by_id(self, id, revoke_until=None, revoke_once=False):
        return self.revoke(Task(id=id), revoke_until, revoke_once)

    def restore_by_id(self, id):
        return self.restore(Task(id=id))

    def _check_revoked(self, data, timestamp=None, peek=True):
        """
        Given raw revocation data, returns a 2-tuple indicating:

        1. Is task revoked?
        2. Should task be restored?
        """
        if data is EmptyData:
            return False, False

        revoke_until, revoke_once = self.serializer.deserialize(data)
        if revoke_until is not None and timestamp is None:
            timestamp = self._get_timestamp()

        if revoke_once:
            # This task *was* revoked for one run, but now it should be
            # restored to normal execution (unless we are just peeking).
            return True, not peek
        elif revoke_until is not None and revoke_until <= timestamp:
            # Task is no longer revoked and can be restored.
            return False, not peek
        else:
            # Task is still revoked. Do not restore.
            return True, False

    def _revoke_keys(self, task):
        if isinstance(task, TaskWrapper) or inspect.isclass(task):
            return [self._task_key(task, 'rt')]
        if isinstance(task, Result):
            task = task.task
        if isinstance(task, Task):
            return [task.revoke_id, self._task_key(type(task), 'rt')]
        return [Task(id=task).revoke_id]

    def is_revoked(self, task, timestamp=None, peek=True):
        keys = self._revoke_keys(task)
        data = self.storage.peek_many(keys)
        for key in keys:
            revoked, restore = self._check_revoked(
                data.get(key, EmptyData), timestamp, peek)
            if restore:
                self.delete(key)
            if revoked:
                return True
        return False

    def add_schedule(self, task):
        data = self.serialize_task(task)
        eta = task.eta or datetime.datetime.fromtimestamp(0)
        self.storage.add_to_schedule(data, eta)
        logger.info('Added task %s to schedule, eta %s', task.id, eta)
        self._emit(S.SIGNAL_SCHEDULED, task)

    def read_schedule(self, timestamp=None):
        if timestamp is None:
            timestamp = self._get_timestamp()
        return self._deserialize_all(self.storage.read_schedule(timestamp))

    def _deserialize_all(self, messages):
        accum = []
        for msg in messages:
            try:
                accum.append(self.deserialize_task(msg))
            except Exception:
                logger.exception('Unable to deserialize task.')
        return accum

    def read_periodic(self, timestamp):
        if timestamp is None:
            timestamp = self._get_timestamp()
        return [task for task in self._registry.periodic_tasks
                if task.validate_datetime(timestamp)]

    def ready_to_run(self, task, timestamp=None):
        if timestamp is None:
            timestamp = self._get_timestamp()
        return task.eta is None or task.eta <= timestamp

    def pending(self, limit=None):
        return self._deserialize_all(self.storage.enqueued_items(limit))

    def pending_count(self):
        return self.storage.queue_size()

    def scheduled(self, limit=None):
        return self._deserialize_all(self.storage.scheduled_items(limit))

    def scheduled_count(self):
        return self.storage.schedule_size()

    def all_results(self):
        return self.storage.result_items()

    def result_count(self):
        return self.storage.result_store_size()

    def __bool__(self):
        return True

    def __len__(self):
        return self.pending_count()

    def flush(self):
        self.storage.flush_all()

    def lock_task(self, lock_name, ttl=None):
        return TaskLock(self, lock_name, ttl)

    def is_locked(self, lock_name):
        return TaskLock(self, lock_name).is_locked()

    def flush_locks(self, *names):
        flushed = set()
        locks = self._locks
        if names:
            lock_template = '%s.lock.%%s' % self.name
            named_locks = (lock_template % name.strip() for name in names)
            locks = itertools.chain(locks, named_locks)

        for lock_key in locks:
            if self.delete(lock_key):
                flushed.add(lock_key.split('.lock.', 1)[-1])

        return flushed

    def rate_limit(self, name, limit, per, retry=True):
        return RateLimit(self, name, limit, per, retry=retry)

    def _result_handle(self, task):
        if task.multi_waiter:
            return SharedResult(self, task, task.result_retention)
        return Result(self, task)

    def shared_result(self, id, retention=None):
        """
        Return a :py:class:`SharedResult` handle for the task with the given id.

        Unlike the handle returned by :py:meth:`Huey.result`, the result is
        read non-destructively and may be awaited by any number of independent
        waiters, each with its own timeout and cancellation. Waiting may begin
        before or after the task completes; the task itself is never re-run.

        :param id: the task's unique identifier.
        :param retention: optional retention period (seconds or
            ``datetime.timedelta``) applied to the handle. When omitted, the
            default configured on the Huey instance is used.
        """
        if retention is None:
            retention = self.result_retention
        return SharedResult(self, Task(id=id), normalize_retention(retention))

    def shared_result_group(self, ids, retention=None):
        """
        Return a :py:class:`SharedResultGroup` for the given task ids, keeping
        the order of the input. Each entry is an independent
        :py:class:`SharedResult`.
        """
        if retention is None:
            retention = self.result_retention
        retention = normalize_retention(retention)
        results = [SharedResult(self, Task(id=id), retention) for id in ids]
        return SharedResultGroup(results)

    def result(self, id, blocking=False, timeout=None, backoff=1.15,
               max_delay=1.0, revoke_on_timeout=False, preserve=False):
        task_result = Result(self, Task(id=id))
        return task_result.get(
            blocking=blocking,
            timeout=timeout,
            backoff=backoff,
            max_delay=max_delay,
            revoke_on_timeout=revoke_on_timeout,
            preserve=preserve)


class Task(object):
    default_expires = None
    default_priority = None
    default_retries = 0
    default_retry_delay = 0
    default_retry_backoff = 0
    default_timeout = None
    multi_waiter = False
    result_retention = None

    def __init__(self, args=None, kwargs=None, id=None, eta=None, retries=None,
                 retry_delay=None, priority=None, expires=None,
                 on_complete=None, on_error=None, expires_resolved=None,
                 timeout=None, chord_config=None, retry_backoff=None,
                 multi_waiter=None, result_retention=None):
        self.name = type(self).__name__
        self.args = () if args is None else args
        self.kwargs = {} if kwargs is None else kwargs
        self.id = id or self.create_id()
        self.revoke_id = 'r:%s' % self.id
        self.eta = eta
        self.retries = retries if retries is not None else self.default_retries
        self.retry_delay = retry_delay if retry_delay is not None else \
                self.default_retry_delay
        self.retry_backoff = retry_backoff if retry_backoff is not None else \
                self.default_retry_backoff
        self.priority = priority if priority is not None else \
                self.default_priority
        self.expires = expires if expires is not None else self.default_expires
        self.expires_resolved = expires_resolved
        self.timeout = timeout if timeout is not None else self.default_timeout
        self.chord_config = chord_config
        self.multi_waiter = (self.multi_waiter if multi_waiter is None
                             else multi_waiter)
        self.result_retention = (normalize_retention(
            self.result_retention if result_retention is None
            else result_retention))
        self._deadline = None

        self.on_complete = on_complete
        self.on_error = on_error

    @property
    def data(self):
        return (self.args, self.kwargs)

    def __repr__(self):
        rep = '%s.%s: %s' % (self.__module__, self.name, self.id)
        if self.eta:
            rep += ' @%s' % self.eta
        if self.expires:
            if self.expires_resolved and self.expires != self.expires_resolved:
                rep += ' exp=%s (%s)' % (self.expires, self.expires_resolved)
            else:
                rep += ' exp=%s' % self.expires
        if self.priority:
            rep += ' p=%s' % self.priority
        if self.retries:
            rep += ' %s retries' % self.retries
        if self.timeout:
            rep += ' timeout=%s' % self.timeout
        if self.on_complete:
            rep += ' -> %s' % self.on_complete
        if self.on_error:
            rep += ', on error %s' % self.on_error
        if self.chord_config:
            rep += ', chord %s' % self.chord_config.cid
        return rep

    def __hash__(self):
        return hash(self.id)

    def create_id(self):
        return str(uuid.uuid4())

    def resolve_expires(self, utc=True):
        if self.expires:
            self.expires_resolved = normalize_expire_time(self.expires, utc)
        return self.expires_resolved

    @property
    def time_remaining(self):
        if self._deadline:
            return max(0, self._deadline - time.monotonic())
        return float('inf')

    @property
    def is_timed_out(self):
        return self._deadline and time.monotonic() >= self._deadline

    def check_timeout(self):
        if self.is_timed_out:
            raise TaskTimeout('timeout %ss' % self.timeout)

    def extend_data(self, data):
        if data is None or data == ():
            return

        if isinstance(data, tuple):
            self.args += data
        elif isinstance(data, dict):
            # XXX: alternate would be self.kwargs.update(data), but this will
            # stomp on user-provided parameters.
            for key, value in data.items():
                self.kwargs.setdefault(key, value)
        else:
            self.args = self.args + (data,)

    def then(self, task, *args, **kwargs):
        if self.on_complete:
            self.on_complete.then(task, *args, **kwargs)
        else:
            self.on_complete = _signature(task, args, kwargs)
        return self

    def error(self, task, *args, **kwargs):
        if self.on_error:
            self.on_error.error(task, *args, **kwargs)
        else:
            self.on_error = _signature(task, args, kwargs)
        return self

    def execute(self):
        # Implementation provided by subclass, see: TaskWrapper.create_task().
        raise NotImplementedError

    def __eq__(self, rhs):
        if not isinstance(rhs, Task):
            return False

        return (
            self.id == rhs.id and
            self.eta == rhs.eta and
            type(self) == type(rhs))


def _signature(task, args=(), kwargs=None):
    if not isinstance(task, Task):
        return task.s(*args, **(kwargs or {}))
    if args: task.extend_data(args)
    if kwargs: task.extend_data(kwargs)
    return task


class PeriodicTask(Task):
    def validate_datetime(self, timestamp):
        return False


class TaskWrapper(object):
    task_base = Task

    def __init__(self, huey, func, context=False, name=None, task_base=None,
                 **settings):
        self.__doc__ = getattr(func, '__doc__', None)
        self.huey = huey
        self.func = func
        self.context = context
        self.name = name
        self.settings = settings
        if task_base is not None:
            self.task_base = task_base

        # Dynamically create task class and register with Huey instance.
        self.task_class = self.create_task(func, context, name, **settings)
        self.huey._registry.register(self.task_class)

    @property
    def retries(self):
        return self.task_class.default_retries

    @property
    def retry_delay(self):
        return self.task_class.default_retry_delay

    def unregister(self):
        return self.huey._registry.unregister(self.task_class)

    def create_task(self, func, context=False, name=None, **settings):
        if inspect.iscoroutinefunction(func):
            raise ConfigurationError(
                'huey does not support async functions. Wrap the coroutine '
                'with asyncio.run() in a regular function.')

        def execute(self):
            args, kwargs = self.data
            if self.context:
                # Copy, so a retry doesn't serialize the task into its kwargs.
                kwargs = dict(kwargs, task=self)
            return func(*args, **kwargs)

        attrs = {
            'context': context,
            'execute': execute,
            '__module__': func.__module__,
            '__doc__': func.__doc__}
        attrs.update(settings)

        if not name:
            name = func.__name__

        return type(name, (self.task_base,), attrs)

    def is_revoked(self, timestamp=None, peek=True):
        return self.huey.is_revoked(self.task_class, timestamp, peek)

    def revoke(self, revoke_until=None, revoke_once=False):
        self.huey.revoke_all(self.task_class, revoke_until, revoke_once)

    def restore(self):
        return self.huey.restore_all(self.task_class)

    def schedule(self, args=None, kwargs=None, eta=None, delay=None,
                 priority=None, retries=None, retry_delay=None,
                 retry_backoff=None, expires=None, timeout=None, id=None,
                 multi_waiter=None, result_retention=None):
        if eta is None and delay is None:
            if isinstance(args, (int, float)):
                delay = args
            elif isinstance(args, datetime.timedelta):
                delay = args.total_seconds()
            elif isinstance(args, datetime.datetime):
                eta = args
            else:
                raise ValueError('schedule() missing required eta= or delay=')
            args = None

        if kwargs is not None and not isinstance(kwargs, dict):
            raise ValueError('schedule() kwargs argument must be a dict.')

        eta = normalize_time(eta, delay, self.huey.utc)
        task = self.task_class(
            args or (),
            kwargs or {},
            id=id,
            eta=eta,
            retries=retries,
            retry_delay=retry_delay,
            retry_backoff=retry_backoff,
            priority=priority,
            expires=expires,
            timeout=timeout,
            multi_waiter=multi_waiter,
            result_retention=result_retention)
        return self.huey.enqueue(task)

    def _apply(self, it):
        return [self.s(*(i if isinstance(i, tuple) else (i,))) for i in it]

    def map(self, it):
        results = [self.huey.enqueue(t) for t in self._apply(it)]
        if self.huey.results:
            if results and all(isinstance(r, SharedResult) for r in results):
                return SharedResultGroup(results)
            return ResultGroup(results)

    def __call__(self, *args, **kwargs):
        return self.huey.enqueue(self.s(*args, **kwargs))

    def call_local(self, *args, **kwargs):
        return self.func(*args, **kwargs)

    def s(self, *args, **kwargs):
        eta = kwargs.pop('eta', None)
        delay = kwargs.pop('delay', None)
        if delay is not None and isinstance(delay, datetime.timedelta):
            delay = delay.total_seconds()
        if eta is not None or delay is not None:
            eta = normalize_time(eta, delay, self.huey.utc)

        return self.task_class(args, kwargs,
                               id=kwargs.pop('id', None),
                               eta=eta,
                               retries=kwargs.pop('retries', None),
                               retry_delay=kwargs.pop('retry_delay', None),
                               retry_backoff=kwargs.pop('retry_backoff', None),
                               priority=kwargs.pop('priority', None),
                               expires=kwargs.pop('expires', None),
                               timeout=kwargs.pop('timeout', None),
                               multi_waiter=kwargs.pop('multi_waiter', None),
                               result_retention=kwargs.pop(
                                   'result_retention', None))


class TaskLock(object):
    """
    Utilize the Storage key/value APIs to implement simple locking. For more
    details see :py:meth:`Huey.lock_task`.
    """
    def __init__(self, huey, name, ttl=None):
        self._huey = huey
        self._name = name
        self._ttl = ttl
        self._key = '%s.lock.%s' % (self._huey.name, self._name)
        self._huey._locks.add(self._key)

    def is_locked(self):
        return self._huey.storage.has_data_for_key(self._key)
    locked = is_locked

    def __call__(self, fn):
        @wraps(fn)
        def inner(*args, **kwargs):
            with self:
                return fn(*args, **kwargs)
        return inner

    def __enter__(self):
        self.acquire()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self._huey.delete(self._key)

    def acquire(self):
        if not self._huey.put_if_empty(self._key, '1', self._ttl):
            raise TaskLockedException('unable to acquire lock %s' % self._name)
        return True

    def clear(self):
        return self._huey.delete(self._key)
    release = clear


class RateLimit(object):
    """
    Utilize the Storage API counter to implement fixed window rate-limit.
    """
    def __init__(self, huey, name, limit, per, retry=True):
        self._huey = huey
        self._name = name
        self._limit = limit
        self._per = per
        self._retry = retry
        self._counter_key = '%s.rl.%s' % (self._huey.name, self._name)
        self._window_key = '%s.rl.%s.w' % (self._huey.name, self._name)

    def _current_window(self, now):
        return int(now) // self._per

    def _time_until_reset(self, now):
        window_end = (self._current_window(now) + 1) * self._per
        return window_end - now

    def reset(self):
        self._huey.delete(self._window_key)
        self._huey.storage.delete_counter(self._counter_key)

    def acquire(self):
        now = time.time()
        window = self._current_window(now)

        stored = self._huey.get(self._window_key, peek=True)
        if stored != window:
            self._huey.storage.delete_counter(self._counter_key)
            self._huey.put(self._window_key, window)

        count = self._huey.storage.incr(self._counter_key)
        if count > self._limit:
            ttr = self._time_until_reset(now)
            raise RateLimitExceeded(self._name, ttr, retry=self._retry)

    def current_usage(self):
        return self._huey.storage.incr(self._counter_key, 0)

    def __call__(self, fn):
        @wraps(fn)
        def inner(*args, **kwargs):
            with self:
                return fn(*args, **kwargs)
        return inner

    def __enter__(self):
        self.acquire()

    def __exit__(self, exc_type, exc_val, exc_tb):
        pass


class group(object):
    def __init__(self, tasks):
        self.tasks = tasks

    def then(self, task, *args, **kwargs):
        return chord(self.tasks, _signature(task, args, kwargs))

    def error(self, *args, **kwargs):
        for task in self.tasks:
            task.error(*args, **kwargs)
        return self


class chord(object):
    def __init__(self, tasks, callback):
        self.tasks = tasks
        self.callback = _signature(callback)

    def then(self, task, *args, **kwargs):
        self.callback.then(task, *args, **kwargs)
        return self

    def error(self, task, *args, **kwargs):
        self.callback.error(task, *args, **kwargs)
        return self


class Result(object):
    """
    Wrapper around task result data. When a task is executed, an instance of
    ``Result`` is returned to provide access to the return value.

    To retrieve the task's result value, you can simply call the wrapper::

        @huey.task()
        def my_task(a, b):
            return a + b

        result = my_task(1, 2)

        # After a moment, when the consumer has executed the task and put
        # the result in the result storage, we can retrieve the value.
        print result()  # Prints 3

        # If you want to block until the result is ready, you can pass
        # blocking=True. We'll also specify a 4 second timeout so we don't
        # block forever if the consumer goes down:
        result2 = my_task(2, 3)
        print(result(blocking=True, timeout=4))
    """
    def __init__(self, huey, task):
        self.huey = huey
        self.task = task
        self.revoke_id = task.revoke_id
        self._result = EmptyData

    def __repr__(self):
        return '<Result: task %s>' % self.id

    @property
    def id(self):
        return self.task.id

    def __call__(self, *args, **kwargs):
        return self.get(*args, **kwargs)

    def is_ready(self):
        return self._get() is not EmptyData

    def _get(self, preserve=False):
        task_id = self.id
        if self._result is EmptyData:
            res = self.huey.get_raw(task_id, peek=preserve)

            if res is not EmptyData:
                self._result = self.huey.serializer.deserialize(res)
                return self._result
            else:
                return res
        else:
            return self._result

    def get_raw_result(self, blocking=False, timeout=None, backoff=1.15,
                       max_delay=1.0, revoke_on_timeout=False, preserve=False):
        res = self._get(preserve)
        if res is not EmptyData:
            return res
        elif not blocking:
            return

        if self.huey.storage.wait_result(self.id, timeout, backoff, max_delay):
            res = self._get(preserve)
            if res is not EmptyData:
                return res

        if revoke_on_timeout:
            self.revoke()
        raise ResultTimeout('timed out waiting for result')

    def get(self, blocking=False, timeout=None, backoff=1.15, max_delay=1.0,
            revoke_on_timeout=False, preserve=False):
        result = self.get_raw_result(blocking, timeout, backoff, max_delay,
                                     revoke_on_timeout, preserve)
        if result is not None and isinstance(result, Error):
            raise TaskException(result.metadata)
        return result

    def is_revoked(self):
        return self.huey.is_revoked(self.task, peek=True)

    def revoke(self, revoke_once=True):
        self.huey.revoke(self.task, revoke_once=revoke_once)

    def restore(self):
        return self.huey.restore(self.task)

    def reschedule(self, eta=None, delay=None, expires=None, priority=None,
                   preserve_pipeline=True):
        # Rescheduling works by revoking the currently-scheduled task (nothing
        # is done to check if the task has already run, however). Then the
        # original task's data is used to enqueue a new task with a new task ID
        # and execution_time.
        self.revoke()
        if eta is not None or delay is not None:
            eta = normalize_time(eta, delay, self.huey.utc)
        if preserve_pipeline:
            on_complete = self.task.on_complete
            on_error = self.task.on_error
        else:
            on_complete = on_error = None

        task = type(self.task)(
            self.task.args,
            self.task.kwargs,
            eta=eta,
            retries=self.task.retries,
            retry_delay=self.task.retry_delay,
            retry_backoff=self.task.retry_backoff,
            priority=priority if priority is not None else self.task.priority,
            expires=expires if expires is not None else self.task.expires,
            timeout=self.task.timeout,
            chord_config=self.task.chord_config,
            on_complete=on_complete,
            on_error=on_error,
            multi_waiter=self.task.multi_waiter,
            result_retention=self.task.result_retention)
        return self.huey.enqueue(task)

    def reset(self):
        self._result = EmptyData


class ResultGroup(object):
    def __init__(self, results):
        self._results = results

    def get(self, *args, **kwargs):
        return [result.get(*args, **kwargs) for result in self._results]
    __call__ = get

    def __getitem__(self, idx):
        return self._results[idx].get(True)
    def __iter__(self):
        return iter(self._results)
    def __len__(self):
        return len(self._results)
    def as_completed(self, backoff=1.15, max_delay=1.0):
        res = deque(self._results)
        delay = {r.id: 0. for r in res}
        while res:
            r = res.popleft()
            if delay[r.id]:
                time.sleep(delay[r.id])
            if r._get() is EmptyData:
                res.append(r)
                delay[r.id] = min((delay[r.id] or 0.1) * backoff, max_delay)
            else:
                yield r.get()


class ChordResult(object):
    def __init__(self, results, callback_result, pipeline=None):
        results = list(results)
        if results and all(isinstance(r, SharedResult) for r in results):
            self.results = SharedResultGroup(results)
        else:
            self.results = ResultGroup(results)
        self.callback = callback_result
        self.pipeline_results = pipeline

    def get(self, *args, **kwargs):
        return self.callback.get(*args, **kwargs)
    __call__ = get

    def reset(self):
        self.callback.reset()


# States reported by :class:`SharedResult` and :class:`WaitResult`. A
# successful task whose return value is ``None`` is STATE_SUCCESS, which is
# deliberately distinct from STATE_PENDING (task not finished) and
# STATE_EXPIRED (result no longer retained).
STATE_PENDING = 'pending'
STATE_SUCCESS = 'success'
STATE_FAILURE = 'failure'
STATE_EXPIRED = 'expired'

# Storage keys for shared (multi-waiter) results. These are kept in a separate
# namespace from the task-id keys used by the ordinary, destructive result
# store, so the semantics of ``Result.get`` / ``Huey.get`` / peek are unchanged.
shared_result_key = 'shared:result:%s'
shared_pending_key = 'shared:pending:%s'


class WaitResult(object):
    """
    The outcome of a single wait (or non-blocking inspection) for a task whose
    result is shared among multiple waiters.

    ``state`` is one of :data:`STATE_PENDING`, :data:`STATE_SUCCESS`,
    :data:`STATE_FAILURE` or :data:`STATE_EXPIRED`:

    * ``STATE_SUCCESS`` -- the task finished. ``value`` is the return value and
      may legitimately be ``None``.
    * ``STATE_FAILURE`` -- the task raised. ``metadata`` holds the error dict
      that would normally be passed to :py:class:`TaskException`.
    * ``STATE_PENDING`` -- the task has not finished yet. ``timed_out`` is
      ``True`` when a blocking wait gave up because its own timeout elapsed.
    * ``STATE_EXPIRED`` -- there is no live result and no live pending marker,
      so the result was never registered or is past its retention period.

    Instances are immutable value objects.
    """
    __slots__ = ('state', 'value', 'metadata', 'task_id', 'timed_out')

    def __init__(self, state, value=None, metadata=None, task_id=None,
                 timed_out=False):
        self.state = state
        self.value = value
        self.metadata = metadata
        self.task_id = task_id
        self.timed_out = timed_out

    @property
    def pending(self):
        return self.state == STATE_PENDING

    @property
    def ok(self):
        return self.state == STATE_SUCCESS

    @property
    def failed(self):
        return self.state == STATE_FAILURE

    @property
    def expired(self):
        return self.state == STATE_EXPIRED

    def get(self):
        """
        Return the task's return value, or raise for any non-success state.

        :raises TaskException: the task failed (see ``metadata``).
        :raises ResultPending: the task has not finished yet.
        :raises ResultExpired: the result is past its retention period.
        """
        if self.state == STATE_FAILURE:
            raise TaskException(self.metadata)
        if self.state == STATE_PENDING:
            raise ResultPending('task %s has not finished' % self.task_id)
        if self.state == STATE_EXPIRED:
            raise ResultExpired('result for task %s has expired' % self.task_id)
        return self.value

    def __repr__(self):
        return '<WaitResult: %s task=%s>' % (self.state, self.task_id)

    def __eq__(self, other):
        if isinstance(other, WaitResult):
            return (self.state == other.state and
                    self.value == other.value and
                    self.metadata == other.metadata and
                    self.task_id == other.task_id and
                    self.timed_out == other.timed_out)
        return NotImplemented

    def __ne__(self, other):
        result = self.__eq__(other)
        return result if result is NotImplemented else not result

    def __hash__(self):
        return hash((self.state, self.task_id))


class Waiter(object):
    """
    An independent waiter on a :py:class:`SharedResult`.

    Every waiter owns its own cancellation flag and applies its own timeout, so
    cancelling or timing out one waiter has no effect on any other waiter, and
    none of them can consume the result.
    """
    def __init__(self, result, waiter_id=None):
        self.result = result
        self.id = waiter_id if waiter_id is not None else id(self)
        self._cancel = threading.Event()

    def wait(self, timeout=None, backoff=1.15, max_delay=1.0):
        """
        Block until the task reaches a terminal state, this waiter is
        cancelled, or ``timeout`` seconds elapse.

        :param timeout: maximum number of seconds to wait, or ``None`` to wait
            indefinitely. The timeout is local to this waiter.
        :param backoff: multiplier applied to the polling delay each iteration.
        :param max_delay: maximum polling delay.
        :return: a :py:class:`WaitResult`. A timeout yields a ``STATE_PENDING``
            result with ``timed_out=True``.
        :raises WaitCancelled: if this waiter was cancelled.
        """
        result = self.result
        deadline = None if timeout is None else time.monotonic() + timeout
        delay = 0.05
        while True:
            if self._cancel.is_set():
                raise WaitCancelled('waiter %s was cancelled' % (self.id,))

            snapshot = result._snapshot()
            if snapshot.state != STATE_PENDING:
                return snapshot

            if deadline is None:
                sleep_for = delay
            else:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    return WaitResult(STATE_PENDING, task_id=result.id,
                                      timed_out=True)
                sleep_for = min(delay, remaining)

            # Sleeping on the event lets cancel() wake the waiter immediately,
            # without disturbing any other waiter.
            if self._cancel.wait(sleep_for):
                raise WaitCancelled('waiter %s was cancelled' % (self.id,))
            delay = min(delay * backoff, max_delay)

    __call__ = wait

    def cancel(self):
        """Cancel *this* waiter only. Other waiters are unaffected."""
        self._cancel.set()

    def is_cancelled(self):
        return self._cancel.is_set()

    def reset(self):
        """Clear the cancellation flag so the waiter can be used again."""
        self._cancel.clear()

    def __repr__(self):
        return '<Waiter: %s task=%s>' % (self.id, self.result.id)


class SharedResult(object):
    """
    Multi-waiter handle for a task result.

    Unlike :py:class:`Result`, reads are **non-destructive**: the result is
    stored in a dedicated namespace and stays readable by any number of
    independent waiters until its retention period elapses. Obtaining a handle
    or waiting on it never re-executes the task, so waiting may begin before or
    after the task completes.

    Handles are normally obtained by enqueuing a task declared with
    ``multi_waiter=True``, by enqueuing a group of such tasks (which yields a
    :py:class:`SharedResultGroup`), or via :py:meth:`Huey.shared_result`.

    :param huey: the :py:class:`Huey` instance.
    :param task: the :py:class:`Task` whose result is shared.
    :param retention: how long (in seconds) the result remains readable after
        the task completes, or ``None`` to retain it indefinitely.
    """
    def __init__(self, huey, task, retention=None):
        self.huey = huey
        self.task = task
        self.revoke_id = task.revoke_id
        self.retention = normalize_retention(retention)
        self._waiters = {}
        self._lock = threading.Lock()
        self._result_key = shared_result_key % self.id
        self._pending_key = shared_pending_key % self.id

    def __repr__(self):
        return '<SharedResult: task %s>' % self.id

    @property
    def id(self):
        return self.task.id

    # -- storage helpers ---------------------------------------------------

    def _now(self):
        return time.time()

    def _read_envelope(self):
        data = self.huey.storage.peek_data(self._result_key)
        if data is EmptyData:
            return EmptyData

        envelope = self.huey.serializer.deserialize(data)
        deadline = envelope.get('deadline')
        if deadline is not None and self._now() >= deadline:
            # Expired: lazily remove the record. Every reader applies the same
            # check, so failing to delete it is harmless.
            try:
                self.huey.storage.delete_data(self._result_key)
            except Exception:
                logger.exception('Unable to delete expired result %s', self.id)
            return EmptyData
        return envelope

    def _deadline(self):
        if self.retention is None:
            return None
        return self._now() + self.retention

    def _store(self, envelope):
        self.huey.storage.put_data(
            self._result_key,
            self.huey.serializer.serialize(envelope),
            is_result=True)
        # The terminal result supersedes the pending marker.
        self.huey.storage.delete_data(self._pending_key)

    # -- lifecycle (used by the consumer) ----------------------------------

    def register_pending(self, retention=None):
        """
        Record that the task is in flight, so "not finished" can be told apart
        from "no such result". Called automatically when a multi-waiter task is
        enqueued. ``put_if_empty`` makes this idempotent.
        """
        if retention is not None:
            self.retention = normalize_retention(retention)
        marker = {'created': self._now()}
        self.huey.storage.put_if_empty(
            self._pending_key, self.huey.serializer.serialize(marker))

    def store_success(self, value):
        """Store a successful return value (``None`` included)."""
        self._store({'error': False,
                     'value': value,
                     'metadata': None,
                     'created': self._now(),
                     'deadline': self._deadline()})

    def store_failure(self, metadata):
        """Store a task failure, preserving the error metadata."""
        self._store({'error': True,
                     'value': None,
                     'metadata': metadata,
                     'created': self._now(),
                     'deadline': self._deadline()})

    # -- inspection --------------------------------------------------------

    def _snapshot(self):
        envelope = self._read_envelope()
        if envelope is not EmptyData:
            if envelope['error']:
                return WaitResult(STATE_FAILURE, metadata=envelope['metadata'],
                                  task_id=self.id)
            return WaitResult(STATE_SUCCESS, value=envelope['value'],
                              task_id=self.id)

        if self.huey.storage.has_data_for_key(self._pending_key):
            return WaitResult(STATE_PENDING, task_id=self.id)

        return WaitResult(STATE_EXPIRED, task_id=self.id)

    def state(self):
        """
        Return the current state without blocking. See :data:`STATE_PENDING`
        and friends.
        """
        return self._snapshot().state

    def is_ready(self):
        """
        Return whether a terminal (success or failure) result is available.

        Unlike :py:meth:`Result.is_ready`, this is non-destructive: the value
        remains readable by every waiter.
        """
        return self.state() in (STATE_SUCCESS, STATE_FAILURE)

    def inspect(self):
        """Return a non-blocking :class:`WaitResult` snapshot."""
        return self._snapshot()

    # -- waiting -----------------------------------------------------------

    def waiter(self, waiter_id=None):
        """
        Return a waiter. When ``waiter_id`` is given, repeated calls with the
        same id return the same waiter (useful for a stable per-consumer
        identity); otherwise a new, independent waiter is created.
        """
        with self._lock:
            if waiter_id is not None:
                existing = self._waiters.get(waiter_id)
                if existing is not None:
                    return existing
            waiter = Waiter(self, waiter_id)
            if waiter_id is not None:
                self._waiters[waiter_id] = waiter
            return waiter

    def new_waiter(self):
        """Create a fresh, independent waiter."""
        return Waiter(self)

    def wait(self, timeout=None, backoff=1.15, max_delay=1.0):
        """
        Block for the result using an anonymous, independent waiter.

        See :py:meth:`Waiter.wait` for the meaning of the parameters. Returns a
        :py:class:`WaitResult`; timeouts return a pending result rather than
        raising.
        """
        return self.new_waiter().wait(timeout, backoff, max_delay)

    def get(self, timeout=None, blocking=True, backoff=1.15, max_delay=1.0):
        """
        Return the task's value, or raise for non-success states.

        :param timeout: seconds to block (when ``blocking=True``).
        :param bool blocking: when ``False``, return immediately, raising
            :py:class:`ResultPending` if the task has not finished.
        :raises TaskException: the task failed.
        :raises ResultTimeout: a blocking wait timed out.
        :raises ResultPending: not finished and ``blocking=False``.
        :raises ResultExpired: the result is past its retention period.
        """
        if blocking:
            result = self.wait(timeout, backoff, max_delay)
            if result.pending:
                raise ResultTimeout('timed out waiting for result')
        else:
            result = self.inspect()
            if result.pending:
                raise ResultPending('task %s has not finished' % self.id)
        return result.get()

    def discard(self):
        """Remove all shared state (pending marker and result) for this task."""
        self.huey.storage.delete_data(self._result_key)
        self.huey.storage.delete_data(self._pending_key)

    def reset(self):
        """Forget locally-cached waiters (storage is left untouched)."""
        with self._lock:
            self._waiters.clear()


class SharedResultGroup(object):
    """
    A group of :py:class:`SharedResult` handles.

    The original task order is preserved. Each item is waited on independently
    (with its own timeout/cancellation), and failures are expressed per item
    rather than aborting the whole group.
    """
    def __init__(self, results):
        self._results = list(results)

    def __repr__(self):
        return '<SharedResultGroup: %d tasks>' % len(self._results)

    def __iter__(self):
        return iter(self._results)

    def __len__(self):
        return len(self._results)

    def __getitem__(self, idx):
        return self._results[idx]

    @property
    def results(self):
        return list(self._results)

    @property
    def ids(self):
        return [result.id for result in self._results]

    def is_ready(self):
        """Return ``True`` when every item has a terminal result."""
        return all(result.is_ready() for result in self._results)

    def states(self):
        """Return the states of the items, in task order."""
        return [result.state() for result in self._results]

    def inspect(self):
        """Return non-blocking :class:`WaitResult` snapshots, in task order."""
        return [result.inspect() for result in self._results]

    def wait(self, timeout=None, backoff=1.15, max_delay=1.0):
        """
        Wait for every item, returning a :class:`WaitResult` per item in task
        order. Each item is given its own ``timeout`` budget, so one item
        timing out does not prevent the others from being reported.
        """
        return [result.wait(timeout, backoff, max_delay)
                for result in self._results]

    def get(self, timeout=None, backoff=1.15, max_delay=1.0):
        """
        Return the raw values of every item, in task order.

        A failed item is represented by an :py:class:`~huey.utils.Error`
        wrapper (as with chord callbacks), so failures are expressed per item.
        A pending (timed out) or expired item raises ``ResultTimeout`` /
        ``ResultExpired`` respectively -- use :py:meth:`wait` if a fully
        non-raising, per-item result is required.
        """
        accum = []
        for idx, item in enumerate(self.wait(timeout, backoff, max_delay)):
            if item.failed:
                accum.append(Error(item.metadata))
            elif item.pending:
                raise ResultTimeout(
                    'timed out waiting for result item %d' % idx)
            elif item.expired:
                raise ResultExpired(
                    'result for item %d has expired' % idx)
            else:
                accum.append(item.value)
        return accum
    __call__ = get

    def values(self, timeout=None, backoff=1.15, max_delay=1.0):
        """Alias for :py:meth:`get`."""
        return self.get(timeout, backoff, max_delay)

    def discard(self):
        """Discard the shared state of every item."""
        for result in self._results:
            result.discard()

    def reset(self):
        for result in self._results:
            result.reset()


dash_re = re.compile(r'(\d+)-(\d+)(?:/(\d+))?')
every_re = re.compile(r'\*\/(\d+)')


def crontab(minute='*', hour='*', day='*', month='*', day_of_week='*', strict=False):
    """
    Convert a "crontab"-style set of parameters into a test function that will
    return True when the given datetime matches the parameters set forth in
    the crontab.

    For day-of-week, 0=Sunday and 6=Saturday.

    Acceptable inputs:
    * = every distinct value
    */n = run every "n" times, i.e. hours='*/4' == 0, 4, 8, 12, 16, 20
    m-n = run every time m..n
    m,n = run on m and n

    The strict parameter will cause crontab to raise a ValueError if an input
    does not match a supported crontab input format. This provides backwards
    compatibility.
    """
    validation = (
        ('m', month, range(1, 13)),
        ('d', day, range(1, 32)),
        ('w', day_of_week, range(8)), # 0-6, but also 7 for Sunday.
        ('H', hour, range(24)),
        ('M', minute, range(60))
    )
    cron_settings = []

    for (date_str, value, acceptable) in validation:
        settings = set([])

        if isinstance(value, int):
            value = str(value)

        for piece in value.split(','):
            if piece == '*':
                settings.update(acceptable)
                continue

            if piece.isdigit():
                piece = int(piece)
                if piece not in acceptable:
                    raise ValueError('%d is not a valid input' % piece)
                elif date_str == 'w':
                    piece %= 7
                settings.add(piece)
                continue

            dash_match = dash_re.fullmatch(piece)
            if dash_match:
                lhs, rhs, step = dash_match.groups()
                lhs, rhs, step = int(lhs), int(rhs), int(step or 1)
                if lhs not in acceptable or rhs not in acceptable:
                    raise ValueError('%s is not a valid input' % piece)
                if lhs <= rhs:
                    values = list(range(lhs, rhs + 1))
                else:
                    hi = 6 if date_str == 'w' else acceptable[-1]
                    values = (list(range(lhs, hi + 1)) +
                              list(range(acceptable[0], rhs + 1)))
                values = values[::step]
                if date_str == 'w':
                    values = [v % 7 for v in values]
                settings.update(values)
                continue

            # Handle stuff like */3, */6.
            every_match = every_re.fullmatch(piece)
            if every_match:
                if date_str == 'w':
                    raise ValueError('Cannot perform this kind of matching'
                                     ' on day-of-week.')
                interval = int(every_match.groups()[0])
                settings.update(acceptable[::interval])
                continue

            # Older versions of Huey would, at this point, ignore the unmatched piece.
            if strict:
                raise ValueError('%s is not a valid input' % piece)

        if not settings:
            raise ValueError('%s matches no values' % value)
        cron_settings.append(sorted(list(settings)))

    def validate_date(timestamp):
        _, m, d, H, M, _, w, _, _ = timestamp.timetuple()

        # fix the weekday to be sunday=0
        w = (w + 1) % 7

        for (date_piece, selection) in zip((m, d, w, H, M), cron_settings):
            if date_piece not in selection:
                return False

        return True

    return validate_date


# Convenience helpers.
crontab.hourly = partial(crontab, minute='0')
crontab.daily = partial(crontab, minute='0', hour='0')


# Convenience wrappers for the various storage implementations.
class BlackHoleHuey(Huey):
    storage_class = BlackHoleStorage

class MemoryHuey(Huey):
    storage_class = MemoryStorage

class SqliteHuey(Huey):
    storage_class = SqliteStorage

class CySqliteHuey(Huey):
    storage_class = CySqliteStorage

class PostgresHuey(Huey):
    storage_class = PostgresStorage

class RedisHuey(Huey):
    storage_class = RedisStorage

class RedisExpireHuey(RedisHuey):
    storage_class = RedisExpireStorage

class PriorityRedisHuey(RedisHuey):
    storage_class = PriorityRedisStorage

class PriorityRedisExpireHuey(RedisHuey):
    storage_class = PriorityRedisExpireStorage

class FileHuey(Huey):
    storage_class = FileStorage
