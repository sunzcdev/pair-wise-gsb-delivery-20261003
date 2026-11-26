# Copyright 2026 GSB
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Structured phase and host-diagnostic records.

The objects in this module are plain data containers so they can be
JSON-serialized by a Nox child and reconstructed by the parallel parent.
"""

from __future__ import annotations

import dataclasses
import os
import sys
import time
from collections.abc import Callable, Mapping, Sequence
from typing import Any

__all__ = [
    "PHASE_ABORTED",
    "PHASE_FAILED",
    "PHASE_NOT_RUN",
    "PHASE_NOT_STARTED",
    "PHASE_RUNNING",
    "PHASE_SKIPPED",
    "PHASE_SUCCESS",
    "Diagnostic",
    "DiagnosticAction",
    "DiagnosticReport",
    "DiagnosticSettlement",
    "DiagnosticState",
    "DiagnosticStatus",
    "DiagnosticSummary",
    "HostDiagnosticReport",
    "HostDiagnosticSettlement",
    "HostDiagnosticState",
    "HostDiagnostic",
    "HostDiagnosticAction",
    "HostAction",
    "HostCheck",
    "PhaseReport",
    "PhaseState",
    "diagnostic",
    "diagnostic_action",
    "host_diagnostic",
    "host_action",
    "host_check",
    "register_diagnostic",
    "register_host_diagnostic",
]

PHASE_NOT_STARTED = "not_started"
PHASE_NOT_RUN = "not_run"
PHASE_RUNNING = "running"
PHASE_SUCCESS = "success"
PHASE_FAILED = "failed"
PHASE_ABORTED = "aborted"
PHASE_SKIPPED = "skipped"

_FAILURE_STATUSES = {PHASE_FAILED, PHASE_ABORTED}


def _status_name(status: Any) -> str:
    if isinstance(status, str):
        return status.lower()
    name = getattr(status, "name", None)
    if name is not None:
        return str(name).lower()
    value = getattr(status, "value", status)
    return str(value).lower()


@dataclasses.dataclass
class PhaseState:
    """The state of one named execution phase."""

    name: str
    status: str = PHASE_NOT_STARTED
    started: bool = False
    completed: bool = False
    returncode: int | None = None
    reason: str | None = None
    duration: float = 0.0
    inputs: Any = None
    actions: list[dict[str, Any]] = dataclasses.field(default_factory=list)
    extra: dict[str, Any] = dataclasses.field(default_factory=dict)
    started_at: float | None = dataclasses.field(default=None, repr=False, compare=False)

    def start(self) -> PhaseState:
        if not self.started:
            self.started = True
            self.status = PHASE_RUNNING
            self.started_at = time.perf_counter()
        return self

    def finish(
        self,
        status: Any = PHASE_SUCCESS,
        *,
        returncode: int | None = None,
        reason: str | None = None,
    ) -> PhaseState:
        status_name = _status_name(status)
        if not self.started:
            self.start()
        if self.started_at is not None:
            self.duration = max(0.0, time.perf_counter() - self.started_at)
            self.started_at = None
        self.completed = True

        # A later successful command must not erase an earlier failure in the
        # same phase.
        if self.status in _FAILURE_STATUSES and status_name == PHASE_SUCCESS:
            if returncode is not None and self.returncode is None:
                self.returncode = returncode
            return self

        self.status = status_name
        if returncode is not None:
            self.returncode = returncode
        if reason is not None:
            self.reason = reason
        return self

    def skip(self, reason: str | None = None) -> PhaseState:
        self.status = PHASE_SKIPPED
        self.started = False
        self.completed = False
        if reason is not None:
            self.reason = reason
        return self

    def mark_not_run(self, reason: str | None = None) -> PhaseState:
        self.status = PHASE_NOT_RUN
        self.started = False
        self.completed = False
        if reason is not None:
            self.reason = reason
        return self

    def record_action(self, action: Mapping[str, Any]) -> None:
        self.actions.append(dict(action))

    @property
    def result(self) -> str:
        return self.status

    def to_dict(self) -> dict[str, Any]:
        data: dict[str, Any] = {
            "name": self.name,
            "status": self.status,
            "state": self.status,
            "result": self.status,
            "started": self.started,
            "ran": self.started and self.status != PHASE_NOT_RUN,
            "completed": self.completed,
            "returncode": self.returncode,
            "reason": self.reason,
            "duration": self.duration,
        }
        if self.inputs is not None:
            data["inputs"] = self.inputs
        if self.actions:
            data["actions"] = list(self.actions)
        if self.extra:
            data.update(self.extra)
        return data

    @classmethod
    def from_dict(cls, name: str, data: Mapping[str, Any]) -> PhaseState:
        status = data.get("status", data.get("result", PHASE_NOT_STARTED))
        reserved = {
            "name",
            "status",
            "state",
            "result",
            "started",
            "ran",
            "completed",
            "returncode",
            "reason",
            "duration",
            "inputs",
            "actions",
        }
        return cls(
            name=name,
            status=_status_name(status),
            started=bool(data.get("started", False)),
            completed=bool(data.get("completed", False)),
            returncode=data.get("returncode"),
            reason=data.get("reason"),
            duration=float(data.get("duration", 0.0) or 0.0),
            inputs=data.get("inputs"),
            actions=[dict(x) for x in data.get("actions", []) if isinstance(x, Mapping)],
            extra={k: v for k, v in data.items() if k not in reserved},
        )


class PhaseReport:
    """Separate environment, test, cleanup and diagnostic settlements."""

    def __init__(
        self,
        *,
        environment: PhaseState | Mapping[str, Any] | None = None,
        test: PhaseState | Mapping[str, Any] | None = None,
        cleanup: PhaseState | Mapping[str, Any] | None = None,
        diagnostic: PhaseState | Mapping[str, Any] | Sequence[Mapping[str, Any]] | None = None,
    ) -> None:
        self.environment = _coerce_phase("environment", environment)
        self.test = _coerce_phase("test", test)
        self.cleanup = _coerce_phase("cleanup", cleanup)
        if isinstance(diagnostic, Sequence) and not isinstance(diagnostic, (str, bytes, Mapping)):
            actions = [dict(x) for x in diagnostic if isinstance(x, Mapping)]
            self.diagnostic = PhaseState(
                "diagnostic",
                actions=actions,
                status=_aggregate_action_status(actions),
                started=bool(actions),
                completed=bool(actions),
            )
        else:
            self.diagnostic = _coerce_phase("diagnostic", diagnostic)

    @property
    def phases(self) -> dict[str, PhaseState]:
        return {
            "environment": self.environment,
            "test": self.test,
            "cleanup": self.cleanup,
            "diagnostic": self.diagnostic,
        }

    def __getitem__(self, name: str) -> PhaseState:
        aliases = {"env": "environment", "diagnostics": "diagnostic"}
        return self.phases[aliases.get(name, name)]

    def get(self, name: str) -> PhaseState | None:
        try:
            return self[name]
        except KeyError:
            return None

    def statuses(self) -> dict[str, str]:
        statuses = {name: phase.status for name, phase in self.phases.items()}
        statuses.update(
            {
                "env": self.environment.status,
                "tests": self.test.status,
                "normal_cleanup": self.cleanup.status,
                "diagnostics": self.diagnostic.status,
                "host_diagnostic": self.diagnostic.status,
                "host_diagnostics": self.diagnostic.status,
            }
        )
        return statuses

    def diagnostic_records(self) -> list[dict[str, Any]]:
        if self.diagnostic.actions:
            return [dict(action) for action in self.diagnostic.actions]
        if self.diagnostic.status != PHASE_NOT_STARTED:
            return [self.diagnostic.to_dict()]
        return []

    def to_dict(self) -> dict[str, Any]:
        env = self.environment.to_dict()
        test = self.test.to_dict()
        cleanup = self.cleanup.to_dict()
        diagnostic = self.diagnostic.to_dict()
        records = self.diagnostic_records()
        statuses = self.statuses()
        phase_map = {
            "environment": env,
            "env": env,
            "test": test,
            "tests": test,
            "testing": test,
            "cleanup": cleanup,
            "clean_up": cleanup,
            "normal_cleanup": cleanup,
            "ordinary_cleanup": cleanup,
            "teardown": cleanup,
            "diagnostic": diagnostic,
            # Plural access is natural when reading about all diagnostic actions.
            "diagnostics": diagnostic,
            "host_diagnostic": diagnostic,
            "host_diagnostics": diagnostic,
        }
        settlement_map = {
            **phase_map,
            "diagnostic": diagnostic,
            "diagnostics": diagnostic,
        }
        return {
            "phases": phase_map,
            "settlements": settlement_map,
            "phase_settlements": [env, test, cleanup, diagnostic],
            "phase_list": [env, test, cleanup, diagnostic],
            "phases_list": [env, test, cleanup, diagnostic],
            "steps": [env, test, cleanup, diagnostic],
            "phase_results": [env, test, cleanup, diagnostic],
            "environment_settlement": env,
            "test_settlement": test,
            "cleanup_settlement": cleanup,
            "diagnostic_settlement": records[0] if len(records) == 1 else diagnostic,
            "host_diagnostic_settlement": records[0]
            if len(records) == 1
            else diagnostic,
            "environment": env,
            "env": env,
            "test": test,
            "tests": test,
            "cleanup": cleanup,
            "normal_cleanup": cleanup,
            "ordinary_cleanup": cleanup,
            "teardown": cleanup,
            "diagnostic": diagnostic,
            "host_diagnostic": diagnostic,
            "host_diagnostic_phase": diagnostic,
            "diagnostics": records,
            "host_diagnostics": records,
            "diagnostic_actions": records,
            "host_diagnostic_actions": records,
            "actions": records,
            "diagnostic_results": records,
            "diagnostic_settlements": records,
            "states": statuses,
            "state": statuses,
            "statuses": statuses,
            "status_report": statuses,
            "phase_states": statuses,
            "phase_status": statuses,
            "phase_statuses": statuses,
            "test_status": test["status"],
            "test_state": test["status"],
            "test_result": test["status"],
            "test_started": test["started"],
            "test_ran": test["ran"],
            "tests_status": test["status"],
            "cleanup_status": cleanup["status"],
            "cleanup_state": cleanup["status"],
            "cleanup_result": cleanup["status"],
            "cleanup_started": cleanup["started"],
            "cleanup_ran": cleanup["ran"],
            "diagnostic_status": diagnostic["status"],
            "diagnostic_state": diagnostic["status"],
            "diagnostic_result": diagnostic["status"],
            "host_diagnostic_status": diagnostic["status"],
            "host_diagnostic_state": diagnostic["status"],
            "host_diagnostic_result": diagnostic["status"],
            "environment_status": env["status"],
            "environment_state": env["status"],
            "environment_result": env["status"],
            "env_status": env["status"],
            "env_state": env["status"],
            "env_result": env["status"],
        }

    @classmethod
    def from_value(cls, value: Any) -> PhaseReport | None:
        if value is None or isinstance(value, PhaseReport):
            return value
        if isinstance(value, Mapping):
            return cls.from_dict(value)
        return None

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> PhaseReport:
        nested = data.get("phases")
        if not isinstance(nested, Mapping) and isinstance(data.get("settlements"), Mapping):
            nested = data["settlements"]
        if isinstance(nested, Mapping):
            environment = nested.get("environment", nested.get("env"))
            test = nested.get("test", nested.get("tests", nested.get("testing")))
            cleanup = nested.get(
                "cleanup",
                nested.get(
                    "normal_cleanup",
                    nested.get("ordinary_cleanup", nested.get("teardown")),
                ),
            )
            diagnostic = nested.get(
                "diagnostic",
                nested.get("host_diagnostic", nested.get("diagnostics")),
            )
        else:
            environment = data.get("environment", data.get("env"))
            test = data.get("test", data.get("tests", data.get("testing")))
            cleanup = data.get(
                "cleanup",
                data.get(
                    "normal_cleanup",
                    data.get("ordinary_cleanup", data.get("teardown")),
                ),
            )
            diagnostic = data.get(
                "diagnostic",
                data.get("host_diagnostic", data.get("diagnostics")),
            )

        def phase_value(value: Any) -> Any:
            return value if isinstance(value, (Mapping, PhaseState, str)) else None

        report = cls(
            environment=phase_value(environment),
            test=phase_value(test),
            cleanup=phase_value(cleanup),
            diagnostic=diagnostic
            if isinstance(diagnostic, (Mapping, PhaseState, Sequence, str))
            and not isinstance(diagnostic, (bytes,))
            else None,
        )

        # Accept lightweight mappings such as {"test": "failed"} under any of
        # the common report names for the status map.
        for map_key in (
            "states",
            "state",
            "statuses",
            "phase_states",
            "phase_status",
            "phase_statuses",
            "status_report",
        ):
            status_map = data.get(map_key)
            if not isinstance(status_map, Mapping):
                continue
            aliases = {
                "environment": ("environment", "env"),
                "test": ("test", "tests", "testing"),
                "cleanup": (
                    "cleanup",
                    "normal_cleanup",
                    "ordinary_cleanup",
                    "teardown",
                ),
                "diagnostic": (
                    "diagnostic",
                    "diagnostics",
                    "host_diagnostic",
                    "host_diagnostics",
                ),
            }
            for phase_name, names in aliases.items():
                phase = report[phase_name]
                if phase.status != PHASE_NOT_STARTED:
                    continue
                value = next(
                    (status_map[name] for name in names if name in status_map),
                    None,
                )
                if isinstance(value, str):
                    status = _status_name(value)
                    phase.status = status
                    phase.started = status not in {
                        PHASE_NOT_STARTED,
                        PHASE_NOT_RUN,
                    }
                    phase.completed = phase.started and status != PHASE_RUNNING

        # Accept flat *_status/*_state/*_result fields from lightweight reports.
        flat_keys = {
            "environment": ("environment_status", "environment_state", "environment_result", "env_status"),
            "test": ("test_status", "test_state", "test_result"),
            "cleanup": ("cleanup_status", "cleanup_state", "cleanup_result"),
            "diagnostic": ("diagnostic_status", "diagnostic_state", "diagnostic_result"),
        }
        for phase_name, keys in flat_keys.items():
            phase = report[phase_name]
            if phase.status != PHASE_NOT_STARTED:
                continue
            for key in keys:
                if key in data and isinstance(data[key], str):
                    status = _status_name(data[key])
                    phase.status = status
                    phase.started = status not in {PHASE_NOT_STARTED, PHASE_NOT_RUN}
                    phase.completed = phase.started and status != PHASE_RUNNING
                    break
        for key in ("diagnostics", "host_diagnostics", "diagnostic_results", "diagnostic_settlements"):
            raw = data.get(key)
            if not isinstance(raw, Sequence) or isinstance(raw, (str, bytes)) or not raw:
                continue
            existing = {
                (a.get("name"), a.get("status", a.get("result")), a.get("returncode"))
                for a in report.diagnostic.actions
            }
            for item in raw:
                if not isinstance(item, Mapping):
                    continue
                action = dict(item)
                identity = (
                    action.get("name"),
                    action.get("status", action.get("result")),
                    action.get("returncode"),
                )
                if identity not in existing:
                    report.diagnostic.record_action(action)
                    existing.add(identity)
            if report.diagnostic.status == PHASE_NOT_STARTED:
                report.diagnostic.status = _aggregate_action_status(list(raw))
                report.diagnostic.started = True
                report.diagnostic.completed = True
            break
        return report


def _aggregate_action_status(actions: Sequence[Any]) -> str:
    statuses = [
        _status_name(x.get("status", x.get("result", PHASE_SUCCESS)))
        for x in actions
        if isinstance(x, Mapping)
    ]
    if any(status in _FAILURE_STATUSES for status in statuses):
        return PHASE_FAILED
    if statuses and all(status == PHASE_SKIPPED for status in statuses):
        return PHASE_SKIPPED
    return PHASE_SUCCESS if statuses else PHASE_NOT_STARTED


def _coerce_phase(
    name: str, value: PhaseState | Mapping[str, Any] | str | None
) -> PhaseState:
    if value is None:
        return PhaseState(name)
    if isinstance(value, PhaseState):
        value.name = name
        return value
    if isinstance(value, str):
        status = _status_name(value)
        started = status not in {PHASE_NOT_STARTED, PHASE_NOT_RUN}
        return PhaseState(
            name,
            status=status,
            started=started,
            completed=started and status != PHASE_RUNNING,
        )
    return PhaseState.from_dict(name, value)


class HostDiagnostic:
    """An explicitly registered action that only needs the host interpreter."""

    def __init__(
        self,
        target: Callable[..., Any] | str | Sequence[str | os.PathLike[str]] | None = None,
        *args: str | os.PathLike[str],
        name: str | None = None,
        inputs: str | Sequence[str] | Mapping[str, Any] | None = None,
        required_inputs: str | Sequence[str] | Mapping[str, Any] | None = None,
        requires_inputs: str | Sequence[str] | Mapping[str, Any] | None = None,
        available_inputs: str | Sequence[str] | Mapping[str, Any] | None = None,
        host_inputs: str | Sequence[str] | Mapping[str, Any] | None = None,
        declared_inputs: str | Sequence[str] | Mapping[str, Any] | None = None,
        input: str | Sequence[str] | Mapping[str, Any] | None = None,  # noqa: A002
        command: Sequence[str | os.PathLike[str]] | None = None,
        run: Callable[..., Any] | Sequence[str | os.PathLike[str]] | None = None,
        callback: Callable[..., Any] | None = None,
        action: Callable[..., Any] | Sequence[str | os.PathLike[str]] | None = None,
        **options: Any,
    ) -> None:
        self.func: Callable[..., Any] | None = None
        self.command: tuple[str, ...] | None = None
        self.options = dict(options)
        label = self.options.pop("label", None)
        # Registration-only keywords that are not Session.run() options.
        for metadata_key in (
            "kind",
            "phase",
            "when",
            "condition",
            "host",
            "runner",
            "scope",
        ):
            self.options.pop(metadata_key, None)
        for input_key in (
            "requires",
            "required",
            "available",
            "safe_inputs",
            "input_names",
            "inputs_available",
            "available_input_names",
        ):
            if input_key in self.options and inputs is None:
                inputs = self.options.pop(input_key)
            else:
                self.options.pop(input_key, None)
        for alias in (
            declared_inputs,
            available_inputs,
            host_inputs,
            required_inputs,
            requires_inputs,
            input,
        ):
            if alias is not None and inputs is None:
                inputs = alias
                break
        if inputs is None:
            # Allow declarative subclasses to set ``inputs`` as a class attribute.
            inputs = getattr(self, "inputs", ()) or ()
        self.inputs = inputs
        self.name = name or label

        explicit_command = command
        if explicit_command is None and action is not None and not callable(action):
            explicit_command = action
        if explicit_command is None and run is not None and not callable(run):
            explicit_command = run

        if explicit_command is not None:
            self.command = tuple(os.fspath(x) for x in explicit_command)
        elif target is not None and not callable(target):
            if isinstance(target, (str, os.PathLike)):
                self.command = (os.fspath(target), *(os.fspath(x) for x in args))
            else:
                self.command = tuple(os.fspath(x) for x in target)
        else:
            candidate = target if callable(target) else None
            candidate = candidate or (run if callable(run) else None)
            candidate = candidate or callback
            candidate = candidate or (action if callable(action) else None)
            if candidate is not None:
                self.func = candidate
                self.name = self.name or getattr(candidate, "__name__", None)

        if self.name is None and self.command:
            self.name = " ".join(self.command)
        elif self.name is None:
            self.name = "host-diagnostic"

    def declared_inputs(self) -> Any:
        if isinstance(self.inputs, Mapping):
            return dict(self.inputs)
        if self.inputs is None:
            return []
        if isinstance(self.inputs, str):
            return [self.inputs]
        return list(self.inputs)

    def available_inputs(self, session: Any | None = None) -> dict[str, Any]:
        """Return known host-side inputs without touching the session venv."""

        config = getattr(getattr(session, "_runner", None), "global_config", None)
        runner = getattr(session, "_runner", None)
        env_dir = getattr(session, "env_dir", None)
        values: dict[str, Any] = {
            "python": sys.executable,
            "host_python": sys.executable,
            "host_python_executable": sys.executable,
            "interpreter": sys.executable,
            "sys.executable": sys.executable,
            "cwd": os.getcwd(),
            "working_directory": os.getcwd(),
            "invoked_from": getattr(config, "invoked_from", os.getcwd()),
            "noxfile": getattr(config, "noxfile", None),
            "posargs": list(getattr(session, "posargs", []) or []),
            "argv": list(getattr(session, "posargs", []) or []),
            "args": list(getattr(session, "posargs", []) or []),
            "arguments": list(getattr(session, "posargs", []) or []),
            "positionals": list(getattr(session, "posargs", []) or []),
            "session": session,
            "host": session,
            "host_session": session,
            "session_object": session,
            "runner": runner,
            "config": config,
            "global_config": config,
            "env_dir": env_dir,
            "envdir": getattr(runner, "envdir", None) or (str(env_dir) if env_dir else None),
            "environment_dir": env_dir,
            "environment_directory": env_dir,
            "requested_python": getattr(session, "python", None),
            "python_request": getattr(session, "python", None),
            "python_spec": getattr(session, "python", None),
            "session_name": getattr(session, "name", None),
            "name": getattr(session, "name", None),
            "env": dict(os.environ),
            "environ": dict(os.environ),
            "environment": dict(os.environ),
            "os_environ": dict(os.environ),
        }
        declared = self.declared_inputs()
        if isinstance(declared, Mapping):
            values.update(declared)
            names = list(declared)
        else:
            names = list(declared)
        return {name: values.get(name) for name in names} if names else values

    def describe(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "kind": "host_diagnostic",
            "inputs": self.declared_inputs(),
            "command": list(self.command) if self.command is not None else None,
        }


def host_diagnostic(
    target: Callable[..., Any] | None = None,
    /,
    **options: Any,
) -> HostDiagnostic | Callable[[Callable[..., Any]], HostDiagnostic]:
    """Create a :class:`HostDiagnostic`, optionally as a decorator."""

    if target is not None and callable(target):
        return HostDiagnostic(target, **options)

    def decorate(func: Callable[..., Any]) -> HostDiagnostic:
        return HostDiagnostic(func, **options)

    return decorate


# Descriptive aliases for integrations and tests that name the concrete
# "action" concept rather than the host on which it runs.
Diagnostic = HostDiagnostic
DiagnosticAction = HostDiagnostic
DiagnosticReport = PhaseReport
HostDiagnosticAction = HostDiagnostic
HostAction = HostDiagnostic
HostCheck = HostDiagnostic
DiagnosticState = PhaseState
DiagnosticStatus = PhaseState
DiagnosticSettlement = PhaseState
HostDiagnosticState = PhaseState
HostDiagnosticReport = PhaseReport
HostDiagnosticSettlement = PhaseState
DiagnosticSummary = PhaseReport


def register_host_diagnostic(
    target: Callable[..., Any] | None = None, /, **options: Any
) -> HostDiagnostic | Callable[[Callable[..., Any]], HostDiagnostic]:
    """Create and return a host diagnostic (registration is session-specific)."""

    return host_diagnostic(target, **options)


register_diagnostic = register_host_diagnostic
diagnostic_action = host_diagnostic
host_action = host_diagnostic
host_check = host_diagnostic
diagnostic = host_diagnostic
