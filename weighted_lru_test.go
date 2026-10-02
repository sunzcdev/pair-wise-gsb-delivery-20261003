// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package lru

import (
	"math"
	"reflect"
	"sync"
	"testing"
)

func TestNewWeightedInvalid(t *testing.T) {
	if _, err := NewWeighted[int, int](-1); err == nil {
		t.Fatal("expected error for negative budget")
	}
}

func TestWeightedCacheBasic(t *testing.T) {
	l, err := NewWeighted[int, int](10)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if added, n := l.Add(1, 1, 6); !added || n != 0 {
		t.Fatalf("add: added=%v n=%d", added, n)
	}
	if added, n := l.Add(2, 2, 5); !added || n != 1 {
		t.Fatalf("add with eviction: added=%v n=%d", added, n)
	}
	if l.Weight() != 5 || l.Len() != 1 || l.Cap() != 10 {
		t.Fatalf("bad state: weight=%d len=%d cap=%d", l.Weight(), l.Len(), l.Cap())
	}
	if _, ok := l.Peek(1); ok {
		t.Fatal("key1 should be evicted")
	}
	if v, ok := l.Get(2); !ok || v != 2 {
		t.Fatal("key2 should be present")
	}

	if !l.Remove(2) {
		t.Fatal("key2 should be present")
	}
	if l.Weight() != 0 {
		t.Fatalf("weight after remove: %d", l.Weight())
	}
}

func TestWeightedCacheRejectedUpdateNoPhantomEviction(t *testing.T) {
	type event struct{ k, v int }
	var events []event

	l, err := NewWeightedWithEvict(10, func(k, v int) {
		events = append(events, event{k, v})
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	l.Add(1, 1, 4)
	l.Add(2, 2, 3)

	// Oversized new entry: rejected, nothing evicted, no callback.
	if added, n := l.Add(3, 3, 11); added || n != 0 {
		t.Fatalf("oversized add: added=%v n=%d", added, n)
	}
	// Oversized replacement: old value, weight and order kept, no callback.
	if added, n := l.Add(2, 22, 11); added || n != 0 {
		t.Fatalf("oversized update: added=%v n=%d", added, n)
	}
	// Negative weight: rejected, no callback.
	if added, n := l.Add(4, 4, -1); added || n != 0 {
		t.Fatalf("negative add: added=%v n=%d", added, n)
	}
	if len(events) != 0 {
		t.Fatalf("phantom eviction notifications: %v", events)
	}
	if v, _ := l.Peek(2); v != 2 {
		t.Fatalf("old value must survive rejected update: %d", v)
	}
	if w, _ := l.PeekWeight(2); w != 3 {
		t.Fatalf("old weight must survive rejected update: %d", w)
	}
	if l.Weight() != 7 || l.Len() != 2 {
		t.Fatalf("bad state: weight=%d len=%d", l.Weight(), l.Len())
	}

	// The rejected update did not refresh key2: the next overflow evicts it
	// as the oldest entry and exactly one notification fires.
	if added, n := l.Add(3, 3, 7); !added || n != 1 {
		t.Fatalf("add: added=%v n=%d", added, n)
	}
	if !reflect.DeepEqual(events, []event{{1, 1}}) {
		t.Fatalf("callback events: %v", events)
	}
}

func TestWeightedCacheEvictionOrderOutsideLock(t *testing.T) {
	type event struct{ k, v int }
	var mu sync.Mutex
	var events []event
	var lenDuringCallback []int

	var l *WeightedCache[int, int]
	l, _ = NewWeightedWithEvict(10, func(k, v int) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event{k, v})
		// The lock is released during the callback: Len() must not deadlock.
		lenDuringCallback = append(lenDuringCallback, l.Len())
	})

	l.Add(1, 1, 3)
	l.Add(2, 2, 3)
	l.Add(3, 3, 3)

	// One add pushes out the two oldest entries, in eviction order.
	if added, n := l.Add(4, 4, 6); !added || n != 2 {
		t.Fatalf("add: added=%v n=%d", added, n)
	}
	mu.Lock()
	if !reflect.DeepEqual(events, []event{{1, 1}, {2, 2}}) {
		mu.Unlock()
		t.Fatalf("callback order: %v", events)
	}
	// All removals for the batch happen under the lock; callbacks only run
	// after the batch snapshot was taken and the lock released.
	if !reflect.DeepEqual(lenDuringCallback, []int{2, 2}) {
		mu.Unlock()
		t.Fatalf("len during callbacks: %v", lenDuringCallback)
	}
	mu.Unlock()

	// Purge reports oldest first, outside the lock.
	events = nil
	lenDuringCallback = nil
	l.Purge()
	if !reflect.DeepEqual(events, []event{{3, 3}, {4, 4}}) {
		t.Fatalf("purge callback order: %v", events)
	}

	// Resize reports oldest first, outside the lock.
	events = nil
	l.Add(1, 1, 3)
	l.Add(2, 2, 3)
	l.Add(3, 3, 3)
	if n := l.Resize(3); n != 2 {
		t.Fatalf("resize: n=%d", n)
	}
	if !reflect.DeepEqual(events, []event{{1, 1}, {2, 2}}) {
		t.Fatalf("resize callback order: %v", events)
	}
}

