from collections.abc import Set as AbstractSet
from typing import Any

from taskiq import InMemoryBroker, SimpleRetryMiddleware
from taskiq.abc.middleware import TaskiqMiddleware
from taskiq.kicker import AsyncKicker


class _TaskOnlyLabelMiddleware(TaskiqMiddleware):
    """Middleware that declares a label as task-only (never goes on wire)."""

    @classmethod
    def task_only_labels(cls) -> AbstractSet[str]:
        return super().task_only_labels() | {"label_not_on_wire"}


async def test_task_only_labels_not_serialized() -> None:
    """Labels doesn't go on wire when corresponding middlewares are registered."""
    broker = InMemoryBroker().with_middlewares(
        SimpleRetryMiddleware(),
        _TaskOnlyLabelMiddleware(),
    )

    @broker.task(
        types_of_exceptions=(ValueError, TypeError),
        label_not_on_wire="test_label_value",
    )
    async def run_task() -> None:
        pass

    kicker = run_task.kicker()
    message = kicker._prepare_message()

    assert "types_of_exceptions" not in message.labels
    assert "types_of_exceptions" not in (message.labels_types or {})
    assert "label_not_on_wire" not in message.labels
    assert "label_not_on_wire" not in (message.labels_types or {})


async def test_task_only_labels_still_local_on_task() -> None:
    """The registered task still keeps the real label values locally."""
    broker = InMemoryBroker().with_middlewares(
        SimpleRetryMiddleware(),
        _TaskOnlyLabelMiddleware(),
    )

    @broker.task(
        types_of_exceptions=(ValueError, TypeError),
        label_not_on_wire="test_label_value",
    )
    async def run_task() -> None:
        pass

    task = broker.find_task(run_task.task_name)
    assert task is not None
    assert task.labels["types_of_exceptions"] == (ValueError, TypeError)
    assert task.labels["label_not_on_wire"] == "test_label_value"


async def test_other_labels_still_serialized() -> None:
    """Unrelated labels are unaffected by the fix."""
    broker = InMemoryBroker().with_middlewares(
        SimpleRetryMiddleware(),
        _TaskOnlyLabelMiddleware(),
    )

    kicker: AsyncKicker[Any, Any] = AsyncKicker(
        task_name="some_task",
        broker=broker,
        labels={"retries": 3, "queue": "high_priority"},
    )
    message = kicker._prepare_message()

    assert message.labels["retries"] == "3"
    assert message.labels["queue"] == "high_priority"


async def test_without_labels_revokes_inherited_labels() -> None:
    """Labels dropped from the kicker are absent from the prepared message."""
    broker = InMemoryBroker()

    @broker.task(tenant="acme", priority=5, region="eu")
    async def run_task() -> None:
        pass

    message = (
        run_task.kicker()
        .without_labels("tenant", "priority")
        ._prepare_message()
    )

    assert "tenant" not in message.labels
    assert "priority" not in message.labels
    assert message.labels["region"] == "eu"


async def test_without_labels_ignores_unknown_names() -> None:
    """Revoking a label that was never set is a no-op, not an error."""
    broker = InMemoryBroker()

    @broker.task(tenant="acme")
    async def run_task() -> None:
        pass

    message = run_task.kicker().without_labels("missing")._prepare_message()

    assert message.labels == {"tenant": "acme"}


async def test_kicker_label_changes_never_reach_task_template() -> None:
    """with_labels/without_labels operate on a per-kick copy."""
    broker = InMemoryBroker()

    @broker.task(tenant="acme", priority=5)
    async def run_task() -> None:
        pass

    original = dict(run_task.labels)

    run_task.kicker().with_labels(tenant="other", extra="x")
    run_task.kicker().without_labels("priority")

    assert run_task.labels == original


async def test_fresh_kicker_follows_template_after_revocations() -> None:
    """A new kicker always inherits the untouched template labels."""
    broker = InMemoryBroker()

    @broker.task(tenant="acme", priority=5)
    async def run_task() -> None:
        pass

    revoked_message = run_task.kicker().without_labels("tenant")._prepare_message()
    assert "tenant" not in revoked_message.labels

    fresh_message = run_task.kicker()._prepare_message()
    assert fresh_message.labels == {"tenant": "acme", "priority": "5"}


async def test_without_labels_returns_same_kicker_for_chaining() -> None:
    """Revocation is chainable with the other kicker modifiers."""
    broker = InMemoryBroker()
    kicker: AsyncKicker[Any, Any] = AsyncKicker(
        task_name="some_task",
        broker=broker,
        labels={"a": 1, "b": 2},
    )

    assert kicker.without_labels("a") is kicker
    assert kicker.labels == {"b": 2}

