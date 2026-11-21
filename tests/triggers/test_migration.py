from __future__ import annotations

import copy
from datetime import UTC, date, datetime
from zoneinfo import ZoneInfo

import attrs
import pytest

from apscheduler._exceptions import MaxIterationsReached
from apscheduler.triggers.calendarinterval import CalendarIntervalTrigger
from apscheduler.triggers.cron import CronTrigger
from apscheduler.triggers.interval import IntervalTrigger
from apscheduler.triggers.migration import (
    FireTime,
    LocalTimeResolution,
    LocalTimeRule,
    SkippedLocalTime,
    TimezoneMigrationPreview,
    preview_timezone_migration,
)

berlin = ZoneInfo("Europe/Berlin")
nuuk = ZoneInfo("America/Nuuk")
eastern = ZoneInfo("US/Eastern")

# Nuuk springs forward on 2026-03-28 at 23:00 local (23:00-23:59 is missing).
NUUK_GAP = datetime(2026, 3, 28, 23, 30, tzinfo=nuuk)
# Nuuk falls back on 2026-10-24 at 23:00 local (23:00-23:59 is repeated).
NUUK_FALLBACK_WALL = datetime(2026, 10, 24, 23, 30)


def _cron_monthly(
    timezone: ZoneInfo, *, day: int = 28, hour: int = 23, minute: int = 30
) -> CronTrigger:
    return CronTrigger(
        day=day,
        hour=hour,
        minute=minute,
        start_time=datetime(2026, 1, day, hour, minute, tzinfo=timezone),
        timezone=timezone,
    )


def _calendar_monthly(
    timezone: ZoneInfo, *, day: int = 28, hour: int = 23, minute: int = 30
) -> CalendarIntervalTrigger:
    return CalendarIntervalTrigger(
        months=1,
        hour=hour,
        minute=minute,
        start_date=date(2026, 1, day),
        timezone=timezone,
    )


def test_unsupported_trigger_type() -> None:
    trigger = IntervalTrigger(hours=1)
    with pytest.raises(TypeError, match="only supported for CronTrigger"):
        preview_timezone_migration(trigger, berlin)


def test_invalid_rule() -> None:
    trigger = _cron_monthly(berlin)
    with pytest.raises(TypeError, match="invalid local time rule"):
        # type: ignore[arg-type]
        preview_timezone_migration(trigger, berlin, rule="preserve_wall_time")


@pytest.mark.parametrize(
    "kwargs",
    [
        {"range_start": datetime(2026, 1, 1)},
        {"range_end": datetime(2026, 1, 1)},
    ],
    ids=["naive_start", "naive_end"],
)
def test_naive_range_bounds_rejected(kwargs) -> None:
    trigger = _cron_monthly(berlin)
    with pytest.raises(ValueError, match="must be timezone aware"):
        preview_timezone_migration(trigger, berlin, **kwargs)


def test_inverted_range_rejected() -> None:
    trigger = _cron_monthly(berlin)
    with pytest.raises(ValueError, match="range_end cannot be earlier"):
        preview_timezone_migration(
            trigger,
            berlin,
            range_start=datetime(2027, 1, 1, tzinfo=UTC),
            range_end=datetime(2026, 1, 1, tzinfo=UTC),
        )


def test_exhausted_trigger_rejected() -> None:
    trigger = CronTrigger(
        year=2020,
        month=1,
        day=1,
        start_time=datetime(2020, 1, 1, tzinfo=UTC),
        end_time=datetime(2020, 2, 1, tzinfo=UTC),
        timezone=UTC,
    )
    assert trigger.next() is not None
    with pytest.raises(ValueError, match="no next fire time"):
        preview_timezone_migration(trigger, berlin)


def test_preview_is_read_only_for_cron() -> None:
    trigger = _cron_monthly(berlin)
    # Advance the trigger a few times, as would happen in a live schedule.
    for _ in range(3):
        trigger.next()

    # Capture where the trigger currently stands before the preview.
    last_fire_before = trigger._last_fire_time
    expected_next = copy.deepcopy(trigger).next()
    preview = preview_timezone_migration(
        trigger, nuuk, range_end=datetime(2027, 1, 1, tzinfo=UTC)
    )

    # The original trigger's internal state is untouched by the preview...
    assert trigger._last_fire_time == last_fire_before
    # ...and a deep copy resumes from exactly the same position.
    assert copy.deepcopy(trigger).next() == expected_next
    assert preview.boundary == expected_next


