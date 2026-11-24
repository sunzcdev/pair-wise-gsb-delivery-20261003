import asyncio
import json
from collections.abc import AsyncGenerator

import pytest

from taskiq import Context, MultiBroker, SimpleRetryMiddleware, TaskiqDepends
from taskiq.abc.serializer import TaskiqSerializer
from taskiq.acks import AckableMessage
from taskiq.brokers.inmemory_broker import InmemoryResultBackend
from taskiq.events import TaskiqEvents
from taskiq.message import BrokerMessage, TaskiqMessage
from taskiq.receiver import Receiver
from tests.utils import AsyncQueueBroker


def build_message(labels: dict[str, str] | None = None) -> BrokerMessage:
    """Build a minimal broker message for kick tests."""
    return BrokerMessage(
        task_id="task-id",
        task_name="tests:test_task",
        message=b"payload",
        labels=labels or {},
    )


def build_serialized_message(
    broker: MultiBroker,
    labels: dict[str, object] | None = None,
    labels_types: dict[str, int] | None = None,
) -> BrokerMessage:
    """Build a message whose payload is a real serialized TaskiqMessage."""
    labels = labels or {}
    return broker.formatter.dumps(
        TaskiqMessage(
            task_id="task-id",
            task_name="tests:test_task",
            labels=labels,
            labels_types=labels_types,
            args=[],
            kwargs={},
        ),
    )


def wire_labels(inner: AsyncQueueBroker) -> dict[str, object]:
    """Decode the payload that actually reached an inner broker."""
    return inner.formatter.loads(inner.queue.get_nowait()).labels


def test_multi_broker_requires_non_empty_routing() -> None:
    with pytest.raises(ValueError, match="non-empty routing"):
        MultiBroker()

    with pytest.raises(ValueError, match="non-empty routing"):
        MultiBroker(routing={})


def test_multi_broker_validates_routed_broker_types() -> None:
    with pytest.raises(ValueError, match="AsyncBroker"):
        MultiBroker(routing={"fast": 42})  # type: ignore[dict-item]


def test_multi_broker_validates_default_routing_label() -> None:
    with pytest.raises(ValueError, match="no broker mapped"):
        MultiBroker(
            routing={"fast": AsyncQueueBroker()},
            default_routing_label="slow",
        )


def test_task_registration_on_inner_broker_raises() -> None:
    first = AsyncQueueBroker()
    MultiBroker(routing={"first": first})

    with pytest.raises(ValueError, match="MultiBroker itself"):

        @first.task
        async def inner_task() -> None: ...

    with pytest.raises(ValueError, match="MultiBroker itself"):
        first.task()


async def test_kick_routes_by_queue_name_label() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(routing={"first": first, "second": second})

    await broker.kick(build_message(labels={"queue_name": "second"}))

    assert first.queue.qsize() == 0
    assert second.queue.qsize() == 1


async def test_kick_without_label_uses_default_routing_label() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first, "second": second},
        default_routing_label="second",
    )

    await broker.kick(build_message())

    assert first.queue.qsize() == 0
    assert second.queue.qsize() == 1


async def test_kick_without_label_and_default_raises() -> None:
    broker = MultiBroker(routing={"first": AsyncQueueBroker()})

    with pytest.raises(ValueError, match="default_routing_label"):
        await broker.kick(build_message())


async def test_kick_with_unknown_queue_name_raises() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(routing={"first": first, "second": second})

    with pytest.raises(ValueError, match="no broker mapped for queue 'fast'"):
        await broker.kick(build_message(labels={"queue_name": "fast"}))


async def test_kick_uses_custom_routing_label() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first, "emails": second},
        default_routing_label="first",
        routing_label="topic",
    )

    await broker.kick(build_message(labels={"topic": "emails"}))

    assert first.queue.qsize() == 0
    assert second.queue.qsize() == 1


async def test_same_broker_under_two_labels_is_listened_once() -> None:
    class ListenCountingBroker(AsyncQueueBroker):
        def __init__(self) -> None:
            super().__init__()
            self.listen_calls = 0

        async def listen(self) -> AsyncGenerator[AckableMessage, None]:
            self.listen_calls += 1
            async for message in super().listen():
                yield message

    shared, other = ListenCountingBroker(), AsyncQueueBroker()
    broker = MultiBroker(
        routing={"a": shared, "b": shared, "other": other},
        default_routing_label="a",
    )
    await other.kick(build_message())

    iterator = broker.listen()
    await asyncio.wait_for(iterator.__anext__(), timeout=1)
    await iterator.aclose()

    assert shared.listen_calls == 1


