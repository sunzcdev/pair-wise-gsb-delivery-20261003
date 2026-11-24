from __future__ import annotations

import time
import traceback
from collections.abc import Callable
from itertools import product
from typing import TYPE_CHECKING, cast

import gevent
from gevent.lock import BoundedSemaphore
from paramiko import SSHException

from pyinfra import logger
from pyinfra.api.output import format_text
from pyinfra.connectors.util import CommandOutput, OutputLine
from pyinfra.context import ctx_host, ctx_state
from pyinfra.progress import progress_spinner

from .arguments import CONNECTOR_ARGUMENT_KEYS, ConnectorArguments
from .command import FunctionCommand, PyinfraCommand, StringCommand
from .dependencies import (
    CrossHostDependencyGraph,
    DependencyStatus,
    OperationHandle,
)
from .exceptions import (
    CrossHostDependencyError,
    NestedOperationError,
    PyinfraError,
)
from .util import (
    format_exception,
    log_error_or_warning,
    log_host_command_error,
    log_operation_start,
    print_host_combined_output,
)

if TYPE_CHECKING:
    from .inventory import Host
    from .state import State


# Run a single host operation
#


def run_host_op(state: State, host: Host, op_hash: str) -> bool:
    state.trigger_callbacks("operation_host_start", host, op_hash)

    if op_hash not in state.ops[host]:
        logger.info(f"{host.print_prefix}{format_text('Skipped', 'blue')}")
        return True

    op_meta = state.get_op_meta(op_hash)
    logger.debug("Starting operation %r on %s", op_meta.names, host)

    if host.executing_op_hash is None:
        host.executing_op_hash = op_hash
    else:
        host.nested_executing_op_hash = op_hash

    try:
        return _run_host_op(state, host, op_hash)
    finally:
        if host.nested_executing_op_hash:
            host.nested_executing_op_hash = None
        else:
            host.executing_op_hash = None


