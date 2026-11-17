"""Threadsafe pure-Python implementation of the Circuit Breaker pattern, described
by Michael T. Nygard in his book 'Release It!'.

For more information on this and other patterns and best practices, buy the
book at https://pragprog.com/titles/mnee2/release-it-second-edition/
"""

from __future__ import annotations

import calendar
import contextlib
import logging
import sys
import threading
import time
import types
from abc import abstractmethod
from dataclasses import dataclass
from datetime import datetime, timedelta
from functools import wraps
from typing import (
    TYPE_CHECKING,
    Any,
    Callable,
    Literal,
    NoReturn,
    TypeVar,
    Union,
    cast,
    overload,
)

if TYPE_CHECKING:
    from collections.abc import Generator, Iterable, Sequence

# For compatibility with Python 3.10 and earlier.
# Otherwise, `from datetime import UTC` would suffice.
try:
    from datetime import UTC  # type: ignore[attr-defined]
except ImportError:
    from datetime import timezone

    UTC = timezone.utc

try:
    from tornado import gen

    HAS_TORNADO_SUPPORT = True
except ImportError:
    HAS_TORNADO_SUPPORT = False

try:
    from redis import Redis
    from redis.client import Pipeline
    from redis.exceptions import RedisError

    HAS_REDIS_SUPPORT = True
except ImportError:
    HAS_REDIS_SUPPORT = False

__all__ = (
    "CircuitBreaker",
    "CircuitBreakerListener",
    "CircuitBreakerError",
    "CircuitBreakerStorage",
    "CircuitMemoryStorage",
    "CircuitRedisStorage",
    "CircuitBreakerSnapshot",
    "CircuitBreakerStorageError",
    "CircuitBreakerStorageVersionConflictError",
    "CircuitBreakerStorageUnsupportedError",
    "MAX_CONDITIONAL_ATTEMPTS",
    "STATE_OPEN",
    "STATE_CLOSED",
    "STATE_HALF_OPEN",
)

STATE_OPEN = "open"
STATE_CLOSED = "closed"
STATE_HALF_OPEN = "half-open"

# Maximum number of times a version-conditional (CAS) storage operation is
# retried before giving up on an apparently contended storage backend.
MAX_CONDITIONAL_ATTEMPTS = 10


class CircuitBreakerStorageError(Exception):
    """Base class for errors raised by circuit breaker state storage.

    These errors describe problems with the storage itself, and are therefore
    distinct from exceptions raised by the guarded (dependency) calls.
    """


class CircuitBreakerStorageVersionConflictError(CircuitBreakerStorageError):
    """Raised when a version-conditional update cannot be applied because the
    shared state was modified by another breaker instance (or process) after
    the expected version was read.

    The stale snapshot is attached so callers can inspect or re-read the
    current state before retrying.
    """

    def __init__(self, storage_name: str, expected_version: int) -> None:
        """Create the conflict error for `storage_name` at `expected_version`."""
        self.storage_name = storage_name
        self.expected_version = expected_version
        super().__init__(f"Version conflict on storage {storage_name!r}: state no longer at version {expected_version}")


class CircuitBreakerStorageUnsupportedError(CircuitBreakerStorageError):
    """Raised when a storage backend does not support a requested operation,
    e.g. version-conditional updates on a backend that cannot guarantee them.

    This makes it impossible to silently fall back to a read-then-write
    sequence that would overwrite concurrent updates.
    """


@dataclass(frozen=True)
class CircuitBreakerSnapshot:
    """Immutable, consistent view of a circuit breaker's state.

    The probe round (`state`), the moment the circuit was opened
    (`opened_at`) and the current-round counters are read together so that a
    decision is never made on a mixture of different storage versions. The
    `version` is an opaque token that changes whenever the stored state
    changes; it is used by version-conditional updates.
    """

    state: str
    fail_counter: int
    success_counter: int
    opened_at: datetime | None
    version: int


T = TypeVar("T")
ExceptionType = TypeVar("ExceptionType", bound=BaseException)
CBListenerType = TypeVar("CBListenerType", bound="CircuitBreakerListener")
CBStateType = Union["CircuitClosedState", "CircuitHalfOpenState", "CircuitOpenState"]


