// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package lru

import (
	"reflect"
	"sync"
	"testing"
)

func TestNewWeightedInvalid(t *testing.T) {
	if _, err := NewWeighted[int, int](0); err == nil {
		t.Fatalf("expected error for zero budget")
	}
	if _, err := NewWeightedWithEvict[int, int](-5, nil); err == nil {
		t.Fatalf("expected error for negative budget")
	}
}

func TestWeightedCacheEvictionAndWeight(t *testing.T) {
	l, err := NewWeighted[int, int](10)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	l.Add(1, 1, 6)
	l.Add(2, 2, 4)
	if l.Weight() != 10 || l.Len() != 2 || l.Cap() != 10 {
		t.Fatalf("bad state: weight=%d len=%d cap=%d", l.Weight(), l.Len(), l.Cap())
	}

	l.Add(3, 3, 5)
	if _, ok := l.Peek(1); ok {
		t.Fatalf("key 1 should be evicted")
	}
	if l.Weight() != 9 {
		t.Fatalf("bad weight: %d", l.Weight())
	}
	if !reflect.DeepEqual(l.Keys(), []int{2, 3}) {
		t.Fatalf("unexpected keys: %v", l.Keys())
	}

	l.Resize(5)
	if _, ok := l.Peek(2); ok {
		t.Fatalf("key 2 should be evicted after shrinking")
	}
	if _, ok := l.Peek(3); !ok {
		t.Fatalf("key 3 should survive shrinking")
	}
	if l.Weight() != 5 || l.Len() != 1 {
		t.Fatalf("bad state after resize: weight=%d len=%d", l.Weight(), l.Len())
	}
}

func TestWeightedCacheCallbackOrder(t *testing.T) {
	var mu sync.Mutex
	var order []int
	l, err := NewWeightedWithEvict[int, int](10, func(k, _ int) {
		mu.Lock()
		order = append(order, k)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	l.Add(1, 1, 3)
	l.Add(2, 2, 3)
	l.Add(3, 3, 3)

	// Adding weight 6 evicts the two oldest in actual eviction order.
	l.Add(4, 4, 6)
	mu.Lock()
	got := append([]int(nil), order...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("callbacks must fire oldest-first in eviction order, got %v", got)
	}

	// RemoveOldest and Resize deliver the removed entry once.
	l.RemoveOldest()
	l.Resize(1)
	mu.Lock()
	got = append([]int(nil), order...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
		t.Fatalf("unexpected callback sequence: %v", got)
	}

	// Purge reports in oldest-to-newest order.
	l.Add(7, 7, 1)
	l.Purge()
	mu.Lock()
	got = append([]int(nil), order...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []int{1, 2, 3, 4, 7}) {
		t.Fatalf("purge callback order wrong: %v", got)
	}
}

func TestWeightedCacheRejectedAddNoCallback(t *testing.T) {
	var fired []int
	l, _ := NewWeightedWithEvict[int, int](10, func(k, _ int) {
		fired = append(fired, k)
	})

	l.Add(1, 1, 6)
	l.Add(2, 2, 4)

	if l.Add(3, 3, 11) {
		t.Fatalf("oversized add must report no eviction")
	}
	// Growing the existing value beyond the budget is also rejected.
	if l.Add(2, 99, 11) {
		t.Fatalf("oversized replacement must report no eviction")
	}
	if v, ok := l.Peek(2); !ok || v != 2 {
		t.Fatalf("old value must survive rejected replacement: %v, %v", v, ok)
	}
	if len(fired) != 0 {
		t.Fatalf("rejected updates must not notify evictions, got %v", fired)
	}
	if l.Weight() != 10 || l.Len() != 2 {
		t.Fatalf("state must be unchanged: weight=%d len=%d", l.Weight(), l.Len())
	}
	if !reflect.DeepEqual(l.Keys(), []int{1, 2}) {
		t.Fatalf("order must be unchanged, got %v", l.Keys())
	}
}

func TestWeightedCacheCallbackRunsOutsideLock(t *testing.T) {
	// If the callback ran while the internal lock was held, re-entering via
	// Len/Get would deadlock. Every add of weight 2 evicts the previous one.
	observed := make(chan int, 8)
	var l *WeightedCache[int, int]
	var err error
	l, err = NewWeightedWithEvict[int, int](3, func(k, _ int) {
		observed <- l.Len()
		_, _ = l.Get(k)
	})

	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for i := 0; i < 6; i++ {
		l.Add(i, i, 2)
	}
	close(observed)
	count := 0
	for range observed {
		count++
	}
	if count != 5 {
		t.Fatalf("expected 5 eviction callbacks, got %d", count)
	}
}

func TestWeightedCacheCallbackReentrancy(t *testing.T) {
	var c *WeightedCache[int, int]
	c, _ = NewWeightedWithEvict[int, int](5, func(k, v int) {
		// Re-enter the cache from the callback: read state and add a
		// replacement entry, which may itself evict and re-enter.
		_ = c.Len()
		_ = c.Weight()
		_, _ = c.Get(100)
		c.Add(100+k, 1000+k, 1)
	})

	c.Add(1, 1, 2)
	c.Add(2, 2, 2)
	// Evicts keys 1 and 2; each callback re-enters and adds an entry.
	c.Add(3, 3, 5)

	if c.Weight() > c.Cap() {
		t.Fatalf("cache must stay within budget after re-entrant adds: weight=%d", c.Weight())
	}
	// All callback-inserted and remaining entries are observable.
	for _, k := range c.Keys() {
		if _, ok := c.Peek(k); !ok {
			t.Fatalf("key %d missing after re-entrant callbacks", k)
		}
	}
}

func TestWeightedCacheConcurrent(t *testing.T) {
	l, _ := NewWeighted[int, int](64)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				k := (base*1000 + i) % 200
				l.Add(k, k, int64(1+k%7))
				_, _ = l.Get(k)
				_ = l.Keys()
				if l.Weight() > l.Cap() {
					t.Errorf("budget exceeded: %d > %d", l.Weight(), l.Cap())
					return
				}
			}
		}(g)
	}
	wg.Wait()

	var total int64
	for _, k := range l.Keys() {
		if _, ok := l.Peek(k); !ok {
			t.Fatalf("key %d reported by Keys but missing", k)
		}
		total++
	}
	if int64(l.Len()) != total {
		t.Fatalf("inconsistent len: %d vs %d", l.Len(), total)
	}
}

