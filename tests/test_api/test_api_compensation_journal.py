from unittest import TestCase

from pyinfra.api import Config, FunctionCommand, State
from pyinfra.api.compensation import AppliedChange, CompensationStatus
from pyinfra.api.connect import connect_all
from pyinfra.api.exceptions import PyinfraError
from pyinfra.api.operation import add_op
from pyinfra.api.operations import run_ops
from pyinfra.api.state import StateStage
from pyinfra.operations import python, server

from ..util import make_inventory

# Fake hosts must not sleep between simulated commands.
NO_DELAY = {"fake_delay": 0, "fake_delay_jitter": 0}


class CompensationTestCase(TestCase):
    """
    Runs deploys against an ``@fake`` host which simulates a small remote state.

    ``self.remote`` maps a path to its content. ``external_change`` simulates
    somebody else changing a target part way through the run: it is applied just
    before the named command runs, so it lands *after* our own operation.
    """

    external_change: tuple[str, str, str] | None = None

    def setUp(self):
        self.remote = {}
        self._external_change_applied = False

    def make_state(self, external_change=None, **host_data):
        self.external_change = external_change
        data = {**NO_DELAY, **host_data}
        inventory = make_inventory(hosts=(("@fake", data),))
        state = State(inventory, Config())
        state.current_stage = StateStage.Prepare
        connect_all(state)
        host = inventory.get_host("@fake")

        run_shell_command = host.run_shell_command

        def run_with_third_party(command, *args, **kwargs):
            self._maybe_apply_external_change(str(command))
            return run_shell_command(command, *args, **kwargs)

        host.run_shell_command = run_with_third_party  # type: ignore[method-assign]
        return state, host

    def _maybe_apply_external_change(self, command):
        if self.external_change is None or self._external_change_applied:
            return
        trigger, path, content = self.external_change
        if trigger in command:
            self.remote[path] = content
            self._external_change_applied = True

    def apply_config(self, state, host, path, content, compensate=True, verify=True, **kwargs):
        """
        Add an operation which writes ``content`` to ``path`` on the fake host.
        """

        def write(state, host, path, content):
            status, _ = host.run_shell_command(f"write-config {path} {content}")
            if status:
                self.remote[path] = content
            return status

        def compensate_config():
            def revert(state, host):
                status, _ = host.run_shell_command(f"remove-config {path}")
                if status:
                    self.remote.pop(path, None)
                return status

            yield FunctionCommand(revert, (), {})

        op_kwargs = {"_compensation_scope": path}
        if compensate:
            op_kwargs["_compensate"] = compensate_config
        if verify:
            op_kwargs["_compensation_verify"] = lambda: self.remote.get(path) == content
        op_kwargs.update(kwargs)

        return add_op(
            state,
            python.call,
            function=write,
            path=path,
            content=content,
            **op_kwargs,
        )

    def fail_op(self, state, command="touch /etc/third.conf", **kwargs):
        return add_op(state, server.shell, commands=[command], **kwargs)

    def report_for(self, state, host):
        report = state.get_compensation_journal(host).last_report
        assert report is not None, "expected a rollback report"
        return report

    def reverted_commands(self, host):
        return [
            command
            for command in host.connector.executed_commands
            if command.startswith("remove-config ")
        ]


