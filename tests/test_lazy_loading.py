"""Tests for the thread-safe lazy-loading publication boundary.

These tests exercise :mod:`chardet._lazy` with controlled loader doubles
(events and call counters) rather than the shipped model artifacts: no test
here reads, writes, or mutates the real ``models.bin`` family.
"""

from __future__ import annotations

import struct
import threading
import zlib
from unittest.mock import MagicMock

import pytest

import chardet.models as models_mod
from chardet._lazy import LazyKeyedCache, LazyResource, lazy


def test_lazy_resource_loads_once_and_shares_one_object() -> None:
    """A successful load runs the loader once and every getter shares it."""
    calls = 0

    def loader() -> object:
        nonlocal calls
        calls += 1
        return object()

    resource: LazyResource[object] = LazyResource(loader, name="test")
    assert resource.loaded() is False

    first = resource.get()
    second = resource.get()

    assert first is second
    assert calls == 1
    assert resource.loaded() is True


def test_lazy_resource_single_flight_with_two_waiters() -> None:
    """Two waiters block on one leader and receive the leader's object.

    The leader (the main thread) holds the load open behind an event while
    two waiter threads call :meth:`LazyResource.get`; neither waiter may
    invoke the loader itself, and both must return the exact object the
    leader publishes.
    """
    enter_loader = threading.Event()
    release_loader = threading.Event()
    loader_started = threading.Event()
    calls = 0
    published = object()

    def loader() -> object:
        nonlocal calls
        calls += 1
        loader_started.set()
        enter_loader.set()
        assert release_loader.wait(timeout=5)
        return published

    resource: LazyResource[object] = LazyResource(loader, name="test")
    results: list[object] = []
    errors: list[Exception] = []

    def waiter() -> None:
        enter_loader.wait(timeout=5)
        try:
            results.append(resource.get())
        except Exception as exc:  # captured for assertion, not swallowed
            errors.append(exc)

    leader_grab = threading.Thread(target=resource.get)
    leader_grab.start()
    assert loader_started.wait(timeout=5)

    waiter_threads = [threading.Thread(target=waiter) for _ in range(2)]
    for thread in waiter_threads:
        thread.start()
    # Give the waiters time to park on the in-flight load.
    assert enter_loader.wait(timeout=5)
    release_loader.set()

    leader_grab.join(timeout=5)
    for thread in waiter_threads:
        thread.join(timeout=5)
        assert not thread.is_alive()

    assert not errors
    assert calls == 1
    assert results == [published, published]
    assert resource.get() is published
    assert calls == 1


def test_lazy_resource_failure_reaches_all_waiters_and_clears_for_retry() -> None:
    """One failing load raises in every waiter; the next call retries fresh.

    The failure is remembered only long enough to release the waiters, so a
    single temporary read error can never disable the resource for the
    process.  The retry's successful value is then shared normally.
    """
    enter_loader = threading.Event()
    release_loader = threading.Event()
    loader_started = threading.Event()
    calls = 0
    failure = OSError("temporary read failure")
    recovered = object()

    def flaky_loader() -> object:
        nonlocal calls
        calls += 1
        if calls == 1:
            loader_started.set()
            enter_loader.set()
            assert release_loader.wait(timeout=5)
            raise failure
        return recovered

    resource: LazyResource[object] = LazyResource(flaky_loader, name="test")
    waiter_errors: list[Exception] = []

    def waiter() -> None:
        enter_loader.wait(timeout=5)
        with pytest.raises(OSError, match="temporary read failure") as exc_info:
            resource.get()
        waiter_errors.append(exc_info.value)

    leader = threading.Thread(target=_expect_oserror, args=(resource,))
    leader.start()
    assert loader_started.wait(timeout=5)

    waiter_threads = [threading.Thread(target=waiter) for _ in range(2)]
    for thread in waiter_threads:
        thread.start()
    release_loader.set()

    leader.join(timeout=5)
    for thread in waiter_threads:
        thread.join(timeout=5)
        assert not thread.is_alive()

    # The leader and both waiters saw the same failure object, preserving
    # its cause/message rather than a generic "resource unavailable".
    assert len(waiter_errors) == 2
    assert all(err is failure for err in waiter_errors)
    assert resource.loaded() is False

    # The failure is not cached: the next call runs the loader again.
    assert resource.get() is recovered
    assert calls == 2
    assert resource.get() is recovered
    assert calls == 2


