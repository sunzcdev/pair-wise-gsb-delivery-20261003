from __future__ import annotations

from datetime import UTC, date, datetime, timedelta

import pytest

from apscheduler import MaxIterationsReached
from apscheduler.triggers.calendarinterval import CalendarIntervalTrigger
from apscheduler.triggers.combining import (
    AndTrigger,
    ExclusionWindow,
    OrTrigger,
)
from apscheduler.triggers.cron import CronTrigger
from apscheduler.triggers.date import DateTrigger
from apscheduler.triggers.interval import IntervalTrigger


class TestAndTrigger:
    @pytest.mark.parametrize("threshold", [1, 0])
    def test_two_datetriggers(self, timezone, serializer, threshold):
        date1 = datetime(2020, 5, 16, 14, 17, 30, 254212, tzinfo=timezone)
        date2 = datetime(2020, 5, 16, 14, 17, 31, 254212, tzinfo=timezone)
        trigger = AndTrigger(
            [DateTrigger(date1), DateTrigger(date2)], threshold=threshold
        )
        if serializer:
            trigger = serializer.deserialize(serializer.serialize(trigger))

        if threshold:
            # date2 was within the threshold so it will not be used
            assert trigger.next() == date1

        assert trigger.next() is None

    def test_max_iterations(self, timezone, serializer):
        start_time = datetime(2020, 5, 16, 14, 17, 30, 254212, tzinfo=timezone)
        trigger = AndTrigger(
            [
                IntervalTrigger(seconds=4, start_time=start_time),
                IntervalTrigger(
                    seconds=4, start_time=start_time + timedelta(seconds=2)
                ),
            ]
        )
        if serializer:
            trigger = serializer.deserialize(serializer.serialize(trigger))

        pytest.raises(MaxIterationsReached, trigger.next)

    def test_repr(self, timezone, serializer):
        start_time = datetime(2020, 5, 16, 14, 17, 30, 254212, tzinfo=timezone)
        trigger = AndTrigger(
            [
                IntervalTrigger(seconds=4, start_time=start_time),
                IntervalTrigger(
                    seconds=4, start_time=start_time + timedelta(seconds=2)
                ),
            ]
        )
        if serializer:
            trigger = serializer.deserialize(serializer.serialize(trigger))

        assert repr(trigger) == (
            "AndTrigger([IntervalTrigger(seconds=4, "
            "start_time='2020-05-16 14:17:30.254212+02:00'), IntervalTrigger("
            "seconds=4, start_time='2020-05-16 14:17:32.254212+02:00')], "
            "threshold=1.0, max_iterations=10000)"
        )

    @pytest.mark.parametrize(
        "left_trigger,right_trigger,expected_datetimes",
        [
            (
                IntervalTrigger(hours=6, start_time=datetime(2024, 5, 1, tzinfo=UTC)),
                IntervalTrigger(hours=12, start_time=datetime(2024, 5, 1, tzinfo=UTC)),
                [
                    datetime(2024, 5, 1, 0, tzinfo=UTC),
                    datetime(2024, 5, 1, 12, tzinfo=UTC),
                    datetime(2024, 5, 2, 0, tzinfo=UTC),
                ],
            ),
            (
                IntervalTrigger(days=1, start_time=datetime(2024, 5, 1, tzinfo=UTC)),
                IntervalTrigger(weeks=1, start_time=datetime(2024, 5, 1, tzinfo=UTC)),
                [
                    datetime(2024, 5, 1, tzinfo=UTC),
                    datetime(2024, 5, 8, tzinfo=UTC),
                    datetime(2024, 5, 15, tzinfo=UTC),
                ],
            ),
            (
                CronTrigger(
                    day_of_week="mon-fri",
                    hour="*",
                    timezone=UTC,
                    start_time=datetime(2024, 5, 3, tzinfo=UTC),
                ),
                IntervalTrigger(hours=12, start_time=datetime(2024, 5, 3, tzinfo=UTC)),
                [
                    datetime(2024, 5, 3, 0, tzinfo=UTC),
                    datetime(2024, 5, 3, 12, tzinfo=UTC),
                    datetime(2024, 5, 6, 0, tzinfo=UTC),
                ],
            ),
            (
                CronTrigger(
                    day_of_week="mon-fri",
                    timezone=UTC,
                    start_time=datetime(2024, 5, 13, tzinfo=UTC),
                ),
                IntervalTrigger(days=4, start_time=datetime(2024, 5, 13, tzinfo=UTC)),
                [
                    datetime(2024, 5, 13, tzinfo=UTC),
                    datetime(2024, 5, 17, tzinfo=UTC),
                    datetime(2024, 5, 21, tzinfo=UTC),
                    datetime(2024, 5, 29, tzinfo=UTC),
                ],
            ),
            (
                CalendarIntervalTrigger(
                    months=1,
                    timezone=UTC,
                    start_date=datetime(2024, 1, 1, tzinfo=UTC),
                ),
                CronTrigger(
                    day_of_week="mon-fri",
                    timezone=UTC,
                    start_time=datetime(2024, 1, 1, tzinfo=UTC),
                ),
                [
                    datetime(2024, 1, 1, tzinfo=UTC),
                    datetime(2024, 2, 1, tzinfo=UTC),
                    datetime(2024, 3, 1, tzinfo=UTC),
                    datetime(2024, 4, 1, tzinfo=UTC),
                    datetime(2024, 5, 1, tzinfo=UTC),
                    datetime(2024, 7, 1, tzinfo=UTC),
                    datetime(2024, 8, 1, tzinfo=UTC),
                    datetime(2024, 10, 1, tzinfo=UTC),
                    datetime(2024, 11, 1, tzinfo=UTC),
                ],
            ),
        ],
    )
    def test_overlapping_triggers(
        self, left_trigger, right_trigger, expected_datetimes
    ):
        """
        Verify that the `AndTrigger` fires at the intersection of two triggers.
        """
        and_trigger = AndTrigger([left_trigger, right_trigger])
        for expected_datetime in expected_datetimes:
            next_datetime = and_trigger.next()
            assert next_datetime == expected_datetime


