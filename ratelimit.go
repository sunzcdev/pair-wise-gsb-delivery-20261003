/*
Simple, thread-safe Go rate-limiter.
Inspired by Antti Huima's algorithm on http://stackoverflow.com/a/668327

Example:

	// Create a new rate-limiter, allowing up-to 10 calls
	// per second
	rl := ratelimit.New(10, time.Second)

	for i:=0; i<20; i++ {
	  if rl.Limit() {
	    fmt.Println("DOH! Over limit!")
	  } else {
	    fmt.Println("OK")
	  }
	}
*/
package ratelimit

import (
	"math"
	"math/bits"
	"sync"
	"time"
)

// Clock abstracts the time source used by a RateLimiter.
// It allows tests to inject a fake clock instead of relying on real sleeps.
type Clock interface {
	Now() time.Time
}

// ClockFunc allows a plain function to be used as a Clock.
type ClockFunc func() time.Time

// Now implements the Clock interface.
func (f ClockFunc) Now() time.Time { return f() }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RateLimiter instances are thread-safe.
type RateLimiter struct {
	mu sync.Mutex

	rate, allowance, max, unit uint64
	lastCheck                  int64
	clock                      Clock
}

// Option customises a RateLimiter on construction.
type Option func(*RateLimiter)

// WithClock overrides the time source of the limiter, primarily for testing.
func WithClock(c Clock) Option {
	return func(rl *RateLimiter) {
		if c != nil {
			rl.clock = c
		}
	}
}

// New creates a new rate limiter instance
func New(rate int, per time.Duration, opts ...Option) *RateLimiter {
	nano := uint64(per)
	if nano < 1 {
		nano = uint64(time.Second)
	}
	if rate < 1 {
		rate = 1
	}

	rl := &RateLimiter{
		rate:      uint64(rate),               // store the rate
		allowance: mulSat(uint64(rate), nano), // set our allowance to max in the beginning
		max:       mulSat(uint64(rate), nano), // remember our maximum allowance
		unit:      nano,                       // remember our unit size
		clock:     realClock{},
	}
	for _, opt := range opts {
		opt(rl)
	}
	rl.lastCheck = rl.clock.Now().UnixNano()
	return rl
}

// UpdateRate allows to update the allowed rate
func (rl *RateLimiter) UpdateRate(rate int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.rate = uint64(rate)
	rl.max = mulSat(uint64(rate), rl.unit)
}

// Limit returns true if rate was exceeded
func (rl *RateLimiter) Limit() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock.Now().UnixNano()

	// Add the ns that have passed since our last call to our allowance
	rl.allowance = rl.allowanceAt(now)
	rl.lastCheck = now

	// If our allowance is less than one unit, rate-limit!
	if rl.allowance < rl.unit {
		return true
	}

	// Not limited, subtract a unit
	rl.allowance -= rl.unit
	return false
}

// Take attempts to consume n units in a single, all-or-nothing operation:
// either the full cost is deducted or nothing is deducted at all.
//
// It returns whether the request was accepted and the earliest time at which
// a request of cost n can be accepted given the current rate. On success that
// time is the current time.
//
// A zero cost always succeeds and consumes nothing. A cost exceeding the
// bucket capacity can never be satisfied; it is rejected and the returned
// time is the zero value of time.Time.
func (rl *RateLimiter) Take(n uint64) (bool, time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock.Now()
	allowance := rl.allowanceAt(now.UnixNano())

	ok, at := rl.judge(n, allowance, now)

	// Commit the accrued refill; on success also deduct the cost.
	rl.allowance = allowance
	rl.lastCheck = now.UnixNano()
	if ok {
		rl.allowance -= n * rl.unit // safe: judge verified n*unit <= allowance
	}
	return ok, at
}

// Estimate reports whether a request of cost n could be accepted right now
// and the earliest time at which it would be accepted, without consuming any
// allowance. It never pre-spends quota, so repeated calls are idempotent.
func (rl *RateLimiter) Estimate(n uint64) (bool, time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock.Now()
	return rl.judge(n, rl.allowanceAt(now.UnixNano()), now)
}

// judge evaluates a request of cost n against the given allowance.
// Callers must hold the lock.
func (rl *RateLimiter) judge(n uint64, allowance uint64, now time.Time) (bool, time.Time) {
	// Reject costs that overflow or exceed the bucket capacity; they can
	// never be satisfied, no matter how long one waits.
	hi, cost := bits.Mul64(n, rl.unit)
	if hi != 0 || cost > rl.max {
		return false, time.Time{}
	}

	if cost <= allowance {
		return true, now
	}

	// Compute how long it takes to accrue the missing allowance.
	deficit := cost - allowance
	wait := deficit / rl.rate
	if deficit%rl.rate != 0 {
		wait++
	}
	if wait > math.MaxInt64 {
		wait = math.MaxInt64
	}
	return false, now.Add(time.Duration(wait))
}

// allowanceAt returns the effective allowance at the given time, refilling
// for the elapsed period and capping the result at the bucket capacity, so a
// long idle period can never push the allowance beyond max or overflow.
// Callers must hold the lock.
func (rl *RateLimiter) allowanceAt(now int64) uint64 {
	allowance := rl.allowance
	if allowance > rl.max {
		allowance = rl.max
	}

	passed := now - rl.lastCheck
	if passed <= 0 {
		return allowance
	}

	// Fully refilled once enough time has passed; avoids the overflow of
	// passed*rate for very long idle periods.
	if uint64(passed) >= (rl.max-allowance)/rl.rate+1 {
		return rl.max
	}
	return allowance + uint64(passed)*rl.rate
}

// Undo reverts the last Limit() call, returning consumed allowance
func (rl *RateLimiter) Undo() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.allowance += rl.unit

	// Ensure our allowance is not over maximum
	if current := rl.allowance; current > rl.max {
		rl.allowance += ^((current - rl.max) - 1)
	}
}

// mulSat returns a*b, saturating on overflow.
func mulSat(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		return math.MaxUint64
	}
	return lo
}
