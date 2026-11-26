import asyncio
from collections.abc import AsyncGenerator
from typing import Any

import pytest
from taskiq_dependencies import Depends

from taskiq.abc.broker import AsyncBroker
from taskiq.abc.middleware import TaskiqMiddleware
from taskiq.abc.result_backend import AsyncResultBackend
from taskiq.acks import (
    AckableMessage,
    AcknowledgeType,
    AckStage,
    ResultSaveStatus,
)
from taskiq.context import Context
from taskiq.exceptions import NoResultError
from taskiq.message import BrokerMessage, TaskiqMessage
from taskiq.receiver import Receiver
from taskiq.result import TaskiqResult


class _ScriptedResultBackend(AsyncResultBackend[Any]):
    """Result backend that scripts per-task write outcomes."""

    def __init__(
        self,
        timeout_task_id: str,
        save_statuses: dict[str, ResultSaveStatus],
    ) -> None:
        self.timeout_task_id = timeout_task_id
        self.save_statuses = save_statuses
        self.results: dict[str, TaskiqResult[Any]] = {}
        self.write_attempts: list[str] = []

    async def set_result(self, task_id: str, result: TaskiqResult[Any]) -> None:
        self.write_attempts.append(task_id)
        if task_id == self.timeout_task_id:
            # The backend never confirms the write, exactly like a timeout.
            raise asyncio.TimeoutError("result backend write timed out")
        self.results[task_id] = result
        self.save_statuses[task_id] = ResultSaveStatus.CONFIRMED

    async def is_result_ready(self, task_id: str) -> bool:
        return task_id in self.results

    async def get_result(
        self,
        task_id: str,
        with_logs: bool = False,
    ) -> TaskiqResult[Any]:
        return self.results[task_id]


class _FailingPostSaveMiddleware(TaskiqMiddleware):
    """Middleware whose post_save hook fails for one specific task."""

    def __init__(self, failing_task_id: str) -> None:
        super().__init__()
        self.failing_task_id = failing_task_id
        self.post_save_calls: list[str] = []

    def post_save(
        self,
        message: TaskiqMessage,
        result: TaskiqResult[Any],
    ) -> None:
        self.post_save_calls.append(message.task_id)
        if message.task_id == self.failing_task_id:
            raise RuntimeError("post_save hook blew up after the write")


class _ReceiptBroker(AsyncBroker):
    """Local queue broker that records the real acknowledgement stage."""

    def __init__(self) -> None:
        super().__init__()
        self.queue: asyncio.Queue[AckableMessage] = asyncio.Queue()
        # task_id -> stage at which the broker confirmed the delivery.
        self.ack_receipts: dict[str, str] = {}

    async def kick(self, message: BrokerMessage) -> None:
        task_id = message.task_id

        def ack(stage: str) -> None:
            self.ack_receipts[task_id] = stage

        await self.queue.put(AckableMessage(data=message.message, ack=ack))

    async def deliver(self, message: TaskiqMessage) -> None:
        """Format a TaskiqMessage and enqueue it as an ackable delivery."""
        await self.kick(self.formatter.dumps(message))

    async def listen(self) -> AsyncGenerator[AckableMessage, None]:
        while True:
            yield await self.queue.get()


def _build_receiver(
    broker: AsyncBroker,
    ack_type: AcknowledgeType,
) -> Receiver:
    return Receiver(
        broker,
        max_async_tasks=1,
        run_startup=False,
        ack_type=ack_type,
    )


