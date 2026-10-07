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

// RateLimiter instances are thread-safe.
type RateLimiter struct {
	mu sync.Mutex

	rate, allowance, max, unit uint64
	lastCheck                  int64
	now                        func() int64
}

// New creates a new rate limiter instance
func New(rate int, per time.Duration) *RateLimiter {
	return NewWithClock(rate, per, nowNano)
}

// NewWithClock creates a new rate limiter instance with a custom clock.
// The clock must return the current time in Unix nanoseconds, it is
// primarily useful for testing.
func NewWithClock(rate int, per time.Duration, clock func() int64) *RateLimiter {
	nano := uint64(per)
	if nano < 1 {
		nano = uint64(time.Second)
	}
	if rate < 1 {
		rate = 1
	}
	if clock == nil {
		clock = nowNano
	}

	return &RateLimiter{
		rate:      uint64(rate),        // store the rate
		allowance: uint64(rate) * nano, // set our allowance to max in the beginning
		max:       uint64(rate) * nano, // remember our maximum allowance
		unit:      nano,                // remember our unit size
		now:       clock,               // remember our clock

		lastCheck: clock(),
	}
}

// UpdateRate allows to update the allowed rate.
//
// Deprecated: this legacy method only adjusts the rate and the maximum
// allowance. It neither settles the allowance accrued under the old
// configuration nor migrates the remaining balance, which may lead to
// surprising results. Use Reconfigure for a precise, atomic switch.
func (rl *RateLimiter) UpdateRate(rate int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.rate = uint64(rate)
	rl.max = uint64(rate) * rl.unit
}

// MigrationStrategy determines how the remaining allowance is carried
// over when a limiter is reconfigured via Reconfigure.
type MigrationStrategy int

const (
	// PreserveAllowance keeps the absolute remaining allowance (in
	// nanoseconds), clamped to the new maximum. An empty bucket stays
	// empty; a full bucket stays full when the capacity shrinks, and
	// keeps its absolute balance when the capacity grows.
	PreserveAllowance MigrationStrategy = iota

	// PreserveRatio keeps the fill ratio of the bucket, i.e. the
	// remaining allowance is scaled by newMax/oldMax. A full bucket
	// stays full, an empty bucket stays empty.
	PreserveRatio
)

// Reconfigure atomically switches the limiter to a new rate and period:
//
//  1. the allowance accrued under the old configuration is settled up to
//     the moment of the switch;
//  2. the remaining balance is migrated to the new capacity according to
//     the given strategy.
//
// If any argument is invalid (rate < 1, per < 1ns, rate*per overflowing
// uint64, or an unknown strategy), Reconfigure is a no-op and returns
// false. On success it returns true.
//
// Reconfigure is fully serialized with Limit and Undo through the same
// mutex: concurrent calls observe the configuration either entirely
// before or entirely after the switch, with the switch moment defined by
// the lock acquisition order.
//
// Each strategy is lossless-or-lossy on its own: repeatedly switching
// back and forth with a single strategy never creates allowance (the
// ratio strategy may lose sub-unit remainders to rounding). Alternating
// between strategies across switches is not guaranteed to be neutral.
func (rl *RateLimiter) Reconfigure(rate int, per time.Duration, strategy MigrationStrategy) bool {
	nano := uint64(per)
	if per < 1 || rate < 1 || uint64(rate) > math.MaxUint64/nano {
		return false
	}
	if strategy != PreserveAllowance && strategy != PreserveRatio {
		return false
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Settle the allowance under the old configuration, up to now.
	now := rl.now()
	if passed := now - rl.lastCheck; passed > 0 {
		rl.allowance += uint64(passed) * rl.rate
		if rl.allowance > rl.max {
			rl.allowance = rl.max
		}
	}
	rl.lastCheck = now

	// Migrate the balance to the new capacity.
	newMax := uint64(rate) * nano
	switch strategy {
	case PreserveRatio:
		hi, lo := bits.Mul64(rl.allowance, newMax)
		rl.allowance, _ = bits.Div64(hi, lo, rl.max)
	default: // PreserveAllowance
		if rl.allowance > newMax {
			rl.allowance = newMax
		}
	}

	rl.rate = uint64(rate)
	rl.unit = nano
	rl.max = newMax
	return true
}

// Limit returns true if rate was exceeded
func (rl *RateLimiter) Limit() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()

	// Calculate the number of ns that have passed since our last call
	passed := now - rl.lastCheck
	rl.lastCheck = now

	// Add them to our allowance
	rl.allowance += uint64(passed) * uint64(rl.rate)
	current := rl.allowance

	// Ensure our allowance is not over maximum
	if current > rl.max {
		rl.allowance += ^((current - rl.max) - 1)
		current = rl.max
	}

	// If our allowance is less than one unit, rate-limit!
	if current < rl.unit {
		return true
	}

	// Not limited, subtract a unit
	rl.allowance += ^(rl.unit - 1)
	return false
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

func nowNano() int64 {
	return time.Now().UnixNano()
}
