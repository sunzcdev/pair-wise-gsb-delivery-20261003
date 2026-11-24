import asyncio
import contextlib
import math
import sys
from collections.abc import AsyncGenerator
from logging import getLogger
from typing import TYPE_CHECKING, Any, NoReturn

from pydantic import Field

from taskiq.abc.broker import AckableMessage, AsyncBroker
from taskiq.message import BrokerMessage
from taskiq.utils import merge_async_iterables

if sys.version_info >= (3, 11):
    from typing import Self
else:
    from typing_extensions import Self

if TYPE_CHECKING:  # pragma: no cover
    from taskiq.abc.result_backend import AsyncResultBackend

logger = getLogger(__name__)


def _task_registration_disabled(*args: Any, **kwargs: Any) -> NoReturn:
    """Replace the `task` method of brokers managed by a MultiBroker."""
    raise ValueError(
        "Cannot register tasks on brokers managed by a MultiBroker. "
        "Register tasks on the MultiBroker itself.",
    )


class SourcedMessage(AckableMessage):
    """
    Ackable message tagged with its actual source and listen round.

    Every ackable delivery that passes through a MultiBroker is wrapped
    in this model, so a worker can always tell which inner broker and
    which invocation of its `listen` produced the message.

    The `ack` callable is the one captured from the original delivery,
    it is never replaced when the source broker reopens its listener.
    A message taken before a disconnect is therefore acknowledged on
    the listen round it came from, never on a later one.

    :param source: routing label (queue name) of the inner broker
        that produced the message.
    :param listen_round: 1-based number of the listener invocation
        that produced the message on the source broker. It grows by
        one every time the broker reopens its listener.
    """

    source: str
    listen_round: int = Field(gt=0)


