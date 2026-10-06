package btree

import (
	"math/rand"
	"sort"
	"testing"
)

// rec is a stored interval record for the differential tests.
type rec struct {
	start int // value coordinate, or -1 for -infinity
	end   int // value coordinate, or sentinelInf for +infinity
	lc    bool
	rc    bool
	id    int
}

const infCoord = 1 << 30

func lessInt(a, b int) bool { return a < b }

func toInterval(r rec) Interval[int, int] {
	iv := Interval[int, int]{
		Start: End[int]{Value: r.start, Inclusive: r.lc},
		End:   End[int]{Value: r.end, Inclusive: r.rc},
		ID:    r.id,
		Item:  r.id,
	}
	if r.start == -1 {
		iv.Start = End[int]{Infinite: true}
	}
	if r.end == infCoord {
		iv.End = End[int]{Infinite: true}
	}
	return iv
}

// points maps the dense integer coordinates of rec (which lie in 0..N-1
// or the infinity sentinels) onto a doubled grid: coordinate x occupies
// slot 2x+1. An interval contains slot 2x when x is included by a closed
// boundary, and slot 2x+1 for interior points. This turns open/closed
// boundary semantics into plain integer-set membership.
func slotRange(r rec) (lo, hi int, ok bool) {
	if r.start == -1 {
		lo = -2 * infCoord // -infinity, below every finite slot
	} else if r.lc {
		lo = 2 * r.start // include the boundary slot
	} else {
		lo = 2*r.start + 1 // interior only
	}
	if r.end == infCoord {
		hi = 2*infCoord + 1 // +infinity
	} else if r.rc {
		hi = 2 * r.end
	} else {
		hi = 2*r.end - 1
	}
	if lo > hi {
		return 0, 0, false
	}
	return lo, hi, true
}

func refOverlap(a, b rec) bool {
	la, ha, oka := slotRange(a)
	lb, hb, okb := slotRange(b)
	if !oka || !okb {
		return false
	}
	return la <= hb && lb <= ha
}

func recLess(a, b rec) bool {
	if (a.start == -1) != (b.start == -1) {
		return a.start == -1
	}
	if a.start != -1 && a.start != b.start {
		return a.start < b.start
	}
	if a.start != -1 && a.lc != b.lc {
		return a.lc // closed first
	}
	return a.id < b.id
}

// checkTree compares every finite query against the brute-force model and
// validates the augmentation invariant on every node.
func checkTree(t *testing.T, tr *IntervalTreeG[int, int], recs []rec, coords int) {
	t.Helper()
	// augmentation invariant: node meta == max end of the whole subtree.
	if tr.tr.root != nil {
		verifyMeta(t, tr, tr.tr.root)
	}
	if tr.Len() != len(recs) {
		t.Fatalf("len: got %d want %d", tr.Len(), len(recs))
	}
	// exhaustive queries over every coordinate and boundary combination,
	// plus unbounded ends on both sides.
	var queries []rec
	for x := 0; x < coords; x++ {
		for _, lc := range []bool{false, true} {
			for _, rc := range []bool{false, true} {
				queries = append(queries, rec{start: x, end: x, lc: lc, rc: rc})
			}
		}
	}
	for a := 0; a < coords; a++ {
		for b := a; b < coords; b++ {
			queries = append(queries,
				rec{start: a, end: b, lc: false, rc: false},
				rec{start: a, end: b, lc: true, rc: false},
				rec{start: a, end: b, lc: false, rc: true},
				rec{start: a, end: b, lc: true, rc: true},
			)
		}
	}
	queries = append(queries,
		rec{start: -1, end: infCoord},
		rec{start: -1, end: coords - 1, rc: true},
		rec{start: 0, end: infCoord, lc: true},
	)
	for _, q := range queries {
		var want []rec
		for _, r := range recs {
			if refOverlap(r, q) {
				want = append(want, r)
			}
		}
		sort.SliceStable(want, func(i, j int) bool { return recLess(want[i], want[j]) })
		var got []rec
		tr.Overlaps(toInterval(q), func(iv Interval[int, int]) bool {
			got = append(got, rec{
				start: infStart(iv.Start),
				end:   infEnd(iv.End),
				lc:    iv.Start.Inclusive,
				rc:    iv.End.Inclusive,
				id:    iv.ID,
			})
			return true
		})
		if len(got) != len(want) {
			t.Fatalf("query %+v: got %d hits want %d\ngot=%v\nwant=%v",
				q, len(got), len(want), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("query %+v: order/content mismatch at %d\ngot=%v\nwant=%v",
					q, i, got, want)
			}
		}
	}
	// full ordered scan must itself be start/id sorted
	var all []rec
	tr.Scan(
		func(iv Interval[int, int]) bool {
			all = append(all, rec{
				start: infStart(iv.Start), end: infEnd(iv.End),
				lc: iv.Start.Inclusive, rc: iv.End.Inclusive, id: iv.ID,
			})
			return true
		})
	if len(all) != len(recs) {
		t.Fatalf("scan: got %d want %d", len(all), len(recs))
	}
	if !sort.SliceIsSorted(all, func(i, j int) bool { return recLess(all[i], all[j]) }) {
		t.Fatalf("scan not start/id sorted: %v", all)
	}
}

