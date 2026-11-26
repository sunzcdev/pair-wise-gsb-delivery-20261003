import pytest

from taskiq.abc.schedule_source import ScheduleSource
from taskiq.scheduler.merge_functions import (
    only_unique,
    preserve_all,
    resolve_schedules,
)
from taskiq.scheduler.scheduled_task import ScheduledTask, ScheduledTaskCancellation


def test_preserve_all() -> None:
    first = [1, 2, 3]
    second = [3, 4, 5]
    assert preserve_all(first, second) == [1, 2, 3, 3, 4, 5]  # type: ignore


def test_only_unique() -> None:
    first = [1, 2, 3]
    second = [3, 4, 5]
    assert only_unique(first, second) == [1, 2, 3, 4, 5]  # type: ignore


def test_only_unique_compares_schedules_by_id() -> None:
    base = schedule(args=["base"])
    overlay = schedule(args=["overlay"])

    assert only_unique([base], [overlay]) == [base]


class PrioritySource(ScheduleSource):
    """Source declaring a fixed priority."""

    def __init__(self, priority: int, schedules: list[ScheduledTask]) -> None:
        self.priority = priority
        self.schedules = schedules

    async def get_schedules(self) -> list[ScheduledTask]:
        return self.schedules


def schedule(**overrides: object) -> ScheduledTask:
    values: dict[str, object] = {
        "task_name": "demo.task",
        "labels": {},
        "args": [],
        "kwargs": {},
        "schedule_id": "shared",
        "cron": "* * * * *",
    }
    values.update(overrides)
    return ScheduledTask(**values)


def resolved(
    sources: list[PrioritySource],
) -> dict[str, ScheduledTask]:
    pairs = resolve_schedules(sources, [source.schedules for source in sources])
    return {task.schedule_id: task for _, tasks in pairs for task in tasks}


def test_higher_priority_source_wins_for_same_id() -> None:
    base = PrioritySource(0, [schedule(args=["base"])])
    overlay = PrioritySource(10, [schedule(args=["overlay"])])

    merged = resolved([base, overlay])

    assert merged["shared"].args == ["overlay"]
    assert merged["shared"].cron == "* * * * *"


def test_declaration_order_breaks_priority_ties() -> None:
    first = PrioritySource(0, [schedule(args=["first"])])
    second = PrioritySource(0, [schedule(args=["second"])])

    assert resolved([first, second])["shared"].args == ["first"]
    assert resolved([second, first])["shared"].args == ["second"]


def test_missing_trigger_is_inherited_from_lower_source() -> None:
    base = PrioritySource(0, [schedule(args=["base"])])
    overlay = PrioritySource(5, [ScheduledTask(partial=True, schedule_id="shared")])

    merged = resolved([base, overlay])

    assert merged["shared"].cron == "* * * * *"
    assert merged["shared"].args == ["base"]
    assert merged["shared"].task_name == "demo.task"


def test_sparse_override_without_partial_marker_inherits_missing_fields() -> None:
    base = PrioritySource(0, [schedule(args=["base"])])
    overlay = PrioritySource(
        5,
        [ScheduledTask(schedule_id="shared", args=["overlay"])],
    )

    merged = resolved([base, overlay])["shared"]

    assert merged.args == ["overlay"]
    assert merged.kwargs == {}
    assert merged.cron == "* * * * *"
    assert merged.task_name == "demo.task"


def test_partial_layer_only_overrides_declared_fields() -> None:
    base = PrioritySource(
        0,
        [schedule(args=["base"], kwargs={"keep": True}, labels={"tier": "base"})],
    )
    overlay = PrioritySource(
        5,
        [
            ScheduledTask(
                partial=True,
                schedule_id="shared",
                args=["overlay"],
                labels={"extra": "overlay"},
            ),
        ],
    )

    merged = resolved([base, overlay])["shared"]

    assert merged.args == ["overlay"]
    assert merged.kwargs == {"keep": True}
    assert merged.labels == {"extra": "overlay", "tier": "base"}


def test_labels_of_higher_priority_win_per_key() -> None:
    base = PrioritySource(0, [schedule(labels={"a": "base", "b": "base"})])
    overlay = PrioritySource(5, [schedule(labels={"b": "overlay"})])

    assert resolved([base, overlay])["shared"].labels == {"a": "base", "b": "overlay"}


def test_explicit_cancellation_removes_the_schedule() -> None:
    base = PrioritySource(0, [schedule()])
    overlay = PrioritySource(5, [ScheduledTaskCancellation(schedule_id="shared")])

    assert resolved([base, overlay]) == {}


def test_cancellation_from_lower_priority_source_is_ignored() -> None:
    base = PrioritySource(0, [ScheduledTaskCancellation(schedule_id="shared")])
    overlay = PrioritySource(5, [schedule(args=["overlay"])])

    assert resolved([base, overlay])["shared"].args == ["overlay"]


def test_cancelled_schedule_is_not_part_of_any_source() -> None:
    base = PrioritySource(0, [schedule()])
    overlay = PrioritySource(5, [ScheduledTaskCancellation(schedule_id="shared")])
    pairs = resolve_schedules([base, overlay], [base.schedules, overlay.schedules])

    assert [(source, tasks) for source, tasks in pairs] == [
        (base, []),
        (overlay, []),
    ]


def test_effective_schedule_is_reported_under_winning_source() -> None:
    base = PrioritySource(0, [schedule(args=["base"])])
    overlay = PrioritySource(5, [schedule(args=["overlay"])])
    pairs = resolve_schedules([base, overlay], [base.schedules, overlay.schedules])

    assert [(source, [task.args for task in tasks]) for source, tasks in pairs] == [
        (base, []),
        (overlay, [["overlay"]]),
    ]


def test_schedules_with_distinct_ids_are_kept() -> None:
    base = PrioritySource(0, [schedule(schedule_id="base")])
    overlay = PrioritySource(5, [schedule(schedule_id="overlay")])

    assert set(resolved([base, overlay])) == {"base", "overlay"}


def test_partial_definition_without_any_base_is_dropped(
    caplog: pytest.LogCaptureFixture,
) -> None:
    overlay = PrioritySource(5, [ScheduledTask(partial=True, schedule_id="shared")])

    assert resolved([overlay]) == {}
    assert "merged definition is incomplete" in caplog.text


def test_duplicate_id_within_one_source_keeps_last_declaration() -> None:
    source = PrioritySource(
        0,
        [schedule(args=["first"]), schedule(args=["second"])],
    )

    assert resolved([source])["shared"].args == ["second"]
