package ratelimit

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestRateLimiter(rate int, per time.Duration, allowance uint64, lastCheck int64) *RateLimiter {
	limiter := New(rate, per)
	limiter.lastCheck = lastCheck
	limiter.allowance = allowance
	return limiter
}

func TestLimitManyAllBucketsAdmittedOrNoneChange(t *testing.T) {
	unit := uint64(time.Minute)
	global := newTestRateLimiter(10, time.Minute, 2*unit, 1_000)
	tenant := newTestRateLimiter(10, time.Minute, 1*unit, 1_000)
	user := newTestRateLimiter(10, time.Minute, 2*unit, 1_000)

	clockCalls := 0
	limited, failed := limitMany(
		[]*RateLimiter{global, tenant, user},
		[]uint64{1, 2, 1},
		func() int64 {
			clockCalls++
			return 1_000
		},
	)

	if !limited {
		t.Fatal("expected merged request to be limited")
	}
	if failed != 1 {
		t.Fatalf("expected failed bucket index 1, got %d", failed)
	}
	if clockCalls != 1 {
		t.Fatalf("expected one consistent clock check, got %d", clockCalls)
	}
	if global.allowance != 2*unit || tenant.allowance != unit || user.allowance != 2*unit {
		t.Fatal("a rejected request changed bucket allowances")
	}
	if global.lastCheck != 1_000 || tenant.lastCheck != 1_000 || user.lastCheck != 1_000 {
		t.Fatal("a rejected request advanced bucket check times")
	}

	limited, failed = limitMany(
		[]*RateLimiter{global, tenant, user},
		[]uint64{1, 1, 1},
		func() int64 { return 1_000 },
	)
	if limited || failed != -1 {
		t.Fatalf("expected admitted request, got limited=%v failed=%d", limited, failed)
	}
	if global.allowance != unit || tenant.allowance != 0 || user.allowance != unit {
		t.Fatal("admitted request did not deduct every bucket once")
	}
}

func TestLimitManyMergesRepeatedBuckets(t *testing.T) {
	unit := uint64(time.Minute)
	global := newTestRateLimiter(10, time.Minute, unit, 1_000)
	tenant := newTestRateLimiter(10, time.Minute, 2*unit, 1_000)

	limited, failed := limitMany(
		[]*RateLimiter{global, tenant, global, tenant},
		[]uint64{1, 1, 1, 1},
		func() int64 { return 1_000 },
	)
	if !limited {
		t.Fatal("expected merged costs to exceed global budget")
	}
	if failed != 0 {
		t.Fatalf("expected repeated bucket's first index 0, got %d", failed)
	}
	if global.allowance != unit || tenant.allowance != 2*unit {
		t.Fatal("a rejected merged request changed bucket allowances")
	}

	global.allowance = 2 * unit
	limited, failed = limitMany(
		[]*RateLimiter{tenant, global, global, tenant},
		[]uint64{1, 1, 1, 1},
		func() int64 { return 1_000 },
	)
	if limited || failed != -1 {
		t.Fatalf("expected merged request to pass, got limited=%v failed=%d", limited, failed)
	}
	if global.allowance != 0 || tenant.allowance != 0 {
		t.Fatal("merged costs were not deducted once per unique bucket")
	}
}

func TestLimitManyDetectsOverflowWithoutMutation(t *testing.T) {
	productLimiter := newTestRateLimiter(1, 2*time.Nanosecond, 2, 1_000)
	limited, failed := limitMany(
		[]*RateLimiter{productLimiter},
		[]uint64{math.MaxUint64},
		func() int64 { return 1_000 },
	)
	if !limited || failed != 0 {
		t.Fatalf("expected product overflow at index 0, got limited=%v failed=%d", limited, failed)
	}
	if productLimiter.allowance != 2 || productLimiter.lastCheck != 1_000 {
		t.Fatal("product overflow changed bucket state")
	}

	sumLimiter := newTestRateLimiter(1, time.Nanosecond, 1, 1_000)
	limited, failed = limitMany(
		[]*RateLimiter{sumLimiter, sumLimiter},
		[]uint64{math.MaxUint64, 1},
		func() int64 { return 1_000 },
	)
	if !limited || failed != 1 {
		t.Fatalf("expected sum overflow at index 1, got limited=%v failed=%d", limited, failed)
	}
	if sumLimiter.allowance != 1 || sumLimiter.lastCheck != 1_000 {
		t.Fatal("sum overflow changed bucket state")
	}
}

