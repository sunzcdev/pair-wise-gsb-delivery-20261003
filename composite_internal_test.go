package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type controllableClock struct {
	nowNanos int64
	calls    int64
}

func newControllableClock(now time.Time) *controllableClock {
	return &controllableClock{nowNanos: now.UnixNano()}
}

func (c *controllableClock) Now() time.Time {
	atomic.AddInt64(&c.calls, 1)
	return time.Unix(0, atomic.LoadInt64(&c.nowNanos))
}

func (c *controllableClock) advance(d time.Duration) {
	atomic.AddInt64(&c.nowNanos, int64(d))
}

func (c *controllableClock) callCount() int64 {
	return atomic.LoadInt64(&c.calls)
}

type limiterState struct {
	rate      uint64
	allowance uint64
	max       uint64
	unit      uint64
	lastCheck int64
}

func snapshotLimiters(limiters ...*RateLimiter) []limiterState {
	states := make([]limiterState, len(limiters))
	for i, limiter := range limiters {
		states[i] = limiterState{
			rate:      limiter.rate,
			allowance: limiter.allowance,
			max:       limiter.max,
			unit:      limiter.unit,
			lastCheck: limiter.lastCheck,
		}
	}
	return states
}

func (c *controllableClock) resetCalls() {
	atomic.StoreInt64(&c.calls, 0)
}

func assertLimiterStates(t *testing.T, limiters []*RateLimiter, expected []limiterState) {
	t.Helper()

	actual := snapshotLimiters(limiters...)
	for i := range expected {
		if actual[i] != expected[i] {
			t.Fatalf("limiter %d state = %+v, want %+v", i, actual[i], expected[i])
		}
	}
}

func testBucket(clock clock, capacity uint64) *RateLimiter {
	return newWithClock(int(capacity), time.Nanosecond, clock)
}

func TestLimitAllMergesDuplicateLimiters(t *testing.T) {
	base := time.Unix(0, 100)
	clock := newControllableClock(base)
	global := testBucket(clock, 4)
	tenant := testBucket(clock, 3)
	user := testBucket(clock, 2)
	clock.resetCalls()

	limited, failedAt := LimitAll(
		[]*RateLimiter{global, tenant, global, user},
		[]uint64{1, 1, 2, 1},
	)

	if limited || failedAt != -1 {
		t.Fatalf("LimitAll = %v, %d, want false, -1", limited, failedAt)
	}
	if global.allowance != 1 || tenant.allowance != 2 || user.allowance != 1 {
		t.Fatalf(
			"allowances = %d, %d, %d, want 1, 2, 1",
			global.allowance,
			tenant.allowance,
			user.allowance,
		)
	}
	if global.lastCheck != base.UnixNano() || tenant.lastCheck != base.UnixNano() || user.lastCheck != base.UnixNano() {
		t.Fatal("all limiters must be charged at the same logical time")
	}
	if calls := clock.callCount(); calls != 1 {
		t.Fatalf("clock calls = %d, want 1", calls)
	}
}

func TestLimitAllRejectionLeavesAllBucketsUntouched(t *testing.T) {
	base := time.Unix(0, 100)
	clock := newControllableClock(base)
	global := testBucket(clock, 2)
	tenant := testBucket(clock, 5)
	user := testBucket(clock, 5)
	limiters := []*RateLimiter{global, tenant, user}
	clock.resetCalls()
	before := snapshotLimiters(limiters...)

	limited, failedAt := LimitAll(limiters, []uint64{3, 1, 1})

	if !limited || failedAt != 0 {
		t.Fatalf("LimitAll = %v, %d, want true, 0", limited, failedAt)
	}
	assertLimiterStates(t, limiters, before)
	if calls := clock.callCount(); calls != 1 {
		t.Fatalf("clock calls = %d, want 1", calls)
	}
}

