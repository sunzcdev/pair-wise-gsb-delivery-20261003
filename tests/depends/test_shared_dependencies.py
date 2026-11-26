"""Tests for explicit message scoped shared dependencies.

The scenario mirrors the review counterexample:

* two dependency branches enter one after another;
* the first branch obtains resource ``A``;
* the second asks for ``A`` (same tenant) and then ``B``;
* ``B`` raises while it is being constructed;
* ``A`` is entered and released exactly once;
* teardown happens in the reverse order of actual entry;
* a failed construction leaves no reusable handle;
* another concurrently processed message gets its own independent session.
"""

import asyncio
from typing import Any

import pytest

from taskiq import (
    InMemoryBroker,
    Shared,
    SharedDepends,
    SharedScope,
    TaskiqDepends,
    current_scope,
    shared_scope,
)
from taskiq.depends.shared import ShareKey


def make_session(lifecycle: list[tuple[Any, ...]], tenant: str, *, fail: bool = False):
    """Create an async generator resource recording open/close events."""

    counter = {"value": 0}

    async def session() -> int:
        counter["value"] += 1
        instance = counter["value"]
        lifecycle.append(("open", tenant, instance))
        if fail:
            raise RuntimeError(f"cannot construct {tenant}")
        try:
            yield instance
        finally:
            lifecycle.append(("close", tenant, instance))

    return session


async def test_scope_reuses_same_key_and_closes_once_lifo() -> None:
    lifecycle: list[tuple[Any, ...]] = []
    first_a = make_session(lifecycle, "A")
    second_a = make_session(lifecycle, "A")  # distinct callable, same share key
    scope = SharedScope()
    key = ShareKey(name=("session", "tenant-a"))

    a1 = await scope.get_or_create_async(key, first_a)
    lifecycle.append(("first", a1))
    a2 = await scope.get_or_create_async(key, second_a)
    assert a1 == a2  # same key -> same handle even with distinct factories

    await scope.aclose()
    opens = [event for event in lifecycle if event[0] == "open"]
    closes = [event for event in lifecycle if event[0] == "close"]
    assert len(opens) == 1
    assert len(closes) == 1
    assert opens[0][2] == closes[0][2]
    assert lifecycle[-1][0] == "close"


async def test_failed_construction_is_not_cached() -> None:
    lifecycle: list[tuple[Any, ...]] = []
    good = make_session(lifecycle, "A")
    bad = make_session(lifecycle, "B", fail=True)
    scope = SharedScope()
    key_a = ShareKey(name="A")
    key_b = ShareKey(name="B")

    assert await scope.get_or_create_async(key_a, good) == 1
    with pytest.raises(RuntimeError, match="cannot construct B"):
        await scope.get_or_create_async(key_b, bad)

    # The failed construction must not leave a cached entry.
    assert scope._find_existing(key_b) is None  # noqa: SLF001

    # Asking again constructs a fresh instance and fails again.
    with pytest.raises(RuntimeError):
        await scope.get_or_create_async(key_b, bad)
    b_open_attempts = [
        event for event in lifecycle if event[0] == "open" and event[1] == "B"
    ]
    assert len(b_open_attempts) == 2

    await scope.aclose()
    # Only the healthy A resource is closed.
    assert [event[0] for event in lifecycle if event[0] == "close"] == ["close"]


async def test_reverse_entry_order_with_multiple_resources() -> None:
    lifecycle: list[str] = []

    async def res(name: str):
        lifecycle.append(f"open-{name}")
        try:
            yield name
        finally:
            lifecycle.append(f"close-{name}")

    scope = SharedScope()
    await scope.acquire("a", lambda: res("a"))
    await scope.acquire("b", lambda: res("b"))
    await scope.aclose()
    assert lifecycle == ["open-a", "open-b", "close-b", "close-a"]


async def test_shared_scope_context_manager_binds_current_scope() -> None:
    assert current_scope() is None
    async with shared_scope() as scope:
        assert current_scope() is scope
        value = await scope.provide("k", lambda: _value_resource("v"))
        assert value == "v"
    assert current_scope() is None


async def _value_resource(value: str):
    yield value


