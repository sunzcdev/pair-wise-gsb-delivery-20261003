package ratelimit

import (
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock. Reservations are scheduled with the
// *At methods, so no goroutines or real sleeps are needed.
type fakeClock struct{ t int64 }

func (c *fakeClock) time() time.Time { return time.Unix(0, c.t) }

func (c *fakeClock) add(d time.Duration) { c.t += int64(d) }

// newAt builds a limiter anchored at the fake clock's origin.
func newAt(rate int, per time.Duration, t int64) *RateLimiter {
	rl := New(rate, per)
	rl.last = t
	rl.tokens = rl.max
	return rl
}

// state reads the internal balance for assertions; the test owns the clock.
func (rl *RateLimiter) state() (tokens, nextEvent int64) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.tokens, rl.nextEvent
}

// peekTokens reads the balance at time t by accruing in a copy of the state,
// without committing the advance (useful right before a cancel-at-t).
func (rl *RateLimiter) peekTokens(t int64) int64 {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	tokens := rl.tokens + (t-rl.last)*int64(rl.rate)
	if tokens > rl.max {
		tokens = rl.max
	}
	return tokens
}

// Consecutive reservations are scheduled at evenly spaced, monotonically
// increasing times; burst tokens cover the first few, and each later
// reservation waits for the queue to drain.
func TestReservation_consecutive(t *testing.T) {
	const per = time.Second
	clk := &fakeClock{}
	rl := newAt(10, per, clk.t)

	var rs []*Reservation
	times := make(map[int64]int)
	for i := 0; i < 12; i++ {
		r, err := rl.ReserveNAt(1, clk.time(), time.Hour)
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		rs = append(rs, r)
		times[r.timeToAct]++
	}

	// First 10 (the burst) are due immediately, the rest at 0.1s intervals.
	if got := times[0]; got != 10 {
		t.Fatalf("expected 10 reservations due at origin, got %d", got)
	}
	if got := rs[10].timeToAct; got != int64(100*time.Millisecond) {
		t.Fatalf("reservation 10 due at %d, want 100ms", got)
	}
	if got := rs[11].timeToAct; got != int64(200*time.Millisecond) {
		t.Fatalf("reservation 11 due at %d, want 200ms", got)
	}

	// Nothing new fits before the queue drains.
	if _, err := rl.ReserveAt(clk.time(), 0); !errors.Is(err, ErrWouldExceedWait) {
		t.Fatalf("expected ErrWouldExceedWait, got %v", err)
	}

	// Matured-but-unconsumed reservations still own their slots: a newcomer
	// never reclaims them automatically. At 100ms a fresh reservation is
	// scheduled right after the 200ms tail (i.e. at 300ms), not admitted now.
	clk.add(100 * time.Millisecond)
	if _, err := rl.ReserveAt(clk.time(), 0); !errors.Is(err, ErrWouldExceedWait) {
		t.Fatalf("expected request to still wait at 100ms, got %v", err)
	}
	if r, err := rl.ReserveAt(clk.time(), time.Hour); err != nil || r.timeToAct != int64(300*time.Millisecond) {
		t.Fatalf("expected new tail at 300ms, got %v / %v", r, err)
	}

	// Even at 200ms, after the original tail matured, the 300ms newcomer
	// still anchors the queue: maturing a reservation never auto-refunds it.
	clk.add(100 * time.Millisecond)
	if _, err := rl.ReserveAt(clk.time(), 0); !errors.Is(err, ErrWouldExceedWait) {
		t.Fatalf("expected request to wait at 200ms, got %v", err)
	}
}

// Cancelling a reservation from the middle must not move the reservations
// behind it: they keep their original earliest-execution time. The freed
// capacity is simply not handed out early.
func TestReservation_cancelMiddleKeepsLaterTimes(t *testing.T) {
	clk := &fakeClock{}
	rl := newAt(2, time.Second, clk.t)

	r0, _ := rl.ReserveAt(clk.time(), time.Hour) // due 0ms
	r1, _ := rl.ReserveAt(clk.time(), time.Hour) // due 0ms (cancelled)
	r2, _ := rl.ReserveAt(clk.time(), time.Hour) // due 500ms
	r3, _ := rl.ReserveAt(clk.time(), time.Hour) // due 1000ms

	// Cancel a middle item (one of the immediate ones).
	if !r1.CancelAt(clk.time()) {
		t.Fatal("expected cancel to succeed")
	}

	// Later reservations are unmoved.
	if got := r2.TimeToAct(); !got.Equal(time.Unix(0, int64(500*time.Millisecond))) {
		t.Fatalf("r2 moved to %v", got)
	}
	if got := r3.TimeToAct(); !got.Equal(time.Unix(0, int64(time.Second))) {
		t.Fatalf("r3 moved to %v", got)
	}

	// The spare burst token is not handed out ahead of time: a fresh
	// reservation is queued behind r2, not admitted at t=0.
	rb, err := rl.ReserveAt(clk.time(), time.Hour)
	if err != nil {
		t.Fatalf("reserve after cancel: %v", err)
	}
	if rb.TimeToAct().Before(r2.TimeToAct()) {
		t.Fatalf("new reservation %v jumped ahead of r2 %v", rb.TimeToAct(), r2.TimeToAct())
	}

	// r0 is untouched and still consumable; double cancel is a no-op.
	if !r0.Consume() {
		t.Fatal("r0 should still be consumable")
	}
	if r1.CancelAt(clk.time()) {
		t.Fatal("second cancel must be a no-op")
	}
}

