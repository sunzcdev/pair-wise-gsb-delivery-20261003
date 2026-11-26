from taskiq.abc.schedule_source import ScheduleSource
from taskiq.brokers.inmemory_broker import InMemoryBroker
from taskiq.exceptions import ScheduledTaskCancelledError
from taskiq.scheduler.scheduled_task import ScheduledTask
from taskiq.scheduler.scheduler import TaskiqScheduler
from tests.utils import AsyncQueueBroker


class CancellingScheduleSource(ScheduleSource):
    async def get_schedules(self) -> list["ScheduledTask"]:
        """Return schedules list."""
        return []

    def pre_send(
        self,
        task: "ScheduledTask",
    ) -> None:
        """Raise cancelled error."""
        raise ScheduledTaskCancelledError


async def test_scheduled_task_cancelled() -> None:
    broker = InMemoryBroker()
    source = CancellingScheduleSource()
    scheduler = TaskiqScheduler(broker=broker, sources=[source])
    task = ScheduledTask(
        task_name="ping:pong",
        labels={},
        args=[],
        kwargs={},
        cron="* * * * *",
    )

    await scheduler.on_ready(source, task)  # error is caught


class ReadySource(ScheduleSource):
    """Source that can trigger `on_ready`."""

    async def get_schedules(self) -> list[ScheduledTask]:
        return []


async def test_same_occurrence_from_another_source_is_sent_once() -> None:
    broker = AsyncQueueBroker()
    base_source = ReadySource()
    overlay_source = ReadySource()
    scheduler = TaskiqScheduler(
        broker=broker,
        sources=[base_source, overlay_source],
    )
    task = ScheduledTask(
        task_name="ping:pong",
        labels={},
        args=[],
        kwargs={},
        schedule_id="shared-program",
        interval=30,
    )

    await scheduler.on_ready(overlay_source, task)
    await scheduler.on_ready(base_source, task)

    assert broker.queue.qsize() == 1

    # Explicit repeated calls from the same source remain allowed.
    await scheduler.on_ready(overlay_source, task)
    assert broker.queue.qsize() == 2