def _expect_oserror(resource: LazyResource[object]) -> None:
    with pytest.raises(OSError, match="temporary read failure"):
        resource.get()


def test_lazy_resource_repeated_failures_each_retry() -> None:
    """Every call while the loader keeps failing gets its own attempt."""
    attempts = 0

    def always_fails() -> object:
        nonlocal attempts
        attempts += 1
        raise ValueError(f"attempt {attempts}")

    resource: LazyResource[object] = LazyResource(always_fails, name="test")
    for expected in (1, 2, 3):
        with pytest.raises(ValueError, match=f"attempt {expected}"):
            resource.get()
        assert resource.loaded() is False
    assert attempts == 3


def test_lazy_resource_cache_clear_forces_reload() -> None:
    """cache_clear drops the published value and the next get reloads."""
    calls = 0

    def loader() -> object:
        nonlocal calls
        calls += 1
        return object()

    resource: LazyResource[object] = LazyResource(loader, name="test")
    first = resource.get()
    resource.cache_clear()
    assert resource.loaded() is False
    second = resource.get()
    assert second is not first
    assert calls == 2


def test_lazy_resource_cache_clear_releases_waiters_to_retry() -> None:
    """A reset while a load is in flight wakes waiters onto a fresh attempt."""
    enter_loader = threading.Event()
    release_loader = threading.Event()
    loader_started = threading.Event()
    calls = 0
    values = [object(), object()]

    def loader() -> object:
        nonlocal calls
        index = calls
        calls += 1
        if index == 0:
            loader_started.set()
            enter_loader.set()
            assert release_loader.wait(timeout=5)
        return values[index]

    resource: LazyResource[object] = LazyResource(loader, name="test")
    waited: list[object] = []

    def waiter() -> None:
        enter_loader.wait(timeout=5)
        waited.append(resource.get())

    first_attempt = threading.Thread(target=resource.get)
    first_attempt.start()
    assert loader_started.wait(timeout=5)
    waiter_thread = threading.Thread(target=waiter)
    waiter_thread.start()

    # Reset while the first load is still open: the waiter must not accept
    # the stale result once the loader finishes.
    resource.cache_clear()
    release_loader.set()
    first_attempt.join(timeout=5)
    waiter_thread.join(timeout=5)
    assert not waiter_thread.is_alive()

    assert waited == [values[1]]
    assert calls == 2
    assert resource.get() is values[1]


def test_lazy_decorator_keeps_cache_clear() -> None:
    """The lazy decorator preserves the functools.cache-shaped surface."""
    calls = 0

    @lazy(name="decorated")
    def load() -> tuple[int, ...]:
        nonlocal calls
        calls += 1
        return (calls,)

    assert load() == (1,)
    assert load() == (1,)
    load.cache_clear()
    assert load() == (2,)


def test_lazy_decorator_bare_form() -> None:
    """The decorator also works without a keyword name."""

    @lazy
    def load() -> str:
        return "value"

    assert load() == "value"
    assert load() == "value"
    load.cache_clear()
    assert load() == "value"


def test_keyed_cache_loads_each_key_independently() -> None:
    """Different keys get separate resources with separate load counts."""
    calls: dict[str, int] = {}

    def factory(key: str) -> object:
        def loader() -> object:
            calls[key] = calls.get(key, 0) + 1
            return (key, calls[key])

        return loader

    cache: LazyKeyedCache[str, object] = LazyKeyedCache(factory, name="model")

    assert cache.get("a") == ("a", 1)
    assert cache.get("a") == ("a", 1)
    assert cache.get("b") == ("b", 1)
    assert cache.get("b") == ("b", 1)
    assert calls == {"a": 1, "b": 1}
    assert cache.loaded_keys() == frozenset({"a", "b"})


def test_keyed_cache_failure_is_scoped_to_one_key() -> None:
    """A failing key retries independently and never poisons other keys."""
    attempts: dict[str, int] = {}

    def factory(key: str) -> object:
        def loader() -> object:
            attempts[key] = attempts.get(key, 0) + 1
            if key == "bad" and attempts[key] == 1:
                raise OSError(f"{key} unreadable")
            return f"value-{key}-{attempts[key]}"

        return loader

    cache: LazyKeyedCache[str, object] = LazyKeyedCache(factory, name="model")

    with pytest.raises(OSError, match="bad unreadable"):
        cache.get("bad")
    # The good key is unaffected by the other key's failed attempt.
    assert cache.get("good") == "value-good-1"
    # The failed key retries and then succeeds.
    assert cache.get("bad") == "value-bad-2"
    assert cache.get("bad") == "value-bad-2"
    assert cache.loaded_keys() == frozenset({"bad", "good"})


