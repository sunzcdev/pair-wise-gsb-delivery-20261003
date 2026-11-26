import asyncio
import inspect
import sys
from datetime import datetime, timedelta, timezone
from logging import basicConfig, getLogger
from typing import Any, TypeAlias
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

import pycron

from taskiq.abc.schedule_source import ScheduleSource
from taskiq.cli.scheduler.args import SchedulerArgs
from taskiq.cli.utils import import_object, import_tasks
from taskiq.scheduler.scheduled_task import ScheduledTask
from taskiq.scheduler.scheduler import TaskiqScheduler, get_source_schedules

logger = getLogger(__name__)


ScheduleId: TypeAlias = str

#: Moment of a trigger point that can be dispatched once.
OccurrenceKey: TypeAlias = datetime

#: How long a dispatched trigger point is remembered. It only needs to
#: outlive the schedule refresh that may move an effective definition
#: from one source to another, so a few update intervals are enough.
OCCURRENCE_RETENTION: timedelta = timedelta(minutes=10)


def to_tz_aware(time: datetime) -> datetime:
    """
    Convert datetime to timezone aware.

    This function takes a datetime and if
    timezone was not yet specified, it will
    be set to UTC.

    :param time: time to make timezone aware.
    :return: timezone aware time.
    """
    if time.tzinfo is None:
        return time.replace(tzinfo=timezone.utc)
    return time


async def get_schedules(source: ScheduleSource) -> list[ScheduledTask]:
    """
    Get schedules from source.

    If source raises an exception, it will be
    logged and an empty list will be returned.

    :param source: source to get schedules from.
    """
    return await get_source_schedules(source)


async def get_all_schedules(
    scheduler: TaskiqScheduler,
) -> list[tuple[ScheduleSource, list[ScheduledTask]]]:
    """
    Task to update all schedules.

    This function updates all schedules from all sources, resolves
    definitions that share the same `schedule_id` according to the
    source priorities and returns a list of (source, tasks) pairs.

    Every resolved schedule is reported once, under the source whose
    definition is effective, so source hooks run for the winning layer.

    :param scheduler: current scheduler.
    :return: list of (source, tasks) pairs.
    """
    logger.debug("Started schedule update.")
    schedules: list[list[ScheduledTask]] = await asyncio.gather(
        *[get_schedules(source) for source in scheduler.sources],
    )
    return scheduler.resolve_schedules(schedules)


class CronValueError(Exception):
    """Raised on invalid cron value."""


def is_cron_task_now(
    cron_value: str,
    now: datetime,
    offset: str | timedelta | None = None,
    last_run: datetime | None = None,
) -> bool:
    """
    Checks whether the cron task should start now.

    :raises CronValueError: On invalid cron value.
    """
    if last_run is not None:
        seconds_spend = (now - last_run).total_seconds()
        seconds_should_sped = timedelta(minutes=1).total_seconds()
        if round(seconds_spend) < round(seconds_should_sped):
            return False
    # If user specified cron offset we apply it.
    # If it's timedelta, we simply add the delta to current time.
    if offset and isinstance(offset, timedelta):
        now += offset
    # If timezone was specified as string we convert it timezone
    # offset and then apply.
    elif offset and isinstance(offset, str):
        try:
            now = now.astimezone(ZoneInfo(offset))
        # ZoneInfoNotFoundError for unknown keys, ModuleNotFoundError
        # for systems without timezone data available (e.g. missing
        # tzdata on Windows).
        except (ZoneInfoNotFoundError, ModuleNotFoundError) as e:
            raise CronValueError(e) from e

    try:
        return pycron.is_now(cron_value, now)
    except ValueError as e:
        raise CronValueError(e) from e


def is_time_task_now(
    time_value: datetime,
    now: datetime,
    last_run: datetime | None = None,
) -> bool:
    """Checks whether the time task should start now."""
    if last_run is not None:
        return False

    time_value = to_tz_aware(time_value)
    now = to_tz_aware(now)
    return time_value <= now


