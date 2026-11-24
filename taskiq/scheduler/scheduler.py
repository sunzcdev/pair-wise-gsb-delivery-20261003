from logging import getLogger
from typing import TYPE_CHECKING, Optional

from taskiq.exceptions import ScheduledTaskCancelledError
from taskiq.kicker import AsyncKicker
from taskiq.scheduler.scheduled_task import ScheduledTask
from taskiq.utils import maybe_awaitable

if TYPE_CHECKING:  # pragma: no cover
    from taskiq.abc.broker import AsyncBroker
    from taskiq.abc.schedule_source import ScheduleSource

logger = getLogger(__name__)


class TaskiqScheduler:
    """Scheduler class."""

    def __init__(
        self,
        broker: "AsyncBroker",
        sources: list["ScheduleSource"],
    ) -> None:  # pragma: no cover
        self.broker = broker
        self.sources = sources

    async def startup(self) -> None:  # pragma: no cover
        """
        This method is called on startup.

        Here you can do stuff, like creating
        connections or anything you'd like.
        """
        await self.broker.startup()

    async def on_ready(self, source: "ScheduleSource", task: ScheduledTask) -> None:
        """
        This method is called when task is ready to be enqueued.

        It's triggered on proper time depending on `task.cron`, `task.interval`
        or `task.time` attribute.

        The ``pre_send`` check may wait on an external confirmation, and the
        schedule definition may be replaced in the source while the check is
        in flight. To never dispatch a stale payload, the checked definition
        is verified against the source at a barrier right before sending:
        if it changed, the payload is refreshed and a new ``pre_send`` round
        is started; if it disappeared, the dispatch is dropped. Only a
        definition that is still current at the barrier is sent, and
        ``post_send`` receives exactly the version that was committed to
        the broker.

        :param source: source that triggered this event.
        :param task: task to send
        """
        checked_task = await self._confirm_current_definition(source, task)
        if checked_task is None:
            return
        await (
            AsyncKicker(checked_task.task_name, self.broker, checked_task.labels)
            .with_labels(
                schedule_id=checked_task.schedule_id,
            )
            .with_task_id(task_id=checked_task.task_id)
            .kiq(
                *checked_task.args,
                **checked_task.kwargs,
            )
        )
        await maybe_awaitable(source.post_send(checked_task))

    async def _confirm_current_definition(
        self,
        source: "ScheduleSource",
        task: ScheduledTask,
    ) -> Optional[ScheduledTask]:
        """
        Run ``pre_send`` and confirm the checked definition is still current.

        The source is re-queried after every ``pre_send`` round. If the
        definition for the same ``schedule_id`` no longer matches the one
        that was just checked, the payload is refreshed with the current
        definition and a new round begins, so only an explicitly approved
        and still current schedule instance is handed to the broker.

        :param source: source that triggered this event.
        :param task: task that is about to be sent.
        :return: the verified task to send, or None if it must not be sent.
        """
        while True:
            try:
                await maybe_awaitable(source.pre_send(task))
            except ScheduledTaskCancelledError:
                logger.info("Scheduled task %s has been cancelled.", task.task_name)
                return None
            current = await self._get_current_definition(source, task.schedule_id)
            if current is None:
                logger.info(
                    "Schedule %s for task %s was removed or became unreadable "
                    "before sending. Skipping this dispatch.",
                    task.schedule_id,
                    task.task_name,
                )
                return None
            if current == task:
                return task
            logger.info(
                "Schedule %s for task %s changed before sending. "
                "Refreshing payload and re-checking.",
                task.schedule_id,
                task.task_name,
            )
            task = current

    @staticmethod
    async def _get_current_definition(
        source: "ScheduleSource",
        schedule_id: str,
    ) -> Optional[ScheduledTask]:
        """
        Fetch the current definition of a schedule from its source.

        :param source: source to query.
        :param schedule_id: id of the schedule to find.
        :return: current schedule definition, or None if it is gone
            or the source failed to list schedules.
        """
        try:
            schedules = await source.get_schedules()
        except Exception:
            logger.exception(
                "Cannot verify schedule %s with source %s.",
                schedule_id,
                source,
            )
            return None
        for schedule in schedules:
            if schedule.schedule_id == schedule_id:
                return schedule
        return None

    async def shutdown(self) -> None:
        """Shutdown the scheduler process."""
        await self.broker.shutdown()