func infStart(e End[int]) int {
	if e.Infinite {
		return -1
	}
	return e.Value
}

func infEnd(e End[int]) int {
	if e.Infinite {
		return infCoord
	}
	return e.Value
}

func verifyMeta(t *testing.T, it *IntervalTreeG[int, int],
	n *node[Interval[int, int]],
) endBound[int] {
	t.Helper()
	var want endBound[int]
	for i := range n.items {
		want.absorb(endOf(n.items[i]), it.lessK)
	}
	if !n.leaf() {
		for i := range *n.children {
			want.absorb(verifyMeta(t, it, (*n.children)[i]), it.lessK)
		}
	}
	got, ok := n.meta.(endBound[int])
	if !ok {
		t.Fatalf("node meta missing or wrong type")
	}
	if got != want {
		t.Fatalf("node meta mismatch: got %+v want %+v", got, want)
	}
	return got
}

func randRec(rng *rand.Rand, id, coords int, allowInf bool) rec {
	r := rec{id: id}
	if allowInf && rng.Intn(6) == 0 {
		r.start = -1
	} else {
		r.start = rng.Intn(coords)
		r.lc = rng.Intn(2) == 0
	}
	if allowInf && rng.Intn(6) == 0 {
		r.end = infCoord
	} else {
		r.end = rng.Intn(coords)
		r.rc = rng.Intn(2) == 0
	}
	return r
}

func TestIntervalRandomized(t *testing.T) {
	const coords = 6
	for _, degree := range []int{2, 3, 32} {
		rng := rand.New(rand.NewSource(int64(degree*7919 + 42)))
		tr := NewIntervalTreeG[int, int](lessInt, lessInt, degree)
		var recs []rec
		nextID := 1
		for step := 0; step < 6000; step++ {
			switch rng.Intn(10) {
			case 0, 1, 2, 3, 4, 5, 6:
				r := randRec(rng, nextID, coords, true)
				nextID++
				tr.Set(toInterval(r))
				recs = append(recs, r)
			case 7:
				if len(recs) > 0 {
					i := rng.Intn(len(recs))
					r := recs[i]
					// delete with the exact (start, ID) key but a zero
					// end payload, proving the end is not part of the key.
					key := rec{start: r.start, end: r.start, lc: r.lc,
						rc: false, id: r.id}
					prev, ok := tr.Delete(toInterval(key))
					if !ok || prev.ID != r.id {
						t.Fatalf("delete miss: %v ok=%v prev=%v", r, ok, prev)
					}
					recs = append(recs[:i], recs[i+1:]...)
				}
			case 8:
				// replace keeping identity, changing bounds
				if len(recs) > 0 {
					i := rng.Intn(len(recs))
					r := recs[i]
					nr := randRec(rng, r.id, coords, true)
					// keep the unique start/ID slot; only change the end
					nr.start = r.start
					nr.lc = r.lc
					tr.Set(toInterval(nr))
					recs[i] = nr
				}
			case 9:
				if len(recs) > 20 {
					// bulk-load a fresh tree occasionally
					// (keep within this switch; rare)
				}
			}
			if step%300 == 0 {
				checkTree(t, tr, recs, coords)
			}
		}
		checkTree(t, tr, recs, coords)
	}
}

