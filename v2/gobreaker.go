// Package gobreaker implements the Circuit Breaker pattern.
// See https://msdn.microsoft.com/en-us/library/dn589784.aspx.
package gobreaker

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// State is a type that represents a state of CircuitBreaker.
type State int

// These constants are states of CircuitBreaker.
const (
	StateClosed State = iota
	StateHalfOpen
	StateOpen
)

var (
	// ErrTooManyRequests is returned when the CB state is half open and the requests count is over the cb maxRequests
	ErrTooManyRequests = errors.New("too many requests")
	// ErrOpenState is returned when the CB state is open
	ErrOpenState = errors.New("circuit breaker is open")
)

// String implements stringer interface.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateHalfOpen:
		return "half-open"
	case StateOpen:
		return "open"
	default:
		return fmt.Sprintf("unknown state: %d", s)
	}
}

// Settings configures CircuitBreaker:
//
// Name is the name of the CircuitBreaker.
//
// MaxRequests is the maximum number of requests allowed to pass through
// when the CircuitBreaker is half-open.
// If MaxRequests is 0, the CircuitBreaker allows only 1 request.
//
// Interval is the cyclic period of the closed state
// for the CircuitBreaker to clear the internal Counts.
// If Interval is less than or equal to 0, the CircuitBreaker does not clear internal Counts during the closed state.
//
// BucketPeriod defines the time duration for each bucket in the rolling window strategy.
// The internal Counts will be updated and reset gradually for each bucket.
// Interval will be automatically adjusted to be a multiple of BucketPeriod.
// If BucketPeriod is less than or equal to 0, the CircuitBreaker will use a fixed window strategy instead.
//
// Timeout is the period of the open state,
// after which the state of the CircuitBreaker becomes half-open.
// If Timeout is less than or equal to 0, the timeout value of the CircuitBreaker is set to 60 seconds.
//
// ReadyToTrip is called with a copy of Counts whenever a request fails in the closed state.
// If ReadyToTrip returns true, the CircuitBreaker will be placed into the open state.
// If ReadyToTrip is nil, default ReadyToTrip is used.
// Default ReadyToTrip returns true when the number of consecutive failures is more than 5.
//
// OnStateChange is called whenever the state of the CircuitBreaker changes.
//
// IsSuccessful is called with the error returned from a request.
// If IsSuccessful returns true, the error is counted as a success.
// Otherwise the error is counted as a failure.
// If IsSuccessful is nil, default IsSuccessful is used, which returns false for all non-nil errors.
//
// IsExcluded determines whether a request error should be ignored
// for the purposes of updating the circuit breaker metrics.
// If IsExcluded returns true for a given error,
// the request is neither counted as a success nor as a failure.
// This can be used, for example, to ignore context cancellations or
// other errors that should not affect the circuit breaker state.
// If IsExcluded is nil, no requests are excluded.
type Settings struct {
	Name          string
	MaxRequests   uint32
	Interval      time.Duration
	BucketPeriod  time.Duration
	Timeout       time.Duration
	ReadyToTrip   func(counts Counts) bool
	OnStateChange func(name string, from State, to State)
	IsSuccessful  func(err error) bool
	IsExcluded    func(err error) bool
}

// Policy groups the mutable settings of a CircuitBreaker that can be
// changed at runtime via UpdatePolicy.
//
// MaxRequests is the maximum number of requests allowed to pass through
// when the CircuitBreaker is half-open. If MaxRequests is 0, only 1 request
// is allowed.
//
// Interval is the cyclic period of the closed state
// for the CircuitBreaker to clear the internal Counts.
// If Interval is 0, the CircuitBreaker does not clear internal Counts during the closed state.
//
// BucketPeriod defines the time duration for each bucket in the rolling window strategy.
// Interval is automatically adjusted to be a multiple of BucketPeriod.
// If BucketPeriod is 0, the CircuitBreaker uses a fixed window strategy instead.
//
// Timeout is the period of the open state,
// after which the state of the CircuitBreaker becomes half-open.
// If Timeout is 0, the timeout value of the CircuitBreaker is set to 60 seconds.
type Policy struct {
	MaxRequests  uint32
	Interval     time.Duration
	BucketPeriod time.Duration
	Timeout      time.Duration
}

// validate checks the policy as a whole before it is applied.
func (p Policy) validate() error {
	if p.Interval < 0 {
		return fmt.Errorf("gobreaker: invalid policy: Interval must not be negative: %v", p.Interval)
	}
	if p.BucketPeriod < 0 {
		return fmt.Errorf("gobreaker: invalid policy: BucketPeriod must not be negative: %v", p.BucketPeriod)
	}
	if p.Timeout < 0 {
		return fmt.Errorf("gobreaker: invalid policy: Timeout must not be negative: %v", p.Timeout)
	}
	return nil
}

// normalizedPolicy is the effective form of a Policy after applying defaults.
type normalizedPolicy struct {
	maxRequests  uint32
	interval     time.Duration
	bucketPeriod time.Duration
	timeout      time.Duration
	numBuckets   int64
}

