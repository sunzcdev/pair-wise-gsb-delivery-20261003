package gobreaker

import "time"

// TwoStepCircuitBreaker is like CircuitBreaker but instead of surrounding a function
// with the breaker functionality, it only checks whether a request can proceed and
// expects the caller to report the outcome in a separate step using a callback.
type TwoStepCircuitBreaker[T any] struct {
	cb *CircuitBreaker[T]
}

// NewTwoStepCircuitBreaker returns a new TwoStepCircuitBreaker configured with the given Settings.
func NewTwoStepCircuitBreaker[T any](st Settings) *TwoStepCircuitBreaker[T] {
	return &TwoStepCircuitBreaker[T]{
		cb: NewCircuitBreaker[T](st),
	}
}

// Name returns the name of the TwoStepCircuitBreaker.
func (tscb *TwoStepCircuitBreaker[T]) Name() string {
	return tscb.cb.Name()
}

// State returns the current state of the TwoStepCircuitBreaker.
func (tscb *TwoStepCircuitBreaker[T]) State() State {
	return tscb.cb.State()
}

// Counts returns internal counters
func (tscb *TwoStepCircuitBreaker[T]) Counts() Counts {
	return tscb.cb.Counts()
}

// Allow checks if a new request can proceed.
// If the circuit breaker allow the new request, it returns a callback
// used to register the error object that the request will return in a separate step.
// If the circuit breaker does not allow the request, it returns an error.
func (tscb *TwoStepCircuitBreaker[T]) Allow() (done func(err error), err error) {
	generation, age, err := tscb.cb.beforeRequest()
	if err != nil {
		return nil, err
	}

	return func(err error) {
		tscb.cb.afterRequest(generation, age, err)
	}, nil
}

// AllowWithLease checks if a new request can proceed, like Allow,
// but returns a Lease instead of a plain callback.
// The lease must be settled by the caller with Done or Abandon
// before leaseDuration elapses; a non-positive leaseDuration expires immediately.
// Once the lease expires, the circuit breaker settles it automatically
// (as a failure or as an abandonment, depending on Settings.LeaseExpirationIsFailure)
// and releases the half-open slot it occupied.
// If the circuit breaker does not allow the request, it returns an error.
func (tscb *TwoStepCircuitBreaker[T]) AllowWithLease(leaseDuration time.Duration) (*Lease[T], error) {
	return tscb.cb.beforeRequestWithLease(leaseDuration)
}

// Lease is a permit obtained from TwoStepCircuitBreaker.AllowWithLease.
// A lease is settled exactly once: the first of Done, Abandon or expiration wins,
// and any later settlement attempt is ignored.
// A lease is also ignored once the circuit breaker has moved to a new generation,
// so a stale lease can never affect a new generation of the circuit breaker.
type Lease[T any] struct {
	cb         *CircuitBreaker[T]
	generation uint64
	age        uint64
	deadline   time.Time
	settled    bool // guarded by cb.mutex
}

// Done completes the lease, registering the given error
// as the outcome of the request, exactly like the callback returned by Allow.
func (l *Lease[T]) Done(err error) {
	l.cb.completeLease(l, err)
}

// Abandon releases the lease without recording a success or a failure.
// The request is excluded from the counts and its half-open slot is released.
func (l *Lease[T]) Abandon() {
	l.cb.abandonLease(l)
}

func (cb *CircuitBreaker[T]) beforeRequestWithLease(leaseDuration time.Duration) (*Lease[T], error) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	now := cb.clock()
	cb.expireLeases(now)
	state, generation, age := cb.currentState(now)

	if state == StateOpen {
		return nil, ErrOpenState
	} else if state == StateHalfOpen && cb.counts.validRequests() >= cb.maxRequests {
		return nil, ErrTooManyRequests
	}

	cb.counts.onRequest()

	lease := &Lease[T]{
		cb:         cb,
		generation: generation,
		age:        age,
		deadline:   now.Add(leaseDuration),
	}
	if cb.leases == nil {
		cb.leases = make(map[*Lease[T]]struct{})
	}
	cb.leases[lease] = struct{}{}
	return lease, nil
}

func (cb *CircuitBreaker[T]) completeLease(lease *Lease[T], err error) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	now := cb.clock()
	cb.expireLeases(now)
	if !cb.settleLease(lease) {
		return
	}
	cb.afterRequestLocked(lease.generation, lease.age, err, now)
}

func (cb *CircuitBreaker[T]) abandonLease(lease *Lease[T]) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	now := cb.clock()
	cb.expireLeases(now)
	if !cb.settleLease(lease) {
		return
	}

	_, generation, _ := cb.currentState(now)
	if generation != lease.generation {
		return
	}
	cb.onExclusion(lease.age)
}

// settleLease marks the lease as settled and stops tracking it.
// It reports false if the lease was already settled.
func (cb *CircuitBreaker[T]) settleLease(lease *Lease[T]) bool {
	if lease.settled {
		return false
	}
	lease.settled = true
	delete(cb.leases, lease)
	return true
}

// expireLeases settles every lease whose deadline has passed.
// The caller must hold cb.mutex.
func (cb *CircuitBreaker[T]) expireLeases(now time.Time) {
	for lease := range cb.leases {
		if lease.settled || now.Before(lease.deadline) {
			continue
		}
		cb.settleLease(lease)
		cb.expireLease(lease, now)
	}
}

func (cb *CircuitBreaker[T]) expireLease(lease *Lease[T], now time.Time) {
	state, generation, _ := cb.currentState(now)
	if generation != lease.generation {
		return
	}

	if cb.leaseExpirationIsFailure {
		cb.onFailure(state, lease.age, now)
	} else {
		cb.onExclusion(lease.age)
	}
}
