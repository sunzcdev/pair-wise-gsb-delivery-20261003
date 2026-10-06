package ratelimit

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func newTestLimiter(rate int, per time.Duration, clk *fakeClock) *RateLimiter {
	return NewWithClock(rate, per, clk)
}

func tta(t *testing.T, r *Reservation) int64 {
	t.Helper()
	return r.timeToAct
}

// TestReserve_Sequential exercises a burst followed by a stream of future
// reservations: their earliest-execution times walk forward at the refill
// interval, and consume is one-shot.
func TestReserve_Sequential(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := newTestLimiter(10, time.Second, clk)

	var rs []*Reservation
	for i := 0; i < 13; i++ {
		r, err := rl.Reserve()
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		rs = append(rs, r)
	}

	for i := 0; i < 10; i++ {
		if got := tta(t, rs[i]); got != 0 {
			t.Fatalf("burst reservation %d tta = %d, want 0", i, got)
		}
	}
	if got := tta(t, rs[10]); got != int64(100*time.Millisecond) {
		t.Fatalf("r10 tta = %d, want 100ms", got)
	}
	if got := tta(t, rs[11]); got != int64(200*time.Millisecond) {
		t.Fatalf("r11 tta = %d, want 200ms", got)
	}
	if got := tta(t, rs[12]); got != int64(300*time.Millisecond) {
		t.Fatalf("r12 tta = %d, want 300ms", got)
	}

	if err := rs[10].Consume(); !errors.Is(err, ErrTooSoon) {
		t.Fatalf("early consume error = %v, want ErrTooSoon", err)
	}
	clk.Advance(100 * time.Millisecond)
	if err := rs[10].Consume(); err != nil {
		t.Fatalf("due consume: %v", err)
	}
	if err := rs[10].Consume(); !errors.Is(err, ErrReservationFinal) {
		t.Fatalf("second consume error = %v, want ErrReservationFinal", err)
	}
	if err := rs[10].Cancel(); !errors.Is(err, ErrReservationFinal) {
		t.Fatalf("cancel after consume error = %v, want ErrReservationFinal", err)
	}
}

// TestReserve_CancelMiddleKeepsLaterTimes pins the key semantic: canceling an
// earlier reservation never moves a surviving later reservation. The cancel
// is ledger-aware rather than Undo stacking: the surviving tail already
// consumes every token the canceled reservation would have had, so a fresh
// reservation is scheduled behind the tail instead of stealing its slot.
func TestReserve_CancelMiddleKeepsLaterTimes(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := newTestLimiter(10, time.Second, clk)

	var rs []*Reservation
	for i := 0; i < 12; i++ {
		r, err := rl.Reserve()
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		rs = append(rs, r)
	}
	// r0..r9 fill the burst (tta 0), r10 queues at 100ms, r11 at 200ms.
	if got := tta(t, rs[10]); got != int64(100*time.Millisecond) {
		t.Fatalf("r10 tta = %d, want 100ms", got)
	}
	if got := tta(t, rs[11]); got != int64(200*time.Millisecond) {
		t.Fatalf("r11 tta = %d, want 200ms", got)
	}

	// Cancel r10 at 50ms, before it is due. r11 keeps its original time.
	clk.Advance(50 * time.Millisecond)
	if err := rs[10].Cancel(); err != nil {
		t.Fatalf("cancel r10: %v", err)
	}
	if got := tta(t, rs[11]); got != int64(200*time.Millisecond) {
		t.Fatalf("r11 moved to %d after cancel, want 200ms", got)
	}

	// Tokens through r11's slot are all committed to surviving reservations;
	// a fresh reservation runs at 300ms. Naive Undo stacking would credit
	// the full cost and schedule it at 200ms, stealing r11's slot.
	r12, err := rl.Reserve()
	if err != nil {
		t.Fatalf("reserve r12: %v", err)
	}
	if got := tta(t, r12); got != int64(300*time.Millisecond) {
		t.Fatalf("r12 tta = %d, want 300ms (no Undo stacking)", got)
	}

	// Cancel is one-shot and never re-touches the balance.
	if err := rs[10].Cancel(); !errors.Is(err, ErrReservationFinal) {
		t.Fatalf("second cancel error = %v, want ErrReservationFinal", err)
	}

	// r11 becomes due at 200ms; canceling then cannot reclaim executed work
	// and must not move r12 forward.
	clk.Advance(150 * time.Millisecond)
	if err := rs[11].Cancel(); err != nil {
		t.Fatalf("cancel r11 at due time: %v", err)
	}
	if got := tta(t, r12); got != int64(300*time.Millisecond) {
		t.Fatalf("r12 moved to %d after due cancel, want 300ms", got)
	}
}