class TestOrTrigger:
    def test_two_datetriggers(self, timezone, serializer):
        date1 = datetime(2020, 5, 16, 14, 17, 30, 254212, tzinfo=timezone)
        date2 = datetime(2020, 5, 18, 15, 1, 53, 940564, tzinfo=timezone)
        trigger = OrTrigger([DateTrigger(date1), DateTrigger(date2)])

        assert trigger.next() == date1

        if serializer:
            trigger = serializer.deserialize(serializer.serialize(trigger))

        assert trigger.next() == date2
        assert trigger.next() is None

    def test_two_interval_triggers(self, timezone, serializer):
        start_time = datetime(2020, 5, 16, 14, 17, 30, 254212, tzinfo=timezone)
        end_time1 = start_time + timedelta(seconds=16)
        end_time2 = start_time + timedelta(seconds=18)
        trigger = OrTrigger(
            [
                IntervalTrigger(seconds=4, start_time=start_time, end_time=end_time1),
                IntervalTrigger(seconds=6, start_time=start_time, end_time=end_time2),
            ]
        )
        if serializer:
            trigger = serializer.deserialize(serializer.serialize(trigger))

        assert trigger.next() == start_time
        assert trigger.next() == start_time + timedelta(seconds=4)
        assert trigger.next() == start_time + timedelta(seconds=6)
        assert trigger.next() == start_time + timedelta(seconds=8)
        assert trigger.next() == start_time + timedelta(seconds=12)
        assert trigger.next() == start_time + timedelta(seconds=16)
        # The end time of the 4 second interval has been reached
        assert trigger.next() == start_time + timedelta(seconds=18)
        # The end time of the 6 second interval has been reached
        assert trigger.next() is None

    def test_repr(self, timezone):
        date1 = datetime(2020, 5, 16, 14, 17, 30, 254212, tzinfo=timezone)
        date2 = datetime(2020, 5, 18, 15, 1, 53, 940564, tzinfo=timezone)
        trigger = OrTrigger([DateTrigger(date1), DateTrigger(date2)])
        assert repr(trigger) == (
            "OrTrigger([DateTrigger('2020-05-16 14:17:30.254212+02:00'), "
            "DateTrigger('2020-05-18 15:01:53.940564+02:00')])"
        )


