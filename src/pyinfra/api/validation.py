"""
Plan validation for operations that require approval before execution.

Operations called with ``_validate_plan=True`` have their generated commands
(and every fact read while generating them) recorded at plan time. When the
operation is later executed the recorded facts are re-read:

+ Facts unchanged -> the approved commands run exactly as recorded, the
  operation is *not* re-evaluated.
+ Facts changed -> only the affected operation is recomputed. If the recompute
  yields no commands the operation is skipped with an explanation. If the
  recompute would change the write targets or expand the change scope the
  deploy is stopped and the old/new difference is reported.

Operations that already completed (successfully or not) are never replayed.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any, Literal

from .command import (
    FileDownloadCommand,
    FileUploadCommand,
    PyinfraCommand,
    RsyncCommand,
    StringCommand,
)

if TYPE_CHECKING:
    from .facts import FactBase, ShortFactBase
    from .host import Host
    from .state import StateOperationHostData


def _src_key(src: Any) -> str:
    if isinstance(src, str):
        return src
    name = getattr(src, "name", None)
    if isinstance(name, str):
        return name
    return type(src).__name__


def _command_key(command: PyinfraCommand) -> tuple[Any, ...]:
    """
    Hashable identity for a command, used to compare planned commands against
    freshly recomputed ones.
    """

    if isinstance(command, StringCommand):
        return ("shell", command.get_raw_value())
    if isinstance(command, FileUploadCommand):
        return ("upload", _src_key(command.src), command.dest)
    if isinstance(command, FileDownloadCommand):
        return ("download", command.src, _src_key(command.dest))
    if isinstance(command, RsyncCommand):
        return ("rsync", command.src, command.dest, tuple(str(flag) for flag in command.flags))
    return (type(command).__name__, repr(command))


def _command_targets(command: PyinfraCommand) -> set[str]:
    """
    The objects a command will modify. File transfer commands have an explicit
    remote target; for shell commands the command itself is the best available
    identity, so any change to it (eg a new path) shows up as a target change.
    """

    if isinstance(command, FileUploadCommand):
        return {command.dest}
    if isinstance(command, FileDownloadCommand):
        return {command.src}
    if isinstance(command, RsyncCommand):
        return {command.dest}
    if isinstance(command, StringCommand):
        return {command.get_masked_value()}
    return {repr(command)}


def _display_command(command: PyinfraCommand) -> str:
    if isinstance(command, StringCommand):
        return command.get_masked_value()
    return repr(command)


def _targets_of(commands: list[PyinfraCommand]) -> set[str]:
    targets: set[str] = set()
    for command in commands:
        targets.update(_command_targets(command))
    return targets


def _is_submultiset(new_keys: list[tuple[Any, ...]], planned_keys: list[tuple[Any, ...]]) -> bool:
    remaining = list(planned_keys)
    for key in new_keys:
        if key not in remaining:
            return False
        remaining.remove(key)
    return True


@dataclass
class FactRead:
    """
    A single fact read recorded while generating an operation's approved plan.
    """

    fact_cls: type[FactBase] | type[ShortFactBase]
    args: tuple[Any, ...]
    kwargs: dict[str, Any]
    value: Any

    def label(self) -> str:
        name = getattr(self.fact_cls, "name", None) or str(self.fact_cls)
        bits = [repr(arg) for arg in self.args]
        bits.extend(f"{key}={value!r}" for key, value in self.kwargs.items())
        return f"{name}({', '.join(bits)})"


@dataclass
class ValidatedOpPlan:
    """
    The approved plan for one validated operation on one host: the commands the
    operation generated at plan time and every fact value those commands were
    based on.
    """

    commands: list[PyinfraCommand] = field(default_factory=list)
    fact_reads: list[FactRead] = field(default_factory=list)
    # Whether fact reads should still be recorded (plan phase only)
    recording: bool = True
    _fact_read_keys: set[tuple[str, str, str]] = field(default_factory=set)

    def record_fact(
        self,
        fact_cls: type[FactBase] | type[ShortFactBase],
        args: tuple[Any, ...],
        kwargs: dict[str, Any],
        value: Any,
    ) -> None:
        key = (
            getattr(fact_cls, "name", None) or str(fact_cls),
            repr(args),
            repr(kwargs),
        )
        if key in self._fact_read_keys:
            return
        self._fact_read_keys.add(key)
        self.fact_reads.append(FactRead(fact_cls, tuple(args), dict(kwargs), value))

    @property
    def targets(self) -> set[str]:
        return _targets_of(self.commands)


def record_fact_read(
    host: Host,
    fact_cls: type[FactBase] | type[ShortFactBase],
    args: tuple[Any, ...],
    kwargs: dict[str, Any],
    value: Any,
) -> None:
    """
    Record a fact read into the approved plan of the operation currently being
    generated, if that operation is validated and still in its plan phase.
    """

    op_hash = host.current_op_hash
    if op_hash is None:
        return
    plan = host.state.get_validated_op_plan(host, op_hash)
    if plan is None or not plan.recording:
        return
    plan.record_fact(fact_cls, args, kwargs, value)


class PlanResolution:
    """
    Outcome of validating an approved plan against the current facts.
    """

    kind: Literal["proceed", "skip", "conflict"]
    commands: list[PyinfraCommand]
    reason: str | None
    report: str | None

    def __init__(
        self,
        kind: Literal["proceed", "skip", "conflict"],
        commands: list[PyinfraCommand] | None = None,
        reason: str | None = None,
        report: str | None = None,
    ):
        self.kind = kind
        self.commands = commands or []
        self.reason = reason
        self.report = report


def _format_fact_changes(changed_facts: list[tuple[FactRead, Any]]) -> str:
    return ", ".join(
        f"{read.label()}: {read.value!r} -> {new_value!r}" for read, new_value in changed_facts
    )


def _format_conflict_report(
    plan: ValidatedOpPlan,
    new_commands: list[PyinfraCommand],
    changed_facts: list[tuple[FactRead, Any]],
) -> str:
    planned_targets = plan.targets
    new_targets = _targets_of(new_commands)

    reasons = []
    if new_targets != planned_targets:
        reasons.append("write targets differ")
    if len(new_commands) > len(plan.commands):
        reasons.append("change scope expanded")
    if not reasons:
        reasons.append("commands differ from the approved plan")

    lines = [
        f"Approved plan is no longer valid ({'; '.join(reasons)}):",
        "    facts changed since approval:",
    ]
    lines.extend(
        f"      {read.label()}: {read.value!r} -> {new_value!r}"
        for read, new_value in changed_facts
    )
    lines.append("    approved commands:")
    lines.extend(f"      {_display_command(command)}" for command in plan.commands)
    lines.append("    recomputed commands:")
    lines.extend(f"      {_display_command(command)}" for command in new_commands)
    lines.append(f"    approved targets: {', '.join(sorted(planned_targets)) or '(none)'}")
    lines.append(f"    recomputed targets: {', '.join(sorted(new_targets)) or '(none)'}")
    return "\n".join(lines)


def resolve_validated_plan(
    host: Host,
    op_data: StateOperationHostData,
    plan: ValidatedOpPlan,
) -> PlanResolution:
    """
    Validate an approved plan against the current state of the host. Facts are
    re-read first; only when they have changed is the operation recomputed.
    """

    changed_facts: list[tuple[FactRead, Any]] = []
    for read in plan.fact_reads:
        new_value = host.get_fact(read.fact_cls, *read.args, **read.kwargs)
        if new_value != read.value:
            changed_facts.append((read, new_value))

    if not changed_facts:
        # Facts unchanged since approval: run the approved commands exactly as
        # recorded, without re-evaluating the operation.
        return PlanResolution("proceed", commands=plan.commands)

    # Facts changed: recompute only this operation's commands.
    new_commands = list(op_data.command_generator())

    if not new_commands:
        return PlanResolution(
            "skip",
            reason=(
                "facts changed since the plan was approved "
                f"({_format_fact_changes(changed_facts)}), recomputed operation makes no "
                "changes - the target is already in the desired state"
            ),
        )

    planned_keys = [_command_key(command) for command in plan.commands]
    new_keys = [_command_key(command) for command in new_commands]

    if new_keys == planned_keys:
        return PlanResolution("proceed", commands=new_commands)

    if _is_submultiset(new_keys, planned_keys):
        # Recomputed commands are a subset of the approved plan (reduced
        # scope), still safe to apply.
        return PlanResolution("proceed", commands=new_commands)

    return PlanResolution(
        "conflict",
        report=_format_conflict_report(plan, new_commands, changed_facts),
    )
