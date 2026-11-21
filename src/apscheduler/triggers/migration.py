"""
Read-only previews for moving triggers between time zones.

This module never mutates the trigger passed in.  Every calculation is done on
deep copies of the trigger, so calling :meth:`~apscheduler.abc.Trigger.next` on
the original trigger after a preview still returns the same fire time that it
would have returned before the preview.
"""

from __future__ import annotations

import copy
import enum
from collections import deque
from datetime import UTC, datetime, tzinfo

import attrs
from attr.validators import instance_of

from .._converters import as_timezone
from .._exceptions import MaxIterationsReached
from .calendarinterval import CalendarIntervalTrigger
from .cron import CronTrigger

#: Maximum number of candidate fire times examined within a single preview.
_MAX_ITERATIONS = 100_000


class LocalTimeRule(enum.Enum):
    """
    Rules for translating a trigger's fire times into another time zone.

    .. attribute:: preserve_wall_time

        Keep the local (wall clock) time of day and calendar fields.  A monthly
        task that fires at 06:00 in the original time zone will fire at 06:00 in
        the target time zone, at a (potentially) different UTC instant.
    """

    preserve_wall_time = enum.auto()


class LocalTimeResolution(enum.Enum):
    """
    How a trigger resolves a local wall clock time that is ambiguous or does
    not exist.

    .. attribute:: normal

        the local time exists exactly once and is used as is

    .. attribute:: earlier_occurrence

        the local time is repeated due to a backward DST shift; the trigger
        uses the earlier of the two instances (``fold=0``)

    .. attribute:: skipped_to_next_match

        the local time does not exist due to a forward DST shift; a
        :class:`~apscheduler.triggers.cron.CronTrigger` skips it and fires at
        the next matching local time

    .. attribute:: date_discarded

        the local time does not exist due to a forward DST shift; a
        :class:`~apscheduler.triggers.calendarinterval.CalendarIntervalTrigger`
        discards the whole calendar date
    """

    normal = enum.auto()
    earlier_occurrence = enum.auto()
    skipped_to_next_match = enum.auto()
    date_discarded = enum.auto()


@attrs.define(frozen=True)
class FireTime:
    """
    A single fire time of either the original or the migrated sequence.

    :ivar local_time: fire time expressed in the sequence's time zone
    :ivar utc_time: the same fire time expressed in UTC
    :ivar fold: the ``fold`` value the trigger assigned to the local time
        (``1`` marks the later instance of a repeated local time, though the
        built-in triggers always choose the earlier instance)
    :ivar resolution: how the local time was resolved in its time zone
    """

    local_time: datetime = attrs.field(validator=instance_of(datetime))
    utc_time: datetime = attrs.field(validator=instance_of(datetime))
    fold: int
    resolution: LocalTimeResolution

    def __attrs_post_init__(self) -> None:
        if self.local_time.tzinfo is None:
            raise ValueError("local_time must be timezone aware")
        if self.utc_time.tzinfo is None:
            raise ValueError("utc_time must be timezone aware")


@attrs.define(frozen=True)
class SkippedLocalTime:
    """
    A local wall clock time that the migrated trigger wanted to fire at but
    which does not exist in the target time zone because of a forward DST
    shift.

    :ivar local_time: the intended local time (attached to the target time
        zone, but not a point on the UTC timeline)
    :ivar gap_start_utc: UTC instant at which the skipped local time would
        have started according to the pre-transition offset
    :ivar gap_end_utc: UTC instant at which the skipped local time would have
        ended according to the post-transition offset
    :ivar resolution: how the trigger dealt with the missing local time
    """

    local_time: datetime = attrs.field(validator=instance_of(datetime))
    gap_start_utc: datetime = attrs.field(validator=instance_of(datetime))
    gap_end_utc: datetime = attrs.field(validator=instance_of(datetime))
    resolution: LocalTimeResolution


