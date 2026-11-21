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
from typing import TYPE_CHECKING, Iterable
from uuid import uuid4

from .broker import get_broker
from .common import current_millis
from .rate_limits import Barrier
from .results import ResultMissing

if TYPE_CHECKING:
    from .message import Message


def _copy_message(message: Message, **attributes) -> Message:
    """Copy a message so that it no longer points at the rest of the
    composition it was taken from.

    ``Message.copy`` creates a fresh ``options`` dict, so dropping the
    ``pipe_target`` option here is safe and leaves the source message
    untouched.
    """
    message = message.copy(**attributes)
    message.options.pop("pipe_target", None)
    return message


def _link_messages(messages: list[Message]) -> None:
    """Point every message at the one that follows it.

    The target payload shares the next message's ``options`` dict, so
    setting later links is reflected in earlier payloads.  Callers must
    only ever pass messages that belong to a single composition/run.
    """
    for message, next_message in zip(messages, messages[1:]):
        message.options["pipe_target"] = next_message.asdict()


def _completed(messages: list[Message]) -> bool:
    try:
        messages[-1].get_result()

        return True
    except ResultMissing:
        return False


def _completed_count(messages: list[Message]) -> int:
    count = 0
    for count, message in enumerate(messages, start=1):
        try:
            message.get_result()
        except ResultMissing:
            return count - 1

    return count


def _get_result(broker, messages: list[Message], *, block=False, timeout=None):
    last_message = messages[-1]
    backend = broker.get_results_backend()
    return last_message.get_result(backend=backend, block=block, timeout=timeout)


def _get_results(broker, messages: list[Message], *, block=False, timeout=None):
    deadline = None
    if timeout:
        deadline = time.monotonic() + timeout / 1000

    for message in messages:
        if deadline:
            timeout = max(0, int((deadline - time.monotonic()) * 1000))

        backend = broker.get_results_backend()
        yield message.get_result(backend=backend, block=block, timeout=timeout)