def test_preview_is_read_only_for_calendar() -> None:
    trigger = _calendar_monthly(berlin)
    trigger.next()

    # Capture where the trigger currently stands before the preview.
    last_fire_before = trigger._last_fire_date
    expected_next = copy.deepcopy(trigger).next()
    preview = preview_timezone_migration(
        trigger, nuuk, range_end=datetime(2027, 1, 1, tzinfo=UTC)
    )
    # The preview ran on copies and did not move the original again.
    assert trigger._last_fire_date == last_fire_before
    assert copy.deepcopy(trigger).next() == expected_next
    assert preview.boundary == expected_next


def test_cron_preserves_wall_time_and_reports_both_zoned_times() -> None:
    trigger = _cron_monthly(berlin, hour=6, minute=0)
    preview = preview_timezone_migration(
        trigger, nuuk, range_end=datetime(2026, 3, 1, tzinfo=UTC)
    )

    assert isinstance(preview, TimezoneMigrationPreview)
    assert preview.source_timezone == berlin
    assert preview.target_timezone == nuuk
    assert preview.rule is LocalTimeRule.preserve_wall_time
    assert len(preview.old_fire_times) == 2
    assert len(preview.new_fire_times) == 2

    old, new = preview.old_fire_times[0], preview.new_fire_times[0]
    assert isinstance(old, FireTime)
    # The wall clock time of day is preserved...
    assert (old.local_time.hour, old.local_time.minute) == (6, 0)
    assert (new.local_time.hour, new.local_time.minute) == (6, 0)
    # ...but the UTC instant moves with the target zone's offset.
    assert old.local_time.tzinfo == berlin
    assert new.local_time.tzinfo == nuuk
    assert old.utc_time.tzinfo == UTC
    assert new.utc_time == new.local_time.astimezone(UTC)
    assert old.utc_time != new.utc_time
    assert old.resolution is LocalTimeResolution.normal
    assert new.resolution is LocalTimeResolution.normal


def test_cron_skips_nonexistent_local_time() -> None:
    trigger = _cron_monthly(berlin)  # 23:30 wall, missing in Nuuk on 2026-03-28
    preview = preview_timezone_migration(
        trigger,
        nuuk,
        range_start=datetime(2026, 3, 1, tzinfo=UTC),
        range_end=datetime(2026, 6, 1, tzinfo=UTC),
    )

    assert len(preview.skipped_local_times) == 1
    skipped = preview.skipped_local_times[0]
    assert isinstance(skipped, SkippedLocalTime)
    assert skipped.local_time.replace(tzinfo=None) == NUUK_GAP.replace(tzinfo=None)
    assert skipped.resolution is LocalTimeResolution.skipped_to_next_match
    assert skipped.gap_start_utc < skipped.gap_end_utc

    # The March slot is absent from the new sequence; the February slot (which
    # precedes the range start but is still at or after the January boundary)
    # remains, followed by April.
    new_months = [fire.local_time.month for fire in preview.new_fire_times]
    assert 3 not in new_months
    assert new_months == [2, 4, 5]

    # Cross-check against an independently migrated trigger anchored at the
    # same boundary and filtered to the same range.
    direct = attrs.evolve(
        trigger,
        timezone=nuuk,
        start_time=preview.boundary.astimezone(nuuk),
    )
    direct._last_fire_time = None
    direct_expected = []
    while (fire := direct.next()) <= datetime(2026, 6, 1, tzinfo=UTC):
        if fire >= datetime(2026, 3, 1, tzinfo=UTC):
            direct_expected.append(fire)

    assert [fire.local_time for fire in preview.new_fire_times] == direct_expected


def test_calendar_discards_nonexistent_date() -> None:
    trigger = _calendar_monthly(berlin)
    preview = preview_timezone_migration(
        trigger,
        nuuk,
        range_start=datetime(2026, 3, 1, tzinfo=UTC),
        range_end=datetime(2026, 6, 1, tzinfo=UTC),
    )

    assert len(preview.skipped_local_times) == 1
    skipped = preview.skipped_local_times[0]
    assert skipped.resolution is LocalTimeResolution.date_discarded

    new_months = [fire.local_time.month for fire in preview.new_fire_times]
    assert 3 not in new_months
    assert new_months == [2, 4, 5]

    direct = attrs.evolve(trigger, timezone=nuuk)
    direct_expected = []
    while (fire := direct.next()) <= datetime(2026, 6, 1, tzinfo=UTC):
        if fire >= datetime(2026, 3, 1, tzinfo=UTC):
            direct_expected.append(fire)

    assert [fire.local_time for fire in preview.new_fire_times] == direct_expected

