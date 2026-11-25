"""
Compensation (saga-style rollback) support for deploys.

An operation may declare a ``_compensate`` generator that yields the commands needed to
undo its changes, together with an optional ``_compensation_verify`` callable describing
how to confirm the target is still the one the operation changed. When a later operation
fails on a host, ``run_ops`` replays the recorded compensations in reverse order.

A compensation is only safe to run when the target has not been touched by anybody else
since the forward operation succeeded. The verifier therefore runs first: returning
``False`` (or raising) means the target drifted - rollback stops in a conflict state and
the remaining changes are left for a human to judge, rather than blindly reverting work
that may belong to another actor.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import TYPE_CHECKING, Any, cast
from collections.abc import Callable, Iterable

from pyinfra import logger

from .arguments import CONNECTOR_ARGUMENT_KEYS, AllArguments, ConnectorArguments
from .command import PyinfraCommand, StringCommand
from .exceptions import CompensationError
from .output import format_text

if TYPE_CHECKING:
    from .host import Host
    from .state import State


# A zero-argument function/generator yielding shell strings/PyinfraCommands that undo
# an operation's changes. Called with the operation's host/state context active.
Compensator = Callable[[], Iterable[Any]]

# Returns True when the recorded target is still in the state the forward operation
# produced, False when somebody else changed it. Exceptions are treated as conflicts.
CompensationVerifier = Callable[[], bool]


class CompensationStatus(str, Enum):
    # Compensation commands ran and (where a verifier was given) the target was confirmed.
    REVERTED = "reverted"
    # Compensation commands ran, but no verifier confirmed the target afterwards.
    REVERTED_UNCONFIRMED = "reverted_unconfirmed"
    # The verifier reported external drift, or could not establish ownership. The
    # distinct ``CONFLICT_ERROR`` name is retained while sharing the same stop state.
    CONFLICT = "conflict"
    CONFLICT_ERROR = "conflict"
    # A compensation command itself failed partway through.
    FAILED = "failed"
    # Not attempted because rollback had already stopped at an earlier conflict/failure.
    SKIPPED = "skipped"
    # A successful change declared no compensator and needs a human decision.
    MANUAL = "manual"
    # A failed operation may have applied some commands, so its exact state is unknown.
    UNKNOWN = "unknown"


@dataclass
class CompensationRecord:
    """
    A single successful, compensatable operation on a host.

    Records everything required to undo the change later: the operation identity, the
    connector arguments the forward commands used, and the verification scope that
    guards against reverting somebody else's work.
    """

    host: Host | None
    op_hash: str
    op_names: set[str]
    compensator: Compensator | None = None
    verifier: CompensationVerifier | None = None
    scope: str | None = None
    connector_arguments: ConnectorArguments = field(
        default_factory=lambda: cast(ConnectorArguments, {}),
    )
    sequence: int = 0
    command_count: int = 0
    kind: str = "applied"
    # Filled in when/if the record is replayed:
    status: CompensationStatus | None = None
    detail: str | None = None
    commands_run: list[str] = field(default_factory=list)
    operation_meta: Any = None

    @property
    def label(self) -> str:
        base = ", ".join(sorted(self.op_names)) or self.op_hash
        return f"{base} (scope: {self.scope})" if self.scope else base

    @property
    def scoped_label(self) -> str:
        return self.label

    # Compatibility aliases for report/outcome APIs that call the operation names
    # ``names`` and the executed compensation commands ``reverted_commands``.
    @property
    def names(self) -> tuple[str, ...]:
        return tuple(sorted(self.op_names))

    @property
    def change(self) -> CompensationRecord:
        return self

    @property
    def reverted_commands(self) -> list[str]:
        return self.commands_run

    @reverted_commands.setter
    def reverted_commands(self, value: list[str]) -> None:
        self.commands_run = value

    @property
    def is_compensable(self) -> bool:
        return self.kind == "applied" and self.compensator is not None

    @property
    def reason(self) -> str:
        if self.kind == "unknown":
            count = self.command_count or "some"
            return (
                f"failed after {count} command(s), so its state is unknown and needs manual review"
            )
        if self.compensator is None:
            return "no `_compensate` declared, so it cannot be reverted automatically"
        return self.detail or "requires manual review"


# Records created by the execution path use ``CompensationRecord``. ``UnknownChange``
# remains a semantic alias for callers that want to name the kind explicitly.
UnknownChange = CompensationRecord


@dataclass
class ManualChange:
    """A change deliberately left for a human to judge."""

    label: str
    reason: str
    op_hash: str | None = None
    names: tuple[str, ...] = ()
    scope: str | None = None
    status: CompensationStatus = CompensationStatus.MANUAL
    operation_meta: Any = None


@dataclass
class AppliedChange:
    """
    Compatibility record shape used by journal-level callers.

    The execution engine records :class:`CompensationRecord`; this class accepts the
    older/journal API fields (``names``, ``compensate`` and ``verify``) directly.
    """

    sequence: int
    op_hash: str
    names: tuple[str, ...]
    scope: str | None
    command_count: int
    compensate: Compensator | None
    verify: CompensationVerifier | None
    connector_arguments: ConnectorArguments
    operation_meta: Any = None
    status: CompensationStatus | None = None
    detail: str | None = None
    commands_run: list[str] = field(default_factory=list)
    kind: str = "applied"
    host: Host | None = None

    @property
    def op_names(self) -> set[str]:
        return set(self.names)

    @property
    def compensator(self) -> Compensator | None:
        return self.compensate

    @property
    def verifier(self) -> CompensationVerifier | None:
        return self.verify

    @property
    def reverted_commands(self) -> list[str]:
        return self.commands_run

    @property
    def label(self) -> str:
        base = ", ".join(self.names) or self.op_hash
        return f"{base} (scope: {self.scope})" if self.scope else base

    @property
    def scoped_label(self) -> str:
        return self.label

    @property
    def change(self) -> AppliedChange:
        return self

    @property
    def is_compensable(self) -> bool:
        return self.compensate is not None

    @property
    def reason(self) -> str:
        return self.detail or "requires manual review"


@dataclass
class CompensationReport:
    """
    Outcome of a rollback pass on one host.

    ``fully_reverted`` is only True when every recorded compensation was replayed and
    confirmed. A conflict, a failed compensation command, or a successful operation that
    had no compensator at all means the batch cannot be claimed as fully restored.
    """

    host: Host
    failed_operation: str | None = None
    reverted: list[CompensationRecord] = field(default_factory=list)
    conflicts: list[CompensationRecord] = field(default_factory=list)
    failed: list[CompensationRecord] = field(default_factory=list)
    outcomes: list[CompensationRecord] = field(default_factory=list)
    # Successful ops that made changes but declared no compensator - human judgement only.
    uncompensated: list[tuple[str, set[str]]] = field(default_factory=list)
    manual_scopes: dict[str, str | None] = field(default_factory=dict)
    unknown: list[CompensationRecord] = field(default_factory=list)
    stopped_at: CompensationRecord | None = None

    @property
    def host_name(self) -> str:
        return self.host.name

    @property
    def manual_changes(self) -> list[ManualChange]:
        return [
            ManualChange(
                op_hash=op_hash,
                names=tuple(sorted(names)),
                scope=self.manual_scopes.get(op_hash),
                label=(
                    f"{', '.join(sorted(names)) or op_hash} (scope: {self.manual_scopes[op_hash]})"
                    if self.manual_scopes.get(op_hash)
                    else ", ".join(sorted(names)) or op_hash
                ),
                reason="no `_compensate` declared, so it cannot be reverted automatically",
                status=CompensationStatus.MANUAL,
            )
            for op_hash, names in self.uncompensated
        ]

    @property
    def unknown_changes(self) -> list[ManualChange]:
        return [
            ManualChange(
                op_hash=record.op_hash,
                names=record.names,
                scope=record.scope,
                label=record.label,
                reason=record.reason,
                status=CompensationStatus.UNKNOWN,
                operation_meta=record.operation_meta,
            )
            for record in self.unknown
        ]

    @property
    def fully_reverted(self) -> bool:
        return (
            self.stopped_at is None
            and not self.conflicts
            and not self.failed
            and not self.uncompensated
            and not self.unknown
            and all(record.status is CompensationStatus.REVERTED for record in self.outcomes)
        )

    @property
    def has_conflict(self) -> bool:
        return any(
            record.status in (CompensationStatus.CONFLICT, CompensationStatus.CONFLICT_ERROR)
            for record in self.conflicts
        )

    def summary_line(self) -> str:
        parts = [f"{len(self.reverted)} change(s) reverted"]
        if self.uncompensated:
            parts.append(f"{len(self.uncompensated)} require(s) manual judgement")
        if self.unknown:
            parts.append(f"{len(self.unknown)} unconfirmed")
        if self.conflicts:
            parts.append(f"{len(self.conflicts)} conflict(s)")
        if self.failed:
            parts.append(f"{len(self.failed)} failed")
        summary = "; ".join(parts)
        if self.fully_reverted:
            return f"{summary}; batch fully restored"
        return f"{summary}; batch was NOT fully restored"

    def summary_lines(self) -> list[str]:
        lines = [f"Compensation summary for {self.host.name}:"]
        for record in self.outcomes:
            status = record.status.value if record.status else "pending"
            lines.append(f"  - {status}: {record.scoped_label}")
        for change in self.manual_changes:
            lines.append(f"  - MANUAL: {change.label} - {change.reason}")
        for record in self.unknown:
            lines.append(f"  - UNKNOWN: {record.label} - {record.reason}")
        if self.fully_reverted:
            lines.append("  all confirmed changes were reverted")
        else:
            lines.append(f"  {self.summary_line()}")
        return lines

    # Older/report API name.
    def format_lines(self) -> list[str]:
        return self.summary_lines()

    def log(self, host: Host | None = None) -> None:
        target = host or self.host
        for line in self.summary_lines():
            logger.info(f"{target.print_prefix}{line}" if line.startswith("  ") else line)


class CompensationJournal:
    """
    Per-host ordered record of applied changes and rollback reports.

    The run boundary prevents a later :func:`pyinfra.api.operations.run_ops` call from
    rolling back records left by an earlier successful batch.
    """

    def __init__(self, records: list[CompensationRecord] | None = None) -> None:
        self.records: list[CompensationRecord] = records if records is not None else []
        self.reports: list[CompensationReport] = []
        self.is_reverting: bool = False
        self._sequence = 0
        self._run_start = 0
        self.manual_start = 0
        self.unknown_start = 0

    def start_run(self) -> None:
        self._run_start = len(self.records)

    def begin_run_scopes(self, manual_count: int, unknown_count: int) -> None:
        self.start_run()
        self.manual_start = manual_count
        self.unknown_start = unknown_count

    def next_sequence(self) -> int:
        self._sequence += 1
        return self._sequence

    def record_count(self) -> int:
        return len(self.records)

    def drop_last(self, count: int) -> None:
        if count:
            del self.records[-count:]

    @property
    def applied(self) -> list[CompensationRecord]:
        return [record for record in self.records if record.kind == "applied"]

    @property
    def unknown(self) -> list[CompensationRecord]:
        return [record for record in self.records if record.kind == "unknown"]

    @property
    def last_report(self) -> CompensationReport | None:
        return self.reports[-1] if self.reports else None

    def iter_current_run(self) -> list[CompensationRecord]:
        return self.records[self._run_start :]

    def end_run(self) -> None:
        """Remove this run's records after a successful run."""

        del self.records[self._run_start :]


