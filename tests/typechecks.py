from pybreaker import (
    STATE_CLOSED,
    CircuitBreaker,
    CircuitBreakerSnapshot,
    CircuitBreakerStorageUnsupportedError,
    CircuitMemoryStorage,
)

# issue #90: incorrect typing for exclude argument
# this should not give errors in mypy
CircuitBreaker(
    fail_max=1,
    reset_timeout=1000,
    exclude=[lambda e: isinstance(e, RuntimeError)],
    state_storage=CircuitMemoryStorage(STATE_CLOSED),
)

# Version-conditional sharing mode and the snapshot/storage-error API.
storage = CircuitMemoryStorage(STATE_CLOSED)
breaker = CircuitBreaker(fail_max=3, state_storage=storage, conditional_updates=True)
assert breaker.conditional_updates is True

snapshot: CircuitBreakerSnapshot = storage.get_snapshot()
reveal_type(snapshot.state)
reveal_type(snapshot.fail_counter)
reveal_type(snapshot.success_counter)
reveal_type(snapshot.version)

next_snapshot: CircuitBreakerSnapshot = storage.conditional_increment_counter(snapshot.version)
swapped: CircuitBreakerSnapshot = storage.compare_and_swap_state(
    next_snapshot.version, STATE_CLOSED, opened_at=None, fail_counter=0, success_counter=0
)
assert swapped.version >= 0

try:
    CircuitBreaker(state_storage=storage, conditional_updates=True)
except CircuitBreakerStorageUnsupportedError:
    pass
