from __future__ import annotations

from datetime import UTC, datetime, timedelta
from typing import Any

import attrs
from attr.validators import instance_of, optional

from .._converters import as_aware_datetime, as_datetime
from .._exceptions import DeserializationError
from .._utils import require_state_version
from ..abc import Trigger


def _require_aware_datetime(value: datetime | str, field_name: str) -> datetime:
    """
    Convert a serialized state value to an aware datetime.

    Strings are parsed as ISO 8601. Naive datetimes are rejected because the
    actual instant they denote cannot be determined unambiguously (the offset is
    unknown, and during a backward DST shift the same wall clock reading occurs
    twice).
    """
    value = as_datetime(value)
    if not isinstance(value, datetime):
        raise DeserializationError(
            f"Cannot deserialize IntervalTrigger: the {field_name} value "
            f"({value!r}) is not a date/time"
        )

    if value.tzinfo is None or value.utcoffset() is None:
        raise DeserializationError(
            f"Cannot deserialize IntervalTrigger: the {field_name} value "
            f"({value.isoformat()!r}) has no time zone information, so the "
            f"corresponding instant cannot be determined unambiguously"
        )

    return value


@attrs.define(kw_only=True)
class IntervalTrigger(Trigger):
    """
    Triggers on specified intervals.

    The first trigger time is on ``start_time`` which is the  moment the trigger was
    created unless specifically overridden. If ``end_time`` is specified, the last
    trigger time will be at or before that time. If no ``end_time`` has been given, the
    trigger will produce new trigger times as long as the resulting datetimes are valid
    datetimes in Python.

    The interval is always measured in absolute time. This means that the interval
    does not stretch or shrink across daylight saving time transitions: on a backward
    shift (fold), where a wall clock reading occurs twice, each occurrence gets its
    own fire time if the interval lands on it, and on a forward shift (gap), the fire
    times keep their absolute cadence even though the local clock readings skip an
    hour. As a consequence, a trigger restored from a serialized state always
    continues with the same sequence of fire times (as instants) as an instance that
    was never serialized, regardless of which side of a DST transition the state was
    saved on.

    .. note:: The serialized state format is at version 2. Version 1 states (where
        the fire times were computed using wall clock arithmetic) are still
        accepted; the stored ``last_fire_time`` is then interpreted as the actual
        instant of the last fire, and subsequent fire times are computed in absolute
        time.

    :param weeks: number of weeks to wait
    :param days: number of days to wait
    :param hours: number of hours to wait
    :param minutes: number of minutes to wait
    :param seconds: number of seconds to wait
    :param microseconds: number of microseconds to wait
    :param start_time: first trigger date/time (defaults to current date/time if
        omitted)
    :param end_time: latest possible date/time to trigger on
    """

    weeks: float = 0
    days: float = 0
    hours: float = 0
    minutes: float = 0
    seconds: float = 0
    microseconds: float = 0
    start_time: datetime = attrs.field(
        converter=as_aware_datetime,
        factory=datetime.now,
        validator=instance_of(datetime),
    )
    end_time: datetime | None = attrs.field(
        converter=as_aware_datetime,
        validator=optional(instance_of(datetime)),
        default=None,
    )
    _interval: timedelta = attrs.field(init=False, eq=False, repr=False)
    _last_fire_time: datetime | None = attrs.field(
        init=False, eq=False, converter=as_aware_datetime, default=None
    )

    def __attrs_post_init__(self) -> None:
        self._interval = timedelta(
            weeks=self.weeks,
            days=self.days,
            hours=self.hours,
            minutes=self.minutes,
            seconds=self.seconds,
            microseconds=self.microseconds,
        )

        if self._interval.total_seconds() <= 0:
            raise ValueError("The time interval must be positive")

        if self.end_time and self.end_time < self.start_time:
            raise ValueError("end_time cannot be earlier than start_time")

    def next(self) -> datetime | None:
        if self._last_fire_time is None:
            next_fire_time = self.start_time
        else:
            # Add the interval in absolute (UTC) time so that DST transitions
            # neither stretch nor shrink the interval, and so that a wall clock
            # reading repeated by a backward shift yields a separate fire time
            # for each occurrence instead of skipping one
            next_fire_time = (
                self._last_fire_time.astimezone(UTC) + self._interval
            ).astimezone(self._last_fire_time.tzinfo)

        if self.end_time is None or next_fire_time <= self.end_time:
            self._last_fire_time = next_fire_time
            return next_fire_time
        else:
            return None

    def __getstate__(self) -> dict[str, Any]:
        return {
            "version": 2,
            "interval": [
                self.weeks,
                self.days,
                self.hours,
                self.minutes,
                self.seconds,
                self.microseconds,
            ],
            "start_time": self.start_time,
            "end_time": self.end_time,
            "last_fire_time": self._last_fire_time,
        }

    def __setstate__(self, state: dict[str, Any]) -> None:
        require_state_version(self, state, 2)
        (
            self.weeks,
            self.days,
            self.hours,
            self.minutes,
            self.seconds,
            self.microseconds,
        ) = state["interval"]
        self.start_time = _require_aware_datetime(state["start_time"], "start_time")
        self.end_time = (
            _require_aware_datetime(state["end_time"], "end_time")
            if state["end_time"] is not None
            else None
        )
        self._last_fire_time = (
            _require_aware_datetime(state["last_fire_time"], "last_fire_time")
            if state["last_fire_time"] is not None
            else None
        )
        self._interval = timedelta(
            weeks=self.weeks,
            days=self.days,
            hours=self.hours,
            minutes=self.minutes,
            seconds=self.seconds,
            microseconds=self.microseconds,
        )

    def __repr__(self) -> str:
        fields = []
        for field in "weeks", "days", "hours", "minutes", "seconds", "microseconds":
            value = getattr(self, field)
            if value > 0:
                fields.append(f"{field}={value}")

        fields.append(f"start_time='{self.start_time}'")
        if self.end_time:
            fields.append(f"end_time='{self.end_time}'")

        return f"{self.__class__.__name__}({', '.join(fields)})"