async def test_declarative_shared_key_through_broker() -> None:
    broker = InMemoryBroker(await_inplace=True)
    lifecycle: list[tuple[Any, ...]] = []
    left = make_session(lifecycle, "A")
    right = make_session(lifecycle, "A")
    failing_b = make_session(lifecycle, "B", fail=True)

    async def first(s: int = TaskiqDepends(left, shared="tenant-a")):
        lifecycle.append(("first-enter", s))
        try:
            yield s
        finally:
            lifecycle.append(("first-exit", s))

    async def second(
        s: int = TaskiqDepends(right, shared="tenant-a"),
        b: int = TaskiqDepends(failing_b, shared="tenant-b"),
    ):
        lifecycle.append(("second", s, b))

    @broker.task
    async def target(
        a: Any = TaskiqDepends(first),
        b: Any = TaskiqDepends(second),
    ) -> None:
        raise AssertionError("target must not run")

    handle = await target.kiq()
    result = await handle.get_result()
    assert isinstance(result.error, RuntimeError)

    opens_a = [e for e in lifecycle if e[0] == "open" and e[1] == "A"]
    closes_a = [e for e in lifecycle if e[0] == "close" and e[1] == "A"]
    assert len(opens_a) == 1
    assert len(closes_a) == 1
    await broker.shutdown()


async def test_declarative_shared_true_keys_by_callable_and_kwargs() -> None:
    broker = InMemoryBroker(await_inplace=True)
    lifecycle: list[tuple[Any, ...]] = []

    async def session(tenant: str) -> Any:
        instance = len([e for e in lifecycle if e[0] == "open"]) + 1
        lifecycle.append(("open", tenant, instance))
        try:
            yield instance
        finally:
            lifecycle.append(("close", tenant, instance))

    seen: list[int] = []

    @broker.task
    async def target(
        a1: int = TaskiqDepends(
            session,
            shared=True,
            kwargs={"tenant": "A"},
        ),
        a2: int = TaskiqDepends(
            session,
            shared=True,
            kwargs={"tenant": "A"},
        ),
        b: int = TaskiqDepends(
            session,
            shared=True,
            kwargs={"tenant": "B"},
        ),
    ) -> None:
        seen.extend([a1, a2, b])

    handle = await target.kiq()
    result = await handle.get_result()
    assert result.error is None
    assert seen[0] == seen[1]
    assert seen[2] != seen[0]
    assert len([e for e in lifecycle if e[0] == "open"]) == 2
    await broker.shutdown()


async def test_independent_declaration_uses_cache_false() -> None:
    marker = TaskiqDepends(lambda: 1, independent=True)
    assert marker.use_cache is False
    plain = TaskiqDepends(lambda: 1)
    assert plain.use_cache is True


async def test_independent_dependencies_construct_separately_in_broker() -> None:
    broker = InMemoryBroker(await_inplace=True)
    constructed = 0

    async def resource() -> Any:
        nonlocal constructed
        constructed += 1
        yield constructed

    seen: list[int] = []

    @broker.task
    async def target(
        a: int = TaskiqDepends(resource, independent=True),
        b: int = TaskiqDepends(resource, independent=True),
    ) -> None:
        seen.extend([a, b])

    handle = await target.kiq()
    result = await handle.get_result()
    assert result.error is None
    assert constructed == 2
    assert seen[0] != seen[1]
    await broker.shutdown()


async def test_async_context_manager_resource_is_shared() -> None:
    entered: list[str] = []
    exited: list[str] = []

    class Session:
        def __init__(self, tenant: str) -> None:
            self.tenant = tenant

        async def __aenter__(self) -> "Session":
            entered.append(self.tenant)
            return self

        async def __aexit__(self, *exc: Any) -> None:
            exited.append(self.tenant)

    scope = SharedScope()
    first = await scope.acquire("a", lambda: Session("t1"))
    second = await scope.acquire("a", lambda: Session("t1"))
    assert first is second
    await scope.aclose()
    assert entered == ["t1"]
    assert exited == ["t1"]


async def test_shared_depends_aliases_return_dependency_marker() -> None:
    for declaration in (
        SharedDepends(lambda: 1, key="k"),
        Shared(lambda: 1, share_key="k"),
    ):
        assert declaration.shared is True  # type: ignore[attr-defined]
        assert declaration.share_key is not None  # type: ignore[attr-defined]