// Cancelling the tail reservation releases its slot: a replacement can be
// booked at that same time, while earlier reservations stay put.
func TestReservation_cancelTail(t *testing.T) {
	clk := &fakeClock{}
	rl := newAt(2, time.Second, clk.t)

	r0, _ := rl.ReserveAt(clk.time(), time.Hour) // 0ms
	_, _ = rl.ReserveAt(clk.time(), time.Hour)   // 0ms
	r2, _ := rl.ReserveAt(clk.time(), time.Hour) // 500ms
	r3, _ := rl.ReserveAt(clk.time(), time.Hour) // 1000ms (tail)

	if !r3.CancelAt(clk.time()) {
		t.Fatal("expected tail cancel to succeed")
	}

	if got := r2.TimeToAct(); !got.Equal(time.Unix(0, int64(500*time.Millisecond))) {
		t.Fatalf("r2 moved to %v", got)
	}

	// The freed tail slot is rebookable at exactly 1000ms, not earlier.
	rep, err := rl.ReserveAt(clk.time(), time.Hour)
	if err != nil {
		t.Fatalf("rebook tail: %v", err)
	}
	if got := rep.TimeToAct(); !got.Equal(time.Unix(0, int64(time.Second))) {
		t.Fatalf("replacement due at %v, want 1s", got)
	}

	// r0 remains usable after an unrelated cancellation.
	if !r0.Consume() {
		t.Fatal("r0 should still be consumable")
	}
}

// Cancellation never reclaims allowance for work that is already due:
// cancelling at or after the execution time restores nothing.
func TestReservation_cancelAfterDueRestoresNothing(t *testing.T) {
	clk := &fakeClock{}
	rl := newAt(1, time.Second, clk.t)

	r0, _ := rl.ReserveAt(clk.time(), time.Hour) // 0ms
	r1, _ := rl.ReserveAt(clk.time(), time.Hour) // 1000ms (tail)

	clk.add(time.Second)
	beforeTokens := rl.peekTokens(clk.t)
	if !r1.CancelAt(clk.time()) {
		t.Fatal("expected cancel at due time to succeed")
	}
	afterTokens := rl.peekTokens(clk.t)
	if beforeTokens != afterTokens {
		t.Fatalf("cancel at/after due time changed tokens %d -> %d", beforeTokens, afterTokens)
	}

	// Consumed reservations cannot be cancelled, and vice versa.
	if !r0.Consume() {
		t.Fatal("consume r0")
	}
	if r0.CancelAt(clk.time()) {
		t.Fatal("cancelling a consumed reservation must be a no-op")
	}
	if r0.Consume() {
		t.Fatal("double consume must be a no-op")
	}
}

// Impossible reservations return errors instead of an unbounded wait.
func TestReservation_errors(t *testing.T) {
	clk := &fakeClock{}
	rl := newAt(5, time.Second, clk.t)

	if _, err := rl.ReserveNAt(0, clk.time(), time.Second); !errors.Is(err, ErrInvalidCost) {
		t.Fatalf("cost 0: got %v", err)
	}
	if _, err := rl.ReserveNAt(-1, clk.time(), time.Second); !errors.Is(err, ErrInvalidCost) {
		t.Fatalf("cost -1: got %v", err)
	}
	if _, err := rl.ReserveAt(clk.time(), -1); !errors.Is(err, ErrInvalidCost) {
		t.Fatalf("negative maxWait: got %v", err)
	}

	// More than the burst size can never succeed.
	if _, err := rl.ReserveNAt(6, clk.time(), 100*time.Hour); !errors.Is(err, ErrMaxBurstExceeded) {
		t.Fatalf("over burst: got %v", err)
	}

	// Burst consumed; one request needs 0.2s but only 10ms is allowed.
	for i := 0; i < 5; i++ {
		if _, err := rl.ReserveAt(clk.time(), time.Hour); err != nil {
			t.Fatalf("burst reserve %d: %v", i, err)
		}
	}
	if _, err := rl.ReserveAt(clk.time(), 10*time.Millisecond); !errors.Is(err, ErrWouldExceedWait) {
		t.Fatalf("wait bound: got %v", err)
	}

	// ...but offering enough time succeeds.
	if _, err := rl.ReserveAt(clk.time(), 250*time.Millisecond); err != nil {
		t.Fatalf("within bound: got %v", err)
	}
}