func (p Policy) normalize() normalizedPolicy {
	np := normalizedPolicy{maxRequests: p.MaxRequests}
	if np.maxRequests == 0 {
		np.maxRequests = 1
	}

	switch {
	case p.Interval <= 0:
		np.interval = defaultInterval
		np.bucketPeriod = np.interval
		np.numBuckets = 1
	case p.BucketPeriod <= 0:
		np.interval = p.Interval
		np.bucketPeriod = np.interval
		np.numBuckets = 1
	default:
		np.interval = (p.Interval + p.BucketPeriod - 1) / p.BucketPeriod * p.BucketPeriod
		np.bucketPeriod = p.BucketPeriod
		np.numBuckets = int64(np.interval / np.bucketPeriod)
	}

	np.timeout = p.Timeout
	if np.timeout <= 0 {
		np.timeout = defaultTimeout
	}

	return np
}

// CircuitBreaker is a state machine to prevent sending requests that are likely to fail.
type CircuitBreaker[T any] struct {
	name          string
	maxRequests   uint32
	interval      time.Duration
	bucketPeriod  time.Duration
	timeout       time.Duration
	readyToTrip   func(counts Counts) bool
	isSuccessful  func(err error) bool
	isExcluded    func(err error) bool
	onStateChange func(name string, from State, to State)

	mutex      sync.Mutex
	state      State
	generation uint64
	counts     *rollingCounts
	start      time.Time
	expiry     time.Time
}

// NewCircuitBreaker returns a new CircuitBreaker configured with the given Settings.
func NewCircuitBreaker[T any](st Settings) *CircuitBreaker[T] {
	cb := new(CircuitBreaker[T])

	cb.name = st.Name
	cb.onStateChange = st.OnStateChange

	np := Policy{
		MaxRequests:  st.MaxRequests,
		Interval:     st.Interval,
		BucketPeriod: st.BucketPeriod,
		Timeout:      st.Timeout,
	}.normalize()
	cb.maxRequests = np.maxRequests
	cb.interval = np.interval
	cb.bucketPeriod = np.bucketPeriod
	cb.timeout = np.timeout
	cb.counts = newRollingCounts(np.numBuckets)

	if st.ReadyToTrip == nil {
		cb.readyToTrip = defaultReadyToTrip
	} else {
		cb.readyToTrip = st.ReadyToTrip
	}

	if st.IsSuccessful == nil {
		cb.isSuccessful = defaultIsSuccessful
	} else {
		cb.isSuccessful = st.IsSuccessful
	}

	if st.IsExcluded == nil {
		cb.isExcluded = defaultIsExcluded
	} else {
		cb.isExcluded = st.IsExcluded
	}

	cb.toNewGeneration(time.Now())

	return cb
}

const defaultInterval = time.Duration(0) * time.Second
const defaultTimeout = time.Duration(60) * time.Second

func defaultReadyToTrip(counts Counts) bool {
	return counts.ConsecutiveFailures > 5
}

func defaultIsSuccessful(err error) bool {
	return err == nil
}

func defaultIsExcluded(_ error) bool {
	return false
}

// Name returns the name of the CircuitBreaker.
func (cb *CircuitBreaker[T]) Name() string {
	return cb.name
}

// State returns the current state of the CircuitBreaker.
func (cb *CircuitBreaker[T]) State() State {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	now := time.Now()
	state, _, _ := cb.currentState(now)
	return state
}

// Counts returns internal counters
func (cb *CircuitBreaker[T]) Counts() Counts {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	return cb.counts.Counts
}

// Policy returns the effective policy currently applied to the CircuitBreaker.
// The returned value is a copy; modifying it does not affect the CircuitBreaker.
func (cb *CircuitBreaker[T]) Policy() Policy {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	return Policy{
		MaxRequests:  cb.maxRequests,
		Interval:     cb.interval,
		BucketPeriod: cb.bucketPeriod,
		Timeout:      cb.timeout,
	}
}

// UpdatePolicy validates the given policy as a whole and, only if it is valid,
// applies it atomically. If the policy is invalid, UpdatePolicy returns an
// error and the CircuitBreaker is left completely unchanged.
//
// preserveCounts selects whether the counts accumulated in the closed state
// are kept across the update. Counts are preserved only in the closed state;
// in the half-open and open states they are always reset. When the window
// layout (Interval/BucketPeriod) changes, preserved counts are consolidated
// into the current bucket of the new layout; the old buckets are never
// spliced into a layout they do not belong to.
//
// Generation rule: every successful update starts a new generation.
// A request that begins before an update and finishes after it belongs to
// the old generation, and its result is discarded.
func (cb *CircuitBreaker[T]) UpdatePolicy(p Policy, preserveCounts bool) error {
	if err := p.validate(); err != nil {
		return err
	}
	np := p.normalize()

	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	now := time.Now()
	cb.currentState(now)

	layoutChanged := np.bucketPeriod != cb.bucketPeriod ||
		np.numBuckets != int64(len(cb.counts.buckets))

	cb.maxRequests = np.maxRequests
	cb.interval = np.interval
	cb.bucketPeriod = np.bucketPeriod
	cb.timeout = np.timeout

	cb.generation++

	if cb.state == StateClosed && preserveCounts {
		if layoutChanged {
			preserved := cb.counts.Counts
			cb.counts = newRollingCounts(np.numBuckets)
			cb.counts.Counts = preserved
			cb.counts.buckets[cb.counts.current()] = preserved
			cb.start = now
		}
	} else {
		cb.counts = newRollingCounts(np.numBuckets)
		cb.start = now
	}

	var zero time.Time
	switch cb.state {
	case StateClosed:
		if cb.interval == 0 || len(cb.counts.buckets) >= 2 {
			cb.expiry = zero
		} else {
			cb.expiry = cb.start.Add(cb.interval)
		}
	case StateOpen:
		cb.expiry = now.Add(cb.timeout)
	default: // StateHalfOpen
		cb.expiry = zero
	}

	return nil
}

