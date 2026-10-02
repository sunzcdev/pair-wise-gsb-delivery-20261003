// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package simplelru

import (
	"math"
	"reflect"
	"testing"
)

func TestNewWeightedLRUInvalid(t *testing.T) {
	if _, err := NewWeightedLRU[int, int](-1, nil); err == nil {
		t.Fatal("expected error for negative budget")
	}
}

func TestWeightedLRUBasicEviction(t *testing.T) {
	var evicted []int
	l, err := NewWeightedLRU(10, func(k, v int) {
		evicted = append(evicted, k)
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	added, n := l.Add(1, 1, 4)
	if !added || n != 0 {
		t.Fatalf("add(1): added=%v n=%d", added, n)
	}
	added, n = l.Add(2, 2, 3)
	if !added || n != 0 {
		t.Fatalf("add(2): added=%v n=%d", added, n)
	}
	added, n = l.Add(3, 3, 5)
	if !added || n != 1 {
		t.Fatalf("add(3): added=%v n=%d", added, n)
	}
	if l.Weight() != 8 || l.Len() != 2 || l.Cap() != 10 {
		t.Fatalf("bad state: weight=%d len=%d cap=%d", l.Weight(), l.Len(), l.Cap())
	}
	if !reflect.DeepEqual(evicted, []int{1}) {
		t.Fatalf("eviction order: %v", evicted)
	}
	if _, ok := l.Get(1); ok {
		t.Fatal("key 1 should have been evicted")
	}
	if v, ok := l.Get(3); !ok || v != 3 {
		t.Fatal("key 3 should be present")
	}
	if keys := l.Keys(); !reflect.DeepEqual(keys, []int{2, 3}) {
		t.Fatalf("keys: %v", keys)
	}
}

func TestWeightedLRUOversizedSingle(t *testing.T) {
	l, _ := NewWeightedLRU[int, int](10, nil)

	if added, n := l.Add(1, 1, 4); !added || n != 0 {
		t.Fatalf("add(1): added=%v n=%d", added, n)
	}
	if added, n := l.Add(2, 2, 11); added || n != 0 {
		t.Fatalf("oversized add: added=%v n=%d", added, n)
	}
	if l.Len() != 1 || l.Weight() != 4 {
		t.Fatalf("oversized add mutated cache: len=%d weight=%d", l.Len(), l.Weight())
	}

	// An entry with exactly the budget size fits.
	if added, n := l.Add(2, 2, 10); !added || n != 1 {
		t.Fatalf("exact-budget add: added=%v n=%d", added, n)
	}
	if l.Len() != 1 || l.Weight() != 10 {
		t.Fatalf("bad state: len=%d weight=%d", l.Len(), l.Weight())
	}

	// Negative weights are rejected.
	if added, _ := l.Add(3, 3, -1); added {
		t.Fatal("negative weight must be rejected")
	}
}

func TestWeightedLRUReplaceShrinkAndOrder(t *testing.T) {
	l, _ := NewWeightedLRU[int, int](10, nil)

	l.Add(1, 1, 4)
	l.Add(2, 2, 3)

	// Shrink an existing entry: it stays MRU and nothing is evicted.
	if added, n := l.Add(2, 20, 1); !added || n != 0 {
		t.Fatalf("shrink: added=%v n=%d", added, n)
	}
	if v, ok := l.Peek(2); !ok || v != 20 {
		t.Fatal("replacement value not stored")
	}
	if w, ok := l.PeekWeight(2); !ok || w != 1 {
		t.Fatalf("replacement weight: %d %v", w, ok)
	}
	if l.Weight() != 5 {
		t.Fatalf("bad weight: %d", l.Weight())
	}

	// Grow the entry beyond the budget: rejected, old value/weight/order kept.
	if added, n := l.Add(2, 200, 11); added || n != 0 {
		t.Fatalf("oversized update: added=%v n=%d", added, n)
	}
	if v, _ := l.Peek(2); v != 20 {
		t.Fatalf("rejected update changed value: %d", v)
	}
	if w, _ := l.PeekWeight(2); w != 1 {
		t.Fatalf("rejected update changed weight: %d", w)
	}

	// Order before the rejected update was key1(oldest), key2(newest) and
	// must be unchanged. Adding weight 6 evicts the oldest entry (key1),
	// never the updated key.
	if added, n := l.Add(3, 3, 6); !added || n != 1 {
		t.Fatalf("add after rejected update: added=%v n=%d", added, n)
	}
	if _, ok := l.Peek(1); ok {
		t.Fatal("oldest entry should have been evicted")
	}
	if _, ok := l.Peek(2); !ok {
		t.Fatal("updated key must survive the eviction it triggered")
	}
	if l.Weight() != 7 || l.Len() != 2 {
		t.Fatalf("bad state: weight=%d len=%d", l.Weight(), l.Len())
	}
}

func TestWeightedLRUReplaceEvictsOther(t *testing.T) {
	// Replacing an existing key first frees its old weight, then evicts the
	// oldest other entries while preserving the replaced key.
	var evicted []int
	l, _ := NewWeightedLRU(10, func(k, v int) {
		evicted = append(evicted, k)
	})

	l.Add(1, 1, 2)
	l.Add(2, 2, 2)
	l.Add(3, 3, 2)

	// Touch key1 so key2 is oldest; then grow key3 from 2 to 8.
	l.Get(1)
	if added, n := l.Add(3, 30, 8); !added || n != 1 {
		t.Fatalf("grow: added=%v n=%d", added, n)
	}
	if !reflect.DeepEqual(evicted, []int{2}) {
		t.Fatalf("eviction order: %v", evicted)
	}
	if _, ok := l.Peek(2); ok {
		t.Fatal("key2 should be evicted")
	}
	if v, ok := l.Peek(3); !ok || v != 30 {
		t.Fatal("key3 should hold the new value")
	}
	if l.Weight() != 10 || l.Len() != 2 {
		t.Fatalf("bad state: weight=%d len=%d", l.Weight(), l.Len())
	}
}

func TestWeightedLRUZeroWeight(t *testing.T) {
	var evicted []int
	l, _ := NewWeightedLRU(5, func(k, v int) {
		evicted = append(evicted, k)
	})

	l.Add(1, 1, 0)
	l.Add(2, 2, 5)

	// Adding another full-weight entry overflows; the oldest entry is the
	// zero-weight one, so it is evicted in normal LRU order.
	if added, n := l.Add(3, 3, 5); !added || n != 2 {
		t.Fatalf("add: added=%v n=%d", added, n)
	}
	if !reflect.DeepEqual(evicted, []int{1, 2}) {
		t.Fatalf("eviction order: %v", evicted)
	}
	if _, ok := l.Peek(3); !ok {
		t.Fatal("key3 should remain")
	}

	// Resizing to zero keeps the remaining zero-weight entries (none here),
	// and further zero-weight adds never evict.
	l.Resize(0)
	if added, n := l.Add(4, 4, 0); !added || n != 0 {
		t.Fatalf("zero-weight add with zero budget: added=%v n=%d", added, n)
	}
	if l.Weight() != 0 || l.Len() != 1 {
		t.Fatalf("bad state: weight=%d len=%d", l.Weight(), l.Len())
	}
}

func TestWeightedLRUResize(t *testing.T) {
	var evicted []int
	l, _ := NewWeightedLRU(10, func(k, v int) {
		evicted = append(evicted, k)
	})

	l.Add(1, 1, 2)
	l.Add(2, 2, 3)
	l.Add(3, 3, 5)

	if n := l.Resize(6); n != 2 {
		t.Fatalf("shrink evictions: %d", n)
	}
	if !reflect.DeepEqual(evicted, []int{1, 2}) {
		t.Fatalf("eviction order: %v", evicted)
	}
	if l.Weight() != 5 || l.Cap() != 6 {
		t.Fatalf("bad state after shrink: weight=%d cap=%d", l.Weight(), l.Cap())
	}

	// Growing the budget does not restore anything.
	if n := l.Resize(100); n != 0 || l.Len() != 1 {
		t.Fatalf("grow should not evict: n=%d len=%d", n, l.Len())
	}

	// Negative budgets are clamped to zero.
	l.Add(4, 4, 0)
	if n := l.Resize(-5); n != 1 {
		t.Fatalf("negative resize: n=%d", n)
	}
	if l.Weight() != 0 || l.Len() != 1 || l.Cap() != 0 {
		t.Fatalf("bad state: weight=%d len=%d cap=%d", l.Weight(), l.Len(), l.Cap())
	}

	// Shrinking below the largest single entry evicts that entry too: the
	// total-weight invariant always holds after Resize.
	l2, _ := NewWeightedLRU[int, int](10, nil)
	l2.Add(1, 1, 2)
	l2.Add(2, 2, 3)
	l2.Add(3, 3, 5)
	if n := l2.Resize(4); n != 3 || l2.Weight() != 0 || l2.Len() != 0 {
		t.Fatalf("shrink below largest entry: n=%d weight=%d len=%d", n, l2.Weight(), l2.Len())
	}
}

func TestWeightedLRUOverflow(t *testing.T) {
	// The running weight sum overflows int64 while evictions bring it back
	// into range: arithmetic must stay correct instead of wrapping.
	l, _ := NewWeightedLRU[int, int](math.MaxInt64, nil)

	half := int64(math.MaxInt64 / 2)
	l.Add(1, 1, half)
	l.Add(2, 2, half)
	if l.Weight() != math.MaxInt64-1 || l.Len() != 2 {
		t.Fatalf("two half entries: weight=%d len=%d", l.Weight(), l.Len())
	}

	// The intermediate sum is MaxInt64+1, which wraps to a negative int64;
	// eviction must still happen and the accounting must be correct.
	added, n := l.Add(3, 3, 2)
	if !added || n != 1 {
		t.Fatalf("overflowing add: added=%v n=%d", added, n)
	}
	if w := l.Weight(); w != half+2 {
		t.Fatalf("weight after overflow eviction: got %d want %d", w, half+2)
	}
	if _, ok := l.Peek(1); ok {
		t.Fatal("key1 should be evicted")
	}

	// A rejected oversized update must not corrupt the accounting that the
	// above eviction restored.
	// Cache holds key2(half) + key3(2); growing key2 to MaxInt64 forces
	// key3 out and leaves exactly the budget used.
	if added, n := l.Add(2, 2, math.MaxInt64); !added || n != 1 {
		t.Fatalf("budget-fitting update: added=%v n=%d", added, n)
	}
	if l.Weight() != math.MaxInt64 {
		t.Fatalf("bad weight: %d", l.Weight())
	}
	if _, ok := l.Peek(2); !ok {
		t.Fatal("key2 should remain")
	}

	// Adding anything to a cache already using its whole MaxInt64 budget
	// crosses the int64 boundary again and evicts the giant entry.
	if added, n := l.Add(4, 4, 1); !added || n != 1 {
		t.Fatalf("full-budget add: added=%v n=%d", added, n)
	}
	if l.Weight() != 1 || l.Len() != 1 {
		t.Fatalf("bad state after eviction: weight=%d len=%d", l.Weight(), l.Len())
	}
	if _, ok := l.Peek(2); ok {
		t.Fatal("key2 should have been evicted")
	}
}

func TestWeightedLRURemoveAndPurge(t *testing.T) {
	var evicted []int
	l, _ := NewWeightedLRU(10, func(k, v int) {
		evicted = append(evicted, k)
	})

	l.Add(1, 1, 4)
	l.Add(2, 2, 3)
	l.Add(3, 3, 2)

	if !l.Remove(2) {
		t.Fatal("key2 should be present")
	}
	if l.Remove(2) {
		t.Fatal("key2 should be gone")
	}
	if l.Weight() != 6 {
		t.Fatalf("weight after remove: %d", l.Weight())
	}
	if !reflect.DeepEqual(evicted, []int{2}) {
		t.Fatalf("remove callback: %v", evicted)
	}

	if k, v, ok := l.RemoveOldest(); !ok || k != 1 || v != 1 {
		t.Fatalf("removeOldest: %d %d %v", k, v, ok)
	}
	if l.Weight() != 2 {
		t.Fatalf("weight after removeOldest: %d", l.Weight())
	}

	// Purge reports remaining entries oldest-first and resets the accounting.
	l.Purge()
	if !reflect.DeepEqual(evicted, []int{2, 1, 3}) {
		t.Fatalf("purge callback order: %v", evicted)
	}
	if l.Len() != 0 || l.Weight() != 0 {
		t.Fatalf("bad state after purge: len=%d weight=%d", l.Len(), l.Weight())
	}

	if _, _, ok := l.GetOldest(); ok {
		t.Fatal("cache should be empty")
	}
	if _, _, ok := l.RemoveOldest(); ok {
		t.Fatal("RemoveOldest on empty cache")
	}
}

func TestWeightedLRURefreshAvoidsEviction(t *testing.T) {
	l, _ := NewWeightedLRU[int, int](10, nil)

	l.Add(1, 1, 5)
	l.Add(2, 2, 5)
	l.Get(1) // key2 is now oldest

	// A weight-5 entry needs room; key1 is MRU so key2 must go.
	if added, n := l.Add(3, 3, 5); !added || n != 1 {
		t.Fatalf("add: added=%v n=%d", added, n)
	}
	if keys := l.Keys(); !reflect.DeepEqual(keys, []int{1, 3}) {
		t.Fatalf("keys oldest->newest: %v", keys)
	}
}