// TestReserve_CancelTailFreesSlot checks that an early cancel returns only
// what later reservations have not committed: canceling the tail reservation
// frees its slot for a new reservation while earlier reservations stay put.
func TestReserve_CancelTailFreesSlot(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := newTestLimiter(10, time.Second, clk)

	var rs []*Reservation
	for i := 0; i < 11; i++ {
		r, err := rl.Reserve()
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		rs = append(rs, r)
	}
	// r10 is the tail, due at 100ms.
	if got := tta(t, rs[10]); got != int64(100*time.Millisecond) {
		t.Fatalf("r10 tta = %d, want 100ms", got)
	}

	clk.Advance(50 * time.Millisecond)
	if err := rs[10].Cancel(); err != nil {
		t.Fatalf("cancel r10: %v", err)
	}

	r11, err := rl.Reserve()
	if err != nil {
		t.Fatalf("reserve r11: %v", err)
	}
	if got := tta(t, r11); got != int64(100*time.Millisecond) {
		t.Fatalf("r11 tta = %d, want 100ms (freed tail slot)", got)
	}

	// A second new reservation queues behind the freed one.
	r12, err := rl.Reserve()
	if err != nil {
		t.Fatalf("reserve r12: %v", err)
	}
	if got := tta(t, r12); got != int64(200*time.Millisecond) {
		t.Fatalf("r12 tta = %d, want 200ms", got)
	}
}

// TestReserve_CancelTailWithGap verifies tail retraction when reservations
// are not packed back-to-back: waiting before a reservation leaves a gap, and
// canceling that reservation must not allow an oversell in the gap.
func TestReserve_CancelTailWithGap(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := newTestLimiter(10, time.Second, clk)

	var rs []*Reservation
	for i := 0; i < 10; i++ {
		r, err := rl.Reserve()
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		rs = append(rs, r)
	}

	// Let 50ms accrue (0.5 token), then reserve: tta is 100ms, leaving a gap.
	clk.Advance(50 * time.Millisecond)
	rLate, err := rl.Reserve()
	if err != nil {
		t.Fatalf("reserve late: %v", err)
	}
	if got := tta(t, rLate); got != int64(100*time.Millisecond) {
		t.Fatalf("late tta = %d, want 100ms", got)
	}

	// Cancel immediately (at 50ms); its slot is returned. A fresh reservation
	// must account for the 0.5 token already accrued and schedule at 100ms,
	// never in the past.
	if err := rLate.Cancel(); err != nil {
		t.Fatalf("cancel late: %v", err)
	}
	rNext, err := rl.Reserve()
	if err != nil {
		t.Fatalf("reserve next: %v", err)
	}
	if got := tta(t, rNext); got != int64(100*time.Millisecond) {
		t.Fatalf("next tta = %d, want 100ms", got)
	}
}

// TestReserve_RejectedCosts ensures unsatisfiable reservations return an
// error rather than a reservation at an infinite future.
func TestReserve_RejectedCosts(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := newTestLimiter(10, time.Second, clk)

	if _, err := rl.ReserveN(11); !errors.Is(err, ErrCostExceedsBurst) {
		t.Fatalf("ReserveN(11) error = %v, want ErrCostExceedsBurst", err)
	}
	if _, err := rl.ReserveN(0); !errors.Is(err, ErrInvalidCost) {
		t.Fatalf("ReserveN(0) error = %v, want ErrInvalidCost", err)
	}
	if _, err := rl.ReserveN(-1); !errors.Is(err, ErrInvalidCost) {
		t.Fatalf("ReserveN(-1) error = %v, want ErrInvalidCost", err)
	}

	// A cost within the burst but beyond current tokens is still satisfiable.
	r, err := rl.ReserveN(10)
	if err != nil {
		t.Fatalf("ReserveN(10): %v", err)
	}
	if got := tta(t, r); got != 0 {
		t.Fatalf("burst reservation tta = %d, want 0", got)
	}
}

