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

Callers that can wait before sending a request use Reserve instead:

	r, err := rl.Reserve()
	if err != nil {
		// ...
	}
	time.Sleep(r.Delay())
	if err := r.Consume(); err != nil {
		// reservation was canceled meanwhile
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

	// rate tokens are refilled every unit nanoseconds; one token costs
	// `unit` allowance-nanoseconds. max is the burst capacity in the same
	// unit.
	rate      uint64
	unit      uint64
	max       int64
	allowance int64

	// lastCheck is the time up to which refill has been integrated.
	lastCheck int64
	// lastEvent marks the end of the reserved schedule: the greatest
	// earliest-execution time handed out by a reservation (or the last
	// refill time when nothing is reserved). It is what makes cancel
	// accounting conservative instead of blindly re-crediting the cost.
	lastEvent int64

	now func() time.Time
}

// New creates a new rate limiter instance
func New(rate int, per time.Duration) *RateLimiter {
	return NewWithClock(rate, per, systemClock{})
}

// NewWithClock creates a rate limiter whose time source is the supplied
// clock. Passing a controllable clock lets tests reserve, consume and cancel
// deterministically.
func NewWithClock(rate int, per time.Duration, clock Clock) *RateLimiter {
	nano := uint64(per)
	if nano < 1 {
		nano = uint64(time.Second)
	}
	if rate < 1 {
		rate = 1
	}

	start := clock.Now().UnixNano()
	max := satMul(int64(rate), int64(nano))
	return &RateLimiter{
		rate:      uint64(rate),
		unit:      nano,
		max:       max,
		allowance: max,
		lastCheck: start,
		lastEvent: start,
		now:       clock.Now,
	}
}

// Burst returns the maximum number of tokens that can be made available at a
// single instant, equal to the configured rate.
func (rl *RateLimiter) Burst() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return int(rl.rate)
}

// UpdateRate allows to update the allowed rate. Reservations already made
// keep their earliest-execution time; the new rate governs refill from now on
// and the accounting of any later cancel.
func (rl *RateLimiter) UpdateRate(rate int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rate < 1 {
		rate = 1
	}
	rl.rate = uint64(rate)
	rl.max = satMul(int64(rate), int64(rl.unit))
}

// Limit returns true if rate was exceeded
func (rl *RateLimiter) Limit() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now().UnixNano()
	rl.advance(now)

	// If our allowance is less than one unit, rate-limit!
	if rl.allowance < int64(rl.unit) {
		return true
	}

	// Not limited, subtract a unit
	rl.allowance -= int64(rl.unit)
	return false
}

// Undo reverts the last Limit() call, returning consumed allowance.
//
// Undo is the legacy, unconditional one-token restore and does not consult
// the reservation schedule. It must not be used to implement reservation
// cancellation: cancel uses the schedule-aware ledger in Reservation.CancelAt
// so that it cannot reclaim work that later reservations already rely on.
func (rl *RateLimiter) Undo() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.advance(rl.now().UnixNano())
	rl.allowance += int64(rl.unit)

	// Ensure our allowance is not over maximum
	if rl.allowance > rl.max {
		rl.allowance = rl.max
	}
}

// advance integrates token refill up to now, clamps to the burst and returns
// the current balance. The caller must hold rl.mu.
func (rl *RateLimiter) advance(now int64) int64 {
	if now > rl.lastCheck {
		rl.allowance = satAdd(rl.allowance, satMul(now-rl.lastCheck, int64(rl.rate)))
		rl.lastCheck = now
		if rl.allowance > rl.max {
			rl.allowance = rl.max
		}
	}
	return rl.allowance
}

func maxInt64(a, b int64) int64 {
	if b > a {
		return b
	}
	return a
}

// ceilDiv returns ceil(n/d) for d > 0; inputs are non-negative.
func ceilDiv(n, d int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + d - 1) / d
}

// satAdd returns a+b, saturated to int64.
func satAdd(a, b int64) int64 {
	if b > 0 && a > 1<<63-1-b {
		return 1<<63 - 1
	}
	if b < 0 && a < -1<<63-b {
		return -1 << 63
	}
	return a + b
}

// satMul returns a*b for non-negative a and b, saturated to int64.
func satMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > (1<<63-1)/b {
		return 1<<63 - 1
	}
	return a * b
}