func TestWeightedCacheZeroWeight(t *testing.T) {
	l, _ := NewWeighted[int, int](2)
	l.Add(1, 1, 0)
	l.Add(2, 2, 0)
	if l.Weight() != 0 || l.Len() != 2 {
		t.Fatalf("bad zero-weight state")
	}
	if l.Add(3, 3, 3) {
		t.Fatalf("entry above budget must be rejected")
	}
	if l.Len() != 2 {
		t.Fatalf("rejected add must not change cache")
	}
	l.Add(3, 3, 2)
	if l.Len() != 3 || l.Weight() != 2 {
		t.Fatalf("zero-weight entries are skipped by budget eviction: len=%d weight=%d", l.Len(), l.Weight())
	}
}

func TestWeightedCacheShrinkCallback(t *testing.T) {
	var order []int
	l, _ := NewWeightedWithEvict[int, int](10, func(k, _ int) {
		order = append(order, k)
	})
	l.Add(1, 1, 2)
	l.Add(2, 2, 3)
	l.Add(3, 3, 5)

	if n := l.Resize(5); n != 2 {
		t.Fatalf("expected 2 evictions, got %d", n)
	}
	if !reflect.DeepEqual(order, []int{1, 2}) {
		t.Fatalf("shrink callbacks must run oldest-first, got %v", order)
	}
	// A non-positive resize is a no-op.
	if n := l.Resize(-1); n != 0 || l.Cap() != 5 {
		t.Fatalf("non-positive resize must be ignored, n=%d cap=%d", n, l.Cap())
	}
}

func TestWeightedCacheValuesAndOldest(t *testing.T) {
	l, _ := NewWeighted[int, int](10)
	l.Add(1, 10, 2)
	l.Add(2, 20, 3)
	if k, v, ok := l.GetOldest(); !ok || k != 1 || v != 10 {
		t.Fatalf("bad oldest: %v, %v, %v", k, v, ok)
	}
	if !reflect.DeepEqual(l.Values(), []int{10, 20}) {
		t.Fatalf("bad values: %v", l.Values())
	}
	if !l.Contains(1) {
		t.Fatalf("expected contains")
	}
	if !l.Remove(1) {
		t.Fatalf("expected remove present")
	}
	if l.Contains(1) {
		t.Fatalf("not expected contains")
	}
}
