"""Tests for the Receiver optional unregistered-task waiting window."""

import asyncio
from collections.abc import AsyncGenerator, Callable
from time import monotonic
from typing import Any

import pytest

from taskiq.abc.broker import AsyncBroker
from taskiq.acks import AckableMessage, AckController
from taskiq.message import BrokerMessage, TaskiqMessage
from taskiq.receiver import Receiver
from taskiq.receiver.receiver import _PendingTask, _PrefetchedMessage


async def wait_until(predicate: Callable[[], bool], timeout: float = 2) -> None:
    """Poll a synchronous predicate until it holds or the timeout expires."""
    deadline = asyncio.get_running_loop().time() + timeout
    while not predicate():
        if asyncio.get_running_loop().time() >= deadline:
            raise AssertionError("Condition was not met before the timeout.")
        await asyncio.sleep(0.02)


class RequeueBroker(AsyncBroker):
    """Queue-backed broker recording acks and broker-level requeues."""

    def __init__(self) -> None:
        super().__init__()
        self.incoming: asyncio.Queue[AckableMessage] = asyncio.Queue()
        self.acked: list[str] = []
        self.requeued: list[str] = []
        self.requeue_supported = True
        # When True the delivery hooks are coroutine functions.
        self.async_hooks = False
        # When True the requeue hook raises instead of recording the message.
        self.fail_requeue = False

    async def kick(self, message: BrokerMessage) -> None:
        """Put an ackable/requeueable encoded delivery into the listener."""
        task_id = message.task_id

        def ack() -> None:
            self.acked.append(task_id)

        async def async_ack() -> None:
            self.acked.append(task_id)

        def requeue() -> None:
            if self.fail_requeue:
                raise RuntimeError("requeue failed")
            self.requeued.append(task_id)

        async def async_requeue() -> None:
            if self.fail_requeue:
                raise RuntimeError("requeue failed")
            self.requeued.append(task_id)

        await self.incoming.put(
            AckableMessage(
                data=message.message,
                ack=async_ack if self.async_hooks else ack,
                requeue=(
                    (async_requeue if self.async_hooks else requeue)
                    if self.requeue_supported
                    else None
                ),
            ),
        )

    async def listen(self) -> AsyncGenerator[AckableMessage, None]:
        """Yield every queued delivery and block on an empty queue."""
        while True:
            yield await self.incoming.get()

    def encode(self, task_name: str, task_id: str) -> BrokerMessage:
        """Build an encoded message addressed to a task name."""
        return self.formatter.dumps(
            TaskiqMessage(
                task_id=task_id,
                task_name=task_name,
                labels={},
                args=[],
                kwargs={},
            ),
        )


def build_receiver(
    broker: AsyncBroker,
    *,
    timeout: float | None,
    max_unregistered: int = 100,
    max_async_tasks: int | None = 10,
) -> Receiver:
    """Create a receiver without running broker startup."""
    return Receiver(
        broker,
        max_async_tasks=max_async_tasks,
        max_prefetch=0,
        run_startup=False,
        unregistered_task_timeout=timeout,
        max_unregistered_tasks=max_unregistered,
        wait_tasks_timeout=5,
    )


async def test_unregistered_deliveries_wait_then_requeue_on_timeout() -> None:
    """Two parked messages are requeued once their waiting window expires."""
    broker = RequeueBroker()
    receiver = build_receiver(broker, timeout=0.15)
    for task_id in ("pending-1", "pending-2"):
        await broker.kick(broker.encode("not.yet.registered", task_id))

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    try:
        # Both deliveries are parked inside the bounded registry.
        await wait_until(
            lambda: set(receiver.pending_tasks) == {"pending-1", "pending-2"},
        )

        await wait_until(lambda: len(broker.requeued) == 2)
        assert sorted(broker.requeued) == ["pending-1", "pending-2"]
        assert sorted(broker.acked) == ["pending-1", "pending-2"]
        assert not receiver.pending_tasks
    finally:
        finish_event.set()
        await asyncio.wait_for(listen_task, timeout=2)


async def test_known_task_completes_before_late_registration_settles_pending() -> None:
    """Known work is not blocked and a settled message is never executed."""
    broker = RequeueBroker()
    events: list[str] = []

    @broker.task(task_name="registered.early")
    async def early_task() -> None:
        events.append("early")

    receiver = build_receiver(broker, timeout=1)
    await broker.kick(broker.encode("registered.late", "late-1"))
    await broker.kick(broker.encode("registered.late", "late-2"))
    await broker.kick(broker.encode("registered.early", "early-1"))

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    try:
        # The two late tasks occupy the waiting positions first.
        await wait_until(
            lambda: set(receiver.pending_tasks) == {"late-1", "late-2"},
        )

        # The registered task is processed while the others keep waiting.
        await wait_until(lambda: events == ["early"])
        assert broker.acked == ["early-1"]
        assert set(receiver.pending_tasks) == {"late-1", "late-2"}

        # After the waiting window expires the name is registered too late:
        # the decision was already made, so the task body must never run.
        await wait_until(lambda: sorted(broker.requeued) == ["late-1", "late-2"])

        async def late_body() -> None:
            events.append("late")

        broker.register_task(late_body, task_name="registered.late")
        await asyncio.sleep(0.2)

        assert events == ["early"]
        assert sorted(broker.acked) == ["early-1", "late-1", "late-2"]
        assert not receiver.pending_tasks
    finally:
        finish_event.set()
        await asyncio.wait_for(listen_task, timeout=2)