func TestLimitAllFailureIndex(t *testing.T) {
	t.Run("insufficient unique bucket", func(t *testing.T) {
		clock := newControllableClock(time.Unix(0, 100))
		global := testBucket(clock, 5)
		tenant := testBucket(clock, 1)
		user := testBucket(clock, 5)
		limiters := []*RateLimiter{global, tenant, user}
		before := snapshotLimiters(limiters...)

		limited, failedAt := LimitAll(limiters, []uint64{1, 2, 1})

		if !limited || failedAt != 1 {
			t.Fatalf("LimitAll = %v, %d, want true, 1", limited, failedAt)
		}
		assertLimiterStates(t, limiters, before)
	})

	t.Run("insufficient merged bucket", func(t *testing.T) {
		clock := newControllableClock(time.Unix(0, 100))
		global := testBucket(clock, 2)
		tenant := testBucket(clock, 10)
		user := testBucket(clock, 10)
		limiters := []*RateLimiter{global, tenant, global, user}
		clock.resetCalls()
		before := snapshotLimiters(limiters...)

		limited, failedAt := LimitAll(limiters, []uint64{1, 2, 2, 1})

		if !limited || failedAt != 0 {
			t.Fatalf("LimitAll = %v, %d, want true, 0", limited, failedAt)
		}
		assertLimiterStates(t, limiters, before)
	})

	t.Run("merged cost addition overflow", func(t *testing.T) {
		clock := newControllableClock(time.Unix(0, 100))
		global := testBucket(clock, 10)
		tenant := testBucket(clock, 10)
		user := testBucket(clock, 10)
		limiters := []*RateLimiter{global, tenant, global, user}
		clock.resetCalls()
		before := snapshotLimiters(limiters...)

		limited, failedAt := LimitAll(
			limiters,
			[]uint64{1, 1, ^uint64(0), 1},
		)

		if !limited || failedAt != 0 {
			t.Fatalf("LimitAll = %v, %d, want true, 0", limited, failedAt)
		}
		assertLimiterStates(t, limiters, before)
		if calls := clock.callCount(); calls != 0 {
			t.Fatalf("clock calls = %d, want 0", calls)
		}
	})

	t.Run("scaled cost multiplication overflow", func(t *testing.T) {
		clock := newControllableClock(time.Unix(0, 100))
		small := testBucket(clock, 10)
		slow := newWithClock(1, time.Second, clock)
		limiters := []*RateLimiter{small, slow}
		clock.resetCalls()
		before := snapshotLimiters(limiters...)
		const overflowingCost = ^uint64(0)/uint64(time.Second) + 1

		limited, failedAt := LimitAll(limiters, []uint64{1, overflowingCost})

		if !limited || failedAt != 1 {
			t.Fatalf("LimitAll = %v, %d, want true, 1", limited, failedAt)
		}
		assertLimiterStates(t, limiters, before)
		if calls := clock.callCount(); calls != 0 {
			t.Fatalf("clock calls = %d, want 0", calls)
		}
	})
}

func TestLimitAllUsesOneConsistentCheckTime(t *testing.T) {
	base := time.Unix(0, 100)
	clock := newControllableClock(base)
	global := testBucket(clock, 3)
	tenant := testBucket(clock, 3)
	user := testBucket(clock, 3)
	limiters := []*RateLimiter{global, tenant, user}

	if limited, failedAt := LimitAll(limiters, []uint64{3, 3, 3}); limited || failedAt != -1 {
		t.Fatalf("initial LimitAll = %v, %d, want false, -1", limited, failedAt)
	}

	clock.advance(2 * time.Nanosecond)
	if limited, failedAt := LimitAll(limiters, []uint64{2, 2, 2}); limited || failedAt != -1 {
		t.Fatalf("refilled LimitAll = %v, %d, want false, -1", limited, failedAt)
	}

	refilledAt := base.Add(2 * time.Nanosecond).UnixNano()
	for i, limiter := range limiters {
		if limiter.allowance != 1 || limiter.lastCheck != refilledAt {
			t.Fatalf("limiter %d = allowance %d, lastCheck %d, want 1, %d", i, limiter.allowance, limiter.lastCheck, refilledAt)
		}
	}

	clock.advance(2 * time.Nanosecond)
	before := snapshotLimiters(limiters...)
	limited, failedAt := LimitAll(limiters, []uint64{1, 4, 1})
	if !limited || failedAt != 1 {
		t.Fatalf("rejected LimitAll = %v, %d, want true, 1", limited, failedAt)
	}
	assertLimiterStates(t, limiters, before)
}

func TestLimitAllConcurrentDuplicateLimitersConserveAllowance(t *testing.T) {
	base := time.Unix(0, 100)
	clock := newControllableClock(base)
	global := testBucket(clock, 20)
	tenant := testBucket(clock, 20)
	user := testBucket(clock, 20)
	limiters := []*RateLimiter{global, tenant, user}
	clock.resetCalls()

	const goroutines = 20
	start := make(chan struct{})
	var done sync.WaitGroup
	done.Add(goroutines)
	rejectedResults := make([]bool, goroutines)
	failedIndexes := make([]int, goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer done.Done()
			<-start

			if i%2 == 0 {
				rejectedResults[i], failedIndexes[i] = LimitAll(
					[]*RateLimiter{limiters[0], limiters[1], limiters[0], limiters[2]},
					[]uint64{1, 1, 1, 1},
				)
			} else {
				rejectedResults[i], failedIndexes[i] = LimitAll(
					[]*RateLimiter{limiters[2], limiters[0], limiters[1], limiters[0]},
					[]uint64{1, 1, 1, 1},
				)
			}
		}()
	}

	close(start)
	finished := make(chan struct{})
	go func() {
		done.Wait()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent LimitAll calls with duplicate limiters deadlocked")
	}

	allowed := 0
	for i, rejected := range rejectedResults {
		if !rejected {
			allowed++
		}
		wantFailedAt := 0
		if i%2 != 0 {
			wantFailedAt = 1
		}
		if rejected && failedIndexes[i] != wantFailedAt {
			t.Fatalf("request %d failed at %d, want %d", i, failedIndexes[i], wantFailedAt)
		}
	}

	if allowed != 10 {
		t.Fatalf("allowed requests = %d, want 10", allowed)
	}

	if global.allowance != 0 || tenant.allowance != 10 || user.allowance != 10 {
		t.Fatalf(
			"final allowances = %d, %d, %d, want 0, 10, 10",
			global.allowance,
			tenant.allowance,
			user.allowance,
		)
	}
	if calls := clock.callCount(); calls != goroutines {
		t.Fatalf("clock calls = %d, want %d", calls, goroutines)
	}
}