class CircuitBreaker:
    """More abstractly, circuit breakers exists to allow one subsystem to fail
    without destroying the entire system.

    This is done by wrapping dangerous operations (typically integration points)
    with a component that can circumvent calls when the system is not healthy.

    This pattern is described by Michael T. Nygard in his book 'Release It!'.
    """

    def __init__(
        self,
        fail_max: int = 5,
        reset_timeout: float = 60,
        success_threshold: int = 1,
        exclude: Iterable[type[ExceptionType] | Callable[[Any], bool]] | None = None,
        listeners: Sequence[CBListenerType] | None = None,
        state_storage: CircuitBreakerStorage | None = None,
        name: str | None = None,
        throw_new_error_on_trip: bool = True,
        *,
        conditional_updates: bool = False,
    ) -> None:
        """Create a new circuit breaker with the given parameters.

        When `conditional_updates` is ``True``, every state transition and
        current-round counter update is performed as a single
        version-conditional storage operation (with retry on conflict). This
        is required when several breaker instances share the same storage;
        separate read-then-write steps would otherwise overwrite each other's
        updates. The storage backend must report
        ``supports_conditional_updates`` as ``True``, otherwise a
        `CircuitBreakerStorageUnsupportedError` error is raised rather than
        silently pretending the local lock provides cross-process safety.
        """
        self._lock = threading.RLock()
        self._state_storage = state_storage or CircuitMemoryStorage(STATE_CLOSED)
        self._conditional_updates = conditional_updates
        if self._conditional_updates and not self._state_storage.supports_conditional_updates:
            message = (
                f"Storage {self._state_storage.name!r} does not support version-conditional "
                "updates; cannot safely share its state between breaker instances"
            )
            raise CircuitBreakerStorageUnsupportedError(message)

        self._state = self._create_new_state(self.current_state)
        self._latest_snapshot: CircuitBreakerSnapshot | None = None

        self._fail_max = fail_max
        self._reset_timeout = reset_timeout
        self._success_threshold = success_threshold

        self._excluded_exceptions = list(exclude or [])
        self._listeners = list(listeners or [])
        self._name = name

        self._throw_new_error_on_trip = throw_new_error_on_trip

    @property
    def conditional_updates(self) -> bool:
        """Return whether this breaker drives storage through version-conditional
        (compare-and-swap) operations instead of separate read-then-write steps.
        """
        return self._conditional_updates

    @property
    def fail_counter(self) -> int:
        """Return the current number of consecutive failures."""
        return self._state_storage.counter

    @property
    def success_counter(self) -> int:
        """Return the current number of consecutive successes in half-open state."""
        return self._state_storage.success_counter

    @property
    def fail_max(self) -> int:
        """Return the maximum number of failures tolerated before the circuit is opened."""
        return self._fail_max

    @fail_max.setter
    def fail_max(self, number: int) -> None:
        """Set the maximum `number` of failures tolerated before the circuit is opened."""
        self._fail_max = number

    @property
    def reset_timeout(self) -> float:
        """Once this circuit breaker is opened, it should remain opened until the
        timeout period, in seconds, elapses.
        """
        return self._reset_timeout

    @reset_timeout.setter
    def reset_timeout(self, timeout: float) -> None:
        """Set the `timeout` period, in seconds, this circuit breaker should be kept open."""
        self._reset_timeout = timeout

    @property
    def success_threshold(self) -> int:
        """Return the number of successful requests required before transitioning from half-open to closed state."""
        return self._success_threshold

    @success_threshold.setter
    def success_threshold(self, threshold: int) -> None:
        """Set the number of successful requests required before transitioning from half-open to closed state."""
        self._success_threshold = threshold

    def _create_new_state(
        self,
        new_state: str,
        prev_state: CircuitBreakerState | None = None,
        notify: bool = False,
    ) -> CBStateType:
        """Return state object from state string, i.e., 'closed' -> <CircuitClosedState>."""
        state_map: dict[str, type[CBStateType]] = {
            STATE_CLOSED: CircuitClosedState,
            STATE_OPEN: CircuitOpenState,
            STATE_HALF_OPEN: CircuitHalfOpenState,
        }
        try:
            cls = state_map[new_state]
            return cls(self, prev_state=prev_state, notify=notify)
        except KeyError as e:
            msg = "Unknown state {!r}, valid states: {}"
            raise ValueError(msg.format(new_state, ", ".join(state_map))) from e

    @property
    def state(self) -> CBStateType:
        """Update (if needed) and returns the cached state object."""
        # Ensure cached state is up-to-date
        if self.current_state != self._state.name:
            # If cached state is out-of-date, that means that it was likely
            # changed elsewhere (e.g. another process instance). In the
            # conditional mode we refresh the cached state without running
            # the notify side effects (counter resets), since the storage is
            # the single source of truth and those resets would themselves
            # mutate the shared state and bump its version. We still send out
            # a notification, informing others that this particular circuit
            # breaker instance noticed the changed circuit.
            if self._conditional_updates:
                self._sync_cached_state()
            else:
                self.state = self.current_state  # type: ignore[assignment]
        return self._state

    @state.setter
    def state(self, state_str: str) -> None:
        """Set cached state and notify listeners of newly cached state."""
        with self._lock:
            self._state = self._create_new_state(state_str, prev_state=self._state, notify=True)

    @property
    def current_state(self) -> str:
        """Return a string that identifies the state of the circuit breaker as
        reported by the _state_storage. i.e., 'closed', 'open', 'half-open'.
        """
        return self._state_storage.state

    @property
    def excluded_exceptions(
        self,
    ) -> tuple[type[ExceptionType] | Callable[[Any], bool], ...]:
        """Return the list of excluded exceptions, e.g., exceptions that should
        not be considered system errors by this circuit breaker.
        """
        return tuple(self._excluded_exceptions)

    def add_excluded_exception(self, exception: type[ExceptionType]) -> None:
        """Add an exception to the list of excluded exceptions."""
        with self._lock:
            self._excluded_exceptions.append(exception)

    def add_excluded_exceptions(self, *exceptions: type[ExceptionType]) -> None:
        """Add exceptions to the list of excluded exceptions."""
        for exc in exceptions:
            self.add_excluded_exception(exc)

    def remove_excluded_exception(self, exception: type[ExceptionType]) -> None:
        """Remove an exception from the list of excluded exceptions."""
        with self._lock:
            self._excluded_exceptions.remove(exception)

    def _inc_counter(self) -> None:
        """Increment the counter of failed calls."""
        if self._conditional_updates:
            self._latest_snapshot = self._state_storage.conditional_increment_counter(self._require_snapshot().version)
        else:
            self._state_storage.increment_counter()

    def _current_snapshot(self) -> CircuitBreakerSnapshot:
        """Read the whole circuit state (probe round, opened moment and the
        current-round counters) as a single consistent snapshot.
        """
        snapshot = self._state_storage.get_snapshot()
        self._latest_snapshot = snapshot
        return snapshot

    def _require_snapshot(self) -> CircuitBreakerSnapshot:
        """Return the most recent snapshot, reading one if none is cached."""
        if self._latest_snapshot is None:
            return self._current_snapshot()
        return self._latest_snapshot

    def _run_conditional(
        self, operation: Callable[[CircuitBreakerSnapshot], CircuitBreakerSnapshot]
    ) -> CircuitBreakerSnapshot:
        """Run a version-conditional storage `operation`, re-reading the
        consistent snapshot and retrying while another instance wins the
        version race. Each attempt applies `operation` to the latest
        snapshot, so a retry never double-applies an already committed
        counter update. A storage error other than a version conflict is
        reported as-is: no partial round is committed and the guarded call's
        outcome remains separate from the storage failure.
        """
        snapshot = self._current_snapshot()
        for _ in range(MAX_CONDITIONAL_ATTEMPTS):
            try:
                new_snapshot = operation(snapshot)
            except CircuitBreakerStorageVersionConflictError:
                # Another breaker instance moved the shared state first;
                # re-read and re-apply our decision against the new version.
                snapshot = self._current_snapshot()
                continue
            self._latest_snapshot = new_snapshot
            return new_snapshot
        raise CircuitBreakerStorageVersionConflictError(self._state_storage.name, snapshot.version)

    def _adopt_snapshot(self, snapshot: CircuitBreakerSnapshot) -> None:
        """Refresh the cached state object from `snapshot` without triggering
        the state-construction side effects (counter resets), and notify
        listeners if the probe round actually changed.
        """
        with self._lock:
            self._latest_snapshot = snapshot
            if snapshot.state != self._state.name:
                prev_state = self._state
                # notify=False: in conditional mode the storage is the single
                # source of truth for counters, so constructing the state
                # object must not reset anything.
                self._state = self._create_new_state(snapshot.state, prev_state=prev_state, notify=False)
                for listener in self._listeners:
                    listener.state_change(self, prev_state, self._state)

    def _sync_cached_state(self) -> None:
        """Re-read the shared storage and adopt its latest snapshot."""
        with self._lock:
            self._adopt_snapshot(self._current_snapshot())

    def _record_failure_round(self) -> CircuitBreakerSnapshot:
        """Atomically account for one failed call.

        The failure counter increment and the possible state transition to
        'open' (together with its opened moment and success-counter reset)
        are committed as one version-conditional operation, so a failure can
        never leave a half-written round behind. This covers both a threshold
        trip in 'closed' state and a failed trial call in 'half-open' state.
        """

        def apply(snapshot: CircuitBreakerSnapshot) -> CircuitBreakerSnapshot:
            new_fail_counter = snapshot.fail_counter + 1
            if snapshot.state == STATE_CLOSED and new_fail_counter >= self._fail_max:
                return self._state_storage.compare_and_swap_state(
                    snapshot.version,
                    STATE_OPEN,
                    opened_at=datetime.now(UTC),
                    fail_counter=new_fail_counter,
                    success_counter=0,
                )
            if snapshot.state == STATE_HALF_OPEN:
                return self._state_storage.compare_and_swap_state(
                    snapshot.version,
                    STATE_OPEN,
                    opened_at=datetime.now(UTC),
                    fail_counter=new_fail_counter,
                    success_counter=0,
                )
            return self._state_storage.conditional_increment_counter(snapshot.version)

        snapshot = self._run_conditional(apply)
        self._adopt_snapshot(snapshot)
        return snapshot

    def _record_success_round(self) -> CircuitBreakerSnapshot:
        """Atomically account for one successful call.

        In 'half-open' the success counter is incremented (and the stale
        failure counter cleared) in one operation; if the success threshold
        is reached, the transition back to 'closed' is committed in the very
        same compare-and-swap.
        """

        def apply(snapshot: CircuitBreakerSnapshot) -> CircuitBreakerSnapshot:
            if snapshot.state == STATE_HALF_OPEN:
                new_success_counter = snapshot.success_counter + 1
                if new_success_counter >= self._success_threshold:
                    return self._state_storage.compare_and_swap_state(
                        snapshot.version,
                        STATE_CLOSED,
                        opened_at=None,
                        fail_counter=0,
                        success_counter=0,
                    )
                return self._state_storage.conditional_increment_success_counter(
                    snapshot.version, reset_fail_counter=True
                )
            return self._state_storage.conditional_reset_counter(snapshot.version)

        snapshot = self._run_conditional(apply)
        self._adopt_snapshot(snapshot)
        return snapshot

    def _enter_half_open_if_due(self) -> None:
        """Move an expired 'open' circuit to 'half-open' using a
        version-conditional transition that also resets the success counter,
        retrying if another instance transitioned first.
        """

        def apply(snapshot: CircuitBreakerSnapshot) -> CircuitBreakerSnapshot:
            if snapshot.state == STATE_OPEN:
                return self._state_storage.compare_and_swap_state(
                    snapshot.version,
                    STATE_HALF_OPEN,
                    opened_at=snapshot.opened_at,
                    fail_counter=snapshot.fail_counter,
                    success_counter=0,
                )
            return snapshot

        snapshot = self._run_conditional(apply)
        self._adopt_snapshot(snapshot)

    def is_system_error(self, exception: ExceptionType) -> bool:
        """Return whether the exception `exception` is considered a signal of
        system malfunction. Business exceptions should not cause this circuit
        breaker to open.
        """
        exception_type = type(exception)
        for exclusion in self._excluded_exceptions:
            if type(exclusion) is type:
                if issubclass(exception_type, exclusion):
                    return False
            elif callable(exclusion):
                if exclusion(exception):
                    return False
        return True

    def call(self, func: Callable[..., T], *args: Any, **kwargs: Any) -> T:
        """Call `func` with the given `args` and `kwargs` according to the rules
        implemented by the current state of this circuit breaker.
        """
        with self._lock:
            return self.state.call(func, *args, **kwargs)

    @contextlib.contextmanager
    def calling(self) -> Any:
        """Return a context manager, enabling the circuit breaker to be used with a
        `with` statement. The block of code inside the `with` statement will be
        executed according to the rules implemented by the current state of this
        circuit breaker.
        """

        def _wrapper() -> Generator:
            yield

        yield from self.call(_wrapper)

    def call_async(self, func, *args, **kwargs):  # type: ignore[no-untyped-def]
        """Call async `func` with the given `args` and `kwargs` according to the rules
        implemented by the current state of this circuit breaker.

        Return a closure to prevent import errors when using without tornado present
        """

        @gen.coroutine
        def wrapped():  # type: ignore[no-untyped-def]
            with self._lock:
                ret = yield self.state.call_async(func, *args, **kwargs)
                raise gen.Return(ret)

        return wrapped()

    def open(self) -> bool:
        """Open the circuit, e.g., the following calls will immediately fail until timeout elapses."""
        with self._lock:
            if self._conditional_updates:
                snapshot = self._run_conditional(
                    lambda snapshot: self._state_storage.compare_and_swap_state(
                        snapshot.version,
                        STATE_OPEN,
                        opened_at=datetime.now(UTC),
                        fail_counter=snapshot.fail_counter,
                        success_counter=0,
                    )
                )
                self._adopt_snapshot(snapshot)
            else:
                self._state_storage.opened_at = datetime.now(UTC)
                self.state = self._state_storage.state = STATE_OPEN  # type: ignore[assignment]

            return self._throw_new_error_on_trip

    def half_open(self) -> None:
        """Half-open the circuit, e.g. lets the following call pass through and
        opens the circuit if the call fails (or closes the circuit if the call
        succeeds).
        """
        with self._lock:
            if self._conditional_updates:
                snapshot = self._run_conditional(
                    lambda snapshot: self._state_storage.compare_and_swap_state(
                        snapshot.version,
                        STATE_HALF_OPEN,
                        opened_at=snapshot.opened_at,
                        fail_counter=snapshot.fail_counter,
                        success_counter=0,
                    )
                )
                self._adopt_snapshot(snapshot)
            else:
                self.state = self._state_storage.state = STATE_HALF_OPEN  # type: ignore[assignment]

    def close(self) -> None:
        """Close the circuit, e.g. lets the following calls execute as usual."""
        with self._lock:
            if self._conditional_updates:
                snapshot = self._run_conditional(
                    lambda snapshot: self._state_storage.compare_and_swap_state(
                        snapshot.version,
                        STATE_CLOSED,
                        opened_at=None,
                        fail_counter=0,
                        success_counter=0,
                    )
                )
                self._adopt_snapshot(snapshot)
            else:
                self._state_storage.reset_success_counter()  # Reset success counter when closing
                self.state = self._state_storage.state = STATE_CLOSED  # type: ignore[assignment]

    def __call__(self, *call_args: Any, **call_kwargs: bool) -> Callable:
        """Return a wrapper that calls the function `func` according to the rules
        implemented by the current state of this circuit breaker.

        Optionally takes the keyword argument `__pybreaker_call_coroutine`,
        which will will call `func` as a Tornado co-routine.
        """
        call_async = call_kwargs.pop("__pybreaker_call_async", False)

        if call_async and not HAS_TORNADO_SUPPORT:
            message = "No module named tornado"
            raise ImportError(message)

        def _outer_wrapper(func):  # type: ignore[no-untyped-def]
            @wraps(func)
            def _inner_wrapper(*args, **kwargs):  # type: ignore[no-untyped-def]
                if call_async:
                    return self.call_async(func, *args, **kwargs)
                return self.call(func, *args, **kwargs)

            return _inner_wrapper

        if call_args:
            return _outer_wrapper(*call_args)
        return _outer_wrapper

    @property
    def listeners(self) -> tuple[CBListenerType, ...]:
        """Return the registered listeners as a tuple."""
        return tuple(self._listeners)  # type: ignore[arg-type]

    def add_listener(self, listener: CBListenerType) -> None:
        """Register a listener for this circuit breaker."""
        with self._lock:
            self._listeners.append(listener)  # type: ignore[arg-type]

    def add_listeners(self, *listeners: CBListenerType) -> None:
        """Register listeners for this circuit breaker."""
        for listener in listeners:
            self.add_listener(listener)

    def remove_listener(self, listener: CBListenerType) -> None:
        """Unregister a listener of this circuit breaker."""
        with self._lock:
            self._listeners.remove(listener)  # type: ignore[arg-type]

    @property
    def name(self) -> str | None:
        """Return the name of this circuit breaker. Useful for logging."""
        return self._name

    @name.setter
    def name(self, name: str) -> None:
        """Set the name of this circuit breaker."""
        self._name = name


