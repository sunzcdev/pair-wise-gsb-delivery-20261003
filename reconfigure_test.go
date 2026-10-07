package ratelimit_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/bsm/ratelimit/v3"
)

type fakeClock struct{ now int64 }

func newFakeClock() *fakeClock { return &fakeClock{now: time.Now().UnixNano()} }

func (c *fakeClock) UnixNano() int64         { return atomic.LoadInt64(&c.now) }
func (c *fakeClock) Advance(d time.Duration) { atomic.AddInt64(&c.now, int64(d)) }

// countAllowed consumes the bucket without advancing the clock and
// returns the number of permitted calls.
func countAllowed(rl *RateLimiter) int {
	count := 0
	for !rl.Limit() {
		count++
	}
	return count
}

func TestReconfigure_preserveAllowance(t *testing.T) {
	clock := newFakeClock()
	rl := NewWithClock(10, time.Second, clock.UnixNano)

	// consume 6 of 10
	for i := 0; i < 6; i++ {
		if rl.Limit() {
			t.Fatalf("expected no limit on cycle %d", i+1)
		}
	}

	// keep absolute balance: 4 units remain, under the new max of 5
	if !rl.Reconfigure(5, time.Second, PreserveAllowance) {
		t.Fatal("expected reconfigure to succeed")
	}
	if exp, got := 4, countAllowed(rl); exp != got {
		t.Errorf("expected %v, got %v", exp, got)
	}
}

func TestReconfigure_preserveAllowanceClampsToNewMax(t *testing.T) {
	clock := newFakeClock()
	rl := NewWithClock(10, time.Second, clock.UnixNano)

	// full bucket, shrink capacity: absolute balance is clamped to the new max
	if !rl.Reconfigure(4, time.Second, PreserveAllowance) {
		t.Fatal("expected reconfigure to succeed")
	}
	if exp, got := 4, countAllowed(rl); exp != got {
		t.Errorf("expected %v, got %v", exp, got)
	}
}

func TestReconfigure_preserveRatio(t *testing.T) {
	clock := newFakeClock()
	rl := NewWithClock(10, time.Second, clock.UnixNano)

	// consume 5 of 10 -> bucket is half full
	for i := 0; i < 5; i++ {
		if rl.Limit() {
			t.Fatalf("expected no limit on cycle %d", i+1)
		}
	}

	// keep fill ratio: half of 20 is 10
	if !rl.Reconfigure(20, time.Second, PreserveRatio) {
		t.Fatal("expected reconfigure to succeed")
	}
	if exp, got := 10, countAllowed(rl); exp != got {
		t.Errorf("expected %v, got %v", exp, got)
	}
}

func TestReconfigure_shorterPeriod(t *testing.T) {
	clock := newFakeClock()
	rl := NewWithClock(10, time.Minute, clock.UnixNano)

	// consume 5 of 10 -> bucket is half full
	for i := 0; i < 5; i++ {
		if rl.Limit() {
			t.Fatalf("expected no limit on cycle %d", i+1)
		}
	}

	// same rate, shorter period: ratio strategy keeps half of the new max
	if !rl.Reconfigure(10, time.Second, PreserveRatio) {
		t.Fatal("expected reconfigure to succeed")
	}
	if exp, got := 5, countAllowed(rl); exp != got {
		t.Errorf("expected %v, got %v", exp, got)
	}
}

func TestReconfigure_emptyAndFullBuckets(t *testing.T) {
	clock := newFakeClock()

	// empty bucket stays empty under both strategies
	for _, strategy := range []MigrationStrategy{PreserveAllowance, PreserveRatio} {
		rl := NewWithClock(10, time.Second, clock.UnixNano)
		countAllowed(rl)
		if !rl.Reconfigure(20, time.Second, strategy) {
			t.Fatal("expected reconfigure to succeed")
		}
		if exp, got := 0, countAllowed(rl); exp != got {
			t.Errorf("strategy %v: expected %v, got %v", strategy, exp, got)
		}
	}

	// full bucket, growing capacity: PreserveAllowance keeps the absolute
	// balance of 10, PreserveRatio keeps the bucket full at 20
	for _, tc := range []struct {
		strategy MigrationStrategy
		exp      int
	}{
		{PreserveAllowance, 10},
		{PreserveRatio, 20},
	} {
		rl := NewWithClock(10, time.Second, clock.UnixNano)
		if !rl.Reconfigure(20, time.Second, tc.strategy) {
			t.Fatal("expected reconfigure to succeed")
		}
		if exp, got := tc.exp, countAllowed(rl); exp != got {
			t.Errorf("strategy %v: expected %v, got %v", tc.strategy, exp, got)
		}
	}
}