class TestExclusionWindow:
    def test_invalid_interval(self, timezone):
        start = datetime(2020, 5, 16, tzinfo=timezone)
        with pytest.raises(ValueError, match="end must be later than its start"):
            ExclusionWindow(start, start)

    def test_naive_datetimes_are_localized(self, timezone):
        window = ExclusionWindow(datetime(2020, 5, 16), datetime(2020, 5, 17))
        assert window.start.tzinfo is not None
        assert window.end.tzinfo is not None
        assert datetime(2020, 5, 16, 12, tzinfo=timezone) in window

    def test_half_open_semantics(self, timezone):
        start = datetime(2020, 5, 16, 8, tzinfo=timezone)
        end = datetime(2020, 5, 16, 9, tzinfo=timezone)
        window = ExclusionWindow(start, end)

        # The start is inclusive, the end is exclusive
        assert start in window
        assert end not in window
        assert datetime(2020, 5, 16, 8, 30, tzinfo=timezone) in window
        assert datetime(2020, 5, 16, 7, 59, 59, tzinfo=timezone) not in window

    def test_endpoints_compared_by_instant_across_timezones(self, timezone):
        # Window from 08:00 to 09:00 Berlin (UTC+2 in May)
        window = ExclusionWindow(
            datetime(2020, 5, 16, 8, tzinfo=timezone),
            datetime(2020, 5, 16, 9, tzinfo=timezone),
        )

        # 06:30 UTC is the same instant as 08:30 Berlin -> inside
        assert datetime(2020, 5, 16, 6, 30, tzinfo=UTC) in window
        # 07:00 UTC is the same instant as 09:00 Berlin (the exclusive end)
        assert datetime(2020, 5, 16, 7, tzinfo=UTC) not in window
        # 06:00 UTC is the same instant as 08:00 Berlin (the inclusive start)
        assert datetime(2020, 5, 16, 6, tzinfo=UTC) in window

    def test_serialization(self, timezone, serializer):
        window = ExclusionWindow(
            datetime(2020, 5, 16, 8, tzinfo=timezone),
            datetime(2020, 5, 16, 9, tzinfo=timezone),
        )
        restored = serializer.deserialize(serializer.serialize(window))
        assert restored == window

    def test_repr(self, timezone):
        window = ExclusionWindow(
            datetime(2020, 5, 16, 8, tzinfo=timezone),
            datetime(2020, 5, 16, 9, tzinfo=timezone),
        )
        assert repr(window) == (
            "ExclusionWindow('2020-05-16T08:00:00+02:00', "
            "'2020-05-16T09:00:00+02:00')"
        )


