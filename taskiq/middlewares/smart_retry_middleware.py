import asyncio
import datetime
import random
from collections.abc import Iterable
from collections.abc import Set as AbstractSet
from logging import getLogger
from typing import Any

from taskiq.abc.middleware import RetryPhase
from taskiq.abc.schedule_source import ScheduleSource
from taskiq.kicker import AsyncKicker
from taskiq.message import TaskiqMessage
from taskiq.middlewares.retry_middleware import BaseRetryMiddleware

__all__ = ("SmartRetryMiddleware",)

_logger = getLogger("taskiq.smart_retry_middleware")


class SmartRetryMiddleware(BaseRetryMiddleware):
    """
    Middleware to retry failed tasks with delays.

    This middleware retries failed tasks with support for:
    - max retries
    - delay
    - jitter
    - exponential backoff
    - phase selection via ``retry_on``

    Failures while saving the result are retried in place after the
    calculated delay: only the result backend is called again, the task
    function itself is not executed a second time.
    """

    def __init__(
        self,
        default_retry_count: int = 3,
        default_retry_label: bool = False,
        no_result_on_retry: bool = True,
        default_delay: float = 5,
        use_jitter: bool = False,
        use_delay_exponent: bool = False,
        max_delay_exponent: float = 60,
        schedule_source: ScheduleSource | None = None,
        types_of_exceptions: Iterable[type[BaseException]] | None = None,
        retry_on: Iterable[RetryPhase | str] | None = None,
        **phase_aliases: Any,
    ) -> None:
        """
        Initialize retry middleware.

        :param default_retry_count: Default max retries if not specified.
        :param default_retry_label: Whether to retry tasks by default.
        :param no_result_on_retry: Replace result with NoResultError on retry.
        :param default_delay: Delay in seconds before retrying.
        :param use_jitter: Add random jitter to retry delay.
        :param use_delay_exponent: Apply exponential backoff to delay.
        :param max_delay_exponent: Maximum allowed delay when using backoff.
        :param schedule_source: Schedule source to use for scheduling.
            If None, the default broker will be used.
        :param types_of_exceptions: Types of exceptions to retry from.
        :param retry_on: Phases that are allowed to be retried. Defaults
            to dependency preparation and task execution.
        :param phase_aliases: Alternative names for ``retry_on``.
        """
        super().__init__(
            default_retry_count=default_retry_count,
            default_retry_label=default_retry_label,
            no_result_on_retry=no_result_on_retry,
            types_of_exceptions=types_of_exceptions,
            retry_on=retry_on,
            default_delay=default_delay,
            **phase_aliases,
        )
        self.use_jitter = use_jitter
        self.use_delay_exponent = use_delay_exponent
        self.max_delay_exponent = max_delay_exponent
        self.schedule_source = schedule_source

        if not isinstance(schedule_source, (ScheduleSource, type(None))):
            raise TypeError(
                "schedule_source must be an instance of ScheduleSource or None",
            )

    def is_retry_on_error(self, message: TaskiqMessage) -> bool:
        """
        Check if retry is enabled for this task.

        Looks for `retry_on_error` label, falls back to default.

        :param message: Original task message.
        :return: True if should retry on error.
        """
        return self._is_retry_enabled(message)

    def make_delay(self, message: TaskiqMessage, retries: int) -> float:
        """
        Calculate retry delay.

        Includes jitter and exponential backoff if enabled.

        :param message: Task message.
        :param retries: Current retry count.
        :return: Delay in seconds.
        """
        delay = float(message.labels.get("delay", self.default_delay))
        if self.use_delay_exponent:
            delay = min(delay * retries, self.max_delay_exponent)

        if self.use_jitter:
            delay += random.random()  # noqa: S311

        return delay

    async def on_send(
        self,
        kicker: AsyncKicker[Any, Any],
        message: TaskiqMessage,
        delay: float,
    ) -> None:
        """Execute the task with a delay."""
        if self.schedule_source is None:
            await kicker.with_labels(delay=delay).kiq(*message.args, **message.kwargs)
        else:
            target_time = datetime.datetime.now(
                datetime.timezone.utc,
            ) + datetime.timedelta(
                seconds=delay,
            )
            await kicker.schedule_by_time(
                self.schedule_source,
                target_time,
                *message.args,
                **message.kwargs,
            )

    async def send_retry(
        self,
        kicker: AsyncKicker[Any, Any],
        message: TaskiqMessage,
        retries: int,
    ) -> None:
        """Re-send the failed message using the calculated delay."""
        delay = self.make_delay(message, retries)

        _logger.info(
            "Task %s failed. Retrying %d/%d in %.2f seconds.",
            message.task_name,
            retries,
            int(message.labels.get("max_retries", self.default_retry_count)),
            delay,
        )

        await self.on_send(kicker, message, delay)

    async def delay_before_save_retry(
        self,
        message: TaskiqMessage,
        retries: int,
    ) -> None:
        """Wait for the calculated delay before retrying a result save."""
        delay = self.make_delay(message, retries)
        _logger.info(
            "Task %s result saving failed. Retrying save %d/%d in %.2f seconds.",
            message.task_name,
            retries,
            int(message.labels.get("max_retries", self.default_retry_count)),
            delay,
        )
        if delay > 0:
            await asyncio.sleep(delay)

    @classmethod
    def task_only_labels(cls) -> AbstractSet[str]:
        """
        Adds ``types_of_exceptions`` to the set of task-only labels.

        Per-task ``types_of_exceptions`` set via the task decorator is read
        from the registered task object and cannot be properly serialized.
        """
        return super().task_only_labels()
