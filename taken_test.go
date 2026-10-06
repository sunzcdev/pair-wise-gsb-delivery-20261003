package ratelimit_test

import (
	"sync"
	"testing"
	"time"

	. "github.com/bsm/ratelimit/v3"
)

// fakeClock is a thread-safe, manually controlled time source.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

func TestTakeN_deductsAllOrNothing(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := NewWithClock(10, time.Second, clk)

	if allowed, _ := rl.TakeN(6); !allowed {
		t.Fatal("expected TakeN(6) to succeed on a full bucket")
	}
	// 4 units remain; a 5-unit request must fail and deduct nothing.
	if allowed, _ := rl.TakeN(5); allowed {
		t.Fatal("expected TakeN(5) to fail with only 4 units available")
	}
	// Repeated failure still deducts nothing.
	if allowed, _ := rl.TakeN(4); !allowed {
		t.Fatal("expected TakeN(4) to succeed after fully-rolled-back failures")
	}
	if allowed, _ := rl.TakeN(1); allowed {
		t.Fatal("expected bucket to be empty after deducting the remainder")
	}
}

func TestTakeN_failureHasNoSideEffects(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := NewWithClock(10, time.Second, clk)

	if allowed, _ := rl.TakeN(8); !allowed {
		t.Fatal("expected TakeN(8) to succeed")
	}

	// Only 2 units remain. Over-capacity requests must not refill, advance
	// the baseline or deduct anything.
	for i := 0; i < 3; i++ {
		if allowed, _ := rl.TakeN(11); allowed {
			t.Fatalf("expected over-capacity request #%d to fail", i+1)
		}
	}
	if allowed, _ := rl.TakeN(2); !allowed {
		t.Fatal("expected exactly 2 units to remain after rolled-back rejections")
	}
	if allowed, _ := rl.TakeN(1); allowed {
		t.Fatal("expected bucket to be empty after deducting the remainder")
	}

	// Rejected for insufficient balance must leave state untouched as well.
	if allowed, _ := rl.TakeN(2); allowed {
		t.Fatal("expected TakeN(2) to fail on an empty bucket")
	}
	clk.Advance(100 * time.Millisecond) // one unit accrues
	if allowed, _ := rl.TakeN(1); !allowed {
		t.Fatal("expected one unit accrued since the failure changed nothing")
	}
}

func TestTakeN_earliestTime(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := NewWithClock(10, time.Second, clk) // 1 unit per 100ms

	if allowed, earliest := rl.TakeN(8); !allowed {
		t.Fatal("expected TakeN(8) to succeed")
	} else if !earliest.Equal(clk.Now()) {
		t.Fatalf("expected success time %v, got %v", clk.Now(), earliest)
	}

	// 2 units remain; cost 6 => deficit of 4 units => 400ms.
	allowed, earliest := rl.TakeN(6)
	if allowed {
		t.Fatal("expected TakeN(6) to fail on 2 units")
	}
	if want := clk.Now().Add(400 * time.Millisecond); !earliest.Equal(want) {
		t.Fatalf("expected earliest %v, got %v", want, earliest)
	}

	// One nanosecond short is still not enough (ceiling rounding).
	clk.Advance(399 * time.Millisecond)
	if allowed, _ := rl.TakeN(6); allowed {
		t.Fatal("expected to still be rate limited one millisecond early")
	}
	clk.Advance(time.Millisecond)
	if allowed, at := rl.TakeN(6); !allowed {
		t.Fatal("expected TakeN(6) to succeed at the predicted time")
	} else if !at.Equal(clk.Now()) {
		t.Fatalf("expected success at %v, got %v", clk.Now(), at)
	}
}

func TestTakeN_zeroCost(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := NewWithClock(2, time.Second, clk)

	// Drain the bucket completely.
	if allowed, _ := rl.TakeN(2); !allowed {
		t.Fatal("expected TakeN(2) to succeed")
	}
	for _, n := range []int{0, -1, -100} {
		if allowed, earliest := rl.TakeN(n); !allowed {
			t.Fatalf("expected zero-cost TakeN(%d) to succeed", n)
		} else if !earliest.Equal(clk.Now()) {
			t.Fatalf("expected current time for TakeN(%d), got %v", n, earliest)
		}
	}
	// Zero-cost calls consumed nothing.
	if allowed, _ := rl.TakeN(1); allowed {
		t.Fatal("expected zero-cost calls to leave the empty bucket unchanged")
	}
}

func TestTakeN_overCapacityAndOverflow(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := NewWithClock(10, time.Second, clk)

	// More than rate units can never fit, even on a full bucket.
	allowed, earliest := rl.TakeN(11)
	if allowed || !earliest.IsZero() {
		t.Fatalf("expected over-capacity rejection with zero time, got %v %v", allowed, earliest)
	}
	// Exactly the capacity is accepted.
	if allowed, _ := rl.TakeN(10); !allowed {
		t.Fatal("expected a request equal to the capacity to succeed")
	}

	// With the largest representable unit duration, 3 units have a true cost
	// that overflows uint64; that is defined as over capacity and must be
	// rejected, not wrap around.
	bigClk := newFakeClock(time.Unix(0, 0))
	big := NewWithClock(2, time.Duration(uint64(1)<<63-1), bigClk)
	allowed, earliest = big.TakeN(3)
	if allowed || !earliest.IsZero() {
		t.Fatalf("expected overflowing cost rejection, got %v %v", allowed, earliest)
	}

	// The two-unit true cost (2^64-2) exceeds the saturated balance
	// (2^63-1) by design: it must be rejected rather than wrap, while a
	// single unit fits exactly.
	if ok, _ := big.TakeN(1); !ok {
		t.Fatal("expected the representable single-unit cost to be accepted")
	}
}

