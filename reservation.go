package ratelimit

import (
	"errors"
	"time"
)

// ErrInvalidCost is returned when a reservation requests fewer than one
// token.
var ErrInvalidCost = errors.New("ratelimit: must reserve at least one token")

// ErrCostExceedsBurst is returned when a reservation asks for more tokens
// than the limiter can ever deliver within a single unit. Such a reservation
// could only be satisfied at an infinite future, so it is rejected instead.
var ErrCostExceedsBurst = errors.New("ratelimit: requested cost exceeds limiter burst")

// ErrReservationFinal is returned when Consume or Cancel is invoked on a
// reservation that was already consumed or canceled. A reservation changes
// the limiter balance at most once, so the call has no effect.
var ErrReservationFinal = errors.New("ratelimit: reservation already consumed or canceled")

// ErrTooSoon is returned when Consume is called before the reservation's
// earliest-execution time. The caller must wait until TimeToAct.
var ErrTooSoon = errors.New("ratelimit: reservation not ready yet")

// ReservationStatus reports the lifecycle state of a Reservation.
type ReservationStatus uint8

const (
	// ReservationPending means the reservation can still be consumed or
	// canceled.
	ReservationPending ReservationStatus = iota
	// ReservationConsumed means the work has been acknowledged via Consume.
	ReservationConsumed
	// ReservationCanceled means Cancel or CancelAt was invoked. When it was
	// invoked before the earliest-execution time, the schedule-aware ledger
	// adjusted the balance once; at/after execution time nothing is given
	// back.
	ReservationCanceled
)

// A Reservation holds information about the tokens reserved by a single call.
// A reservation is one-shot: after Consume or Cancel the balance is never
// touched again.
type Reservation struct {
	limiter *RateLimiter

	// cost is the number of reserved tokens, expressed in allowance units
	// (cost * unit nanoseconds).
	cost      int64
	costUnits int64
	limit     int64

	timeToAct int64
	status    ReservationStatus
}

// Cost returns the number of tokens reserved.
func (r *Reservation) Cost() int {
	return int(r.cost)
}

// TimeToAct returns the earliest time the work may execute.
func (r *Reservation) TimeToAct() time.Time {
	return time.Unix(0, r.timeToAct)
}

// Delay returns how long to wait from the limiter's current time until the
// reservation may be consumed. It is zero if the reservation is already due.
func (r *Reservation) Delay() time.Duration {
	r.limiter.mu.Lock()
	defer r.limiter.mu.Unlock()

	d := r.timeToAct - r.limiter.now().UnixNano()
	if d < 0 {
		return 0
	}
	return time.Duration(d)
}

// Status reports whether the reservation is pending, consumed or canceled.
func (r *Reservation) Status() ReservationStatus {
	r.limiter.mu.Lock()
	defer r.limiter.mu.Unlock()
	return r.status
}

// Reserve reserves one token. See ReserveN.
func (rl *RateLimiter) Reserve() (*Reservation, error) {
	return rl.ReserveN(1)
}

// ReserveN reserves n tokens at the limiter's current time.
//
// The tokens are debited immediately and a schedule tail (lastEvent) is
// advanced, so consecutive reservations get non-decreasing earliest-execution
// times. Cancellation never moves an existing later reservation: later
// reservations keep the time they were given. A request larger than the
// burst cannot be served in finite time and returns ErrCostExceedsBurst.
func (rl *RateLimiter) ReserveN(n int) (*Reservation, error) {
	return rl.reserveN(rl.now().UnixNano(), int64(n))
}

func (rl *RateLimiter) reserveN(now, n int64) (*Reservation, error) {
	if n < 1 {
		return nil, ErrInvalidCost
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	burst := int64(rl.rate)
	if n > burst {
		return nil, ErrCostExceedsBurst
	}

	tokens := rl.advance(now)

	// Earliest execution time is the later of (a) the time the unfunded part
	// of the cost takes to accrue from now and (b) the end of the current
	// schedule already promised to other reservations.
	needed := satMul(n, int64(rl.unit)) - tokens
	wait := ceilDiv(needed, int64(rl.rate))
	timeToAct := maxInt64(satAdd(now, wait), rl.lastEvent)

	r := &Reservation{
		limiter:   rl,
		cost:      n,
		costUnits: satMul(n, int64(rl.unit)),
		limit:     int64(rl.rate),
		timeToAct: timeToAct,
	}

	// Spend the cost now, carrying the schedule tail forward.
	rl.allowance -= r.costUnits
	rl.lastEvent = timeToAct
	return r, nil
}

// Consume acknowledges that the reserved work executes at the limiter's
// current time. It may only be called once and not before TimeToAct.
func (r *Reservation) Consume() error {
	return r.consumeAt(r.limiter.now().UnixNano())
}

func (r *Reservation) consumeAt(now int64) error {
	r.limiter.mu.Lock()
	defer r.limiter.mu.Unlock()

	if r.status != ReservationPending {
		return ErrReservationFinal
	}
	if now < r.timeToAct {
		return ErrTooSoon
	}
	r.status = ReservationConsumed
	return nil
}

// Cancel abandons the reservation at the limiter's current time. See
// CancelAt. It always returns nil for a first, valid cancel; subsequent calls
// return ErrReservationFinal.
func (r *Reservation) Cancel() error {
	return r.CancelAt(r.limiter.now())
}

// CancelAt abandons the reservation as of time t.
//
// At or after the earliest-execution time the work may already have happened,
// so no allowance is ever returned: cancellation cannot reach back past work
// that is due and reclaim its tokens. Before that time, only the part of the
// cost that has not already been committed to later reservations is given
// back, and no later reservation is rescheduled. Calling CancelAt again (or
// CancelAt after Consume) returns ErrReservationFinal and changes nothing.
func (r *Reservation) CancelAt(t time.Time) error {
	now := t.UnixNano()

	r.limiter.mu.Lock()
	defer r.limiter.mu.Unlock()

	if r.status != ReservationPending {
		return ErrReservationFinal
	}
	r.status = ReservationCanceled

	if now >= r.timeToAct {
		return nil
	}

	// Time the schedule tail sits beyond this reservation. Every nanosecond
	// of it is work that later reservations already rely on, so the tokens
	// that would have accrued in that span cannot be given back.
	tail := r.limiter.lastEvent - r.timeToAct
	committed := satMul(tail, r.limit)
	restore := r.costUnits - committed
	if restore <= 0 {
		return nil
	}

	r.limiter.advance(now)
	r.limiter.allowance = satAdd(r.limiter.allowance, restore)
	if r.limiter.allowance > r.limiter.max {
		r.limiter.allowance = r.limiter.max
	}

	// If this reservation owned the tail of the schedule, retract the tail
	// to now; surviving reservations still cover everything up to their own
	// times and are never moved.
	if r.limiter.lastEvent == r.timeToAct {
		r.limiter.lastEvent = maxInt64(r.limiter.lastCheck, now)
	}
	return nil
}