async def test_listen_yields_messages_from_all_brokers() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(routing={"first": first, "second": second})
    await first.kick(build_message())
    await second.kick(build_message())
    await second.kick(build_message())

    received = 0
    iterator = broker.listen()
    for _ in range(3):
        await asyncio.wait_for(iterator.__anext__(), timeout=1)
        received += 1
    await iterator.aclose()

    assert received == 3
    assert first.queue.qsize() == 0
    assert second.queue.qsize() == 0


async def test_listen_with_single_broker() -> None:
    first = AsyncQueueBroker()
    broker = MultiBroker(routing={"first": first})
    await first.kick(build_message())

    iterator = broker.listen()
    await asyncio.wait_for(iterator.__anext__(), timeout=1)
    await iterator.aclose()

    assert first.queue.qsize() == 0


def test_process_flags_propagate_to_inner_brokers() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(routing={"first": first, "second": second})

    broker.is_worker_process = True
    assert first.is_worker_process is True
    assert second.is_worker_process is True

    broker.is_worker_process = False
    assert first.is_worker_process is False

    broker.is_scheduler_process = True
    assert first.is_scheduler_process is True
    assert second.is_scheduler_process is True


async def test_startup_delegates_to_inner_brokers() -> None:
    started: list[str] = []
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    first.add_event_handler(
        TaskiqEvents.WORKER_STARTUP,
        lambda _: started.append("first"),
    )
    second.add_event_handler(
        TaskiqEvents.WORKER_STARTUP,
        lambda _: started.append("second"),
    )
    broker = MultiBroker(routing={"first": first, "second": second})
    broker.add_event_handler(
        TaskiqEvents.WORKER_STARTUP,
        lambda _: started.append("composite"),
    )
    broker.is_worker_process = True

    await broker.startup()

    assert sorted(started) == ["composite", "first", "second"]


async def test_shutdown_delegates_to_inner_brokers() -> None:
    shut_down: list[str] = []
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    first.add_event_handler(
        TaskiqEvents.WORKER_SHUTDOWN,
        lambda _: shut_down.append("first"),
    )
    second.add_event_handler(
        TaskiqEvents.WORKER_SHUTDOWN,
        lambda _: shut_down.append("second"),
    )
    broker = MultiBroker(routing={"first": first, "second": second})
    broker.add_event_handler(
        TaskiqEvents.WORKER_SHUTDOWN,
        lambda _: shut_down.append("composite"),
    )
    broker.is_worker_process = True

    await broker.shutdown()

    assert sorted(shut_down) == ["composite", "first", "second"]


async def test_shutdown_tolerates_failing_inner_brokers() -> None:
    shut_down: list[str] = []

    class FailingBroker(AsyncQueueBroker):
        async def shutdown(self) -> None:
            raise RuntimeError("shutdown failed")

    failing = FailingBroker()
    ok_broker = AsyncQueueBroker()
    ok_broker.add_event_handler(
        TaskiqEvents.CLIENT_SHUTDOWN,
        lambda _: shut_down.append("ok"),
    )
    broker = MultiBroker(routing={"bad": failing, "ok": ok_broker})
    broker.add_event_handler(
        TaskiqEvents.CLIENT_SHUTDOWN,
        lambda _: shut_down.append("composite"),
    )

    await broker.shutdown()

    assert "ok" in shut_down
    assert "composite" in shut_down


async def test_find_task_finds_tasks_registered_on_composite() -> None:
    broker = MultiBroker(routing={"first": AsyncQueueBroker()})

    @broker.task
    async def composite_task() -> None: ...

    assert broker.find_task(composite_task.task_name) is composite_task


async def test_get_all_tasks_returns_tasks_registered_on_composite() -> None:
    broker = MultiBroker(routing={"first": AsyncQueueBroker()})

    @broker.task
    async def composite_task() -> None: ...

    assert broker.get_all_tasks()[composite_task.task_name] is composite_task


async def test_with_result_backend_propagates_to_inner_brokers() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(routing={"first": first, "second": second})
    backend = InmemoryResultBackend[None]()

    broker.with_result_backend(backend)

    assert broker.result_backend is backend
    assert first.result_backend is backend
    assert second.result_backend is backend