async def test_registration_within_window_redelivers_and_executes_once() -> None:
    """A task registered before the deadline is redelivered and run once."""
    broker = RequeueBroker()
    executed: list[str] = []

    receiver = build_receiver(broker, timeout=0.5)
    await broker.kick(broker.encode("registered.in.time", "in-time-1"))

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    try:
        await wait_until(lambda: set(receiver.pending_tasks) == {"in-time-1"})

        async def body() -> None:
            executed.append("in-time-1")

        broker.register_task(body, task_name="registered.in.time")

        await wait_until(lambda: executed == ["in-time-1"])
        assert broker.requeued == []
        assert broker.acked == ["in-time-1"]
        assert not receiver.pending_tasks

        # A later re-registration cannot re-run an already settled delivery.
        async def replacement() -> None:
            executed.append("replacement")

        broker.register_task(replacement, task_name="registered.in.time")
        await asyncio.sleep(0.15)
        assert executed == ["in-time-1"]
    finally:
        finish_event.set()
        await asyncio.wait_for(listen_task, timeout=2)


async def test_timeout_without_requeue_support_reports_and_acks() -> None:
    """Brokers without requeue capability get a reported, acknowledged drop."""
    broker = RequeueBroker()
    broker.requeue_supported = False
    receiver = build_receiver(broker, timeout=0.1)
    await broker.kick(broker.encode("missing.task", "no-requeue-1"))

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    try:
        await wait_until(lambda: broker.acked == ["no-requeue-1"])
        assert broker.requeued == []
        assert not receiver.pending_tasks
    finally:
        finish_event.set()
        await asyncio.wait_for(listen_task, timeout=2)


async def test_pending_registry_is_bounded() -> None:
    """Overflowing the bounded registry falls back to immediate processing."""
    broker = RequeueBroker()
    receiver = build_receiver(
        broker,
        timeout=0.5,
        max_unregistered=2,
    )
    for index in range(3):
        await broker.kick(broker.encode("missing.task", f"overflow-{index}"))

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    try:
        await wait_until(
            lambda: set(receiver.pending_tasks) == {"overflow-0", "overflow-1"},
        )
        # The third delivery bypassed the full registry and was acked at once.
        await wait_until(lambda: broker.acked == ["overflow-2"])
    finally:
        finish_event.set()
        await asyncio.wait_for(listen_task, timeout=2)


async def test_feature_disabled_keeps_unknown_task_ack_behavior() -> None:
    """Without a waiting window unknown tasks are acknowledged immediately."""
    broker = RequeueBroker()
    receiver = build_receiver(broker, timeout=None)
    await broker.kick(broker.encode("missing.task", "disabled-1"))

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    try:
        await wait_until(lambda: broker.acked == ["disabled-1"])
        assert broker.requeued == []
        assert not receiver.pending_tasks
    finally:
        finish_event.set()
        await asyncio.wait_for(listen_task, timeout=2)


async def test_shutdown_abandons_pending_without_acknowledgement() -> None:
    """Parked deliveries are left unacked so the broker can redeliver them."""
    broker = RequeueBroker()
    receiver = build_receiver(broker, timeout=5)
    await broker.kick(broker.encode("missing.task", "shutdown-1"))

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    try:
        await wait_until(lambda: set(receiver.pending_tasks) == {"shutdown-1"})
    finally:
        finish_event.set()
        await asyncio.wait_for(listen_task, timeout=2)

    assert broker.acked == []
    assert broker.requeued == []
    assert not receiver.pending_tasks


async def test_unparsable_delivery_bypasses_pending_registry() -> None:
    """A message that cannot be parsed follows the normal callback path."""
    broker = RequeueBroker()
    receiver = build_receiver(broker, timeout=1)
    await broker.incoming.put(
        AckableMessage(
            data=b"not a valid taskiq message",
            ack=lambda: broker.acked.append("garbage"),
        ),
    )

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    try:
        await wait_until(lambda: broker.acked == ["garbage"])
        assert not receiver.pending_tasks
    finally:
        finish_event.set()
        await asyncio.wait_for(listen_task, timeout=2)