def test_repeated_local_time_uses_earlier_occurrence() -> None:
    # US/Eastern repeats 01:30 local on 2026-11-01.
    trigger = CronTrigger(
        day=1,
        hour=1,
        minute=30,
        start_time=datetime(2026, 9, 1, 1, 30, tzinfo=eastern),
        timezone=eastern,
    )
    preview = preview_timezone_migration(
        trigger,
        eastern,
        range_start=datetime(2026, 10, 15, tzinfo=UTC),
        range_end=datetime(2026, 12, 1, tzinfo=UTC),
    )

    november = [
        fire
        for fire in preview.new_fire_times
        if fire.local_time.month == 11 and fire.local_time.day == 1
    ]
    assert len(november) == 2
    earlier, later = november
    assert earlier.fold == 0
    assert earlier.resolution is LocalTimeResolution.earlier_occurrence
    assert later.fold == 1
    assert later.resolution is LocalTimeResolution.normal
    assert earlier.utc_time < later.utc_time
    assert preview.skipped_local_times == []


def test_calendar_repeated_local_time_uses_earlier_occurrence() -> None:
    # 23:30 is repeated in Nuuk on 2026-10-24.
    trigger = CalendarIntervalTrigger(
        months=1,
        hour=23,
        minute=30,
        start_date=date(2026, 9, 24),
        timezone=nuuk,
    )
    preview = preview_timezone_migration(
        trigger,
        nuuk,
        range_start=datetime(2026, 10, 1, tzinfo=UTC),
        range_end=datetime(2026, 11, 1, tzinfo=UTC),
    )
    october = preview.new_fire_times[0]
    assert october.local_time.replace(tzinfo=None) == NUUK_FALLBACK_WALL
    assert october.fold == 0
    assert october.resolution is LocalTimeResolution.earlier_occurrence


def test_range_filters_both_sequences() -> None:
    trigger = _cron_monthly(berlin)
    range_start = datetime(2026, 4, 10, tzinfo=UTC)
    range_end = datetime(2026, 7, 10, tzinfo=UTC)
    preview = preview_timezone_migration(
        trigger, nuuk, range_start=range_start, range_end=range_end
    )

    for fire in preview.old_fire_times + preview.new_fire_times:
        assert range_start <= fire.utc_time <= range_end

    # Berlin is UTC+2 in summer: the July 28th 23:30 local fire (21:30 UTC)
    # falls after the July 10th range end, so only three months are included.
    assert [fire.local_time.month for fire in preview.old_fire_times] == [
        4,
        5,
        6,
    ]


def test_start_at_current_next_anchors_new_sequence() -> None:
    trigger = _cron_monthly(berlin)
    for _ in range(2):
        trigger.next()

    boundary = trigger.next()
    preview = preview_timezone_migration(
        trigger, nuuk, range_end=datetime(2026, 6, 1, tzinfo=UTC)
    )

    # The first new fire is not earlier than the current next instant.
    assert preview.new_fire_times[0].utc_time >= boundary.astimezone(UTC)
    # The wall clock time at the boundary is the first intended slot.
    first_local = preview.new_fire_times[0].local_time
    assert (first_local.hour, first_local.minute) == (23, 30)


def test_natural_start_boundary() -> None:
    trigger = CronTrigger(
        day=15,
        hour=9,
        start_time=datetime(2025, 1, 15, 9, tzinfo=berlin),
        timezone=berlin,
    )
    # The trigger has advanced well past its original start time.
    for _ in range(20):
        trigger.next()

    preview = preview_timezone_migration(
        trigger,
        nuuk,
        start_at_current_next=False,
        range_end=datetime(2025, 6, 1, tzinfo=UTC),
    )
    first = preview.new_fire_times[0]
    assert first.local_time == datetime(2025, 1, 15, 9, tzinfo=nuuk)
    # The boundary still reports where the original trigger currently stands.
    assert preview.boundary > first.local_time


