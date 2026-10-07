package btree

import (
	"errors"
	"runtime"
	"testing"
)

func rangeIter(n int) func(yield func(item int) bool) {
	return func(yield func(item int) bool) {
		for i := 0; i < n; i++ {
			if !yield(i) {
				return
			}
		}
	}
}

// validateTree checks ordering, leaf/internal mixing, occupancy bounds and
// item count, returning the counted number of items.
func validateTree(t *testing.T, tr *BTreeG[int], n *node[int], depth int,
) int {
	t.Helper()
	count := len(n.items)
	for i := 1; i < len(n.items); i++ {
		if !tr.less(n.items[i-1], n.items[i]) {
			t.Fatalf("items out of order at depth %d: %v", depth, n.items)
		}
	}
	if n.leaf() {
		if depth > 0 && count < tr.min {
			t.Fatalf("non-root leaf underfilled: %d < %d", count, tr.min)
		}
		if count > tr.max {
			t.Fatalf("leaf overfilled: %d > %d", count, tr.max)
		}
		return count
	}
	leafKind := (*n.children)[0].leaf()
	for _, c := range *n.children {
		if c.leaf() != leafKind {
			t.Fatal("mixed leaf/internal children")
		}
	}
	if depth > 0 && count < tr.min {
		t.Fatalf("non-root internal underfilled: %d < %d", count, tr.min)
	}
	if count > tr.max {
		t.Fatalf("internal overfilled: %d > %d", count, tr.max)
	}
	// In-order interleaving: child0 max < item0 < child1 max ...
	for i, item := range n.items {
		lc, rc := (*n.children)[i], (*n.children)[i+1]
		_, lmax := subtreeBounds(lc)
		rmin, _ := subtreeBounds(rc)
		if !tr.less(lmax, item) || !tr.less(item, rmin) {
			t.Fatalf("interleaving broken at depth %d index %d", depth, i)
		}
	}
	for _, c := range *n.children {
		count += validateTree(t, tr, c, depth+1)
	}
	return count
}

func subtreeBounds(n *node[int]) (min, max int) {
	cur := n
	for !cur.leaf() {
		cur = (*cur.children)[0]
	}
	min = cur.items[0]
	cur = n
	for !cur.leaf() {
		cur = (*cur.children)[len(*cur.children)-1]
	}
	max = cur.items[len(cur.items)-1]
	return min, max
}

func newBulkTree(degree int) *BTreeG[int] {
	return NewBTreeGOptions(func(a, b int) bool { return a < b },
		Options{Degree: degree, NoLocks: true})
}

func TestBulkLoadValid(t *testing.T) {
	for _, deg := range []int{2, 3, 8, 32} {
		for n := 0; n <= 2000; n++ {
			tr := newBulkTree(deg)
			stats, err := tr.BulkLoad(rangeIter(n), nil)
			if err != nil {
				t.Fatalf("deg=%d n=%d: %v", deg, n, err)
			}
			if tr.Len() != n || stats.Loaded != n {
				t.Fatalf("deg=%d n=%d len=%d loaded=%d",
					deg, n, tr.Len(), stats.Loaded)
			}
			if tr.root != nil {
				if got := validateTree(t, tr, tr.root, 0); got != n {
					t.Fatalf("deg=%d n=%d counted=%d", deg, n, got)
				}
			}
			for i := 0; i < n; i++ {
				if v, ok := tr.Get(i); !ok || v != i {
					t.Fatalf("deg=%d n=%d missing %d", deg, n, i)
				}
			}
		}
	}
}

// The final leaf must straddle capacity boundaries cleanly.
func TestBulkLoadCapacityBoundaries(t *testing.T) {
	for _, deg := range []int{2, 4, 32} {
		max := deg*2 - 1
		for _, delta := range []int{-2, -1, 0, 1, 2} {
			n := max + delta
			if n < 1 {
				continue
			}
			tr2 := newBulkTree(deg)
			if _, err := tr2.BulkLoad(rangeIter(n), nil); err != nil {
				t.Fatalf("n=%d: %v", n, err)
			}
			validateTree(t, tr2, tr2.root, 0)
		}
	}
}

func TestBulkLoadOutOfOrder(t *testing.T) {
	tr := newBulkTree(4)
	_, err := tr.BulkLoad(func(yield func(item int) bool) {
		yield(1)
		yield(3)
		yield(2) // out of order
	}, nil)
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("want ErrOutOfOrder, got %v", err)
	}
	if tr.Len() != 0 {
		t.Fatalf("tree must be untouched on error, len=%d", tr.Len())
	}
}

// The very last key being out of order exercises failure after a near-complete
// tree was built in temporary memory.
func TestBulkLoadLastKeyOutOfOrder(t *testing.T) {
	tr := newBulkTree(4)
	_, err := tr.BulkLoad(func(yield func(item int) bool) {
		for i := 0; i < 200; i++ {
			yield(i)
		}
		yield(150)
	}, nil)
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("want ErrOutOfOrder, got %v", err)
	}
	if tr.Len() != 0 {
		t.Fatalf("old tree must survive, len=%d", tr.Len())
	}
}