def _connector_arguments(global_arguments: AllArguments) -> ConnectorArguments:
    # See: https://github.com/python/mypy/issues/10371
    return cast(
        ConnectorArguments,
        {key: global_arguments[key] for key in CONNECTOR_ARGUMENT_KEYS if key in global_arguments},  # type: ignore[literal-required] # noqa
    )


def record_compensation(
    state: State,
    host: Host,
    op_hash: str,
    executed_commands: int,
) -> None:
    """
    Record a successfully completed, compensatable operation for later rollback.

    Called from the operation execution path only once the operation succeeded;
    ``executed_commands`` is how many commands it actually ran, since a no-op has
    nothing to undo. The operation meta is not marked complete at this point, so the
    decision is passed in rather than read back from ``OperationMeta``.
    """

    if not executed_commands:
        return

    op_data = state.get_op_data_for_host(host, op_hash)
    global_arguments = op_data.global_arguments
    compensator = global_arguments.get("_compensate")
    if compensator is None:
        return

    sequence = 0
    journal = getattr(state, "get_compensation_journal", lambda _host: None)(host)
    if journal is not None:
        sequence = journal.next_sequence()

    record = CompensationRecord(
        host=host,
        op_hash=op_hash,
        op_names=set(state.get_op_meta(op_hash).names),
        compensator=compensator,
        verifier=global_arguments.get("_compensation_verify"),
        scope=global_arguments.get("_compensation_scope"),
        connector_arguments=_connector_arguments(global_arguments),
        sequence=sequence,
        command_count=executed_commands,
        operation_meta=op_data.operation_meta,
    )
    state.compensation_records[host].append(record)
    logger.debug("Recorded compensatable operation %r on %s", record.label, host)