def test_calendar_natural_start_boundary() -> None:
    trigger = CalendarIntervalTrigger(
        months=1,
        hour=6,
        start_date=date(2025, 6, 30),
        timezone=berlin,
    )
    for _ in range(8):
        trigger.next()

    preview = preview_timezone_migration(
        trigger,
        nuuk,
        start_at_current_next=False,
        range_end=datetime(2025, 12, 1, tzinfo=UTC),
    )
    first = preview.new_fire_times[0]
    assert first.local_time == datetime(2025, 6, 30, 6, tzinfo=nuuk)
    assert preview.boundary > first.local_time


def test_string_timezone_accepted() -> None:
    trigger = _cron_monthly(berlin)
    preview = preview_timezone_migration(
        trigger, "America/Nuuk", range_end=datetime(2026, 3, 1, tzinfo=UTC)
    )
    assert preview.target_timezone == nuuk


def test_old_sequence_classifies_source_zone_ambiguity() -> None:
    # 02:30 is repeated in Berlin on 2026-10-25.
    trigger = CronTrigger(
        day=25,
        hour=2,
        minute=30,
        start_time=datetime(2026, 9, 25, 2, 30, tzinfo=berlin),
        timezone=berlin,
    )
    preview = preview_timezone_migration(
        trigger,
        berlin,
        range_start=datetime(2026, 10, 1, tzinfo=UTC),
        range_end=datetime(2026, 11, 1, tzinfo=UTC),
    )
    october = preview.old_fire_times[0]
    assert october.fold == 0
    assert october.resolution is LocalTimeResolution.earlier_occurrence


def test_fire_time_requires_aware_datetimes() -> None:
    with pytest.raises(ValueError, match="local_time must be timezone aware"):
        FireTime(
            local_time=datetime(2026, 1, 1),
            utc_time=datetime(2026, 1, 1, tzinfo=UTC),
            fold=0,
            resolution=LocalTimeResolution.normal,
        )


def test_calendar_internal_time_uses_target_zone() -> None:
    trigger = _calendar_monthly(berlin, hour=8, minute=15)
    preview = preview_timezone_migration(
        trigger, nuuk, range_end=datetime(2026, 3, 1, tzinfo=UTC)
    )
    for fire in preview.new_fire_times:
        assert (fire.local_time.hour, fire.local_time.minute) == (8, 15)
        assert fire.local_time.tzinfo == nuuk


def test_minute_level_gap_with_range_start() -> None:
    # The boundary (February) precedes the range start, so the shadow stream
    # must keep advancing across range-filtered actuals to reveal the March
    # forward-shift gap and its four missing quarter-hour slots.
    trigger = CronTrigger(
        hour=2,
        minute="*/15",
        start_time=datetime(2026, 2, 1, tzinfo=eastern),
        timezone=eastern,
    )
    preview = preview_timezone_migration(
        trigger,
        eastern,
        range_start=datetime(2026, 3, 8, 5, tzinfo=UTC),
        range_end=datetime(2026, 3, 9, 10, tzinfo=UTC),
    )

    gap_walls = [
        skipped.local_time.replace(tzinfo=None)
        for skipped in preview.skipped_local_times
    ]
    assert gap_walls == [
        datetime(2026, 3, 8, 2, 0),
        datetime(2026, 3, 8, 2, 15),
        datetime(2026, 3, 8, 2, 30),
        datetime(2026, 3, 8, 2, 45),
    ]
    assert all(
        skipped.resolution is LocalTimeResolution.skipped_to_next_match
        for skipped in preview.skipped_local_times
    )
    for skipped in preview.skipped_local_times:
        assert skipped.gap_start_utc < skipped.gap_end_utc

    # The gap day itself yields no fire; the schedule resumes the next day.
    assert [fire.local_time.day for fire in preview.new_fire_times] == [9, 9, 9, 9]

    direct = attrs.evolve(trigger)
    direct._last_fire_time = None
    direct_expected = []
    while (fire := direct.next()) <= datetime(2026, 3, 9, 10, tzinfo=UTC):
        if fire >= datetime(2026, 3, 8, 5, tzinfo=UTC):
            direct_expected.append(fire)

    assert [fire.local_time for fire in preview.new_fire_times] == direct_expected


def test_iteration_guard(monkeypatch: pytest.MonkeyPatch) -> None:
    from apscheduler.triggers import migration

    trigger = CronTrigger(minute="*", timezone=UTC)
    monkeypatch.setattr(migration, "_MAX_ITERATIONS", 2)
    with pytest.raises(MaxIterationsReached):
        preview_timezone_migration(
            trigger, berlin, range_end=datetime(2030, 1, 1, tzinfo=UTC)
        )