class CircuitBreakerStorage:
    """Define the underlying storage for a circuit breaker - the underlying
    implementation should be in a subclass that overrides the method this
    class defines.
    """

    def __init__(self, name: str) -> None:
        """Create a new instance identified by `name`."""
        self._name = name

    @property
    def name(self) -> str:
        """Return a human friendly name that identifies this state."""
        return self._name

    @property
    def supports_conditional_updates(self) -> bool:
        """Return whether this storage can perform version-conditional
        (compare-and-swap) updates. Only backends able to atomically check a
        version and commit a new consistent state should return ``True``.
        Remote backends that cannot provide this guarantee must return
        ``False`` so that the conditional sharing mode is rejected explicitly
        instead of silently corrupting shared state.
        """
        return False

    def get_snapshot(self) -> CircuitBreakerSnapshot:
        """Return a consistent snapshot of the full circuit state: the probe
        round (state), the opened moment and both counters, together with the
        version token required by the conditional update methods.
        """
        raise CircuitBreakerStorageUnsupportedError(f"Storage {self._name!r} does not implement get_snapshot()")

    def compare_and_swap_state(
        self,
        expected_version: int,
        new_state: str,
        opened_at: datetime | None,
        fail_counter: int,
        success_counter: int,
    ) -> CircuitBreakerSnapshot:
        """Atomically transition the circuit state, but only if the stored
        version still equals `expected_version`. The opened moment and both
        current-round counters are committed together with the new state, so
        a transition can never leave half-round data behind. Return the
        resulting snapshot; raise `CircuitBreakerStorageVersionConflictError` if
        the version moved, or `CircuitBreakerStorageUnsupportedError` if the
        backend cannot perform conditional updates.
        """
        raise CircuitBreakerStorageUnsupportedError(
            f"Storage {self._name!r} cannot apply transition to {new_state!r} at version "
            f"{expected_version} (fail_counter={fail_counter}, success_counter={success_counter}, "
            f"opened_at={opened_at!r}): version-conditional state transitions are unsupported"
        )

    def conditional_increment_counter(self, expected_version: int) -> CircuitBreakerSnapshot:
        """Atomically increment the failure counter conditional on
        `expected_version`, returning the resulting consistent snapshot.
        """
        raise CircuitBreakerStorageUnsupportedError(
            f"Storage {self._name!r} cannot increment the failure counter at version "
            f"{expected_version}: version-conditional counter updates are unsupported"
        )

    def conditional_increment_success_counter(
        self, expected_version: int, reset_fail_counter: bool = False
    ) -> CircuitBreakerSnapshot:
        """Atomically increment the success counter conditional on
        `expected_version`, optionally clearing the failure counter in the
        same operation, returning the resulting consistent snapshot.
        """
        raise CircuitBreakerStorageUnsupportedError(
            f"Storage {self._name!r} cannot increment the success counter at version "
            f"{expected_version} (reset_fail_counter={reset_fail_counter}): "
            "version-conditional counter updates are unsupported"
        )

    def conditional_reset_counter(self, expected_version: int) -> CircuitBreakerSnapshot:
        """Atomically reset the failure counter conditional on
        `expected_version`, returning the resulting consistent snapshot.
        """
        raise CircuitBreakerStorageUnsupportedError(
            f"Storage {self._name!r} cannot reset the failure counter at version "
            f"{expected_version}: version-conditional counter updates are unsupported"
        )

    @property
    @abstractmethod
    def state(self) -> str:
        """Override this method to retrieve the current circuit breaker state."""

    @state.setter
    def state(self, state: str) -> None:
        """Override this method to set the current circuit breaker state."""

    def increment_counter(self) -> None:
        """Override this method to increase the failure counter by one."""

    def reset_counter(self) -> None:
        """Override this method to set the failure counter to zero."""

    def increment_success_counter(self) -> None:
        """Override this method to increase the success counter by one."""

    def reset_success_counter(self) -> None:
        """Override this method to set the success counter to zero."""

    @property
    @abstractmethod
    def counter(self) -> int:
        """Override this method to retrieve the current value of the failure counter."""

    @property
    @abstractmethod
    def success_counter(self) -> int:
        """Override this method to retrieve the current value of the success counter."""

    @property
    @abstractmethod
    def opened_at(self) -> datetime | None:
        """Override this method to retrieve the most recent value of when the circuit was opened."""

    @opened_at.setter
    def opened_at(self, datetime: datetime) -> None:
        """Override this method to set the most recent value of when the circuit was opened."""


