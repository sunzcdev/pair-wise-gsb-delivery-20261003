package btree

import (
	"errors"
	"fmt"
	"testing"
)

// rangeIter returns an iterator over 0..n-1.
func rangeIter(n int) func() (int, bool) {
	i := 0
	return func() (int, bool) {
		if i >= n {
			return 0, false
		}
		v := i
		i++
		return v, true
	}
}

// assertOrdered verifies that tr contains exactly the keys 0..n-1, both by
// random Get and by an in-order scan.
func assertOrdered(t *testing.T, tr *BTreeG[int], n int) {
	t.Helper()
	if tr.Len() != n {
		t.Fatalf("Len=%d want %d", tr.Len(), n)
	}
	for k := 0; k < n; k++ {
		v, ok := tr.Get(k)
		if !ok || v != k {
			t.Fatalf("Get(%d)=(%d,%v)", k, v, ok)
		}
	}
	got := 0
	tr.Scan(func(item int) bool {
		if item != got {
			t.Fatalf("scan at %d got %d", got, item)
		}
		got++
		return true
	})
	if got != n {
		t.Fatalf("scan saw %d want %d", got, n)
	}
}

func TestBulkLoadOrdered(t *testing.T) {
	for _, deg := range []int{1, 2, 3, 4, 8, 32} {
		t.Run(fmt.Sprintf("deg=%d", deg), func(t *testing.T) {
			probe := NewBTreeGOptions[int](testLess, Options{Degree: deg})
			max := probe.max
			sizes := []int{0, 1, 2, max - 1, max, max + 1,
				2 * max, 3*max + 1,
				max * max, max*max + 1, max*(max+1) + 1}
			for _, n := range sizes {
				tr := NewBTreeGOptions[int](testLess, Options{Degree: deg})
				stats, err := tr.BulkLoad(rangeIter(n), nil)
				if err != nil {
					t.Fatalf("n=%d: %v (%+v)", n, err, stats)
				}
				if stats.Loaded != n || stats.Items != n {
					t.Fatalf("n=%d stats=%+v", n, stats)
				}
				assertOrdered(t, tr, n)
			}
		})
	}
}

func TestBulkLoadComparisons(t *testing.T) {
	tr := NewBTreeG[int](testLess)
	const n = 1000
	stats, err := tr.BulkLoad(rangeIter(n), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Every adjacent pair is compared once while streaming and once during
	// the final in-order verification.
	want := int64(2 * (n - 1))
	if stats.Comparisons != want {
		t.Fatalf("Comparisons=%d want %d", stats.Comparisons, want)
	}
}

func TestBulkLoadPeakBounded(t *testing.T) {
	tr := NewBTreeGOptions[int](testLess, Options{Degree: 8})
	const n = 20000
	stats, err := tr.BulkLoad(rangeIter(n), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PeakBytes == 0 || stats.Nodes == 0 {
		t.Fatalf("stats not recorded: %+v", stats)
	}
	// Per-node accounting must cover the item payload at least once and stay
	// well under a naive full-copy-plus-tree estimate. The point is that peak
	// stays proportional to a single tree rather than the stream plus a tree.
	dataBytes := uintptr(n) * 8
	if stats.PeakBytes < dataBytes {
		t.Fatalf("PeakBytes=%d below item payload %d",
			stats.PeakBytes, dataBytes)
	}
	upper := dataBytes * 16
	if stats.PeakBytes >= upper {
		t.Fatalf("PeakBytes=%d >= %d (unexpected extra buffering)",
			stats.PeakBytes, upper)
	}
	assertOrdered(t, tr, n)
}

func TestBulkLoadReplacesOldTreeAndCopy(t *testing.T) {
	tr := NewBTreeG[int](testLess)
	for _, k := range []int{100, 200, 300} {
		tr.Set(k)
	}
	snapshot := tr.Copy()

	stats, err := tr.BulkLoad(rangeIter(50), nil)
	if err != nil {
		t.Fatalf("%v (%+v)", err, stats)
	}
	assertOrdered(t, tr, 50)
	if _, ok := tr.Get(200); ok {
		t.Fatal("old key 200 should have been replaced")
	}

	// The snapshot must keep the pre-load contents.
	if snapshot.Len() != 3 {
		t.Fatalf("snapshot Len=%d", snapshot.Len())
	}
	for _, k := range []int{100, 200, 300} {
		if _, ok := snapshot.Get(k); !ok {
			t.Fatalf("snapshot lost key %d", k)
		}
	}
}

func TestBulkLoadFailureKeepsOldTreeAndCopy(t *testing.T) {
	tr := NewBTreeG[int](testLess)
	for _, k := range []int{100, 200, 300} {
		tr.Set(k)
	}
	snapshot := tr.Copy()

	// The last key is out of order.
	seq := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 2}
	i := 0
	stats, err := tr.BulkLoad(func() (int, bool) {
		if i >= len(seq) {
			return 0, false
		}
		v := seq[i]
		i++
		return v, true
	}, nil)
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("want ErrOutOfOrder, got %v", err)
	}
	if stats.Items != len(seq) {
		t.Fatalf("Items=%d want %d", stats.Items, len(seq))
	}
	if tr.Len() != 3 {
		t.Fatalf("old tree mutated on failure, Len=%d", tr.Len())
	}
	for _, k := range []int{100, 200, 300} {
		if _, ok := tr.Get(k); !ok {
			t.Fatalf("old tree lost key %d after failed load", k)
		}
	}
	if snapshot.Len() != 3 {
		t.Fatalf("copy mutated on failure, Len=%d", snapshot.Len())
	}
}