class pipeline:
    """Chain actors together, passing the result of one actor to the
    next one in line.

    A ``pipeline`` is a reusable template: running it derives a fresh
    set of messages (with new message ids and their own ``pipe_target``
    links) so that the same definition can be run any number of times,
    even concurrently, without the runs sharing state.

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
        self._last_run: pipeline_run | None = None
        messages: list[Message]
        self.messages = messages = []

        for child in children:
            if isinstance(child, pipeline):
                messages.extend(_copy_message(message) for message in child.messages)
            elif isinstance(child, pipeline_run):
                raise TypeError(
                    "pipeline_run is a handle for a run that was already enqueued and cannot be composed. "
                    "Compose the pipeline template instead."
                )
            else:
                messages.append(_copy_message(child))

        _link_messages(messages)

    def __len__(self):
        """Returns the length of the pipeline."""
        return len(self.messages)

    def __or__(self, other):
        """Returns a new pipeline with "other" added to the end."""
        return type(self)(self.messages + [other], broker=self.broker)

    def __str__(self):  # pragma: no cover
        return "pipeline([%s])" % ", ".join(str(m) for m in self.messages)

    def _current_messages(self) -> list[Message]:
        """The messages queries should read.

        Before the template is run these are the template messages, so
        the historical "run, then query the same object" usage keeps
        working.  After a run they are the messages of the most recent
        run, whose ids are the ones that were actually enqueued.
        """
        if self._last_run is not None:
            return self._last_run.messages
        return self.messages

    @property
    def last_run(self) -> pipeline_run | None:
        """The handle of the most recent :meth:`run`, or ``None`` if the
        template hasn't been run yet.

        Keep the handle instead of the template when you need to query
        a specific run, since running the template again moves this
        property to the new run.
        """
        return self._last_run

    @property
    def completed(self):
        """Returns True when all the jobs in the pipeline have been
        completed.  This will always return False if the last actor in
        the pipeline doesn't store results.

        Raises:
          RuntimeError: If your broker doesn't have a result backend
            set up.
        """
        return _completed(self._current_messages())

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
        return _completed_count(self._current_messages())

    def _derive_run(self, *, last_message_options=None, first_message_attributes=None) -> pipeline_run:
        """Build a fresh set of messages for a single run.

        Every message gets a new id and timestamp, ``pipe_target``
        links are rebuilt against the new messages and no option dict
        is shared with the template, so a run cannot leak into the
        template or into another run.
        """
        # Imported here to avoid a circular dependency at module load
        # time (message imports pipeline at the top of its module).
        from .message import generate_unique_id

        messages = [
            _copy_message(
                message,
                message_id=generate_unique_id(),
                message_timestamp=current_millis(),
            )
            for message in self.messages
        ]

        if messages and first_message_attributes:
            messages[0] = messages[0].copy(**first_message_attributes)

        if messages and last_message_options:
            messages[-1] = messages[-1].copy(options=last_message_options)

        _link_messages(messages)
        return pipeline_run(self.broker, messages)

    def _run(self, *, delay=None, args=None, kwargs=None, last_message_options=None) -> pipeline_run:
        """Derive, enqueue and record a single run of this template."""
        first_message_attributes = {}
        if args is not None:
            first_message_attributes["args"] = tuple(args)
        if kwargs is not None:
            first_message_attributes["kwargs"] = dict(kwargs)

        run = self._derive_run(
            last_message_options=last_message_options,
            first_message_attributes=first_message_attributes,
        )
        delay = max(delay or 0, run.messages[0].options.get("delay") or 0) or None
        self.broker.enqueue(run.messages[0], delay=delay)
        self._last_run = run
        return run

    def run(self, *, delay=None, args=None, kwargs=None):
        """Run this pipeline.

        Every call derives an independent run: the template messages,
        their options and their ``pipe_target`` links are left
        untouched and the returned handle owns the messages that were
        actually enqueued.  Running the same template twice therefore
        cannot make the two runs share ids, options or downstream
        messages.

        Parameters:
          delay(int): The minimum amount of time, in milliseconds, the
            pipeline should be delayed by. If both pipeline's delay and
            first message's delay are provided, the bigger value will be
            used.
          args(tuple): When given, replaces the positional arguments of
            the first message for this run only.  This is how a service
            that keeps a single template around feeds it different
            inputs.
          kwargs(dict): When given, replaces the keyword arguments of
            the first message for this run only.

        Returns:
          pipeline_run: A handle for this run.
        """
        return self._run(delay=delay, args=args, kwargs=kwargs)

    def get_result(self, *, block=False, timeout=None):
        """Get the result of this pipeline.

        Pipeline results are represented by the result of the last
        message in the chain.  When the pipeline has been run, this
        reads the most recent run.

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
        return _get_result(self.broker, self._current_messages(), block=block, timeout=timeout)

    def get_results(self, *, block=False, timeout=None):
        """Get the results of each job in the pipeline.

        When the pipeline has been run, this reads the most recent run.

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
        yield from _get_results(self.broker, self._current_messages(), block=block, timeout=timeout)


class pipeline_run:
    """The handle returned by :meth:`pipeline.run`.

    It owns the messages of exactly one run, so two handles produced
    from the same template never share message ids, options or
    ``pipe_target`` links.

    Parameters:
      broker(Broker): The broker the run was enqueued on.
      messages(list[Message]): The messages of this run, in order.
    """

    def __init__(self, broker, messages: list[Message]):
        self.broker = broker
        self.messages = messages
        self.run_id = str(uuid4())

    def __len__(self):
        """Returns the length of the run."""
        return len(self.messages)

    def __str__(self):  # pragma: no cover
        return "pipeline_run(%r, [%s])" % (self.run_id, ", ".join(str(m) for m in self.messages))

    @property
    def completed(self):
        """Returns True when all the jobs in this run have been
        completed.  This will always return False if the last actor in
        the pipeline doesn't store results.

        Raises:
          RuntimeError: If your broker doesn't have a result backend
            set up.
        """
        return _completed(self.messages)

    @property
    def completed_count(self):
        """Returns the total number of jobs in this run that have been
        completed.

        Raises:
          RuntimeError: If your broker doesn't have a result backend
            set up.

        Returns:
          int: The total number of results.
        """
        return _completed_count(self.messages)

    def get_result(self, *, block=False, timeout=None):
        """Get the result of this run.

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
        return _get_result(self.broker, self.messages, block=block, timeout=timeout)

    def get_results(self, *, block=False, timeout=None):
        """Get the results of each job in this run.

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
        yield from _get_results(self.broker, self.messages, block=block, timeout=timeout)


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
        self._last_run_children = None

    def _current_children(self):
        """The children queries should read.

        Before the group is run these are the declared children, so
        querying a group that was never run keeps behaving as before.
        After a run they are the handles/messages that run actually
        enqueued, so a re-run does not make the previous run's results
        ambiguous.
        """
        if self._last_run_children is not None:
            return self._last_run_children
        return self.children

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
        for child in self._current_children():
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
        completion_options = None
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

            completion_options = {
                "group_completion_uuid": completion_uuid,
                "group_completion_callbacks": self.completion_callbacks,
            }

        run_children = []
        for child in self.children:
            if isinstance(child, group):
                if completion_options is not None:
                    raise NotImplementedError

                run_children.append(child.run(delay=delay))

            elif isinstance(child, pipeline_run):
                raise NotImplementedError(
                    "A pipeline_run has already been enqueued; pass the pipeline template to the group instead."
                )

            elif isinstance(child, pipeline):
                # Rely on the template to derive the run; when there are
                # completion callbacks, only the derived last message
                # carries the completion options.
                run_children.append(child._run(delay=delay, last_message_options=completion_options))

            else:
                if completion_options is not None:
                    child = child.copy(options=completion_options)

                self.broker.enqueue(child, delay=delay)
                run_children.append(child)

        self._last_run_children = run_children
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

        for child in self._current_children():
            if deadline:
                timeout = max(0, int((deadline - time.monotonic()) * 1000))

            if isinstance(child, group):
                yield list(child.get_results(block=block, timeout=timeout))
            elif isinstance(child, (pipeline, pipeline_run)):
                yield child.get_result(block=block, timeout=timeout)
            else:
                backend = self.broker.get_results_backend()
                yield child.get_result(backend=backend, block=block, timeout=timeout)

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
