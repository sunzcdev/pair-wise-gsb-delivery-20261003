# This file is a part of Dramatiq.
#
# Copyright (C) 2017,2018 CLEARTYPE SRL <bogdan@cleartype.io>
#
# Dramatiq is free software; you can redistribute it and/or modify it
# under the terms of the GNU Lesser General Public License as published by
# the Free Software Foundation, either version 3 of the License, or (at
# your option) any later version.
#
# Dramatiq is distributed in the hope that it will be useful, but WITHOUT
# ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
# FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser General Public
# License for more details.
#
# You should have received a copy of the GNU Lesser General Public License
# along with this program.  If not, see <http://www.gnu.org/licenses/>.
from __future__ import annotations

import time
from typing import TYPE_CHECKING, Any, Iterable, Iterator
from uuid import uuid4

from .broker import get_broker
from .rate_limits import Barrier
from .results import ResultFailure, ResultMissing, ResultStatus
from .results.backend import DEFAULT_TIMEOUT
from .results.group_result import GroupResult

if TYPE_CHECKING:
    from .message import Message

#: The minimum amount of time, in milliseconds, to wait between polls
#: of group results while iterating them in completion order.
ITER_RESULTS_BACKOFF = 100


def _get_composition_result(child: Message | pipeline, backend, *, block: bool, timeout: int | None) -> Any:
    """Fetch the terminal result of a group leaf.

    Pipeline results are represented by their terminal message, so
    this mirrors what :meth:`group.get_results` yields for pipeline
    children.
    """
    if isinstance(child, pipeline):
        return child.get_result(block=block, timeout=timeout)

    return child.get_result(backend=backend, block=block, timeout=timeout)


def _collect_group_leaves(
    children: list[Message | group | pipeline], prefix: tuple[int, ...] = ()
) -> list[tuple[tuple[int, ...], Message | pipeline]]:
    """Flatten a group's children into the terminal jobs whose results
    can be polled, pairing each one with its index path.

    The index path records the position of the job at every level of
    nesting, so the terminal job of a pipeline nested inside the second
    child of an inner group keeps a path such as ``(1, 0)`` that can
    be used to locate it within the original composition.
    """
    leaves: list[tuple[tuple[int, ...], Message | pipeline]] = []
    for index, child in enumerate(children):
        index_path = prefix + (index,)
        if isinstance(child, group):
            leaves.extend(_collect_group_leaves(child.children, index_path))
        else:
            # Pipelines are treated as a single leaf: only the result
            # of the terminal message of the chain is exposed.
            leaves.append((index_path, child))

    return leaves


class pipeline:
    """Chain actors together, passing the result of one actor to the
    next one in line.

    Parameters:
      children(Iterable[Message|pipeline]): A sequence of messages or
        pipelines.  Child pipelines are flattened into the resulting
        pipeline.
      broker(Broker): The broker to run the pipeline on.  Defaults to
        the current global broker.
    """

    messages: list[Message]

    def __init__(self, children: Iterable[Message | pipeline], *, broker=None):
        self.broker = broker or get_broker()
        messages: list[Message]
        self.messages = messages = []

        for child in children:
            if isinstance(child, pipeline):
                messages.extend(message.copy() for message in child.messages)
            else:
                messages.append(child.copy())

        for message, next_message in zip(messages, messages[1:]):
            message.options["pipe_target"] = next_message.asdict()

    def __len__(self):
        """Returns the length of the pipeline."""
        return len(self.messages)

    def __or__(self, other):
        """Returns a new pipeline with "other" added to the end."""
        return type(self)(self.messages + [other])

    def __str__(self):  # pragma: no cover
        return "pipeline([%s])" % ", ".join(str(m) for m in self.messages)

    @property
    def completed(self):
        """Returns True when all the jobs in the pipeline have been
        completed.  This will always return False if the last actor in
        the pipeline doesn't store results.

        Raises:
          RuntimeError: If your broker doesn't have a result backend
            set up.
        """
        try:
            self.messages[-1].get_result()

            return True
        except ResultMissing:
            return False

    @property
    def completed_count(self):
        """Returns the total number of jobs that have been completed.
        Actors that don't store results are not counted, meaning this
        may be inaccurate if all or some of your actors don't store
        results.

        Raises:
          RuntimeError: If your broker doesn't have a result backend
            set up.

        Returns:
          int: The total number of results.
        """
        for count, message in enumerate(self.messages, start=1):
            try:
                message.get_result()
            except ResultMissing:
                return count - 1

        return count

    def run(self, *, delay=None):
        """Run this pipeline.

        Parameters:
          delay(int): The minimum amount of time, in milliseconds, the
            pipeline should be delayed by. If both pipeline's delay and
            first message's delay are provided, the bigger value will be
            used.

        Returns:
          pipeline: Itself.
        """
        delay = max(delay or 0, self.messages[0].options.get("delay") or 0) or None
        self.broker.enqueue(self.messages[0], delay=delay)
        return self

    def get_result(self, *, block=False, timeout=None):
        """Get the result of this pipeline.

        Pipeline results are represented by the result of the last
        message in the chain.

        Parameters:
          block(bool): Whether or not to block until a result is set.
          timeout(int): The maximum amount of time, in ms, to wait for
            a result when block is True.  Defaults to 10 seconds.

        Raises:
          ResultMissing: When block is False and the result isn't set.
          ResultTimeout: When waiting for a result times out.

        Returns:
          object: The result.
        """
        last_message = self.messages[-1]

        if isinstance(last_message, (group, pipeline)):
            return last_message.get_result(block=block, timeout=timeout)

        backend = self.broker.get_results_backend()
        return last_message.get_result(backend=backend, block=block, timeout=timeout)

    def get_results(self, *, block=False, timeout=None):
        """Get the results of each job in the pipeline.

        Parameters:
          block(bool): Whether or not to block until a result is set.
          timeout(int): The maximum amount of time, in ms, to wait for
            a result when block is True.  Defaults to 10 seconds.

        Raises:
          ResultMissing: When block is False and the result isn't set.
          ResultTimeout: When waiting for a result times out.

        Returns:
          A result generator.
        """
        deadline = None
        if timeout:
            deadline = time.monotonic() + timeout / 1000

        for message in self.messages:
            if deadline:
                timeout = max(0, int((deadline - time.monotonic()) * 1000))

            if isinstance(message, (group, pipeline)):
                yield message.get_result(block=block, timeout=timeout)

            backend = self.broker.get_results_backend()
            yield message.get_result(backend=backend, block=block, timeout=timeout)