async def test_receiver_processes_tasks_from_all_brokers() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first, "second": second},
        default_routing_label="first",
    )
    broker.with_result_backend(InmemoryResultBackend())
    executed: list[str] = []

    @broker.task(labels={"queue_name": "first"})
    async def task_one() -> str:
        executed.append("one")
        return "one"

    @broker.task(labels={"queue_name": "second"})
    async def task_two() -> str:
        executed.append("two")
        return "two"

    first_handle = await task_one.kiq()
    second_handle = await task_two.kiq()

    receiver = Receiver(broker=broker)
    finish_event = asyncio.Event()
    listener = asyncio.create_task(receiver.listen(finish_event))

    first_result = await first_handle.wait_result(timeout=5)
    second_result = await second_handle.wait_result(timeout=5)

    assert first_result.return_value == "one"
    assert second_result.return_value == "two"
    assert sorted(executed) == ["one", "two"]

    finish_event.set()
    await asyncio.wait_for(listener, timeout=5)
    await broker.shutdown()


class DyingBroker(AsyncQueueBroker):
    """Broker that yields N messages and then fails like a dead cluster."""

    def __init__(self, messages_before_death: int) -> None:
        super().__init__()
        self.messages_before_death = messages_before_death

    async def listen(self) -> AsyncGenerator[AckableMessage, None]:
        while self.messages_before_death > 0:
            self.messages_before_death -= 1
            data = await self.queue.get()
            yield AckableMessage(data=data, ack=self.queue.task_done)
        raise RuntimeError("cluster is down")


async def test_failed_broker_does_not_stop_healthy_brokers(
    caplog: pytest.LogCaptureFixture,
) -> None:
    dying = DyingBroker(messages_before_death=2)
    healthy = AsyncQueueBroker()
    broker = MultiBroker(routing={"dying": dying, "healthy": healthy})

    for _ in range(2):
        await dying.kick(build_message())
    for _ in range(3):
        await healthy.kick(build_message())

    received = 0
    iterator = broker.listen()
    for _ in range(5):
        await asyncio.wait_for(iterator.__anext__(), timeout=1)
        received += 1
    await iterator.aclose()

    assert received == 5
    assert "cluster is down" in caplog.text


async def test_listen_ends_when_all_inner_brokers_fail() -> None:
    broker = MultiBroker(
        routing={
            "first": DyingBroker(messages_before_death=0),
            "second": DyingBroker(messages_before_death=0),
        },
    )

    with pytest.raises(StopAsyncIteration):
        await asyncio.wait_for(broker.listen().__anext__(), timeout=1)


class _MarkedSerializer(TaskiqSerializer):
    """Serializer that prefixes payloads so foreign encodings are detectable."""

    def dumpb(self, value: object) -> bytes:
        return b"MARK:" + json.dumps(value).encode()

    def loadb(self, value: bytes) -> object:
        return json.loads(value.removeprefix(b"MARK:"))


class _MarkedBroker(AsyncQueueBroker):
    """Inner broker whose serializer differs from the composite's one."""

    def __init__(self) -> None:
        super().__init__()
        self.serializer = _MarkedSerializer()


async def test_routing_label_is_routing_only_and_never_reaches_worker() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(routing={"first": first, "second": second})
    message = build_serialized_message(
        broker,
        labels={"queue_name": "second", "tenant": "acme"},
    )

    await broker.kick(message)

    assert first.queue.qsize() == 0
    assert second.queue.qsize() == 1
    assert wire_labels(second) == {"tenant": "acme"}


async def test_routing_label_type_is_stripped_from_payload_too() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(routing={"first": first, "second": second})
    message = build_serialized_message(
        broker,
        labels={"queue_name": "second", "tenant": "acme"},
        labels_types={"queue_name": 3, "tenant": 2},
    )

    await broker.kick(message)

    decoded = second.formatter.loads(second.queue.get_nowait())
    assert decoded.labels == {"tenant": "acme"}
    assert decoded.labels_types == {"tenant": 2}


async def test_business_labels_are_forwarded_untouched() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first, "second": second},
        default_routing_label="first",
    )
    message = build_serialized_message(
        broker,
        labels={"tenant": "acme", "priority": "5", "retry": "true"},
    )

    await broker.kick(message)

    assert wire_labels(first) == {
        "tenant": "acme",
        "priority": "5",
        "retry": "true",
    }


