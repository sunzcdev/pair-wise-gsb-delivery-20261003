from datetime import datetime, timedelta, timezone

from taskiq.abc.schedule_source import ScheduleSource
from taskiq.cli.scheduler.run import SchedulerLoop
from taskiq.scheduler.scheduled_task import ScheduledTask, ScheduledTaskCancellation
from taskiq.scheduler.scheduler import TaskiqScheduler


class MutableSource(ScheduleSource):
    """Source whose schedules can change while the loop is running."""

    def __init__(self, priority: int, schedules: list[ScheduledTask]) -> None:
        self.priority = priority
        self.schedules = schedules

    async def get_schedules(self) -> list[ScheduledTask]:
        return list(self.schedules)


class RecordingScheduler(TaskiqScheduler):
    """Scheduler recording which source dispatched which definition."""

    def __init__(self, sources: list[ScheduleSource]) -> None:
        super().__init__(broker=None, sources=sources)  # type: ignore[arg-type]
        self.dispatched: list[tuple[int, str, list[object]]] = []

    async def on_ready(
        self,
        source: ScheduleSource,
        task: ScheduledTask,
    ) -> None:
        self.dispatched.append(
            (self.sources.index(source), task.schedule_id, task.args),
        )


def schedule(**overrides: object) -> ScheduledTask:
    values: dict[str, object] = {
        "task_name": "demo.task",
        "labels": {},
        "args": [],
        "kwargs": {},
        "schedule_id": "shared",
        "cron": "* * * * *",
    }
    values.update(overrides)
    return ScheduledTask(**values)


def make_loop(
    sources: list[ScheduleSource],
) -> tuple[SchedulerLoop, RecordingScheduler]:
    scheduler = RecordingScheduler(sources)
    return SchedulerLoop(scheduler), scheduler


async def refresh(loop: SchedulerLoop) -> None:
    """Apply the current source state, as a background refresh would."""
    loop.scheduled_tasks = await loop.scheduler.get_all_schedules()


def effective(loop: SchedulerLoop) -> ScheduledTask:
    for _, tasks in loop.scheduled_tasks:
        for task in tasks:
            return task
    raise AssertionError("no effective schedule")


async def test_override_dispatches_once_and_base_does_not_repeat_it() -> None:
    moment = datetime(2026, 1, 1, 12, 0, 0, tzinfo=timezone.utc)
    base = MutableSource(0, [schedule(args=["base"])])
    overlay = MutableSource(10, [schedule(args=["overlay"])])
    loop, scheduler = make_loop([base, overlay])
    await refresh(loop)

    assert effective(loop).args == ["overlay"]
    assert loop._is_schedule_ready_to_send(effective(loop), moment)
    await scheduler.on_ready(overlay, effective(loop))

    # The overlay leaves and the base definition becomes effective.
    overlay.schedules = []
    await refresh(loop)

    assert effective(loop).args == ["base"]
    assert not loop._is_schedule_ready_to_send(effective(loop), moment)
    assert scheduler.dispatched == [(1, "shared", ["overlay"])]

    # The following trigger point still fires from the base definition.
    assert loop._is_schedule_ready_to_send(
        effective(loop),
        moment + timedelta(minutes=1),
    )


async def test_next_trigger_point_after_switch_uses_base_definition() -> None:
    moment = datetime(2026, 1, 1, 12, 0, 0, tzinfo=timezone.utc)
    base = MutableSource(0, [schedule(args=["base"])])
    overlay = MutableSource(10, [schedule(args=["overlay"])])
    loop, scheduler = make_loop([base, overlay])
    await refresh(loop)
    await scheduler.on_ready(overlay, effective(loop))

    overlay.schedules = []
    await refresh(loop)
    later = moment + timedelta(minutes=1)
    assert loop._is_schedule_ready_to_send(effective(loop), later)
    assert loop._is_schedule_ready_to_send(
        effective(loop),
        later + timedelta(minutes=1),
    )