def record_uncompensated_change(
    state: State,
    host: Host,
    op_hash: str,
    executed_commands: int,
) -> None:
    """
    Note a successful change that has no compensator, so the rollback report can flag it
    as requiring manual judgement rather than silently claiming a full restore.
    """

    if not executed_commands:
        return

    op_data = state.get_op_data_for_host(host, op_hash)
    if op_data.global_arguments.get("_compensate") is not None:
        return

    state.uncompensated_changes[host].append(
        (op_hash, set(state.get_op_meta(op_hash).names)),
    )


def record_unknown_change(
    state: State,
    host: Host,
    op_hash: str,
    executed_commands: int,
    detail: str | None = None,
) -> None:
    """
    Record a failed operation which may have applied some commands.

    Such an operation cannot safely be compensated from a finished-state verifier:
    the commands may have completed, partially completed, or left a resource in an
    intermediate state. It is therefore exposed as an unconfirmed item for a human.
    """

    if not executed_commands:
        return

    op_data = state.get_op_data_for_host(host, op_hash)
    global_arguments = op_data.global_arguments
    record = CompensationRecord(
        host=host,
        op_hash=op_hash,
        op_names=set(state.get_op_meta(op_hash).names),
        compensator=global_arguments.get("_compensate"),
        verifier=global_arguments.get("_compensation_verify"),
        scope=global_arguments.get("_compensation_scope"),
        connector_arguments=_connector_arguments(global_arguments),
        command_count=executed_commands,
        kind="unknown",
        status=CompensationStatus.UNKNOWN,
        detail=detail,
        operation_meta=op_data.operation_meta,
    )
    state.unknown_changes[host].append(record)
    state.compensation_records[host].append(record)


