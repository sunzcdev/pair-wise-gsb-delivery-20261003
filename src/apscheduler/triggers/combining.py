from __future__ import annotations

from abc import abstractmethod
from datetime import datetime, timedelta
from typing import Any

import attrs
from attr.validators import and_, deep_iterable, gt, instance_of

from .._converters import as_aware_datetime, as_timedelta, list_converter
from .._exceptions import MaxIterationsReached
from .._marshalling import marshal_object, unmarshal_object
from .._utils import require_state_version
from ..abc import Trigger


def as_exclusion_window(
    value: ExclusionWindow | tuple | list | dict,
) -> ExclusionWindow:
    if isinstance(value, ExclusionWindow):
        return value
    elif isinstance(value, dict):
        return ExclusionWindow(**value)
    elif isinstance(value, (tuple, list)):
        return ExclusionWindow(*value)

    raise TypeError(
        f"Expected an ExclusionWindow, a (start, end) pair or a dict, got "
        f"{value!r}"
    )


@attrs.define(frozen=True)
class ExclusionWindow:
    """
    Represents a half-open (``[start, end)``) time interval during which the
    enclosing trigger must not produce any fire times.

    A candidate fire time is skipped when it falls on or after ``start`` and
    before ``end``. Both endpoints must be timezone aware. Endpoints expressed
    in different time zones are compared by the same absolute instant, so a
    candidate matching an endpoint in another time zone is handled correctly.
    Windows are allowed to be adjacent to or overlap with each other.

    :param start: first instant (inclusive) at which fire times are suppressed
    :param end: first instant (exclusive) at which fire times are allowed again
    """

    start: datetime = attrs.field(
        converter=as_aware_datetime, validator=instance_of(datetime)
    )
    end: datetime = attrs.field(
        converter=as_aware_datetime, validator=instance_of(datetime)
    )

    def __attrs_post_init__(self) -> None:
        if self.end <= self.start:
            raise ValueError("exclusion window end must be later than its start")

    def __contains__(self, fire_time: datetime) -> bool:
        return self.start <= fire_time < self.end

    def __getstate__(self) -> dict[str, Any]:
        return {"version": 1, "start": self.start, "end": self.end}

    def __setstate__(self, state: dict[str, Any]) -> None:
        require_state_version(self, state, 1)
        object.__setattr__(self, "start", as_aware_datetime(state["start"]))
        object.__setattr__(self, "end", as_aware_datetime(state["end"]))

    def __repr__(self) -> str:
        return f"ExclusionWindow('{self.start.isoformat()}', '{self.end.isoformat()}')"


@attrs.define
class BaseCombiningTrigger(Trigger):
    triggers: list[Trigger]
    exclusion_windows: list[ExclusionWindow] = attrs.field(
        converter=list_converter(as_exclusion_window),
        validator=deep_iterable(instance_of(ExclusionWindow)),
        factory=list,
    )
    max_iterations: int = attrs.field(
        validator=and_(instance_of(int), gt(0)), default=10000
    )
    _next_fire_times: list[datetime | None] = attrs.field(
        init=False, eq=False, converter=list_converter(as_aware_datetime), factory=list
    )

    def _is_excluded(self, fire_time: datetime) -> bool:
        return any(fire_time in window for window in self.exclusion_windows)

    def __getstate__(self) -> dict[str, Any]:
        return {
            "version": 2,
            "triggers": [marshal_object(trigger) for trigger in self.triggers],
            "exclusion_windows": [
                marshal_object(window) for window in self.exclusion_windows
            ],
            "max_iterations": self.max_iterations,
            "next_fire_times": self._next_fire_times,
        }

    @abstractmethod
    def __setstate__(self, state: dict[str, Any]) -> None:
        self.triggers = [
            unmarshal_object(*trigger_state) for trigger_state in state["triggers"]
        ]
        self.exclusion_windows = [
            unmarshal_object(*window_state)
            for window_state in state.get("exclusion_windows", [])
        ]
        self.max_iterations = state.get("max_iterations", 10000)
        self._next_fire_times = state["next_fire_times"]


