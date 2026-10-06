package btree

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

func TestIntervalEmpty(t *testing.T) {
	empties := []Interval{
		Closed(5, 3),                       // lo > hi
		Open(4, 4),                         // degenerate, both open
		ClosedOpen(4, 4),                   // degenerate, hi open
		OpenClosed(4, 4),                   // degenerate, lo open
		Closed(math.Inf(1), math.Inf(1)),   // degenerate at +inf
		Closed(math.Inf(-1), math.Inf(-1)), // degenerate at -inf
	}
	for _, iv := range empties {
		if !iv.Empty() {
			t.Fatalf("expected %v to be empty", iv)
		}
	}
	nonEmpties := []Interval{
		Closed(4, 4), // a single point
		Closed(4, 5),
		Open(4, 5),
		Every(),
		Closed(math.Inf(-1), 0),
		Closed(0, math.Inf(1)),
	}
	for _, iv := range nonEmpties {
		if iv.Empty() {
			t.Fatalf("expected %v to be non-empty", iv)
		}
	}
}

func TestIntervalOverlaps(t *testing.T) {
	cases := []struct {
		a, b Interval
		want bool
	}{
		{Closed(0, 10), Closed(10, 20), true},      // touching, closed
		{ClosedOpen(0, 10), Closed(10, 20), false}, // touching, hi open
		{Closed(0, 10), OpenClosed(10, 20), false}, // touching, lo open
		{Open(0, 10), Open(10, 20), false},
		{Open(0, 10), Closed(5, 15), true},
		{Closed(0, 0), Closed(0, 0), true}, // same point
		{Closed(0, 0), Open(0, 1), false},  // point excluded by open lo
		{Every(), Closed(5, 6), true},
		{Closed(math.Inf(-1), 3), Closed(3, 9), true},
		{Closed(7, math.Inf(1)), Closed(3, 9), true},
		{Closed(math.Inf(-1), 3), Closed(4, math.Inf(1)), false},
		{Closed(0, 5), Closed(6, 7), false},
		{Closed(0, 10), Closed(3, 4), true}, // containment
		// empty intervals never overlap
		{Closed(5, 3), Closed(0, 10), false},
		{Closed(0, 10), Closed(5, 3), false},
		{Open(4, 4), Open(4, 4), false},
		{Every(), Closed(5, 3), false},
	}
	for _, c := range cases {
		if got := c.a.Overlaps(c.b); got != c.want {
			t.Fatalf("%v.Overlaps(%v) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := c.b.Overlaps(c.a); got != c.want {
			t.Fatalf("%v.Overlaps(%v) = %v, want %v", c.b, c.a, got, c.want)
		}
	}
}

func TestIntervalIndexBasic(t *testing.T) {
	ix := NewIntervalIndex()
	id1 := ix.Insert(Closed(0, 10))
	id2 := ix.Insert(Closed(0, 10)) // identical bounds, distinct identity
	id3 := ix.Insert(ClosedOpen(0, 10))
	if id1 == id2 || id2 == id3 {
		t.Fatal("expected distinct identities")
	}
	if ix.Len() != 3 {
		t.Fatalf("expected len 3, got %d", ix.Len())
	}
	// duplicate bounds must come back in identity order
	res := ix.Query(Closed(5, 5))
	if len(res) != 3 || res[0].ID != id1 || res[1].ID != id2 ||
		res[2].ID != id3 {
		t.Fatalf("unexpected results: %v", res)
	}
	if !ix.Delete(id2) {
		t.Fatal("expected delete to succeed")
	}
	if ix.Delete(id2) {
		t.Fatal("expected second delete to fail")
	}
	res = ix.Query(Closed(5, 5))
	if len(res) != 2 || res[0].ID != id1 || res[1].ID != id3 {
		t.Fatalf("unexpected results after delete: %v", res)
	}
	// empty query matches nothing
	if res := ix.Query(Closed(9, 3)); len(res) != 0 {
		t.Fatalf("empty query returned %v", res)
	}
	// NaN panics
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic on NaN insert")
			}
		}()
		ix.Insert(Closed(math.NaN(), 1))
	}()
}

