from typing import Any

import pytest
from taskiq_dependencies import Depends

from taskiq import (
    EXECUTIONS_LABEL,
    RETRY_PHASE_LABEL,
    InMemoryBroker,
    RetryPhase,
    SimpleRetryMiddleware,
    SmartRetryMiddleware,
    TaskiqResult,
    TaskiqResultTimeoutError,
)
from taskiq.brokers.inmemory_broker import InmemoryResultBackend
from taskiq.middlewares.retry_middleware import BaseRetryMiddleware


class FlakyResultBackend(InmemoryResultBackend[Any]):
    """Result backend that fails according to a configurable predicate."""

    def __init__(self, fail_times: int = 1) -> None:
        super().__init__()
        self.fail_times = fail_times
        self.set_calls = 0

    async def set_result(self, task_id: str, result: TaskiqResult[Any]) -> None:
        self.set_calls += 1
        if self.set_calls <= self.fail_times:
            raise ConnectionError("result backend is unavailable")
        await super().set_result(task_id, result)


def _make_broker(
    middleware_class: type[BaseRetryMiddleware],
    **middleware_kwargs: Any,
) -> InMemoryBroker:
    broker = InMemoryBroker()
    broker.result_backend = FlakyResultBackend()
    return broker.with_middlewares(middleware_class(**middleware_kwargs))


@pytest.mark.parametrize(
    "middleware_class",
    [SimpleRetryMiddleware, SmartRetryMiddleware],
)
async def test_dependency_failure_is_retried_and_reprepared(
    middleware_class: type[BaseRetryMiddleware],
) -> None:
    """A flaky dependency is re-resolved and the task eventually succeeds."""
    broker = _make_broker(
        middleware_class,
        default_retry_label=True,
        default_retry_count=5,
        default_delay=0,
        retry_on=(RetryPhase.DEPENDENCIES, RetryPhase.EXECUTION, RetryPhase.SAVE),
    )
    dep_calls = 0
    task_runs = 0

    def flaky_dependency() -> int:
        nonlocal dep_calls
        dep_calls += 1
        if dep_calls == 1:
            raise ConnectionError("dependency is not ready")
        return 42

    @broker.task(retry_on_error=True, max_retries=5)
    async def my_task(value: int = Depends(flaky_dependency)) -> int:
        nonlocal task_runs
        task_runs += 1
        return value

    task = await my_task.kiq()
    result = await task.wait_result(timeout=2)

    assert not result.is_err
    assert result.return_value == 42
    # The dependency was resolved twice, once per delivery.
    assert dep_calls == 2
    # The task function itself ran only on the successful delivery.
    assert task_runs == 1
    assert result.labels[EXECUTIONS_LABEL] == 1


@pytest.mark.parametrize(
    "middleware_class",
    [SimpleRetryMiddleware, SmartRetryMiddleware],
)
async def test_save_failure_retries_without_rerunning_task(
    middleware_class: type[BaseRetryMiddleware],
) -> None:
    """Failing result writes only retry the save, never the task body."""
    broker = _make_broker(
        middleware_class,
        default_retry_label=True,
        default_retry_count=5,
        default_delay=0,
        retry_on=(RetryPhase.SAVE,),
    )
    task_runs = 0

    @broker.task(retry_on_error=True, max_retries=5)
    async def my_task() -> str:
        nonlocal task_runs
        task_runs += 1
        return "saved"

    task = await my_task.kiq()
    result = await task.wait_result(timeout=2)

    assert result.return_value == "saved"
    assert not result.is_err
    # The business logic ran exactly once even though the first save failed.
    assert task_runs == 1
    assert result.labels[EXECUTIONS_LABEL] == 1
    # One failed attempt plus one successful retry of the save.
    assert broker.result_backend.set_calls == 2


async def test_alternating_dependency_and_save_failures() -> None:
    """Dependency prep and result saving take turns failing.

    The flaky dependency fails on the first delivery, so the whole message
    is re-sent and the dependency gets another chance. On the second
    delivery the task runs and succeeds, but the first save fails; the
    save is retried in place. The task function must run only once.
    """
    broker = InMemoryBroker()

    class AlternatingBackend(InmemoryResultBackend[Any]):
        def __init__(self) -> None:
            super().__init__()
            self.set_calls = 0

        async def set_result(
            self,
            task_id: str,
            result: TaskiqResult[Any],
        ) -> None:
            self.set_calls += 1
            if self.set_calls == 1:
                raise ConnectionError("save failed once")
            await super().set_result(task_id, result)

    broker.result_backend = AlternatingBackend()
    broker.with_middlewares(
        SimpleRetryMiddleware(
            default_retry_label=True,
            default_retry_count=5,
            retry_on=(
                RetryPhase.DEPENDENCIES,
                RetryPhase.EXECUTION,
                RetryPhase.SAVE,
            ),
        ),
    )

    dep_calls = 0
    task_runs = 0

    def flaky_dependency() -> int:
        nonlocal dep_calls
        dep_calls += 1
        if dep_calls == 1:
            raise ConnectionError("dependency failed once")
        return 7

    @broker.task(retry_on_error=True, max_retries=5)
    async def my_task(value: int = Depends(flaky_dependency)) -> int:
        nonlocal task_runs
        task_runs += 1
        return value

    task = await my_task.kiq()
    result = await task.wait_result(timeout=2)

    assert not result.is_err
    assert result.return_value == 7
    # Dependency preparation was attempted on both deliveries.
    assert dep_calls == 2
    # The task body was executed only on the second delivery.
    assert task_runs == 1
    assert result.labels[EXECUTIONS_LABEL] == 1
    # Two save attempts: one failure, one in-place successful retry.
    assert broker.result_backend.set_calls == 2