async def test_strict_when_saved_policy_for_three_messages() -> None:
    """Strict acking distinguishes confirmed writes from unknown writes."""
    broker = _ReceiptBroker()
    save_statuses: dict[str, ResultSaveStatus] = {}

    hook_failure_task_id = "hook-failure"
    timeout_task_id = "save-timeout"
    no_result_task_id = "no-result"

    result_backend = _ScriptedResultBackend(timeout_task_id, save_statuses)
    middleware = _FailingPostSaveMiddleware(hook_failure_task_id)
    broker.with_result_backend(result_backend).with_middlewares(middleware)

    @broker.task(task_name="strict.hook_failure")
    async def hook_failure_task() -> str:
        return "saved-but-hook-fails"

    @broker.task(task_name="strict.save_timeout")
    async def save_timeout_task() -> str:
        return "backend-never-confirms"

    @broker.task(task_name="strict.no_result")
    async def no_result_task() -> None:
        raise NoResultError

    receiver = _build_receiver(broker, AcknowledgeType.WHEN_SAVED_STRICT)

    for task, task_id in (
        (hook_failure_task, hook_failure_task_id),
        (save_timeout_task, timeout_task_id),
        (no_result_task, no_result_task_id),
    ):
        message = task.kicker()._prepare_message()
        message.task_id = task_id
        await broker.deliver(message)

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))

    # Wait until all three backends writes have been attempted.
    await asyncio.wait_for(
        _wait_for(lambda: len(result_backend.write_attempts) == 2),
        timeout=2,
    )
    # Give the no-result delivery a chance to settle as well.
    await asyncio.sleep(0.05)
    finish_event.set()
    await asyncio.wait_for(listen_task, timeout=2)

    # Only the two successful outcomes are acknowledged, each exactly once.
    assert broker.ack_receipts == {
        hook_failure_task_id: AckStage.WHEN_SAVED.value,
        no_result_task_id: AckStage.NO_RESULT.value,
    }
    # The timed-out delivery is never acknowledged, so the broker may redeliver.
    assert timeout_task_id not in broker.ack_receipts

    # A confirmed write with a failing hook is different from an unknown write.
    assert save_statuses == {
        hook_failure_task_id: ResultSaveStatus.CONFIRMED,
    }
    assert timeout_task_id not in save_statuses
    assert hook_failure_task_id in result_backend.results
    assert timeout_task_id not in result_backend.results
    assert no_result_task_id not in result_backend.results

    # The post_save hook ran for the confirmed write even though it raised.
    assert middleware.post_save_calls == [hook_failure_task_id]


async def test_when_saved_strict_label_overrides_worker_policy() -> None:
    """A task can opt into the strict policy through its ack_type label."""
    broker = _ReceiptBroker()
    save_statuses: dict[str, ResultSaveStatus] = {}
    timeout_task_id = "label-timeout"
    result_backend = _ScriptedResultBackend(timeout_task_id, save_statuses)
    broker.with_result_backend(result_backend)

    @broker.task(task_name="strict.label", ack_type="when_saved_strict")
    async def timeout_task() -> str:
        return "label-controlled"

    # The worker itself uses the lenient, legacy default policy.
    receiver = _build_receiver(broker, AcknowledgeType.WHEN_SAVED)

    message = timeout_task.kicker()._prepare_message()
    message.task_id = timeout_task_id
    await broker.deliver(message)

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    await asyncio.wait_for(
        _wait_for(lambda: timeout_task_id in result_backend.write_attempts),
        timeout=2,
    )
    await asyncio.sleep(0.05)
    finish_event.set()
    await asyncio.wait_for(listen_task, timeout=2)

    assert timeout_task_id not in broker.ack_receipts
    assert timeout_task_id not in save_statuses


async def test_lenient_when_saved_acks_even_when_write_times_out() -> None:
    """The legacy WHEN_SAVED policy keeps its existing semantics."""
    broker = _ReceiptBroker()
    save_statuses: dict[str, ResultSaveStatus] = {}
    timeout_task_id = "lenient-timeout"
    result_backend = _ScriptedResultBackend(timeout_task_id, save_statuses)
    broker.with_result_backend(result_backend)

    @broker.task(task_name="lenient.timeout")
    async def timeout_task() -> str:
        return "legacy-policy"

    receiver = _build_receiver(broker, AcknowledgeType.WHEN_SAVED)

    message = timeout_task.kicker()._prepare_message()
    message.task_id = timeout_task_id
    await broker.deliver(message)

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    await asyncio.wait_for(
        _wait_for(lambda: timeout_task_id in result_backend.write_attempts),
        timeout=2,
    )
    await asyncio.sleep(0.05)
    finish_event.set()
    await asyncio.wait_for(listen_task, timeout=2)

    assert broker.ack_receipts == {
        timeout_task_id: AckStage.WHEN_SAVED.value,
    }