@attrs.define(frozen=True)
class TimezoneMigrationPreview:
    """
    The result of a read-only time zone migration preview.

    :ivar source_timezone: time zone of the original trigger
    :ivar target_timezone: time zone the trigger would be migrated to
    :ivar rule: local time translation rule that was applied
    :ivar boundary: the original trigger's current next fire instant at the
        time the preview was made (before any preview iteration); when
        ``start_at_current_next`` is enabled this is also the first instant at
        or after which the migrated sequence is reported
    :ivar old_fire_times: fire times the original trigger produces within the
        requested range, in chronological order
    :ivar new_fire_times: fire times the migrated trigger produces within the
        requested range, in chronological order
    :ivar skipped_local_times: intended local fire times that do not exist in
        the target time zone and were therefore dropped by the migrated
        trigger, in chronological order
    """

    source_timezone: tzinfo
    target_timezone: tzinfo
    rule: LocalTimeRule
    boundary: datetime
    old_fire_times: list[FireTime] = attrs.field(factory=list)
    new_fire_times: list[FireTime] = attrs.field(factory=list)
    skipped_local_times: list[SkippedLocalTime] = attrs.field(factory=list)


def _classify_local_time(
    wall_time: datetime, timezone: tzinfo
) -> tuple[LocalTimeResolution, datetime, datetime]:
    """
    Classify a naive wall clock time in the given time zone.

    :return: a tuple of ``(resolution, earlier_dt, later_dt)`` where the two
        datetimes are always ordered by UTC instant (``earlier_dt`` does not
        resolve to a later instant than ``later_dt``):

        * for a normal time both datetimes are identical
        * for a forward-shift gap they bracket the skipped interval
          (``earlier_dt`` carries ``fold=1``, ``later_dt`` ``fold=0``)
        * for a backward-shift ambiguity they are the earlier (``fold=0``)
          and later (``fold=1``) instances
    """
    fold0 = wall_time.replace(tzinfo=timezone, fold=0)
    fold1 = wall_time.replace(tzinfo=timezone, fold=1)
    if fold0.timestamp() == fold1.timestamp():
        return LocalTimeResolution.normal, fold0, fold1
    elif fold0.timestamp() > fold1.timestamp():
        # A forward shift: the wall clock time is skipped entirely.
        return LocalTimeResolution.skipped_to_next_match, fold1, fold0

    # A backward shift: the wall clock time is repeated.
    return LocalTimeResolution.earlier_occurrence, fold0, fold1


def _resolution_for_fire(
    fire_time: datetime, slot_resolution: LocalTimeResolution
) -> LocalTimeResolution:
    """Resolve the classification of an actual fire time against its slot."""
    if slot_resolution is LocalTimeResolution.earlier_occurrence and fire_time.fold:
        # The later instance of a repeated time is a perfectly normal instant.
        return LocalTimeResolution.normal

    return slot_resolution


def _classify_fire_time(fire_time: datetime) -> LocalTimeResolution:
    """Classify an actual, normalized fire time in its own time zone."""
    wall_time = fire_time.replace(tzinfo=None)
    resolution, _, _ = _classify_local_time(wall_time, fire_time.tzinfo)
    if resolution is LocalTimeResolution.earlier_occurrence and fire_time.fold:
        return LocalTimeResolution.normal

    return resolution


def _to_fire_time(fire_time: datetime, resolution: LocalTimeResolution) -> FireTime:
    return FireTime(
        local_time=fire_time,
        utc_time=fire_time.astimezone(UTC),
        fold=fire_time.fold,
        resolution=resolution,
    )


def _check_range_bounds(
    range_start: datetime | None, range_end: datetime | None
) -> None:
    for name, value in (("range_start", range_start), ("range_end", range_end)):
        if value is not None and value.tzinfo is None:
            raise ValueError(f"{name} must be timezone aware")

    if (
        range_start is not None
        and range_end is not None
        and range_end < range_start
    ):
        raise ValueError("range_end cannot be earlier than range_start")


def _build_migrated_cron(
    trigger: CronTrigger,
    target_timezone: tzinfo,
    wall_start: datetime,
) -> tuple[CronTrigger, CronTrigger]:
    # The migrated trigger keeps every cron expression; only its time zone and
    # the earliest allowed fire time change.  The last-fire bookkeeping is
    # reset so that the new sequence really starts at the requested wall clock
    # boundary instead of at the original trigger's current position.
    migrated = attrs.evolve(
        trigger,
        timezone=target_timezone,
        start_time=wall_start.replace(tzinfo=target_timezone),
    )
    if migrated.end_time is not None:
        migrated.end_time = trigger.end_time.astimezone(target_timezone)

    migrated._last_fire_time = None

    # A UTC "shadow" trigger produces the intended wall clock slots without any
    # DST gaps or repeated times getting in the way.
    shadow = attrs.evolve(
        trigger,
        timezone=UTC,
        start_time=wall_start.replace(tzinfo=UTC),
    )
    shadow.end_time = None
    shadow._last_fire_time = None
    return migrated, shadow


