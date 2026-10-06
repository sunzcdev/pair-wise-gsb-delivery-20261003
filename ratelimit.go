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

A single export request may consume hundreds of units. Instead of calling
Limit in a loop, use TakeN to atomically deduct N units in one call:

	if allowed, earliest := rl.TakeN(500); !allowed {
	  fmt.Println("retry no earlier than", earliest)
	}

TakeN either deducts all N units or none. When it cannot accept the
request, the returned time indicates the earliest moment at which the
current refill rate allows N units to become available. Estimates never
pre-borrow allowance: queries do not refill or mutate the limiter.

For fast, deterministic tests inject a controllable time source with
NewWithClock.
*/
package ratelimit

import (
	"math/bits"
	"sync"
	"time"
)

// Clock returns the current time. Instances must be safe for concurrent use.
type Clock interface {
	Now() time.Time
}

// ClockFunc is a functional Clock implementation.
type ClockFunc func() time.Time

// Now implements Clock.
func (f ClockFunc) Now() time.Time { return f() }

// RateLimiter instances are thread-safe.
type RateLimiter struct {
	mu sync.Mutex

	rate, allowance, max, unit uint64
	lastCheck                  int64

	clock Clock
}

// New creates a new rate limiter instance
func New(rate int, per time.Duration) *RateLimiter {
	return NewWithClock(rate, per, nil)
}

// NewWithClock creates a new rate limiter instance driven by clock. Pass a
// nil clock to use the real system clock. A controllable clock keeps tests
// fast and deterministic without real sleeping.
func NewWithClock(rate int, per time.Duration, clock Clock) *RateLimiter {
	nano := uint64(per)
	if nano < 1 {
		nano = uint64(time.Second)
	}
	if rate < 1 {
		rate = 1
	}
	if clock == nil {
		clock = ClockFunc(time.Now)
	}

	// Burst capacity is exactly "rate" units; a true capacity that does not
	// fit the internal representation saturates to the maximum representable
	// value, so extreme parameters never wrap or produce a zero capacity.
	max := mulSaturate(uint64(rate), nano)
	now := clock.Now().UnixNano()
	return &RateLimiter{
		rate:      uint64(rate), // store the rate
		allowance: max,          // set our allowance to max in the beginning
		max:       max,          // remember our maximum allowance
		unit:      nano,         // remember our unit size

		lastCheck: now,
		clock:     clock,
	}
}

// UpdateRate allows to update the allowed rate
func (rl *RateLimiter) UpdateRate(rate int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rate < 1 {
		rate = 1
	}
	rl.rate = uint64(rate)
	rl.max = mulSaturate(rl.rate, rl.unit)

	// Clamp a previously accumulated allowance down to the new maximum.
	if rl.allowance > rl.max {
		rl.allowance = rl.max
	}
}

// Limit returns true if rate was exceeded
func (rl *RateLimiter) Limit() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock.Now().UnixNano()

	// Calculate the number of ns that have passed since our last call. A
	// backwards clock jump contributes nothing instead of wrapping.
	var passedNanos uint64
	if delta := now - rl.lastCheck; delta > 0 {
		passedNanos = uint64(delta)
	}
	rl.lastCheck = now

	// Add them to our allowance
	current := rl.refill(rl.allowance, passedNanos)
	rl.allowance = current

	// If our allowance is less than one unit, rate-limit!
	if current < rl.unit {
		return true
	}

	// Not limited, subtract a unit
	rl.allowance -= rl.unit
	return false
}

// Undo reverts the last Limit() call, returning consumed allowance
func (rl *RateLimiter) Undo() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.allowance = satAdd(rl.allowance, rl.unit)

	// Ensure our allowance is not over maximum
	if rl.allowance > rl.max {
		rl.allowance = rl.max
	}
}

