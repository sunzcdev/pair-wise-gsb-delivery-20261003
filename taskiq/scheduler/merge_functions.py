import copy
from logging import getLogger
from typing import TYPE_CHECKING, Any, Sequence

from pydantic import ValidationError

from taskiq.scheduler.scheduled_task import ScheduledTask

if TYPE_CHECKING:  # pragma: no cover
    from taskiq.abc.schedule_source import ScheduleSource

logger = getLogger(__name__)

#: Fields a higher priority definition may inherit from a lower priority one.
#: `schedule_id` is the identity itself, while `cancelled` and `partial`
#: describe the layer, not the schedule, so they are never inherited.
INHERITABLE_FIELDS = (
    "task_name",
    "labels",
    "args",
    "kwargs",
    "task_id",
)

#: Fields that describe when a definition runs, grouped by trigger kind.
TRIGGER_GROUPS = {
    "cron": ("cron", "cron_offset"),
    "time": ("time",),
    "interval": ("interval",),
}


def source_priority(source: "ScheduleSource") -> int:
    """
    Get the declared priority of a schedule source.

    Higher values win. Sources without an explicit priority
    are treated as the lowest priority layer.

    :param source: source to inspect.
    :return: declared priority.
    """
    for name in ("priority", "source_priority", "schedule_priority"):
        value = getattr(source, name, None)
        if value is None:
            continue
        if callable(value):
            try:
                value = value()
            except Exception:  # noqa: BLE001 - priority is optional metadata.
                continue
        try:
            return int(value or 0)
        except (TypeError, ValueError):
            continue
    return 0


def _ordered_source_indexes(sources: Sequence["ScheduleSource"]) -> list[int]:
    """
    Order sources from the highest priority to the lowest one.

    Sources with equal priority keep their declaration order,
    so an earlier source wins over a later one.

    :param sources: declared sources.
    :return: indexes in resolution order.
    """
    return sorted(
        range(len(sources)),
        key=lambda index: (-source_priority(sources[index]), index),
    )


def _resolve_layers(layers: Sequence["ScheduledTask"]) -> "ScheduledTask | None":
    """
    Build a single definition out of priority ordered layers.

    Missing fields are inherited from the next layer, while an
    explicit cancellation masks everything below it. The first
    layer decides whether the schedule exists at all.

    `labels` are the only merged field: each key of a lower priority
    layer is kept unless a higher priority layer declares the same key,
    which makes supplementary labels possible without losing the base
    ones. The trigger, on the other hand, is taken as a whole from the
    highest priority layer that declares one. Its fields are not mixed
    across layers, otherwise a definition could run on a trigger it
    never declared.

    :param layers: definitions of one schedule id, highest priority first.
    :return: resolved definition or None if it was cancelled.
    """
    if not layers or layers[0].cancelled:
        return None

    # The trigger kind comes from the highest priority definition that
    # declares one. Triggers of other kinds are dropped, otherwise a
    # definition could run on a trigger it never declared.
    trigger_group: str | None = None
    for layer in layers:
        if layer.cancelled:
            break
        for name, names in TRIGGER_GROUPS.items():
            if any(getattr(layer, field, None) is not None for field in names):
                trigger_group = name
                break
        if trigger_group is not None:
            break

    values: dict[str, Any] = {}
    explicit: set[str] = set()
    labels: dict[str, Any] = {}

    for layer in layers:
        if layer.cancelled:
            # A cancellation of a lower priority source removes
            # that source and every source below it.
            break
        fields = layer.model_fields_set
        if "labels" in fields:
            # The first declared labels are the most specific ones.
            labels = {**layer.labels, **labels} if labels else dict(layer.labels)

        for name in INHERITABLE_FIELDS:
            if name == "labels" or name in explicit or name not in fields:
                continue
            value = getattr(layer, name)
            if layer.partial and not value:
                # A partial layer describes only the fields the caller
                # cares about. An empty value means "not mentioned",
                # which is a missing item, not an explicit override.
                # This also keeps partial definitions that went through
                # a serializer working, because serialization materializes
                # every field with its default.
                continue
            values[name] = value
            explicit.add(name)

        if trigger_group is None:
            continue
        for name in TRIGGER_GROUPS[trigger_group]:
            if name in explicit or name not in fields:
                continue
            value = getattr(layer, name)
            if layer.partial and not value:
                # Same rule as for the other fields: a materialized
                # default of a partial layer is not a declaration.
                continue
            values[name] = value
            explicit.add(name)

    values["schedule_id"] = layers[0].schedule_id
    values["labels"] = labels
    values["cancelled"] = False
    values["partial"] = False
    try:
        merged = ScheduledTask.model_validate(values)
        if not merged.task_name or (
            merged.cron is None
            and merged.time is None
            and merged.interval is None
        ):
            raise ValueError("Merged schedule is missing its task or trigger.")
        return merged
    except (ValidationError, ValueError):
        logger.warning(
            "Cannot resolve schedule %s, the merged definition is incomplete.",
            layers[0].schedule_id,
        )
        return None