def _run_host_op(state: State, host: Host, op_hash: str) -> bool:
    op_data = state.get_op_data_for_host(host, op_hash)
    global_arguments = op_data.global_arguments

    ignore_errors = global_arguments["_ignore_errors"]
    continue_on_error = global_arguments["_continue_on_error"]
    timeout = global_arguments.get("_timeout", 0)

    # Extract retry arguments
    retries = global_arguments.get("_retries", 0)
    retry_delay = global_arguments.get("_retry_delay", 5)
    retry_until = global_arguments.get("_retry_until", None)

    executor_kwarg_keys = CONNECTOR_ARGUMENT_KEYS
    # See: https://github.com/python/mypy/issues/10371
    base_connector_arguments: ConnectorArguments = cast(
        ConnectorArguments,
        {key: global_arguments[key] for key in executor_kwarg_keys if key in global_arguments},  # type: ignore[literal-required] # noqa
    )

    retry_attempt = 0
    did_error = False
    executed_commands = 0
    commands: list[PyinfraCommand] = []
    all_output_lines: list[OutputLine] = []

    # Retry loop
    while retry_attempt <= retries:
        did_error = False
        executed_commands = 0
        commands = []
        all_output_lines = []

        for command in op_data.command_generator():
            commands.append(command)
            status = False
            connector_arguments = base_connector_arguments.copy()
            connector_arguments.update(command.connector_arguments)

            if not isinstance(command, PyinfraCommand):
                raise TypeError(f"{command} is an invalid pyinfra command!")

            if isinstance(command, FunctionCommand):
                try:
                    with gevent.Timeout(timeout, exception=TimeoutError):
                        status = command.execute(state, host, connector_arguments)

                except TimeoutError as e:
                    log_host_command_error(host, e, timeout=timeout)

                except NestedOperationError:
                    host.log_styled("Error in nested operation", fg="red", log_func=logger.error)
                except Exception as e:
                    # Custom functions could do anything, so expect anything!
                    logger.warning(traceback.format_exc())
                    host.log_styled(
                        f"Unexpected error in Python callback: {format_exception(e)}",
                        fg="red",
                        log_func=logger.warning,
                    )

            elif isinstance(command, StringCommand):
                output_lines = CommandOutput([])
                try:
                    status, output_lines = command.execute(
                        state,
                        host,
                        connector_arguments,
                    )
                except (TimeoutError, OSError, SSHException) as e:
                    log_host_command_error(host, e, timeout=timeout)
                all_output_lines.extend(output_lines)
                # If we failed and have not already printed the stderr, print it
                if status is False and not state.print_output:
                    print_host_combined_output(host, output_lines)

            else:
                try:
                    status = command.execute(state, host, connector_arguments)
                except (TimeoutError, OSError, SSHException) as e:
                    log_host_command_error(host, e, timeout=timeout)

            # Break the loop to trigger a failure
            if status is False:
                did_error = True
                if continue_on_error is True:
                    continue
                break

            executed_commands += 1

        # Check if we should retry
        should_retry = False
        if retry_attempt < retries:
            # Retry on error
            if did_error:
                should_retry = True
            # Retry on condition if no error
            elif retry_until and not did_error:
                try:
                    output_data = {
                        "stdout_lines": [
                            line.line for line in all_output_lines if line.buffer_name == "stdout"
                        ],
                        "stderr_lines": [
                            line.line for line in all_output_lines if line.buffer_name == "stderr"
                        ],
                        "commands": [str(command) for command in commands],
                        "executed_commands": executed_commands,
                        "host": host.name,
                        "operation": ", ".join(state.get_op_meta(op_hash).names) or "Operation",
                    }
                    should_retry = retry_until(output_data)
                except Exception as e:
                    host.log_styled(
                        f"Error in retry_until function: {format_exception(e)}",
                        fg="red",
                        log_func=logger.warning,
                    )

        if should_retry:
            retry_attempt += 1
            state.trigger_callbacks("operation_host_retry", host, op_hash, retry_attempt, retries)
            op_name = ", ".join(state.get_op_meta(op_hash).names) or "Operation"
            host.log_styled(
                f"Retrying {op_name} (attempt {retry_attempt}/{retries}) after {retry_delay}s...",
                fg="yellow",
                log_func=logger.info,
            )
            time.sleep(retry_delay)
            continue

        break

    # Handle results
    op_success = return_status = not did_error
    host_results = state.get_results_for_host(host)

    if did_error is False:
        host_results.ops += 1
        host_results.success_ops += 1

        _status_text = "Success" if executed_commands > 0 else "No changes"
        if retry_attempt > 0:
            _status_text = f"{_status_text} on retry {retry_attempt}"

        _log_status = format_text(_status_text, "green" if executed_commands > 0 else "cyan")
        logger.info(f"{host.print_prefix}{_log_status}")

        state.trigger_callbacks("operation_host_success", host, op_hash, retry_attempt)
    else:
        if ignore_errors:
            host_results.ignored_error_ops += 1
        else:
            host_results.error_ops += 1

        if executed_commands:
            host_results.partial_ops += 1

        _command_description = f"executed {executed_commands} commands"
        if retry_attempt > 0:
            _command_description = (
                f"{_command_description} (failed after {retry_attempt}/{retries} retries)"
            )

        log_error_or_warning(host, ignore_errors, _command_description, continue_on_error)

        # Ignored, op "completes" w/ ignored error
        if ignore_errors:
            host_results.ops += 1
            return_status = True

        # Unignored error -> False
        state.trigger_callbacks("operation_host_error", host, op_hash, retry_attempt, retries)

    op_data.operation_meta.set_complete(
        op_success,
        commands,
        CommandOutput(all_output_lines),
        retry_attempts=retry_attempt,
        max_retries=retries,
    )

    return return_status


# Run all operations strategies
#


def _run_host_op_with_context(state: State, host: Host, op_hash: str):
    with ctx_host.use(host):
        return run_host_op(state, host, op_hash)


def _run_host_ops(state: State, host: Host, progress=None):
    """
    Run all ops for a single server.
    """

    logger.debug("Running all ops on %s", host)

    for op_hash in state.get_op_order():
        op_meta = state.get_op_meta(op_hash)
        log_operation_start(op_meta)

        result = _run_host_op_with_context(state, host, op_hash)

        # Trigger CLI progress if provided
        if progress:
            progress((host, op_hash))

        if result is False:
            raise PyinfraError(
                f"Error in operation {', '.join(op_meta.names)} on {host}",
            )


def _run_serial_ops(state: State):
    """
    Run all ops for all servers, one server at a time.
    """

    for host in list(state.inventory.get_active_hosts()):
        host_operations = product([host], state.get_op_order())
        with progress_spinner(host_operations) as progress:
            try:
                _run_host_ops(
                    state,
                    host,
                    progress=progress,
                )
            except PyinfraError:
                state.fail_hosts({host})


def _run_no_wait_ops(state: State):
    """
    Run all ops for all servers at once.
    """

    hosts_operations = product(state.inventory.get_active_hosts(), state.get_op_order())
    with progress_spinner(hosts_operations) as progress:
        # Spawn greenlet for each host to run *all* ops
        if state.pool is None:
            raise PyinfraError("No pool found on state.")
        greenlet_to_host = {
            state.pool.spawn(
                _run_host_ops,
                state,
                host,
                progress=progress,
            ): host
            for host in state.inventory.get_active_hosts()
        }
        gevent.joinall(list(greenlet_to_host))

        # `_run_host_ops` raises as soon as one of a host's operations fails, and nothing else
        # reports it here: a greenlet that ended with a pyinfra error is a host that failed.
        # Any other exception is a bug, and is raised rather than reported as a host failure -
        # the same distinction `_run_serial_ops` makes.
        failed_hosts = set()

        for greenlet, host in greenlet_to_host.items():
            exception = greenlet.exception
            if exception is None:
                continue
            if not isinstance(exception, PyinfraError):
                raise exception
            failed_hosts.add(host)

        state.fail_hosts(failed_hosts)