async def test_save_retry_keeps_business_exception_rules() -> None:
    """Exception type filtering still applies to save-phase failures."""
    broker = InMemoryBroker()
    broker.result_backend = FlakyResultBackend(fail_times=10)
    broker.with_middlewares(
        SimpleRetryMiddleware(
            default_retry_label=True,
            default_retry_count=5,
            types_of_exceptions=(KeyError,),
            retry_on=(RetryPhase.SAVE,),
        ),
    )
    task_runs = 0

    @broker.task(retry_on_error=True, max_retries=5)
    async def my_task() -> int:
        nonlocal task_runs
        task_runs += 1
        return 1

    task = await my_task.kiq()
    # The result never becomes ready because saves are not retried.
    with pytest.raises(TaskiqResultTimeoutError):
        await task.wait_result(check_interval=0.05, timeout=0.3)

    assert task_runs == 1
    # Only the initial save attempt; the ConnectionError is not retryable.
    assert broker.result_backend.set_calls == 1


async def test_save_phase_disabled_by_default() -> None:
    """Without RetryPhase.SAVE, save failures are not retried in place."""
    broker = InMemoryBroker()
    broker.result_backend = FlakyResultBackend(fail_times=10)
    broker.with_middlewares(
        SimpleRetryMiddleware(
            default_retry_label=True,
            default_retry_count=5,
        ),
    )
    task_runs = 0

    @broker.task(retry_on_error=True, max_retries=5)
    async def my_task() -> int:
        nonlocal task_runs
        task_runs += 1
        return 1

    task = await my_task.kiq()
    with pytest.raises(TaskiqResultTimeoutError):
        await task.wait_result(check_interval=0.05, timeout=0.3)

    assert task_runs == 1
    assert broker.result_backend.set_calls == 1


async def test_execution_phase_disabled_skips_task_error_retries() -> None:
    """When EXECUTION is not allowed, task errors are not re-sent."""
    broker = _make_broker(
        SimpleRetryMiddleware,
        default_retry_label=True,
        default_retry_count=5,
        retry_on=(RetryPhase.SAVE,),
    )
    task_runs = 0

    @broker.task(retry_on_error=True, max_retries=5)
    async def my_task() -> None:
        nonlocal task_runs
        task_runs += 1
        raise ValueError("boom")

    task = await my_task.kiq()
    result = await task.wait_result(timeout=2)

    assert result.is_err
    assert isinstance(result.error, ValueError)
    assert task_runs == 1


async def test_dependency_failure_marks_dependency_phase() -> None:
    """Middlewares can see which phase failed through the message label."""
    broker = InMemoryBroker()
    seen_phases: list[RetryPhase] = []

    class RecordingMiddleware(SimpleRetryMiddleware):
        async def on_error(self, message, result, exception):  # type: ignore[no-untyped-def]
            seen_phases.append(message.labels[RETRY_PHASE_LABEL])
            await super().on_error(message, result, exception)

    broker.with_middlewares(
        RecordingMiddleware(
            default_retry_label=True,
            default_retry_count=2,
            retry_on=(RetryPhase.DEPENDENCIES,),
        ),
    )

    def broken_dependency() -> int:
        raise ConnectionError("no dep")

    @broker.task(retry_on_error=True, max_retries=2)
    async def my_task(value: int = Depends(broken_dependency)) -> int:
        return value

    task = await my_task.kiq()
    result = await task.wait_result(timeout=2)

    assert result.is_err
    assert isinstance(result.error, ConnectionError)
    # Two deliveries: initial attempt plus one retry, both fail in deps.
    assert seen_phases == [RetryPhase.DEPENDENCIES, RetryPhase.DEPENDENCIES]
    assert result.labels[EXECUTIONS_LABEL] == 0


async def test_execution_failure_marks_execution_phase() -> None:
    """Task body failures are reported as the execution phase."""
    broker = InMemoryBroker()
    seen_phases: list[RetryPhase] = []

    class RecordingMiddleware(SimpleRetryMiddleware):
        async def on_error(self, message, result, exception):  # type: ignore[no-untyped-def]
            seen_phases.append(message.labels[RETRY_PHASE_LABEL])
            await super().on_error(message, result, exception)

    broker.with_middlewares(
        RecordingMiddleware(
            default_retry_label=True,
            default_retry_count=2,
        ),
    )

    @broker.task(retry_on_error=True, max_retries=2)
    async def my_task() -> None:
        raise ValueError("boom")

    task = await my_task.kiq()
    result = await task.wait_result(timeout=2)

    assert result.is_err
    assert seen_phases == [RetryPhase.EXECUTION, RetryPhase.EXECUTION]
    assert result.labels[EXECUTIONS_LABEL] == 2


async def test_save_retry_budget_is_shared() -> None:
    """The max_retries budget covers save retries as well."""
    broker = InMemoryBroker()
    backend = FlakyResultBackend(fail_times=10)
    broker.result_backend = backend
    broker.with_middlewares(
        SimpleRetryMiddleware(
            default_retry_label=True,
            default_retry_count=3,
            retry_on=(RetryPhase.SAVE,),
        ),
    )

    @broker.task(retry_on_error=True, max_retries=3)
    async def my_task() -> int:
        return 1

    task = await my_task.kiq()
    with pytest.raises(TaskiqResultTimeoutError):
        await task.wait_result(check_interval=0.05, timeout=0.3)

    # Initial attempt + 2 save retries, then the budget is exhausted.
    assert backend.set_calls == 3
    assert task.wait_result  # broker keeps the message unacked/unresolved