class CircuitMemoryStorage(CircuitBreakerStorage):
    """Implement a `CircuitBreakerStorage` in local memory.

    A reentrant lock serializes every read and write within this process, and
    a monotonically increasing `version` enables optimistic,
    version-conditional updates when several `CircuitBreaker` instances share
    the same storage object. Note that this lock only protects threads inside
    one process; it cannot coordinate separate processes, for which a storage
    with real compare-and-swap semantics would be required.
    """

    def __init__(self, state: str) -> None:
        """Create a new instance with the given `state`."""
        super().__init__("memory")
        self._lock = threading.RLock()
        self._fail_counter = 0
        self._success_counter = 0
        self._opened_at: datetime | None = None
        self._state = state
        self._version = 0

    def _snapshot_locked(self) -> CircuitBreakerSnapshot:
        return CircuitBreakerSnapshot(
            state=self._state,
            fail_counter=self._fail_counter,
            success_counter=self._success_counter,
            opened_at=self._opened_at,
            version=self._version,
        )

    def _check_version_locked(self, expected_version: int) -> None:
        if self._version != expected_version:
            raise CircuitBreakerStorageVersionConflictError(self._name, expected_version)

    @property
    def supports_conditional_updates(self) -> bool:
        """Memory storage fully supports version-conditional updates."""
        return True

    def get_snapshot(self) -> CircuitBreakerSnapshot:
        """Return a consistent snapshot of state, counters, opened moment and version."""
        with self._lock:
            return self._snapshot_locked()

    def compare_and_swap_state(
        self,
        expected_version: int,
        new_state: str,
        opened_at: datetime | None,
        fail_counter: int,
        success_counter: int,
    ) -> CircuitBreakerSnapshot:
        """Commit the new state together with its opened moment and counters
        in one locked, version-conditional step.
        """
        with self._lock:
            self._check_version_locked(expected_version)
            self._state = new_state
            self._opened_at = opened_at
            self._fail_counter = fail_counter
            self._success_counter = success_counter
            self._version += 1
            return self._snapshot_locked()

    def conditional_increment_counter(self, expected_version: int) -> CircuitBreakerSnapshot:
        """Conditionally increment the failure counter in one locked step."""
        with self._lock:
            self._check_version_locked(expected_version)
            self._fail_counter += 1
            self._version += 1
            return self._snapshot_locked()

    def conditional_increment_success_counter(
        self, expected_version: int, reset_fail_counter: bool = False
    ) -> CircuitBreakerSnapshot:
        """Conditionally increment the success counter, optionally clearing
        the failure counter, in one locked step.
        """
        with self._lock:
            self._check_version_locked(expected_version)
            self._success_counter += 1
            if reset_fail_counter:
                self._fail_counter = 0
            self._version += 1
            return self._snapshot_locked()

    def conditional_reset_counter(self, expected_version: int) -> CircuitBreakerSnapshot:
        """Conditionally reset the failure counter in one locked step."""
        with self._lock:
            self._check_version_locked(expected_version)
            self._fail_counter = 0
            self._version += 1
            return self._snapshot_locked()

    @property
    def state(self) -> str:
        """Return the current circuit breaker state."""
        with self._lock:
            return self._state

    @state.setter
    def state(self, state: str) -> None:
        """Set the current circuit breaker state to `state`."""
        with self._lock:
            self._state = state
            self._version += 1

    def increment_counter(self) -> None:
        """Increase the failure counter by one."""
        with self._lock:
            self._fail_counter += 1
            self._version += 1

    def reset_counter(self) -> None:
        """Set the failure counter to zero."""
        with self._lock:
            self._fail_counter = 0
            self._version += 1

    def increment_success_counter(self) -> None:
        """Increase the success counter by one."""
        with self._lock:
            self._success_counter += 1
            self._version += 1

    def reset_success_counter(self) -> None:
        """Set the success counter to zero."""
        with self._lock:
            self._success_counter = 0
            self._version += 1

    @property
    def counter(self) -> int:
        """Return the current value of the failure counter."""
        with self._lock:
            return self._fail_counter

    @property
    def success_counter(self) -> int:
        """Return the current value of the success counter."""
        with self._lock:
            return self._success_counter

    @property
    def opened_at(self) -> datetime | None:
        """Return the most recent value of when the circuit was opened."""
        with self._lock:
            return self._opened_at

    @opened_at.setter
    def opened_at(self, datetime: datetime) -> None:
        """Set the most recent value of when the circuit was opened to `datetime`."""
        with self._lock:
            self._opened_at = datetime
            self._version += 1