def is_interval_task_now(
    interval_value: int | timedelta,
    now: datetime,
    last_run: datetime | None = None,
) -> bool:
    """
    Checks whether the interval task should start now.

    Interval tasks must have a minimum interval of 1 second.
    Fractional intervals (e.g., 0.5 seconds) are not supported.

    :param interval_value: Interval as int (seconds) or timedelta
    :param now: Current datetime
    :param last_run: Last run datetime, None for first run
    :return: True if task should run now
    """
    if last_run is None:
        return True

    if isinstance(interval_value, int):
        interval_value = timedelta(seconds=interval_value)

    seconds_passed: float = (now - last_run).total_seconds()
    interval_seconds: float = interval_value.total_seconds()

    return round(seconds_passed) >= round(interval_seconds)


async def send(
    scheduler: TaskiqScheduler,
    source: ScheduleSource,
    task: ScheduledTask,
) -> None:
    """
    Send a task.

    :param scheduler: current scheduler.
    :param source: source of the task.
    :param task: task to send.
    """
    logger.info(
        "Sending task %s with schedule_id %s.",
        task.task_name,
        task.schedule_id,
    )
    await scheduler.on_ready(source, task)


async def send_with_timeout(
    scheduler: TaskiqScheduler,
    source: ScheduleSource,
    task: ScheduledTask,
    timeout: float,
) -> None:
    """
    Send a task, cancelling it if it does not complete within ``timeout`` seconds.

    A timed-out send is logged at WARNING and swallowed — re-raising would propagate
    out of the scheduler's main loop's ``add_done_callback``, which is not what we
    want. Suppressing it lets the done_callback clear ``running_schedules`` so the
    next cron boundary can re-dispatch the task. The slot is freed; the message is
    dropped (the broker did not acknowledge it within the budget).

    :param scheduler: current scheduler.
    :param source: source of the task.
    :param task: task to send.
    :param timeout: seconds to wait before cancelling the send. Must be > 0.
    """
    try:
        await asyncio.wait_for(send(scheduler, source, task), timeout=timeout)
    except asyncio.TimeoutError:
        logger.warning(
            "Sending task %s with schedule_id %s timed out after %.1fs "
            "and was cancelled. The next scheduled tick will retry.",
            task.task_name,
            task.schedule_id,
            timeout,
        )


async def _sleep_until_next_second() -> None:
    now = datetime.now(tz=timezone.utc)
    await asyncio.sleep(1 - now.microsecond / 1_000_000)