def resolve_schedules(
    sources: Sequence["ScheduleSource"],
    schedules: Sequence[Sequence["ScheduledTask"]],
) -> list[tuple["ScheduleSource", list["ScheduledTask"]]]:
    """
    Resolve definitions from all sources into one per schedule id.

    Every `schedule_id` is resolved once, using the priority declared
    by sources. Fields that are missing in the winning definition are
    inherited from lower priority sources of the same id, but an
    explicit cancellation removes the id completely.

    The returned list has one entry per declared source, in declaration
    order, and every definition is reported under the source that owns
    it, so per-source hooks such as `pre_send` and `post_send` run for
    the source whose definition is effective.

    :param sources: declared schedule sources.
    :param schedules: schedules returned by every source, in the same order.
    :return: (source, effective schedules) pairs.
    """
    ordered = _ordered_source_indexes(sources)
    layers_by_id: dict[str, list[tuple[int, "ScheduledTask"]]] = {}
    position_by_id: dict[str, dict[int, int]] = {}

    for source_index in ordered:
        positions: dict[str, int] = position_by_id.setdefault(source_index, {})
        for task in schedules[source_index]:
            layers = layers_by_id.setdefault(task.schedule_id, [])
            known = positions.get(task.schedule_id)
            if known is not None:
                # The same source declared the id twice. The last
                # declaration wins, but keeps the original position.
                layers[known] = (source_index, task)
                continue
            positions[task.schedule_id] = len(layers)
            layers.append((source_index, task))

    resolved: dict[str, tuple[int, "ScheduledTask"]] = {}
    for schedule_id, layers in layers_by_id.items():
        merged = _resolve_layers([task for _, task in layers])
        if merged is not None:
            resolved[schedule_id] = (layers[0][0], merged)

    grouped: list[list["ScheduledTask"]] = [[] for _ in sources]
    emitted: set[str] = set()
    for source_index, source_schedules in enumerate(schedules):
        for task in source_schedules:
            winner = resolved.get(task.schedule_id)
            if winner is None or winner[0] != source_index:
                continue
            if task.schedule_id in emitted:
                continue
            grouped[source_index].append(winner[1])
            emitted.add(task.schedule_id)

    return [
        (source, grouped[source_index])
        for source_index, source in enumerate(sources)
    ]


def preserve_all(
    old_tasks: list["ScheduledTask"],
    new_tasks: list["ScheduledTask"],
) -> list["ScheduledTask"]:
    """
    This function simply merges two lists.

    It adds new tasks to others.

    :param old_tasks: previously discovered tasks.
    :param new_tasks: newly discovered tasks.
    :return: merged list.
    """
    return old_tasks + new_tasks


def _identity(task: Any) -> Any:
    """
    Get the identity used to compare two schedules.

    Two definitions of the same `schedule_id` are the same schedule,
    even if their fields differ, so the id is the identity whenever
    it's available.

    :param task: task or any other merged value.
    :return: value used for comparison.
    """
    schedule_id = getattr(task, "schedule_id", None)
    return schedule_id if schedule_id is not None else task


def only_unique(
    old_tasks: list["ScheduledTask"],
    new_tasks: list["ScheduledTask"],
) -> list["ScheduledTask"]:
    """
    This function preserves only unique schedules.

    It checks every task and if the schedule is already
    in list, it won't be added. Schedules are compared by
    `schedule_id`, so two definitions of the same schedule
    coming from different sources are considered the same one
    and the first (highest priority) definition is kept.

    :param old_tasks: previously discovered tasks.
    :param new_tasks: newly discovered tasks.
    :return: list of unique schedules.
    """
    result = copy.copy(old_tasks)
    known = {_identity(task) for task in result}
    for task in new_tasks:
        identity = _identity(task)
        if identity in known:
            continue
        known.add(identity)
        result.append(task)
    return result


def only_new(
    _old_tasks: list["ScheduledTask"],
    new_tasks: list["ScheduledTask"],
) -> list["ScheduledTask"]:
    """
    This function preserves only new schedules.

    :param _old_tasks: previously discovered tasks.
    :param new_tasks: newly discovered schedules.
    :return: list of new schedules.
    """
    return new_tasks
