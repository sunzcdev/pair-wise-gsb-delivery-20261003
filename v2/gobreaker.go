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
//
// SlowRequestThreshold, when greater than 0, enables slow-call detection in
// addition to the error-based ReadyToTrip decision. A successful (non-excluded)
// request whose elapsed time is greater than or equal to SlowRequestThreshold
// is recorded as a slow request. Failed requests are not counted as slow: they
// are already accounted for by TotalFailures.
//
// Elapsed time is measured with the monotonic reading embedded in time.Time
// (i.e. time.Since the instant the breaker admitted the request), so wall
// clock adjustments do not distort latency. Requests are attributed to the
// rolling bucket in which they started (their admission time), not the one in
// which they complete. A request that starts in one bucket and finishes in a
// later bucket is recorded in its start bucket as long as that bucket is still
// inside the rolling window; completions older than the window are discarded.
//
// SlowRequestRatio is the minimum fraction of slow requests over the valid
// (non-excluded) requests that, together with MinSlowRequests, trips the
// breaker while it is closed. SlowRequestRatio must be in the range (0, 1].
//
// MinSlowRequests is the minimum number of valid requests that must be
// recorded before the slow-request ratio is evaluated, so that a handful of
// requests cannot trigger a trip. If MinSlowRequests is 0, the breaker uses
// defaultMinSlowRequests.
//
// Excluded requests are consistently excluded from every decision denominator:
// they count towards Requests and TotalExclusions, but they are neither
// successes nor failures and are never counted as slow, and the ratio uses
// Counts.Requests - Counts.TotalExclusions as its denominator.
//
// TreatSlowCallsAsFailuresWhenHalfOpen controls slow successes in the half-open
// state. It only takes effect when SlowRequestThreshold is greater than 0.
// When false (the default), a slow success counts as an ordinary success and
// still helps close the breaker. When true, a slow success is treated as a
// failure in the half-open state and immediately reopens the breaker.
type Settings struct {
	Name                                 string
	MaxRequests                          uint32
	Interval                             time.Duration
	BucketPeriod                         time.Duration
	Timeout                              time.Duration
	ReadyToTrip                          func(counts Counts) bool
	OnStateChange                        func(name string, from State, to State)
	IsSuccessful                         func(err error) bool
	IsExcluded                           func(err error) bool
	SlowRequestThreshold                 time.Duration
	SlowRequestRatio                     float64
	MinSlowRequests                      uint32
	TreatSlowCallsAsFailuresWhenHalfOpen bool
}

// CircuitBreaker is a state machine to prevent sending requests that are likely to fail.
type CircuitBreaker[T any] struct {
	name                                 string
	maxRequests                          uint32
	interval                             time.Duration
	bucketPeriod                         time.Duration
	timeout                              time.Duration
	readyToTrip                          func(counts Counts) bool
	isSuccessful                         func(err error) bool
	isExcluded                           func(err error) bool
	onStateChange                        func(name string, from State, to State)
	slowRequestThreshold                 time.Duration
	slowRequestRatio                     float64
	minSlowRequests                      uint32
	treatSlowCallsAsFailuresWhenHalfOpen bool

	mutex      sync.Mutex
	state      State
	generation uint64
	counts     *rollingCounts
	start      time.Time
	expiry     time.Time
	now        func() time.Time
}

// NewCircuitBreaker returns a new CircuitBreaker configured with the given Settings.
func NewCircuitBreaker[T any](st Settings) *CircuitBreaker[T] {
	cb := new(CircuitBreaker[T])

	cb.name = st.Name
	cb.onStateChange = st.OnStateChange

	if st.MaxRequests == 0 {
		cb.maxRequests = 1
	} else {
		cb.maxRequests = st.MaxRequests
	}

	var numBuckets int64
	if st.Interval <= 0 {
		cb.interval = defaultInterval
		cb.bucketPeriod = cb.interval
		numBuckets = 1
	} else if st.BucketPeriod <= 0 {
		cb.interval = st.Interval
		cb.bucketPeriod = cb.interval
		numBuckets = 1
	} else {
		cb.interval = (st.Interval + st.BucketPeriod - 1) / st.BucketPeriod * st.BucketPeriod
		cb.bucketPeriod = st.BucketPeriod
		numBuckets = int64(cb.interval / cb.bucketPeriod)
	}

	cb.counts = newRollingCounts(numBuckets)

	if st.Timeout <= 0 {
		cb.timeout = defaultTimeout
	} else {
		cb.timeout = st.Timeout
	}

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

	if st.SlowRequestThreshold > 0 {
		cb.slowRequestThreshold = st.SlowRequestThreshold
		cb.slowRequestRatio = st.SlowRequestRatio
		if st.MinSlowRequests == 0 {
			cb.minSlowRequests = defaultMinSlowRequests
		} else {
			cb.minSlowRequests = st.MinSlowRequests
		}
		cb.treatSlowCallsAsFailuresWhenHalfOpen = st.TreatSlowCallsAsFailuresWhenHalfOpen
	}

	cb.now = time.Now

	cb.toNewGeneration(cb.now())

	return cb
}

