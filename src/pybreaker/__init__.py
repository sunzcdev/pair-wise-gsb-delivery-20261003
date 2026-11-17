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
    "CircuitMemoryStorage",
    "CircuitRedisStorage",
    "STATE_OPEN",
    "STATE_CLOSED",
    "STATE_HALF_OPEN",
)

STATE_OPEN = "open"
STATE_CLOSED = "closed"
STATE_HALF_OPEN = "half-open"

# Maximum valid value for the slow-call rate threshold, expressed as a percentage.
MAX_PERCENTAGE = 100

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
        slow_call_duration_threshold: float | None = None,
        slow_call_max: int | None = None,
        slow_call_rate_threshold: float | None = None,
        slow_call_min_calls: int = 1,
        slow_success_counts_toward_recovery: bool = False,
        time_func: Callable[[], float] | None = None,
    ) -> None:
        """Create a new circuit breaker with the given parameters.

        A successful call is considered *slow* when its elapsed time, measured
        with `time_func`, is greater than or equal to
        `slow_call_duration_threshold` seconds. Slow successes and exception
        failures are counted separately. The circuit may be opened either by
        the failure rule (`fail_max`), by the slow-call count rule
        (`slow_call_max`) or by the slow-call rate rule
        (`slow_call_rate_threshold`); each rule is only active when its
        configuration is provided.
        """
        self._validate_slow_call_config(
            slow_call_duration_threshold,
            slow_call_max,
            slow_call_rate_threshold,
            slow_call_min_calls,
        )

        self._lock = threading.RLock()
        self._state_storage = state_storage or CircuitMemoryStorage(STATE_CLOSED)
        self._state = self._create_new_state(self.current_state)

        self._fail_max = fail_max
        self._reset_timeout = reset_timeout
        self._success_threshold = success_threshold

        self._excluded_exceptions = list(exclude or [])
        self._listeners = list(listeners or [])
        self._name = name

        self._throw_new_error_on_trip = throw_new_error_on_trip

        self._slow_call_duration_threshold = slow_call_duration_threshold
        self._slow_call_max = slow_call_max
        self._slow_call_rate_threshold = slow_call_rate_threshold
        self._slow_call_min_calls = slow_call_min_calls
        self._slow_success_counts_toward_recovery = slow_success_counts_toward_recovery
        self._time_func = time_func or time.monotonic

    @staticmethod
    def _validate_slow_call_config(
        slow_call_duration_threshold: float | None,
        slow_call_max: int | None,
        slow_call_rate_threshold: float | None,
        slow_call_min_calls: int,
    ) -> None:
        """Validate the parameters that govern slow-call based protection."""
        if slow_call_duration_threshold is not None and slow_call_duration_threshold <= 0:
            msg = "slow_call_duration_threshold must be greater than zero"
            raise ValueError(msg)

        if (slow_call_max is not None or slow_call_rate_threshold is not None) and (
            slow_call_duration_threshold is None
        ):
            msg = "slow_call_duration_threshold is required to enable slow-call based protection"
            raise ValueError(msg)

        if slow_call_max is not None and slow_call_max < 1:
            msg = "slow_call_max must be greater than zero"
            raise ValueError(msg)

        if slow_call_rate_threshold is not None and not 0 < slow_call_rate_threshold <= MAX_PERCENTAGE:
            msg = "slow_call_rate_threshold must be a percentage between 0 and 100"
            raise ValueError(msg)

        if slow_call_min_calls < 1:
            msg = "slow_call_min_calls must be greater than zero"
            raise ValueError(msg)

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

    @property
    def slow_call_duration_threshold(self) -> float | None:
        """Return the elapsed time, in seconds, from which a successful call is considered slow."""
        return self._slow_call_duration_threshold

    @property
    def slow_call_max(self) -> int | None:
        """Return the maximum number of slow calls tolerated before the circuit is opened."""
        return self._slow_call_max

    @property
    def slow_call_rate_threshold(self) -> float | None:
        """Return the slow-call rate threshold, as a percentage, that opens the circuit."""
        return self._slow_call_rate_threshold

    @property
    def slow_call_min_calls(self) -> int:
        """Return the minimum number of calls required before the slow-call rate rule is evaluated."""
        return self._slow_call_min_calls

    @property
    def slow_success_counts_toward_recovery(self) -> bool:
        """Return whether a slow success in half-open state counts towards the recovery threshold."""
        return self._slow_success_counts_toward_recovery

    @property
    def time_func(self) -> Callable[[], float]:
        """Return the monotonic clock function used to measure call elapsed time."""
        return self._time_func

    @property
    def slow_counter(self) -> int:
        """Return the current number of slow calls recorded in closed state."""
        return self._state_storage.slow_counter

    @property
    def call_counter(self) -> int:
        """Return the current number of executed calls recorded in closed state."""
        return self._state_storage.call_counter

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
            # changed elsewhere (e.g. another process instance). We still send
            # out a notification, informing others that this particular circuit
            # breaker instance noticed the changed circuit.
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

    def slow_call_threshold_reached(self) -> bool:
        """Return whether the configured slow-call count or rate rule is
        currently satisfied, based on the counters in state storage.

        Rules that were not configured (i.e. their parameters are None) are
        simply not evaluated.
        """
        storage = self._state_storage
        count_rule_triggered = self._slow_call_max is not None and storage.slow_counter >= self._slow_call_max
        rate_rule_triggered = (
            self._slow_call_rate_threshold is not None
            and storage.call_counter >= self._slow_call_min_calls
            and (storage.slow_counter / storage.call_counter) * MAX_PERCENTAGE >= self._slow_call_rate_threshold
        )
        return count_rule_triggered or rate_rule_triggered

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
            self._state_storage.opened_at = datetime.now(UTC)
            self.state = self._state_storage.state = STATE_OPEN  # type: ignore[assignment]

            return self._throw_new_error_on_trip

    def half_open(self) -> None:
        """Half-open the circuit, e.g. lets the following call pass through and
        opens the circuit if the call fails (or closes the circuit if the call
        succeeds).
        """
        with self._lock:
            self.state = self._state_storage.state = STATE_HALF_OPEN  # type: ignore[assignment]

    def close(self) -> None:
        """Close the circuit, e.g. lets the following calls execute as usual."""
        with self._lock:
            self._state_storage.reset_success_counter()  # Reset success counter when closing
            self._state_storage.reset_slow_counter()
            self._state_storage.reset_call_counter()
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

    def increment_slow_counter(self) -> None:
        """Override this method to increase the slow-call counter by one."""

    def reset_slow_counter(self) -> None:
        """Override this method to set the slow-call counter to zero."""

    def increment_call_counter(self) -> None:
        """Override this method to increase the executed-call counter by one."""

    def reset_call_counter(self) -> None:
        """Override this method to set the executed-call counter to zero."""

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
    def slow_counter(self) -> int:
        """Override this method to retrieve the current value of the slow-call counter."""

    @property
    @abstractmethod
    def call_counter(self) -> int:
        """Override this method to retrieve the current value of the executed-call counter."""

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
        self._slow_counter = 0
        self._call_counter = 0
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

    def increment_slow_counter(self) -> None:
        """Increase the slow-call counter by one."""
        self._slow_counter += 1

    def reset_slow_counter(self) -> None:
        """Set the slow-call counter to zero."""
        self._slow_counter = 0

    def increment_call_counter(self) -> None:
        """Increase the executed-call counter by one."""
        self._call_counter += 1

    def reset_call_counter(self) -> None:
        """Set the executed-call counter to zero."""
        self._call_counter = 0

    @property
    def counter(self) -> int:
        """Return the current value of the failure counter."""
        return self._fail_counter

    @property
    def success_counter(self) -> int:
        """Return the current value of the success counter."""
        return self._success_counter

    @property
    def slow_counter(self) -> int:
        """Return the current value of the slow-call counter."""
        return self._slow_counter

    @property
    def call_counter(self) -> int:
        """Return the current value of the executed-call counter."""
        return self._call_counter

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
        self._redis.setnx(self._namespace("slow_counter"), 0)
        self._redis.setnx(self._namespace("call_counter"), 0)
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

    def increment_slow_counter(self) -> None:
        """Increase the slow-call counter by one."""
        try:
            self._redis.incr(self._namespace("slow_counter"))
        except RedisError:
            self.logger.exception("RedisError")

    def reset_slow_counter(self) -> None:
        """Set the slow-call counter to zero."""
        try:
            self._redis.set(self._namespace("slow_counter"), 0)
        except RedisError:
            self.logger.exception("RedisError")

    def increment_call_counter(self) -> None:
        """Increase the executed-call counter by one."""
        try:
            self._redis.incr(self._namespace("call_counter"))
        except RedisError:
            self.logger.exception("RedisError")

    def reset_call_counter(self) -> None:
        """Set the executed-call counter to zero."""
        try:
            self._redis.set(self._namespace("call_counter"), 0)
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
    def slow_counter(self) -> int:
        """Return the current value of the slow-call counter."""
        try:
            value = self._redis.get(self._namespace("slow_counter"))
            if value:
                return int(value)
            return 0
        except RedisError:
            self.logger.exception("RedisError: Assuming no slow calls")
            return 0

    @property
    def call_counter(self) -> int:
        """Return the current value of the executed-call counter."""
        try:
            value = self._redis.get(self._namespace("call_counter"))
            if value:
                return int(value)
            return 0
        except RedisError:
            self.logger.exception("RedisError: Assuming no calls")
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
            self._breaker._inc_counter()
            self._breaker._state_storage.increment_call_counter()
            for listener in self._breaker.listeners:
                listener.failure(self._breaker, exc)
            self.on_failure(exc)
        else:
            self._handle_success()

        if reraise:
            raise exc

    def _handle_success(self, slow: bool = False) -> None:
        """Handle a successful call to the guarded operation.

        Slow successes are counted separately from exception failures. The
        slow-call and executed-call counters accumulate while the circuit is
        closed and are only reset when the circuit returns to the closed
        state, so that the slow-call rate can be evaluated over the whole
        observation window.
        """
        storage = self._breaker._state_storage
        storage.increment_call_counter()
        storage.reset_counter()
        if slow:
            storage.increment_slow_counter()
        self.on_success(slow)
        for listener in self._breaker.listeners:
            listener.success(self._breaker)

    def _start_timer(self) -> float | None:
        """Read the injected monotonic clock before the guarded call.

        Any error raised by the clock itself is swallowed so that a broken
        clock can never mask an exception raised by the user function.
        """
        try:
            return self._breaker.time_func()
        except Exception:
            return None

    def _elapsed(self, started_at: float | None) -> float | None:
        """Return the elapsed time since `started_at`, or None when timing is
        unavailable or unreliable (e.g. the clock itself failed).
        """
        if started_at is None:
            return None
        try:
            elapsed = self._breaker.time_func() - started_at
        except Exception:
            return None
        if elapsed < 0:
            return None
        return elapsed

    def _is_slow(self, started_at: float | None) -> bool:
        """Return whether the call elapsed at or beyond the configured slow-call
        duration threshold. Calls that cannot be timed are never considered slow.
        """
        threshold = self._breaker.slow_call_duration_threshold
        if threshold is None:
            return False
        elapsed = self._elapsed(started_at)
        if elapsed is None:
            return False
        return elapsed >= threshold

    def call(self, func: Callable[..., T], *args: Any, **kwargs: Any) -> T:
        """Calls `func` with the given `args` and `kwargs`, and updates the
        circuit breaker state according to the result.
        """
        ret = None

        self.before_call(func, *args, **kwargs)
        for listener in self._breaker.listeners:
            listener.before_call(self._breaker, func, *args, **kwargs)

        started_at = self._start_timer()
        try:
            ret = func(*args, **kwargs)
            if isinstance(ret, types.GeneratorType):
                return self.generator_call(ret)

        except BaseException as e:
            self._handle_error(e)
        else:
            self._handle_success(self._is_slow(started_at))
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

    def on_success(self, slow: bool = False) -> None:
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
        if self._breaker._state_storage.counter >= self._breaker.fail_max:
            throw_new_error = self._breaker.open()

            if throw_new_error:
                error_msg = "Failures threshold reached, circuit breaker opened"
                raise CircuitBreakerError(error_msg).with_traceback(sys.exc_info()[2])
            raise exc

    def on_success(self, slow: bool = False) -> None:
        """Open the circuit breaker when a slow success reaches any of the
        configured slow-call count or rate thresholds. The call itself still
        returns its result successfully.
        """
        if slow and self._breaker.slow_call_threshold_reached():
            self._breaker.open()


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
        throw_new_error = self._breaker.open()

        if throw_new_error:
            error_msg = "Trial call failed, circuit breaker opened"
            raise CircuitBreakerError(error_msg).with_traceback(sys.exc_info()[2])
        raise exc

    def on_success(self, slow: bool = False) -> None:
        """Increment the success counter and close the circuit breaker once the
        recovery threshold is reached.

        A slow success only counts towards recovery when the explicit
        `slow_success_counts_toward_recovery` setting is enabled; otherwise,
        the circuit stays half-open so that another trial call is required.
        """
        if slow and not self._breaker.slow_success_counts_toward_recovery:
            return

        self._breaker._state_storage.increment_success_counter()

        if self._breaker._state_storage.success_counter >= self._breaker.success_threshold:
            self._breaker.close()


class CircuitBreakerError(Exception):
    """When calls to a service fails because the circuit is open, this error is
    raised to allow the caller to handle this type of exception differently.
    """