def _materialize_commands(compensator: Compensator) -> list[PyinfraCommand]:
    result: Any = compensator()
    # Convenience for ``lambda: "rm ..."``; ordinary generators/lists remain iterated.
    if isinstance(result, (str, PyinfraCommand)):
        result = [result]

    commands: list[PyinfraCommand] = []
    for command in result:
        if isinstance(command, str):
            command = StringCommand(command.strip())
        if not isinstance(command, PyinfraCommand):
            raise TypeError(f"{command} is an invalid pyinfra command!")
        commands.append(command)
    return commands


def _execute_commands(
    state: State,
    host: Host,
    commands: list[PyinfraCommand],
    base_connector_arguments: ConnectorArguments,
) -> tuple[bool, list[str], str | None]:
    executed: list[str] = []
    for command in commands:
        connector_arguments = base_connector_arguments.copy()
        connector_arguments.update(command.connector_arguments)
        try:
            result = command.execute(state, host, connector_arguments)
        except Exception as e:  # noqa: BLE001 - converted into a failed report item
            return False, executed, f"raised {type(e).__name__}: {e}"

        # StringCommand (and the file/rsync commands) return a (status, output) tuple,
        # while FunctionCommand returns a bare boolean/None - mirror the main execution
        # path's handling so a failed shell command is recognised as a failure.
        if isinstance(result, tuple):
            status = result[0]
        else:
            status = result

        if status is False:
            return False, executed, None
        executed.append(str(command))
    return True, executed, None