class CircuitRedisStorage(CircuitBreakerStorage):
    """Implement a `CircuitBreakerStorage` using redis."""

    BASE_NAMESPACE = "pybreaker"

    logger = logging.getLogger(__name__)

    def __init__(
        self,
        state: str,
        redis_object: Redis,
        namespace: str | None = None,
        fallback_circuit_state: str = STATE_CLOSED,
        cluster_mode: bool = False,
    ):
        """Create a new instance with the given `state` and `redis` object. The
        redis object should be similar to pyredis' StrictRedis class. If there
        are any connection issues with redis, the `fallback_circuit_state` is
        used to determine the state of the circuit.
        """
        # Module does not exist, so this feature is not available
        if not HAS_REDIS_SUPPORT:
            message = "CircuitRedisStorage can only be used if the required dependencies exist"
            raise ImportError(message)

        super().__init__("redis")

        self._redis = redis_object
        self._namespace_name = namespace
        self._fallback_circuit_state = fallback_circuit_state
        self._initial_state = str(state)
        self._cluster_mode = cluster_mode

        self._initialize_redis_state(self._initial_state)

    def _initialize_redis_state(self, state: str) -> None:
        self._redis.setnx(self._namespace("fail_counter"), 0)
        self._redis.setnx(self._namespace("success_counter"), 0)
        self._redis.setnx(self._namespace("state"), state)

    @property
    def supports_conditional_updates(self) -> bool:
        """Redis storage does not implement the version-conditional protocol.

        The independent keys used here cannot be checked-and-swapped as one
        consistent unit without an additional distributed-locking scheme, so
        this backend explicitly refuses the conditional sharing mode instead
        of letting local locks masquerade as cross-process safety.
        """
        return False

    def _reject_conditional(self, operation: str) -> NoReturn:
        message = (
            f"CircuitRedisStorage cannot perform {operation}: state, counters and opened-at live "
            "in independent keys that cannot be checked-and-swapped atomically"
        )
        raise CircuitBreakerStorageUnsupportedError(message)

    def get_snapshot(self) -> CircuitBreakerSnapshot:
        self._reject_conditional("get_snapshot()")

    def compare_and_swap_state(
        self,
        expected_version: int,
        new_state: str,
        opened_at: datetime | None,
        fail_counter: int,
        success_counter: int,
    ) -> CircuitBreakerSnapshot:
        self._reject_conditional(
            f"compare_and_swap_state(version={expected_version}, new_state={new_state!r}, "
            f"opened_at={opened_at!r}, fail_counter={fail_counter}, success_counter={success_counter})"
        )

    def conditional_increment_counter(self, expected_version: int) -> CircuitBreakerSnapshot:
        self._reject_conditional(f"conditional_increment_counter(version={expected_version})")

    def conditional_increment_success_counter(
        self, expected_version: int, reset_fail_counter: bool = False
    ) -> CircuitBreakerSnapshot:
        self._reject_conditional(
            f"conditional_increment_success_counter(version={expected_version}, "
            f"reset_fail_counter={reset_fail_counter})"
        )

    def conditional_reset_counter(self, expected_version: int) -> CircuitBreakerSnapshot:
        self._reject_conditional(f"conditional_reset_counter(version={expected_version})")

    @property
    def state(self) -> str:
        """Return the current circuit breaker state.

        If the circuit breaker state on Redis is missing, re-initialize it
        with the fallback circuit state and reset the fail counter.
        """
        try:
            state_bytes: bytes | None = self._redis.get(self._namespace("state"))
        except RedisError:
            self.logger.exception("RedisError: falling back to default circuit state")
            return self._fallback_circuit_state

        state = self._fallback_circuit_state
        if state_bytes is not None:
            state = state_bytes.decode("utf-8")
        else:
            # state retrieved from redis was missing, so we re-initialize
            # the circuit breaker state on redis
            self._initialize_redis_state(self._fallback_circuit_state)

        return state

    @state.setter
    def state(self, state: str) -> None:
        """Set the current circuit breaker state to `state`."""
        try:
            self._redis.set(self._namespace("state"), str(state))
        except RedisError:
            self.logger.exception("RedisError")

    def increment_counter(self) -> None:
        """Increase the failure counter by one."""
        try:
            self._redis.incr(self._namespace("fail_counter"))
        except RedisError:
            self.logger.exception("RedisError")

    def reset_counter(self) -> None:
        """Set the failure counter to zero."""
        try:
            self._redis.set(self._namespace("fail_counter"), 0)
        except RedisError:
            self.logger.exception("RedisError")

    def increment_success_counter(self) -> None:
        """Increase the success counter by one."""
        try:
            self._redis.incr(self._namespace("success_counter"))
        except RedisError:
            self.logger.exception("RedisError")

    def reset_success_counter(self) -> None:
        """Set the success counter to zero."""
        try:
            self._redis.set(self._namespace("success_counter"), 0)
        except RedisError:
            self.logger.exception("RedisError")

    @property
    def counter(self) -> int:
        """Return the current value of the failure counter."""
        try:
            value = self._redis.get(self._namespace("fail_counter"))
            if value:
                return int(value)
            return 0
        except RedisError:
            self.logger.exception("RedisError: Assuming no errors")
            return 0

    @property
    def success_counter(self) -> int:
        """Return the current value of the success counter."""
        try:
            value = self._redis.get(self._namespace("success_counter"))
            if value:
                return int(value)
            return 0
        except RedisError:
            self.logger.exception("RedisError: Assuming no successes")
            return 0

    @property
    def opened_at(self) -> datetime | None:
        """Returns a datetime object of the most recent value of when the circuit was opened."""
        try:
            timestamp = self._redis.get(self._namespace("opened_at"))
            if timestamp:
                return datetime(*time.gmtime(int(timestamp))[:6], tzinfo=UTC)
        except RedisError:
            self.logger.exception("RedisError")
        return None

    @opened_at.setter
    def opened_at(self, now: datetime) -> None:
        """Atomically set the most recent value of when the circuit was opened
        to `now`. Stored in redis as a simple integer of unix epoch time.
        To avoid timezone issues between different systems, the passed in
        datetime should be in UTC.
        """
        try:
            key = self._namespace("opened_at")

            if self._cluster_mode:
                current_value = self._redis.get(key)
                next_value = int(calendar.timegm(now.timetuple()))

                if not current_value or next_value > int(current_value):
                    self._redis.set(key, next_value)

            else:

                def set_if_greater(pipe: Pipeline[bytes]) -> None:
                    current_value = cast(bytes, pipe.get(key))
                    next_value = int(calendar.timegm(now.timetuple()))
                    pipe.multi()
                    if not current_value or next_value > int(current_value):
                        pipe.set(key, next_value)

                self._redis.transaction(set_if_greater, key)

        except RedisError:
            self.logger.exception("RedisError")

    def _namespace(self, key: str) -> str:
        name_parts = [self.BASE_NAMESPACE, key]
        if self._namespace_name:
            name_parts.insert(0, self._namespace_name)

        return ":".join(name_parts)


