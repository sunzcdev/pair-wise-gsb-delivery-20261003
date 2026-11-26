"""
Tests for dry-run planning with unknown fact values.

A probe that cannot conclude (eg permission denied) must be reported
separately from a probe that confirmed a resource's absence: operations
depending on the unknown value are marked unplanned, while other hosts and
operations that do not depend on it still produce a plan. Once the probe can
read again, the previously unknown value must not linger on as a confirmed
value.
"""

from unittest import TestCase
from unittest.mock import patch

from pyinfra import host
from pyinfra.api import Config, State, operation
from pyinfra.api.connect import connect_all
from pyinfra.api.facts import _MISSING_COMMAND_MARKER, FactBase
from pyinfra.api.operation import add_op
from pyinfra.api.state import StateStage
from pyinfra.connectors.local import LocalConnector
from pyinfra.connectors.util import CommandOutput, OutputLine
from pyinfra_cli.prints import build_fact_unknowns_json, build_plan_json

from ..util import make_inventory


class ProbeResource(FactBase):
    """Stand-in probe: reads whether a named resource exists on the target."""

    def command(self, path: str) -> str:
        return f"probe-resource {path}"

    def process(self, output: list[str]):
        return {"status": output[0]}


class UnrelatedProbe(FactBase):
    """Stand-in probe that does not depend on ProbeResource at all."""

    def command(self) -> str:
        return "probe-unrelated"

    def process(self, output: list[str]):
        return output[0]


class MissingBinaryProbe(FactBase):
    """Stand-in probe whose required binary is absent during the plan phase."""

    def command(self) -> str:
        return "probe-missing-binary"

    def requires_command(self) -> str:
        return "definitely-missing-binary"


class PreconditionProbe(FactBase):
    """Stand-in probe whose precondition is not satisfied during the plan phase."""

    def check_preconditions(self, state, host) -> str:
        return "kernel module not loaded"

    def command(self) -> str:
        return "probe-precondition"


@operation()
def ensure_resource(path: str):
    current = host.get_fact(ProbeResource, path=path)
    if current is None:
        yield f"create-resource {path}"
    # Resource exists -> no commands (keep as-is)


@operation()
def ensure_unrelated():
    if host.get_fact(UnrelatedProbe) != "ready":
        yield "fix-unrelated"


@operation()
def ensure_with_missing_binary():
    if not host.get_fact(MissingBinaryProbe):
        yield "fix-missing-binary"


@operation()
def ensure_with_precondition():
    if not host.get_fact(PreconditionProbe):
        yield "fix-precondition"


def _make_state() -> State:
    inventory = make_inventory(hosts=("gone", "denied", "present"))
    # Use the local connector as a stand-in target for all three hosts
    for inventory_host in inventory:
        inventory_host.connector_cls = LocalConnector
    state = State(inventory, Config())
    state.current_stage = StateStage.Prepare
    return state


def _denied_run_command(self, command, **kwargs):
    """First run: the denied host refuses to read the resource."""
    cmd = str(command)
    if cmd.startswith("probe-resource"):
        if self.host.name == "gone":
            # Probe concludes: the resource does not exist
            return True, CommandOutput([])
        if self.host.name == "denied":
            # Probe cannot conclude: permission denied
            return False, CommandOutput(
                [OutputLine("stderr", "probe-resource: permission denied")]
            )
        # Probe concludes: the resource exists
        return True, CommandOutput([OutputLine("stdout", "present")])
    if cmd.startswith("probe-unrelated"):
        return True, CommandOutput([OutputLine("stdout", "not-ready")])
    raise AssertionError(f"unexpected command: {cmd}")


def _recovered_run_command(self, command, **kwargs):
    """Second run: the denied host can read again - the resource exists."""
    cmd = str(command)
    if cmd.startswith("probe-resource"):
        if self.host.name == "gone":
            return True, CommandOutput([])
        return True, CommandOutput([OutputLine("stdout", "present")])
    if cmd.startswith("probe-unrelated"):
        return True, CommandOutput([OutputLine("stdout", "not-ready")])
    raise AssertionError(f"unexpected command: {cmd}")


def _get_plan_op(plan: list[dict], name: str) -> dict:
    matches = [op for op in plan if name in op["name"]]
    assert len(matches) == 1, f"expected exactly one plan entry for {name}"
    return matches[0]