func TestBulkLoadReplacesTreeAndPreservesCopy(t *testing.T) {
	tr := newBulkTree(8)
	for i := 0; i < 50; i++ {
		tr.Set(i)
	}
	snap := tr.Copy()

	_, err := tr.BulkLoad(rangeIter(10), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Len() != 10 {
		t.Fatalf("new tree len=%d", tr.Len())
	}
	if snap.Len() != 50 {
		t.Fatalf("copy len=%d, want 50", snap.Len())
	}
	for i := 0; i < 50; i++ {
		if _, ok := snap.Get(i); !ok {
			t.Fatalf("copy missing %d", i)
		}
	}
	for i := 0; i < 10; i++ {
		if _, ok := tr.Get(i); !ok {
			t.Fatalf("new tree missing %d", i)
		}
	}

	// Mutating the replaced tree must not corrupt the copy.
	tr.Set(1000)
	if _, ok := snap.Get(1000); ok {
		t.Fatal("copy isolated from replaced tree")
	}

	// A failed rebuild also leaves both trees intact.
	_, err = tr.BulkLoad(func(yield func(item int) bool) {
		yield(0)
		yield(5)
		yield(3)
	}, nil)
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("got %v", err)
	}
	if tr.Len() != 11 {
		t.Fatalf("tree should keep previous build, len=%d", tr.Len())
	}
	if snap.Len() != 50 {
		t.Fatalf("copy len=%d", snap.Len())
	}
}

func TestBulkLoadDuplicates(t *testing.T) {
	input := func(yield func(item int) bool) {
		values := []int{1, 2, 2, 3, 5, 5, 5, 8}
		for _, v := range values {
			if !yield(v) {
				return
			}
		}
	}

	t.Run("reject", func(t *testing.T) {
		tr := newBulkTree(8)
		_, err := tr.BulkLoad(input, &BulkOptions{Duplicates: RejectDuplicates})
		if !errors.Is(err, ErrDuplicateKey) {
			t.Fatalf("got %v", err)
		}
		if tr.Len() != 0 {
			t.Fatal("reject must leave tree untouched")
		}
	})

	t.Run("keepfirst", func(t *testing.T) {
		tr := newBulkTree(8)
		stats, err := tr.BulkLoad(input,
			&BulkOptions{Duplicates: KeepFirst})
		if err != nil {
			t.Fatal(err)
		}
		if stats.Loaded != 5 || stats.Duplicates != 3 {
			t.Fatalf("loaded=%d dup=%d", stats.Loaded, stats.Duplicates)
		}
		var got []int
		tr.Scan(func(item int) bool { got = append(got, item); return true })
		want := []int{1, 2, 3, 5, 8}
		if len(got) != len(want) {
			t.Fatalf("got %v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	})

	t.Run("keeplast", func(t *testing.T) {
		m := NewMap[int, int](8)
		stats, err := m.BulkLoad(func(yield func(k, v int) bool) {
			pairs := [][2]int{{1, 1}, {2, 20}, {2, 21}, {3, 3},
				{5, 50}, {5, 51}}
			for _, p := range pairs {
				yield(p[0], p[1])
			}
		}, &BulkOptions{Duplicates: KeepLast})
		if err != nil {
			t.Fatal(err)
		}
		if stats.Loaded != 4 {
			t.Fatalf("loaded=%d", stats.Loaded)
		}
		if v, _ := m.Get(2); v != 21 {
			t.Fatalf("key2 want 21 got %d", v)
		}
		if v, _ := m.Get(5); v != 51 {
			t.Fatalf("key5 want 51 got %d", v)
		}
	})
}

// Iterator-provided input is never materialized into one big slice: the
// recorded peak bytes stays proportional to tree nodes, not the full N items.
func TestBulkLoadStatsAndPeakMemory(t *testing.T) {
	const n = 200000
	tr := newBulkTree(32)

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	stats, err := tr.BulkLoad(rangeIter(n), nil)
	runtime.ReadMemStats(&m1)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Loaded != n {
		t.Fatalf("loaded=%d", stats.Loaded)
	}
	if stats.Comparisons == 0 {
		t.Fatal("comparisons should be recorded")
	}
	if stats.PeakBytes == 0 || stats.Nodes == 0 {
		t.Fatal("node accounting missing")
	}
	// Ordered distinct input performs exactly two comparisons per item
	// after the first.
	if want := int64((n - 1) * 2); stats.Comparisons != want {
		t.Fatalf("comparisons=%d want %d", stats.Comparisons, want)
	}

	// The peak temporary memory must be far smaller than collecting all n
	// items into one extra slice of ints (8 bytes each here would be large,
	// but the packed tree itself dominates); assert it is within the order
	// of the resulting tree and does not grow with a giant extra copy.
	t.Logf("nodes=%d itemSlots=%d peakBytes=%d heapDelta=%d",
		stats.Nodes, stats.ItemSlots, stats.PeakBytes,
		m1.TotalAlloc-m0.TotalAlloc)
	if stats.PeakBytes > uintptr(n)*16 {
		t.Fatalf("peakBytes=%d suggests a full-element extra slice",
			stats.PeakBytes)
	}
}

func TestBulkLoadNilIterator(t *testing.T) {
	tr := newBulkTree(8)
	if _, err := tr.BulkLoad(nil, nil); !errors.Is(err, ErrNilIterator) {
		t.Fatalf("got %v", err)
	}

	var s Set[int]
	if _, err := s.BulkLoad(nil, nil); !errors.Is(err, ErrNilIterator) {
		t.Fatalf("got %v", err)
	}
}

func TestSetBulkLoad(t *testing.T) {
	var s Set[int]
	stats, err := s.BulkLoad(func(yield func(key int) bool) {
		for i := 0; i < 100; i++ {
			if !yield(i) {
				return
			}
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Loaded != 100 || s.Len() != 100 {
		t.Fatalf("loaded=%d len=%d", stats.Loaded, s.Len())
	}
	if !s.Contains(99) || s.Contains(100) {
		t.Fatal("set membership wrong")
	}
}

func TestBulkLoadEmpty(t *testing.T) {
	tr := newBulkTree(8)
	stats, err := tr.BulkLoad(func(yield func(item int) bool) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Len() != 0 || stats.Loaded != 0 {
		t.Fatal("empty load should leave empty tree")
	}
}