func TestLimitManyUsesOneClockSnapshot(t *testing.T) {
	global := newTestRateLimiter(1, time.Nanosecond, 0, 1_000)
	tenant := newTestRateLimiter(1, time.Nanosecond, 0, 1_000)
	user := newTestRateLimiter(1, time.Nanosecond, 0, 1_000)

	clockCalls := 0
	limited, failed := limitMany(
		[]*RateLimiter{user, global, tenant},
		[]uint64{1, 1, 1},
		func() int64 {
			clockCalls++
			return 2_000
		},
	)
	if limited || failed != -1 {
		t.Fatalf("expected refill to admit request, got limited=%v failed=%d", limited, failed)
	}
	if clockCalls != 1 {
		t.Fatalf("expected one clock snapshot, got %d", clockCalls)
	}
	for _, limiter := range []*RateLimiter{global, tenant, user} {
		if limiter.allowance != 0 || limiter.lastCheck != 2_000 {
			t.Fatalf("unexpected bucket state: allowance=%d lastCheck=%d", limiter.allowance, limiter.lastCheck)
		}
	}
}

func TestLimitManyConcurrentConservationWithInterleavedOrders(t *testing.T) {
	const capacity = uint64(100)
	unit := uint64(time.Nanosecond)
	global := newTestRateLimiter(1, time.Nanosecond, capacity*unit, 1_000)
	tenant := newTestRateLimiter(1, time.Nanosecond, capacity*unit, 1_000)
	user := newTestRateLimiter(1, time.Nanosecond, capacity*unit, 1_000)

	orders := [][]int{
		{0, 1, 0, 2},
		{2, 0, 1, 0},
		{1, 0, 2, 0},
		{0, 2, 0, 1},
		{2, 1, 0, 0},
		{1, 2, 0, 0},
	}
	buckets := []*RateLimiter{global, tenant, user}
	entryCosts := []uint64{1, 1, 1}

	const goroutines = 32
	const attemptsPerGoroutine = 64
	var successes uint64
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			<-start

			order := orders[g%len(orders)]
			for attempt := 0; attempt < attemptsPerGoroutine; attempt++ {
				limiters := make([]*RateLimiter, len(order))
				costs := make([]uint64, len(order))
				for position, bucket := range order {
					limiters[position] = buckets[bucket]
					costs[position] = entryCosts[bucket]
				}

				limited, failed := limitMany(limiters, costs, func() int64 { return 1_000 })
				if failed < -1 || failed >= len(buckets) {
					t.Errorf("invalid failed bucket index %d", failed)
					return
				}
				if !limited {
					atomic.AddUint64(&successes, 1)
				}
				if attempt%3 == 0 {
					runtime.Gosched()
				}
			}
		}(g)
	}

	close(start)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("interleaved bucket orders deadlocked")
	}

	expectedSuccesses := capacity / 2
	if got := atomic.LoadUint64(&successes); got != expectedSuccesses {
		t.Fatalf("expected exactly %d admitted requests, got %d", expectedSuccesses, got)
	}
	expectedRemaining := map[string]uint64{
		"global": 0,
		"tenant": (capacity - expectedSuccesses) * unit,
		"user":   (capacity - expectedSuccesses) * unit,
	}
	remaining := map[string]*RateLimiter{
		"global": global,
		"tenant": tenant,
		"user":   user,
	}
	for name, expected := range expectedRemaining {
		if got := remaining[name].allowance; got != expected {
			t.Fatalf("%s budget not conserved: expected remaining allowance=%d got=%d", name, expected, got)
		}
	}
}