class TestConflictRollback(CompensationTestCase):
    """
    The acceptance scenario: two changes applied, somebody else moves the first
    target, then a third operation fails.
    """

    def _run(self, state):
        with self.assertRaises(PyinfraError):
            run_ops(state)

    def test_second_is_reverted_and_first_is_left_at_the_conflict(self):
        state, host = self.make_state(
            external_change=("touch /etc/third.conf", "/etc/first.conf", "changed-by-someone-else"),
            fake_responses={"touch /etc/third.conf": {"success": False}},
        )

        first = self.apply_config(state, host, "/etc/first.conf", "first-v1")
        second = self.apply_config(state, host, "/etc/second.conf", "second-v1")
        self.fail_op(state)

        self._run(state)

        report = self.report_for(state, host)

        # The second change is confirmed reverted before the conflict stops us.
        assert [outcome.change.scope for outcome in report.reverted] == ["/etc/second.conf"]
        assert self.reverted_commands(host) == ["remove-config /etc/second.conf"]
        assert "/etc/second.conf" not in self.remote

        # The first change is a conflict, named by its scope, and was left untouched.
        conflicts = report.conflicts
        assert len(conflicts) == 1
        assert conflicts[0].change.scope == "/etc/first.conf"
        assert "/etc/first.conf" in conflicts[0].label
        assert "remove-config /etc/first.conf" not in host.connector.executed_commands
        assert self.remote["/etc/first.conf"] == "changed-by-someone-else"

        # The report never claims the batch was restored.
        assert report.fully_reverted is False
        assert "NOT fully restored" in report.summary_line()

        # And the host is still marked failed.
        assert host in state.failed_hosts

        # Operation handles carry the same verdict, so a programmatic caller does not
        # have to parse logs to know where the rollback stopped.
        assert first[host].compensation_status is CompensationStatus.CONFLICT
        assert first[host].was_rolled_back is False
        assert second[host].compensation_status is CompensationStatus.REVERTED
        assert second[host].was_rolled_back is True

        # `state.get_compensation_report` is the same report.
        assert state.get_compensation_report(host) is report

    def test_report_lines_name_the_conflict_position(self):
        state, host = self.make_state(
            external_change=("touch /etc/third.conf", "/etc/first.conf", "changed-by-someone-else"),
            fake_responses={"touch /etc/third.conf": {"success": False}},
        )
        self.apply_config(state, host, "/etc/first.conf", "first-v1")
        self.apply_config(state, host, "/etc/second.conf", "second-v1")
        self.fail_op(state)

        self._run(state)

        text = "\n".join(self.report_for(state, host).format_lines())
        assert "reverted:" in text
        assert "conflict:" in text
        assert "/etc/first.conf" in text
        assert "batch was NOT fully restored" in text

    def test_changes_behind_the_conflict_are_skipped(self):
        state, host = self.make_state(
            external_change=(
                "touch /etc/fail.conf",
                "/etc/three.conf",
                "changed-by-someone-else",
            ),
            fake_responses={"touch /etc/fail.conf": {"success": False}},
        )
        self.apply_config(state, host, "/etc/one.conf", "one")
        self.apply_config(state, host, "/etc/two.conf", "two")
        self.apply_config(state, host, "/etc/three.conf", "three")
        self.fail_op(state, command="touch /etc/fail.conf")

        self._run(state)

        report = self.report_for(state, host)
        assert report.outcomes[0].status is CompensationStatus.CONFLICT
        assert report.outcomes[0].change.scope == "/etc/three.conf"
        assert all(outcome.status is CompensationStatus.SKIPPED for outcome in report.outcomes[1:])
        assert len(report.outcomes) == 3
        # Nothing behind the conflict was touched.
        assert self.reverted_commands(host) == []
        assert self.remote["/etc/one.conf"] == "one"
        assert self.remote["/etc/two.conf"] == "two"
        assert report.fully_reverted is False


class TestRollbackOrder(CompensationTestCase):
    def test_changes_are_reverted_newest_first(self):
        state, host = self.make_state(
            fake_responses={"touch /etc/fail.conf": {"success": False}},
        )
        self.apply_config(state, host, "/etc/one.conf", "one")
        self.apply_config(state, host, "/etc/two.conf", "two")
        self.apply_config(state, host, "/etc/three.conf", "three")
        self.fail_op(state, command="touch /etc/fail.conf")

        with self.assertRaises(PyinfraError):
            run_ops(state)

        assert self.reverted_commands(host) == [
            "remove-config /etc/three.conf",
            "remove-config /etc/two.conf",
            "remove-config /etc/one.conf",
        ]
        assert self.remote == {}

        report = self.report_for(state, host)
        assert report.fully_reverted is True
        assert "batch fully restored" in report.summary_line()

    def test_nothing_is_reverted_when_the_run_succeeds(self):
        state, host = self.make_state()
        self.apply_config(state, host, "/etc/one.conf", "one")
        self.apply_config(state, host, "/etc/two.conf", "two")

        run_ops(state)

        assert state.get_compensation_journal(host).last_report is None
        assert self.reverted_commands(host) == []