class TestExcludedTriggers:
    @pytest.fixture
    def monthly_trigger(self, timezone):
        return CalendarIntervalTrigger(
            months=1,
            hour=9,
            start_date=date(2026, 10, 15),
            timezone=timezone,
        )

    @pytest.fixture
    def exclusion_windows(self, timezone):
        # Three windows: the first two are adjacent, the third overlaps the
        # second one. Together they cover both the November and the December
        # fire time (2026-11-15 09:00 and 2026-12-15 09:00, Berlin time).
        return [
            ExclusionWindow(
                datetime(2026, 11, 14, tzinfo=timezone),
                datetime(2026, 11, 16, tzinfo=timezone),
            ),
            ExclusionWindow(
                datetime(2026, 11, 16, tzinfo=timezone),
                datetime(2026, 12, 14, 12, tzinfo=timezone),
            ),
            ExclusionWindow(
                datetime(2026, 12, 1, tzinfo=timezone),
                datetime(2027, 1, 2, tzinfo=timezone),
            ),
        ]

    @pytest.mark.parametrize("trigger_class", [OrTrigger, AndTrigger])
    def test_skips_candidates_inside_windows(
        self, trigger_class, monthly_trigger, exclusion_windows, timezone
    ):
        trigger = trigger_class(
            [monthly_trigger], exclusion_windows=exclusion_windows
        )

        # October is before the windows, then November and December are skipped,
        # and the schedule resumes in January (crossing a month and year boundary)
        assert trigger.next() == datetime(2026, 10, 15, 9, tzinfo=timezone)
        assert trigger.next() == datetime(2027, 1, 15, 9, tzinfo=timezone)
        assert trigger.next() == datetime(2027, 2, 15, 9, tzinfo=timezone)

    @pytest.mark.parametrize("trigger_class", [OrTrigger, AndTrigger])
    def test_window_end_is_exclusive(
        self, trigger_class, timezone, serializer
    ):
        trigger_inner = IntervalTrigger(
            hours=1, start_time=datetime(2020, 5, 16, 7, tzinfo=timezone)
        )
        window = ExclusionWindow(
            datetime(2020, 5, 16, 8, tzinfo=timezone),
            datetime(2020, 5, 16, 10, tzinfo=timezone),
        )
        trigger = trigger_class([trigger_inner], exclusion_windows=[window])
        if serializer:
            trigger = serializer.deserialize(serializer.serialize(trigger))

        assert trigger.next() == datetime(2020, 5, 16, 7, tzinfo=timezone)
        # 08:00 and 09:00 are skipped; 10:00 equals the exclusive end so it runs
        assert trigger.next() == datetime(2020, 5, 16, 10, tzinfo=timezone)

    @pytest.mark.parametrize("trigger_class", [OrTrigger, AndTrigger])
    def test_window_accepted_as_pair_or_dict(
        self, trigger_class, timezone, exclusion_windows
    ):
        pair_window = (
            datetime(2026, 11, 14, tzinfo=timezone),
            datetime(2026, 11, 16, tzinfo=timezone),
        )
        dict_window = {
            "start": datetime(2026, 12, 1, tzinfo=timezone),
            "end": datetime(2027, 1, 2, tzinfo=timezone),
        }
        trigger = trigger_class(
            [
                CalendarIntervalTrigger(
                    months=1,
                    hour=9,
                    start_date=date(2026, 10, 15),
                    timezone=timezone,
                )
            ],
            exclusion_windows=[pair_window, dict_window],
        )
        assert all(
            isinstance(window, ExclusionWindow)
            for window in trigger.exclusion_windows
        )

    @pytest.mark.parametrize("trigger_class", [OrTrigger, AndTrigger])
    def test_max_iterations_bounds_consecutive_skips(
        self, trigger_class, timezone
    ):
        inner = IntervalTrigger(
            hours=1, start_time=datetime(2020, 5, 16, tzinfo=timezone)
        )
        window = ExclusionWindow(
            datetime(2020, 1, 1, tzinfo=UTC),
            datetime(2030, 1, 1, tzinfo=UTC),
        )
        trigger = trigger_class(
            [inner], exclusion_windows=[window], max_iterations=3
        )
        with pytest.raises(MaxIterationsReached):
            trigger.next()

    @pytest.mark.parametrize("trigger_class", [OrTrigger, AndTrigger])
    def test_exhausted_trigger_inside_window_returns_none(
        self, trigger_class, timezone
    ):
        inner = CalendarIntervalTrigger(
            months=1,
            hour=9,
            start_date=date(2026, 10, 15),
            end_date=date(2027, 6, 15),
            timezone=timezone,
        )
        window = ExclusionWindow(
            datetime(2020, 1, 1, tzinfo=UTC),
            datetime(2030, 1, 1, tzinfo=UTC),
        )
        trigger = trigger_class([inner], exclusion_windows=[window])
        assert trigger.next() is None

    def test_or_trigger_serialization_roundtrip(
        self, monthly_trigger, exclusion_windows, serializer, timezone
    ):
        trigger = OrTrigger(
            [monthly_trigger], exclusion_windows=exclusion_windows
        )
        restored = serializer.deserialize(serializer.serialize(trigger))
        assert restored.exclusion_windows == exclusion_windows
        assert restored.max_iterations == 10000
        assert restored.next() == datetime(2026, 10, 15, 9, tzinfo=timezone)
        assert restored.next() == datetime(2027, 1, 15, 9, tzinfo=timezone)

    def test_and_trigger_serialization_roundtrip(
        self, monthly_trigger, exclusion_windows, serializer, timezone
    ):
        trigger = AndTrigger(
            [monthly_trigger], exclusion_windows=exclusion_windows
        )
        restored = serializer.deserialize(serializer.serialize(trigger))
        assert restored.exclusion_windows == exclusion_windows
        assert restored.max_iterations == 10000
        assert restored.next() == datetime(2026, 10, 15, 9, tzinfo=timezone)
        assert restored.next() == datetime(2027, 1, 15, 9, tzinfo=timezone)

    def test_resume_after_persistence_matches_original(
        self, monthly_trigger, exclusion_windows, serializer, timezone
    ):
        """
        Build a cross-month plan, consume one fire time, persist the trigger
        before the skipped fire times and read it back. The first runnable
        time after the restore must match that of a fresh, equally advanced
        original instance.
        """
        original = OrTrigger(
            [
                CalendarIntervalTrigger(
                    months=1,
                    hour=9,
                    start_date=date(2026, 10, 15),
                    timezone=timezone,
                )
            ],
            exclusion_windows=exclusion_windows,
        )
        # Advance both instances once (October fire time) so the next
        # candidates are the ones covered by the exclusion windows
        original.next()

        persisted = OrTrigger(
            [
                CalendarIntervalTrigger(
                    months=1,
                    hour=9,
                    start_date=date(2026, 10, 15),
                    timezone=timezone,
                )
            ],
            exclusion_windows=exclusion_windows,
        )
        persisted.next()
        restored = serializer.deserialize(serializer.serialize(persisted))

        # November and December are skipped; the first runnable moment is
        # January 15th, identical on both the original and the restored copy
        expected = datetime(2027, 1, 15, 9, tzinfo=timezone)
        assert restored.next() == expected
        assert original.next() == expected
        assert restored.next() == original.next()

    def test_legacy_v1_state_without_windows(self, monthly_trigger, timezone):
        """Old serialized states (version 1) had no exclusion window data."""
        from apscheduler._marshalling import marshal_object

        state = {
            "version": 1,
            "triggers": [marshal_object(monthly_trigger)],
            "next_fire_times": [],
        }
        restored = OrTrigger.__new__(OrTrigger)
        restored.__setstate__(state)
        assert restored.exclusion_windows == []
        assert restored.max_iterations == 10000
        assert restored.next() == datetime(2026, 10, 15, 9, tzinfo=timezone)

    def test_legacy_and_trigger_v1_state(self, monthly_trigger, timezone):
        from apscheduler._marshalling import marshal_object

        state = {
            "version": 1,
            "triggers": [marshal_object(monthly_trigger)],
            "next_fire_times": [],
            "threshold": timedelta(seconds=1),
            "max_iterations": 10000,
        }
        restored = AndTrigger.__new__(AndTrigger)
        restored.__setstate__(state)
        assert restored.exclusion_windows == []
        assert restored.next() == datetime(2026, 10, 15, 9, tzinfo=timezone)

    def test_repr_with_windows(self, monthly_trigger, exclusion_windows):
        trigger = OrTrigger(
            [monthly_trigger], exclusion_windows=exclusion_windows
        )
        rendered = repr(trigger)
        assert rendered.startswith("OrTrigger([")
        assert "exclusion_windows=[ExclusionWindow(" in rendered

        and_trigger = AndTrigger(
            [monthly_trigger], exclusion_windows=exclusion_windows
        )
        rendered = repr(and_trigger)
        assert "threshold=1.0" in rendered
        assert "max_iterations=10000" in rendered
        assert "exclusion_windows=[ExclusionWindow(" in rendered