async def test_async_requeue_and_ack_hooks_are_awaited() -> None:
    """Coroutine requeue/ack hooks provided by a broker are awaited."""
    broker = RequeueBroker()
    broker.async_hooks = True
    receiver = build_receiver(broker, timeout=0.1)
    await broker.kick(broker.encode("missing.task", "async-hook-1"))

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    try:
        await wait_until(lambda: broker.requeued == ["async-hook-1"])
        assert broker.acked == ["async-hook-1"]
    finally:
        finish_event.set()
        await asyncio.wait_for(listen_task, timeout=2)


async def test_failing_requeue_still_acknowledges_the_delivery() -> None:
    """A broken requeue hook is logged and the delivery is acknowledged once."""
    broker = RequeueBroker()
    broker.fail_requeue = True
    receiver = build_receiver(broker, timeout=0.1)
    await broker.kick(broker.encode("missing.task", "broken-requeue-1"))

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    try:
        await wait_until(lambda: broker.acked == ["broken-requeue-1"])
        assert broker.requeued == []
        assert not receiver.pending_tasks
    finally:
        finish_event.set()
        await asyncio.wait_for(listen_task, timeout=2)


async def test_settlement_helpers_are_idempotent() -> None:
    """The single-decision guards ignore deliveries already settled."""
    broker = RequeueBroker()
    receiver = build_receiver(broker, timeout=1)
    now = monotonic()
    pending = _PendingTask(
        delivery=_PrefetchedMessage(data=b"", owns_delivery_slot=False),
        task_name="guarded.task",
        task_id="guarded-1",
        ack_controller=AckController(None),
        enqueued_at=now,
        deadline=now + 1,
    )
    pending.settled = True

    # Redelivery and expiry must no-op once the decision was made.
    queue: asyncio.Queue[object] = asyncio.Queue()
    receiver._redeliver_pending_task(pending, queue)
    await receiver._settle_expired_pending_task(pending)
    assert queue.empty()
    assert broker.acked == []
    assert broker.requeued == []


async def test_pending_supervisor_exits_after_draining_on_prefetch_end() -> None:
    """The supervisor returns once prefetching ended and nothing remains parked."""
    broker = RequeueBroker()
    receiver = build_receiver(broker, timeout=1)

    finish_event = asyncio.Event()
    prefetch_done = asyncio.Event()
    prefetch_done.set()
    queue: asyncio.Queue[object] = asyncio.Queue()

    await asyncio.wait_for(
        receiver.pending_task_supervisor(queue, finish_event, prefetch_done),
        timeout=1,
    )


async def test_shutdown_wait_returns_when_pending_drain_times_out() -> None:
    """A bounded shutdown wait does not hang on a stuck pending registry."""
    broker = RequeueBroker()
    receiver = build_receiver(broker, timeout=1)
    receiver.wait_tasks_timeout = 0.05
    prefetch_done = asyncio.Event()
    prefetch_done.set()
    receiver.pending_tasks["stuck-1"] = _PendingTask(
        delivery=_PrefetchedMessage(data=b"", owns_delivery_slot=False),
        task_name="stuck.task",
        task_id="stuck-1",
        ack_controller=AckController(None),
        enqueued_at=0,
        deadline=10,
    )
    receiver._pending_empty.clear()

    await asyncio.wait_for(
        receiver._wait_for_pending_tasks(prefetch_done),
        timeout=1,
    )


async def test_failing_ack_is_logged_without_raising() -> None:
    """A broken ack hook on a parked delivery is swallowed and logged."""

    def broken_ack() -> None:
        raise RuntimeError("ack failed")

    broker = RequeueBroker()
    receiver = build_receiver(broker, timeout=1)
    pending = _PendingTask(
        delivery=_PrefetchedMessage(data=b"", owns_delivery_slot=False),
        task_name="broken.ack.task",
        task_id="broken-ack-1",
        ack_controller=AckController(broken_ack),
        enqueued_at=0,
        deadline=10,
    )

    # Must not raise even though the ack hook fails.
    await receiver._ack_pending_task(pending)
    assert broker.acked == []


@pytest.mark.parametrize(
    ("kwargs", "message"),
    [
        (
            {"unregistered_task_timeout": 0},
            "unregistered_task_timeout must be positive",
        ),
        (
            {"unregistered_task_timeout": -1},
            "unregistered_task_timeout must be positive",
        ),
        (
            {"max_unregistered_tasks": 0},
            "max_unregistered_tasks must be positive",
        ),
    ],
)
async def test_invalid_options_are_rejected(
    kwargs: dict[str, Any],
    message: str,
) -> None:
    """Invalid waiting configuration fails before listening starts."""
    broker = RequeueBroker()
    with pytest.raises(ValueError, match=message):
        Receiver(broker, run_startup=False, **kwargs)