func TestReconfigure_settlesBeforeSwitch(t *testing.T) {
	clock := newFakeClock()
	rl := NewWithClock(10, time.Second, clock.UnixNano)

	// drain the bucket, then let half a second pass under the old config
	countAllowed(rl)
	clock.Advance(500 * time.Millisecond)

	// the 5 units accrued under the old config must survive the switch
	if !rl.Reconfigure(10, time.Second, PreserveAllowance) {
		t.Fatal("expected reconfigure to succeed")
	}
	if exp, got := 5, countAllowed(rl); exp != got {
		t.Errorf("expected %v, got %v", exp, got)
	}

	// refill continues under the new config from the switch moment
	clock.Advance(200 * time.Millisecond)
	if exp, got := 2, countAllowed(rl); exp != got {
		t.Errorf("expected %v, got %v", exp, got)
	}
}

func TestReconfigure_invalidParamsAreNoop(t *testing.T) {
	clock := newFakeClock()
	rl := NewWithClock(3, time.Second, clock.UnixNano)

	for _, args := range []struct {
		rate     int
		per      time.Duration
		strategy MigrationStrategy
	}{
		{0, time.Second, PreserveAllowance},
		{-5, time.Second, PreserveRatio},
		{3, 0, PreserveAllowance},
		{3, -time.Second, PreserveRatio},
		{3, time.Second, MigrationStrategy(42)},
	} {
		if rl.Reconfigure(args.rate, args.per, args.strategy) {
			t.Errorf("expected reconfigure(%v, %v, %v) to fail", args.rate, args.per, args.strategy)
		}
	}

	// the limiter is untouched: still 3 per second
	if exp, got := 3, countAllowed(rl); exp != got {
		t.Errorf("expected %v, got %v", exp, got)
	}
	clock.Advance(time.Second)
	if exp, got := 3, countAllowed(rl); exp != got {
		t.Errorf("expected %v, got %v", exp, got)
	}
}

func TestReconfigure_repeatedSwitchingCreatesNoAllowance(t *testing.T) {
	clock := newFakeClock()

	for _, strategy := range []MigrationStrategy{PreserveAllowance, PreserveRatio} {
		rl := NewWithClock(10, time.Second, clock.UnixNano)

		// consume 3 of 10
		for i := 0; i < 3; i++ {
			if rl.Limit() {
				t.Fatalf("expected no limit on cycle %d", i+1)
			}
		}

		// bounce back and forth between two configurations without
		// advancing the clock
		for i := 0; i < 100; i++ {
			if !rl.Reconfigure(100, time.Second, strategy) {
				t.Fatal("expected reconfigure to succeed")
			}
			if !rl.Reconfigure(10, time.Second, strategy) {
				t.Fatal("expected reconfigure to succeed")
			}
		}

		// the remaining 7 units must be preserved, nothing created
		if exp, got := 7, countAllowed(rl); exp != got {
			t.Errorf("strategy %v: expected %v, got %v", strategy, exp, got)
		}

		// and a drained bucket must not gain allowance from switching
		for i := 0; i < 100; i++ {
			rl.Reconfigure(100, time.Second, strategy)
			rl.Reconfigure(10, time.Second, strategy)
		}
		if exp, got := 0, countAllowed(rl); exp != got {
			t.Errorf("strategy %v: expected %v, got %v", strategy, exp, got)
		}
	}
}

func TestReconfigure_concurrentWithLimit(t *testing.T) {
	for _, strategy := range []MigrationStrategy{PreserveAllowance, PreserveRatio} {
		rl := New(100, 10*time.Millisecond)

		var wg sync.WaitGroup
		stop := make(chan struct{})

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					rl.Reconfigure(100, 10*time.Millisecond, strategy)
					rl.Reconfigure(50, 10*time.Millisecond, strategy)
				}
			}
		}()

		allowed := 0
		deadline := time.Now().Add(100 * time.Millisecond)
		for time.Now().Before(deadline) {
			if !rl.Limit() {
				allowed++
			}
		}
		close(stop)
		wg.Wait()

		// at most 100 per 10ms window plus the initial burst
		if max := 1200; allowed > max {
			t.Errorf("strategy %v: expected at most %v allowed calls, got %v", strategy, max, allowed)
		}
	}
}

func TestUpdateRate_keepsLegacyBehaviour(t *testing.T) {
	clock := newFakeClock()
	rl := NewWithClock(5, time.Second, clock.UnixNano)

	// legacy UpdateRate only adjusts rate and max, the balance is untouched
	rl.UpdateRate(10)
	if exp, got := 5, countAllowed(rl); exp != got {
		t.Errorf("expected %v, got %v", exp, got)
	}

	// refill follows the new rate
	clock.Advance(time.Second)
	if exp, got := 10, countAllowed(rl); exp != got {
		t.Errorf("expected %v, got %v", exp, got)
	}
}