async def test_full_prompt_narrative_lifecycle() -> None:
    """The exact ordering quoted in the task statement."""
    broker = InMemoryBroker(await_inplace=True)
    lifecycle: list[tuple[Any, ...]] = []
    counter = {"value": 0}

    def session(tenant: str, *, fail: bool = False):
        async def resource() -> Any:
            counter["value"] += 1
            instance = counter["value"]
            lifecycle.append(("open", tenant, instance))
            if fail:
                raise RuntimeError(f"cannot build {tenant}")
            try:
                yield instance
            finally:
                lifecycle.append(("close", tenant, instance))

        return resource

    resource_a = session("A")
    resource_b = session("B", fail=True)
    key_a = ShareKey(name="tenant-a")
    key_b = ShareKey(name="tenant-b")

    async def first(s: int = TaskiqDepends(resource_a, shared=key_a.name)):
        lifecycle.append(("first-enter", s))
        try:
            yield s
        finally:
            lifecycle.append(("first-exit", s))

    async def second(branch: Any = TaskiqDepends(first)) -> Any:
        # The second branch enters after the first and asks for A again,
        # then asks for B in order.
        scope = current_scope()
        assert scope is not None
        a = await scope.get_or_create_async(key_a, resource_a)
        lifecycle.append(("second-a", a))
        assert a == branch
        await scope.get_or_create_async(key_b, resource_b)

    @broker.task
    async def target(s: Any = TaskiqDepends(second)) -> None:
        raise AssertionError("target must not run")

    handle = await target.kiq()
    result = await handle.get_result()
    assert isinstance(result.error, RuntimeError)

    assert lifecycle == [
        ("open", "A", 1),
        ("first-enter", 1),
        ("second-a", 1),
        ("open", "B", 2),
        ("first-exit", 1),
        ("close", "A", 1),
    ]
    await broker.shutdown()


async def test_tenant_resolved_from_dependency_takes_part_in_key() -> None:
    from taskiq import Context

    broker = InMemoryBroker(await_inplace=True)
    lifecycle: list[tuple[Any, ...]] = []

    async def get_tenant(context: Context = TaskiqDepends()) -> str:
        return str(context.message.kwargs["tenant"])

    async def session(tenant: str = TaskiqDepends(get_tenant)) -> Any:
        instance = len(lifecycle) + 1
        lifecycle.append(("open", tenant, instance))
        try:
            yield instance
        finally:
            lifecycle.append(("close", tenant, instance))

    seen: list[int] = []

    @broker.task
    async def target(
        tenant: str,
        a: int = TaskiqDepends(session, shared=True),
        b: int = TaskiqDepends(session, shared=True),
    ) -> None:
        seen.extend([a, b])

    handle = await target.kiq(tenant="acme")
    result = await handle.get_result()
    assert result.error is None
    assert seen[0] == seen[1]
    assert [e for e in lifecycle if e[0] == "open"] == [("open", "acme", 1)]
    await broker.shutdown()


async def test_scope_can_be_injected_by_type() -> None:
    broker = InMemoryBroker(await_inplace=True)
    seen: list[Any] = []

    @broker.task
    async def target(scope: SharedScope = TaskiqDepends()) -> None:
        seen.append(scope)
        assert isinstance(scope, SharedScope)
        assert current_scope() is scope

    handle = await target.kiq()
    result = await handle.get_result()
    assert result.error is None
    assert len(seen) == 1
    await broker.shutdown()


async def test_concurrent_messages_get_independent_sessions() -> None:
    broker = InMemoryBroker(await_inplace=False, max_async_tasks=8)
    lifecycle: list[tuple[Any, ...]] = []
    both_entered = asyncio.Event()
    release = asyncio.Event()
    active = 0
    max_active = 0

    async def session() -> Any:
        nonlocal active, max_active
        active += 1
        max_active = max(max_active, active)
        instance = active
        lifecycle.append(("open", instance))
        if active == 2:
            both_entered.set()
        await release.wait()
        try:
            yield instance
        finally:
            active -= 1
            lifecycle.append(("close", instance))

    values: list[int] = []

    @broker.task
    async def use_session(s: int = TaskiqDepends(session, shared="a")) -> int:
        values.append(s)
        return s

    first = await use_session.kiq()
    second = await use_session.kiq()
    await asyncio.wait_for(both_entered.wait(), timeout=5)
    release.set()
    await broker.wait_all()

    r1 = await first.get_result()
    r2 = await second.get_result()
    assert r1.error is None and r2.error is None
    assert r1.return_value != r2.return_value
    assert max_active == 2
    assert len([e for e in lifecycle if e[0] == "open"]) == 2
    assert sorted(values) == [1, 2]
    await broker.shutdown()
