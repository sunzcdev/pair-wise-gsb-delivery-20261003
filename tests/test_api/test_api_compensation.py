"""
Tests for saga-style compensation (rollback) in ``run_ops``.

The headline scenario mirrors a maintenance window with three changes: the first two
operations succeed, another party then modifies the first operation's target, and the
third operation fails. Rollback must undo the second change, stop at the conflict on the
first, and the report must not claim the whole batch was restored.
"""

from unittest.mock import patch

from pyinfra.api import (
    CompensationError,
    CompensationStatus,
    Config,
    State,
)
from pyinfra.api.compensation import run_compensation
from pyinfra.api.connect import connect_all
from pyinfra.api.exceptions import PyinfraError
from pyinfra.api.operation import add_op
from pyinfra.api.operations import _run_single_op, run_ops
from pyinfra.api.state import StateStage
from pyinfra.operations import server

from ..paramiko_util import FakeBuffer, FakeChannel, PatchSSHTestCase
from ..util import make_inventory


def _ok():
    return (True, FakeBuffer("", FakeChannel(0)))


def _fail():
    return (False, FakeBuffer("", FakeChannel(1)))


class TestCompensation(PatchSSHTestCase):
    def _make_state(self):
        inventory = make_inventory(hosts=("somehost",))
        state = State(inventory, Config())
        state.current_stage = StateStage.Prepare
        connect_all(state)
        return state, inventory.get_host("somehost")

    # The scenario from the maintenance-window brief.
    def test_reverse_rollback_stops_at_external_conflict(self):
        state, somehost = self._make_state()

        # Shared "remote" state the verifiers inspect; another party flips the first
        # target after the forward operations complete.
        targets = {"first": "ours", "second": "ours"}

        def undo_first():
            yield "rm -f /etc/first.conf"

        def undo_second():
            yield "rm -f /etc/second.conf"

        def verify_first():
            return targets["first"] == "ours"

        def verify_second():
            return targets["second"] == "ours"

        add_op(
            state,
            server.shell,
            "touch /etc/first.conf",
            name="First change",
            _compensate=undo_first,
            _compensation_verify=verify_first,
            _compensation_scope="/etc/first.conf",
        )
        add_op(
            state,
            server.shell,
            "touch /etc/second.conf",
            name="Second change",
            _compensate=undo_second,
            _compensation_verify=verify_second,
            _compensation_scope="/etc/second.conf",
        )
        add_op(state, server.shell, "touch /etc/third.conf", name="Third change")

        executed: list[str] = []

        def side_effect(command, *args, **kwargs):
            rendered = str(command)
            executed.append(rendered)

            # Forward commands: first two succeed, the third fails.
            if rendered == "touch /etc/third.conf":
                # Simulate the other party updating the *first* target before rollback.
                targets["first"] = "theirs"
                return _fail()
            return _ok()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as fake_run_command:
            fake_run_command.side_effect = side_effect

            with self.assertRaises(CompensationError) as caught:
                run_ops(state)

        report = caught.exception.report

        # The second change was verified clean and reverted, in reverse order.
        assert len(report.reverted) == 1
        reverted = report.reverted[0]
        assert reverted.status is CompensationStatus.REVERTED
        assert "Second change" in reverted.op_names
        assert reverted.commands_run == ["rm -f /etc/second.conf"]

        # Rollback stopped at the first change because the target drifted.
        assert report.stopped_at is not None
        assert "First change" in report.stopped_at.op_names
        assert report.stopped_at.status is CompensationStatus.CONFLICT
        assert report.conflicts == [report.stopped_at]
        assert report.stopped_at.scope == "/etc/first.conf"

        # The first op's compensator must never have run.
        assert "rm -f /etc/first.conf" not in executed
        assert "rm -f /etc/second.conf" in executed

        # The report must not claim a full restore.
        assert report.fully_reverted is False
        assert report.has_conflict is True

        # The host is still marked failed by the ordinary failure flow.
        assert somehost in state.failed_hosts

    def test_full_rollback_when_all_targets_unchanged(self):
        state, somehost = self._make_state()

        def undo_first():
            yield "rm -f /etc/first.conf"

        def undo_second():
            yield "rm -f /etc/second.conf"

        add_op(
            state,
            server.shell,
            "touch /etc/first.conf",
            name="First change",
            _compensate=undo_first,
            _compensation_verify=lambda: True,
        )
        add_op(
            state,
            server.shell,
            "touch /etc/second.conf",
            name="Second change",
            _compensate=undo_second,
            _compensation_verify=lambda: True,
        )
        add_op(state, server.shell, "false", name="Failing change")

        executed: list[str] = []

        def side_effect(command, *args, **kwargs):
            rendered = str(command)
            executed.append(rendered)
            return _fail() if rendered == "false" else _ok()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as fake_run_command:
            fake_run_command.side_effect = side_effect

            with self.assertRaises(PyinfraError):
                run_ops(state)

        # Reverse order: second then first.
        assert executed[-2:] == ["rm -f /etc/second.conf", "rm -f /etc/first.conf"]
        records = state.compensation_records[somehost]
        assert [r.status for r in records] == [
            CompensationStatus.REVERTED,
            CompensationStatus.REVERTED,
        ]

    def test_verifier_exception_is_a_conflict_and_stops_rollback(self):
        state, somehost = self._make_state()

        def undo_first():
            yield "rm -f /etc/first.conf"

        def broken_verifier():
            raise RuntimeError("fact collection blew up")

        add_op(
            state,
            server.shell,
            "touch /etc/first.conf",
            name="First change",
            _compensate=undo_first,
            _compensation_verify=broken_verifier,
            _compensation_scope="/etc/first.conf",
        )
        add_op(state, server.shell, "false", name="Failing change")

        executed: list[str] = []

        def side_effect(command, *args, **kwargs):
            executed.append(str(command))
            return _fail() if str(command) == "false" else _ok()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as fake_run_command:
            fake_run_command.side_effect = side_effect

            with self.assertRaises(CompensationError) as caught:
                run_ops(state)

        record = caught.exception.report.stopped_at
        assert record is not None
        assert record.status is CompensationStatus.CONFLICT_ERROR
        assert "RuntimeError" in (record.detail or "")
        # No compensation command ran against an unverified target.
        assert "rm -f /etc/first.conf" not in executed
        assert somehost in state.failed_hosts

    def test_failed_compensation_command_stops_the_rollback(self):
        state, somehost = self._make_state()

        def undo_first():
            yield "rm -f /etc/first.conf"

        add_op(
            state,
            server.shell,
            "touch /etc/first.conf",
            name="First change",
            _compensate=undo_first,
            _compensation_verify=lambda: True,
        )
        add_op(state, server.shell, "false", name="Failing change")

        def side_effect(command, *args, **kwargs):
            rendered = str(command)
            if rendered == "false":
                return _fail()
            if rendered == "rm -f /etc/first.conf":
                return _fail()
            return _ok()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as fake_run_command:
            fake_run_command.side_effect = side_effect

            with self.assertRaises(CompensationError) as caught:
                run_ops(state)

        report = caught.exception.report
        assert report.failed
        assert report.failed[0].status is CompensationStatus.FAILED
        assert report.fully_reverted is False

    def test_uncompensated_successful_change_blocks_full_restore_claim(self):
        state, somehost = self._make_state()

        # The first change has *no* compensator - a human must judge it. The second is
        # compensatable, so rollback produces a report that also lists the manual item.
        add_op(state, server.shell, "touch /etc/manual.conf", name="Manual change")
        add_op(
            state,
            server.shell,
            "touch /etc/second.conf",
            name="Second change",
            _compensate=lambda: (yield "rm -f /etc/second.conf"),
            _compensation_verify=lambda: True,
        )
        add_op(state, server.shell, "false", name="Failing change")

        def side_effect(command, *args, **kwargs):
            return _fail() if str(command) == "false" else _ok()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as fake_run_command:
            fake_run_command.side_effect = side_effect

            with self.assertRaises(CompensationError) as caught:
                run_ops(state)

        report = caught.exception.report
        # The compensatable change is undone, but the manual one blocks a full restore.
        assert len(report.reverted) == 1
        assert len(report.uncompensated) == 1
        assert "Manual change" in report.uncompensated[0][1]
        assert report.fully_reverted is False
        # The uncompensated change is also tracked directly on the state.
        assert state.uncompensated_changes[somehost]
        assert somehost in state.failed_hosts

    def test_only_uncompensated_changes_follow_ordinary_failure_flow(self):
        state, somehost = self._make_state()

        # A successful change with no compensator, then a failure: nothing to roll back,
        # so the ordinary PyinfraError surfaces rather than a CompensationError.
        add_op(state, server.shell, "touch /etc/manual.conf", name="Manual change")
        add_op(state, server.shell, "false", name="Failing change")

        def side_effect(command, *args, **kwargs):
            return _fail() if str(command) == "false" else _ok()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as fake_run_command:
            fake_run_command.side_effect = side_effect

            with self.assertRaises(PyinfraError) as caught:
                run_ops(state)

        assert not isinstance(caught.exception, CompensationError)
        assert state.compensation_records[somehost] == []
        assert len(state.uncompensated_changes[somehost]) == 1
        assert somehost in state.failed_hosts

    def test_noop_operations_are_not_recorded(self):
        state, somehost = self._make_state()

        # An operation whose condition is False yields no commands, so it succeeds but
        # makes no changes and must not be recorded for compensation.
        add_op(
            state,
            server.shell,
            "true",
            name="Skipped change",
            _if=lambda: False,
            _compensate=lambda: (yield "rm -f /etc/never"),
        )
        add_op(state, server.shell, "false", name="Failing change")

        def side_effect(command, *args, **kwargs):
            return _fail() if str(command) == "false" else _ok()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as fake_run_command:
            fake_run_command.side_effect = side_effect

            # No compensatable changes recorded, so rollback has nothing to do and the
            # ordinary PyinfraError surfaces rather than a CompensationError.
            with self.assertRaises(PyinfraError) as caught:
                run_ops(state)

        assert not isinstance(caught.exception, CompensationError)
        assert state.compensation_records[somehost] == []
        assert state.uncompensated_changes[somehost] == []

    def test_run_compensation_replays_records_in_reverse_order(self):
        state, somehost = self._make_state()

        order: list[str] = []

        def make_compensator(label):
            def compensator():
                order.append(label)
                yield f"echo undo-{label}"

            return compensator

        add_op(
            state,
            server.shell,
            "echo one",
            name="One",
            _compensate=make_compensator("one"),
            _compensation_verify=lambda: True,
        )
        add_op(
            state,
            server.shell,
            "echo two",
            name="Two",
            _compensate=make_compensator("two"),
            _compensation_verify=lambda: True,
        )

        def side_effect(command, *args, **kwargs):
            return _ok()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as fake_run_command:
            fake_run_command.side_effect = side_effect
            # Drive the forward ops only, then invoke the rollback directly.
            for op_hash in state.get_op_order():
                _run_single_op(state, op_hash)

            report = run_compensation(state, somehost)

        assert order == ["two", "one"]
        assert report is not None
        assert report.fully_reverted is True
        assert len(report.reverted) == 2

    def _conflict_scenario(self):
        state, somehost = self._make_state()
        targets = {"first": "ours"}

        add_op(
            state,
            server.shell,
            "touch /etc/first.conf",
            name="First change",
            _compensate=lambda: (yield "rm -f /etc/first.conf"),
            _compensation_verify=lambda: targets["first"] == "ours",
            _compensation_scope="/etc/first.conf",
        )
        add_op(state, server.shell, "false", name="Failing change")

        def side_effect(command, *args, **kwargs):
            rendered = str(command)
            if rendered == "false":
                # Another party changes the first target before rollback.
                targets["first"] = "theirs"
                return _fail()
            return _ok()

        return state, somehost, side_effect

    def test_conflict_propagates_in_serial_strategy(self):
        state, somehost, side_effect = self._conflict_scenario()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as fake_run_command:
            fake_run_command.side_effect = side_effect
            with self.assertRaises(CompensationError) as caught:
                run_ops(state, serial=True)

        assert caught.exception.report.has_conflict is True
        assert caught.exception.report.fully_reverted is False
        assert somehost in state.failed_hosts

    def test_conflict_propagates_in_no_wait_strategy(self):
        state, somehost, side_effect = self._conflict_scenario()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as fake_run_command:
            fake_run_command.side_effect = side_effect
            with self.assertRaises(CompensationError) as caught:
                run_ops(state, no_wait=True)

        assert caught.exception.report.has_conflict is True
        assert caught.exception.report.fully_reverted is False
        assert somehost in state.failed_hosts

    def test_clean_rollback_in_serial_strategy_marks_host_failed(self):
        state, somehost = self._make_state()

        add_op(
            state,
            server.shell,
            "touch /etc/first.conf",
            name="First change",
            _compensate=lambda: (yield "rm -f /etc/first.conf"),
            _compensation_verify=lambda: True,
        )
        add_op(state, server.shell, "false", name="Failing change")

        executed: list[str] = []

        def side_effect(command, *args, **kwargs):
            rendered = str(command)
            executed.append(rendered)
            return _fail() if rendered == "false" else _ok()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as fake_run_command:
            fake_run_command.side_effect = side_effect
            # Fully reverted, so the ordinary failure error surfaces instead.
            with self.assertRaises(PyinfraError) as caught:
                run_ops(state, serial=True)

        assert not isinstance(caught.exception, CompensationError)
        assert "rm -f /etc/first.conf" in executed
        assert somehost in state.failed_hosts