def _build_migrated_calendar(
    trigger: CalendarIntervalTrigger, target_timezone: tzinfo
) -> tuple[CalendarIntervalTrigger, CalendarIntervalTrigger]:
    # Calendar fields and the designated time of day are preserved; only the
    # time zone used to resolve the wall clock time changes.
    migrated = attrs.evolve(trigger, timezone=target_timezone)
    shadow = attrs.evolve(trigger, timezone=UTC)
    return migrated, shadow


def preview_timezone_migration(
    trigger: CronTrigger | CalendarIntervalTrigger,
    target_timezone: tzinfo | str,
    range_start: datetime | None = None,
    range_end: datetime | None = None,
    *,
    rule: LocalTimeRule = LocalTimeRule.preserve_wall_time,
    start_at_current_next: bool = True,
) -> TimezoneMigrationPreview:
    """
    Preview what would happen if the given trigger were moved to another time
    zone, without modifying the trigger.

    The preview keeps the local wall clock time of the original schedule (see
    :class:`LocalTimeRule`) and reports both the old and the new fire time
    sequences with their local and UTC representations.  Local times that do
    not exist or are repeated in the target time zone are reported together
    with the resolution the existing trigger logic applies (skips for
    :class:`~apscheduler.triggers.cron.CronTrigger`, discarded dates for
    :class:`~apscheduler.triggers.calendarinterval.CalendarIntervalTrigger`,
    and the earlier instance for repeated times).

    :param trigger: the trigger to preview the migration for
    :param target_timezone: the time zone the trigger would be migrated to
    :param range_start: if given, only fire times at or after this instant are
        included in the preview
    :param range_end: if given, only fire times at or before this instant are
        included in the preview
    :param rule: the local time preservation rule to apply
    :param start_at_current_next: if ``True`` (the default), the migrated
        sequence starts at the original trigger's current next fire instant;
        otherwise it starts at the migrated trigger's own natural start
        boundary (``start_time`` / ``start_date``)
    :return: a :class:`TimezoneMigrationPreview` describing both sequences
    :raises TypeError: if the trigger or rule is of an unsupported type
    :raises ValueError: if the range bounds are invalid or naive, or if the
        trigger has no next fire time to anchor the preview
    """
    if not isinstance(trigger, CronTrigger | CalendarIntervalTrigger):
        raise TypeError(
            "time zone migration preview is only supported for CronTrigger "
            f"and CalendarIntervalTrigger, got {type(trigger).__name__}"
        )

    if not isinstance(rule, LocalTimeRule):
        raise TypeError(f"invalid local time rule: {rule!r}")

    target_tz = as_timezone(target_timezone)
    _check_range_bounds(range_start, range_end)

    # Everything from here on operates on copies; the caller's trigger state
    # (including its last fire time bookkeeping) is left untouched.  The first
    # copy establishes where the original trigger currently stands, which is
    # both the anchor of the preview and the reference used later to prove the
    # original trigger still resumes from the same position.
    source_tz = trigger.timezone
    anchor = copy.deepcopy(trigger)
    boundary = anchor.next()
    if boundary is None:
        raise ValueError(
            "the trigger has no next fire time to anchor the migration preview"
        )

    lower_bound = boundary
    if range_start is not None and range_start > lower_bound:
        lower_bound = range_start

    # Build the old sequence from another independent copy.
    old_trigger = copy.deepcopy(trigger)
    old_fire_times: list[FireTime] = []
    for _ in range(_MAX_ITERATIONS):
        fire_time = old_trigger.next()
        if fire_time is None:
            break

        if fire_time < lower_bound:
            continue
        if range_end is not None and fire_time > range_end:
            break

        resolution, _, _ = _classify_local_time(
            fire_time.replace(tzinfo=None), source_tz
        )
        old_fire_times.append(_to_fire_time(fire_time, resolution))
    else:
        raise MaxIterationsReached(
            f"exceeded {_MAX_ITERATIONS} iterations while calculating the old "
            "fire time sequence"
        )

    if start_at_current_next:
        wall_start = boundary.astimezone(target_tz).replace(tzinfo=None)
    elif isinstance(trigger, CronTrigger):
        wall_start = trigger.start_time.astimezone(target_tz).replace(tzinfo=None)
    else:
        wall_start = datetime.combine(
            trigger.start_date, trigger._time
        ).replace(tzinfo=None)

    if isinstance(trigger, CronTrigger):
        migrated, shadow = _build_migrated_cron(trigger, target_tz, wall_start)
        gap_resolution = LocalTimeResolution.skipped_to_next_match
    else:
        migrated, shadow = _build_migrated_calendar(trigger, target_tz)
        gap_resolution = LocalTimeResolution.date_discarded

    # The new sequence is anchored at the current next fire instant by
    # default, but may start at the migrated trigger's own natural boundary.
    if start_at_current_next:
        new_lower_bound = lower_bound
    else:
        new_lower_bound = wall_start.replace(tzinfo=target_tz)

    if range_start is not None and range_start > new_lower_bound:
        new_lower_bound = range_start

    new_fire_times: list[FireTime] = []
    skipped_local_times: list[SkippedLocalTime] = []

    # Intended wall clock slots generated by the DST-free shadow trigger.
    # The migrated trigger is driven instead, so that its own gap/ambiguity
    # handling (skips and discarded dates) is faithfully reproduced; the shadow
    # stream only explains which intended slot each actual fire time belongs
    # to and reveals the slots lost to forward-shift gaps.
    shadow_slots: deque[tuple[float, datetime, LocalTimeResolution]] = deque()

    def next_shadow_slot() -> tuple[float, datetime, LocalTimeResolution] | None:
        while True:
            intended = shadow.next()
            if intended is None:
                return None

            wall_time = intended.replace(tzinfo=None)
            resolution, earlier_dt, later_dt = _classify_local_time(
                wall_time, target_tz
            )
            if resolution is LocalTimeResolution.skipped_to_next_match:
                if earlier_dt.timestamp() >= new_lower_bound.timestamp() and (
                    range_end is None or earlier_dt <= range_end
                ):
                    skipped_local_times.append(
                        SkippedLocalTime(
                            local_time=wall_time.replace(tzinfo=target_tz),
                            gap_start_utc=earlier_dt.astimezone(UTC),
                            gap_end_utc=later_dt.astimezone(UTC),
                            resolution=gap_resolution,
                        )
                    )

                # The migrated trigger never emits this slot; keep scanning.
                continue

            return earlier_dt.timestamp(), earlier_dt, resolution

    for _ in range(_MAX_ITERATIONS):
        actual = migrated.next()
        if actual is None:
            break

        actual_ts = actual.timestamp()

        # Keep the shadow stream aligned with the actual fire time *before*
        # applying the range filter.  Otherwise the shadow would never advance
        # past actuals outside the range and any gap slots preceding a range
        # boundary would be missed.  Shadow slots the migrated trigger passed
        # without emitting (e.g. a discarded calendar date) are dropped here.
        while True:
            while shadow_slots and shadow_slots[0][0] < actual_ts:
                shadow_slots.popleft()

            if shadow_slots and shadow_slots[0][0] >= actual_ts:
                break

            slot = next_shadow_slot()
            if slot is None:
                break

            shadow_slots.append(slot)

        if shadow_slots and shadow_slots[0][0] == actual_ts:
            _, _, slot_resolution = shadow_slots.popleft()
            resolution = _resolution_for_fire(actual, slot_resolution)
        else:
            # The shadow stream ended without recognizing this instant; fall
            # back to classifying the actual local time directly.
            resolution = _classify_fire_time(actual)

        if actual < new_lower_bound:
            continue
        if range_end is not None and actual > range_end:
            break

        new_fire_times.append(_to_fire_time(actual, resolution))
    else:
        raise MaxIterationsReached(
            f"exceeded {_MAX_ITERATIONS} iterations while calculating the new "
            "fire time sequence"
        )

    return TimezoneMigrationPreview(
        source_timezone=source_tz,
        target_timezone=target_tz,
        rule=rule,
        boundary=boundary,
        old_fire_times=old_fire_times,
        new_fire_times=new_fire_times,
        skipped_local_times=skipped_local_times,
    )