class SchedulerLoop:
    """Abstraction over scheduler loop."""

    def __init__(
        self,
        scheduler: TaskiqScheduler,
        *,
        event_loop: asyncio.AbstractEventLoop | None = None,
    ) -> None:
        self.scheduler = scheduler
        self._event_loop = event_loop or asyncio.get_event_loop()

        # Variables for control the last run of schedules.
        self.cron_tasks_last_run: dict[ScheduleId, datetime] = {}
        self.interval_tasks_last_run: dict[ScheduleId, datetime] = {}
        self.time_tasks_last_run: dict[ScheduleId, datetime] = {}

        # Trigger point that was already dispatched for a schedule id,
        # with the moment it was recorded. It survives a definition
        # moving between sources, and an id that is temporarily hidden
        # by an explicit cancellation, so an occurrence that was already
        # sent isn't sent again when a lower priority definition becomes
        # effective. Entries are dropped by age only, see
        # `OCCURRENCE_RETENTION`.
        self.dispatched_occurrences: dict[
            ScheduleId,
            tuple[OccurrenceKey, datetime],
        ] = {}

        self.scheduled_tasks: list[tuple[ScheduleSource, list[ScheduledTask]]] = []
        self.scheduled_tasks_updated_at: datetime | None = None
        self._update_schedules_task_future: asyncio.Task[Any] | None = None
        self._occurrence_retention: timedelta = OCCURRENCE_RETENTION

    def configure_occurrence_retention(self, update_interval: timedelta) -> None:
        """
        Keep occurrence records long enough to bridge a schedule refresh.

        A refresh happens at most once per `update_interval`, and the
        effective definition is already resolved before it's applied.
        Remembering the dispatched trigger points for a couple of
        update intervals is enough to survive a definition moving
        between sources, including a refresh that was already in
        flight when the definition was sent.

        :param update_interval: configured schedule refresh interval.
        """
        self._occurrence_retention = max(
            OCCURRENCE_RETENTION,
            update_interval * 2,
        )

    def _prune_dispatched_occurrences(self, now: datetime) -> None:
        """
        Drop occurrence records that are too old to matter.

        A record is only needed to bridge the refresh that moves an
        effective definition from one source to another.

        :param now: current time.
        """
        expired = [
            id_
            for id_, (_, recorded_at) in self.dispatched_occurrences.items()
            if now - recorded_at > self._occurrence_retention
        ]
        for id_ in expired:
            del self.dispatched_occurrences[id_]

    def _update_schedules_task_future_callback(self, task_: asyncio.Task[Any]) -> None:
        self.scheduled_tasks = task_.result()

        new_schedules_ids: set[ScheduleId] = set()
        for source, task_list in self.scheduled_tasks:
            logger.debug("Got %d schedules from source %s.", len(task_list), source)
            new_schedules_ids.update({t.schedule_id for t in task_list})

        # Deleting irrelevant scheduled tasks so they don't take up memory.
        # Occurrence records are not tied to the effective definition, so
        # they are pruned by age instead: a cancelled id may become
        # visible again after a later refresh and must not be resent.
        for id_ in self.cron_tasks_last_run.keys() - new_schedules_ids:
            del self.cron_tasks_last_run[id_]
        for id_ in self.interval_tasks_last_run.keys() - new_schedules_ids:
            del self.interval_tasks_last_run[id_]
        for id_ in self.time_tasks_last_run.keys() - new_schedules_ids:
            del self.time_tasks_last_run[id_]

        self._prune_dispatched_occurrences(datetime.now(tz=timezone.utc))

    async def _update_scheduled_tasks(self) -> None:
        if (
            self._update_schedules_task_future is not None
            and not self._update_schedules_task_future.done()
        ):
            logger.warning(
                "Schedules getting task started "
                "before the previous one finished. "
                "Consider increasing the update_interval.",
            )
        else:
            self._update_schedules_task_future = self._event_loop.create_task(
                get_all_schedules(self.scheduler),
            )
            self._update_schedules_task_future.add_done_callback(
                self._update_schedules_task_future_callback,
            )

    def _mark_cron_tasks_as_already_run(self) -> None:
        current_minute = datetime.now(tz=timezone.utc).replace(second=0, microsecond=0)
        for _, task_list in self.scheduled_tasks:
            for task in task_list:
                if task.cron is not None:
                    self.cron_tasks_last_run[task.schedule_id] = current_minute
                    occurrence = self._occurrence_key(task, current_minute)
                    if occurrence is not None:
                        self.dispatched_occurrences[task.schedule_id] = (
                            occurrence,
                            current_minute,
                        )

    def _occurrence_key(
        self,
        task: ScheduledTask,
        now: datetime,
    ) -> OccurrenceKey | None:
        """
        Identify the trigger point a definition is currently due for.

        The key is the moment of the trigger point itself. It never
        contains the source that declared the definition or the field
        used to declare it, so two sources describing the same trigger
        point for the same `schedule_id` produce the same key. That
        lets the scheduler recognise an occurrence as already
        dispatched after the effective definition moved to another
        source, even if the trigger type changed on the way.

        Cron and `time` definitions are identified by their trigger
        minute, so a definition switching between those two
        representations of the same trigger point is still the same
        occurrence. Interval definitions have no fixed trigger point,
        so they are keyed by the second they became due.

        :param task: effective definition.
        :param now: current time.
        :return: occurrence key or None when there is no trigger to track.
        """
        if task.cron is not None:
            moment = now
            offset = task.cron_offset
            try:
                if isinstance(offset, timedelta):
                    moment = now + offset
                elif isinstance(offset, str):
                    moment = now.astimezone(ZoneInfo(offset))
            except (ZoneInfoNotFoundError, ModuleNotFoundError):
                moment = now
            return moment.replace(second=0, microsecond=0)

        if task.time is not None:
            return to_tz_aware(task.time).replace(second=0, microsecond=0)

        if task.interval is not None:
            return now.replace(microsecond=0)

        return None

    def _occurrence_was_dispatched(
        self,
        schedule_id: ScheduleId,
        occurrence: OccurrenceKey,
    ) -> bool:
        """
        Check whether the trigger point was already handled.

        :param schedule_id: id of the schedule.
        :param occurrence: trigger point to check.
        :return: True if it was already dispatched.
        """
        recorded = self.dispatched_occurrences.get(schedule_id)
        return recorded is not None and recorded[0] == occurrence

    def _remember_occurrence(
        self,
        schedule_id: ScheduleId,
        occurrence: OccurrenceKey,
        now: datetime,
    ) -> None:
        """
        Record a trigger point as dispatched.

        :param schedule_id: id of the schedule.
        :param occurrence: dispatched trigger point.
        :param now: current time, used for retention.
        """
        self.dispatched_occurrences[schedule_id] = (occurrence, now)

    def _is_schedule_ready_to_send(
        self,
        task: ScheduledTask,
        now: datetime,
    ) -> bool:
        occurrence = self._occurrence_key(task, now)
        if occurrence is not None and self._occurrence_was_dispatched(
            task.schedule_id,
            occurrence,
        ):
            # This trigger point was already handled. The effective
            # definition belongs to another source now.
            return False

        is_ready_to_send: bool = False

        if not is_ready_to_send and task.cron is not None:
            try:
                is_ready_to_send = is_cron_task_now(
                    cron_value=task.cron,
                    now=now,
                    offset=task.cron_offset,
                    last_run=self.cron_tasks_last_run.get(task.schedule_id),
                )
            except CronValueError:
                logger.warning(
                    "Cannot parse cron: %s for task: %s, schedule_id: %s.",
                    task.cron,
                    task.task_name,
                    task.schedule_id,
                )
            if is_ready_to_send:
                self.cron_tasks_last_run[task.schedule_id] = now

        if not is_ready_to_send and task.interval is not None:
            is_ready_to_send = is_interval_task_now(
                interval_value=task.interval,
                now=now,
                last_run=self.interval_tasks_last_run.get(task.schedule_id),
            )
            if is_ready_to_send:
                self.interval_tasks_last_run[task.schedule_id] = now

        if not is_ready_to_send and task.time is not None:
            is_ready_to_send = is_time_task_now(
                time_value=task.time,
                now=now,
                last_run=self.time_tasks_last_run.get(task.schedule_id),
            )
            if is_ready_to_send:
                self.time_tasks_last_run[task.schedule_id] = now

        if is_ready_to_send and occurrence is not None:
            self._remember_occurrence(task.schedule_id, occurrence, now)

        return is_ready_to_send

    async def run(  # noqa: C901
        self,
        *,
        update_interval: timedelta | None = None,
        loop_interval: timedelta | None = None,
        skip_first_run: bool = False,
        send_timeout: float | None = None,
    ) -> None:
        """
        Runs scheduler loop.

        This function imports taskiq scheduler
        and runs tasks when needed.

        :param update_interval: interval to check for schedule updates.
        :param loop_interval: interval to check tasks to send.
        :param skip_first_run: Wait for the beginning of the next minute
            to skip the first run.
        :param send_timeout: optional per-send timeout (seconds). If set, each
            spawned send task is wrapped in :func:`asyncio.wait_for` with this
            timeout, preventing a single hung ``broker.kick`` from permanently
            blocking subsequent ticks of the same ``schedule_id`` via the
            ``running_schedules`` skip check. Default ``None`` (no timeout —
            backwards-compatible behavior).
        """
        if update_interval is None:
            update_interval = timedelta(minutes=1)
        if loop_interval is None:
            loop_interval = timedelta(seconds=1)
        if send_timeout is not None and send_timeout <= 0:
            raise ValueError("send_timeout must be > 0 when provided")

        running_schedules: dict[ScheduleId, asyncio.Task[Any]] = {}

        self.configure_occurrence_retention(update_interval)
        self.scheduled_tasks = await get_all_schedules(self.scheduler)
        self.scheduled_tasks_updated_at = datetime.now(tz=timezone.utc)
        self._prune_dispatched_occurrences(self.scheduled_tasks_updated_at)

        if skip_first_run:
            self._mark_cron_tasks_as_already_run()

        await _sleep_until_next_second()

        while True:
            now = datetime.now(tz=timezone.utc)
            next_run = (now + loop_interval).replace(microsecond=0)

            if now - self.scheduled_tasks_updated_at >= update_interval:
                await self._update_scheduled_tasks()
                self.scheduled_tasks_updated_at = now

            for source, task_list in self.scheduled_tasks:
                for task in task_list:
                    is_ready_to_send: bool = self._is_schedule_ready_to_send(
                        task=task,
                        now=now,
                    )

                    if is_ready_to_send and task.schedule_id not in running_schedules:
                        if send_timeout is not None:
                            send_coro = send_with_timeout(
                                self.scheduler,
                                source,
                                task,
                                timeout=send_timeout,
                            )
                        else:
                            send_coro = send(self.scheduler, source, task)
                        send_task = self._event_loop.create_task(
                            send_coro,
                            # We need to set the name of the task
                            # to be able to discard its reference
                            # after it is done.
                            name=f"schedule_{task.schedule_id}",
                        )
                        running_schedules[task.schedule_id] = send_task
                        send_task.add_done_callback(
                            lambda task_future: running_schedules.pop(
                                task_future.get_name().removeprefix("schedule_"),
                            ),
                        )

            delay = next_run - datetime.now(tz=timezone.utc)
            logger.debug(
                "Sleeping for %.3f seconds before getting schedules.",
                delay.total_seconds(),
            )
            await asyncio.sleep(delay.total_seconds())


