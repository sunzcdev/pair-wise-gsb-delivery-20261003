from __future__ import annotations

from time import monotonic
from unittest.mock import patch

import gevent
import pytest
from gevent.event import Event

from pyinfra.api import (
    Config,
    CrossHostDependencyError,
    CrossHostDependencyTimeout,
    DependencyStatus,
    OperationHandle,
    State,
)
from pyinfra.api.exceptions import CrossHostDependencyFailed, UnresolvableCrossHostDependency
from pyinfra.api.operation import add_op
from pyinfra.api.operations import run_ops
from pyinfra.api.state import StateStage
from pyinfra.connectors.util import CommandOutput
from pyinfra.operations import server

from ..util import make_inventory


def _state(hosts=("a", "b", "c", "lb")):
    inventory = make_inventory(hosts=hosts)
    state = State(inventory, Config(FAIL_PERCENT=100))
    state.current_stage = StateStage.Prepare
    for host in inventory:
        state.activate_host(host)
    return state, inventory


def test_independent_host_finishes_while_reload_waits_for_timed_out_backend():
    state, inventory = _state()
    a, b, c, lb = (inventory.get_host(name) for name in ("a", "b", "c", "lb"))

    verify_a = add_op(state, server.shell, "verify-version", host=a)
    verify_b = add_op(state, server.shell, "verify-version", host=b)
    reload_op = add_op(state, server.shell, "cluster-reload", host=lb)
    add_op(state, server.shell, "independent-work", host=c)

    state.require_cross_host(
        target=reload_op,
        sources=[verify_a, verify_b],
        timeout=0.2,
        name="reload requires both version checks",
    )

    started = Event()
    c_done = Event()
    release_b = Event()
    completed: list[tuple[str, str]] = []

    def run_shell(host, command, **kwargs):
        text = str(command)
        if host.name == "b" and "verify-version" in text:
            started.set()
            release_b.wait(2)
        completed.append((host.name, text))
        if host.name == "c":
            c_done.set()
        return True, CommandOutput([])

    started_at = monotonic()
    with patch("pyinfra.api.host.Host.run_shell_command", run_shell):
        runner = gevent.spawn(run_ops, state)

        assert started.wait(1)
        assert c_done.wait(1)
        # C has genuinely finished while B's checkpoint call has not returned.
        assert ("c", "independent-work") in completed
        assert not any(text == "cluster-reload" for _, text in completed)

        runner.join(timeout=3)
        elapsed = monotonic() - started_at
        assert runner.ready()
        assert isinstance(runner.exception, CrossHostDependencyTimeout)

    exc_info = runner.exception
    assert elapsed < 2.0
    assert ("a", "verify-version") in completed
    assert ("c", "independent-work") in completed
    assert not any(text == "cluster-reload" for _, text in completed)
    assert "b" in str(exc_info)
    assert "did not settle within 0.2s" in str(exc_info)

    edges = state.cross_host_dependencies.edges
    by_source = {edge.source.host_name: edge for edge in edges}
    assert by_source["a"].status is DependencyStatus.SATISFIED
    assert by_source["b"].status is DependencyStatus.TIMED_OUT
    assert any(
        settlement["status"] == DependencyStatus.BLOCKED.value
        for settlement in state.cross_host_dependencies.settlement_report().values()
    )

    # Recovery: the timed-out edge is not treated as success. B is executed again and only
    # then can reload start; already successful A/C nodes are reused rather than re-executed.
    release_b.set()
    plan = state.cross_host_dependencies.recover()
    assert [edge["source"]["host"] for edge in plan["satisfied"]] == ["a"]
    assert [edge["source"]["host"] for edge in plan["retry"]] == ["b"]

    completed.clear()
    with patch("pyinfra.api.host.Host.run_shell_command", run_shell):
        run_ops(state)

    assert completed == [("b", "verify-version"), ("lb", "cluster-reload")]
    assert all(
        edge.status is DependencyStatus.SATISFIED for edge in state.cross_host_dependencies.edges
    )


def test_failed_checkpoint_settles_edge_failed_and_keeps_dependent_blocked():
    state, inventory = _state()
    a, b, c, lb = (inventory.get_host(name) for name in ("a", "b", "c", "lb"))

    verify_a = add_op(state, server.shell, "verify-version", host=a)
    verify_b = add_op(state, server.shell, "verify-version", host=b)
    reload_op = add_op(state, server.shell, "cluster-reload", host=lb)
    add_op(state, server.shell, "independent-work", host=c)

    state.require_cross_host(target=reload_op, sources=[verify_a, verify_b], timeout=1)

    completed: list[tuple[str, str]] = []
    b_should_fail = True

    def run_shell(host, command, **kwargs):
        text = str(command)
        completed.append((host.name, text))
        if host.name == "b" and "verify-version" in text and b_should_fail:
            return False, CommandOutput([])
        return True, CommandOutput([])

    with patch("pyinfra.api.host.Host.run_shell_command", run_shell):
        with pytest.raises(CrossHostDependencyFailed, match="settled as failed"):
            run_ops(state)

    assert ("c", "independent-work") in completed
    assert not any(text == "cluster-reload" for _, text in completed)
    b_edge = next(
        edge for edge in state.cross_host_dependencies.edges if edge.source.host_name == "b"
    )
    assert b_edge.status is DependencyStatus.FAILED
    assert b in state.failed_hosts

    # A known failure is not auto-retried by ordinary timeout recovery.
    state.cross_host_dependencies.recover()
    assert b_edge.status is DependencyStatus.FAILED

    # Explicit operator recovery after fixing B resets only the genuinely terminal edge and
    # reactivates B; the recovered run can then satisfy the edge and reach reload.
    b_should_fail = False
    state.recover_cross_host_dependencies(reset_failed=True)
    with patch("pyinfra.api.host.Host.run_shell_command", run_shell):
        run_ops(state)

    assert ("lb", "cluster-reload") in completed
    assert b not in state.failed_hosts
    assert all(
        edge.status is DependencyStatus.SATISFIED for edge in state.cross_host_dependencies.edges
    )


def test_invalid_reference_settles_unresolvable_without_waiting_for_timeout():
    state, inventory = _state(hosts=("a", "lb"))
    lb = inventory.get_host("lb")
    reload_op = add_op(state, server.shell, "cluster-reload", host=lb)
    target = OperationHandle.coerce(reload_op)

    state.cross_host_dependencies.add_edge(
        OperationHandle("a", "missing-operation-hash"),
        target,
        timeout=10,
    )
    state.cross_host_dependencies.arm(state)

    edge = state.cross_host_dependencies.edges[0]
    assert edge.status is DependencyStatus.UNRESOLVABLE
    with pytest.raises(UnresolvableCrossHostDependency, match="not planned on host"):
        state.cross_host_dependencies.wait_for_target(target)


def test_host_operation_dag_rejects_cross_host_cycles():
    state, inventory = _state(hosts=("a", "b"))
    a, b = inventory.get_host("a"), inventory.get_host("b")

    first = add_op(state, server.shell, "first", host=a)
    second = add_op(state, server.shell, "second", host=a)
    remote = add_op(state, server.shell, "remote", host=b)

    # Local order first -> second, plus second -> remote -> first creates a cycle.
    state.cross_host_dependencies.add_edge(second, remote)
    state.cross_host_dependencies.add_edge(remote, first)

    with pytest.raises(CrossHostDependencyError, match="Cycle detected"):
        state.get_host_operation_order()
