# This file is a part of Dramatiq.
#
# Copyright (C) 2017,2018,2019 CLEARTYPE SRL <bogdan@cleartype.io>
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

from typing import TYPE_CHECKING, Any, Callable, Optional

if TYPE_CHECKING:
    from ..actor import Actor
    from ..broker import Broker, MessageProxy
    from ..message import Message
    from ..worker import ConsumerThread, Worker, WorkerThread


class MiddlewareError(Exception):
    """Base class for middleware errors."""


#: Sentinel used to tell apart a skip that carries an explicit result
#: (which may itself be ``None``) from a legacy skip that doesn't.
_UNSET = object()


class SkipMessage(MiddlewareError):
    """An exception that may be raised by Middleware inside the
    ``before_process_message`` hook in order to skip a message.

    There are three ways to use this exception:

    * Raising it without an explicit result (the legacy form, e.g.
      ``raise SkipMessage()``) keeps the historical behaviour: the
      actor is not run, the message is acknowledged and, when the
      :class:`~dramatiq.results.Results` middleware is enabled,
      ``None`` is stored as its result.  No callbacks are emitted.
    * Passing an explicit ``result`` marks the skip as an intentional,
      successful terminal state ("the work is already done, nothing
      left to do").  The message is acknowledged, the result is stored
      and the actor's ``on_success`` callback is invoked with it, just
      as if the actor had returned that value.
    * Passing ``fail=True`` marks the skip as a failure ("the required
      resources aren't available, this message counts as failed").  The
      message is rejected (nacked/dead-lettered), the exception is
      stored as the result and the actor's ``on_failure`` callback is
      invoked, just like a message whose actor raised an exception.

    Parameters:
      message(str): An optional human-readable reason for the skip.
      result(Any): The explicit result of the skip.  When provided,
        the skip is treated as a successful terminal state and the
        value flows through callbacks and result backends exactly like
        an actor return value.  Defaults to an unset sentinel so that
        ``SkipMessage(result=None)`` can be distinguished from the
        legacy ``SkipMessage()`` form.
      fail(bool): When ``True``, the skipped message is treated as a
        failure and rejected instead of acknowledged.  Defaults to
        ``False``.
    """

    def __init__(self, message: str = "", *, result: Any = _UNSET, fail: bool = False):  # noqa: B042
        super().__init__(message)
        self.result = result
        self.fail = fail

    @property
    def has_result(self) -> bool:
        """Whether this skip carries an explicit result."""
        return self.result is not _UNSET

    @property
    def explicit(self) -> bool:
        """Whether this skip declares an explicit terminal state.

        Legacy skips (no explicit result, not marked as failed) stay
        fully backwards compatible, while explicit skips align with the
        callback, Results and ack/nack terminal states of ordinary
        messages.
        """
        return self.fail or self.has_result


class Middleware:
    """Base class for broker middleware.  The default implementations
    for all hooks are no-ops and subclasses may implement whatever
    subset of hooks they like.
    """

    @property
    def actor_options(self) -> set[str]:
        """The set of options that may be configured on each actor."""
        return set()

    @property
    def forks(self) -> list[Callable[[], int]]:
        """A list of functions to run in separate forks of the main
        process.
        """
        return []

    def before_ack(self, broker: Broker, message: MessageProxy) -> None:
        """Called before a message is acknowledged."""

    def after_ack(self, broker: Broker, message: MessageProxy) -> None:
        """Called after a message has been acknowledged."""

    def before_nack(self, broker: Broker, message: MessageProxy) -> None:
        """Called before a message is rejected."""

    def after_nack(self, broker: Broker, message: MessageProxy) -> None:
        """Called after a message has been rejected."""

    def before_declare_actor(self, broker: Broker, actor: Actor) -> None:
        """Called before an actor is declared."""

    def after_declare_actor(self, broker: Broker, actor: Actor) -> None:
        """Called after an actor has been declared."""

    def before_declare_queue(self, broker: Broker, queue_name: str) -> None:
        """Called before a queue is declared."""

    def after_declare_queue(self, broker: Broker, queue_name: str) -> None:
        """Called after a queue has been declared.

        This signals that the queue has been registered with the
        broker, but it does not necessarily mean that it was created
        on the server yet.  For example, the RabbitMQ broker declares
        queues when actors are created, but it doesn't instantiate
        them until messages are enqueued or consumed.
        """

    def after_declare_delay_queue(self, broker: Broker, queue_name: str) -> None:
        """Called after a delay queue has been declared."""

    def before_enqueue(self, broker: Broker, message: Message, delay: int) -> None:
        """Called before a message is enqueued (including retries)."""

    def after_enqueue(self, broker: Broker, message: Message, delay: int) -> None:
        """Called after a message has been enqueued (including retries)."""

    def before_delay_message(self, broker: Broker, message: MessageProxy) -> None:
        """Called before a message has been delayed in worker memory."""

    def before_process_message(self, broker: Broker, message: MessageProxy) -> None:
        """Called before a message is processed.

        Raises:
          SkipMessage: If the current message should be skipped.  When
            this is raised, ``after_skip_message`` is emitted instead
            of ``after_process_message``.
        """

    def after_process_message(
        self,
        broker: Broker,
        message: MessageProxy,
        *,
        result: Optional[Any] = None,
        exception: Optional[BaseException] = None,
    ) -> None:
        """Called after a message has been processed."""

    def after_skip_message(
        self,
        broker: Broker,
        message: MessageProxy,
        *,
        result: Any = _UNSET,
        skip: Optional[SkipMessage] = None,
    ) -> None:
        """Called instead of ``after_process_message`` after a message
        has been skipped.

        Parameters:
          broker(Broker): The current broker.
          message(MessageProxy): The message that was skipped.
          result(Any): The explicit result carried by the skip.  This
            is an unset sentinel for legacy skips (those raised without
            an explicit result); it may be ``None`` when a skip was
            raised explicitly with ``result=None``.
          skip(Optional[SkipMessage]): The ``SkipMessage`` exception
            that was raised, if any.
        """

    def after_process_boot(self, broker: Broker) -> None:
        """Called immediately after subprocess start up."""

    def before_worker_boot(self, broker: Broker, worker: Worker) -> None:
        """Called before the worker process starts up."""

    def after_worker_boot(self, broker: Broker, worker: Worker) -> None:
        """Called after the worker process has started up."""

    def before_worker_shutdown(self, broker: Broker, worker: Worker) -> None:
        """Called before the worker process shuts down."""

    def after_worker_shutdown(self, broker: Broker, worker: Worker) -> None:
        """Called after the worker process shuts down."""

    def after_consumer_thread_boot(self, broker: Broker, thread: ConsumerThread) -> None:
        """Called from a consumer thread after it starts but before it starts its run loop."""

    def before_consumer_thread_shutdown(self, broker: Broker, thread: ConsumerThread) -> None:
        """Called before a consumer thread shuts down.  This may be
        used to clean up thread-local resources (such as Django
        database connections).
        """

    def after_worker_thread_boot(self, broker: Broker, thread: WorkerThread) -> None:
        """Called from a worker thread after it starts but before it starts its run loop."""

    def before_worker_thread_shutdown(self, broker: Broker, thread: WorkerThread) -> None:
        """Called before a worker thread shuts down.  This may be used
        to clean up thread-local resources (such as Django database
        connections).
        """
