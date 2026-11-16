"""Thread-safe publication boundaries for lazily loaded resources.

``functools.cache`` makes a loader callable cheap after its first success,
but its publication happens when the wrapped call *returns*: under threads,
several callers that miss the cache together each run the whole loader
themselves, and a loader that raises stores nothing while callers already
waiting inside the cache's own lock observe no defined hand-off.  The
primitives here give model/artifact loading an explicit contract instead:

* **Single in-flight load per resource.**  The first caller for an
  unloaded resource runs the loader; concurrent callers for the *same*
  resource block until that load finishes and then share its result.
  Different resources carry independent locks, so one model finishing its
  disk read never waits behind another model's read --- and scoring code
  never takes a global lock at all.
* **Immutable results only, published once complete.**  A value enters the
  cache only after the loader returns, so no caller can observe a half-built
  table or a dict still being populated.  Every later caller receives that
  same object and is expected to treat it as read-only.
* **Failures are remembered only until the next attempt.**  The exception
  is kept with its traceback so every waiter gets the same diagnostic, but
  the entry returns to the unloaded state once the waiters have been
  released; the next call retries the loader instead of inheriting one
  transient ``OSError`` forever.

The module is deliberately plain interpreted Python: it owns no scoring
loops, and keeping it out of the mypyc set lets it use
:pep:`563` annotations like the rest of the non-compiled package.
"""

from __future__ import annotations

import enum
import threading
from collections.abc import Callable, Hashable
from typing import Generic, TypeVar, overload

_T = TypeVar("_T")
_K = TypeVar("_K", bound=Hashable)


class _State(enum.Enum):
    """Lifecycle of one lazy resource entry."""

    EMPTY = enum.auto()
    LOADING = enum.auto()
    READY = enum.auto()


class LazyResource(Generic[_T]):
    """A lazily loaded, thread-safe single value.

    The wrapped *loader* runs at most once at a time.  On success its
    return value is published immutably to every caller; on failure the
    exception is re-raised to every waiter and dropped afterwards, so the
    following :meth:`get` call starts a fresh attempt.

    :param loader: Zero-argument callable that produces the resource.  It
        must return a fully built, effectively immutable object.
    :param name: Human-readable name used in error messages while waiting
        on a load that fails, e.g. the artifact the loader reads.
    """

    __slots__ = (
        "_attempt",
        "_condition",
        "_failure",
        "_failure_attempt",
        "_loader",
        "_name",
        "_state",
        "_value",
        "_waiters",
    )

    def __init__(self, loader: Callable[[], _T], *, name: str = "resource") -> None:
        self._loader = loader
        self._name = name
        self._condition = threading.Condition()
        self._state = _State.EMPTY
        self._value: _T | None = None
        # When a load fails, the exception is parked here long enough for the
        # threads already waiting on it to receive it; the leader itself
        # re-raises without parking anything permanently, so an unobserved
        # failure leaves the entry EMPTY and retryable.
        self._failure: BaseException | None = None
        # The load attempt that produced the parked failure.  A waiter only
        # consumes the failure of the attempt it slept through; a thread
        # arriving for a later attempt sees the entry EMPTY and starts over.
        self._failure_attempt = -1
        # Bumped every time a thread starts a load, including retries.
        self._attempt = 0
        # Threads currently parked inside get() while a load is in flight.
        # Maintained explicitly so the leader can tell whether anyone needs
        # the failure parked, without touching the lock's private internals.
        self._waiters = 0

    def get(self) -> _T:
        """Return the loaded resource, loading it on first use.

        Concurrent callers collapse onto one loader invocation and all
        observe the same outcome.  A raised exception is propagated to
        every waiter but is not cached: the next call retries.
        """
        with self._condition:
            attempt = 0
            while True:
                state = self._state
                if state is _State.READY:
                    return self._value  # type: ignore[return-value]
                if state is _State.LOADING:
                    waited_attempt = self._attempt
                    self._waiters += 1
                    try:
                        self._condition.wait()
                    finally:
                        self._waiters -= 1
                    failure = self._failure
                    if (
                        failure is not None
                        and self._failure_attempt == waited_attempt
                        and self._state is _State.EMPTY
                    ):
                        raise failure
                    # A successful publish, a reset, or a newer attempt now
                    # owns the entry: re-evaluate from the top.
                    continue
                # EMPTY: this thread owns the load.
                self._attempt += 1
                attempt = self._attempt
                self._state = _State.LOADING
                self._failure = None
                break

        # The loader runs outside the condition's lock: blocking on disk I/O
        # here must not stop other threads from reading their own resources.
        try:
            result = self._loader()
        except BaseException as exc:  # noqa: BLE001 - re-raised, never swallowed
            with self._condition:
                # The entry returns to EMPTY immediately: the failure is a
                # one-attempt diagnostic, never a permanent state.  Waiters
                # still identify it via the attempt number they slept on.
                self._state = _State.EMPTY
                if self._waiters:
                    self._failure = exc
                    self._failure_attempt = attempt
                self._condition.notify_all()
            raise
        with self._condition:
            if self._attempt == attempt:
                self._value = result
                self._state = _State.READY
                self._condition.notify_all()
                return result
            # A cache_clear() raced our successful load: discard the value
            # rather than publishing it into an entry that already moved on,
            # and let the caller retry against the fresh state.
            return self.get()

    def cache_clear(self) -> None:
        """Reset the entry to its unloaded state.

        Threads blocked in :meth:`get` wake and retry instead of consuming a
        value or failure from the load they slept through.
        """
        with self._condition:
            self._state = _State.EMPTY
            self._value = None
            self._failure = None
            self._failure_attempt = -1
            self._attempt += 1
            self._condition.notify_all()

    def loaded(self) -> bool:
        """Return whether a successful value is currently published."""
        with self._condition:
            return self._state is _State.READY

    def __repr__(self) -> str:
        """Show the resource name and lifecycle state for stalled-thread debugging."""
        with self._condition:
            return f"<LazyResource {self._name!r} state={self._state.name.lower()}>"