def _run_single_op(state: State, op_hash: str):
    """
    Run a single operation for all servers. Can be configured to run in serial.
    """

    state.trigger_callbacks("operation_start", op_hash)

    op_meta = state.get_op_meta(op_hash)
    log_operation_start(op_meta)

    if op_meta.global_arguments["_serial"]:
        with progress_spinner(state.inventory.get_active_hosts()) as progress:
            # For each host, run the op
            for host in state.inventory.get_active_hosts():
                result = _run_host_op_with_context(state, host, op_hash)
                progress(host)

                # Fail each host straight away so the fail percent can stop the
                # operation before it runs on the remaining hosts.
                if not result:
                    state.fail_hosts({host})

    else:
        # Start with the whole inventory in one batch
        batches = [list(state.inventory.get_active_hosts())]

        # If parallel set break up the inventory into a series of batches
        parallel = op_meta.global_arguments["_parallel"]
        if parallel:
            hosts = list(state.inventory.get_active_hosts())
            batches = [hosts[i : i + parallel] for i in range(0, len(hosts), parallel)]

        for batch in batches:
            with progress_spinner(batch) as progress:
                # Spawn greenlet for each host
                if state.pool is None:
                    raise PyinfraError("No pool found on state.")
                greenlet_to_host = {
                    state.pool.spawn(_run_host_op_with_context, state, host, op_hash): host
                    for host in batch
                }

                # Trigger CLI progress as hosts complete if provided
                for greenlet in gevent.iwait(greenlet_to_host.keys()):
                    host = greenlet_to_host[greenlet]
                    progress(host)

                # Get all the results
                failed_hosts = {
                    host for greenlet, host in greenlet_to_host.items() if not greenlet.get()
                }

            # Fail each batch's hosts before starting the next batch, so the fail
            # percent can stop the operation part way through.
            state.fail_hosts(failed_hosts)

    state.trigger_callbacks("operation_end", op_hash)


def _run_cross_host_dependency_host_ops(
    state: State,
    host: Host,
    graph: CrossHostDependencyGraph,
    execution_slots: BoundedSemaphore,
    progress: Callable[[tuple[Host, str]], None] | None = None,
) -> None:
    """
    Run one host's local operation stream while honouring only explicit cross-host edges.

    Waiting for an incoming edge blocks this host stream, not streams belonging to unrelated
    hosts. This is the unit that prevents a cluster reload from overtaking its backend checks.
    """

    for op_hash in list(host.op_hash_order):
        handle = OperationHandle(host.name, op_hash)

        try:
            graph.wait_for_target(handle)
        except CrossHostDependencyError as error:
            graph.block_target(handle, str(error))
            logger.error(
                "Skipping dependent operation on %s because its cross-host edge did not settle",
                host,
            )
            if progress:
                progress((host, op_hash))
            raise

        op_data = state.ops[host].get(op_hash)
        if op_data is None:
            graph.settle_node(handle, True, "operation is not present on host")
            if progress:
                progress((host, op_hash))
            continue

        operation_meta = op_data.operation_meta

        # Recovery against the same State must not call set_complete() twice. A genuinely
        # successful node is reused; incomplete timed-out nodes fall through and run again.
        if operation_meta.is_complete():
            success = operation_meta.did_succeed(_raise_if_not_complete=False) is True

            if success:
                graph.settle_node(
                    handle,
                    True,
                    "already completed in the current State",
                )
                if progress:
                    progress((host, op_hash))
                continue

            existing_settlement = graph.settlements.get(handle.node)
            if (
                existing_settlement is not None
                and existing_settlement.status is not DependencyStatus.PENDING
            ):
                graph.settle_node(handle, False, "already failed")
                if progress:
                    progress((host, op_hash))
                op_meta = state.get_op_meta(op_hash)
                raise PyinfraError(
                    f"Error in operation {', '.join(op_meta.names)} on {host}",
                )

            # graph.recover(reset_failed=True) explicitly discarded the terminal settlement.
            operation_meta.reset_for_recovery()

        op_meta = state.get_op_meta(op_hash)
        log_operation_start(op_meta)

        # Dependency waiting happens before acquiring an execution slot. Otherwise a target
        # could occupy a limited parallel slot while waiting for a source that cannot be
        # scheduled, creating an implementation-induced deadlock.
        with execution_slots:
            result = _run_host_op_with_context(state, host, op_hash)

        if progress:
            progress((host, op_hash))

        graph.settle_node(handle, result)

        if result is False:
            raise PyinfraError(
                f"Error in operation {', '.join(op_meta.names)} on {host}",
            )


