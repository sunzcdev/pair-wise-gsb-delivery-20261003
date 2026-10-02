package gobreaker

import (
	"sync"
	"time"
)

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
	permit, err := tscb.cb.acquireLease(0)
	if err != nil {
		return nil, err
	}

	return func(err error) {
		tscb.cb.settleLease(permit, err)
	}, nil
}

// Permit is a permission granted by AllowWithLease. The caller must eventually
// call Done or Abandon exactly once; subsequent calls have no effect, so
// completing the same permit more than once settles it only a single time.
// A permit whose lease deadline expires is settled automatically using the
// policy configured in Settings.LeaseExpiryAsFailure, and later Done or Abandon
// calls on it are ignored.
type Permit[T any] struct {
	tscb  *TwoStepCircuitBreaker[T]
	lease *lease
	once  sync.Once
}

// Done reports the outcome of the request. It is safe for concurrent use and
// only the first call is settled.
func (p *Permit[T]) Done(err error) {
	p.once.Do(func() {
		p.tscb.cb.settleLease(p.lease, err)
	})
}

// Abandon gives the permission back without recording a success or a failure.
// The half-open probe slot and the request count are released. It is safe for
// concurrent use and only the first call has an effect.
func (p *Permit[T]) Abandon() {
	p.once.Do(func() {
		p.tscb.cb.abandonLease(p.lease)
	})
}

// AllowWithLease is like Allow but grants a permission with a lease deadline.
// If the caller neither completes nor abandons the permission before ttl
// elapses, the permission is released automatically on the next breaker
// interaction at or after the deadline: either settled as a failure or merely
// abandoned according to Settings.LeaseExpiryAsFailure.
// A non-positive ttl grants a permission that never expires.
// The caller can complete the request with Permit.Done or give it up with
// Permit.Abandon; only the first of those calls takes effect.
func (tscb *TwoStepCircuitBreaker[T]) AllowWithLease(ttl time.Duration) (*Permit[T], error) {
	permit, err := tscb.cb.acquireLease(ttl)
	if err != nil {
		return nil, err
	}

	return &Permit[T]{
		tscb:  tscb,
		lease: permit,
	}, nil
}