@attrs.define
class AndTrigger(BaseCombiningTrigger):
    """
    Fires on times produced by the enclosed triggers whenever the fire times are within
    the given threshold.

    If the produced fire times are not within the given threshold of each other, the
    trigger(s) that produced the earliest fire time will be asked for their next fire
    time and the iteration is restarted. If instead all the triggers agree on a fire
    time, all the triggers are asked for their next fire times and the earliest of the
    previously produced fire times will be returned.

    This trigger will be finished when any of the enclosed trigger has finished.

    :param triggers: triggers to combine
    :param threshold: maximum time difference between the next fire times of the
        triggers in order for the earliest of them to be returned from :meth:`next` (in
        seconds, or as timedelta)
    :param exclusion_windows: half-open (``[start, end)``) time intervals during
        which any candidate fire time is skipped; the intervals may be adjacent to
        or overlap with each other
    :param max_iterations: maximum number of iterations of fire time calculations before
        giving up
    """

    threshold: timedelta = attrs.field(converter=as_timedelta, default=1)

    def next(self) -> datetime | None:
        if not self._next_fire_times:
            # Fill out the fire times on the first run
            self._next_fire_times = [t.next() for t in self.triggers]

        for _ in range(self.max_iterations):
            # Find the earliest and latest fire times
            earliest_fire_time: datetime | None = None
            latest_fire_time: datetime | None = None
            for fire_time in self._next_fire_times:
                # If any of the fire times is None, this trigger is finished
                if fire_time is None:
                    return None

                if earliest_fire_time is None or earliest_fire_time > fire_time:
                    earliest_fire_time = fire_time

                if latest_fire_time is None or latest_fire_time < fire_time:
                    latest_fire_time = fire_time

            # Replace all the fire times that were within the threshold
            for i, _trigger in enumerate(self.triggers):
                if self._next_fire_times[i] - earliest_fire_time <= self.threshold:
                    self._next_fire_times[i] = self.triggers[i].next()

            # If all the fire times were within the threshold, return the earliest
            # one, unless it falls within one of the exclusion windows
            if latest_fire_time - earliest_fire_time <= self.threshold:
                if not self._is_excluded(earliest_fire_time):
                    return earliest_fire_time
        else:
            raise MaxIterationsReached

    def __getstate__(self) -> dict[str, Any]:
        state = super().__getstate__()
        state["threshold"] = self.threshold
        return state

    def __setstate__(self, state: dict[str, Any]) -> None:
        require_state_version(self, state, 2)
        super().__setstate__(state)
        self.threshold = state["threshold"]

    def __repr__(self) -> str:
        extra = ""
        if self.exclusion_windows:
            extra = f", exclusion_windows={self.exclusion_windows!r}"

        return (
            f"{self.__class__.__name__}({self.triggers}, "
            f"threshold={self.threshold.total_seconds()}, "
            f"max_iterations={self.max_iterations}{extra})"
        )


@attrs.define
class OrTrigger(BaseCombiningTrigger):
    """
    Fires on every fire time of every trigger in chronological order.
    If two or more triggers produce the same fire time, it will only be used once.

    Candidate fire times that fall within one of the ``exclusion_windows`` are
    skipped. The number of consecutive skips is bounded by ``max_iterations`` to
    guarantee that the calculation makes forward progress.

    This trigger will be finished when none of the enclosed triggers can produce any new
    fire times.

    :param triggers: triggers to combine
    :param exclusion_windows: half-open (``[start, end)``) time intervals during
        which any candidate fire time is skipped; the intervals may be adjacent to
        or overlap with each other
    :param max_iterations: maximum number of candidate fire times that may be
        skipped in a row before giving up
    """

    def next(self) -> datetime | None:
        # Fill out the fire times on the first run
        if not self._next_fire_times:
            self._next_fire_times = [t.next() for t in self.triggers]

        for _ in range(self.max_iterations):
            # Find out the earliest of the fire times
            earliest_time: datetime | None = min(
                (
                    fire_time
                    for fire_time in self._next_fire_times
                    if fire_time is not None
                ),
                default=None,
            )
            if earliest_time is None:
                return None

            # Generate new fire times for the trigger(s) that generated the earliest
            # fire time
            for i, fire_time in enumerate(self._next_fire_times):
                if fire_time == earliest_time:
                    self._next_fire_times[i] = self.triggers[i].next()

            # Skip the fire time if it falls within one of the exclusion windows
            if not self._is_excluded(earliest_time):
                return earliest_time
        else:
            raise MaxIterationsReached

    def __setstate__(self, state: dict[str, Any]) -> None:
        require_state_version(self, state, 2)
        super().__setstate__(state)

    def __repr__(self) -> str:
        if self.exclusion_windows:
            return (
                f"{self.__class__.__name__}({self.triggers}, "
                f"exclusion_windows={self.exclusion_windows!r})"
            )

        return f"{self.__class__.__name__}({self.triggers})"