class LazyKeyedCache(Generic[_K, _T]):
    """Independent :class:`LazyResource` entries keyed by hashable *key*.

    Each key gets its own lock and load: two models being read at once never
    serialize against each other, and a failure loading one key leaves every
    other key untouched.  The short guard lock only protects the entry map;
    it is never held while a loader runs, so a slow disk read cannot block
    callers asking for a different key.

    :param loader_factory: Given a key, returns the zero-argument loader for
        that key's resource.
    :param name: Name prefix for diagnostics on the per-key entries.
    """

    __slots__ = ("_entries", "_guard", "_loader_factory", "_name")

    def __init__(
        self,
        loader_factory: Callable[[_K], Callable[[], _T]],
        *,
        name: str = "resource",
    ) -> None:
        self._loader_factory = loader_factory
        self._name = name
        self._guard = threading.Lock()
        self._entries: dict[_K, LazyResource[_T]] = {}

    def get(self, key: _K) -> _T:
        """Return the resource for *key*, loading it once on first use."""
        with self._guard:
            entry = self._entries.get(key)
            if entry is None:
                entry = LazyResource(
                    self._loader_factory(key), name=f"{self._name}[{key!r}]"
                )
                self._entries[key] = entry
        # No guard held: a slow load for this key cannot stall other keys.
        return entry.get()

    def cache_clear(self) -> None:
        """Drop every entry; in-flight loads finish but are not republished."""
        with self._guard:
            entries = list(self._entries.values())
            self._entries.clear()
        for entry in entries:
            entry.cache_clear()

    def loaded_keys(self) -> frozenset[_K]:
        """Return the keys whose resources are currently published."""
        with self._guard:
            return frozenset(
                key for key, entry in self._entries.items() if entry.loaded()
            )


class _LazyCallable(Generic[_T]):
    """Callable facade over a shared :class:`LazyResource`.

    Exists so the wrapped loader keeps the ``cache_clear`` attribute tests
    and callers expect from a ``functools.cache`` function while the state
    machine itself stays in :class:`LazyResource`.
    """

    __slots__ = ("resource",)

    def __init__(self, resource: LazyResource[_T]) -> None:
        self.resource = resource

    def __call__(self) -> _T:
        """Return the lazily loaded resource, loading it on first use."""
        return self.resource.get()

    def cache_clear(self) -> None:
        """Reset the underlying resource to its unloaded state."""
        self.resource.cache_clear()


@overload
def lazy(loader: Callable[[], _T]) -> _LazyCallable[_T]: ...


@overload
def lazy(*, name: str = ...) -> Callable[[Callable[[], _T]], _LazyCallable[_T]]: ...


def lazy(
    loader: Callable[[], _T] | None = None,
    *,
    name: str = "resource",
) -> _LazyCallable[_T] | Callable[[Callable[[], _T]], _LazyCallable[_T]]:
    """Wrap a zero-argument *loader* in a shared :class:`LazyResource`.

    Usable bare (``@lazy``) or with a diagnostic name
    (``@lazy(name="models.bin")``).  The returned object is callable with no
    arguments and keeps a ``cache_clear`` method, so it reads as a drop-in
    replacement for a ``functools.cache``-decorated loader.
    """

    def wrap(actual_loader: Callable[[], _T]) -> _LazyCallable[_T]:
        return _LazyCallable(LazyResource(actual_loader, name=name))

    if loader is None:
        return wrap
    return wrap(loader)
