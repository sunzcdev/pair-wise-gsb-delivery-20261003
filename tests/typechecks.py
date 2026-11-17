from pybreaker import STATE_CLOSED, CircuitBreaker, CircuitMemoryStorage

# issue #90: incorrect typing for exclude argument
# this should not give errors in mypy
breaker = CircuitBreaker(
    fail_max=1,
    reset_timeout=1000,
    exclude=[lambda e: isinstance(e, RuntimeError)],
    state_storage=CircuitMemoryStorage(STATE_CLOSED),
)

# manual overrides: typed usage should not give errors in mypy
override = breaker.set_override("force_allow", reason="deploy", operator="oncall", ttl=60)
is_active: bool = override.is_active()
cleared: bool = breaker.clear_override(override.version)
