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
*/
package ratelimit

import (
	"math"
	"math/bits"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	nextRateLimiterMu sync.Mutex
	nextRateLimiterID uint64
)

// RateLimiter instances are thread-safe.
type RateLimiter struct {
	id                         uint64
	mu                         sync.Mutex
	rate, allowance, max, unit uint64
	lastCheck                  int64
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
		id:        allocateRateLimiterID(),
		rate:      uint64(rate),                             // store the rate
		allowance: saturatingMultiply64(uint64(rate), nano), // set our allowance to max in the beginning
		max:       saturatingMultiply64(uint64(rate), nano), // remember our maximum allowance
		unit:      nano,                                     // remember our unit size

		lastCheck: nowNano(),
	}
}

// UpdateRate allows to update the allowed rate
func (rl *RateLimiter) UpdateRate(rate int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.rate = uint64(rate)
	rl.max = saturatingMultiply64(uint64(rate), rl.unit)
}

// Limit returns true if rate was exceeded
func (rl *RateLimiter) Limit() bool {
	now := nowNano()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Calculate the number of ns that have passed since our last call
	passed := now - rl.lastCheck
	rl.lastCheck = now

	// Add them to our allowance
	rl.allowance += uint64(passed) * uint64(rl.rate)
	current := rl.allowance

	// Ensure our allowance is not over maximum
	if current > rl.max {
		rl.allowance += ^((current - rl.max) - 1)
		current = rl.max
	}

	// If our allowance is less than one unit, rate-limit!
	if current < rl.unit {
		return true
	}

	// Not limited, subtract a unit
	rl.allowance += ^(rl.unit - 1)
	return false
}

// Undo reverts the last Limit() call, returning consumed allowance
func (rl *RateLimiter) Undo() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.allowance += rl.unit

	// Ensure our allowance is not over maximum
	if current := rl.allowance; current > rl.max {
		rl.allowance += ^((current - rl.max) - 1)
	}
}

func nowNano() int64 {
	return time.Now().UnixNano()
}

// LimitMany applies weighted costs to several buckets atomically. It returns
// true when at least one bucket cannot cover its cost. The second result is the
// first original limiter index belonging to the failed bucket, or -1 when
// every bucket admitted the request.
//
// The same limiter may appear more than once; its costs are merged. Costs are
// measured in ordinary Limit() requests: a cost of one behaves like one call.
// When LimitMany returns true, no bucket state is changed.
func LimitMany(costs []uint64, limiters ...*RateLimiter) (bool, int) {
	return limitMany(limiters, costs, nowNano)
}

type rateLimiterCharge struct {
	limiter    *RateLimiter
	cost       uint64
	required   uint64
	available  uint64
	firstIndex int
}

func limitMany(limiters []*RateLimiter, costs []uint64, now func() int64) (bool, int) {
	if len(limiters) != len(costs) {
		panic("ratelimit: limiters and costs must have the same length")
	}
	if len(limiters) == 0 {
		return false, -1
	}
	if now == nil {
		panic("ratelimit: nil clock")
	}

	groupIndex := make(map[*RateLimiter]int, len(limiters))
	groups := make([]rateLimiterCharge, 0, len(limiters))

	for i, limiter := range limiters {
		if limiter == nil {
			panic("ratelimit: nil limiter")
		}

		cost := costs[i]
		if index, ok := groupIndex[limiter]; ok {
			if cost == 0 {
				continue
			}

			merged, carry := bits.Add64(groups[index].cost, cost, 0)
			if carry != 0 {
				return true, i
			}
			groups[index].cost = merged
			continue
		}

		groupIndex[limiter] = len(groups)
		groups = append(groups, rateLimiterCharge{
			limiter:    limiter,
			cost:       cost,
			firstIndex: i,
		})
	}

	positive := make([]rateLimiterCharge, 0, len(groups))
	for _, group := range groups {
		if group.cost > 0 {
			positive = append(positive, group)
		}
	}
	groups = positive
	if len(groups) == 0 {
		return false, -1
	}

	for i := range groups {
		groups[i].limiter.ensureLockOrderID()

		hi, required := bits.Mul64(groups[i].cost, groups[i].limiter.unit)
		if hi != 0 {
			return true, groups[i].firstIndex
		}
		groups[i].required = required
	}

	sort.Slice(groups, func(i, j int) bool {
		return groups[i].limiter.id < groups[j].limiter.id
	})

	for i := range groups {
		groups[i].limiter.mu.Lock()
	}
	defer func() {
		for i := len(groups) - 1; i >= 0; i-- {
			groups[i].limiter.mu.Unlock()
		}
	}()

	checkTime := now()
	failedIndex := -1
	for i := range groups {
		groups[i].available = availableAllowance(groups[i].limiter, checkTime)
		if groups[i].available < groups[i].required {
			if failedIndex == -1 || groups[i].firstIndex < failedIndex {
				failedIndex = groups[i].firstIndex
			}
		}
	}
	if failedIndex != -1 {
		return true, failedIndex
	}

	for _, group := range groups {
		group.limiter.allowance = group.available - group.required
		if checkTime > group.limiter.lastCheck {
			group.limiter.lastCheck = checkTime
		}
	}

	return false, -1
}

func (rl *RateLimiter) ensureLockOrderID() {
	for {
		if id := atomic.LoadUint64(&rl.id); id != 0 {
			return
		}

		candidate := allocateRateLimiterID()
		if atomic.CompareAndSwapUint64(&rl.id, 0, candidate) {
			return
		}
	}
}

func allocateRateLimiterID() uint64 {
	nextRateLimiterMu.Lock()
	defer nextRateLimiterMu.Unlock()

	nextRateLimiterID++
	if nextRateLimiterID == 0 {
		nextRateLimiterID = 1
	}
	return nextRateLimiterID
}

func availableAllowance(rl *RateLimiter, now int64) uint64 {
	if now <= rl.lastCheck {
		return rl.allowance
	}

	added := saturatingMultiply64(rl.rate, uint64(now-rl.lastCheck))
	current, carry := bits.Add64(rl.allowance, added, 0)
	if carry != 0 {
		current = math.MaxUint64
	}
	if current > rl.max {
		current = rl.max
	}
	return current
}

func saturatingMultiply64(a, b uint64) uint64 {
	hi, product := bits.Mul64(a, b)
	if hi != 0 {
		return math.MaxUint64
	}
	return product
}