async def test_kick_does_not_mutate_incoming_message() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(routing={"first": first, "second": second})
    message = build_serialized_message(
        broker,
        labels={"queue_name": "second", "tenant": "acme"},
    )
    original_labels = dict(message.labels)

    await broker.kick(message)

    # The routing option must not be written back into the handled message.
    assert message.labels == original_labels
    assert "queue_name" in message.labels
    # And its payload stays untouched as well.
    decoded_original = broker.formatter.loads(message.message)
    assert decoded_original.labels["queue_name"] == "second"


async def test_message_without_routing_label_is_forwarded_as_is() -> None:
    first = AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first},
        default_routing_label="first",
    )
    message = build_serialized_message(broker, labels={"tenant": "acme"})
    original_message = message.message

    await broker.kick(message)

    assert first.queue.qsize() == 1
    assert wire_labels(first) == {"tenant": "acme"}
    # No routing label means the payload bytes are passed through unchanged.
    assert message.message == original_message


async def test_routing_label_stripping_works_across_serializers() -> None:
    inner = _MarkedBroker()
    broker = MultiBroker(routing={"only": inner}, default_routing_label="only")
    # Payload is produced with the composite's default JSON serializer,
    # while the inner broker decodes with a different serializer.
    message = build_serialized_message(
        broker,
        labels={"queue_name": "only", "tenant": "acme"},
    )

    await broker.kick(message)

    raw = inner.queue.get_nowait()
    assert raw.startswith(b"MARK:")
    assert inner.formatter.loads(raw).labels == {"tenant": "acme"}


async def test_custom_routing_label_is_routing_only() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first, "emails": second},
        default_routing_label="first",
        routing_label="topic",
    )
    message = build_serialized_message(
        broker,
        labels={"topic": "emails", "tenant": "acme"},
    )

    await broker.kick(message)

    assert second.queue.qsize() == 1
    assert wire_labels(second) == {"tenant": "acme"}


async def test_undecodable_payload_keeps_bytes_but_drops_outer_routing_label(
    caplog: pytest.LogCaptureFixture,
) -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(routing={"first": first, "second": second})
    # Hand-crafted message whose payload is not a serialized TaskiqMessage.
    message = build_message(labels={"queue_name": "second", "tenant": "acme"})
    original_envelope = dict(message.labels)

    await broker.kick(message)

    # The caller's message is untouched, including its routing label.
    assert message.labels == original_envelope
    assert "queue_name" in message.labels
    # The opaque payload is forwarded unchanged with a warning.
    assert second.queue.qsize() == 1
    assert second.queue.get_nowait() == b"payload"
    assert "Cannot decode payload" in caplog.text


async def test_same_template_kicked_to_two_brokers_with_overrides() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first, "second": second},
        default_routing_label="first",
    )

    @broker.task(queue_name="first", tenant="acme", priority=5)
    async def routed_task() -> None: ...

    template_before = dict(routed_task.labels)

    # Broker A: override the routing label for this single send.
    await routed_task.kicker().with_labels(queue_name="second").kiq()
    # Broker B: revoke an inherited business label, keep the template route.
    await routed_task.kicker().without_labels("tenant").kiq()

    assert first.queue.qsize() == 1
    assert second.queue.qsize() == 1

    second_labels = wire_labels(second)
    first_labels = wire_labels(first)

    # Routing label is never visible to the actual worker.
    assert "queue_name" not in second_labels
    assert "queue_name" not in first_labels
    # Broker A keeps every business label.
    assert second_labels == {"tenant": "acme", "priority": "5"}
    # Broker B lost only the revoked business label.
    assert first_labels == {"priority": "5"}

    # Neither kick wrote anything back into the registered template.
    assert routed_task.labels == template_before


async def test_template_is_unchanged_after_overrides_and_revocations() -> None:
    first = AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first},
        default_routing_label="first",
    )

    @broker.task(queue_name="first", tenant="acme", priority=5)
    async def routed_task() -> None: ...

    original = dict(routed_task.labels)

    routed_task.kicker().with_labels(queue_name="first", tenant="override")
    routed_task.kicker().without_labels("priority", "tenant")

    # Kicker mutations never leak back, even without sending.
    assert routed_task.labels == original