// checkIntervalAug walks the tree and verifies that the augmented
// value of every node equals the maximum effective hi of its subtree.
func checkIntervalAug(t *testing.T, ix *IntervalIndex) {
	t.Helper()
	var walk func(n *node[Interval]) float64
	walk = func(n *node[Interval]) float64 {
		max := math.Inf(-1)
		for _, iv := range n.items {
			if h := iv.effectiveHi(); h > max {
				max = h
			}
		}
		if !n.leaf() {
			for _, c := range *n.children {
				if h := walk(c); h > max {
					max = h
				}
			}
		}
		got, ok := n.aug.(float64)
		if !ok {
			t.Fatalf("node missing augmented value")
		}
		if got != max {
			t.Fatalf("stale augmented value: got %v, want %v", got, max)
		}
		return max
	}
	if ix.tr.root != nil {
		walk(ix.tr.root)
	}
}

// bruteForceQuery filters the reference set and sorts by (lo, openness,
// identity), the same total order the index scans in.
func bruteForceQuery(ref map[uint64]Interval, q Interval) []Interval {
	var out []Interval
	for _, iv := range ref {
		if iv.Overlaps(q) {
			out = append(out, iv)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return intervalLess(out[i], out[j])
	})
	return out
}

func randInterval(r *rand.Rand) Interval {
	lo := float64(r.Intn(41) - 20)
	hi := float64(r.Intn(41) - 20)
	if r.Intn(20) == 0 {
		lo = math.Inf(-1)
	}
	if r.Intn(20) == 0 {
		hi = math.Inf(1)
	}
	return Interval{
		Lo:     lo,
		Hi:     hi,
		LoOpen: r.Intn(2) == 0,
		HiOpen: r.Intn(2) == 0,
	}
}

func checkQuery(t *testing.T, ix *IntervalIndex, ref map[uint64]Interval,
	q Interval,
) {
	t.Helper()
	got := ix.Query(q)
	want := bruteForceQuery(ref, q)
	if len(got) != len(want) {
		t.Fatalf("query %v: got %d results, want %d\ngot:  %v\nwant: %v",
			q, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("query %v: result %d is %v, want %v",
				q, i, got[i], want[i])
		}
	}
}

func TestIntervalIndexRandom(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	// small degree to force frequent splits, rotations and merges
	ix := newIntervalIndexOptions(Options{Degree: 2})
	ref := make(map[uint64]Interval)
	var ids []uint64

	for op := 0; op < 4000; op++ {
		switch r.Intn(10) {
		case 0, 1, 2, 3, 4: // insert
			iv := randInterval(r)
			id := ix.Insert(iv)
			iv.ID = id
			ref[id] = iv
			ids = append(ids, id)
		case 5, 6, 7: // delete
			if len(ids) == 0 {
				continue
			}
			k := r.Intn(len(ids))
			id := ids[k]
			ids = append(ids[:k], ids[k+1:]...)
			if !ix.Delete(id) {
				t.Fatalf("delete of live id %d failed", id)
			}
			delete(ref, id)
		default: // query
			checkQuery(t, ix, ref, randInterval(r))
		}
		if op%97 == 0 {
			checkIntervalAug(t, ix)
			if ix.Len() != len(ref) {
				t.Fatalf("len mismatch: %d vs %d", ix.Len(), len(ref))
			}
		}
	}
	checkIntervalAug(t, ix)
	// final full verification, including an unbounded query
	checkQuery(t, ix, ref, Every())
}

func TestIntervalIndexCopy(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	ix := newIntervalIndexOptions(Options{Degree: 2})
	ref := make(map[uint64]Interval)
	var ids []uint64
	for i := 0; i < 300; i++ {
		iv := randInterval(r)
		id := ix.Insert(iv)
		iv.ID = id
		ref[id] = iv
		ids = append(ids, id)
	}

	// copy, then diverge: both indexes must stay correct
	ix2 := ix.Copy()
	ref2 := make(map[uint64]Interval, len(ref))
	for id, iv := range ref {
		ref2[id] = iv
	}
	ids2 := append([]uint64{}, ids...)

	for op := 0; op < 1500; op++ {
		switch r.Intn(4) {
		case 0, 1: // insert into both, with different intervals
			iv := randInterval(r)
			id := ix.Insert(iv)
			iv.ID = id
			ref[id] = iv
			ids = append(ids, id)

			iv2 := randInterval(r)
			id2 := ix2.Insert(iv2)
			iv2.ID = id2
			ref2[id2] = iv2
			ids2 = append(ids2, id2)
		case 2: // delete from the original
			if len(ids) > 0 {
				k := r.Intn(len(ids))
				id := ids[k]
				ids = append(ids[:k], ids[k+1:]...)
				ix.Delete(id)
				delete(ref, id)
			}
		case 3: // delete from the copy
			if len(ids2) > 0 {
				k := r.Intn(len(ids2))
				id := ids2[k]
				ids2 = append(ids2[:k], ids2[k+1:]...)
				ix2.Delete(id)
				delete(ref2, id)
			}
		}
		if op%53 == 0 {
			q := randInterval(r)
			checkQuery(t, ix, ref, q)
			checkQuery(t, ix2, ref2, q)
			checkIntervalAug(t, ix)
			checkIntervalAug(t, ix2)
		}
	}
	checkQuery(t, ix, ref, Every())
	checkQuery(t, ix2, ref2, Every())
}