async def test_manual_and_when_received_policies_keep_their_semantics() -> None:
    """Old early/manual acknowledgement entries still record their own stages."""
    broker = _ReceiptBroker()
    save_statuses: dict[str, ResultSaveStatus] = {}
    result_backend = _ScriptedResultBackend("never", save_statuses)
    broker.with_result_backend(result_backend)

    @broker.task(task_name="legacy.early", ack_type="when_received")
    async def early_task() -> int:
        return 1

    early_id = "early"
    early_message = early_task.kicker()._prepare_message()
    early_message.task_id = early_id

    # The manual task acknowledges itself through the injected context.
    @broker.task(task_name="legacy.manual", ack_type="manual")
    async def manual_task(context: Context = Depends()) -> int:
        await context.ack()
        return 2

    manual_id = "manual"
    manual_message = manual_task.kicker()._prepare_message()
    manual_message.task_id = manual_id

    await broker.deliver(early_message)
    await broker.deliver(manual_message)

    receiver = _build_receiver(broker, AcknowledgeType.WHEN_SAVED_STRICT)
    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))

    await asyncio.wait_for(
        _wait_for(lambda: set(broker.ack_receipts) == {early_id, manual_id}),
        timeout=2,
    )
    finish_event.set()
    await asyncio.wait_for(listen_task, timeout=2)

    assert broker.ack_receipts == {
        early_id: AckStage.WHEN_RECEIVED.value,
        manual_id: AckStage.MANUAL.value,
    }


async def test_lenient_when_saved_no_result_keeps_legacy_stage() -> None:
    """NoResultError under the lenient policy is still acked as when_saved."""
    broker = _ReceiptBroker()
    save_statuses: dict[str, ResultSaveStatus] = {}
    result_backend = _ScriptedResultBackend("none", save_statuses)
    broker.with_result_backend(result_backend)

    @broker.task(task_name="lenient.no_result")
    async def no_result_task() -> None:
        raise NoResultError

    receiver = _build_receiver(broker, AcknowledgeType.WHEN_SAVED)
    task_id = "lenient-no-result"
    message = no_result_task.kicker()._prepare_message()
    message.task_id = task_id
    await broker.deliver(message)

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    await asyncio.wait_for(
        _wait_for(lambda: task_id in broker.ack_receipts),
        timeout=2,
    )
    finish_event.set()
    await asyncio.wait_for(listen_task, timeout=2)

    assert broker.ack_receipts == {
        task_id: AckStage.WHEN_SAVED.value,
    }
    # Nothing is stored for a no-result task.
    assert task_id not in result_backend.results


async def test_zero_argument_ack_callable_still_supported() -> None:
    """Brokers with legacy zero-arg ack callbacks remain compatible."""
    broker = _ReceiptBroker()
    save_statuses: dict[str, ResultSaveStatus] = {}
    result_backend = _ScriptedResultBackend("none", save_statuses)
    broker.with_result_backend(result_backend)
    acked = False

    @broker.task(task_name="legacy.callback")
    async def simple_task() -> int:
        return 1

    receiver = _build_receiver(broker, AcknowledgeType.WHEN_SAVED_STRICT)
    encoded = broker.formatter.dumps(
        TaskiqMessage(
            task_id="zero-arg",
            task_name=simple_task.task_name,
            labels={},
            args=[],
            kwargs={},
        ),
    ).message

    def legacy_ack() -> None:
        nonlocal acked
        acked = True

    await receiver.callback(AckableMessage(data=encoded, ack=legacy_ack))
    assert acked


async def _wait_for(condition: Any) -> None:
    """Poll an async condition without adding extra test dependencies."""
    while not condition():
        await asyncio.sleep(0.01)


@pytest.mark.parametrize(
    ("ack_type", "expected_stage"),
    [
        (AcknowledgeType.WHEN_RECEIVED, AckStage.WHEN_RECEIVED),
        (AcknowledgeType.WHEN_EXECUTED, AckStage.WHEN_EXECUTED),
        (AcknowledgeType.WHEN_SAVED, AckStage.WHEN_SAVED),
    ],
)
async def test_ack_receipt_records_reached_stage(
    ack_type: AcknowledgeType,
    expected_stage: AckStage,
) -> None:
    """Every automatic policy stamps the receipt with the reached stage."""
    broker = _ReceiptBroker()
    save_statuses: dict[str, ResultSaveStatus] = {}
    broker.with_result_backend(_ScriptedResultBackend("none", save_statuses))

    @broker.task(task_name=f"stage.{ack_type.value}")
    async def stage_task() -> int:
        return 1

    receiver = _build_receiver(broker, ack_type)
    task_id = f"task-{ack_type.value}"
    message = stage_task.kicker()._prepare_message()
    message.task_id = task_id
    await broker.deliver(message)

    finish_event = asyncio.Event()
    listen_task = asyncio.create_task(receiver.listen(finish_event))
    await asyncio.wait_for(
        _wait_for(lambda: task_id in broker.ack_receipts),
        timeout=2,
    )
    finish_event.set()
    await asyncio.wait_for(listen_task, timeout=2)

    assert broker.ack_receipts[task_id] == expected_stage.value