// refOvIDs returns the sorted IDs of records overlapping q.
func refOvIDs(recs []rec, q rec) []int {
	var ids []int
	for _, r := range recs {
		if refOverlap(r, q) {
			ids = append(ids, r.id)
		}
	}
	sort.Ints(ids)
	return ids
}

func ovIDs(tr *IntervalTreeG[int, int], q rec) []int {
	var ids []int
	tr.Overlaps(toInterval(q), func(iv Interval[int, int]) bool {
		ids = append(ids, iv.ID)
		return true
	})
	sort.Ints(ids)
	return ids
}

// TestIntervalCopyIsolation exercises copy-on-write: both the original
// and the copy must keep correct augmentation after independent sets,
// deletes, splits, rotations and merges.
func TestIntervalCopyIsolation(t *testing.T) {
	const coords = 5
	for _, degree := range []int{2, 32} {
		rng := rand.New(rand.NewSource(int64(degree*131 + 7)))
		tr := NewIntervalTreeG[int, int](lessInt, lessInt, degree)
		var recs []rec
		id := 1
		for i := 0; i < 200; i++ {
			r := randRec(rng, id, coords, true)
			id++
			tr.Set(toInterval(r))
			recs = append(recs, r)
		}
		cp := tr.Copy()
		var cpRecs []rec
		cpRecs = append(cpRecs, recs...)
		for step := 0; step < 3000; step++ {
			target := tr
			targetRecs := &recs
			cpTarget := false
			if rng.Intn(2) == 0 {
				target = cp
				targetRecs = &cpRecs
				cpTarget = true
			}
			switch rng.Intn(3) {
			case 0, 1:
				r := randRec(rng, id, coords, true)
				id++
				target.Set(toInterval(r))
				*targetRecs = append(*targetRecs, r)
			case 2:
				if len(*targetRecs) > 0 {
					i := rng.Intn(len(*targetRecs))
					r := (*targetRecs)[i]
					target.Delete(toInterval(r))
					*targetRecs = append((*targetRecs)[:i],
						(*targetRecs)[i+1:]...)
				}
			}
			_ = cpTarget
			if step%250 == 0 {
				checkTree(t, tr, recs, coords)
				checkTree(t, cp, cpRecs, coords)
			}
		}
		checkTree(t, tr, recs, coords)
		checkTree(t, cp, cpRecs, coords)
	}
}

// TestIntervalLongCoveringShort builds many short disjoint intervals and
// one very long interval covering all of them. Queries under the long
// interval must return every short record even though aggressive max-end
// pruning is in effect; it also checks that deleting the cover keeps the
// shorts intact.
func TestIntervalLongCoveringShort(t *testing.T) {
	for _, degree := range []int{2, 4, 32} {
		tr := NewIntervalTreeG[int, int](lessInt, lessInt, degree)
		const n = 2000
		for i := 0; i < n; i++ {
			// tiny closed point interval [2i, 2i]
			tr.Set(toInterval(rec{start: 2 * i, end: 2 * i,
				lc: true, rc: true, id: i + 1}))
		}
		// long open interval covering every short point
		cover := rec{start: 0, end: 2*n + 1, lc: false, rc: false, id: 999999}
		tr.Set(toInterval(cover))

		// query the whole span: must find all shorts and the cover
		all := rec{start: -1, end: infCoord}
		got := ovIDs(tr, all)
		if len(got) != n+1 {
			t.Fatalf("degree %d: full span got %d want %d",
				degree, len(got), n+1)
		}
		// a query wholly inside the cover: every short in range
		q := rec{start: 10, end: 2*(n-50) + 1, lc: false, rc: false}
		var manual []rec
		for i := 0; i < n; i++ {
			manual = append(manual, rec{start: 2 * i, end: 2 * i,
				lc: true, rc: true, id: i + 1})
		}
		manual = append(manual, cover)
		if ids := ovIDs(tr, q); !equalInts(ids, refOvIDs(manual, q)) {
			t.Fatalf("degree %d: inner query mismatch", degree)
		}
		// point query at the far end: the last short must survive pruning
		point := rec{start: 2 * (n - 1), end: 2 * (n - 1),
			lc: true, rc: true}
		ids := ovIDs(tr, point)
		if !equalInts(ids, refOvIDs(manual, point)) {
			t.Fatalf("degree %d: far-end point query got %v", degree, ids)
		}
		// every short point must find itself together with the cover
		for i := 0; i < n; i += 37 {
			p := rec{start: 2 * i, end: 2 * i, lc: true, rc: true}
			wantIDs := []int{i + 1, cover.id}
			if i == 0 {
				// point 0 is excluded by the cover's open start
				wantIDs = []int{1}
			}
			if ids := ovIDs(tr, p); !equalInts(ids, wantIDs) {
				t.Fatalf("degree %d: point %d got %v", degree, i, ids)
			}
		}
		// an odd interior point hits only the cover, not the even shorts
		gap := rec{start: 3, end: 3, lc: true, rc: true}
		if ids := ovIDs(tr, gap); !equalInts(ids, []int{cover.id}) {
			t.Fatalf("degree %d: cover-only gap got %v", degree, ids)
		}
		// remove the cover; shorts remain queryable
		tr.Delete(toInterval(cover))
		if ids := ovIDs(tr, point); len(ids) != 1 || ids[0] != n {
			t.Fatalf("degree %d: after cover delete point got %v", degree, ids)
		}
		// augmentation must still be structurally valid, and without the
		// cover a far-away point query prunes nearly the entire tree.
		verifyMeta(t, tr, tr.tr.root)
		visited := countVisited(tr, toInterval(point))
		if visited > tr.tr.Height()*tr.tr.max*4 {
			t.Fatalf("degree %d: pruning ineffective, visited %d nodes",
				degree, visited)
		}
	}
}

