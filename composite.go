package ratelimit

import (
	"reflect"
	"sort"
)

// LimitAll checks all rate limiters as one atomic budget admission.
//
// It returns limited=true and the parameter index responsible for the rejection
// when the request cannot be admitted. A rejected request does not modify any
// rate limiter. When all buckets have enough allowance, every distinct bucket
// is refilled and charged at the same logical time. On success it returns
// false and -1.
func LimitAll(limiters []*RateLimiter, costs []uint64) (limited bool, failedAt int) {
	if len(limiters) != len(costs) {
		panic("ratelimit: limiters and costs must have the same length")
	}
	if len(limiters) == 0 {
		return false, -1
	}

	groupOrder := make([]int, 0, len(limiters))
	groupIndex := make(map[*RateLimiter]int, len(limiters))
	mergedCosts := make([]uint64, 0, len(limiters))
	firstIndex := make([]int, 0, len(limiters))

	for i, limiter := range limiters {
		if limiter == nil {
			panic("ratelimit: nil rate limiter")
		}

		group, ok := groupIndex[limiter]
		if !ok {
			groupIndex[limiter] = len(groupOrder)
			groupOrder = append(groupOrder, i)
			mergedCosts = append(mergedCosts, costs[i])
			firstIndex = append(firstIndex, i)
			continue
		}

		merged, overflow := addUint64(mergedCosts[group], costs[i])
		if overflow {
			return true, firstIndex[group]
		}
		mergedCosts[group] = merged
	}

	groups := make([]*RateLimiter, len(groupOrder))
	required := make([]uint64, len(groupOrder))
	for group, parameterIndex := range groupOrder {
		limiter := limiters[parameterIndex]
		groups[group] = limiter

		cost, overflow := multiplyUint64(mergedCosts[group], limiter.unit)
		if overflow {
			return true, firstIndex[group]
		}
		required[group] = cost
	}

	sort.Slice(groups, func(i, j int) bool {
		return limiterOrder(groups[i]) < limiterOrder(groups[j])
	})

	for _, limiter := range groups {
		limiter.mu.Lock()
		defer limiter.mu.Unlock()
	}

	now := groups[0].nano()
	current := make([]uint64, len(groups))
	for i, limiter := range groups {
		current[i] = availableAllowance(limiter, now)
		group, ok := groupIndex[limiter]
		if !ok {
			panic("ratelimit: internal rate limiter lookup failure")
		}
		if current[i] < required[group] {
			return true, firstIndex[group]
		}
	}

	for i, limiter := range groups {
		group := groupIndex[limiter]
		limiter.lastCheck = now
		limiter.allowance = current[i] - required[group]
	}
	return false, -1
}

func limiterOrder(limiter *RateLimiter) uintptr {
	return reflect.ValueOf(limiter).Pointer()
}

func (rl *RateLimiter) nano() int64 {
	if rl.now == nil {
		return timeNowNano()
	}
	return rl.now().UnixNano()
}

func availableAllowance(rl *RateLimiter, now int64) uint64 {
	current := rl.allowance
	if now > rl.lastCheck {
		elapsed := uint64(now - rl.lastCheck)
		current = saturatingAdd(current, saturatingMultiply(elapsed, rl.rate))
	}
	if current > rl.max {
		current = rl.max
	}
	return current
}

func addUint64(left, right uint64) (uint64, bool) {
	sum := left + right
	return sum, sum < left
}

func multiplyUint64(left, right uint64) (uint64, bool) {
	if left == 0 || right == 0 {
		return 0, false
	}
	product := left * right
	return product, product/left != right
}

func saturatingAdd(left, right uint64) uint64 {
	sum, overflow := addUint64(left, right)
	if overflow {
		return ^uint64(0)
	}
	return sum
}

func saturatingMultiply(left, right uint64) uint64 {
	product, overflow := multiplyUint64(left, right)
	if overflow {
		return ^uint64(0)
	}
	return product
}