def test_keyed_cache_single_flight_per_key_with_two_waiters() -> None:
    """Two waiters on the same key share one load; another key stays free."""
    enter_loader = threading.Event()
    release_loader = threading.Event()
    loader_started = threading.Event()
    calls: dict[str, int] = {}
    published = object()

    def factory(key: str) -> object:
        def loader() -> object:
            calls[key] = calls.get(key, 0) + 1
            if key == "a":
                loader_started.set()
                enter_loader.set()
                assert release_loader.wait(timeout=5)
                return published
            return f"value-{key}"

        return loader

    cache: LazyKeyedCache[str, object] = LazyKeyedCache(factory, name="model")
    results: list[object] = []

    def waiter() -> None:
        enter_loader.wait(timeout=5)
        results.append(cache.get("a"))

    leader = threading.Thread(target=cache.get, args=("a",))
    leader.start()
    assert loader_started.wait(timeout=5)

    waiters = [threading.Thread(target=waiter) for _ in range(2)]
    for thread in waiters:
        thread.start()
    # While key "a" is blocked, key "b" must load without waiting on it:
    # no global lock wraps the entries.
    assert cache.get("b") == "value-b"
    release_loader.set()

    leader.join(timeout=5)
    for thread in waiters:
        thread.join(timeout=5)
        assert not thread.is_alive()

    assert results == [published, published]
    assert calls == {"a": 1, "b": 1}


def test_keyed_cache_clear_resets_every_entry() -> None:
    """cache_clear drops all keys so the next get rebuilds them."""
    calls: dict[str, int] = {}

    def factory(key: str) -> object:
        def loader() -> object:
            calls[key] = calls.get(key, 0) + 1
            return f"{key}-{calls[key]}"

        return loader

    cache: LazyKeyedCache[str, object] = LazyKeyedCache(factory, name="model")
    cache.get("a")
    cache.get("b")
    cache.cache_clear()
    assert cache.loaded_keys() == frozenset()
    assert cache.get("a") == "a-2"
    assert cache.get("b") == "b-2"


def test_format_error_and_unreadable_file_are_distinct_diagnostics(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The two failure classes the task separates keep distinct exceptions.

    A malformed artifact (parser-level ``ValueError``) and an unreadable
    file (OS-level ``OSError``) are diagnosed separately all the way through
    the lazy boundary, and both remain retryable.  A mock resource handle
    stands in for the file, so no real model artifact is touched.
    """
    reads: list[Exception] = [
        ValueError("corrupt models.bin: missing CMD2 magic"),
        OSError("models.bin: permission denied"),
    ]
    attempts = {"count": 0}
    mock_ref = MagicMock()

    def fake_read_bytes() -> object:
        outcome = reads[attempts["count"]]
        attempts["count"] += 1
        raise outcome

    mock_ref.read_bytes.side_effect = fake_read_bytes
    handle = MagicMock(joinpath=MagicMock(return_value=mock_ref))

    models_mod._load_models_data.cache_clear()
    try:
        monkeypatch.setattr(
            models_mod.importlib.resources, "files", MagicMock(return_value=handle)
        )

        with pytest.raises(ValueError, match="corrupt models.bin"):
            models_mod._load_models_data()
        with pytest.raises(OSError, match="permission denied"):
            models_mod._load_models_data()

        # After the two transient failures, a valid payload loads normally:
        # neither diagnosis was cached as a permanent state.  Built with the
        # real format writer so parse_models_bin accepts it.
        name = b"fr/cp1252"
        table = bytes(65536)
        header = b"CMD2" + struct.pack("!I", 1)
        header += struct.pack("!I", len(name)) + name + struct.pack("!d", 0.0)
        valid = header + zlib.compress(table, 9)

        mock_ref.read_bytes.side_effect = None
        mock_ref.read_bytes.return_value = valid
        models, norms = models_mod._load_models_data()
        assert set(models) == {"fr/cp1252"}
        assert "fr/cp1252" in norms
    finally:
        models_mod._load_models_data.cache_clear()