const defaultInterval = time.Duration(0) * time.Second
const defaultTimeout = time.Duration(60) * time.Second
const defaultMinSlowRequests uint32 = 5

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

	now := cb.now()
	state, _, _ := cb.currentState(now)
	return state
}

// Counts returns internal counters
func (cb *CircuitBreaker[T]) Counts() Counts {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	return cb.counts.Counts
}

// Execute runs the given request if the CircuitBreaker accepts it.
// Execute returns an error instantly if the CircuitBreaker rejects the request.
// Otherwise, Execute returns the result of the request.
// If a panic occurs in the request, the CircuitBreaker handles it as an error
// and causes the same panic again.
func (cb *CircuitBreaker[T]) Execute(req func() (T, error)) (T, error) {
	generation, age, startedAt, err := cb.beforeRequest()
	if err != nil {
		var defaultValue T
		return defaultValue, err
	}

	defer func() {
		e := recover()
		if e != nil {
			cb.afterRequest(generation, age, startedAt, fmt.Errorf("%v", e))
			panic(e)
		}
	}()

	result, err := req()
	cb.afterRequest(generation, age, startedAt, err)
	return result, err
}

func (cb *CircuitBreaker[T]) beforeRequest() (uint64, uint64, time.Time, error) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	now := cb.now()
	state, generation, age := cb.currentState(now)

	if state == StateOpen {
		return generation, age, now, ErrOpenState
	} else if state == StateHalfOpen && cb.counts.validRequests() >= cb.maxRequests {
		return generation, age, now, ErrTooManyRequests
	}

	cb.counts.onRequest()
	return generation, age, now, nil
}

func (cb *CircuitBreaker[T]) afterRequest(previous uint64, age uint64, startedAt time.Time, err error) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	now := cb.now()
	state, generation, _ := cb.currentState(now)
	if generation != previous {
		return
	}

	if cb.isExcluded(err) {
		cb.onExclusion(age)
		return
	}

	// Elapsed time uses the monotonic clock readings captured at admission and
	// completion, so it is unaffected by wall clock changes. A request is slow
	// only when it succeeded and met the threshold; failures are not double
	// counted as slow.
	slow := cb.slowRequestThreshold > 0 && cb.isSuccessful(err) &&
		now.Sub(startedAt) >= cb.slowRequestThreshold

	if cb.isSuccessful(err) {
		cb.onSuccess(state, age, now, slow)
	} else {
		cb.onFailure(state, age, now)
	}
}

func (cb *CircuitBreaker[T]) onSuccess(state State, age uint64, now time.Time, slow bool) {
	switch state {
	case StateClosed:
		cb.counts.onSuccess(age)
		if slow {
			cb.counts.onSlow(age)
		}
		if cb.shouldTripOnSlowRequests() {
			cb.setState(StateOpen, now)
		}
	case StateHalfOpen:
		if slow && cb.treatSlowCallsAsFailuresWhenHalfOpen {
			cb.setState(StateOpen, now)
			return
		}
		cb.counts.onSuccess(age)
		if slow {
			cb.counts.onSlow(age)
		}
		if cb.counts.ConsecutiveSuccesses >= cb.maxRequests {
			cb.setState(StateClosed, now)
		}
	}
}

// shouldTripOnSlowRequests applies the optional slow-call rule in the closed
// state. It is independent of ReadyToTrip, which keeps governing the error
// success rate. The denominator is the valid (non-excluded) request count,
// identical to the denominator used for the error ratio, so excluded requests
// never influence the decision.
func (cb *CircuitBreaker[T]) shouldTripOnSlowRequests() bool {
	if cb.slowRequestThreshold <= 0 || cb.slowRequestRatio <= 0 {
		return false
	}

	numReqs := cb.counts.validRequests()
	if numReqs < cb.minSlowRequests {
		return false
	}

	slowRatio := float64(cb.counts.TotalSlowRequests) / float64(numReqs)
	return slowRatio >= cb.slowRequestRatio
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
