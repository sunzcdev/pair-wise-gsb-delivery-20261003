from unittest.mock import patch

from pyinfra.api import Config, State
from pyinfra.api.connect import connect_all
from pyinfra.api.exceptions import ArgumentTypeError, PyinfraError
from pyinfra.api.operation import add_op
from pyinfra.api.operations import run_ops
from pyinfra.api.rollout import UNGROUPED_KEY, Rollout
from pyinfra.api.state import StateStage
from pyinfra.context import ctx_host, ctx_state
from pyinfra.operations import server

from ..paramiko_util import FakeBuffer, FakeChannel, PatchSSHTestCase
from ..util import make_inventory

# Patched with autospec=True so the side effect receives the host as its first
# positional argument, which keeps per-host outcomes correct under gevent.
HOST_RUN_SHELL_COMMAND = "pyinfra.api.host.Host.run_shell_command"


def _failing_hosts(failing_hosts):
    """Build a run_shell_command side effect that fails only the named hosts."""

    def effect(host, *args, **kwargs):
        if host.name in failing_hosts:
            return (False, FakeBuffer("", FakeChannel(1)))
        return (True, FakeBuffer("ok", FakeChannel(0)))

    return effect


class TestRolloutPlanning:
    def test_hosts_split_into_group_batches_in_inventory_order(self):
        inventory = make_inventory(
            hosts=("a1", "a2", "a3", "a4", "b1", "b2", "loner"),
            service_a=(["a1", "a2", "a3", "a4"], {}),
            service_b=(["b1", "b2"], {}),
        )
        rollout = Rollout(inventory, batch_size=2, error_budget={"service_a": 1})

        service_a = rollout.groups["service_a"]
        assert [[h.name for h in batch] for batch in service_a.batches] == [
            ["a1", "a2"],
            ["a3", "a4"],
        ]
        assert service_a.budget == 1
        assert rollout.groups["service_b"].budget is None
        # The ungrouped host gets a synthetic, unlimited-budget group.
        ungrouped = rollout.groups[UNGROUPED_KEY]
        assert ungrouped.budget is None
        assert [h.name for batch in ungrouped.batches for h in batch] == ["loner"]

    def test_budget_pause_blocks_later_batches_but_keeps_admitted_state(self):
        inventory = make_inventory(
            hosts=("a1", "a2", "a3", "a4"),
            service_a=(["a1", "a2", "a3", "a4"], {}),
        )
        rollout = Rollout(inventory, batch_size=2, error_budget={"service_a": 1})
        group = rollout.groups["service_a"]

        a1, a2, a3, a4 = (inventory.get_host(name) for name in ("a1", "a2", "a3", "a4"))

        group.next_batch = 1
        assert group.record_errors({a1}) == 1
        assert group.paused is True
        assert group.error_count == 1

        # The admitted batch's hosts are reported as admitted; later batches are not.
        assert group.is_host_admitted(a1)
        assert group.is_host_admitted(a2)
        assert group.is_host_unstarted(a3)
        assert group.is_host_unstarted(a4)
        assert [[h.name for h in batch] for batch in group.unstarted_batches()] == [
            ["a3", "a4"],
        ]

    def test_failures_are_counted_once_per_host(self):
        inventory = make_inventory(
            hosts=("a1", "a2", "a3", "a4", "b1"),
            service_a=(["a1", "a2", "a3", "a4"], {}),
            service_b=(["b1"], {}),
        )
        rollout = Rollout(
            inventory,
            batch_size=2,
            error_budget={"service_a": 2, "service_b": 2},
        )
        group_a = rollout.groups["service_a"]
        group_b = rollout.groups["service_b"]
        a1 = inventory.get_host("a1")
        b1 = inventory.get_host("b1")

        assert group_a.record_errors({a1}) == 1
        # Same host reported again (eg on the next operation) is not double counted.
        assert group_a.record_errors({a1}) == 0
        assert group_a.paused is False
        assert group_a.error_count == 1

        # A host owned by another group does not consume this group's budget.
        assert group_a.record_errors({b1}) == 0
        assert group_b.record_errors({b1}) == 1
        assert group_a.error_count == 1

    def test_zero_budget_pauses_on_first_failure(self):
        inventory = make_inventory(
            hosts=("a1", "a2", "a3"),
            service_a=(["a1", "a2", "a3"], {}),
        )
        rollout = Rollout(inventory, batch_size=1, error_budget={"service_a": 0})
        group = rollout.groups["service_a"]
        a1 = inventory.get_host("a1")

        assert group.record_errors({a1}) == 1
        assert group.paused is True

    def test_invalid_settings_are_rejected(self):
        inventory = make_inventory(
            hosts=("a1", "a2"),
            service_a=(["a1", "a2"], {}),
        )

        for bad_size in (0, -1, True):
            try:
                Rollout(inventory, batch_size=bad_size)  # type: ignore[arg-type]
            except PyinfraError:
                pass
            else:
                raise AssertionError(f"batch_size={bad_size!r} should be rejected")

        try:
            Rollout(inventory, batch_size=1, error_budget={"unknown_group": 1})
        except PyinfraError:
            pass
        else:
            raise AssertionError("unknown budget group should be rejected")

        try:
            Rollout(inventory, batch_size=1, error_budget={"service_a": -1})
        except PyinfraError:
            pass
        else:
            raise AssertionError("negative budget should be rejected")


