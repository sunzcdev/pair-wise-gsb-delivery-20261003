from unittest.mock import patch

from pyinfra import host
from pyinfra.api import Config, FactBase, QuoteString, State, StringCommand
from pyinfra.api.connect import connect_all
from pyinfra.api.exceptions import PyinfraError
from pyinfra.api.operation import add_op, operation
from pyinfra.api.operations import run_host_op, run_ops
from pyinfra.api.state import StateStage
from pyinfra.connectors.util import CommandOutput, OutputLine

from ..paramiko_util import PatchSSHTestCase
from ..util import make_inventory


class FileContent(FactBase[str]):
    def command(self, path: str) -> str:
        return f"cat {path}"

    def process(self, output: list[str]) -> str:
        return "\n".join(output)


@operation()
def standin_write(path: str, content: str):
    if host.get_fact(FileContent, path=path) != content:
        yield StringCommand("standin-write", QuoteString(path), QuoteString(content))


@operation()
def standin_write_pointed(content: str):
    target = host.get_fact(FileContent, path="/etc/standin-target")
    if host.get_fact(FileContent, path=target) != content:
        yield StringCommand("standin-write", QuoteString(target), QuoteString(content))


class StandInMachine:
    """
    A local stand-in for a remote host: serves facts from an in-memory
    filesystem and logs any write commands it receives.
    """

    def __init__(self):
        self.files = {
            "/one": "old",
            "/two": "old",
            "/three-a": "old",
            "/three-b": "old",
            "/etc/standin-target": "/three-a",
        }
        self.write_log: list[str] = []

    def run_shell_command(self, command, **kwargs):
        raw = command if isinstance(command, str) else command.get_raw_value()
        if raw.startswith("cat "):
            path = raw[len("cat ") :]
            if path not in self.files:
                return False, CommandOutput([OutputLine("stderr", f"No such file: {path}")])
            return True, CommandOutput([OutputLine("stdout", self.files[path])])
        if raw.startswith("standin-write "):
            _, path, content = raw.split(" ")
            self.files[path] = content
            self.write_log.append(raw)
            return True, CommandOutput([])
        raise AssertionError(f"Unexpected command: {raw}")


class TestValidatedPlanExecution(PatchSSHTestCase):
    def _make_state(self):
        inventory = make_inventory(hosts=("somehost",))
        state = State(inventory, Config())
        state.current_stage = StateStage.Prepare
        connect_all(state)
        return state, inventory.get_host("somehost")

    def _add_three_ops(self, state, **kwargs):
        add_op(state, standin_write, path="/one", content="new", **kwargs)
        add_op(state, standin_write, path="/two", content="new", **kwargs)
        add_op(state, standin_write_pointed, content="new", **kwargs)

    def test_validated_plan_execution(self):
        state, host = self._make_state()
        machine = StandInMachine()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as run_command:
            run_command.side_effect = machine.run_shell_command

            # Preview the three operations, recording what each will modify
            self._add_three_ops(state, _validate_plan=True)

            planned_targets = set()
            for plan in state.validated_op_plans[host].values():
                planned_targets.update(plan.targets)
            assert planned_targets == {
                "standin-write /one new",
                "standin-write /two new",
                "standin-write /three-a new",
            }

            # The world changes before execution: the second target reaches the
            # desired state on its own, the third operation reads a new target.
            machine.files["/two"] = "new"
            machine.files["/etc/standin-target"] = "/three-b"

            with self.assertLogs("pyinfra", level="INFO") as logs:
                with self.assertRaises(PyinfraError):
                    run_ops(state)

            # Only the first operation's write was executed
            assert machine.write_log == ["standin-write /one new"]

            log_output = "\n".join(logs.output)
            # The second operation's skip is visible, with the reason
            assert "Skipped" in log_output
            assert "already in the desired state" in log_output
            # The third operation's conflict is visible, with old/new difference
            assert "Approved plan is no longer valid" in log_output
            assert "standin-write /three-a new" in log_output
            assert "standin-write /three-b new" in log_output

            op_hashes = host.op_hash_order
            first_meta = state.get_op_data_for_host(host, op_hashes[0]).operation_meta
            second_meta = state.get_op_data_for_host(host, op_hashes[1]).operation_meta
            third_meta = state.get_op_data_for_host(host, op_hashes[2]).operation_meta
            assert first_meta.did_succeed()
            assert first_meta.did_change()
            assert second_meta.did_succeed()
            assert second_meta.did_not_change()
            assert third_meta.did_error()

            # Operations that already confirmed success are not replayed
            with self.assertLogs("pyinfra", level="INFO") as replay_logs:
                assert run_host_op(state, host, op_hashes[0]) is True
            assert "not replaying" in "\n".join(replay_logs.output)
            assert machine.write_log == ["standin-write /one new"]

    def test_unvalidated_operations_regenerate_commands_at_execution(self):
        state, host = self._make_state()
        machine = StandInMachine()

        with patch("pyinfra.connectors.ssh.SSHConnector.run_shell_command") as run_command:
            run_command.side_effect = machine.run_shell_command

            self._add_three_ops(state)
            assert not state.validated_op_plans[host]

            machine.files["/two"] = "new"
            machine.files["/etc/standin-target"] = "/three-b"

            run_ops(state)

            # Current semantics: commands are regenerated at execution time, so
            # the third operation silently writes to the new target.
            assert machine.write_log == [
                "standin-write /one new",
                "standin-write /three-b new",
            ]