class TestNoopAndManualChanges(CompensationTestCase):
    def test_operations_that_applied_nothing_are_not_recorded(self):
        state, host = self.make_state(
            fake_responses={"touch /etc/fail.conf": {"success": False}},
        )
        # No commands at all, so no change happened and no rollback is needed.
        add_op(state, server.shell, commands=[])
        self.fail_op(state, command="touch /etc/fail.conf")

        with self.assertRaises(PyinfraError):
            run_ops(state)

        journal = state.get_compensation_journal(host)
        assert journal.applied == []
        assert journal.last_report is None

    def test_change_without_a_compensator_needs_a_human(self):
        state, host = self.make_state(
            fake_responses={"touch /etc/fail.conf": {"success": False}},
        )
        self.apply_config(state, host, "/etc/one.conf", "one")
        self.apply_config(state, host, "/etc/manual.conf", "manual", compensate=False)
        self.fail_op(state, command="touch /etc/fail.conf")

        with self.assertRaises(PyinfraError):
            run_ops(state)

        report = self.report_for(state, host)
        assert [change.label for change in report.manual_changes] == [
            "python.call (scope: /etc/manual.conf)"
        ]
        assert "no `_compensate` declared" in report.manual_changes[0].reason
        assert report.fully_reverted is False
        # The compensable change in front of it is still reverted.
        assert self.reverted_commands(host) == ["remove-config /etc/one.conf"]
        # And the manual change is deliberately left in place.
        assert self.remote["/etc/manual.conf"] == "manual"

    def test_failed_operation_state_is_reported_as_unknown(self):
        state, host = self.make_state(
            fake_responses={"touch /etc/half.conf": {"success": False}},
        )
        self.apply_config(state, host, "/etc/one.conf", "one")
        # Two commands, the second fails: the operation half applied.
        add_op(
            state,
            server.shell,
            commands=["write-config /etc/half.conf half", "touch /etc/half.conf"],
        )

        with self.assertRaises(PyinfraError):
            run_ops(state)

        report = self.report_for(state, host)
        assert len(report.unknown_changes) == 1
        assert "failed after 1 command(s)" in report.unknown_changes[0].reason
        assert report.fully_reverted is False
        # Earlier successful changes are still rolled back.
        assert self.reverted_commands(host) == ["remove-config /etc/one.conf"]


class TestVerifierOutcomes(CompensationTestCase):
    def test_verifier_exception_is_treated_as_a_conflict(self):
        state, host = self.make_state(
            fake_responses={"touch /etc/fail.conf": {"success": False}},
        )

        def broken_verifier():
            raise RuntimeError("cannot read the target")

        self.apply_config(
            state,
            host,
            "/etc/one.conf",
            "one",
            _compensation_verify=broken_verifier,
        )
        self.fail_op(state, command="touch /etc/fail.conf")

        with self.assertRaises(PyinfraError):
            run_ops(state)

        report = self.report_for(state, host)
        assert report.outcomes[0].status is CompensationStatus.CONFLICT
        assert "verifier raised" in report.outcomes[0].detail
        assert self.reverted_commands(host) == []
        assert report.fully_reverted is False

    def test_missing_verifier_cannot_claim_a_confirmed_revert(self):
        state, host = self.make_state(
            fake_responses={"touch /etc/fail.conf": {"success": False}},
        )
        self.apply_config(state, host, "/etc/one.conf", "one", verify=False)
        self.fail_op(state, command="touch /etc/fail.conf")

        with self.assertRaises(PyinfraError):
            run_ops(state)

        report = self.report_for(state, host)
        assert report.outcomes[0].status is CompensationStatus.REVERTED_UNCONFIRMED
        assert report.fully_reverted is False
        assert self.reverted_commands(host) == ["remove-config /etc/one.conf"]

        # The commands ran, but nothing confirmed the target changed back.
        op_meta = state.ops[host][state.get_op_order()[0]].operation_meta
        assert op_meta.compensation_status is CompensationStatus.REVERTED_UNCONFIRMED
        assert op_meta.was_rolled_back is False

    def test_failing_compensation_command_is_reported(self):
        state, host = self.make_state(
            fake_responses={
                "touch /etc/fail.conf": {"success": False},
                "remove-config /etc/one.conf": {"success": False},
            },
        )
        self.apply_config(state, host, "/etc/one.conf", "one")
        self.fail_op(state, command="touch /etc/fail.conf")

        with self.assertRaises(PyinfraError):
            run_ops(state)

        report = self.report_for(state, host)
        assert report.outcomes[0].status is CompensationStatus.FAILED
        assert "compensation command failed" in report.outcomes[0].detail
        assert report.fully_reverted is False


class TestIgnoredErrors(CompensationTestCase):
    def test_ignored_error_does_not_trigger_a_rollback(self):
        state, host = self.make_state(
            fake_responses={"touch /etc/fail.conf": {"success": False}},
        )
        self.apply_config(state, host, "/etc/one.conf", "one")
        self.fail_op(state, command="touch /etc/fail.conf", _ignore_errors=True)

        run_ops(state)

        assert state.get_compensation_journal(host).last_report is None
        assert self.reverted_commands(host) == []


class TestExecutionStrategies(CompensationTestCase):
    def _assert_rollback_happened(self, state, host):
        assert self.reverted_commands(host) == ["remove-config /etc/one.conf"]
        report = self.report_for(state, host)
        assert report.fully_reverted is True

    def test_serial_run_rolls_back(self):
        state, host = self.make_state(
            fake_responses={"touch /etc/fail.conf": {"success": False}},
        )
        self.apply_config(state, host, "/etc/one.conf", "one")
        self.fail_op(state, command="touch /etc/fail.conf")

        with self.assertRaises(PyinfraError):
            run_ops(state, serial=True)

        self._assert_rollback_happened(state, host)

    def test_no_wait_run_rolls_back(self):
        state, host = self.make_state(
            fake_responses={"touch /etc/fail.conf": {"success": False}},
        )
        self.apply_config(state, host, "/etc/one.conf", "one")
        self.fail_op(state, command="touch /etc/fail.conf")

        with self.assertRaises(PyinfraError):
            run_ops(state, no_wait=True)

        self._assert_rollback_happened(state, host)


