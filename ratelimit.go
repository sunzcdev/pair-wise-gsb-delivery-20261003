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

Reservations (inspired by golang.org/x/time/rate) let a caller reserve a
future slot ahead of time:

	r, err := rl.Reserve(time.Minute) // one request, within at most 1 minute
	if err != nil {
		// err is ErrInvalidCost, ErrMaxBurstExceeded or ErrWouldExceedWait
		return err
	}
	defer r.Cancel()

	time.Sleep(r.Delay())
	if !r.Consume() {
		return errors.New("reservation was already cancelled")
	}

Cancelling a reservation never moves other reservations: reservations behind
a cancelled one keep their original earliest-execution time.
*/
package ratelimit

import (
	"errors"
	"sync"
	"time"
)

var (
	// ErrInvalidCost is returned when a reservation is requested with a
	// non-positive cost or a negative maximum wait.
	ErrInvalidCost = errors.New("ratelimit: invalid reservation cost or max wait")

	// ErrMaxBurstExceeded is returned when a reservation costs more than the
	// limiter's burst size, so it could never be served, even after waiting
	// forever.
	ErrMaxBurstExceeded = errors.New("ratelimit: reservation exceeds burst size")

	// ErrWouldExceedWait is returned when a reservation can only be served
	// later than the requested maximum wait. It is returned instead of an
	// unbounded/infinite delay.
	ErrWouldExceedWait = errors.New("ratelimit: reservation would exceed max wait")
)

// RateLimiter instances are thread-safe.
type RateLimiter struct {
	mu sync.Mutex

	// rate is the number of requests replenished per unit, i.e. the accrual
	// in scaled allowance units per nanosecond.
	rate uint64
	// unit is the cost of a single request, equal to one unit of time.
	unit uint64
	// max is the burst capacity, in scaled allowance units.
	max int64
	// tokens is the current (possibly negative) balance. A negative balance
	// means capacity is owed to outstanding reservations.
	tokens int64
	// last is the anchor time of the current balance: tokens accrue from
	// last onwards.
	last int64
	// nextEvent is the earliest-execution time of the last outstanding
	// reservation (the tail). Zero means no outstanding reservation. It is
	// the anchor used to queue reservations and it is never moved backwards:
	// cancelling an earlier reservation does not pull later ones forward.
	nextEvent int64
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

	return &RateLimiter{
		rate:   uint64(rate),
		unit:   nano,
		max:    int64(uint64(rate) * nano),
		tokens: int64(uint64(rate) * nano), // set our allowance to max initially
		last:   nowNano(),
	}
}

// UpdateRate allows to update the allowed rate.
//
// Outstanding reservations are not rescheduled: each one keeps the
// earliest-execution time promised at its own rate. Cancelling a reservation
// after a rate change restores capacity at that reservation's original rate,
// capped by the new burst size, while new reservations are scheduled at the
// new rate.
func (rl *RateLimiter) UpdateRate(rate int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rate < 1 {
		rate = 1
	}
	rl.rate = uint64(rate)
	rl.max = int64(uint64(rate) * rl.unit)
}

// Limit returns true if rate was exceeded
func (rl *RateLimiter) Limit() bool {
	now := nowNano()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.advance(now)

	// If our allowance is less than one unit, rate-limit!
	if rl.tokens < int64(rl.unit) {
		return true
	}

	// Not limited, subtract a unit
	rl.tokens -= int64(rl.unit)
	rl.nextEvent = now
	return false
}

// Undo reverts the last Limit() call, returning consumed allowance
func (rl *RateLimiter) Undo() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.tokens += int64(rl.unit)

	// Ensure our allowance is not over maximum
	if rl.tokens > rl.max {
		rl.tokens = rl.max
	}
}

// Reserve reserves one request, i.e. it is shorthand for ReserveN(1, maxWait).
func (rl *RateLimiter) Reserve(maxWait time.Duration) (*Reservation, error) {
	return rl.ReserveN(1, maxWait)
}

// ReserveAt reserves one request at an explicit (e.g. fake-clock) time.
func (rl *RateLimiter) ReserveAt(at time.Time, maxWait time.Duration) (*Reservation, error) {
	return rl.ReserveNAt(1, at, maxWait)
}

// ReserveN reserves n requests, requiring them to be executable no later than
// maxWait from now. It returns an error and reserves nothing when the request
// cannot be satisfied within that bound: ErrInvalidCost for invalid arguments,
// ErrMaxBurstExceeded when n exceeds the burst size, or ErrWouldExceedWait
// when the required wait exceeds maxWait.
func (rl *RateLimiter) ReserveN(n int, maxWait time.Duration) (*Reservation, error) {
	return rl.ReserveNAt(n, time.Now(), maxWait)
}

