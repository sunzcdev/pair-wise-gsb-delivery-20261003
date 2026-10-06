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
	"sync"
	"time"
)

// RateLimiter instances are thread-safe.
type RateLimiter struct {
	mu sync.Mutex

	rate, allowance, max, unit uint64
	lastCheck                  int64
	now                        func() time.Time
}

// New creates a new rate limiter instance
func New(rate int, per time.Duration) *RateLimiter {
	return newWithClock(rate, per, systemClock{})
}

type clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

func newWithClock(rate int, per time.Duration, c clock) *RateLimiter {
	nano := uint64(per)
	if nano < 1 {
		nano = uint64(time.Second)
	}
	if rate < 1 {
		rate = 1
	}

	return &RateLimiter{
		rate:      uint64(rate),        // store the rate
		allowance: uint64(rate) * nano, // set our allowance to max in the beginning
		max:       uint64(rate) * nano, // remember our maximum allowance
		unit:      nano,                // remember our unit size
		lastCheck: c.Now().UnixNano(),
		now:       c.Now,
	}
}

// UpdateRate allows to update the allowed rate
func (rl *RateLimiter) UpdateRate(rate int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.rate = uint64(rate)
	rl.max = uint64(rate) * rl.unit
}

// Limit returns true if rate was exceeded
func (rl *RateLimiter) Limit() bool {
	now := rl.nano()

	rl.mu.Lock()
	defer rl.mu.Unlock()

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

func timeNowNano() int64 {
	return time.Now().UnixNano()
}