class CircuitBreakerListener:
    """Listener class used to plug code to a ``CircuitBreaker`` instance when certain events happen."""

    def before_call(self, cb: CircuitBreaker, func: Callable[..., T], *args: Any, **kwargs: Any) -> None:
        """This callback function is called before the circuit breaker `cb` calls `fn`."""

    def failure(self, cb: CircuitBreaker, exc: BaseException) -> None:
        """This callback function is called when a function called by the circuit breaker `cb` fails."""

    def success(self, cb: CircuitBreaker) -> None:
        """This callback function is called when a function called by the circuit breaker `cb` succeeds."""

    def state_change(
        self,
        cb: CircuitBreaker,
        old_state: CircuitBreakerState | None,
        new_state: CircuitBreakerState,
    ) -> None:
        """This callback function is called when the state of the circuit breaker `cb` state changes."""


class CircuitBreakerState:
    """Implement the behavior needed by all circuit breaker states."""

    def __init__(self, cb: CircuitBreaker, name: str) -> None:
        """Create a new instance associated with the circuit breaker `cb` and identified by `name`."""
        self._breaker: CircuitBreaker = cb
        self._name: str = name

    @property
    def name(self) -> str:
        """Return a human friendly name that identifies this state."""
        return self._name

    @overload
    def _handle_error(self, exc: BaseException, reraise: Literal[True] = ...) -> NoReturn:
        ...

    @overload
    def _handle_error(self, exc: BaseException, reraise: Literal[False] = ...) -> None:
        ...

    def _handle_error(self, exc: BaseException, reraise: bool = True) -> None:
        """Handle a failed call to the guarded operation."""
        if self._breaker.is_system_error(exc):
            if self._breaker.conditional_updates:
                # Counter update and possible trip are one conditional
                # operation, so the failure cannot leave half-round data.
                self._breaker._record_failure_round()
            else:
                self._breaker._inc_counter()
            for listener in self._breaker.listeners:
                listener.failure(self._breaker, exc)
            self.on_failure(exc)
        else:
            self._handle_success()

        if reraise:
            raise exc

    def _handle_success(self) -> None:
        """Handle a successful call to the guarded operation."""
        if self._breaker.conditional_updates:
            self._breaker._record_success_round()
        else:
            self._breaker._state_storage.reset_counter()
        self.on_success()
        for listener in self._breaker.listeners:
            listener.success(self._breaker)

    def call(self, func: Callable[..., T], *args: Any, **kwargs: Any) -> T:
        """Calls `func` with the given `args` and `kwargs`, and updates the
        circuit breaker state according to the result.
        """
        ret = None

        self.before_call(func, *args, **kwargs)
        for listener in self._breaker.listeners:
            listener.before_call(self._breaker, func, *args, **kwargs)

        try:
            ret = func(*args, **kwargs)
            if isinstance(ret, types.GeneratorType):
                return self.generator_call(ret)

        except BaseException as e:
            self._handle_error(e)
        else:
            self._handle_success()
        return ret

    def call_async(self, func, *args: Any, **kwargs: Any):  # type: ignore[no-untyped-def]
        """Call async `func` with the given `args` and `kwargs`, and updates the
        circuit breaker state according to the result.

        Return a closure to prevent import errors when using without tornado present
        """

        @gen.coroutine
        def wrapped():  # type: ignore[no-untyped-def]
            ret = None

            self.before_call(func, *args, **kwargs)
            for listener in self._breaker.listeners:
                listener.before_call(self._breaker, func, *args, **kwargs)

            try:
                ret = yield func(*args, **kwargs)
                if isinstance(ret, types.GeneratorType):
                    raise gen.Return(self.generator_call(ret))

            except BaseException as e:
                self._handle_error(e)
            else:
                self._handle_success()
            raise gen.Return(ret)

        return wrapped()

    def generator_call(self, wrapped_generator):  # type: ignore[no-untyped-def]
        try:
            value = yield next(wrapped_generator)
            while True:
                value = yield wrapped_generator.send(value)
        except StopIteration:
            self._handle_success()
            return
        except BaseException as e:
            self._handle_error(e, reraise=False)
            wrapped_generator.throw(e)

    def before_call(self, func: Callable[..., Any], *args: Any, **kwargs: Any) -> None:
        """Override this method to be notified before a call to the guarded operation is attempted."""

    def on_success(self) -> None:
        """Override this method to be notified when a call to the guarded operation succeeds."""

    def on_failure(self, exc: BaseException) -> None:
        """Override this method to be notified when a call to the guarded operation fails."""