class TestRolloutExecution(PatchSSHTestCase):
    def tearDown(self):
        # An operation whose global arguments fail type validation raises inside
        # ``ctx_host.use``; the gevent local then keeps pointing at the last host.
        # Reset both contexts so the leak cannot reach unrelated later tests.
        ctx_host.reset()
        ctx_state.reset()
        super().tearDown()

    def _make_state(self, hosts, **groups):
        inventory = make_inventory(hosts=hosts, **groups)
        state = State(inventory, Config())
        state.current_stage = StateStage.Prepare
        connect_all(state)
        return state, inventory

    def test_budget_pauses_group_while_other_group_finishes(self):
        state, inventory = self._make_state(
            ("a1", "a2", "a3", "a4", "b1", "b2", "b3", "b4"),
            service_a=(["a1", "a2", "a3", "a4"], {}),
            service_b=(["b1", "b2", "b3", "b4"], {}),
        )

        add_op(
            state,
            server.shell,
            "echo roll",
            _batch_size=2,
            _error_budget={"service_a": 1, "service_b": 10},
        )

        with patch(
            HOST_RUN_SHELL_COMMAND,
            autospec=True,
            side_effect=_failing_hosts({"a1"}),
        ) as fake_run_command:
            run_ops(state)

        executed_hosts = {call.args[0].name for call in fake_run_command.call_args_list}

        # First service group: batch 1 (a1, a2) ran; a1 exhausted the budget, so
        # the second batch (a3, a4) was never admitted.
        assert {"a1", "a2"} <= executed_hosts
        assert "a3" not in executed_hosts and "a4" not in executed_hosts

        # The other service group is independent and rolls all the way through.
        assert {"b1", "b2", "b3", "b4"} <= executed_hosts

        group_a = state.rollout.groups["service_a"]
        assert group_a.paused is True
        assert group_a.error_count == 1
        assert group_a.next_batch == 1

        group_b = state.rollout.groups["service_b"]
        assert group_b.paused is False
        assert group_b.next_batch == 2

        # The unadmitted hosts stay active (not failed), and remain locatable.
        a3, a4 = inventory.get_host("a3"), inventory.get_host("a4")
        assert a3 not in state.failed_hosts and a4 not in state.failed_hosts
        assert state.rollout.is_host_unstarted(a3)
        assert state.rollout.is_host_unstarted(a4)

        unstarted = state.rollout.get_unstarted_batches()
        assert list(unstarted) == ["service_a"]
        assert [[h.name for h in batch] for batch in unstarted["service_a"]] == [
            ["a3", "a4"],
        ]

        # Real results still landed for the admitted batch.
        assert state.results[inventory.get_host("a1")].error_ops == 1
        assert state.results[inventory.get_host("a2")].success_ops == 1
        assert state.results[inventory.get_host("b4")].success_ops == 1

    def test_admitted_batch_reports_real_results_after_budget_pause(self):
        """Both hosts of the triggering batch report, even though one fails."""

        state, inventory = self._make_state(
            ("a1", "a2", "a3", "a4"),
            service_a=(["a1", "a2", "a3", "a4"], {}),
        )

        add_op(
            state,
            server.shell,
            "echo roll",
            _batch_size=2,
            _error_budget={"service_a": 1},
        )

        with patch(
            HOST_RUN_SHELL_COMMAND,
            autospec=True,
            side_effect=_failing_hosts({"a1"}),
        ):
            run_ops(state)

        # The successful host in the failing batch still reports its real result.
        assert state.results[inventory.get_host("a2")].success_ops == 1
        # The later batch never ran at all.
        assert state.results[inventory.get_host("a3")].success_ops == 0
        assert state.results[inventory.get_host("a3")].error_ops == 0

    def test_budget_persists_across_operations(self):
        state, inventory = self._make_state(
            ("a1", "a2", "a3", "a4"),
            service_a=(["a1", "a2", "a3", "a4"], {}),
        )

        add_op(
            state,
            server.shell,
            "echo first",
            _batch_size=2,
            _error_budget={"service_a": 1},
        )
        add_op(
            state,
            server.shell,
            "echo second",
            _batch_size=2,
            _error_budget={"service_a": 1},
        )

        with patch(
            HOST_RUN_SHELL_COMMAND,
            autospec=True,
            side_effect=_failing_hosts({"a1"}),
        ) as fake_run_command:
            run_ops(state)

        executed_hosts = [call.args[0].name for call in fake_run_command.call_args_list]

        # First op: a1, a2 run (a1 fails, budget pauses). Second op: a1 is now
        # failed/inactive, but a2 (admitted in batch 1) still runs; a3, a4 never do.
        assert executed_hosts.count("a2") == 2
        assert "a3" not in executed_hosts and "a4" not in executed_hosts
        assert state.rollout.groups["service_a"].paused is True

    def test_group_without_budget_rolls_through_failures(self):
        state, inventory = self._make_state(
            ("a1", "a2", "a3", "a4"),
            service_a=(["a1", "a2", "a3", "a4"], {}),
        )

        add_op(state, server.shell, "echo roll", _batch_size=2)

        with patch(
            HOST_RUN_SHELL_COMMAND,
            autospec=True,
            side_effect=_failing_hosts({"a1", "a3"}),
        ) as fake_run_command:
            run_ops(state)

        executed_hosts = {call.args[0].name for call in fake_run_command.call_args_list}
        # No budget configured: every batch is admitted despite failures.
        assert executed_hosts == {"a1", "a2", "a3", "a4"}
        assert state.rollout.groups["service_a"].paused is False
        assert inventory.get_host("a3") in state.failed_hosts

    def test_serial_and_rollout_are_mutually_exclusive(self):
        state, _ = self._make_state(
            ("a1", "a2"),
            service_a=(["a1", "a2"], {}),
        )
        add_op(
            state,
            server.shell,
            "echo roll",
            _serial=True,
            _batch_size=2,
        )

        with patch(HOST_RUN_SHELL_COMMAND, autospec=True):
            try:
                run_ops(state)
            except PyinfraError as error:
                assert "_serial" in str(error) and "_batch_size" in str(error)
            else:
                raise AssertionError("serial + batch_size should be rejected")

    def test_changing_rollout_settings_between_ops_is_rejected(self):
        state, _ = self._make_state(
            ("a1", "a2", "a3", "a4"),
            service_a=(["a1", "a2", "a3", "a4"], {}),
        )
        add_op(state, server.shell, "echo first", _batch_size=2)
        add_op(state, server.shell, "echo second", _batch_size=3)

        with patch(
            HOST_RUN_SHELL_COMMAND,
            autospec=True,
            side_effect=_failing_hosts(set()),
        ):
            try:
                run_ops(state)
            except PyinfraError as error:
                assert "_batch_size" in str(error)
            else:
                raise AssertionError("changing batch size mid-deploy should be rejected")

    def test_non_rolling_operations_create_no_rollout_state(self):
        state, _ = self._make_state(("a1", "a2"))
        add_op(state, server.shell, "echo plain")

        with patch(
            HOST_RUN_SHELL_COMMAND,
            autospec=True,
            side_effect=_failing_hosts(set()),
        ):
            run_ops(state)

        assert state.rollout is None

    def test_invalid_argument_types_are_rejected_at_add_time(self):
        state, _ = self._make_state(
            ("a1", "a2"),
            service_a=(["a1", "a2"], {}),
        )

        with self.assertRaises(ArgumentTypeError):
            add_op(state, server.shell, "echo roll", _batch_size="two")  # type: ignore[arg-type]

        with self.assertRaises(ArgumentTypeError):
            add_op(
                state,
                server.shell,
                "echo roll",
                _batch_size=1,
                _error_budget={"service_a": "one"},  # type: ignore[dict-item]
            )

        # Nothing was registered for the rejected operations.
        assert not state.op_meta
        assert state.rollout is None