async def _startup_scheduler(scheduler: TaskiqScheduler) -> None:
    """Start sources and scheduler, rolling back sources if startup fails."""
    started_sources: list[ScheduleSource] = []
    try:
        for source in scheduler.sources:
            await source.startup()
            started_sources.append(source)

        logger.info("Starting scheduler.")
        await scheduler.startup()
    except (Exception, asyncio.CancelledError):
        # Only completed startups have a matching shutdown contract.
        for source in reversed(started_sources):
            try:
                await source.shutdown()
            except (Exception, asyncio.CancelledError):
                logger.exception(
                    "Cannot shut down source after startup failure: %s",
                    source,
                )
        raise


async def run_scheduler(args: SchedulerArgs) -> None:
    """
    Run scheduler.

    This function takes all CLI arguments
    and starts the scheduler process.

    :param args: parsed CLI arguments.
    """
    if args.configure_logging:
        basicConfig(
            level=args.log_level,
            format=(
                "[%(asctime)s][%(levelname)-7s]"
                "[%(module)s:%(funcName)s:%(lineno)d]"
                " %(message)s"
            ),
        )
    getLogger("taskiq").setLevel(level=args.log_level)

    if isinstance(args.scheduler, str):
        scheduler = import_object(args.scheduler, app_dir=args.app_dir)
        if inspect.isfunction(scheduler):
            scheduler = scheduler()
    else:
        scheduler = args.scheduler
    if not isinstance(scheduler, TaskiqScheduler):
        logger.error(
            "Imported scheduler is not a subclass of TaskiqScheduler.",
        )
        sys.exit(1)

    scheduler.broker.is_scheduler_process = True
    import_tasks(args.modules, args.tasks_pattern, args.fs_discover)
    update_interval = timedelta(seconds=60)
    if args.update_interval is not None:
        update_interval = timedelta(seconds=args.update_interval)

    loop_interval = timedelta(seconds=1)
    if args.loop_interval is not None:
        loop_interval = timedelta(seconds=args.loop_interval)

    await _startup_scheduler(scheduler)
    logger.info("Startup completed.")

    scheduler_loop = SchedulerLoop(scheduler)
    try:
        await scheduler_loop.run(
            update_interval=update_interval,
            loop_interval=loop_interval,
            skip_first_run=args.skip_first_run,
            send_timeout=args.send_timeout,
        )
    except asyncio.CancelledError:
        logger.info("Shutting down scheduler.")
        await scheduler.shutdown()
        for source in scheduler.sources:
            await source.shutdown()
        logger.info("Scheduler shut down. Good bye!")