// TakeN attempts to atomically deduct exactly n units of cost.
//
// On success the full cost is deducted and allowed is true. On failure
// nothing at all is deducted (the limiter state, including its refill
// baseline, is left untouched) and allowed is false; earliest is the first
// time at which n units can be accepted given the current balance and the
// configured refill rate.
//
// Semantics:
//   - A cost of zero (n <= 0) always succeeds and deducts nothing.
//   - A cost above the bucket capacity (more than rate units) can never be
//     accepted; earliest is the zero time.
//   - Costs whose true size would overflow the internal representation are
//     treated as over capacity: rejected with the zero time.
func (rl *RateLimiter) TakeN(n int) (allowed bool, earliest time.Time) {
	if n <= 0 {
		rl.mu.Lock()
		now := rl.clock.Now()
		rl.mu.Unlock()
		return true, now
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock.Now()

	cost, capped := rl.costOf(n)

	// Cost above the bucket capacity (rate units) can never be accepted.
	if capped {
		return false, time.Time{}
	}

	current, _ := rl.available(now.UnixNano())
	if current >= cost {
		rl.lastCheck = now.UnixNano()
		rl.allowance = current - cost
		return true, now
	}

	// Rejected: do not refill, advance lastCheck or deduct anything.
	return false, rl.earliest(now, current, cost)
}

// EarliestAvailable reports the first time at which n units can be accepted.
// It is a pure read: it neither refills, deducts nor otherwise mutates the
// limiter, so it can never pre-borrow allowance.
//
// A cost of zero (n <= 0) returns the current time. A cost above the bucket
// capacity returns the zero time because it can never be accepted.
func (rl *RateLimiter) EarliestAvailable(n int) time.Time {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.clock.Now()
	if n <= 0 {
		return now
	}

	cost, capped := rl.costOf(n)
	if capped {
		return time.Time{}
	}

	current, _ := rl.available(now.UnixNano())
	if current >= cost {
		return now
	}
	return rl.earliest(now, current, cost)
}

// costOf returns the allowance cost (in internal nanosecond units) of n
// units and reports whether that cost exceeds the bucket capacity. The
// comparison is performed in 128-bit arithmetic, so a true cost larger than
// the uint64 representation is reported as capped rather than wrapping.
func (rl *RateLimiter) costOf(n int) (cost uint64, capped bool) {
	costHi, costLo := bits.Mul64(uint64(n), rl.unit)
	if costHi != 0 {
		return 0, true
	}
	_, capLo := bits.Mul64(rl.rate, rl.unit)
	if costLo > capLo {
		return 0, true
	}
	return costLo, false
}

// available projects the allowance as of nowNano without persisting it.
func (rl *RateLimiter) available(nowNano int64) (uint64, bool) {
	// Ignore negative deltas caused by a backwards-moving time source.
	var passed uint64
	if delta := nowNano - rl.lastCheck; delta > 0 {
		passed = uint64(delta)
	}
	current := rl.refill(rl.allowance, passed)
	return current, current >= rl.max
}

// refill adds the allowance accrued over passed nanoseconds and caps the
// result at the bucket maximum. Long idle periods refill to full at most;
// they never overflow the balance.
func (rl *RateLimiter) refill(allowance uint64, passed uint64) uint64 {
	current := satAdd(allowance, mulSaturate(passed, rl.rate))
	if current > rl.max {
		current = rl.max
	}
	return current
}

// earliest computes the first time the balance reaches cost given the
// current refill rate. The result is the ceiling of the deficit in
// nanoseconds divided by rate; the division is performed in 128-bit
// arithmetic so extreme rates cannot overflow the wait duration.
func (rl *RateLimiter) earliest(now time.Time, current, cost uint64) time.Time {
	deficit := cost - current
	wait, remainder := bits.Div64(0, deficit, rl.rate)
	if remainder != 0 {
		wait = satAdd(wait, 1)
	}
	return now.Add(saturatingDuration(wait))
}

// saturatingDuration converts a nanosecond count to a time.Duration,
// clamping to the maximum (or minimum) duration on int64 overflow.
func saturatingDuration(ns uint64) time.Duration {
	const maxDuration = uint64(1<<63 - 1)
	if ns > maxDuration {
		return 1<<63 - 1
	}
	return time.Duration(ns)
}

// satAdd returns a+b, saturating at the maximum uint64 value on overflow.
func satAdd(a, b uint64) uint64 {
	if sum, carry := bits.Add64(a, b, 0); carry == 0 {
		return sum
	}
	return ^uint64(0)
}

// mulSaturate returns a*b, saturating at the maximum uint64 value on
// overflow.
func mulSaturate(a, b uint64) uint64 {
	if hi, lo := bits.Mul64(a, b); hi == 0 {
		return lo
	}
	return ^uint64(0)
}