class CircuitClosedState(CircuitBreakerState):
    """In the normal "closed" state, the circuit breaker executes operations as
    usual. If the call succeeds, nothing happens. If it fails, however, the
    circuit breaker makes a note of the failure.

    Once the number of failures exceeds a threshold, the circuit breaker trips
    and "opens" the circuit.
    """

    def __init__(
        self,
        cb: CircuitBreaker,
        prev_state: CircuitBreakerState | None = None,
        notify: bool = False,
    ) -> None:
        """Move the given circuit breaker `cb` to the "closed" state."""
        super().__init__(cb, STATE_CLOSED)
        if notify:
            # We only reset the counter if notify is True, otherwise the CircuitBreaker
            # will lose it's failure count due to a second CircuitBreaker being created
            # using the same _state_storage object, or if the _state_storage objects
            # share a central source of truth (as would be the case with the redis
            # storage).
            self._breaker._state_storage.reset_counter()
            for listener in self._breaker.listeners:
                listener.state_change(self._breaker, prev_state, self)

    def on_failure(self, exc: BaseException) -> None:
        """Move the circuit breaker to the "open" state once the failures threshold is reached."""
        if self._breaker.conditional_updates:
            # The failure round (counter increment plus possible trip) was
            # already committed as one conditional operation. Read the probe
            # round from the same consistent snapshot rather than issuing a
            # separate storage read that could observe a newer round.
            tripped = self._breaker._require_snapshot().state == STATE_OPEN
            if not tripped:
                return
            throw_new_error = self._breaker._throw_new_error_on_trip
        else:
            if self._breaker._state_storage.counter < self._breaker.fail_max:
                return
            throw_new_error = self._breaker.open()

        if throw_new_error:
            error_msg = "Failures threshold reached, circuit breaker opened"
            raise CircuitBreakerError(error_msg).with_traceback(sys.exc_info()[2])
        raise exc