// TestReserve_RateChangeAcrossReservations covers cross-reservation rate
// changes: existing reservations keep their times, refill after the change
// follows the new rate, and the ledger never exceeds burst.
func TestReserve_RateChangeAcrossReservations(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := newTestLimiter(10, time.Second, clk)

	r10 := mustReserveN(t, rl, 10)
	r11, err := rl.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if got := tta(t, r11); got != int64(100*time.Millisecond) {
		t.Fatalf("r11 tta = %d, want 100ms", got)
	}

	// Double the rate at 50ms; the existing schedule is immutable.
	clk.Advance(50 * time.Millisecond)
	rl.UpdateRate(20)
	if got := tta(t, r11); got != int64(100*time.Millisecond) {
		t.Fatalf("r11 moved after UpdateRate to %d, want 100ms", got)
	}

	// Reservations after the change advance at the new 50ms/token cadence.
	r12, err := rl.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	r13, err := rl.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if got := tta(t, r13) - tta(t, r12); got != int64(50*time.Millisecond) {
		t.Fatalf("new-rate spacing = %d, want 50ms", got)
	}
	if tta(t, r12) < tta(t, r11) {
		t.Fatalf("r12 tta %d precedes r11 tta %d", tta(t, r12), tta(t, r11))
	}

	// A later burst follows the new rate.
	if got := rl.Burst(); got != 20 {
		t.Fatalf("burst = %d, want 20", got)
	}

	// Canceling the big reservation at 50ms stays consistent under the
	// captured 10/s rate and never lets the balance exceed burst.
	if err := r10.Cancel(); err != nil {
		t.Fatalf("cancel r10: %v", err)
	}
	clk.Advance(time.Hour)
	rl.mu.Lock()
	bal := rl.advance(clk.Now().UnixNano())
	rl.mu.Unlock()
	if bal > rl.max {
		t.Fatalf("balance %d exceeds burst %d", bal, rl.max)
	}
}

// TestReserve_ExecuteCancelRace proves the consume/cancel race resolves
// atomically: each reservation ends in exactly one terminal state and the
// balance is never modified twice.
func TestReserve_ExecuteCancelRace(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := newTestLimiter(50, time.Second, clk)

	var rs []*Reservation
	for i := 0; i < 50; i++ {
		r, err := rl.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}

	const attempts = 64
	var wg sync.WaitGroup
	for _, r := range rs {
		wg.Add(2)
		go func(r *Reservation) {
			defer wg.Done()
			for i := 0; i < attempts; i++ {
				_ = r.Consume()
			}
		}(r)
		go func(r *Reservation) {
			defer wg.Done()
			for i := 0; i < attempts; i++ {
				_ = r.Cancel()
			}
		}(r)
	}
	wg.Wait()

	consumed, canceled := 0, 0
	for i, r := range rs {
		switch r.Status() {
		case ReservationConsumed:
			consumed++
		case ReservationCanceled:
			canceled++
		default:
			t.Fatalf("reservation %d still pending after race", i)
		}
	}
	if consumed+canceled != len(rs) {
		t.Fatalf("terminalized = %d, want %d", consumed+canceled, len(rs))
	}

	rl.mu.Lock()
	bal := rl.allowance
	rl.mu.Unlock()
	if bal > rl.max {
		t.Fatalf("balance %d exceeds burst %d", bal, rl.max)
	}
}

func mustReserveN(t *testing.T, rl *RateLimiter, n int) *Reservation {
	t.Helper()
	r, err := rl.ReserveN(n)
	if err != nil {
		t.Fatalf("ReserveN(%d): %v", n, err)
	}
	return r
}