def _run_cross_host_dependency_ops(state: State) -> None:
    """
    Run host-local streams concurrently and coordinate only registered cross-host edges.

    A timed-out source's host greenlet is cancelled so an unresponsive call cannot keep the
    run alive forever. Unrelated host streams are joined normally and therefore still finish.
    """

    graph = state.cross_host_dependencies
    graph.arm(state)

    active_hosts = list(state.inventory.get_active_hosts())
    planned_operations = product(active_hosts, state.get_op_order())

    dependency_errors: list[CrossHostDependencyError] = []
    failed_hosts: set[Host] = set()
    cancelled_hosts: set[Host] = set()

    with progress_spinner(planned_operations) as progress:
        parallel_limit = state.config.PARALLEL or len(active_hosts) or 1
        execution_slots = BoundedSemaphore(parallel_limit)

        # Spawn all host coordinators directly. They wait on edges outside the parallel
        # semaphore; using state.pool here could block spawning the very source host a queued
        # target is waiting for.
        greenlet_to_host = {
            gevent.spawn(
                _run_cross_host_dependency_host_ops,
                state,
                host,
                graph,
                execution_slots,
                progress,
            ): host
            for host in active_hosts
        }
        host_to_greenlet = {host: greenlet for greenlet, host in greenlet_to_host.items()}
        pending = dict(greenlet_to_host)

        while pending:
            # Short bounded wait lets the coordinator enforce edge deadlines even when the
            # target is not currently inside wait_for_target().
            gevent.wait(list(pending), timeout=0.02)

            for greenlet in list(pending):
                if not greenlet.ready():
                    continue

                host = pending.pop(greenlet)
                host_to_greenlet.pop(host, None)
                exception = greenlet.exception

                if exception is None:
                    continue
                if isinstance(exception, CrossHostDependencyError):
                    dependency_errors.append(exception)
                elif isinstance(exception, PyinfraError):
                    failed_hosts.add(host)
                elif isinstance(exception, gevent.GreenletExit) and host in cancelled_hosts:
                    logger.debug("Cancelled unreachable dependency source on %s", host)
                else:
                    raise exception

            graph.expire_edges()

            # A deadline settles the *edge* as timed out. The source call may still be blocked,
            # so cancel only that source host stream; unrelated greenlets remain untouched.
            for edge in graph.edges:
                if edge.status is not DependencyStatus.TIMED_OUT:
                    continue
                source_host = state.inventory.hosts.get(edge.source.host_name)
                source_greenlet = host_to_greenlet.get(source_host) if source_host else None
                if source_greenlet is not None and not source_greenlet.ready():
                    cancelled_hosts.add(source_host)  # type: ignore[arg-type]
                    source_greenlet.kill(block=False)

    # Preserve pyinfra's normal fail-percent/no-host settlement, but never let that generic
    # message hide which explicit edge failed or became unreachable.
    fail_settlement_error: PyinfraError | None = None
    try:
        state.fail_hosts(failed_hosts)
    except PyinfraError as error:
        fail_settlement_error = error

    if dependency_errors:
        if len(dependency_errors) == 1:
            raise dependency_errors[0] from fail_settlement_error
        message = "Multiple cross-host dependencies were not satisfied:\n" + "\n".join(
            str(error) for error in dependency_errors
        )
        raise CrossHostDependencyError(message) from fail_settlement_error

    try:
        graph.raise_if_unsatisfied()
    except CrossHostDependencyError as error:
        raise error from fail_settlement_error

    if fail_settlement_error is not None:
        raise fail_settlement_error


def run_ops(state: State, serial: bool = False, no_wait: bool = False):
    """
    Runs all operations across all servers in a configurable manner.

    Args:
        state (``pyinfra.api.State`` obj): the deploy state to execute
        serial (boolean): whether to run operations host by host
        no_wait (boolean): whether to wait for all hosts between operations
    """

    # Flag state as deploy in process
    state.is_executing = True

    with ctx_state.use(state):
        # Run all ops, but server by server
        if serial:
            _run_serial_ops(state)
        # Explicit cross-host edges use concurrent host streams so an edge blocks only its
        # dependent stream, rather than inserting a global operation barrier.
        elif state.cross_host_dependencies.edges:
            _run_cross_host_dependency_ops(state)
        # Run all the ops on each server in parallel (not waiting at each operation)
        elif no_wait:
            _run_no_wait_ops(state)
        # Default: run all ops in order, waiting at each for all servers to complete
        else:
            for op_hash in state.get_op_order():
                _run_single_op(state, op_hash)