// TestIntervalIndexLongInterval ensures that a long interval covering
// many short ones is never lost to subtree pruning, and that pruning
// itself keeps results correct.
func TestIntervalIndexLongInterval(t *testing.T) {
	ix := newIntervalIndexOptions(Options{Degree: 2})
	const n = 5000
	for i := 0; i < n; i++ {
		// many short, non-overlapping intervals
		ix.Insert(ClosedOpen(float64(i*2), float64(i*2)+1))
	}
	longID := ix.Insert(Closed(-1e6, 1e6))

	// query a small range buried in the short intervals
	res := ix.Query(Closed(2000.25, 2000.75))
	if len(res) != 2 {
		t.Fatalf("expected 2 results, got %d: %v", len(res), res)
	}
	var foundLong bool
	for _, iv := range res {
		if iv.ID == longID {
			foundLong = true
		}
	}
	if !foundLong {
		t.Fatal("long interval was pruned away")
	}

	// query a range that only the long interval covers
	res = ix.Query(Closed(-5000, -4000))
	if len(res) != 1 || res[0].ID != longID {
		t.Fatalf("expected only the long interval, got %v", res)
	}

	// delete the long interval; the short ones must remain intact
	if !ix.Delete(longID) {
		t.Fatal("delete of long interval failed")
	}
	res = ix.Query(Closed(-5000, -4000))
	if len(res) != 0 {
		t.Fatalf("expected no results, got %v", res)
	}
	res = ix.Query(Closed(2000.25, 2000.75))
	if len(res) != 1 {
		t.Fatalf("expected 1 result, got %v", res)
	}
	checkIntervalAug(t, ix)
}

func TestIntervalIndexUnboundedAndEmpty(t *testing.T) {
	ix := NewIntervalIndex()
	emptyID := ix.Insert(Closed(9, 2))  // empty: lo > hi
	ix.Insert(Open(4, 4))               // empty: degenerate open
	ix.Insert(Every())                  // unbounded both sides
	ix.Insert(Closed(math.Inf(-1), 0))  // unbounded below
	ix.Insert(Closed(100, math.Inf(1))) // unbounded above

	// the empty intervals never match, everything else does
	res := ix.Query(Closed(-50, -40))
	if len(res) != 2 { // Every + (-inf, 0]
		t.Fatalf("expected 2 results, got %v", res)
	}
	for _, iv := range res {
		if iv.ID == emptyID {
			t.Fatal("empty interval returned by query")
		}
	}
	res = ix.Query(Every())
	if len(res) != 3 {
		t.Fatalf("expected 3 results, got %v", res)
	}
	// empty intervals can still be fetched and deleted by identity
	if _, ok := ix.Get(emptyID); !ok {
		t.Fatal("empty interval not found by id")
	}
	if !ix.Delete(emptyID) {
		t.Fatal("delete of empty interval failed")
	}
	checkIntervalAug(t, ix)
}

// TestIntervalAugmentedTreeOps drives the augmented tree through every
// structural mutation path (bulk load, pop, delete-at, iterative and
// ranged deletes) and verifies the augmented values stay correct.
func TestIntervalAugmentedTreeOps(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	ix := newIntervalIndexOptions(Options{Degree: 2})
	tr := ix.tr
	var seq uint64
	next := func() Interval {
		seq++
		iv := randInterval(r)
		iv.ID = seq
		return iv
	}
	for op := 0; op < 3000; op++ {
		switch r.Intn(8) {
		case 0:
			tr.Set(next())
		case 1:
			tr.Load(next())
		case 2:
			tr.PopMin()
		case 3:
			tr.PopMax()
		case 4:
			if tr.Len() > 0 {
				tr.DeleteAt(r.Intn(tr.Len()))
			}
		case 5:
			tr.Delete(next())
		case 6:
			keep := r.Intn(2) == 0
			tr.DeleteAscend(next(), func(item Interval) Action {
				if keep {
					return Keep
				}
				return Delete
			})
		case 7:
			tr.DeleteRange(next(), next(), nil)
		}
		checkIntervalAug(t, ix)
	}
}