func TestBulkLoadDuplicatePolicies(t *testing.T) {
	seq := []int{0, 1, 2, 2, 3, 4, 4, 4, 5}

	type runResult struct {
		tr    *BTreeG[int]
		stats BulkStats
		err   error
	}
	run := func(policy DuplicatePolicy) runResult {
		tr := NewBTreeG[int](testLess)
		i := 0
		stats, err := tr.BulkLoad(func() (int, bool) {
			if i >= len(seq) {
				return 0, false
			}
			v := seq[i]
			i++
			return v, true
		}, &BulkOptions{Duplicates: policy})
		return runResult{tr, stats, err}
	}

	if r := run(RejectDuplicates); !errors.Is(r.err, ErrDuplicate) {
		t.Fatalf("RejectDuplicates: want ErrDuplicate, got %v", r.err)
	}

	r := run(KeepFirst)
	tr, stats, err := r.tr, r.stats, r.err
	if err != nil {
		t.Fatalf("KeepFirst: %v", err)
	}
	if stats.Duplicates != 3 || stats.Loaded != 6 || stats.Items != len(seq) {
		t.Fatalf("KeepFirst stats=%+v", stats)
	}
	assertOrdered(t, tr, 6)

	r = run(KeepLast)
	tr, stats, err = r.tr, r.stats, r.err
	if err != nil {
		t.Fatalf("KeepLast: %v", err)
	}
	if stats.Duplicates != 3 || stats.Loaded != 6 {
		t.Fatalf("KeepLast stats=%+v", stats)
	}
	assertOrdered(t, tr, 6)
}

func TestBulkLoadBadIterator(t *testing.T) {
	tr := NewBTreeG[int](testLess)
	if _, err := tr.BulkLoad(nil, nil); !errors.Is(err, ErrNilIterator) {
		t.Fatalf("nil iterator: got %v", err)
	}

	var yielded int
	tr2 := NewBTreeG(func(a, b any) bool { return a.(int) < b.(int) })
	_, err := tr2.BulkLoad(func() (any, bool) {
		yielded++
		if yielded == 1 {
			return 1, true
		}
		return nil, true
	}, nil)
	if !errors.Is(err, ErrNilItem) {
		t.Fatalf("nil item: got %v", err)
	}
}

func TestBulkLoadBoundaryExact(t *testing.T) {
	// Exercise sizes exactly on and just past a leaf capacity boundary so the
	// final leaf goes through the borrow path and the normal path.
	for _, deg := range []int{2, 4, 32} {
		mk := func() *BTreeG[int] {
			return NewBTreeGOptions[int](testLess, Options{Degree: deg})
		}
		max := mk().max
		for _, n := range []int{
			max + 1, // one short trailing leaf after a split
			max + 2,
			2 * max,
			2*max + 1,
		} {
			tr := mk()
			if _, err := tr.BulkLoad(rangeIter(n), nil); err != nil {
				t.Fatalf("deg=%d n=%d: %v", deg, n, err)
			}
			assertOrdered(t, tr, n)
		}
	}
}

// TestBulkLoadRandomEquivalence builds random sorted ranges and compares the
// resulting tree against one populated through ordinary Set calls.
func TestBulkLoadRandomEquivalence(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		n := randPseudo(iter) % 500
		deg := []int{2, 3, 5, 32}[iter%4]
		bulk := NewBTreeGOptions[int](testLess, Options{Degree: deg})
		ref := NewBTreeGOptions[int](testLess, Options{Degree: deg})
		if _, err := bulk.BulkLoad(rangeIter(n), nil); err != nil {
			t.Fatalf("iter=%d n=%d: %v", iter, n, err)
		}
		for k := 0; k < n; k++ {
			ref.Set(k)
		}
		if bulk.Len() != ref.Len() {
			t.Fatalf("iter=%d n=%d len mismatch", iter, n)
		}
		for k := 0; k < n; k++ {
			v1, ok1 := bulk.Get(k)
			v2, ok2 := ref.Get(k)
			if ok1 != ok2 || v1 != v2 {
				t.Fatalf("iter=%d key=%d bulk=(%d,%v) ref=(%d,%v)",
					iter, k, v1, ok1, v2, ok2)
			}
		}
	}
}

// randPseudo is a small deterministic generator used in place of the package
// rand source to keep the test self contained.
func randPseudo(seed int) int {
	x := uint32(seed*2654435761 + 1)
	x ^= x << 13
	x ^= x >> 17
	x ^= x << 5
	return int(x % 1000)
}
