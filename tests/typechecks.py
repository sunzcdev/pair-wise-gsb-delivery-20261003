from pybreaker import (
    STATE_CLOSED,
    CircuitBreaker,
    CircuitMemoryStorage,
    ListenerErrorPolicy,
    ListenerNotificationFailure,
)

# issue #90: incorrect typing for exclude argument
# this should not give errors in mypy
CircuitBreaker(
    fail_max=1,
    reset_timeout=1000,
    exclude=[lambda e: isinstance(e, RuntimeError)],
    state_storage=CircuitMemoryStorage(STATE_CLOSED),
)

# listener error policy accepts both the enum and its string value
CircuitBreaker(listener_error_policy=ListenerErrorPolicy.STRICT)
CircuitBreaker(listener_error_policy="log")
CircuitBreaker(listener_error_policy=None)

breaker = CircuitBreaker()
policy: ListenerErrorPolicy = breaker.listener_error_policy
failures: tuple[ListenerNotificationFailure, ...] = breaker.listener_failures