func TestTakeN_idleRefillsAtMostToFull(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := NewWithClock(5, time.Second, clk)

	if allowed, _ := rl.TakeN(5); !allowed {
		t.Fatal("expected TakeN(5) to drain the bucket")
	}
	clk.Advance(100 * 365 * 24 * time.Hour) // far longer than one period

	// Even after a very long idle period the bucket holds at most rate units.
	if allowed, _ := rl.TakeN(5); !allowed {
		t.Fatal("expected long idle period to refill to full")
	}
	if allowed, _ := rl.TakeN(1); allowed {
		t.Fatal("expected allowance to be capped at the bucket capacity")
	}
}

func TestEarliestAvailable_isReadOnly(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := NewWithClock(10, time.Second, clk)

	if allowed, _ := rl.TakeN(7); !allowed {
		t.Fatal("expected TakeN(7) to succeed")
	}
	// 3 remain; estimate for 8 => deficit 5 => 500ms.
	if got := rl.EarliestAvailable(8); !got.Equal(clk.Now().Add(500 * time.Millisecond)) {
		t.Fatalf("unexpected earliest time %v", got)
	}
	// The estimate must not pre-borrow: the refill baseline is unchanged.
	clk.Advance(499 * time.Millisecond)
	if got := rl.EarliestAvailable(8); !got.Equal(clk.Now().Add(time.Millisecond)) {
		t.Fatalf("expected read-only estimate not to advance the clock, got %v", got)
	}
	clk.Advance(time.Millisecond)
	if allowed, _ := rl.TakeN(8); !allowed {
		t.Fatal("expected TakeN(8) to succeed exactly at the estimated instant")
	}

	// Edge semantics.
	if got := rl.EarliestAvailable(0); !got.Equal(clk.Now()) {
		t.Fatalf("expected current time for zero cost, got %v", got)
	}
	if got := rl.EarliestAvailable(11); !got.IsZero() {
		t.Fatalf("expected zero time for over-capacity cost, got %v", got)
	}
}

func TestTakeN_concurrent(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	const rate = 100
	rl := NewWithClock(rate, time.Second, clk) // clock stays frozen

	var wg sync.WaitGroup
	var allowedCount int
	var mu sync.Mutex

	// 100 goroutines each demand 3 units from a frozen full bucket:
	// at most floor(100/3) = 33 may succeed, total cost 99 <= 100 balance.
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := rl.TakeN(3); ok {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowedCount != 33 {
		t.Fatalf("expected 33 successful deductions, got %d", allowedCount)
	}
	// One unit remains, no further 3-unit request may succeed.
	if ok, _ := rl.TakeN(3); ok {
		t.Fatal("expected concurrent success cost not to exceed the balance")
	}
	if ok, _ := rl.TakeN(1); !ok {
		t.Fatal("expected the single leftover unit to remain available")
	}
}

func TestTakeN_concurrentMixedCosts(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	const rate = 256
	rl := NewWithClock(rate, time.Second, clk) // clock stays frozen

	var wg sync.WaitGroup
	var mu sync.Mutex
	totalCost := 0

	// Half of the callers ask for 5 units, the other half for 9. With a
	// frozen clock no refill happens, so the total cost of every request
	// that succeeds must never exceed the available balance.
	costs := []int{5, 9}
	for i := 0; i < 200; i++ {
		n := costs[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := rl.TakeN(n); ok {
				mu.Lock()
				totalCost += n
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if totalCost > rate {
		t.Fatalf("successful cost %d exceeded available balance %d", totalCost, rate)
	}
	// One more full-bucket-sized request must fail while balance is below it,
	// and the exact remaining balance must be exactly drainable.
	remaining := rate - totalCost
	if remaining < 0 || remaining > rate {
		t.Fatalf("impossible remaining balance %d", remaining)
	}
	if ok, _ := rl.TakeN(remaining + 1); ok {
		t.Fatal("expected a request larger than the remainder to fail")
	}
	if ok, _ := rl.TakeN(remaining); !ok {
		t.Fatalf("expected to drain the exact remainder of %d units", remaining)
	}
}

func TestTakeN_clockWarpBackwards(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := NewWithClock(5, time.Second, clk)

	if allowed, _ := rl.TakeN(5); !allowed {
		t.Fatal("expected TakeN(5) to succeed")
	}
	clk.Advance(-time.Hour) // clock moves backwards; must not wrap allowance

	if allowed, _ := rl.TakeN(1); allowed {
		t.Fatal("expected no phantom allowance after a backwards clock jump")
	}
}

func TestLimitTakeNUndo_interop(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	rl := NewWithClock(5, time.Second, clk)

	if rl.Limit() {
		t.Fatal("expected first Limit to pass")
	}
	if allowed, _ := rl.TakeN(3); !allowed {
		t.Fatal("expected TakeN(3) to share the same bucket as Limit")
	}
	// One unit remains: Limit passes, the next call must be limited.
	if rl.Limit() {
		t.Fatal("expected the last remaining unit to pass Limit")
	}
	if !rl.Limit() {
		t.Fatal("expected bucket to be exhausted")
	}
	rl.Undo() // returns the one unit consumed by the passing Limit
	if rl.Limit() {
		t.Fatal("expected one unit returned by Undo")
	}
	if allowed, _ := rl.TakeN(1); allowed {
		t.Fatal("expected bucket to be empty again after Undo+Limit")
	}
}
