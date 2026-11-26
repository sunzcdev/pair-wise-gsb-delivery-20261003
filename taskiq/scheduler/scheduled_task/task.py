import sys
import uuid
from datetime import datetime, timedelta
from typing import Any

from pydantic import BaseModel, Field, model_validator

from taskiq.scheduler.scheduled_task.validators import (
    CronOffset,
    validate_interval_value,
)

if sys.version_info >= (3, 11):
    from typing import Self
else:
    from typing_extensions import Self

#: Fields a non partial, non cancelled definition must declare.
REQUIRED_FIELDS = frozenset({"task_name", "labels", "args", "kwargs"})


class ScheduledTask(BaseModel):
    """
    Abstraction over task schedule.

    A definition coming from a source may be complete, partial or an
    explicit cancellation of a `schedule_id`:

    * a complete definition declares at least one of `cron`, `time`
      or `interval`;
    * a `partial` definition may omit the trigger and every optional
      field. Such fields are inherited from definitions of the same
      `schedule_id` declared by lower priority sources;
    * a `cancelled` definition removes the `schedule_id` for the
      source that declares it, and prevents lower priority sources
      from defining it.
    """

    task_name: str = ""
    labels: dict[str, Any] = Field(default_factory=dict)
    args: list[Any] = Field(default_factory=list)
    kwargs: dict[str, Any] = Field(default_factory=dict)
    task_id: str | None = None
    schedule_id: str = Field(default_factory=lambda: uuid.uuid4().hex)
    cron: str | None = None
    cron_offset: CronOffset | None = None
    time: datetime | None = None
    interval: int | timedelta | None = None
    cancelled: bool = False
    partial: bool = False

    @model_validator(mode="after")
    def __check(self) -> Self:
        """Validate.

        This method validates,
        that either `cron`, `interval` or `time` field is present.
        For interval tasks, validates that interval is at least 1 second
        and has no fractional seconds.

        Cancellations and partial definitions are allowed to omit the
        trigger and any other field, because they either remove a
        schedule or inherit the omitted fields from another source.
        Only fields explicitly passed by the caller count as declared,
        which is what makes the inheritance of missing fields possible.

        :raises ValueError: if cron, interval and time are none, or interval is invalid.
        """
        incomplete_layer = bool(REQUIRED_FIELDS - self.model_fields_set)
        if self.cancelled or self.partial or incomplete_layer:
            validate_interval_value(self.interval)
            return self

        if self.cron is None and self.time is None and self.interval is None:
            raise ValueError("Either cron, interval, or datetime must be present.")

        validate_interval_value(self.interval)
        return self


class ScheduledTaskCancellation(ScheduledTask):
    """
    Explicit cancellation of a schedule id declared by a source.

    A cancellation is not a missing definition: it removes the
    `schedule_id` for the declaring source and every source below it,
    even if those sources still provide the schedule.
    """

    cancelled: bool = True


# Common naming alternatives for callers discovering the tombstone type.
CancelledSchedule = ScheduledTaskCancellation
ScheduleCancellation = ScheduledTaskCancellation