// ReserveNAt reserves n requests at an explicit (e.g. fake-clock) time.
func (rl *RateLimiter) ReserveNAt(n int, at time.Time, maxWait time.Duration) (*Reservation, error) {
	if n < 1 || maxWait < 0 {
		return nil, ErrInvalidCost
	}
	now := at.UnixNano()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.advance(now)

	// A request costs unit; the burst size is rate requests, so anything
	// above it can never be served, even after an unbounded wait.
	burst := rl.max / int64(rl.unit)
	if int64(n) > burst {
		return nil, ErrMaxBurstExceeded
	}

	cost := int64(n) * int64(rl.unit)

	// Queue behind any outstanding reservation. This anchor is the reason a
	// later reservation never jumps ahead of earlier ones.
	anchor := now
	if rl.nextEvent > anchor {
		anchor = rl.nextEvent
	}

	// Balance projected to the anchor: between now and the anchor the bucket
	// keeps accruing. The reservation waits only for what is still missing
	// after that accrual, rounded up to whole nanoseconds.
	deficit := cost - (rl.tokens + (anchor-now)*int64(rl.rate))
	if deficit < 0 {
		deficit = 0
	}
	wait := (deficit + int64(rl.rate) - 1) / int64(rl.rate)

	timeToAct := anchor + wait
	if timeToAct < now || timeToAct-now > int64(maxWait) {
		return nil, ErrWouldExceedWait
	}

	r := &Reservation{
		rl:        rl,
		n:         n,
		cost:      cost,
		rate:      rl.rate,
		timeToAct: timeToAct,
		state:     reservationPending,
	}

	// Pre-sell the capacity at "now": tokens stays the balance at rl.last
	// (which advance just moved to now), so it can go negative.
	rl.tokens -= cost
	rl.nextEvent = timeToAct
	return r, nil
}

// advance accrues tokens up to now and moves the accounting anchor.
// Caller must hold rl.mu.
func (rl *RateLimiter) advance(now int64) {
	elapsed := now - rl.last
	if elapsed == 0 {
		rl.last = now
		return
	}
	rl.last = now
	if elapsed < 0 {
		// Clock moved backwards: un-accrue the interval. Negative balances
		// from outstanding reservations are preserved.
		rl.tokens += elapsed * int64(rl.rate)
		return
	}
	rl.tokens += elapsed * int64(rl.rate)
	if rl.tokens > rl.max {
		rl.tokens = rl.max
	}
}

const (
	reservationPending uint8 = iota
	reservationConsumed
	reservationCancelled
)

// Reservation is a future, pre-paid rate-limiter slot. Its methods are
// thread-safe. A reservation may be consumed or cancelled exactly once:
// whichever happens first wins and the losing operation is a no-op that
// reports false, so the balance can never be changed twice.
type Reservation struct {
	rl        *RateLimiter
	n         int   // number of reserved requests
	cost      int64 // in scaled allowance units
	rate      uint64
	timeToAct int64 // earliest-execution time, Unix nanos

	mu    sync.Mutex
	state uint8
}

// Cost returns the number of requests reserved.
func (r *Reservation) Cost() int { return r.n }

// TimeToAct returns the earliest time at which the reservation may be used.
func (r *Reservation) TimeToAct() time.Time {
	return time.Unix(0, r.timeToAct)
}

// Delay returns the duration from now until the reservation may be used. It
// is zero once the time has arrived.
func (r *Reservation) Delay() time.Duration {
	return r.DelayFrom(time.Now())
}

// DelayFrom returns the remaining delay measured from an explicit (e.g.
// fake-clock) time.
func (r *Reservation) DelayFrom(now time.Time) time.Duration {
	d := r.timeToAct - now.UnixNano()
	if d <= 0 {
		return 0
	}
	return time.Duration(d)
}

// Consume marks the reservation as used after the caller performed the
// reserved work. Capacity was already deducted at reservation time, so this
// only finalizes the state. It returns false when the reservation was already
// consumed or cancelled, in which case nothing is changed.
func (r *Reservation) Consume() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != reservationPending {
		return false
	}
	r.state = reservationConsumed
	return true
}

// Cancel abandons the reservation and restores capacity that has not been used
// yet. It returns true when it performed the cancellation, false when the
// reservation was already consumed or cancelled. Cancelling never moves other
// reservations and never reclaims allowance for work already due: only the
// portion of the reservation strictly after the cancellation time is
// returned.
func (r *Reservation) Cancel() bool {
	return r.CancelAt(time.Now())
}

// CancelAt cancels the reservation as if at an explicit (e.g. fake-clock)
// time.
func (r *Reservation) CancelAt(at time.Time) bool {
	r.mu.Lock()
	if r.state != reservationPending {
		r.mu.Unlock()
		return false
	}
	r.state = reservationCancelled
	r.mu.Unlock()

	now := at.UnixNano()
	restore := int64(0)

	r.rl.mu.Lock()
	defer r.rl.mu.Unlock()

	// Restore only the capacity that has not accrued by the cancellation
	// time: never claw back work that is already executable/done.
	if r.timeToAct > now {
		restore = (r.timeToAct - now) * int64(r.rate)
		if restore > r.cost {
			restore = r.cost
		}
	}
	if restore > 0 {
		r.rl.tokens += restore
		if r.rl.tokens > r.rl.max {
			r.rl.tokens = r.rl.max
		}
		// Only the tail reservation anchors the queue. Removing any other
		// reservation leaves the ones behind at their promised times; removing
		// the tail frees its slot without pulling anyone forward.
		if r.rl.nextEvent == r.timeToAct {
			r.rl.nextEvent = 0
		}
	}

	return true
}

func nowNano() int64 {
	return time.Now().UnixNano()
}
