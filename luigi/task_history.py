# -*- coding: utf-8 -*-
#
# Copyright 2012-2015 Spotify AB
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
"""
Abstract class for task history.
Currently the only subclass is :py:class:`~luigi.db_task_history.DbTaskHistory`.
"""

import abc
import copy
import logging

logger = logging.getLogger("luigi-interface")


class FrozenTask:
    """
    Timestamped, immutable copy of the task fields a history writer needs.

    Events are decided once (the scheduler assigns them a sequence number at
    the moment the status changes) but may be written later. Passing a frozen
    copy to the history implementation means a deferred write records the task
    as it was when the event happened, not as it looks when the write finally
    runs.
    """

    __slots__ = ("_source", "id", "task_id", "family", "params", "public_params", "status")

    def __init__(self, task, status):
        # Keep a complete detached copy as well as the commonly used fields.
        # The scheduler's live Task object can move to PENDING/RUNNING while a
        # history write is delayed; this copy keeps the event at its decision.
        source = copy.deepcopy(task)
        source.status = status
        object.__setattr__(self, "_source", source)

        self.id = task.id
        self.task_id = getattr(task, "task_id", task.id)
        self.family = task.family
        self.params = dict(task.params or {})
        self.public_params = dict(getattr(task, "public_params", None) or self.params)
        self.status = status

    def __getattr__(self, name):
        # Unpickling asks for slots before __init__ has restored _source.
        if name == "_source":
            raise AttributeError(name)
        # Fall back to the full Task-compatible object so existing history
        # implementations can keep using helpers such as pretty_id.
        return getattr(self._source, name)

    def __repr__(self):
        return "FrozenTask(id=%r, status=%r)" % (self.id, self.status)


class StoredTask:
    """
    Interface for methods on TaskHistory
    """

    # TODO : do we need this task as distinct from luigi.scheduler.Task?
    #        this only records host and record_id in addition to task parameters.

    def __init__(self, task, status, host=None):
        self._task = task
        self.status = status
        self.record_id = None
        self.host = host

    @property
    def task_family(self):
        return self._task.family

    @property
    def parameters(self):
        return self._task.params


class TaskHistory(metaclass=abc.ABCMeta):
    """
    Abstract Base Class for updating the run history of a task

    Implementations normally record each event with the wall clock time of the
    call. When an event is written late (for example after a deferred write is
    flushed by the scheduler), the scheduler wants to keep the original
    decision time so the event stays at its logical position in the history.
    Implementations that can honour ``ts`` set :py:attr:`accepts_event_timestamps`
    to ``True`` and accept an optional ``ts`` keyword on every callback; all
    other implementations keep the legacy callback signatures and get the
    write time.

    An implementation may also *defer* a write by returning ``False``. In that
    case the scheduler keeps the event in its journal as outstanding and
    retries it later instead of treating it as recorded.
    """

    accepts_event_timestamps = False

    @abc.abstractmethod
    def task_scheduled(self, task):
        pass

    @abc.abstractmethod
    def task_finished(self, task, successful):
        pass

    @abc.abstractmethod
    def task_started(self, task, worker_host):
        pass

    # TODO(erikbern): should web method (find_latest_runs etc) be abstract?


class NopHistory(TaskHistory):
    def task_scheduled(self, task):
        pass

    def task_finished(self, task, successful):
        pass

    def task_started(self, task, worker_host):
        pass


class DeferringHistory(TaskHistory):
    """
    Test/support helper that buffers events instead of writing them.

    ``defer`` is the number of upcoming writes to hold back; with ``defer=None``
    every write is held back. ``defer_kinds`` restricts the hold-back to those
    event kinds (``"scheduled"``/``"started"``/``"finished"``), e.g. to let a
    single failing event lag behind while everything else is written.

    Held events are kept in :py:attr:`pending`; :py:meth:`flush` moves them to
    :py:attr:`events` in the order the scheduler decided them, keeping their
    original ``ts``. The scheduler learns that a write did not happen from the
    ``False`` return value of the callbacks.
    """

    accepts_event_timestamps = True

    def __init__(self, defer=0, defer_kinds=None):
        self.defer = defer
        self.defer_kinds = set(defer_kinds) if defer_kinds else None
        # Each entry is (kind, task, payload, ts).
        self.events = []  # written
        self.pending = []  # still outstanding

    def _should_defer(self, kind):
        if self.defer_kinds is not None:
            # A kind filter holds every matching write back until release().
            return kind in self.defer_kinds
        return self.defer is None or self.defer > 0

    def _record(self, kind, task, payload=None, ts=None):
        if self._should_defer(kind):
            if self.defer_kinds is None and self.defer is not None:
                self.defer -= 1
            self.pending.append((kind, task, payload, ts))
            return False
        self.events.append((kind, task, payload, ts))
        return True

    def task_scheduled(self, task, ts=None):
        return self._record("scheduled", task, ts=ts)

    def task_started(self, task, worker_host, ts=None):
        return self._record("started", task, worker_host, ts)

    def task_finished(self, task, successful, ts=None):
        return self._record("finished", task, bool(successful), ts)

    def release(self, kind=None):
        """Stop holding back writes (optionally only for one kind)."""
        if kind is None:
            self.defer = 0
            self.defer_kinds = None
        elif self.defer_kinds is not None:
            self.defer_kinds.discard(kind)

    def written(self, kind=None):
        """The events this history has actually recorded, optionally by kind."""
        return [entry for entry in self.events if kind is None or entry[0] == kind]
