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

// ReconfigureStrategy determines how the remaining allowance is migrated
// to the new capacity when a limiter is reconfigured via Reconfigure.
type ReconfigureStrategy int

const (
	// PreserveAllowance keeps the absolute remaining allowance, clamped
	// to the new maximum capacity. An empty bucket stays empty; a full
	// bucket stays full only if the capacity did not shrink.
	PreserveAllowance ReconfigureStrategy = iota

	// PreserveFillRatio keeps the relative fill level of the bucket: a
	// bucket that was 60% full will be 60% full of the new capacity. An
	// empty bucket stays empty and a full bucket stays full. Scaling
	// always rounds down, so repeated reconfiguration can never create
	// additional allowance.
	PreserveFillRatio
)

func (s ReconfigureStrategy) valid() bool {
	return s == PreserveAllowance || s == PreserveFillRatio
}

// RateLimiter instances are thread-safe.
type RateLimiter struct {
	mu sync.Mutex

	rate, allowance, max, unit uint64
	lastCheck                  int64

	now func() int64
}

// New creates a new rate limiter instance
func New(rate int, per time.Duration) *RateLimiter {
	nano := uint64(per)
	if nano < 1 {
		nano = uint64(time.Second)
	}
	if rate < 1 {
		rate = 1
	}

	return newLimiter(rate, nano, nowNano)
}

func newLimiter(rate int, nano uint64, now func() int64) *RateLimiter {
	return &RateLimiter{
		rate:      uint64(rate),        // store the rate
		allowance: uint64(rate) * nano, // set our allowance to max in the beginning
		max:       uint64(rate) * nano, // remember our maximum allowance
		unit:      nano,                // remember our unit size

		lastCheck: now(),
		now:       now,
	}
}

// UpdateRate allows to update the allowed rate
//
// Deprecated: UpdateRate only adjusts the rate while keeping the period
// and the raw (unsettled) allowance untouched. Use Reconfigure for a
// precise, atomic switch of both rate and period with explicit balance
// migration semantics.
func (rl *RateLimiter) UpdateRate(rate int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.rate = uint64(rate)
	rl.max = uint64(rate) * rl.unit
}

// Reconfigure atomically switches the limiter to a new rate and period
// (burst capacity) without discarding the instance.
//
// The switch is performed in two steps:
//
//  1. The allowance is settled under the old configuration, i.e. accrued
//     up to the exact moment of the switch and capped at the old maximum.
//  2. The remaining balance is migrated to the new capacity according to
//     the given strategy (see PreserveAllowance and PreserveFillRatio).
//
// If any argument is invalid (rate < 1, per < 1ns, rate*per overflowing
// uint64, or an unknown strategy), Reconfigure is a no-op: the limiter
// is left completely untouched and false is returned.
//
// Reconfigure is fully serialized with Limit and Undo through the same
// mutex: every concurrent call is applied either entirely before or
// entirely after the reconfiguration, never interleaved with it.
func (rl *RateLimiter) Reconfigure(rate int, per time.Duration, strategy ReconfigureStrategy) bool {
	nano := uint64(per)
	if rate < 1 || nano < 1 || !strategy.valid() || uint64(rate) > math.MaxUint64/nano {
		return false
	}
	max := uint64(rate) * nano

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()

	// Step 1: settle the allowance under the old configuration.
	rl.settle(now)

	// Step 2: migrate the balance to the new capacity.
	if strategy == PreserveFillRatio {
		rl.allowance = scaleAllowance(rl.allowance, rl.max, max)
	}
	if rl.allowance > max {
		rl.allowance = max
	}

	rl.rate = uint64(rate)
	rl.unit = nano
	rl.max = max
	rl.lastCheck = now
	return true
}

// Limit returns true if rate was exceeded
func (rl *RateLimiter) Limit() bool {
	now := rl.now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.settle(now)
	current := rl.allowance

	// If our allowance is less than one unit, rate-limit!
	if current < rl.unit {
		return true
	}

	// Not limited, subtract a unit
	rl.allowance += ^(rl.unit - 1)
	return false
}

// settle accrues the allowance under the current configuration up to now
// and caps it at the current maximum. The caller must hold the lock.
func (rl *RateLimiter) settle(now int64) {
	// Calculate the number of ns that have passed since our last call
	passed := now - rl.lastCheck
	rl.lastCheck = now

	// Add them to our allowance
	rl.allowance += uint64(passed) * rl.rate

	// Ensure our allowance is not over maximum
	if current := rl.allowance; current > rl.max {
		rl.allowance += ^((current - rl.max) - 1)
	}
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

// scaleAllowance proportionally scales an allowance from an old to a new
// maximum capacity. The result never exceeds newMax and rounding is
// always towards zero, so repeated scaling cannot create allowance.
func scaleAllowance(allowance, oldMax, newMax uint64) uint64 {
	if allowance == 0 || oldMax == 0 || newMax == 0 {
		return 0
	}
	hi, lo := bits.Mul64(allowance, newMax)
	if hi >= oldMax {
		return newMax
	}
	q, _ := bits.Div64(hi, lo, oldMax)
	return q
}

func nowNano() int64 {
	return time.Now().UnixNano()
}