async def test_explicit_cancellation_is_not_a_missing_definition() -> None:
    moment = datetime(2026, 1, 1, 12, 0, 0, tzinfo=timezone.utc)
    base = MutableSource(0, [schedule(args=["base"])])
    overlay = MutableSource(10, [schedule(args=["overlay"])])
    loop, _ = make_loop([base, overlay])
    await refresh(loop)
    assert effective(loop).args == ["overlay"]

    overlay.schedules = [ScheduledTaskCancellation(schedule_id="shared")]
    await refresh(loop)

    # Nothing is effective although the base source still declares it.
    assert [tasks for _, tasks in loop.scheduled_tasks] == [[], []]

    # Dropping the cancellation brings the base definition back.
    overlay.schedules = []
    await refresh(loop)
    assert effective(loop).args == ["base"]
    assert loop._is_schedule_ready_to_send(effective(loop), moment)


async def test_partial_override_keeps_base_trigger_after_refresh() -> None:
    moment = datetime(2026, 1, 1, 12, 0, 0, tzinfo=timezone.utc)
    base = MutableSource(0, [schedule(args=["base"], kwargs={"keep": True})])
    overlay = MutableSource(
        10,
        [ScheduledTask(partial=True, schedule_id="shared", args=["overlay"])],
    )
    loop, scheduler = make_loop([base, overlay])
    await refresh(loop)

    merged = effective(loop)
    assert merged.args == ["overlay"]
    assert merged.kwargs == {"keep": True}
    assert merged.cron == "* * * * *"
    assert loop._is_schedule_ready_to_send(merged, moment)
    await scheduler.on_ready(overlay, merged)

    overlay.schedules = []
    await refresh(loop)
    assert not loop._is_schedule_ready_to_send(effective(loop), moment)


async def test_same_trigger_point_from_both_sources_is_sent_once() -> None:
    moment = datetime(2026, 1, 1, 12, 0, 0, tzinfo=timezone.utc)
    base = MutableSource(0, [schedule(args=["base"])])
    overlay = MutableSource(10, [schedule(args=["overlay"])])
    loop, scheduler = make_loop([base, overlay])
    await refresh(loop)

    for source, tasks in loop.scheduled_tasks:
        for task in tasks:
            if loop._is_schedule_ready_to_send(task, moment):
                await scheduler.on_ready(source, task)

    assert scheduler.dispatched == [(1, "shared", ["overlay"])]


async def test_trigger_switching_from_cron_to_time_keeps_the_occurrence() -> None:
    moment = datetime(2026, 1, 1, 12, 0, 0, tzinfo=timezone.utc)
    base = MutableSource(0, [schedule(cron="* * * * *")])
    overlay = MutableSource(
        10,
        [schedule(cron=None, time=moment)],
    )
    loop, _ = make_loop([base, overlay])
    await refresh(loop)

    assert loop._is_schedule_ready_to_send(effective(loop), moment)

    # The overlay disappears, so the base cron definition is effective.
    overlay.schedules = []
    await refresh(loop)
    assert effective(loop).cron == "* * * * *"
    assert not loop._is_schedule_ready_to_send(effective(loop), moment)


async def test_time_trigger_is_not_repeated_after_the_override_disappears() -> None:
    moment = datetime(2026, 1, 1, 12, 0, 0, tzinfo=timezone.utc)
    base = MutableSource(0, [schedule(cron=None, time=moment, args=["base"])])
    overlay = MutableSource(10, [schedule(cron=None, time=moment, args=["overlay"])])
    loop, scheduler = make_loop([base, overlay])
    await refresh(loop)

    assert loop._is_schedule_ready_to_send(effective(loop), moment)
    await scheduler.on_ready(overlay, effective(loop))

    overlay.schedules = []
    await refresh(loop)

    assert effective(loop).args == ["base"]
    assert not loop._is_schedule_ready_to_send(effective(loop), moment)
    assert scheduler.dispatched == [(1, "shared", ["overlay"])]


async def test_refresh_keeps_occurrence_when_id_is_temporarily_cancelled() -> None:
    moment = datetime(2026, 1, 1, 12, 0, 0, tzinfo=timezone.utc)
    base = MutableSource(0, [schedule(args=["base"])])
    overlay = MutableSource(10, [schedule(args=["overlay"])])
    loop, _ = make_loop([base, overlay])
    await refresh(loop)
    assert loop._is_schedule_ready_to_send(effective(loop), moment)

    overlay.schedules = [ScheduledTaskCancellation(schedule_id="shared")]
    await refresh(loop)
    overlay.schedules = []
    await refresh(loop)

    assert effective(loop).args == ["base"]
    assert not loop._is_schedule_ready_to_send(effective(loop), moment)
