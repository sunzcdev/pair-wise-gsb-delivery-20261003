from __future__ import annotations

from datetime import datetime, timedelta
from itertools import pairwise
from typing import Any

import pytest

from apscheduler import DeserializationError
from apscheduler.abc import Serializer, Trigger
from apscheduler.triggers.interval import IntervalTrigger


def run_to_completion(trigger: Trigger) -> list[datetime]:
    """Collect fire times from the trigger until it returns ``None``."""
    fire_times = []
    while (fire_time := trigger.next()) is not None:
        fire_times.append(fire_time)

    return fire_times


def test_bad_interval():
    exc = pytest.raises(ValueError, IntervalTrigger)
    exc.match("The time interval must be positive")


def test_bad_end_time(timezone):
    start_time = datetime(2020, 5, 16, tzinfo=timezone)
    end_time = datetime(2020, 5, 15, tzinfo=timezone)
    exc = pytest.raises(
        ValueError, IntervalTrigger, seconds=1, start_time=start_time, end_time=end_time
    )
    exc.match("end_time cannot be earlier than start_time")


def test_end_time(timezone, serializer):
    start_time = datetime(2020, 5, 16, 19, 32, 44, 649521, tzinfo=timezone)
    end_time = datetime(2020, 5, 16, 22, 33, 1, tzinfo=timezone)
    interval = timedelta(hours=1, seconds=6)
    trigger = IntervalTrigger(
        start_time=start_time, end_time=end_time, hours=1, seconds=6
    )
    assert trigger.next() == start_time

    if serializer:
        trigger = serializer.deserialize(serializer.serialize(trigger))

    assert trigger.next() == start_time + interval
    assert trigger.next() == start_time + interval * 2
    assert trigger.next() is None


def test_repr(timezone, serializer):
    start_time = datetime(2020, 5, 15, 12, 55, 32, 954032, tzinfo=timezone)
    end_time = datetime(2020, 6, 4, 16, 18, 49, 306942, tzinfo=timezone)
    trigger = IntervalTrigger(
        weeks=1,
        days=2,
        hours=3,
        minutes=4,
        seconds=5,
        microseconds=123525,
        start_time=start_time,
        end_time=end_time,
    )
    if serializer:
        trigger = serializer.deserialize(serializer.serialize(trigger))

    assert repr(trigger) == (
        "IntervalTrigger(weeks=1, days=2, hours=3, minutes=4, seconds=5, "
        "microseconds=123525, start_time='2020-05-15 12:55:32.954032+02:00', "
        "end_time='2020-06-04 16:18:49.306942+02:00')"
    )


@pytest.mark.parametrize("fires_before_save", [2, 5, 8])
def test_dst_fold_back_serialization(
    timezone, serializer: Serializer, fires_before_save: int
):
    """
    Test that a trigger restored from a state saved before, during or after a
    backward DST shift produces the same sequence of fire times (as instants) and
    the same stopping point as a continuously running instance.

    In Europe/Berlin, the clocks were turned back from 03:00 to 02:00 on
    2016-10-30, so the wall clock readings 02:00 - 03:00 occurred twice.

    """
    start_time = datetime(2016, 10, 29, 23, 0, tzinfo=timezone)
    end_time = datetime(2016, 10, 30, 8, 0, tzinfo=timezone)

    # Reference: an instance that runs continuously through the fold
    reference = IntervalTrigger(hours=1, start_time=start_time, end_time=end_time)
    reference_fire_times = run_to_completion(reference)
    reference_timestamps = [t.timestamp() for t in reference_fire_times]

    # The repeated wall clock hour must not cause a fire to be skipped: both
    # occurrences of 02:00 (fold=0 and fold=1) must appear, as distinct instants
    # one hour apart
    first = datetime(2016, 10, 30, 2, 0, tzinfo=timezone, fold=0).timestamp()
    second = datetime(2016, 10, 30, 2, 0, tzinfo=timezone, fold=1).timestamp()
    assert second - first == 3600
    assert len(reference_fire_times) == 11
    assert first in reference_timestamps
    assert second in reference_timestamps
    assert reference_fire_times[-1] == end_time

    # Instance serialized after a few fires, then restored and run to completion
    trigger = IntervalTrigger(hours=1, start_time=start_time, end_time=end_time)
    fire_times = [trigger.next() for _ in range(fires_before_save)]
    trigger = serializer.deserialize(serializer.serialize(trigger))
    fire_times += run_to_completion(trigger)

    # The restored instance must produce the same instants in the same order,
    # stopping at the same point. Instants are compared via timestamps because
    # datetimes restored from a serialized state may carry a fixed-offset tzinfo,
    # and PEP 495 makes == fold-sensitive across differing tzinfo classes.
    assert [t.timestamp() for t in fire_times] == reference_timestamps


