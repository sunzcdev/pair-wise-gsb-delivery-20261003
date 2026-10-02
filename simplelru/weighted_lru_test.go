// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package simplelru

import (
	"math"
	"reflect"
	"testing"
)

func TestNewWeightedLRUInvalid(t *testing.T) {
	if _, err := NewWeightedLRU[int, int](0, nil); err == nil {
		t.Fatalf("expected error for zero max weight")
	}
	if _, err := NewWeightedLRU[int, int](-1, nil); err == nil {
		t.Fatalf("expected error for negative max weight")
	}
}

func TestWeightedLRUBasic(t *testing.T) {
	l, err := NewWeightedLRU[int, int](10, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if l.Add(1, 1, 4) {
		t.Fatalf("first add should not evict")
	}
	if l.Add(2, 2, 6) {
		t.Fatalf("exact fit should not evict")
	}
	if l.Weight() != 10 {
		t.Fatalf("bad weight: %d", l.Weight())
	}

	// Adding another positive-weight entry evicts the oldest.
	if !l.Add(3, 3, 1) {
		t.Fatalf("expected eviction")
	}
	if _, ok := l.Peek(1); ok {
		t.Fatalf("key 1 should have been evicted")
	}
	if _, ok := l.Peek(2); !ok {
		t.Fatalf("key 2 should remain")
	}
	if l.Weight() != 7 {
		t.Fatalf("bad weight after eviction: %d", l.Weight())
	}

	// Recently-used order drives eviction.
	if _, ok := l.Get(2); !ok {
		t.Fatalf("key 2 should be present")
	}
	l.Add(4, 4, 4)
	if _, ok := l.Peek(3); ok {
		t.Fatalf("key 3 should have been evicted as least recently used")
	}
	if _, ok := l.Peek(2); !ok {
		t.Fatalf("key 2 should remain after recent use")
	}
}

func TestWeightedLRUZeroWeight(t *testing.T) {
	l, _ := NewWeightedLRU[int, int](2, nil)

	if l.Add(1, 1, 0) {
		t.Fatalf("zero-weight add should not evict")
	}
	if l.Add(2, 2, 0) {
		t.Fatalf("zero-weight add should not evict")
	}
	if l.Add(3, 3, 0) {
		t.Fatalf("zero-weight add should not evict")
	}
	if l.Len() != 3 || l.Weight() != 0 {
		t.Fatalf("bad state: len=%d weight=%d", l.Len(), l.Weight())
	}

	// A positive-weight entry pushes the zero-weight entries out, oldest first.
	if l.Add(4, 4, 1) {
		t.Fatalf("expected an eviction")
	}
	if l.Len() != 4 || l.Weight() != 1 {
		t.Fatalf("evicting zero-weight entries cannot free room, so they stay, len=%d weight=%d", l.Len(), l.Weight())
	}
	if _, ok := l.Peek(1); !ok {
		t.Fatalf("evicting a zero-weight entry is pointless, the oldest must stay")
	}

	// The zero-weight entries age out only as side effects of evictions
	// driven by positive-weight pressure.
	l.Add(5, 5, 1)
	l.Add(6, 6, 2)
	if l.Weight() != 2 {
		t.Fatalf("unexpected weight: %d", l.Weight())
	}
	for _, k := range l.Keys() {
		if k == 4 || k == 5 || k == 6 {
			continue
		}
		if _, ok := l.Peek(k); !ok {
			t.Fatalf("remaining zero-weight key %d missing", k)
		}
	}
}

func TestWeightedLRUTooLargeRejected(t *testing.T) {
	var evictedKeys []int
	l, _ := NewWeightedLRU[int, int](10, func(k, _ int) {
		evictedKeys = append(evictedKeys, k)
	})

	l.Add(1, 1, 4)
	l.Add(2, 2, 3)

	if l.Add(3, 3, 11) {
		t.Fatalf("oversized add should report no eviction")
	}
	if _, ok := l.Peek(3); ok {
		t.Fatalf("oversized entry should not be admitted")
	}
	if l.Weight() != 7 {
		t.Fatalf("weight should be unchanged, got %d", l.Weight())
	}
	if !reflect.DeepEqual(l.Keys(), []int{1, 2}) {
		t.Fatalf("cache contents should be unchanged, got %v", l.Keys())
	}
	if len(evictedKeys) != 0 {
		t.Fatalf("no eviction callbacks should fire on rejected add, got %v", evictedKeys)
	}
}

func TestWeightedLRUReplaceGrowsAndShrinks(t *testing.T) {
	var evictedKeys []int
	l, _ := NewWeightedLRU[int, int](10, func(k, _ int) {
		evictedKeys = append(evictedKeys, k)
	})

	l.Add(1, 1, 6)
	l.Add(2, 2, 4)

	// Growing key 2 to 7 requires evicting key 1.
	if !l.Add(2, 22, 7) {
		t.Fatalf("expected eviction when growing replacement")
	}
	if v, ok := l.Peek(2); !ok || v != 22 {
		t.Fatalf("replaced value missing: %v, %v", v, ok)
	}
	if _, ok := l.Peek(1); ok {
		t.Fatalf("key 1 should be evicted")
	}
	if l.Weight() != 7 {
		t.Fatalf("bad weight: %d", l.Weight())
	}
	if !reflect.DeepEqual(evictedKeys, []int{1}) {
		t.Fatalf("bad eviction order: %v", evictedKeys)
	}

	// Shrinking the weight frees budget.
	l.Add(2, 222, 2)
	if l.Weight() != 2 {
		t.Fatalf("bad weight after shrink: %d", l.Weight())
	}
	l.Add(3, 3, 8)
	if l.Weight() != 10 {
		t.Fatalf("expected exact fit, got weight %d", l.Weight())
	}
}

func TestWeightedLRUReplaceOversizedKeepsOld(t *testing.T) {
	var evictedKeys []int
	l, _ := NewWeightedLRU[int, int](10, func(k, _ int) {
		evictedKeys = append(evictedKeys, k)
	})

	l.Add(1, 1, 6)
	l.Add(2, 2, 4)
	// Make key 2 the most recently used so key 1 is evicted first.
	if _, ok := l.Get(2); !ok {
		t.Fatalf("key 2 missing")
	}

	if l.Add(2, 99, 11) {
		t.Fatalf("replacement heavier than the budget must be rejected")
	}
	if v, ok := l.Peek(2); !ok || v != 2 {
		t.Fatalf("old value must be retained, got %v, %v", v, ok)
	}
	if l.Weight() != 10 {
		t.Fatalf("weight must be unchanged, got %d", l.Weight())
	}
	if !reflect.DeepEqual(l.Keys(), []int{1, 2}) {
		t.Fatalf("order and contents must be retained, got %v", l.Keys())
	}
	if len(evictedKeys) != 0 {
		t.Fatalf("rejected replacement must not evict, got %v", evictedKeys)
	}
}

func TestWeightedLRUReplaceOldestGrowEvictsNewer(t *testing.T) {
	var evictedKeys []int
	l, _ := NewWeightedLRU[int, int](10, func(k, _ int) {
		evictedKeys = append(evictedKeys, k)
	})

	l.Add(1, 1, 6)
	l.Add(2, 2, 4)

	// Replacing the oldest entry with a larger weight that fits only after
	// removing the other entry: the admitted entry is treated as newest.
	if !l.Add(1, 11, 7) {
		t.Fatalf("growing replacement should evict the other entry")
	}
	if !reflect.DeepEqual(evictedKeys, []int{2}) {
		t.Fatalf("only the other entry should be evicted, got %v", evictedKeys)
	}
	if !reflect.DeepEqual(l.Keys(), []int{1}) {
		t.Fatalf("replaced key should remain and be newest, got %v", l.Keys())
	}
	if v, ok := l.Peek(1); !ok || v != 11 {
		t.Fatalf("replaced value missing: %v, %v", v, ok)
	}
	if l.Weight() != 7 {
		t.Fatalf("bad weight: %d", l.Weight())
	}
}

func TestWeightedLRUSumOverflow(t *testing.T) {
	// Capacity and weights near math.MaxInt64 exercise arithmetic that must
	// never wrap around.
	l, _ := NewWeightedLRU[int, int](math.MaxInt64-1, nil)

	if l.Add(1, 1, math.MaxInt64-2) {
		t.Fatalf("unexpected eviction")
	}
	if l.Add(2, 2, 0) {
		t.Fatalf("zero weight should fit without eviction")
	}
	if l.Weight() != math.MaxInt64-2 {
		t.Fatalf("bad weight: %d", l.Weight())
	}

	// Weight 2 only fits by evicting the big entry: the eviction math must
	// compare via subtraction, since MaxInt64-2 + 2 would equal MaxInt64
	// (which still fits here) and larger sums would wrap to negative.
	if !l.Add(3, 3, 2) {
		t.Fatalf("expected evictions while admitting weight 2")
	}
	if _, ok := l.Peek(2); !ok {
		t.Fatalf("zero-weight entry should outrank the older big entry")
	}
	if _, ok := l.Peek(1); ok {
		t.Fatalf("big entry should have been evicted")
	}
	if l.Weight() != 2 {
		t.Fatalf("bad weight after eviction: %d", l.Weight())
	}

	// Re-adding a nearly maximal weight evicts zero- and positive-weight
	// entries without any additive overflow.
	if !l.Add(1, 1, math.MaxInt64-2) {
		t.Fatalf("expected evictions while refilling to near max")
	}
	if l.Weight() != math.MaxInt64-2 {
		t.Fatalf("bad weight near max: %d", l.Weight())
	}

	// A value heavier than the budget is rejected and keeps old state.
	if l.Add(1, 99, math.MaxInt64) {
		t.Fatalf("entry heavier than the budget must be rejected")
	}
	if v, ok := l.Peek(1); !ok || v != 1 {
		t.Fatalf("old value must be retained, got %v, %v", v, ok)
	}
	if l.Weight() != math.MaxInt64-2 {
		t.Fatalf("rejected add must keep old weight, got %d", l.Weight())
	}
}

func TestWeightedLRUResize(t *testing.T) {
	var evictedKeys []int
	l, _ := NewWeightedLRU[int, int](10, func(k, _ int) {
		evictedKeys = append(evictedKeys, k)
	})

	l.Add(1, 1, 2)
	l.Add(2, 2, 3)
	l.Add(3, 3, 5)

	if n := l.Resize(0); n != 0 {
		t.Fatalf("non-positive resize must be ignored, evicted=%d", n)
	}
	if l.Cap() != 10 || l.Len() != 3 {
		t.Fatalf("state changed by ignored resize: cap=%d len=%d", l.Cap(), l.Len())
	}

	if n := l.Resize(5); n != 2 {
		t.Fatalf("expected 2 evictions on shrink, got %d", n)
	}
	if !reflect.DeepEqual(evictedKeys, []int{1, 2}) {
		t.Fatalf("bad shrink eviction order: %v", evictedKeys)
	}
	if _, ok := l.Peek(3); !ok {
		t.Fatalf("key 3 should remain")
	}
	if l.Weight() != 5 {
		t.Fatalf("bad weight after shrink: %d", l.Weight())
	}

	// Growing the budget never evicts.
	if n := l.Resize(100); n != 0 {
		t.Fatalf("growing budget should not evict, got %d", n)
	}
	if l.Cap() != 100 {
		t.Fatalf("bad cap: %d", l.Cap())
	}
}

func TestWeightedLRUPurgeOrder(t *testing.T) {
	var order []int
	l, _ := NewWeightedLRU[int, int](10, func(k, _ int) {
		order = append(order, k)
	})

	l.Add(1, 1, 1)
	l.Add(2, 2, 1)
	l.Add(3, 3, 1)
	l.Purge()

	if !reflect.DeepEqual(order, []int{1, 2, 3}) {
		t.Fatalf("purge callbacks should run oldest-first, got %v", order)
	}
	if l.Len() != 0 || l.Weight() != 0 {
		t.Fatalf("cache should be empty after purge: len=%d weight=%d", l.Len(), l.Weight())
	}
}

func TestWeightedLRURemove(t *testing.T) {
	l, _ := NewWeightedLRU[int, int](10, nil)
	l.Add(1, 1, 4)
	l.Add(2, 2, 6)

	if !l.Remove(1) {
		t.Fatalf("remove should report present")
	}
	if l.Remove(1) {
		t.Fatalf("second remove should report absent")
	}
	if l.Weight() != 6 {
		t.Fatalf("bad weight after remove: %d", l.Weight())
	}

	k, v, ok := l.RemoveOldest()
	if !ok || k != 2 || v != 2 {
		t.Fatalf("bad oldest removal: %v, %v, %v", k, v, ok)
	}
	if l.Weight() != 0 || l.Len() != 0 {
		t.Fatalf("cache should be empty: weight=%d len=%d", l.Weight(), l.Len())
	}
}

func TestWeightedLRUNegativeWeightPanics(t *testing.T) {
	l, _ := NewWeightedLRU[int, int](10, nil)
	defer func() {
		if recover() == nil {
			t.Fatalf("negative weight should panic")
		}
	}()
	l.Add(1, 1, -1)
}