async def test_chained_override_and_revocation_on_one_kick() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first, "second": second},
        default_routing_label="first",
    )

    @broker.task(queue_name="second", tenant="acme", priority=5, env="prod")
    async def routed_task() -> None: ...

    await (
        routed_task.kicker()
        .with_labels(queue_name="first")
        .without_labels("tenant", "env")
        .kiq()
    )

    assert first.queue.qsize() == 1
    assert second.queue.qsize() == 0
    assert wire_labels(first) == {"priority": "5"}


async def test_revoking_routing_label_falls_back_to_default_route() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first, "second": second},
        default_routing_label="first",
    )

    @broker.task(queue_name="second", tenant="acme")
    async def routed_task() -> None: ...

    await routed_task.kicker().without_labels("queue_name").kiq()

    assert first.queue.qsize() == 1
    assert second.queue.qsize() == 0
    labels = wire_labels(first)
    assert "queue_name" not in labels
    assert labels["tenant"] == "acme"


async def test_default_kick_after_overrides_still_follows_template() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first, "second": second},
        default_routing_label="first",
    )

    @broker.task(queue_name="first", tenant="acme")
    async def routed_task() -> None: ...

    # A scoped override sends one message elsewhere...
    await routed_task.kicker().with_labels(queue_name="second").kiq()
    # ...and the next plain kick follows the original template again.
    await routed_task.kiq()

    assert first.queue.qsize() == 1
    assert second.queue.qsize() == 1
    assert wire_labels(first) == {"tenant": "acme"}
    assert wire_labels(second) == {"tenant": "acme"}
    assert routed_task.labels["queue_name"] == "first"


async def test_requeue_keeps_business_labels_through_routing() -> None:
    first, second = AsyncQueueBroker(), AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first, "second": second},
        default_routing_label="first",
    )
    broker.with_result_backend(InmemoryResultBackend())
    seen_labels: list[dict[str, object]] = []
    runs = 0

    @broker.task(queue_name="first", tenant="acme")
    async def requeued_task(context: Context = TaskiqDepends()) -> None:
        nonlocal runs
        seen_labels.append(dict(context.message.labels))
        runs += 1
        if runs < 2:
            await context.requeue()

    handle = await requeued_task.kiq()
    receiver = Receiver(broker=broker)
    finish_event = asyncio.Event()
    listener = asyncio.create_task(receiver.listen(finish_event))

    result = await handle.wait_result(timeout=5)
    assert not result.is_err

    finish_event.set()
    await asyncio.wait_for(listener, timeout=5)
    await broker.shutdown()

    assert runs == 2
    assert len(seen_labels) == 2
    # Routing stays internal; business labels survive every delivery.
    for labels in seen_labels:
        assert "queue_name" not in labels
        assert labels["tenant"] == "acme"
    # The requeue marker is added only to the redelivered message.
    assert "X-Taskiq-requeue" not in seen_labels[0]
    assert seen_labels[1]["X-Taskiq-requeue"] == "1"


async def test_retry_keeps_business_labels_and_template_through_routing() -> None:
    first = AsyncQueueBroker()
    broker = MultiBroker(
        routing={"first": first},
        default_routing_label="first",
    )
    broker.with_result_backend(InmemoryResultBackend())
    broker.add_middlewares(SimpleRetryMiddleware(default_retry_count=2))
    attempts = 0

    @broker.task(
        queue_name="first",
        tenant="acme",
        retry_on_error=True,
        max_retries=2,
    )
    async def flaky_task() -> str:
        nonlocal attempts
        attempts += 1
        if attempts < 2:
            raise RuntimeError("boom")
        return "ok"

    handle = await flaky_task.kiq()
    receiver = Receiver(broker=broker)
    finish_event = asyncio.Event()
    listener = asyncio.create_task(receiver.listen(finish_event))

    result = await handle.wait_result(timeout=5)
    assert not result.is_err
    assert result.return_value == "ok"
    assert attempts == 2

    finish_event.set()
    await asyncio.wait_for(listener, timeout=5)
    await broker.shutdown()

    # Retries never mutate the registered template.
    assert flaky_task.labels == {
        "queue_name": "first",
        "tenant": "acme",
        "retry_on_error": True,
        "max_retries": 2,
    }
    # The redelivered payload carried business labels but no routing label.
    assert first.queue.qsize() == 0

