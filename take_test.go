package ratelimit_test

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/bsm/ratelimit/v3"
)

// fakeClock is a manually advanced Clock for tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestRateLimiter_Take_allOrNothing(t *testing.T) {
	clock := newFakeClock()
	rl := New(10, time.Second, WithClock(clock))

	if ok, at := rl.Take(7); !ok || !at.Equal(clock.Now()) {
		t.Errorf("expected Take(7) to succeed at now, got %v, %v", ok, at)
	}

	// only 3 units remain: Take(4) must fail and deduct nothing
	if ok, _ := rl.Take(4); ok {
		t.Error("expected Take(4) to be rejected")
	}
	if ok, _ := rl.Take(3); !ok {
		t.Error("expected Take(3) to succeed after failed Take(4)")
	}
	if ok, _ := rl.Take(1); ok {
		t.Error("expected Take(1) to be rejected on empty bucket")
	}
}

func TestRateLimiter_Take_zeroCost(t *testing.T) {
	clock := newFakeClock()
	rl := New(5, time.Second, WithClock(clock))

	if ok, _ := rl.Take(5); !ok {
		t.Fatal("expected Take(5) to succeed")
	}
	// bucket is empty, but zero cost must still succeed
	if ok, at := rl.Take(0); !ok || !at.Equal(clock.Now()) {
		t.Errorf("expected Take(0) to succeed at now, got %v, %v", ok, at)
	}
	if ok, at := rl.Estimate(0); !ok || !at.Equal(clock.Now()) {
		t.Errorf("expected Estimate(0) to succeed at now, got %v, %v", ok, at)
	}
}

func TestRateLimiter_Take_exceedsCapacity(t *testing.T) {
	clock := newFakeClock()
	rl := New(10, time.Second, WithClock(clock))

	ok, at := rl.Take(11)
	if ok {
		t.Error("expected Take(11) to be rejected")
	}
	if !at.IsZero() {
		t.Errorf("expected zero time for unsatisfiable cost, got %v", at)
	}

	// the failed attempt must not have consumed anything
	if ok, _ := rl.Take(10); !ok {
		t.Error("expected Take(10) to succeed after rejected Take(11)")
	}
}

func TestRateLimiter_Take_overflow(t *testing.T) {
	clock := newFakeClock()
	rl := New(10, time.Second, WithClock(clock))

	ok, at := rl.Take(math.MaxUint64)
	if ok {
		t.Error("expected Take(MaxUint64) to be rejected")
	}
	if !at.IsZero() {
		t.Errorf("expected zero time for overflowing cost, got %v", at)
	}

	// limiter must remain fully functional
	if ok, _ := rl.Take(10); !ok {
		t.Error("expected Take(10) to succeed after overflowing request")
	}
}

func TestRateLimiter_Take_earliestAcceptance(t *testing.T) {
	clock := newFakeClock()
	rl := New(10, time.Second, WithClock(clock)) // 1 unit per 100ms

	if ok, _ := rl.Take(10); !ok {
		t.Fatal("expected Take(10) to succeed")
	}

	// bucket empty: 3 units require 300ms of refill
	want := clock.Now().Add(300 * time.Millisecond)
	ok, at := rl.Take(3)
	if ok {
		t.Error("expected Take(3) to be rejected on empty bucket")
	}
	if !at.Equal(want) {
		t.Errorf("expected earliest acceptance at %v, got %v", want, at)
	}

	// estimate must agree with the actual deduction
	if ok, at := rl.Estimate(3); ok || !at.Equal(want) {
		t.Errorf("expected Estimate(3) to report %v, got %v, %v", want, ok, at)
	}

	// advance to just before the deadline: still rejected
	clock.Advance(299 * time.Millisecond)
	if ok, _ := rl.Take(3); ok {
		t.Error("expected Take(3) to be rejected before the deadline")
	}

	// advance past the deadline: accepted
	clock.Advance(2 * time.Millisecond)
	if ok, _ := rl.Take(3); !ok {
		t.Error("expected Take(3) to succeed after the deadline")
	}
}

func TestRateLimiter_Take_refillCapsAtMax(t *testing.T) {
	clock := newFakeClock()
	rl := New(10, time.Second, WithClock(clock))

	if ok, _ := rl.Take(10); !ok {
		t.Fatal("expected Take(10) to succeed")
	}

	// a very long idle period must refill to capacity, never beyond
	clock.Advance(1000 * time.Hour)
	if ok, _ := rl.Take(10); !ok {
		t.Error("expected Take(10) to succeed after long idle")
	}
	if ok, _ := rl.Take(1); ok {
		t.Error("expected allowance to be capped at capacity")
	}
}

func TestRateLimiter_Estimate_doesNotPreSpend(t *testing.T) {
	clock := newFakeClock()
	rl := New(10, time.Second, WithClock(clock))

	for i := 0; i < 100; i++ {
		if ok, _ := rl.Estimate(10); !ok {
			t.Fatalf("expected Estimate(10) to succeed on iteration %d", i)
		}
	}
	// repeated estimates must not have consumed anything
	if ok, _ := rl.Take(10); !ok {
		t.Error("expected Take(10) to succeed after repeated estimates")
	}
	if ok, _ := rl.Take(1); ok {
		t.Error("expected bucket to be empty after Take(10)")
	}
}

func TestRateLimiter_Take_concurrent(t *testing.T) {
	rl := New(100, time.Minute)

	var accepted int64
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := rl.Take(1); ok {
				atomic.AddInt64(&accepted, 1)
			}
		}()
	}
	wg.Wait()

	if exp := int64(100); accepted != exp {
		t.Errorf("expected %v accepted requests, got %v", exp, accepted)
	}
}

func TestRateLimiter_Limit_compatibleWithClock(t *testing.T) {
	clock := newFakeClock()
	rl := New(5, time.Second, WithClock(clock))

	for i := 0; i < 5; i++ {
		if rl.Limit() {
			t.Fatalf("expected no limit on cycle %d", i+1)
		}
	}
	if !rl.Limit() {
		t.Error("expected limit on cycle 6")
	}

	// Undo still restores a single unit
	rl.Undo()
	if rl.Limit() {
		t.Error("expected no limit after undo")
	}
	if !rl.Limit() {
		t.Error("expected to limit again")
	}

	// refill follows the injected clock, not real time
	clock.Advance(200 * time.Millisecond)
	if rl.Limit() {
		t.Error("expected no limit after advancing the fake clock")
	}
}