class CircuitOpenState(CircuitBreakerState):
    """When the circuit is "open", calls to the circuit breaker fail immediately,
    without any attempt to execute the real operation. This is indicated by the
    ``CircuitBreakerError`` exception.

    After a suitable amount of time, the circuit breaker decides that the
    operation has a chance of succeeding, so it goes into the "half-open" state.
    """

    def __init__(
        self,
        cb: CircuitBreaker,
        prev_state: CircuitBreakerState | None = None,
        notify: bool = False,
    ) -> None:
        """Move the given circuit breaker `cb` to the "open" state."""
        super().__init__(cb, STATE_OPEN)
        if notify:
            # Reset success counter when opening the circuit
            self._breaker._state_storage.reset_success_counter()
            for listener in self._breaker.listeners:
                listener.state_change(self._breaker, prev_state, self)

    def before_call(self, func: Callable[..., T], *args: Any, **kwargs: Any) -> T:
        """After the timeout elapses, move the circuit breaker to the "half-open"
        state; otherwise, raises ``CircuitBreakerError`` without any attempt
        to execute the real operation.
        """
        if self._breaker.conditional_updates:
            # Read the probe round, opened moment and counters as one
            # consistent snapshot; the timeout decision and the conditional
            # transition therefore cannot be based on mixed versions.
            snapshot = self._breaker._current_snapshot()
            opened_at = snapshot.opened_at
            timed_out = not opened_at or datetime.now(UTC) >= opened_at + timedelta(seconds=self._breaker.reset_timeout)
            if not timed_out:
                error_msg = "Timeout not elapsed yet, circuit breaker still open"
                raise CircuitBreakerError(error_msg)
            self._breaker._enter_half_open_if_due()
            return self._breaker.call(func, *args, **kwargs)

        timeout = timedelta(seconds=self._breaker.reset_timeout)
        opened_at = self._breaker._state_storage.opened_at
        if opened_at and datetime.now(UTC) < opened_at + timeout:
            error_msg = "Timeout not elapsed yet, circuit breaker still open"
            raise CircuitBreakerError(error_msg)
        self._breaker.half_open()
        return self._breaker.call(func, *args, **kwargs)

    def call(self, func: Callable[..., T], *args: Any, **kwargs: Any) -> T:
        """Delegate the call to before_call, if the time out is not elapsed it will throw an exception, otherwise we get
        the results from the call performed after the state is switch to half-open.
        """
        return self.before_call(func, *args, **kwargs)


class CircuitHalfOpenState(CircuitBreakerState):
    """In the "half-open" state, the next call to the circuit breaker is allowed
    to execute the dangerous operation. Should the call succeed, the circuit
    breaker resets and returns to the "closed" state. If this trial call fails,
    however, the circuit breaker returns to the "open" state until another
    timeout elapses.
    """

    def __init__(
        self,
        cb: CircuitBreaker,
        prev_state: CircuitBreakerState | None,
        notify: bool = False,
    ) -> None:
        """Move the given circuit breaker `cb` to the "half-open" state."""
        super().__init__(cb, STATE_HALF_OPEN)
        if notify:
            # Reset success counter when entering half-open state
            self._breaker._state_storage.reset_success_counter()
            for listener in self._breaker._listeners:
                listener.state_change(self._breaker, prev_state, self)

    def on_failure(self, exc: BaseException) -> NoReturn:
        """Opens the circuit breaker."""
        if self._breaker.conditional_updates:
            # The failed trial call was already re-opened atomically (new
            # opened moment plus counter reset) while recording the round.
            throw_new_error = self._breaker._throw_new_error_on_trip
        else:
            throw_new_error = self._breaker.open()

        if throw_new_error:
            error_msg = "Trial call failed, circuit breaker opened"
            raise CircuitBreakerError(error_msg).with_traceback(sys.exc_info()[2])
        raise exc

    def on_success(self) -> None:
        """Increment success counter and close the circuit breaker if threshold is reached."""
        if self._breaker.conditional_updates:
            # The success counter increment, stale failure-counter cleanup and
            # the possible transition to 'closed' were committed together as
            # one conditional operation; nothing else needs to be written.
            return

        self._breaker._state_storage.increment_success_counter()

        if self._breaker._state_storage.success_counter >= self._breaker.success_threshold:
            self._breaker.close()


class CircuitBreakerError(Exception):
    """When calls to a service fails because the circuit is open, this error is
    raised to allow the caller to handle this type of exception differently.
    """