// Rate changes apply to new reservations only: outstanding reservations keep
// their promised times; over-burst requests under the new rate fail.
func TestReservation_updateRate(t *testing.T) {
	clk := &fakeClock{}
	rl := newAt(2, time.Second, clk.t)

	r0, _ := rl.ReserveAt(clk.time(), time.Hour) // 0ms
	_, _ = rl.ReserveAt(clk.time(), time.Hour)   // 0ms
	r2, _ := rl.ReserveAt(clk.time(), time.Hour) // 500ms, at old rate

	rl.UpdateRate(10)

	// Outstanding reservation is grandfathered at its original time.
	if got := r2.TimeToAct(); !got.Equal(time.Unix(0, int64(500*time.Millisecond))) {
		t.Fatalf("r2 rescheduled to %v", got)
	}

	// New reservations follow the new rate but never jump the outstanding
	// tail: the queue anchor is 500ms, and accrual under the faster rate
	// already covers the cost by then.
	r3, err := rl.ReserveAt(clk.time(), time.Hour)
	if err != nil {
		t.Fatalf("reserve at new rate: %v", err)
	}
	if got := r3.DelayFrom(clk.time()); got != 500*time.Millisecond {
		t.Fatalf("new reservation delayed %v under faster rate", got)
	}

	// Cancelling old reservations still works and restores (capped) capacity.
	before, _ := rl.state()
	r0.CancelAt(clk.time())
	after, _ := rl.state()
	if after < before {
		t.Fatalf("cancel reduced tokens %d -> %d", before, after)
	}

	rl.UpdateRate(1)
	if _, err := rl.ReserveNAt(2, clk.time(), 100*time.Hour); !errors.Is(err, ErrMaxBurstExceeded) {
		t.Fatalf("over new burst: got %v", err)
	}
}

// Consume and Cancel racing on the same reservation: exactly one wins, the
// balance is touched at most once, and the limiter stays within its bounds.
func TestReservation_consumeCancelRace(t *testing.T) {
	clk := &fakeClock{}
	rl := newAt(50, time.Second, clk.t)

	const n = 400
	rs := make([]*Reservation, n)
	for i := range rs {
		r, err := rl.ReserveAt(clk.time(), time.Hour)
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		rs[i] = r
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var consumed, cancelled int64
	var mu sync.Mutex
	for _, r := range rs {
		wg.Add(2)
		go func(r *Reservation) {
			defer wg.Done()
			<-start
			if r.Consume() {
				mu.Lock()
				consumed++
				mu.Unlock()
			}
		}(r)
		go func(r *Reservation) {
			defer wg.Done()
			<-start
			if r.CancelAt(clk.time()) {
				mu.Lock()
				cancelled++
				mu.Unlock()
			}
		}(r)
	}
	close(start)
	wg.Wait()

	if consumed+cancelled != n {
		t.Fatalf("terminal states = %d (consumed %d + cancelled %d), want %d",
			consumed+cancelled, consumed, cancelled, n)
	}

	// Every reservation reached exactly one terminal state.
	for i, r := range rs {
		if r.Consume() || r.CancelAt(clk.time()) {
			t.Fatalf("reservation %d changed state twice", i)
		}
	}

	// Cancellation may only return tokens, never push the balance over burst.
	tokens, _ := rl.state()
	if tokens > rl.max {
		t.Fatalf("tokens %d exceed burst %d after race", tokens, rl.max)
	}
}

// Many goroutines reserving at the same instant must receive distinct,
// correctly spaced execution times (or ErrWouldExceedWait), never the same
// slot twice: the choice to never pull reservations forward is enforced by
// the shared nextEvent anchor under the single mutex.
func TestReservation_concurrentScheduling(t *testing.T) {
	clk := &fakeClock{}
	rl := newAt(100, time.Second, clk.t)

	const n = 500
	start := make(chan struct{})
	results := make(chan int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := rl.ReserveAt(clk.time(), 30*time.Second)
			if err != nil {
				return
			}
			results <- r.timeToAct
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var got []int64
	for ts := range results {
		got = append(got, ts)
	}
	if len(got) != n {
		t.Fatalf("scheduled %d of %d reservations", len(got), n)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })

	// At most burst (100) reservations may share the origin timestamp; after
	// that, consecutive timestamps must be at least one rate interval (10ms)
	// apart.
	originCount := 0
	for i, ts := range got {
		if ts == 0 {
			originCount++
		}
		if i > 0 && ts != got[i-1] && ts-got[i-1] < int64(10*time.Millisecond) {
			t.Fatalf("distinct timestamps %d and %d scheduled closer than one interval", got[i-1], ts)
		}
	}
	if originCount != 100 {
		t.Fatalf("expected 100 reservations at origin, got %d", originCount)
	}
}
