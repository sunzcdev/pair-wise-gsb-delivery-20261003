from pybreaker import STATE_CLOSED, CircuitBreaker, CircuitMemoryStorage

# issue #90: incorrect typing for exclude argument
# this should not give errors in mypy
CircuitBreaker(
    fail_max=1,
    reset_timeout=1000,
    exclude=[lambda e: isinstance(e, RuntimeError)],
    state_storage=CircuitMemoryStorage(STATE_CLOSED),
)

# half-open probe limits and waiting are configurable, at creation time and later
breaker = CircuitBreaker(half_open_max_calls=2, half_open_wait_timeout=1.0)
breaker.half_open_max_calls = 3
breaker.half_open_wait_timeout = None
max_calls: int = breaker.half_open_max_calls
wait_timeout: float | None = breaker.half_open_wait_timeout