def test_dst_spring_forward_serialization(timezone, serializer: Serializer):
    """
    Test that a forward DST shift neither compresses the interval nor breaks
    serialization: the fire times keep their absolute cadence even though the
    local clock readings skip an hour.

    In Europe/Berlin, the clocks were turned forward from 02:00 to 03:00 on
    2016-03-27.

    """
    start_time = datetime(2016, 3, 26, 23, 0, tzinfo=timezone)
    end_time = datetime(2016, 3, 27, 8, 0, tzinfo=timezone)

    reference = IntervalTrigger(hours=1, start_time=start_time, end_time=end_time)
    reference_fire_times = run_to_completion(reference)

    # No wall clock reading between 02:00 and 03:00 exists on that day, and the
    # absolute cadence of one hour is preserved throughout
    assert len(reference_fire_times) == 9
    assert datetime(2016, 3, 27, 1, 0, tzinfo=timezone) in reference_fire_times
    assert datetime(2016, 3, 27, 3, 0, tzinfo=timezone) in reference_fire_times
    for earlier, later in pairwise(reference_fire_times):
        assert later.timestamp() - earlier.timestamp() == 3600

    trigger = IntervalTrigger(hours=1, start_time=start_time, end_time=end_time)
    fire_times = [trigger.next() for _ in range(4)]
    trigger = serializer.deserialize(serializer.serialize(trigger))
    fire_times += run_to_completion(trigger)

    assert [t.timestamp() for t in fire_times] == [
        t.timestamp() for t in reference_fire_times
    ]


@pytest.mark.parametrize("as_string", [True, False], ids=["iso-string", "datetime"])
def test_state_version_1(timezone, as_string: bool):
    """
    Test that a version 1 serialized state can still be deserialized, and that the
    trigger then continues from the restored instant using absolute time
    arithmetic.

    """
    last_fire_time = datetime(2016, 10, 30, 2, 0, tzinfo=timezone, fold=1)
    state: dict[str, Any] = {
        "version": 1,
        "interval": [0, 0, 1, 0, 0, 0],
        "start_time": datetime(2016, 10, 29, 23, 0, tzinfo=timezone),
        "end_time": None,
        "last_fire_time": last_fire_time.isoformat() if as_string else last_fire_time,
    }
    trigger = IntervalTrigger.__new__(IntervalTrigger)
    trigger.__setstate__(state)

    # The last fire time denotes the second occurrence of 02:00 (01:00 UTC), so
    # the next fire time one absolute hour later is 03:00+01:00 (02:00 UTC)
    assert trigger.next() == datetime(2016, 10, 30, 3, 0, tzinfo=timezone)
    assert trigger.next() == datetime(2016, 10, 30, 4, 0, tzinfo=timezone)


@pytest.mark.parametrize("version", [1, 2])
@pytest.mark.parametrize(
    "field", ["start_time", "end_time", "last_fire_time"], ids=lambda v: v
)
def test_state_naive_datetime(timezone, version: int, field: str):
    """
    Test that a serialized state containing a datetime without time zone
    information is rejected with a descriptive error, since the corresponding
    instant cannot be determined unambiguously.

    """
    state: dict[str, Any] = {
        "version": version,
        "interval": [0, 0, 1, 0, 0, 0],
        "start_time": datetime(2016, 10, 29, 23, 0, tzinfo=timezone),
        "end_time": None,
        "last_fire_time": None,
    }
    state[field] = "2016-10-30T02:00:00"
    trigger = IntervalTrigger.__new__(IntervalTrigger)
    exc = pytest.raises(DeserializationError, trigger.__setstate__, state)
    exc.match(f"the {field} value .* has no time zone information")


def test_state_version_3(timezone):
    """Test that a state from a newer version of the trigger is rejected."""
    state: dict[str, Any] = {
        "version": 3,
        "interval": [0, 0, 1, 0, 0, 0],
        "start_time": datetime(2016, 10, 29, 23, 0, tzinfo=timezone),
        "end_time": None,
        "last_fire_time": None,
    }
    trigger = IntervalTrigger.__new__(IntervalTrigger)
    exc = pytest.raises(DeserializationError, trigger.__setstate__, state)
    exc.match("received a serialized state with version 3")