class TestNestedAndRetry(CompensationTestCase):
    def test_container_operation_is_not_recorded_twice(self):
        state, host = self.make_state(
            fake_responses={"touch /etc/fail.conf": {"success": False}},
        )

        def write(state, host):
            status, _ = host.run_shell_command("write-config /etc/one.conf one")
            if status:
                self.remote["/etc/one.conf"] = "one"
            return status

        def compensate_one():
            def revert(state, host):
                status, _ = host.run_shell_command("remove-config /etc/one.conf")
                if status:
                    self.remote.pop("/etc/one.conf", None)
                return status

            yield FunctionCommand(revert, (), {})

        # `python.call` wraps the work; only the compensable wrapper owns it.
        add_op(
            state,
            python.call,
            function=write,
            _compensate=compensate_one,
            _compensation_verify=lambda: self.remote.get("/etc/one.conf") == "one",
            _compensation_scope="/etc/one.conf",
        )
        self.fail_op(state, command="touch /etc/fail.conf")

        with self.assertRaises(PyinfraError):
            run_ops(state)

        report = self.report_for(state, host)
        assert [outcome.change.scope for outcome in report.outcomes] == ["/etc/one.conf"]
        assert report.manual_changes == []
        assert self.reverted_commands(host) == ["remove-config /etc/one.conf"]
        assert report.fully_reverted is True

    def test_retried_operation_records_the_attempt_that_got_furthest(self):
        state, host = self.make_state(
            fake_responses={"touch /etc/fail.conf": {"success": False}},
        )

        calls = {"count": 0}
        run_shell_command = host.run_shell_command

        def flaky(command, *args, **kwargs):
            # First attempt writes half the change then fails; the retry works.
            if str(command) == "write-config /etc/one.conf one":
                calls["count"] += 1
                if calls["count"] == 1:
                    self.remote["/etc/one.conf"] = "partial"
                    return False, None
            return run_shell_command(command, *args, **kwargs)

        host.run_shell_command = flaky  # type: ignore[method-assign]

        def write(state, host):
            status, _ = host.run_shell_command("write-config /etc/one.conf one")
            if status:
                self.remote["/etc/one.conf"] = "one"
            return status

        add_op(
            state,
            python.call,
            function=write,
            _retries=1,
            _retry_delay=0,
            _compensate=lambda: ["remove-config /etc/one.conf"],
            _compensation_verify=lambda: self.remote.get("/etc/one.conf") == "one",
            _compensation_scope="/etc/one.conf",
        )
        self.fail_op(state, command="touch /etc/fail.conf")

        with self.assertRaises(PyinfraError):
            run_ops(state)

        report = self.report_for(state, host)
        assert report.unknown_changes == []
        assert self.reverted_commands(host) == ["remove-config /etc/one.conf"]


class TestAcrossRuns(CompensationTestCase):
    """
    A batch only ever rolls back its own changes: what an earlier successful run
    applied is never a candidate for this run's rollback.
    """

    def test_successful_run_leaves_no_pending_records(self):
        state, host = self.make_state()
        self.apply_config(state, host, "/etc/one.conf", "one")

        run_ops(state)

        journal = state.get_compensation_journal(host)
        assert journal.records == []
        assert journal.last_report is None
        assert self.remote == {"/etc/one.conf": "one"}

    def test_only_changes_recorded_after_start_run_are_rolled_back(self):
        state, host = self.make_state()
        journal = state.get_compensation_journal(host)

        def applied(op_hash, scope):
            return AppliedChange(
                sequence=journal.next_sequence(),
                op_hash=op_hash,
                names=("earlier.operation",),
                scope=scope,
                command_count=1,
                compensate=lambda: ["remove-config " + scope],
                verify=None,
                connector_arguments={},
            )

        # An earlier batch which was left in place (eg it succeeded).
        journal.records.append(applied("earlier", "/etc/earlier.conf"))

        # This batch only owns what it records itself.
        journal.start_run()
        journal.records.append(applied("current", "/etc/current.conf"))
        assert [record.op_hash for record in journal.iter_current_run()] == ["current"]

        journal.end_run()
        assert [record.op_hash for record in journal.records] == ["earlier"]
