import asyncio

from taskiq.abc.schedule_source import ScheduleSource
from taskiq.brokers.inmemory_broker import InMemoryBroker
from taskiq.exceptions import ScheduledTaskCancelledError
from taskiq.message import BrokerMessage
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


class RecordingBroker(AsyncQueueBroker):
    """Record kicked messages instead of transporting them."""

    def __init__(self) -> None:
        super().__init__()
        self.sent: list[BrokerMessage] = []

    async def kick(self, message: BrokerMessage) -> None:
        self.sent.append(message)


class MutableScheduleSource(ScheduleSource):
    """Source whose stored definition can be replaced mid-flight."""

    def __init__(self, schedules: list[ScheduledTask]) -> None:
        self.schedules = list(schedules)
        self.pre_send_calls: list[ScheduledTask] = []
        self.post_send_calls: list[ScheduledTask] = []

    async def get_schedules(self) -> list[ScheduledTask]:
        return list(self.schedules)

    def pre_send(self, task: ScheduledTask) -> None:
        self.pre_send_calls.append(task)

    def post_send(self, task: ScheduledTask) -> None:
        self.post_send_calls.append(task)

    def replace(self, task: ScheduledTask) -> None:
        """Replace the stored definition keeping its schedule_id."""
        self.schedules = [
            task if current.schedule_id == task.schedule_id else current
            for current in self.schedules
        ]


class BarrierScheduleSource(MutableScheduleSource):
    """Source whose first pre_send sleeps on a barrier."""

    def __init__(self, schedules: list[ScheduledTask]) -> None:
        super().__init__(schedules)
        self.entered = asyncio.Event()
        self.release = asyncio.Event()
        self.cancel_payloads: set[str] = set()
        self._blocked = False

    async def pre_send(self, task: ScheduledTask) -> None:
        self.pre_send_calls.append(task)
        if task.kwargs.get("payload") in self.cancel_payloads:
            raise ScheduledTaskCancelledError
        if not self._blocked:
            self._blocked = True
            self.entered.set()
            await self.release.wait()


def make_task(payload: str, schedule_id: str = "schedule-1") -> ScheduledTask:
    """Build a schedule definition carrying a distinguishable payload."""
    return ScheduledTask(
        task_name="ping:pong",
        labels={},
        args=[],
        kwargs={"payload": payload},
        cron="* * * * *",
        schedule_id=schedule_id,
    )


def sent_payloads(broker: RecordingBroker) -> list[str]:
    """Decode payloads of all kicked messages."""
    return [
        broker.formatter.loads(message.message).kwargs["payload"]
        for message in broker.sent
    ]


async def test_unchanged_definition_is_sent_after_single_check() -> None:
    task = make_task("current")
    source = MutableScheduleSource([task])
    broker = RecordingBroker()
    scheduler = TaskiqScheduler(broker=broker, sources=[source])

    await scheduler.on_ready(source, task)

    assert sent_payloads(broker) == ["current"]
    assert source.pre_send_calls == [task]
    assert source.post_send_calls == [task]


async def test_replaced_definition_during_pre_send_is_refreshed() -> None:
    old_task = make_task("old")
    new_task = make_task("new")
    source = BarrierScheduleSource([old_task])
    broker = RecordingBroker()
    scheduler = TaskiqScheduler(broker=broker, sources=[source])

    dispatch = asyncio.create_task(scheduler.on_ready(source, old_task))
    # The old callback now sleeps on the barrier inside pre_send.
    await source.entered.wait()
    # The scheduling side replaces the definition before the dispatch.
    source.replace(new_task)
    source.release.set()
    await dispatch

    # The refreshed payload was re-checked and sent, not the stale one.
    assert sent_payloads(broker) == ["new"]
    assert [call.kwargs["payload"] for call in source.pre_send_calls] == ["old", "new"]
    assert [call.kwargs["payload"] for call in source.post_send_calls] == ["new"]


async def test_refreshed_definition_can_still_be_cancelled() -> None:
    old_task = make_task("old")
    new_task = make_task("new")
    source = BarrierScheduleSource([old_task])
    source.cancel_payloads.add("new")
    broker = RecordingBroker()
    scheduler = TaskiqScheduler(broker=broker, sources=[source])

    dispatch = asyncio.create_task(scheduler.on_ready(source, old_task))
    await source.entered.wait()
    source.replace(new_task)
    source.release.set()
    await dispatch

    # The refreshed definition did not pass pre_send, so nothing was sent.
    assert broker.sent == []
    assert source.post_send_calls == []


async def test_removed_schedule_during_pre_send_is_not_sent() -> None:
    old_task = make_task("old")
    source = BarrierScheduleSource([old_task])
    broker = RecordingBroker()
    scheduler = TaskiqScheduler(broker=broker, sources=[source])

    dispatch = asyncio.create_task(scheduler.on_ready(source, old_task))
    await source.entered.wait()
    source.schedules.clear()
    source.release.set()
    await dispatch

    assert broker.sent == []
    assert source.post_send_calls == []


async def test_replacement_after_send_still_commits_old_definition() -> None:
    old_task = make_task("old")
    new_task = make_task("new")
    source = MutableScheduleSource([old_task])
    broker = RecordingBroker()
    scheduler = TaskiqScheduler(broker=broker, sources=[source])

    original_kick = broker.kick

    async def replacing_kick(message: BrokerMessage) -> None:
        # The definition is replaced only after the dispatch has started.
        source.replace(new_task)
        await original_kick(message)

    broker.kick = replacing_kick  # type: ignore[method-assign]

    await scheduler.on_ready(source, old_task)

    # The sent message belongs to the old definition,
    # and post_send records exactly that committed version.
    assert sent_payloads(broker) == ["old"]
    assert source.post_send_calls == [old_task]


async def test_dispatch_dropped_when_source_cannot_confirm_definition() -> None:
    old_task = make_task("old")
    source = MutableScheduleSource([old_task])
    broker = RecordingBroker()
    scheduler = TaskiqScheduler(broker=broker, sources=[source])

    async def failing_get_schedules() -> list[ScheduledTask]:
        raise RuntimeError("source unavailable")

    source.get_schedules = failing_get_schedules  # type: ignore[method-assign]

    await scheduler.on_ready(source, old_task)

    assert broker.sent == []
    assert source.post_send_calls == []
