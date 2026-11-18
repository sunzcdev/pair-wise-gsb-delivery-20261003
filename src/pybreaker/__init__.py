"""Threadsafe pure-Python implementation of the Circuit Breaker pattern, described
by Michael T. Nygard in his book 'Release It!'.

For more information on this and other patterns and best practices, buy the
book at https://pragprog.com/titles/mnee2/release-it-second-edition/
"""

from __future__ import annotations

import asyncio
import calendar
import contextlib
import logging
import sys
import threading
import time
import types
import weakref
from abc import abstractmethod
from datetime import datetime, timedelta
from functools import wraps
from typing import (
    TYPE_CHECKING,
    Any,
    Callable,
    NoReturn,
    TypeVar,
    Union,
    cast,
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
    "CircuitMemoryStorage",
    "CircuitRedisStorage",
    "STATE_OPEN",
    "STATE_CLOSED",
    "STATE_HALF_OPEN",
)

STATE_OPEN = "open"
STATE_CLOSED = "closed"
STATE_HALF_OPEN = "half-open"

T = TypeVar("T")
ExceptionType = TypeVar("ExceptionType", bound=BaseException)
CBListenerType = TypeVar("CBListenerType", bound="CircuitBreakerListener")
CBStateType = Union["CircuitClosedState", "CircuitHalfOpenState", "CircuitOpenState"]


