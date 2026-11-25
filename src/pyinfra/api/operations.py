from __future__ import annotations

import time
import traceback
from itertools import product
from typing import TYPE_CHECKING, cast

import gevent
from paramiko import SSHException

from pyinfra import logger
from pyinfra.api.output import format_text
from pyinfra.connectors.util import CommandOutput, OutputLine
from pyinfra.context import ctx_host, ctx_state
from pyinfra.progress import progress_spinner

from .arguments import CONNECTOR_ARGUMENT_KEYS, ConnectorArguments
from .command import FunctionCommand, PyinfraCommand, StringCommand
from .compensation import (
    CompensationError,
    record_compensation,
    record_unknown_change,
    record_uncompensated_change,
    run_compensation,
)
from .exceptions import NestedOperationError, PyinfraError
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
    applied_commands = 0
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
            applied_commands = max(applied_commands, executed_commands)
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

    applied_commands = max(applied_commands, executed_commands)

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

        # Record successful changes for compensation should a later operation fail.
        # No-ops (zero applied commands) made no changes and have nothing to undo.
        record_compensation(state, host, op_hash, applied_commands)
        record_uncompensated_change(state, host, op_hash, applied_commands)

        state.trigger_callbacks("operation_host_success", host, op_hash, retry_attempt)
    else:
        if ignore_errors:
            host_results.ignored_error_ops += 1
        else:
            host_results.error_ops += 1

        if executed_commands:
            host_results.partial_ops += 1

        # A failed op which got through at least one command may have changed the
        # host; record it as unconfirmed rather than guessing a safe compensation.
        record_unknown_change(state, host, op_hash, applied_commands)

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


def _compensate_host(state: State, host: Host, op_hash: str | None = None) -> None:
    """
    Roll back the host's recorded compensatable operations after a failure.

    Runs within the host context so compensator generators and verifiers see the same
    ``pyinfra.host``/``pyinfra.state`` as the forward operations. A conflict or failed
    compensation raises ``CompensationError`` (carrying the detailed report); when there
    is simply nothing to roll back this returns normally and the ordinary failure flow
    continues.
    """

    with ctx_host.use(host):
        run_compensation(state, host, op_hash)


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
            # Mark the host failed and undo whatever it successfully changed before
            # reporting the failure. A compensation conflict raises CompensationError.
            _fail_host_with_compensation(state, host, op_hash)
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
            except CompensationError:
                # The host is already marked failed and its rollback report takes
                # precedence over the generic failure bookkeeping below.
                raise
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
            # A compensation conflict carries the authoritative rollback report; surface
            # it rather than folding the host into generic failure bookkeeping.
            if isinstance(exception, CompensationError):
                raise exception
            failed_hosts.add(host)

        state.fail_hosts(failed_hosts)


def _mark_host_failed_silently(state: State, host: Host) -> None:
    """
    Record a host failure without the fail-percent / "no hosts remaining" raise that
    ``fail_hosts`` performs, so compensation can run and surface its own report.
    """

    state.failed_hosts.add(host)
    state.active_hosts.discard(host)


def _raise_failed_host_checks(state: State) -> None:
    """
    Apply the fail-percent / "no hosts remaining" checks normally done inside
    ``fail_hosts``, after compensation has finished.
    """

    if not state.active_hosts:
        raise PyinfraError("No hosts remaining!")

    if state.config.FAIL_PERCENT is not None:
        activated_count = len(state.activated_hosts)
        percent_failed = (1 - len(state.active_hosts) / activated_count) * 100
        if percent_failed > state.config.FAIL_PERCENT:
            if state.should_raise_failed_hosts and state.should_raise_failed_hosts(state) is False:
                return
            raise PyinfraError(
                f"Over {state.config.FAIL_PERCENT}% of hosts failed "
                f"({int(round(percent_failed))}%)",
            )


def _fail_host_with_compensation(
    state: State,
    host: Host,
    op_hash: str | None = None,
) -> None:
    """
    Mark a host failed and roll back its successful compensatable changes.

    The host is recorded as failed first (without raising), so it stays in
    ``failed_hosts`` even when rollback itself fails. Compensation then runs and a
    conflict or failed compensation command raises ``CompensationError`` - still a
    ``PyinfraError`` - carrying its detailed report; that error takes precedence over
    the ordinary fail-percent / "no hosts remaining" checks.
    """

    _mark_host_failed_silently(state, host)
    try:
        _compensate_host(state, host, op_hash)
    except CompensationError:
        raise
    else:
        _raise_failed_host_checks(state)


def _fail_hosts_with_compensation(
    state: State,
    hosts: set[Host],
    op_hash: str | None = None,
) -> None:
    """
    Batch equivalent of :func:`_fail_host_with_compensation`: mark every failed host,
    compensate them all, then run the ordinary failure checks for the whole batch.
    """

    for host in hosts:
        _mark_host_failed_silently(state, host)
    try:
        for host in hosts:
            _compensate_host(state, host, op_hash)
    except CompensationError:
        raise
    else:
        _raise_failed_host_checks(state)


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
                    _fail_host_with_compensation(state, host, op_hash)

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

            # Roll back each failed host, then mark the whole batch failed so the fail
            # percent can stop the operation part way through. A compensation conflict
            # raises CompensationError from inside the helper before that point.
            _fail_hosts_with_compensation(state, failed_hosts, op_hash)

    state.trigger_callbacks("operation_end", op_hash)


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
        # Bound compensation to this run: a later failure must not undo records from
        # an earlier successful ``run_ops`` call on the same state.
        for host in state.inventory:
            journal = state.get_compensation_journal(host)
            journal.begin_run_scopes(
                len(state.uncompensated_changes.get(host, [])),
                len(state.unknown_changes.get(host, [])),
            )

        # Run all ops, but server by server
        if serial:
            _run_serial_ops(state)
        # Run all the ops on each server in parallel (not waiting at each operation)
        elif no_wait:
            _run_no_wait_ops(state)
        # Default: run all ops in order, waiting at each for all servers to complete
        else:
            for op_hash in state.get_op_order():
                _run_single_op(state, op_hash)

        # A successful batch has no pending rollback; clear only this run's records.
        for host in state.inventory:
            state.get_compensation_journal(host).end_run()
