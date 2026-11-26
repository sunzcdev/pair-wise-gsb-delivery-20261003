import asyncio
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from logging import getLogger
from typing import TYPE_CHECKING
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

from taskiq.exceptions import ScheduledTaskCancelledError
from taskiq.kicker import AsyncKicker
from taskiq.scheduler.merge_functions import resolve_schedules, source_priority
from taskiq.scheduler.scheduled_task import ScheduledTask
from taskiq.utils import maybe_awaitable

if TYPE_CHECKING:  # pragma: no cover
    from taskiq.abc.broker import AsyncBroker
    from taskiq.abc.schedule_source import ScheduleSource

logger = getLogger(__name__)


async def get_source_schedules(source: "ScheduleSource") -> list[ScheduledTask]:
    """
    Read schedules from one source, tolerating source failures.

    A failing source contributes nothing instead of breaking the
    whole refresh, so a broken override layer can't hide the base
    definitions.

    :param source: source to read.
    :return: schedules declared by the source.
    """
    try:
        return await source.get_schedules()
    except Exception:
        logger.exception("Cannot update schedules with source: %s", source)
        return []


_DISPATCH_RECORD_RETENTION = timedelta(minutes=10)


@dataclass
class _DispatchRecord:
    """An in-flight or emitted dispatch observed by this scheduler."""

    source: "ScheduleSource"
    recorded_at: datetime
    task: ScheduledTask


def _to_utc(value: datetime) -> datetime:
    """Normalize a datetime to an aware UTC value."""
    if value.tzinfo is None:
        return value.replace(tzinfo=timezone.utc)
    return value.astimezone(timezone.utc)


def _trigger_minute(task: ScheduledTask, now: datetime) -> datetime | None:
    """Return the stable minute identifying a cron/time trigger."""
    if task.time is not None:
        return _to_utc(task.time).replace(second=0, microsecond=0)
    if task.cron is None:
        return None

    moment = now
    offset = task.cron_offset
    if isinstance(offset, timedelta):
        moment = now + offset
    elif isinstance(offset, str):
        try:
            moment = now.astimezone(ZoneInfo(offset))
        except (ZoneInfoNotFoundError, ModuleNotFoundError):
            moment = now
    return moment.replace(second=0, microsecond=0)


def _interval_seconds(task: ScheduledTask) -> float | None:
    """Get an interval trigger's length in seconds."""
    if task.interval is None:
        return None
    if isinstance(task.interval, int):
        return float(task.interval)
    return task.interval.total_seconds()


def _is_same_dispatch_trigger(
    previous: _DispatchRecord,
    task: ScheduledTask,
    now: datetime,
) -> bool:
    """Check whether two direct ready calls represent the same occurrence."""
    interval = _interval_seconds(previous.task)
    next_interval = _interval_seconds(task)
    if interval is not None and next_interval is not None:
        elapsed = (now - previous.recorded_at).total_seconds()
        return round(elapsed) < round(interval)

    previous_minute = _trigger_minute(previous.task, previous.recorded_at)
    current_minute = _trigger_minute(task, now)
    return (
        previous_minute is not None
        and current_minute is not None
        and previous_minute == current_minute
    )


class TaskiqScheduler:
    """Scheduler class."""

    def __init__(
        self,
        broker: "AsyncBroker",
        sources: list["ScheduleSource"],
    ) -> None:  # pragma: no cover
        self.broker = broker
        self.sources = sources
        self._dispatch_records: dict[str, _DispatchRecord] = {}

    def _prune_dispatch_records(self, now: datetime) -> None:
        """Drop old direct-dispatch records to bound scheduler memory."""
        expired: list[str] = []
        for schedule_id, record in self._dispatch_records.items():
            retention = _DISPATCH_RECORD_RETENTION
            interval = _interval_seconds(record.task)
            if interval is not None:
                retention = max(retention, timedelta(seconds=interval * 2))
            if now - record.recorded_at > retention:
                expired.append(schedule_id)
        for schedule_id in expired:
            del self._dispatch_records[schedule_id]

    async def startup(self) -> None:  # pragma: no cover
        """
        This method is called on startup.

        Here you can do stuff, like creating
        connections or anything you'd like.
        """
        await self.broker.startup()

    def resolve_schedules(
        self,
        schedules: list[list[ScheduledTask]],
    ) -> list[tuple["ScheduleSource", list[ScheduledTask]]]:
        """
        Resolve raw per-source schedules into effective definitions.

        Definitions sharing a `schedule_id` are resolved once, using
        the priority declared by their sources. Each effective
        definition is reported under the source that owns it.

        :param schedules: schedules per source, in the sources order.
        :return: (source, effective schedules) pairs.
        """
        return resolve_schedules(self.sources, schedules)

    async def get_all_schedules(
        self,
    ) -> list[tuple["ScheduleSource", list[ScheduledTask]]]:
        """
        Read every source and resolve definitions that share an id.

        This is the single entry point for schedule reads. The result
        keeps the declared sources order and contains one effective
        definition per `schedule_id`.

        :return: (source, effective schedules) pairs.
        """
        schedules = await asyncio.gather(
            *[get_source_schedules(source) for source in self.sources],
        )
        return self.resolve_schedules(list(schedules))

    async def on_ready(self, source: "ScheduleSource", task: ScheduledTask) -> None:
        """
        This method is called when task is ready to be enqueued.

        It's triggered on proper time depending on `task.cron`, `task.interval`
        or `task.time` attribute. Direct calls from a different source for the
        same schedule occurrence are ignored after the first source has claimed
        or emitted it. Calls from the same source remain explicit retries.

        :param source: source that triggered this event.
        :param task: task to send
        """
        now = datetime.now(tz=timezone.utc)
        self._prune_dispatch_records(now)
        previous = self._dispatch_records.get(task.schedule_id)
        if (
            previous is not None
            and previous.source is not source
            and source_priority(previous.source) >= source_priority(source)
            and _is_same_dispatch_trigger(previous, task, now)
        ):
            logger.info(
                "Skipping duplicate occurrence of schedule %s from another source.",
                task.schedule_id,
            )
            return

        record = _DispatchRecord(source=source, recorded_at=now, task=task)
        self._dispatch_records[task.schedule_id] = record

        def rollback() -> None:
            current = self._dispatch_records.get(task.schedule_id)
            if current is record:
                if previous is not None:
                    self._dispatch_records[task.schedule_id] = previous
                else:
                    del self._dispatch_records[task.schedule_id]

        try:
            await maybe_awaitable(source.pre_send(task))
        except ScheduledTaskCancelledError:
            rollback()
            logger.info("Scheduled task %s has been cancelled.", task.task_name)
            return
        except (Exception, asyncio.CancelledError):
            rollback()
            raise

        try:
            await (
                AsyncKicker(task.task_name, self.broker, task.labels)
                .with_labels(
                    schedule_id=task.schedule_id,
                )
                .with_task_id(task_id=task.task_id)
                .kiq(
                    *task.args,
                    **task.kwargs,
                )
            )
        except (Exception, asyncio.CancelledError):
            rollback()
            raise

        await maybe_awaitable(source.post_send(task))

    async def shutdown(self) -> None:
        """Shutdown the scheduler process."""
        await self.broker.shutdown()