class _TrialCall:
    """Bookkeeping for a single call admitted by a circuit breaker.

    A trial remembers the *round* (epoch) it was admitted in, whether it was
    admitted as a half-open probe, and whether its permit was already handed
    back. A trial whose epoch is no longer the breaker's current epoch is
    stale: its outcome must not change the state or the counters of the round
    that replaced it.
    """

    __slots__ = ("epoch", "state", "is_probe", "released", "handed_off")

    def __init__(self, epoch: int, state: CircuitBreakerState, *, is_probe: bool) -> None:
        """Create a trial admitted by `state` in `epoch`; `is_probe` marks half-open trials."""
        self.epoch = epoch
        self.state = state
        self.is_probe = is_probe
        self.released = False
        # Set when an asynchronous call handed the trial to a generator that
        # outlives the coroutine and now owns the permit.
        self.handed_off = False


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
        half_open_max_calls: int = 1,
        half_open_wait_timeout: float | None = None,
    ) -> None:
        """Create a new circuit breaker with the given parameters.

        `half_open_max_calls` limits how many trial calls may probe the guarded
        operation at the same time while the circuit is half-open. Calls beyond
        that limit are rejected with ``CircuitBreakerError`` unless
        `half_open_wait_timeout` is set, in which case they wait at most that
        many seconds for a trial permit to become available.
        """
        if half_open_max_calls < 1:
            msg = "half_open_max_calls must be at least 1"
            raise ValueError(msg)
        if half_open_wait_timeout is not None and half_open_wait_timeout < 0:
            msg = "half_open_wait_timeout must be None or a non-negative number"
            raise ValueError(msg)

        self._lock = threading.RLock()
        self._cond = threading.Condition(self._lock)

        # Every state change starts a new "round": trial calls admitted in a
        # previous round must not affect the state or counters of the new one.
        self._epoch = 0
        self._probes_in_flight = 0

        self._state_storage = state_storage or CircuitMemoryStorage(STATE_CLOSED)
        self._state = self._create_new_state(self.current_state)

        self._fail_max = fail_max
        self._reset_timeout = reset_timeout
        self._success_threshold = success_threshold
        self._half_open_max_calls = half_open_max_calls
        self._half_open_wait_timeout = half_open_wait_timeout

        self._excluded_exceptions = list(exclude or [])
        self._listeners = list(listeners or [])
        self._name = name

        self._throw_new_error_on_trip = throw_new_error_on_trip

    @property
    def half_open_max_calls(self) -> int:
        """Return how many trial calls may probe the guarded operation concurrently."""
        return self._half_open_max_calls

    @half_open_max_calls.setter
    def half_open_max_calls(self, number: int) -> None:
        """Set how many trial calls may probe the guarded operation concurrently."""
        if number < 1:
            msg = "half_open_max_calls must be at least 1"
            raise ValueError(msg)
        with self._cond:
            self._half_open_max_calls = number
            self._cond.notify_all()

    @property
    def half_open_wait_timeout(self) -> float | None:
        """Return how long, in seconds, calls wait for a half-open trial permit.

        ``None`` means calls beyond ``half_open_max_calls`` are rejected instead
        of waiting.
        """
        return self._half_open_wait_timeout

    @half_open_wait_timeout.setter
    def half_open_wait_timeout(self, timeout: float | None) -> None:
        """Set how long calls wait for a half-open trial permit, or ``None`` to never wait."""
        if timeout is not None and timeout < 0:
            msg = "half_open_wait_timeout must be None or a non-negative number"
            raise ValueError(msg)
        with self._cond:
            self._half_open_wait_timeout = timeout
            self._cond.notify_all()

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
            with self._cond:
                # Re-check while holding the lock: another thread may have
                # already refreshed the cached state.
                if self.current_state != self._state.name:
                    # If cached state is out-of-date, that means that it was likely
                    # changed elsewhere (e.g. another process instance). We still send
                    # out a notification, informing others that this particular circuit
                    # breaker instance noticed the changed circuit.
                    self._start_round(self.current_state, persist=False)
        return self._state

    @state.setter
    def state(self, state_str: str) -> None:
        """Set cached state and notify listeners of newly cached state."""
        with self._cond:
            self._start_round(state_str, persist=False)

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
        self._state_storage.increment_counter()

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

        The breaker lock is held only while admitting the call and while
        recording its outcome, so `func` itself runs unlocked.
        """
        trial = self._acquire_trial()
        return self._execute(trial, func, *args, **kwargs)

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

        Calls are rejected, rather than waiting, when the half-open probe budget
        is exhausted: blocking the IOLoop to wait for a probe slot would stall
        every other coroutine.

        Return a closure to prevent import errors when using without tornado present
        """

        @gen.coroutine
        def wrapped():  # type: ignore[no-untyped-def]
            # Never block the IOLoop waiting for a probe slot: asynchronous
            # callers are rejected when the half-open budget is exhausted.
            trial = self._acquire_trial(allow_wait=False)
            ret = yield self._execute_async(trial, func, *args, **kwargs)
            raise gen.Return(ret)

        return wrapped()

    def _acquire_trial(self, allow_wait: bool = True) -> _TrialCall:
        """Admit a call to the guarded operation.

        Returns the bookkeeping record of the admitted call, or raises
        ``CircuitBreakerError`` when the circuit is open, or when the half-open
        probe budget is exhausted and waiting is disabled or times out.
        """
        with self._cond:
            wait_timeout = self._half_open_wait_timeout if allow_wait else None
            deadline = None if wait_timeout is None else time.monotonic() + wait_timeout

            while True:
                # Reading the state may notice a change made elsewhere and start
                # a new round, so it happens while holding the lock.
                trial = self.state.acquire()
                if trial is not None:
                    return trial

                # The circuit is half-open and every trial slot is taken.
                if deadline is None:
                    msg = "Too many trial calls in flight, circuit breaker is half-open"
                    raise CircuitBreakerError(msg)

                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    msg = "Timed out waiting for a half-open trial call to finish"
                    raise CircuitBreakerError(msg)

                # Waiting releases the lock and never holds a trial permit.
                self._cond.wait(remaining)

    def _execute(self, trial: _TrialCall, func: Callable[..., T], *args: Any, **kwargs: Any) -> T:
        """Run `func` and record its outcome for `trial`, outside the breaker lock."""
        try:
            self._before_call(trial, func, *args, **kwargs)
        except BaseException:
            # A listener refused the call: hand the trial slot back untouched.
            self._finalize(trial)
            raise

        try:
            ret = func(*args, **kwargs)
            if isinstance(ret, types.GeneratorType):
                return self._drive_generator(trial, ret)  # type: ignore[return-value]
        except GeneratorExit:
            # The call was abandoned rather than completed: not an outcome.
            self._finalize(trial)
            raise
        except BaseException as exc:
            self._finish_failure(trial, exc)
        else:
            self._finish_success(trial)
        return ret

    def _execute_async(self, trial: _TrialCall, func, *args: Any, **kwargs: Any):  # type: ignore[no-untyped-def]
        """Await async `func` and record its outcome for `trial`, outside the breaker lock."""

        @gen.coroutine
        def wrapped():  # type: ignore[no-untyped-def]
            try:
                self._before_call(trial, func, *args, **kwargs)

                try:
                    ret = yield func(*args, **kwargs)
                except (GeneratorExit, asyncio.CancelledError):
                    # The caller cancelled the operation: release the permit
                    # once the operation stops, without counting the
                    # cancellation as a call outcome.
                    raise
                except BaseException as exc:
                    self._finish_failure(trial, exc)
                    raise

                if isinstance(ret, types.GeneratorType):
                    # The trial outlives this coroutine: the generator records
                    # the outcome and returns the permit when it terminates.
                    trial.handed_off = True
                    ret = self._drive_generator(trial, ret)
                else:
                    self._finish_success(trial)
                raise gen.Return(ret)
            finally:
                # A listener refusing the call, or an error while recording the
                # outcome, must never strand the half-open permit. A cancelled
                # caller keeps the permit until the operation itself stops, so
                # the probe budget still reflects the work in flight.
                self._finalize_accepted_call(trial)

        return wrapped()

    def _before_call(self, trial: _TrialCall, func: Callable[..., Any], *args: Any, **kwargs: Any) -> None:
        """Notify listeners that `func` is about to run, without holding the lock."""
        trial.state.before_call(func, *args, **kwargs)
        for listener in self.listeners:
            listener.before_call(self, func, *args, **kwargs)

    def _drive_generator(self, trial: _TrialCall, wrapped_generator: Generator) -> Generator:
        """Drive `wrapped_generator`, recording its outcome when it terminates.

        The trial permit is returned once the generator finishes, fails, is
        abandoned, or is garbage collected without ever being started.
        """
        # Registered before the driving generator starts, so a guard that is
        # created but never iterated cannot strand its permit.
        reaper = weakref.finalize(wrapped_generator, self._finalize, trial)

        def drive() -> Generator:
            try:
                value = yield next(wrapped_generator)
                while True:
                    value = yield wrapped_generator.send(value)
            except StopIteration:
                self._finish_success(trial)
                return
            except GeneratorExit:
                # The consumer abandoned the generator: give the trial slot
                # back without counting the call as success or failure.
                self._finalize(trial)
                wrapped_generator.close()
                raise
            except BaseException as e:
                self._finish_failure(trial, e, reraise=False)
                wrapped_generator.throw(e)
            finally:
                reaper.detach()

        return drive()

    def _finish_success(self, trial: _TrialCall) -> None:
        """Record a successful call, ignoring outcomes from superseded rounds."""
        with self._cond:
            try:
                if self._is_current_round(trial):
                    self._state_storage.reset_counter()
                    trial.state.on_success()
                for listener in self.listeners:
                    listener.success(self)
            finally:
                self._release_trial(trial)

    def _finish_failure(self, trial: _TrialCall, exc: BaseException, reraise: bool = True) -> None:
        """Record a failed call, ignoring outcomes from superseded rounds."""
        if self.is_system_error(exc):
            with self._cond:
                try:
                    if self._is_current_round(trial):
                        self._inc_counter()
                    for listener in self.listeners:
                        listener.failure(self, exc)
                    if self._is_current_round(trial):
                        trial.state.on_failure(exc)
                finally:
                    self._release_trial(trial)
        else:
            # Excluded (business) exceptions are treated as a successful call.
            self._finish_success(trial)

        if reraise:
            raise exc

    def _is_current_round(self, trial: _TrialCall) -> bool:
        """Return whether `trial` was admitted by the round currently in progress."""
        return trial.epoch == self._epoch

    def _finalize(self, trial: _TrialCall) -> None:
        """Give back any permit still held by `trial`; safe to call more than once."""
        with self._cond:
            self._release_trial(trial)

    def _finalize_accepted_call(self, trial: _TrialCall) -> None:
        """Finalize a recognised call, unless a generator took over the trial.

        While a guarded generator drives the operation, the permit belongs to it
        and must not be returned by the call that created it.
        """
        if not trial.handed_off:
            self._finalize(trial)

    def _release_trial(self, trial: _TrialCall) -> None:
        """Hand back the half-open slot held by `trial` and wake waiting callers."""
        if trial.released:
            return
        trial.released = True
        if trial.is_probe and trial.epoch == self._epoch:
            self._probes_in_flight -= 1
        self._cond.notify_all()

    def _start_round(self, state_str: str, *, persist: bool = True) -> CBStateType:
        """Switch to `state_str` and begin a new round of calls.

        Trials admitted before this point are superseded: their outcomes must
        not change the state, the counters or the probe budget of the new round.
        """
        self._epoch += 1
        self._probes_in_flight = 0
        if persist:
            self._state_storage.state = state_str
        self._state = self._create_new_state(state_str, prev_state=self._state, notify=True)
        self._cond.notify_all()
        return self._state

    def open(self) -> bool:
        """Open the circuit, e.g., the following calls will immediately fail until timeout elapses."""
        with self._cond:
            self._state_storage.opened_at = datetime.now(UTC)
            self._start_round(STATE_OPEN)
            return self._throw_new_error_on_trip

    def half_open(self) -> None:
        """Half-open the circuit, e.g. lets the following call pass through and
        opens the circuit if the call fails (or closes the circuit if the call
        succeeds).
        """
        with self._cond:
            self._start_round(STATE_HALF_OPEN)

    def close(self) -> None:
        """Close the circuit, e.g. lets the following calls execute as usual."""
        with self._cond:
            self._state_storage.reset_success_counter()  # Reset success counter when closing
            self._start_round(STATE_CLOSED)

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
    """Implement a `CircuitBreakerStorage` in local memory."""

    def __init__(self, state: str) -> None:
        """Create a new instance with the given `state`."""
        super().__init__("memory")
        self._fail_counter = 0
        self._success_counter = 0
        self._opened_at: datetime | None = None
        self._state = state

    @property
    def state(self) -> str:
        """Return the current circuit breaker state."""
        return self._state

    @state.setter
    def state(self, state: str) -> None:
        """Set the current circuit breaker state to `state`."""
        self._state = state

    def increment_counter(self) -> None:
        """Increase the failure counter by one."""
        self._fail_counter += 1

    def reset_counter(self) -> None:
        """Set the failure counter to zero."""
        self._fail_counter = 0

    def increment_success_counter(self) -> None:
        """Increase the success counter by one."""
        self._success_counter += 1

    def reset_success_counter(self) -> None:
        """Set the success counter to zero."""
        self._success_counter = 0

    @property
    def counter(self) -> int:
        """Return the current value of the failure counter."""
        return self._fail_counter

    @property
    def success_counter(self) -> int:
        """Return the current value of the success counter."""
        return self._success_counter

    @property
    def opened_at(self) -> datetime | None:
        """Return the most recent value of when the circuit was opened."""
        return self._opened_at

    @opened_at.setter
    def opened_at(self, datetime: datetime) -> None:
        """Set the most recent value of when the circuit was opened to `datetime`."""
        self._opened_at = datetime


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

    def acquire(self) -> _TrialCall | None:
        """Admit a call while in this state, or return ``None`` to make it wait.

        Called with the breaker lock held. Returning ``None`` means the circuit
        would accept the call but has no trial budget left right now; the
        breaker decides whether the caller is rejected or waits.
        """
        return _TrialCall(self._breaker._epoch, self, is_probe=False)

    def before_call(self, func: Callable[..., Any], *args: Any, **kwargs: Any) -> None:
        """Override this method to be notified before a call to the guarded operation is attempted."""

    def on_success(self) -> None:
        """Override this method to be notified when a call admitted by this state succeeds."""

    def on_failure(self, exc: BaseException) -> None:
        """Override this method to be notified when a call admitted by this state fails."""


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
        if self._breaker._state_storage.counter >= self._breaker.fail_max:
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

    def acquire(self) -> _TrialCall | None:
        """Reject calls until the timeout elapses, then start a half-open round."""
        timeout = timedelta(seconds=self._breaker.reset_timeout)
        opened_at = self._breaker._state_storage.opened_at
        if opened_at and datetime.now(UTC) < opened_at + timeout:
            error_msg = "Timeout not elapsed yet, circuit breaker still open"
            raise CircuitBreakerError(error_msg)

        # The timeout elapsed: this call probes the guarded operation, opening a
        # fresh half-open round so no stale outcome can end it.
        half_open_state = self._breaker._start_round(STATE_HALF_OPEN)
        return half_open_state.acquire()