// Execute runs the given request if the CircuitBreaker accepts it.
// Execute returns an error instantly if the CircuitBreaker rejects the request.
// Otherwise, Execute returns the result of the request.
// If a panic occurs in the request, the CircuitBreaker handles it as an error
// and causes the same panic again.
func (cb *CircuitBreaker[T]) Execute(req func() (T, error)) (T, error) {
	generation, age, err := cb.beforeRequest()
	if err != nil {
		var defaultValue T
		return defaultValue, err
	}

	defer func() {
		e := recover()
		if e != nil {
			cb.afterRequest(generation, age, fmt.Errorf("%v", e))
			panic(e)
		}
	}()

	result, err := req()
	cb.afterRequest(generation, age, err)
	return result, err
}

func (cb *CircuitBreaker[T]) beforeRequest() (uint64, uint64, error) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	now := time.Now()
	state, generation, age := cb.currentState(now)

	if state == StateOpen {
		return generation, age, ErrOpenState
	} else if state == StateHalfOpen && cb.counts.validRequests() >= cb.maxRequests {
		return generation, age, ErrTooManyRequests
	}

	cb.counts.onRequest()
	return generation, age, nil
}

func (cb *CircuitBreaker[T]) afterRequest(previous uint64, age uint64, err error) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	now := time.Now()
	state, generation, _ := cb.currentState(now)
	if generation != previous {
		return
	}

	if cb.isExcluded(err) {
		cb.onExclusion(age)
		return
	}

	if cb.isSuccessful(err) {
		cb.onSuccess(state, age, now)
	} else {
		cb.onFailure(state, age, now)
	}
}

func (cb *CircuitBreaker[T]) onSuccess(state State, age uint64, now time.Time) {
	switch state {
	case StateClosed:
		cb.counts.onSuccess(age)
	case StateHalfOpen:
		cb.counts.onSuccess(age)
		if cb.counts.ConsecutiveSuccesses >= cb.maxRequests {
			cb.setState(StateClosed, now)
		}
	}
}

func (cb *CircuitBreaker[T]) onFailure(state State, age uint64, now time.Time) {
	switch state {
	case StateClosed:
		cb.counts.onFailure(age)
		if cb.readyToTrip(cb.counts.Counts) {
			cb.setState(StateOpen, now)
		}
	case StateHalfOpen:
		cb.setState(StateOpen, now)
	}
}

func (cb *CircuitBreaker[T]) onExclusion(age uint64) {
	cb.counts.onExclusion(age)
}

func (cb *CircuitBreaker[T]) currentState(now time.Time) (State, uint64, uint64) {
	switch cb.state {
	case StateClosed:
		if !cb.expiry.IsZero() && cb.expiry.Before(now) {
			cb.toNewGeneration(now)
		} else if len(cb.counts.buckets) >= 2 {
			cb.counts.grow(cb.age(now))
		}
	case StateOpen:
		if cb.expiry.Before(now) {
			cb.setState(StateHalfOpen, now)
		}
	}
	return cb.state, cb.generation, cb.counts.age
}

func (cb *CircuitBreaker[T]) age(now time.Time) uint64 {
	if cb.bucketPeriod == 0 {
		return 0
	}

	elapsed := now.Sub(cb.start)
	age := int64(elapsed / cb.bucketPeriod)
	if age < 0 {
		return 0
	}
	return uint64(age)
}

func (cb *CircuitBreaker[T]) setState(state State, now time.Time) {
	if cb.state == state {
		return
	}

	prev := cb.state
	cb.state = state

	cb.toNewGeneration(now)

	if cb.onStateChange != nil {
		cb.onStateChange(cb.name, prev, state)
	}
}

func (cb *CircuitBreaker[T]) toNewGeneration(now time.Time) {
	cb.generation++
	cb.start = now
	cb.counts.clear()

	var zero time.Time
	switch cb.state {
	case StateClosed:
		if cb.interval == 0 || len(cb.counts.buckets) >= 2 {
			cb.expiry = zero
		} else {
			cb.expiry = now.Add(cb.interval)
		}
	case StateOpen:
		cb.expiry = now.Add(cb.timeout)
	default: // StateHalfOpen
		cb.expiry = zero
	}
}
