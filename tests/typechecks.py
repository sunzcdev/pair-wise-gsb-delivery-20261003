from pybreaker import (
    STATE_CLOSED,
    CircuitBreaker,
    CircuitBreakerPolicy,
    CircuitMemoryStorage,
    PolicyEvaluation,
)

# issue #90: incorrect typing for exclude argument
# this should not give errors in mypy
CircuitBreaker(
    fail_max=1,
    reset_timeout=1000,
    exclude=[lambda e: isinstance(e, RuntimeError)],
    state_storage=CircuitMemoryStorage(STATE_CLOSED),
)

# policies can be supplied as a single validated object, and evaluated without
# executing the guarded operation
breaker = CircuitBreaker(policy=CircuitBreakerPolicy(fail_max=2, reset_timeout=10, success_threshold=1))
evaluation: PolicyEvaluation = breaker.evaluate_policy(CircuitBreakerPolicy(fail_max=1))
revealed: bool | None = evaluation.would_open
revealed_missing: tuple[str, ...] = evaluation.missing_history
breaker.apply_policy_evaluation(evaluation)