class CircuitHalfOpenState(CircuitBreakerState):
    """In the "half-open" state, a limited number of trial calls are allowed to
    execute the dangerous operation. Should enough of them succeed, the circuit
    breaker resets and returns to the "closed" state. If one of them fails,
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

    def acquire(self) -> _TrialCall | None:
        """Admit a trial call as long as the probe budget is not exhausted."""
        breaker = self._breaker
        if breaker._probes_in_flight >= breaker.half_open_max_calls:
            return None

        breaker._probes_in_flight += 1
        return _TrialCall(breaker._epoch, self, is_probe=True)

    def on_failure(self, exc: BaseException) -> NoReturn:
        """Opens the circuit breaker."""
        throw_new_error = self._breaker.open()

        if throw_new_error:
            error_msg = "Trial call failed, circuit breaker opened"
            raise CircuitBreakerError(error_msg).with_traceback(sys.exc_info()[2])
        raise exc

    def on_success(self) -> None:
        """Increment success counter and close the circuit breaker if threshold is reached."""
        self._breaker._state_storage.increment_success_counter()

        if self._breaker._state_storage.success_counter >= self._breaker.success_threshold:
            self._breaker.close()


class CircuitBreakerError(Exception):
    """When calls to a service fails because the circuit is open, this error is
    raised to allow the caller to handle this type of exception differently.
    """