class group:
    """Run a group of actors in parallel.

    Parameters:
      children(Iterator[Message|group|pipeline]): A sequence of
        messages, groups or pipelines.
      broker(Broker): The broker to run the group on.  Defaults to the
        current global broker.
    """

    def __init__(self, children, *, broker=None):
        self.children = list(children)
        self.broker = broker or get_broker()
        self.completion_callbacks = []

    def __len__(self):
        """Returns the size of the group."""
        return len(self.children)

    def __str__(self):  # pragma: no cover
        return "group([%s])" % ", ".join(str(c) for c in self.children)

    def add_completion_callback(self, message):
        """Adds a completion callback to run once every job in this
        group has completed successfully.  Each group may have multiple completion
        callbacks.

        Important:
          If any job in the group does not complete successfully,
          the completion callback will not be run.

        Warning:
          This functionality is dependent upon the optional |GroupCallbacks|
          middleware.  If that's not set up correctly, then calling
          run after adding a callback will raise a RuntimeError.

        Parameters:
          message(Message)
        """
        self.completion_callbacks.append(message.asdict())

    @property
    def completed(self):
        """Returns True when all the jobs in the group have been
        completed.  Actors that don't store results are not counted,
        meaning this may be inaccurate if all or some of your actors
        don't store results.

        Raises:
          RuntimeError: If your broker doesn't have a result backend
            set up.
        """
        return self.completed_count == len(self)

    @property
    def completed_count(self):
        """Returns the total number of jobs that have been completed.
        Actors that don't store results are not counted, meaning this
        may be inaccurate if all or some of your actors don't store
        results.

        Raises:
          RuntimeError: If your broker doesn't have a result backend
            set up.

        Returns:
          int: The total number of results.
        """
        count = 0
        for child in self.children:
            try:
                if isinstance(child, group):
                    child.get_results()
                else:
                    child.get_result()
                count += 1
            except ResultMissing:
                pass

        return count

    def run(self, *, delay=None):
        """Run the actors in this group.

        Parameters:
          delay(int): The minimum amount of time, in milliseconds,
            each message in the group should be delayed by.

        Returns:
          group: This same group.
        """
        if self.completion_callbacks:
            from .middleware.group_callbacks import GroupCallbacks

            for middleware in self.broker.middleware:
                if isinstance(middleware, GroupCallbacks):
                    rate_limiter_backend = middleware.rate_limiter_backend
                    barrier_ttl = middleware.barrier_ttl
                    break
            else:
                raise RuntimeError(
                    "GroupCallbacks middleware not found! Did you forget "
                    "to set it up? It is required if you want to use "
                    "group callbacks."
                )

            # Generate a new completion uuid on every run so that if a
            # group is re-run, the barriers are all separate.
            # Re-using a barrier's name is an unsafe operation.
            completion_uuid = str(uuid4())
            completion_barrier = Barrier(rate_limiter_backend, completion_uuid, ttl=barrier_ttl)
            completion_barrier.create(len(self.children))

            children = []
            for child in self.children:
                if isinstance(child, group):
                    raise NotImplementedError

                elif isinstance(child, pipeline):
                    pipeline_children = child.messages[:]
                    pipeline_children[-1] = pipeline_children[-1].copy(
                        options={
                            "group_completion_uuid": completion_uuid,
                            "group_completion_callbacks": self.completion_callbacks,
                        }
                    )

                    children.append(pipeline(pipeline_children, broker=child.broker))

                else:
                    children.append(
                        child.copy(
                            options={
                                "group_completion_uuid": completion_uuid,
                                "group_completion_callbacks": self.completion_callbacks,
                            }
                        )
                    )
        else:
            children = self.children

        for child in children:
            if isinstance(child, (group, pipeline)):
                child.run(delay=delay)
            else:
                self.broker.enqueue(child, delay=delay)

        return self

    def get_results(self, *, block=False, timeout=None):
        """Get the results of each job in the group.

        Parameters:
          block(bool): Whether or not to block until the results are stored.
          timeout(int): The maximum amount of time, in milliseconds,
            to wait for results when block is True.  Defaults to 10
            seconds.

        Raises:
          ResultMissing: When block is False and the results aren't set.
          ResultTimeout: When waiting for results times out.

        Returns:
          A result generator.
        """
        deadline = None
        if timeout:
            deadline = time.monotonic() + timeout / 1000

        for child in self.children:
            if deadline:
                timeout = max(0, int((deadline - time.monotonic()) * 1000))

            if isinstance(child, group):
                yield list(child.get_results(block=block, timeout=timeout))
            elif isinstance(child, pipeline):
                yield child.get_result(block=block, timeout=timeout)
            else:
                backend = self.broker.get_results_backend()
                yield child.get_result(backend=backend, block=block, timeout=timeout)

    def iter_results(self, *, block: bool = False, timeout: int | None = None) -> Iterator[GroupResult]:
        """Iterate over the group's results in the order they complete.

        Unlike :meth:`get_results`, which yields results in submission
        order and blocks on each one in turn, this polls every job in
        the group and yields each result as soon as it becomes
        available.  A job that finishes ahead of the others is
        therefore yielded immediately instead of being held up by a
        slower job earlier in the group.

        Each yielded :class:`~dramatiq.results.GroupResult` carries the
        ``index_path`` of its job within the group.  Nested groups are
        flattened and each leaf keeps the full path of indices leading
        to it (for example ``(0, 1)`` for the second child of the first
        nested group), while a pipeline child is represented by the
        result of its terminal message.

        Failures, jobs that have not finished yet and jobs whose
        result is genuinely ``None`` are all expressed distinctly via
        the result's :class:`~dramatiq.results.ResultStatus` instead of
        being raised or collapsed into a single value.

        A single shared deadline applies to the whole iteration: when
        ``block`` is True, the total time spent waiting is capped by
        ``timeout`` (in milliseconds, defaulting to 10 seconds).  Any
        jobs still unfinished once the deadline expires are yielded
        once with :attr:`~dramatiq.results.ResultStatus.MISSING`, and
        iteration then stops.  When ``block`` is False, every job is
        inspected exactly once; available results are yielded and the
        rest are yielded as missing.

        Parameters:
          block(bool): Whether or not to keep polling until every job
            has a result or the timeout expires.
          timeout(int): The maximum amount of time, in milliseconds, to
            wait for the whole group's results when block is True.
            Defaults to 10 seconds.  Ignored when block is False.

        Raises:
          RuntimeError: If your broker doesn't have a result backend
            set up.

        Returns:
          An iterator of :class:`~dramatiq.results.GroupResult`
          instances, one per leaf job in the group.
        """
        backend = self.broker.get_results_backend()
        pending = _collect_group_leaves(self.children)

        deadline = None
        if block:
            deadline = time.monotonic() + (timeout or DEFAULT_TIMEOUT) / 1000

        while pending:
            remaining_timeout: int | None = None
            if deadline is not None:
                remaining_timeout = max(0, int((deadline - time.monotonic()) * 1000))
                if remaining_timeout == 0:
                    for index_path, _child in pending:
                        yield GroupResult(index_path=index_path, status=ResultStatus.MISSING)
                    return

            still_pending: list[tuple[tuple[int, ...], Message | pipeline]] = []
            for index_path, child in pending:
                try:
                    value = _get_composition_result(child, backend, block=False, timeout=remaining_timeout)
                except ResultFailure as e:
                    yield GroupResult(index_path=index_path, status=ResultStatus.FAILED, error=e)
                except ResultMissing:
                    still_pending.append((index_path, child))
                else:
                    yield GroupResult(index_path=index_path, status=ResultStatus.COMPLETE, value=value)

            if not still_pending:
                return

            if not block:
                for index_path, _child in still_pending:
                    yield GroupResult(index_path=index_path, status=ResultStatus.MISSING)
                return

            pending = still_pending
            time.sleep(ITER_RESULTS_BACKOFF / 1000)

    def wait(self, *, timeout=None):
        """Block until all the jobs in the group have finished or
        until the timeout expires.

        Parameters:
          timeout(int): The maximum amount of time, in ms, to wait.
            Defaults to 10 seconds.

        Raises:
          ResultTimeout: When waiting times out.
        """
        for _ in self.get_results(block=True, timeout=timeout):  # pragma: no cover
            pass