// countVisited runs an overlap query while counting how many tree nodes
// the traversal enters, exposing the effect of subtree pruning.
func countVisited(it *IntervalTreeG[int, int], q Interval[int, int]) int {
	if q.Empty(it.lessK) {
		return 0
	}
	visited := 0
	var walk func(cn **node[Interval[int, int]])
	walk = func(cn **node[Interval[int, int]]) {
		n := *cn
		visited++
		itemLimit := len(n.items)
		if !q.End.Infinite {
			for itemLimit > 0 {
				s := n.items[itemLimit-1].Start
				if s.Infinite || it.lessK(s.Value, q.End.Value) ||
					(!it.lessK(q.End.Value, s.Value) &&
						s.Inclusive && q.End.Inclusive) {
					break
				}
				itemLimit--
			}
		}
		if n.leaf() {
			return
		}
		for i := 0; i < itemLimit; i++ {
			if it.childCanReach((*n.children)[i], q.Start) {
				walk(&(*n.children)[i])
			}
		}
		if itemLimit == len(n.items) {
			if it.childCanReach((*n.children)[len(*n.children)-1], q.Start) {
				walk(&(*n.children)[len(*n.children)-1])
			}
		} else if it.childCanReach((*n.children)[itemLimit], q.Start) {
			walk(&(*n.children)[itemLimit])
		}
	}
	walk(&it.tr.root)
	return visited
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestIntervalDuplicateBounds verifies independent identities with equal
// bounds, stable start/identity ordering, and open/closed touching.
func TestIntervalDuplicateBounds(t *testing.T) {
	tr := NewIntervalTreeG[int, int](lessInt, lessInt, 2)
	// five identities on the exact same interval [3, 7)
	for _, id := range []int{50, 10, 40, 20, 30} {
		tr.Set(toInterval(rec{start: 3, end: 7, lc: true, rc: false, id: id}))
	}
	// [7, 9) does NOT overlap (open/closed touch at 7)
	tr.Set(toInterval(rec{start: 7, end: 9, lc: false, rc: true, id: 7}))
	// [7, 9] DOES overlap [3,7] (closed both at 7)
	tr.Set(toInterval(rec{start: 7, end: 9, lc: true, rc: true, id: 8}))

	var ids []int
	tr.Overlaps(toInterval(rec{start: 3, end: 7, lc: true, rc: false}),
		func(iv Interval[int, int]) bool {
			ids = append(ids, iv.ID)
			return true
		})
	// id 7 is (7,9]: its open start excludes point 7, so no touch overlap.
	// id 8 is [7,9] and the query end is open, so point 7 is excluded too;
	// only the five equal-bounded intervals match.
	if !equalInts(ids, []int{10, 20, 30, 40, 50}) {
		t.Fatalf("overlap ids = %v", ids)
	}

	// [7,7] closed point hits only [7,9] (id 8): (7,9] excludes 7 by its
	// open start, and [3,7) excludes 7 by its open end.
	ids = nil
	tr.Overlaps(toInterval(rec{start: 7, end: 7, lc: true, rc: true}),
		func(iv Interval[int, int]) bool {
			ids = append(ids, iv.ID)
			return true
		})
	if !equalInts(ids, []int{8}) {
		t.Fatalf("point ids = %v", ids)
	}
}

// TestIntervalEmptyAndUnbounded checks the documented semantics directly.
func TestIntervalEmptyAndUnbounded(t *testing.T) {
	less := lessInt
	mk := func(s, e int, lc, rc bool) Interval[int, int] {
		return toInterval(rec{start: s, end: e, lc: lc, rc: rc})
	}
	emptyOpen := mk(3, 3, false, false) // (3,3)
	emptyHalf := mk(3, 3, true, false)  // [3,3)
	point := mk(3, 3, true, true)       // [3,3]
	normal := mk(0, 10, false, true)    // (0,10]
	negInf := toInterval(rec{start: -1, end: 5, rc: true})
	posInf := toInterval(rec{start: 5, end: infCoord, lc: true})
	allInf := Interval[int, int]{
		Start: End[int]{Infinite: true},
		End:   End[int]{Infinite: true},
	}
	if !emptyOpen.Empty(less) || !emptyHalf.Empty(less) {
		t.Fatal("open/half-open degenerate intervals must be empty")
	}
	if point.Empty(less) || normal.Empty(less) || negInf.Empty(less) ||
		posInf.Empty(less) || allInf.Empty(less) {
		t.Fatal("these intervals must not be empty")
	}
	for _, other := range []Interval[int, int]{point, normal, negInf,
		posInf, allInf, emptyOpen} {
		if emptyOpen.Overlaps(other, less) {
			t.Fatal("empty interval must not overlap anything")
		}
	}
	if !negInf.Overlaps(posInf, less) {
		t.Fatal("(-inf,5] and [5,+inf) share point 5")
	}
	negInfOpen := toInterval(rec{start: -1, end: 5}) // (-inf,5)
	if negInfOpen.Overlaps(posInf, less) {
		t.Fatal("(-inf,5) and [5,+inf) must not overlap")
	}
	if !allInf.Overlaps(normal, less) {
		t.Fatal("fully unbounded overlaps everything non-empty")
	}

	// empty intervals are stored and enumerated by Scan but never matched
	tr := NewIntervalTreeG[int, int](lessInt, lessInt, 2)
	tr.Set(emptyOpen)
	tr.Set(point)
	if tr.Len() != 2 {
		t.Fatalf("len = %d", tr.Len())
	}
	var n int
	tr.Scan(func(iv Interval[int, int]) bool { n++; return true })
	if n != 2 {
		t.Fatalf("scan = %d", n)
	}
	n = 0
	tr.Overlaps(allInf, func(iv Interval[int, int]) bool { n++; return true })
	if n != 1 {
		t.Fatalf("unbounded overlap matched %d (empty must be excluded)", n)
	}
}

// TestIntervalLoad exercises the bulk-loading augmentation path.
func TestIntervalLoad(t *testing.T) {
	tr := NewIntervalTreeG[int, int](lessInt, lessInt, 32)
	var recs []rec
	for i := 0; i < 500; i++ {
		r := rec{start: i, end: i + 2, lc: true, rc: false, id: i + 1}
		recs = append(recs, r)
		tr.Load(toInterval(r))
	}
	checkTree(t, tr, recs, 503)
	q := rec{start: 100, end: 200, lc: true, rc: false}
	ids := ovIDs(tr, q)
	// [i,i+2) overlaps [100,200) for starts 99..199; the interval
	// starting exactly at 200 is excluded by the query's open end.
	if len(ids) != 101 {
		t.Fatalf("load query got %d want 101", len(ids))
	}
}