class TestUnknownFactPlan(TestCase):
    def test_plan_distinguishes_create_unplanned_and_keep(self):
        state = _make_state()
        connect_all(state)

        with patch.object(LocalConnector, "run_shell_command", _denied_run_command):
            add_op(state, ensure_resource, "/etc/motd")
            add_op(state, ensure_unrelated)

        plan = build_plan_json(state)

        resource_op = _get_plan_op(plan, "ensure_resource")
        # Probe confirmed absence -> create
        assert resource_op["hosts_with_change"] == ["gone"]
        # Probe could not conclude -> unplanned (to be verified)
        assert resource_op["hosts_unplanned"] == ["denied"]
        assert "permission denied" in resource_op["unplanned_reasons"]["denied"][0]
        # Probe confirmed existence -> keep (neither change nor unplanned)
        assert "present" not in resource_op["hosts_with_change"]
        assert "present" not in resource_op["hosts_unplanned"]

        # The denied host is not failed, and its unrelated operation - which
        # does not depend on the unknown value - still produces a plan.
        assert not state.failed_hosts
        unrelated_op = _get_plan_op(plan, "ensure_unrelated")
        assert unrelated_op["hosts_with_change"] == ["denied", "gone", "present"]
        assert unrelated_op["hosts_unplanned"] == []

    def test_unknown_reason_is_retained_for_the_plan_report(self):
        state = _make_state()
        connect_all(state)

        with patch.object(LocalConnector, "run_shell_command", _denied_run_command):
            add_op(state, ensure_resource, "/etc/motd")

        denied_host = state.inventory.get_host("denied")
        unknowns = state.get_fact_unknowns(denied_host)
        assert len(unknowns) == 1
        fact_key, reason = next(iter(unknowns.items()))
        assert "ProbeResource" in fact_key
        assert "/etc/motd" in fact_key
        assert "permission denied" in reason

        # Serialised for callers of the JSON plan report
        report = build_fact_unknowns_json(state)
        assert report == {"denied": {fact_key: reason}}

        # Hosts whose probes concluded have no unknown records
        assert state.get_fact_unknowns(state.inventory.get_host("gone")) == {}
        assert state.get_fact_unknowns(state.inventory.get_host("present")) == {}

    def test_recovered_probe_replaces_unknown_with_confirmed_value(self):
        state = _make_state()
        connect_all(state)
        denied_host = state.inventory.get_host("denied")

        with patch.object(LocalConnector, "run_shell_command", _denied_run_command):
            add_op(state, ensure_resource, "/etc/motd")
        assert state.get_fact_unknowns(denied_host) != {}

        # The host can read again: the fact is re-fetched, the real value is
        # returned and the unknown record is cleared - the previously unknown
        # value must not masquerade as a confirmed absence.
        with patch.object(LocalConnector, "run_shell_command", _recovered_run_command):
            value = denied_host.get_fact(ProbeResource, path="/etc/motd")
        assert value == {"status": "present"}
        assert state.get_fact_unknowns(denied_host) == {}

        # Re-running the whole deploy now plans the host as keep, not create:
        # the old unknown did not stick as a confirmed-absent default.
        second_state = _make_state()
        connect_all(second_state)
        with patch.object(LocalConnector, "run_shell_command", _recovered_run_command):
            add_op(second_state, ensure_resource, "/etc/motd")

        plan = build_plan_json(second_state)
        resource_op = _get_plan_op(plan, "ensure_resource")
        assert resource_op["hosts_with_change"] == ["gone"]
        assert resource_op["hosts_unplanned"] == []
        assert resource_op["unplanned_reasons"] == {}


class TestCompatDefaultUnknownReasons(TestCase):
    """
    Facts that only lack preconditions during the plan phase keep their compat
    default, but the unknown reason is retained and dependent operations are
    marked unplanned.
    """

    def _run_case(self, op_func, expected_reason):
        state = _make_state()
        connect_all(state)

        def fake_run_command(self, command, **kwargs):
            cmd = str(command)
            if "definitely-missing-binary" in cmd:
                # The if/then/else guard emits the sentinel when the binary is absent
                return True, CommandOutput([OutputLine("stdout", _MISSING_COMMAND_MARKER)])
            raise AssertionError(f"unexpected command: {cmd}")

        with patch.object(LocalConnector, "run_shell_command", fake_run_command):
            add_op(state, op_func)

        plan = build_plan_json(state)
        plan_op = _get_plan_op(plan, op_func._inner.__name__)
        assert plan_op["hosts_unplanned"] == ["denied", "gone", "present"]
        for reasons in plan_op["unplanned_reasons"].values():
            assert expected_reason in reasons[0]
        # The compat default still drove change detection, so no host failed
        assert not state.failed_hosts

    def test_missing_command_retains_unknown_reason(self):
        self._run_case(ensure_with_missing_binary, "command not found")

    def test_precondition_retains_unknown_reason(self):
        self._run_case(ensure_with_precondition, "precondition not satisfied")
