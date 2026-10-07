package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a deterministic, manually advanced clock.
type fakeClock struct {
	nano int64
}

func (c *fakeClock) now() int64              { return c.nano }
func (c *fakeClock) advance(d time.Duration) { c.nano += int64(d) }

func newFakeLimiter(rate int, per time.Duration) (*RateLimiter, *fakeClock) {
	clock := &fakeClock{nano: 1_000_000_000}
	return newLimiter(rate, uint64(per), clock.now), clock
}

func TestReconfigure_invalidParams(t *testing.T) {
	rl, clock := newFakeLimiter(10, time.Second)
	clock.advance(300 * time.Millisecond)

	// Consume 2 units so we have a non-trivial state.
	for i := 0; i < 2; i++ {
		if rl.Limit() {
			t.Fatalf("unexpected limit on cycle %d", i+1)
		}
	}

	snapRate, snapUnit := rl.rate, rl.unit
	snapMax, snapAllowance, snapLastCheck := rl.max, rl.allowance, rl.lastCheck

	bad := []struct {
		name     string
		rate     int
		per      time.Duration
		strategy ReconfigureStrategy
	}{
		{"zero rate", 0, time.Second, PreserveAllowance},
		{"negative rate", -5, time.Second, PreserveAllowance},
		{"zero period", 10, 0, PreserveAllowance},
		{"negative period", 10, -time.Second, PreserveFillRatio},
		{"unknown strategy", 10, time.Second, ReconfigureStrategy(42)},
		{"overflowing capacity", 1 << 40, time.Hour, PreserveAllowance},
	}
	for _, tc := range bad {
		if ok := rl.Reconfigure(tc.rate, tc.per, tc.strategy); ok {
			t.Errorf("%s: expected Reconfigure to fail", tc.name)
		}
	}

	// The whole update must not have taken effect.
	if rl.rate != snapRate || rl.unit != snapUnit || rl.max != snapMax ||
		rl.allowance != snapAllowance || rl.lastCheck != snapLastCheck {
		t.Errorf("invalid reconfigure mutated state: got %+v", rl)
	}
}

func TestReconfigure_preserveAllowance(t *testing.T) {
	rl, clock := newFakeLimiter(10, time.Second)

	// Consume 3 of 10 units.
	for i := 0; i < 3; i++ {
		rl.Limit()
	}
	// Accrue 2 more units under the old config before the switch.
	clock.advance(200 * time.Millisecond)

	if ok := rl.Reconfigure(5, 2*time.Second, PreserveAllowance); !ok {
		t.Fatal("expected Reconfigure to succeed")
	}

	// Settled balance (10-3+2 = 9 units) is kept absolutely,
	// new capacity is 5*2s.
	if exp, got := uint64(9_000_000_000), rl.allowance; exp != got {
		t.Errorf("expected allowance %d, got %d", exp, got)
	}
	if exp, got := uint64(5), rl.rate; exp != got {
		t.Errorf("expected rate %d, got %d", exp, got)
	}
	if exp, got := uint64(2*time.Second), rl.unit; exp != got {
		t.Errorf("expected unit %d, got %d", exp, got)
	}
	if exp, got := uint64(10_000_000_000), rl.max; exp != got {
		t.Errorf("expected max %d, got %d", exp, got)
	}
	if exp, got := clock.nano, rl.lastCheck; exp != got {
		t.Errorf("expected lastCheck %d, got %d", exp, got)
	}

	// Shrinking the capacity clamps the preserved balance to the new max.
	if ok := rl.Reconfigure(1, time.Second, PreserveAllowance); !ok {
		t.Fatal("expected Reconfigure to succeed")
	}
	if exp, got := uint64(1_000_000_000), rl.allowance; exp != got {
		t.Errorf("expected clamped allowance %d, got %d", exp, got)
	}
}

func TestReconfigure_preserveFillRatio(t *testing.T) {
	rl, clock := newFakeLimiter(10, time.Second)

	// Drain to exactly 50%.
	for i := 0; i < 5; i++ {
		rl.Limit()
	}
	if ok := rl.Reconfigure(4, time.Second, PreserveFillRatio); !ok {
		t.Fatal("expected Reconfigure to succeed")
	}
	// 50% of the new 4*1s capacity.
	if exp, got := uint64(2_000_000_000), rl.allowance; exp != got {
		t.Errorf("expected allowance %d, got %d", exp, got)
	}

	// A fully empty bucket stays empty.
	for i := 0; i < 2; i++ {
		rl.Limit()
	}
	if exp, got := uint64(0), rl.allowance; got != exp {
		t.Fatalf("expected empty bucket, got %d", got)
	}
	if ok := rl.Reconfigure(100, time.Minute, PreserveFillRatio); !ok {
		t.Fatal("expected Reconfigure to succeed")
	}
	if exp, got := uint64(0), rl.allowance; got != exp {
		t.Errorf("expected empty bucket to stay empty, got %d", got)
	}

	// A fully full bucket stays full.
	clock.advance(time.Minute)
	rl.settle(clock.nano)
	if exp, got := uint64(100)*uint64(time.Minute), rl.allowance; exp != got {
		t.Fatalf("expected full bucket %d, got %d", exp, got)
	}
	if ok := rl.Reconfigure(7, 500*time.Millisecond, PreserveFillRatio); !ok {
		t.Fatal("expected Reconfigure to succeed")
	}
	if exp, got := uint64(3_500_000_000), rl.allowance; exp != got {
		t.Errorf("expected full bucket to stay full (%d), got %d", exp, got)
	}
}