func TestWeightedCacheCallbackReentrancy(t *testing.T) {
	type event struct{ k, v int }
	var mu sync.Mutex
	var events []event

	var l *WeightedCache[int, int]
	l, _ = NewWeightedWithEvict(10, func(k, v int) {
		mu.Lock()
		events = append(events, event{k, v})
		mu.Unlock()
		// Re-enter the cache from the callback: this must not deadlock and
		// must not mix its own evictions into the outer notification batch.
		l.Add(100+k, 1000+v, 1)
	})

	// Fill the cache so evicting entries while re-adding from callbacks keeps
	// evicting again.
	for i := 1; i <= 4; i++ {
		l.Add(i, i, 3) // 3,6,9 then evict on the 4th
	}

	// Trigger two chained evictions; callbacks nest but every evicted pair is
	// reported exactly once.
	mu.Lock()
	got := append([]event(nil), events...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("expected eviction callbacks")
	}
	seen := make(map[event]int)
	for _, e := range got {
		seen[e]++
	}
	for e, count := range seen {
		if count != 1 {
			t.Fatalf("event %v reported %d times", e, count)
		}
	}
	if l.Weight() > 10 {
		t.Fatalf("budget violated: %d", l.Weight())
	}
}

func TestWeightedCacheContainsOrAdd(t *testing.T) {
	l, _ := NewWeighted[int, int](10)

	l.Add(1, 1, 4)

	if found, added, n := l.ContainsOrAdd(1, 2, 6); !found || added || n != 0 {
		t.Fatalf("existing key: found=%v added=%v n=%d", found, added, n)
	}
	if v, _ := l.Peek(1); v != 1 {
		t.Fatal("ContainsOrAdd must not overwrite an existing key")
	}

	if found, added, n := l.ContainsOrAdd(2, 2, 7); found || !added || n != 1 {
		t.Fatalf("new key: found=%v added=%v n=%d", found, added, n)
	}
	if _, ok := l.Peek(1); ok {
		t.Fatal("key1 should be evicted")
	}

	if found, added, _ := l.ContainsOrAdd(3, 3, 11); found || added {
		t.Fatal("oversized add must be rejected")
	}
}

func TestWeightedCachePeekOrAdd(t *testing.T) {
	l, _ := NewWeighted[int, int](10)

	l.Add(1, 1, 4)

	prev, found, added, n := l.PeekOrAdd(1, 2, 6)
	if !found || added || n != 0 || prev != 1 {
		t.Fatalf("existing key: prev=%d found=%v added=%v n=%d", prev, found, added, n)
	}

	prev, found, added, n = l.PeekOrAdd(2, 2, 7)
	if found || !added || n != 1 || prev != 0 {
		t.Fatalf("new key: prev=%d found=%v added=%v n=%d", prev, found, added, n)
	}

	_, found, added, _ = l.PeekOrAdd(3, 3, 11)
	if found || added {
		t.Fatal("oversized add must be rejected")
	}
}

func TestWeightedCacheZeroWeight(t *testing.T) {
	l, _ := NewWeighted[int, int](0)

	for i := 0; i < 100; i++ {
		if added, n := l.Add(i, i, 0); !added || n != 0 {
			t.Fatalf("zero-weight add %d: added=%v n=%d", i, added, n)
		}
	}
	if l.Len() != 100 || l.Weight() != 0 {
		t.Fatalf("bad state: len=%d weight=%d", l.Len(), l.Weight())
	}

	// A positive entry does not fit a zero budget and is rejected without
	// evicting zero-weight entries.
	if added, _ := l.Add(1000, 1000, 1); added {
		t.Fatal("positive weight must not fit a zero budget")
	}
	if l.Len() != 100 {
		t.Fatalf("zero-weight entries must be kept: len=%d", l.Len())
	}

	// Growing the budget lets a positive entry in.
	if n := l.Resize(1); n != 0 {
		t.Fatalf("resize: n=%d", n)
	}
	if added, n := l.Add(1000, 1000, 1); !added || n != 0 {
		t.Fatalf("positive add after resize: added=%v n=%d", added, n)
	}
}

func TestWeightedCacheOverflowAccounting(t *testing.T) {
	l, _ := NewWeighted[int, int](math.MaxInt64)

	half := int64(math.MaxInt64 / 2)
	l.Add(1, 1, half)
	l.Add(2, 2, half)

	if added, n := l.Add(3, 3, 2); !added || n != 1 {
		t.Fatalf("overflow add: added=%v n=%d", added, n)
	}
	if l.Weight() != half+2 {
		t.Fatalf("bad weight: %d", l.Weight())
	}
}

func TestWeightedCacheConcurrent(t *testing.T) {
	l, _ := NewWeightedWithEvict(64, func(k, v int) {})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				key := (offset*500 + i) % 200
				l.Add(key, i, int64(1+i%5))
				_, _ = l.Get(key)
				_ = l.Contains(key)
				_, _ = l.Peek(key)
				if i%37 == 0 {
					l.Remove(key)
				}
				if i%101 == 0 {
					l.Resize(int64(32 + i%64))
				}
			}
		}(g)
	}
	wg.Wait()

	if l.Weight() > l.Cap() {
		t.Fatalf("budget violated: weight=%d cap=%d", l.Weight(), l.Cap())
	}
}