def _set_operation_status(record: CompensationRecord) -> None:
    if record.operation_meta is not None:
        try:
            record.operation_meta._compensation_status = record.status
        except Exception:  # noqa: BLE001 - reporting must not hide rollback result
            pass


def run_compensation(
    state: State,
    host: Host,
    failed_op_hash: str | None = None,
) -> CompensationReport | None:
    """
    Replay the host's recorded compensations in reverse order.

    Before each compensation its verifier confirms the target is still the one the
    forward operation changed. If verification fails the rollback stops in a conflict
    state, marks older records as skipped, and leaves the disputed target untouched.
    The report never claims the batch was fully restored unless every change, including
    changes without compensators and partially failed operations, is accounted for.

    Returns ``None`` when the host has no explicit compensatable operation; manual and
    unknown items are still logged on the state/report paths. A non-``None`` report that
    is not fully reverted raises :class:`~pyinfra.api.exceptions.CompensationError`.
    """

    journal = (
        state.get_compensation_journal(host) if hasattr(state, "get_compensation_journal") else None
    )
    current_records = (
        journal.iter_current_run()
        if journal is not None
        else state.compensation_records.get(host, [])
    )

    manual_start = getattr(journal, "manual_start", 0)
    unknown_start = getattr(journal, "unknown_start", 0)
    uncompensated = state.uncompensated_changes.get(host, [])[manual_start:]
    unknown = state.unknown_changes.get(host, [])[unknown_start:]

    compensatable = [
        record
        for record in current_records
        if record.kind == "applied" and record.compensator is not None
    ]

    if not compensatable:
        if uncompensated or unknown:
            logger.warning(
                "%s%s",
                host.print_prefix,
                format_text(
                    "No compensatable operations to roll back; "
                    f"{len(uncompensated)} manual and {len(unknown)} unconfirmed change(s) "
                    "require judgement",
                    "yellow",
                ),
            )
        return None

    failed_operation = None
    if failed_op_hash is not None and failed_op_hash in state.op_meta:
        op_names = state.get_op_meta(failed_op_hash).names
        failed_operation = ", ".join(sorted(op_names)) or failed_op_hash

    manual_scopes: dict[str, str | None] = {}
    for op_hash, _names in uncompensated:
        try:
            manual_scopes[op_hash] = state.get_op_data_for_host(host, op_hash).global_arguments.get(
                "_compensation_scope",
            )
        except Exception:  # noqa: BLE001 - scope is only report metadata
            manual_scopes[op_hash] = None

    report = CompensationReport(
        host=host,
        failed_operation=failed_operation,
        uncompensated=list(uncompensated),
        manual_scopes=manual_scopes,
        unknown=list(unknown),
    )
    if journal is not None:
        journal.reports.append(report)

    logger.info(
        "%s%s",
        host.print_prefix,
        format_text(f"Rolling back {len(compensatable)} compensatable operation(s)", "yellow"),
    )

    if journal is not None:
        journal.is_reverting = True
    try:
        blocked = False
        for record in reversed(compensatable):
            op_label = record.label

            if blocked:
                record.status = CompensationStatus.SKIPPED
                record.detail = "not attempted because rollback had stopped"
                report.outcomes.append(record)
                _set_operation_status(record)
                continue

            # Verify the target hasn't drifted before undoing anything.
            if record.verifier is not None:
                try:
                    still_ours = bool(record.verifier())
                except Exception as e:  # noqa: BLE001 - verifier failures are conflicts
                    record.status = CompensationStatus.CONFLICT_ERROR
                    record.detail = f"verifier raised {type(e).__name__}: {e}"
                    report.conflicts.append(record)
                    report.outcomes.append(record)
                    report.stopped_at = record
                    blocked = True
                    _set_operation_status(record)
                    logger.error(
                        "%s%s",
                        host.print_prefix,
                        format_text(
                            f"Compensation conflict on {op_label}: verification raised {e}",
                            "red",
                        ),
                    )
                    continue

                if not still_ours:
                    record.status = CompensationStatus.CONFLICT
                    scope = f" {record.scope}" if record.scope else ""
                    record.detail = f"target{scope} was updated by another party"
                    report.conflicts.append(record)
                    report.outcomes.append(record)
                    report.stopped_at = record
                    blocked = True
                    _set_operation_status(record)
                    logger.error(
                        "%s%s",
                        host.print_prefix,
                        format_text(
                            f"Compensation conflict on {op_label}: target changed externally; "
                            "stopping rollback",
                            "red",
                        ),
                    )
                    continue

            # Target confirmed (or no verifier was provided) - run the compensator.
            try:
                if record.compensator is None:
                    raise TypeError("compensation record has no compensator")
                commands = _materialize_commands(record.compensator)
            except Exception as e:  # noqa: BLE001 - report rather than crash rollback
                record.status = CompensationStatus.FAILED
                record.detail = f"compensator raised {type(e).__name__}: {e}"
                report.failed.append(record)
                report.outcomes.append(record)
                report.stopped_at = record
                blocked = True
                _set_operation_status(record)
                logger.error(
                    "%s%s",
                    host.print_prefix,
                    format_text(f"Compensation failed on {op_label}: {e}", "red"),
                )
                continue

            ok, executed, execution_error = _execute_commands(
                state,
                host,
                commands,
                record.connector_arguments,
            )
            record.commands_run = executed
            if not ok:
                record.status = CompensationStatus.FAILED
                failed_command = commands[len(executed)] if len(executed) < len(commands) else None
                if execution_error is not None:
                    record.detail = f"compensation command {execution_error}"
                elif failed_command is not None:
                    record.detail = f"compensation command failed: {failed_command}"
                else:
                    record.detail = "compensation command failed"
                report.failed.append(record)
                report.outcomes.append(record)
                report.stopped_at = record
                blocked = True
                _set_operation_status(record)
                logger.error(
                    "%s%s",
                    host.print_prefix,
                    format_text(
                        f"Compensation failed on {op_label}: {record.detail}",
                        "red",
                    ),
                )
                continue

            # With a verifier, ownership/scope was confirmed before the undo and the
            # command succeeded. Without one, the commands ran but restoration is not
            # independently confirmed.
            record.status = (
                CompensationStatus.REVERTED
                if record.verifier is not None
                else CompensationStatus.REVERTED_UNCONFIRMED
            )
            record.detail = (
                None if record.verifier is not None else "no `_compensation_verify` declared"
            )
            report.reverted.append(record)
            report.outcomes.append(record)
            _set_operation_status(record)
            logger.info(
                "%s%s",
                host.print_prefix,
                format_text(f"Reverted {op_label}", "green"),
            )
    finally:
        if journal is not None:
            journal.is_reverting = False

    for line in report.summary_lines():
        logger.info(f"{host.print_prefix}{line}" if line.startswith("  ") else line)

    if not report.fully_reverted:
        raise CompensationError(host, report)

    return report


# Descriptive alias for callers that prefer the saga terminology.
revert_applied_changes = run_compensation