func TestReconfigure_shorterPeriodAndSmallerCapacity(t *testing.T) {
	rl, clock := newFakeLimiter(10, time.Minute)

	// Accrue under the old config, then switch to a much shorter period
	// with a smaller absolute capacity.
	clock.advance(30 * time.Second)
	if ok := rl.Reconfigure(2, time.Second, PreserveFillRatio); !ok {
		t.Fatal("expected Reconfigure to succeed")
	}
	// Bucket was full (10*60s), so it maps to a full 2*1s bucket.
	if exp, got := uint64(2_000_000_000), rl.allowance; exp != got {
		t.Errorf("expected allowance %d, got %d", exp, got)
	}

	// Same switch with absolute preservation clamps to the new max.
	rl2, clock2 := newFakeLimiter(10, time.Minute)
	clock2.advance(30 * time.Second)
	if ok := rl2.Reconfigure(2, time.Second, PreserveAllowance); !ok {
		t.Fatal("expected Reconfigure to succeed")
	}
	if exp, got := uint64(2_000_000_000), rl2.allowance; exp != got {
		t.Errorf("expected clamped allowance %d, got %d", exp, got)
	}
}

func TestReconfigure_roundTripCreatesNoAllowance(t *testing.T) {
	for _, strategy := range []ReconfigureStrategy{PreserveAllowance, PreserveFillRatio} {
		rl, _ := newFakeLimiter(10, time.Second)

		// Consume 3 units; no time passes during the round trips.
		for i := 0; i < 3; i++ {
			rl.Limit()
		}
		initial := rl.allowance

		for i := 0; i < 100; i++ {
			if !rl.Reconfigure(7, 500*time.Millisecond, strategy) {
				t.Fatal("expected Reconfigure to succeed")
			}
			if !rl.Reconfigure(10, time.Second, strategy) {
				t.Fatal("expected Reconfigure to succeed")
			}
			if rl.allowance > initial {
				t.Fatalf("strategy %d, round %d: allowance grew from %d to %d without time passing",
					strategy, i, initial, rl.allowance)
			}
		}
	}
}

func TestReconfigure_neverBeatsControlLimiter(t *testing.T) {
	for _, strategy := range []ReconfigureStrategy{PreserveAllowance, PreserveFillRatio} {
		clock := &fakeClock{nano: 1_000_000_000}
		reconfigured := newLimiter(10, uint64(time.Second), clock.now)
		control := newLimiter(10, uint64(time.Second), clock.now)

		configs := []struct {
			rate int
			per  time.Duration
		}{
			{7, 500 * time.Millisecond},
			{10, time.Second},
			{3, 250 * time.Millisecond},
			{10, time.Second},
		}
		for round := 0; round < 25; round++ {
			for _, cfg := range configs {
				clock.advance(137 * time.Millisecond)
				if !reconfigured.Reconfigure(cfg.rate, cfg.per, strategy) {
					t.Fatal("expected Reconfigure to succeed")
				}
				// The control limiter accrues under the original config.
				control.settle(clock.nano)

				if reconfigured.allowance > control.allowance {
					t.Fatalf("strategy %d, round %d: reconfigured allowance %d exceeds control %d",
						strategy, round, reconfigured.allowance, control.allowance)
				}
				if reconfigured.allowance > reconfigured.max {
					t.Fatalf("strategy %d, round %d: allowance %d exceeds max %d",
						strategy, round, reconfigured.allowance, reconfigured.max)
				}
			}
		}
	}
}

func TestReconfigure_serializedWithLimit(t *testing.T) {
	rl, _ := newFakeLimiter(1000, time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				rl.Limit()
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(strategy ReconfigureStrategy) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				rl.Reconfigure(100+j, time.Second, strategy)
			}
		}(ReconfigureStrategy(i))
	}
	wg.Wait()

	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.allowance > rl.max {
		t.Fatalf("allowance %d exceeds max %d after concurrent reconfiguration",
			rl.allowance, rl.max)
	}
}

func TestUpdateRate_compatBehavior(t *testing.T) {
	rl, clock := newFakeLimiter(10, time.Second)

	// Consume 3 units, then update only the rate.
	for i := 0; i < 3; i++ {
		rl.Limit()
	}
	rl.UpdateRate(5)

	// Legacy behavior: unit and (unsettled) allowance stay untouched,
	// only rate and max change.
	if exp, got := uint64(5), rl.rate; exp != got {
		t.Errorf("expected rate %d, got %d", exp, got)
	}
	if exp, got := uint64(5_000_000_000), rl.max; exp != got {
		t.Errorf("expected max %d, got %d", exp, got)
	}
	if exp, got := uint64(time.Second), rl.unit; exp != got {
		t.Errorf("expected unit %d, got %d", exp, got)
	}
	if exp, got := uint64(7_000_000_000), rl.allowance; exp != got {
		t.Errorf("expected untouched allowance %d, got %d", exp, got)
	}

	// The stale allowance is settled (and capped) on the next Limit call.
	clock.advance(time.Second)
	rl.Limit()
	if exp, got := uint64(4_000_000_000), rl.allowance; exp != got {
		t.Errorf("expected settled allowance %d, got %d", exp, got)
	}
}