class MultiBroker(AsyncBroker):
    """
    Broker that combines multiple brokers into one.

    It wraps several real brokers and allows a single worker
    to consume messages from all of them at the same time.
    Every inner broker keeps its own connection and queue,
    this broker only merges the listening and routes kicks.

    If an inner broker fails, the other brokers keep delivering
    messages. The failed broker is reconnected independently,
    on its own retry rhythm: its listener is reopened repeatedly
    until it succeeds, each attempt opening a new listen round.
    The merged queue is bounded and applies backpressure to every
    source once it fills up.

    Ackable deliveries are wrapped in `SourcedMessage`, which keeps
    the routing label of the source broker and the listen round
    the message came from. The acknowledgement stays bound to the
    original delivery, so a message taken before a disconnect is
    never acknowledged through a later listener of the same broker.

    Tasks are registered on the MultiBroker itself and routed
    to inner brokers with the routing label:

    >>> fast_broker = AioPikaBroker(url, queue_name="fast")
    >>> slow_broker = AioPikaBroker(url, queue_name="slow")
    >>> broker = MultiBroker(
    ...     routing={"fast": fast_broker, "slow": slow_broker},
    ...     default_routing_label="fast",
    ... )

    >>> @broker.task(labels={"queue_name": "slow"})
    ... async def my_task() -> None: ...

    A single `taskiq worker` command consumes messages
    from all inner brokers this way.

    :param routing: mapping of routing label values to brokers,
        at least one entry is required. Brokers are deduplicated,
        every unique broker is listened to once.
    :param default_routing_label: label used to kick messages
        without the routing label, must be a key of `routing`.
        If it's not set, unlabeled messages are rejected.
    :param routing_label: message label used for routing,
        defaults to `queue_name`.
    :param max_buffer_size: maximum number of messages buffered
        in the merged queue while the worker is busy, must be
        positive. Defaults to an unbounded buffer.
    :param reconnect_delay: seconds to wait before reopening
        the listener of a failed inner broker, must not be
        negative. Every broker waits independently.
    :raises ValueError: if `routing` is empty or has values
        of a wrong type, if `default_routing_label`
        has no mapped broker, or if the buffer or delay
        parameters have invalid values.
    """

    def __init__(
        self,
        routing: "dict[str, AsyncBroker] | None" = None,
        default_routing_label: str | None = None,
        routing_label: str = "queue_name",
        max_buffer_size: float = math.inf,
        reconnect_delay: float = 1.0,
    ) -> None:
        if not routing:
            raise ValueError(
                "MultiBroker requires a non-empty routing mapping "
                "of queue names to brokers.",
            )
        for queue_name, inner_broker in routing.items():
            if not isinstance(inner_broker, AsyncBroker):
                raise ValueError(
                    "All routed brokers must be AsyncBroker instances, "
                    f"got {type(inner_broker).__name__} for queue '{queue_name}'.",
                )
        if default_routing_label is not None and default_routing_label not in routing:
            raise ValueError(
                "There is no broker mapped for the default queue "
                f"'{default_routing_label}'. Known queues: {sorted(routing)}.",
            )
        if max_buffer_size <= 0:
            raise ValueError("max_buffer_size must be positive.")
        if reconnect_delay < 0:
            raise ValueError("reconnect_delay must not be negative.")
        # Inner brokers must be saved before calling super().__init__(),
        # since it sets the is_worker_process flag,
        # which propagates to all inner brokers.
        # Several labels can point to the same broker,
        # so brokers are deduplicated to listen to each of them once.
        self.brokers: list[AsyncBroker] = list(dict.fromkeys(routing.values()))
        self.routing: dict[str, AsyncBroker] = dict(routing)
        # The first label mapped to a broker names its source,
        # since deduplicated brokers are listened to once.
        self._broker_sources: dict[int, str] = {}
        for source, inner_broker in self.routing.items():
            self._broker_sources.setdefault(id(inner_broker), source)
        self.default_routing_label = default_routing_label
        self.routing_label = routing_label
        self.max_buffer_size = max_buffer_size
        self.reconnect_delay = reconnect_delay
        for inner_broker in self.brokers:
            # Tasks must live in the registry of this broker,
            # so accidental registration on inner brokers is loud.
            inner_broker.task = (  # type: ignore[method-assign]
                _task_registration_disabled
            )
        super().__init__()

    @property
    def is_worker_process(self) -> bool:
        """True only if broker runs in worker process."""
        return self._is_worker_process

    @is_worker_process.setter
    def is_worker_process(self, value: bool) -> None:
        self._is_worker_process = value
        for inner_broker in self.brokers:
            inner_broker.is_worker_process = value

    @property
    def is_scheduler_process(self) -> bool:
        """True only if broker runs in scheduler process."""
        return self._is_scheduler_process

    @is_scheduler_process.setter
    def is_scheduler_process(self, value: bool) -> None:
        self._is_scheduler_process = value
        for inner_broker in self.brokers:
            inner_broker.is_scheduler_process = value

    async def kick(self, message: BrokerMessage) -> None:
        """
        Kick a message using one of the inner brokers.

        The broker is selected by the routing label of the message.
        Messages without the label go to the broker mapped
        to the default routing label.

        :param message: message to send.
        :raises ValueError: if the message has no routing label
            and no default is configured, or the label value
            has no mapped broker.
        """
        label_value = message.labels.get(self.routing_label)
        if label_value is None:
            if self.default_routing_label is None:
                raise ValueError(
                    f"Message has no '{self.routing_label}' label and the "
                    "MultiBroker has no default_routing_label, cannot route it.",
                )
            label_value = self.default_routing_label
        target_broker = self.routing.get(label_value)
        if target_broker is None:
            raise ValueError(
                f"There is no broker mapped for queue '{label_value}'. "
                f"Known queues: {sorted(self.routing)}.",
            )
        await target_broker.kick(message)

    async def listen(self) -> AsyncGenerator["bytes | SourcedMessage", None]:
        """
        Listen to all inner brokers simultaneously.

        This method merges listen generators of all inner brokers
        into a single generator, so the worker receives messages
        from every broker at the same time. The merged queue holds
        at most `max_buffer_size` pending messages and blocks the
        sources when the worker cannot keep up.

        A failing inner broker does not stop the other brokers.
        Its listener is reopened independently in a new listen
        round after `reconnect_delay`, while the remaining brokers
        keep delivering. Only shutting the whole generator down
        ends the listening.

        Ackable messages are yielded as `SourcedMessage`, carrying
        their source label and listen round and keeping the original
        acknowledgement callable. Plain bytes messages pass through
        unchanged, since they carry nothing to acknowledge.

        :yield: incoming messages from all inner brokers.
        :return: nothing.
        """
        merged = merge_async_iterables(
            *(self._listen_one(inner_broker) for inner_broker in self.brokers),
            max_buffer_size=self.max_buffer_size,
        )
        async for message in merged:
            yield message

    async def _listen_one(
        self,
        inner_broker: AsyncBroker,
    ) -> AsyncGenerator["bytes | SourcedMessage", None]:
        """
        Listen to a single inner broker, reopening it on failure.

        Every invocation of the inner broker's `listen` is one
        listen round. When the listener raises or ends unexpectedly,
        the error is logged and the broker is reopened after
        `reconnect_delay`, starting the next round. Other inner
        brokers are unaffected because every one runs this loop
        independently.

        Messages already delivered keep the round they came from:
        the ack callable is captured from the original delivery and
        a reopened listener never replaces it.

        :param inner_broker: broker to listen to.
        :yield: source-tagged incoming messages from the inner broker.
        :return: nothing.
        """
        source = self._broker_sources[id(inner_broker)]
        listen_round = 0
        while True:
            listen_round += 1
            iterator = inner_broker.listen()
            try:
                async for message in iterator:
                    yield self._tag_message(message, source, listen_round)
            except asyncio.CancelledError:
                # The whole merged generator is shutting down.
                raise
            except Exception as exc:
                logger.exception(
                    "Inner broker %s (source %r) failed on listen round %d: "
                    "%r. Reopening its listener in %.2f seconds, "
                    "other brokers keep listening.",
                    inner_broker,
                    source,
                    listen_round,
                    exc,
                    self.reconnect_delay,
                )
            else:
                logger.warning(
                    "Inner broker %s (source %r) ended its listen round %d "
                    "without failing. Reopening its listener in %.2f seconds.",
                    inner_broker,
                    source,
                    listen_round,
                    self.reconnect_delay,
                )
            finally:
                # Catches failures, unexpected endings and outer shutdown:
                # a suspended generator must run its cleanup logic.
                with contextlib.suppress(RuntimeError):
                    await iterator.aclose()
            await asyncio.sleep(self.reconnect_delay)

    @staticmethod
    def _tag_message(
        message: "bytes | AckableMessage",
        source: str,
        listen_round: int,
    ) -> "bytes | SourcedMessage":
        """
        Tag an inner delivery with its source and listen round.

        Plain bytes messages have no acknowledgement to preserve,
        so they pass through unchanged. Ackable messages are wrapped
        in `SourcedMessage` without replacing their ack callable.

        :param message: message yielded by the inner broker.
        :param source: routing label of the inner broker.
        :param listen_round: listen round the message came from.
        :return: the original bytes or a source-tagged wrapper.
        """
        if not isinstance(message, AckableMessage):
            return message
        if isinstance(message, SourcedMessage):
            return message
        return SourcedMessage(
            data=message.data,
            ack=message.ack,
            source=source,
            listen_round=listen_round,
        )

    async def startup(self) -> None:
        """
        Startup the composite broker and all inner brokers.

        Event handlers and middlewares of this broker are called
        first, then every inner broker is started.
        """
        await super().startup()
        for inner_broker in self.brokers:
            await inner_broker.startup()

    async def shutdown(self) -> None:
        """
        Shut down all inner brokers and the composite broker.

        Exceptions raised by inner brokers are logged
        and don't prevent other brokers from shutting down.
        """
        for inner_broker in self.brokers:
            try:
                await inner_broker.shutdown()
            except Exception as exc:
                logger.warning(
                    "Cannot shutdown broker %s: %s",
                    inner_broker,
                    exc,
                    exc_info=True,
                )
        await super().shutdown()

    def with_result_backend(
        self,
        result_backend: "AsyncResultBackend[Any]",
    ) -> Self:
        """
        Set a result backend for the composite and all inner brokers.

        Inner brokers use their result backend to serve
        `wait_result` calls on the client side, while the worker
        saves results using the result backend of this broker.
        Setting the same backend everywhere keeps them in sync.

        :param result_backend: new result backend.
        :return: self
        """
        self.result_backend = result_backend
        for inner_broker in self.brokers:
            inner_broker.result_backend = result_backend
        return self
