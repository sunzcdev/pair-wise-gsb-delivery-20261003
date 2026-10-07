package rtree

import (
	"math"
	"sync/atomic"
	"testing"
)

func pt2(x, y float64) [2]float64 {
	return [2]float64{x, y}
}

// snapID scans a tree into a map from identity id to its current rect.
func snapID(tr *RTreeG[int]) map[int]rect[float64] {
	out := make(map[int]rect[float64])
	tr.Scan(func(min, max [2]float64, id int) bool {
		if _, dup := out[id]; dup {
			panic("duplicate identity in tree")
		}
		out[id] = rect[float64]{min, max}
		return true
	})
	return out
}

func mri(id int, omin, omax, nmin, nmax [2]float64) ReplaceItem[int] {
	return ReplaceItem[int]{
		OldMin: omin, OldMax: omax, OldData: id,
		NewMin: nmin, NewMax: nmax, NewData: id,
	}
}

func mustReplace(t *testing.T, tr *RTreeG[int], items []ReplaceItem[int],
) *RTreeG[int] {
	t.Helper()
	next, err := tr.ReplaceItems(items, nil)
	if err != nil {
		t.Fatalf("ReplaceItems failed unexpectedly: %v", err)
	}
	if next == nil {
		t.Fatal("ReplaceItems returned nil tree without error")
	}
	return next
}

func replaceErr(t *testing.T, tr *RTreeG[int], items []ReplaceItem[int],
) *ReplaceError[int] {
	t.Helper()
	next, err := tr.ReplaceItems(items, nil)
	if err == nil {
		t.Fatal("ReplaceItems succeeded, want error")
	}
	if next != nil {
		t.Fatal("ReplaceItems returned a tree on failure")
	}
	rerr, ok := err.(*ReplaceError[int])
	if !ok {
		t.Fatalf("error type %T, want *ReplaceError", err)
	}
	return rerr
}

// Two objects exchange positions in a single frame. The resulting snapshot
// must show both objects at their new positions, never a half-swapped state.
func TestReplaceItemsSwap(t *testing.T) {
	var tr RTreeG[int]
	a := rect[float64]{pt2(0, 0), pt2(1, 1)}
	b := rect[float64]{pt2(9, 9), pt2(10, 10)}
	tr.Insert(a.min, a.max, 1)
	tr.Insert(b.min, b.max, 2)

	items := []ReplaceItem[int]{
		mri(1, a.min, a.max, b.min, b.max), // id 1 moves where id 2 was
		mri(2, b.min, b.max, a.min, a.max), // id 2 moves where id 1 was
	}
	next := mustReplace(t, &tr, items)
	if next.Len() != 2 {
		t.Fatalf("len = %d, want 2", next.Len())
	}
	got := snapID(next)
	if got[1] != b || got[2] != a {
		t.Fatalf("swap result wrong: 1=%v 2=%v", got[1], got[2])
	}
	// Original frame is still fully intact.
	old := snapID(&tr)
	if old[1] != a || old[2] != b {
		t.Fatal("original tree mutated by ReplaceItems")
	}
	if err := rSane(next); err != nil {
		t.Fatalf("new tree not sane: %v", err)
	}
}

// Identical rectangles with different identities are distinct. Matching must
// never delete "the first entry at the coordinates" and call it success.
func TestReplaceItemsSameRectDifferentIdentity(t *testing.T) {
	var tr RTreeG[int]
	r := rect[float64]{pt2(5, 5), pt2(6, 6)}
	tr.Insert(r.min, r.max, 100)
	tr.Insert(r.min, r.max, 200)

	// Move only identity 200. The other identity must stay put.
	nr := rect[float64]{pt2(20, 20), pt2(21, 21)}
	next := mustReplace(t, &tr, []ReplaceItem[int]{
		mri(200, r.min, r.max, nr.min, nr.max),
	})
	got := snapID(next)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[100] != r {
		t.Fatalf("identity 100 moved unexpectedly: %v", got[100])
	}
	if got[200] != nr {
		t.Fatalf("identity 200 not moved: %v", got[200])
	}

	// Referencing a missing third identity at the same rect is an error even
	// though two matching rectangles exist, and the tree is left unchanged.
	rerr := replaceErr(t, &tr, []ReplaceItem[int]{
		mri(300, r.min, r.max, nr.min, nr.max),
	})
	if len(rerr.Conflicts) != 1 || !rerr.Conflicts[0].Missing {
		t.Fatalf("want one missing conflict, got %+v", rerr.Conflicts)
	}
}

// Operating on the same entry twice in one batch is an error, regardless of
// whether the duplicate targets a new or the original rectangle.
func TestReplaceItemsDuplicate(t *testing.T) {
	var tr RTreeG[int]
	r := rect[float64]{pt2(0, 0), pt2(1, 1)}
	tr.Insert(r.min, r.max, 7)

	// One real entry, two operations referencing it.
	rerr := replaceErr(t, &tr, []ReplaceItem[int]{
		mri(7, r.min, r.max, pt2(2, 2), pt2(3, 3)),
		mri(7, r.min, r.max, pt2(4, 4), pt2(5, 5)),
	})
	var dup, miss int
	for _, c := range rerr.Conflicts {
		if c.Duplicate {
			dup++
		}
		if c.Missing {
			miss++
		}
	}
	if dup != 1 || miss != 0 {
		t.Fatalf("want exactly one duplicate, got dup=%d miss=%d (%+v)",
			dup, miss, rerr.Conflicts)
	}

	// Two distinct entries at one rect, three operations: first two match,
	// third is a duplicate.
	tr.Insert(r.min, r.max, 8)
	rerr = replaceErr(t, &tr, []ReplaceItem[int]{
		mri(7, r.min, r.max, pt2(2, 2), pt2(3, 3)),
		mri(8, r.min, r.max, pt2(4, 4), pt2(5, 5)),
		mri(7, r.min, r.max, pt2(6, 6), pt2(7, 7)),
	})
	if len(rerr.Conflicts) != 1 || !rerr.Conflicts[0].Duplicate ||
		rerr.Conflicts[0].Index != 2 {
		t.Fatalf("want duplicate at index 2, got %+v", rerr.Conflicts)
	}

	if got := snapID(&tr); len(got) != 2 {
		t.Fatal("original tree mutated by failed batch")
	}
}

// Invalid new rectangles and missing entries are all reported together, even
// in the same item. The old tree must not change.
func TestReplaceItemsConflictsComplete(t *testing.T) {
	var tr RTreeG[int]
	r := rect[float64]{pt2(0, 0), pt2(1, 1)}
	tr.Insert(r.min, r.max, 1)

	items := []ReplaceItem[int]{
		mri(1, r.min, r.max, pt2(2, 2), pt2(3, 3)),         // ok
		mri(2, r.min, r.max, pt2(0, 0), pt2(1, 1)),         // missing identity
		mri(1, r.min, r.max, pt2(9, 9), pt2(8, 8)),         // duplicate + bad rect
		mri(9, pt2(5, 5), pt2(6, 6), pt2(7, 7), pt2(6, 6)), // missing + bad rect
	}
	rerr := replaceErr(t, &tr, items)
	byIndex := map[int]ReplaceConflict[int]{}
	for _, c := range rerr.Conflicts {
		byIndex[c.Index] = c
	}
	if _, ok := byIndex[0]; ok {
		t.Fatal("item 0 should have no conflict")
	}
	if c := byIndex[1]; !c.Missing || c.InvalidNewRect || c.Duplicate {
		t.Fatalf("item 1 want Missing only, got %+v", c)
	}
	if c := byIndex[2]; !c.Duplicate || !c.InvalidNewRect {
		t.Fatalf("item 2 want Duplicate+InvalidNewRect, got %+v", c)
	}
	if c := byIndex[3]; !c.Missing || !c.InvalidNewRect {
		t.Fatalf("item 3 want Missing+InvalidNewRect, got %+v", c)
	}

	got := snapID(&tr)
	if len(got) != 1 || got[1] != r {
		t.Fatal("original tree mutated by failed batch")
	}
	if tr.Len() != 1 {
		t.Fatalf("len = %d, want 1", tr.Len())
	}
}

// NaN boundaries are invalid new rectangles.
func TestReplaceItemsNaN(t *testing.T) {
	var tr RTreeG[int]
	r := rect[float64]{pt2(0, 0), pt2(1, 1)}
	tr.Insert(r.min, r.max, 1)
	nan := math.NaN()
	rerr := replaceErr(t, &tr, []ReplaceItem[int]{
		mri(1, r.min, r.max, pt2(nan, 0), pt2(1, 1)),
	})
	if !rerr.Conflicts[0].InvalidNewRect {
		t.Fatalf("want InvalidNewRect, got %+v", rerr.Conflicts[0])
	}
}

// Moving every object outside the previous root bounds grows the new tree's
// bounds while leaving the old frame viewable with its old bounds.
func TestReplaceItemsMoveOutsideRootBounds(t *testing.T) {
	var tr RTreeG[int]
	tr.Insert(pt2(0, 0), pt2(1, 1), 1)
	tr.Insert(pt2(2, 2), pt2(3, 3), 2)
	oldMin, oldMax := tr.Bounds()

	items := []ReplaceItem[int]{
		mri(1, pt2(0, 0), pt2(1, 1), pt2(100, 100), pt2(101, 101)),
		mri(2, pt2(2, 2), pt2(3, 3), pt2(-50, -50), pt2(-49, -49)),
	}
	next := mustReplace(t, &tr, items)
	nMin, nMax := next.Bounds()
	if !(nMin[0] == -50 && nMin[1] == -50 && nMax[0] == 101 && nMax[1] == 101) {
		t.Fatalf("new bounds wrong: %v %v", nMin, nMax)
	}
	// The old tree still reports the old bounds.
	min, max := tr.Bounds()
	if min != oldMin || max != oldMax {
		t.Fatal("original bounds changed")
	}
	got := snapID(next)
	if got[1] != (rect[float64]{pt2(100, 100), pt2(101, 101)}) {
		t.Fatalf("id 1 wrong: %v", got[1])
	}
	if got[2] != (rect[float64]{pt2(-50, -50), pt2(-49, -49)}) {
		t.Fatalf("id 2 wrong: %v", got[2])
	}
	if err := rSane(next); err != nil {
		t.Fatalf("new tree not sane: %v", err)
	}

	// Search on the old bounds region finds nothing in the new frame.
	var hits int
	next.Search(pt2(-1, -1), pt2(4, 4), func(_, _ [2]float64, _ int) bool {
		hits++
		return true
	})
	if hits != 0 {
		t.Fatalf("found %d items in vacated region", hits)
	}
}

// Replace the whole population in one frame: old identities disappear into an
// empty staging copy and brand new identities are inserted, proving the
// "delete to empty then add" transition is atomic and never observed halfway.
func TestReplaceItemsDeleteToEmptyThenAdd(t *testing.T) {
	var tr RTreeG[int]
	const n = 300
	oldRects := make(map[int]rect[float64])
	var items []ReplaceItem[int]
	for i := 0; i < n; i++ {
		or := rect[float64]{
			min: pt2(float64(i), 0),
			max: pt2(float64(i)+0.5, 1),
		}
		oldRects[i] = or
		tr.Insert(or.min, or.max, i)
	}
	// New frame carries an entirely different set of ids placed in a disjoint
	// region. Every old id is referenced once and every new id added once by
	// folding the add onto the same slot (NewData changes identity too).
	for i := 0; i < n; i++ {
		or := oldRects[i]
		newID := n + i
		nr := rect[float64]{
			min: pt2(float64(i), 100),
			max: pt2(float64(i)+0.5, 101),
		}
		items = append(items, ReplaceItem[int]{
			OldMin: or.min, OldMax: or.max, OldData: i,
			NewMin: nr.min, NewMax: nr.max, NewData: newID,
		})
	}
	next := mustReplace(t, &tr, items)
	if err := rSane(next); err != nil {
		t.Fatalf("new tree not sane: %v", err)
	}
	got := snapID(next)
	if len(got) != n {
		t.Fatalf("len = %d, want %d", len(got), n)
	}
	for i := 0; i < n; i++ {
		if _, present := got[i]; present {
			t.Fatalf("old id %d still present in new frame", i)
		}
		want := rect[float64]{
			min: pt2(float64(i), 100),
			max: pt2(float64(i)+0.5, 101),
		}
		if got[n+i] != want {
			t.Fatalf("new id %d wrong: %v", n+i, got[n+i])
		}
	}
	if len(snapID(&tr)) != n {
		t.Fatal("original tree changed")
	}
}

// An empty batch returns an independent copy; mutating it must not affect the
// receiver.
func TestReplaceItemsEmpty(t *testing.T) {
	var tr RTreeG[int]
	tr.Insert(pt2(0, 0), pt2(1, 1), 1)
	next := mustReplace(t, &tr, nil)
	if next == &tr {
		t.Fatal("expected a distinct tree value")
	}
	if next.Len() != 1 {
		t.Fatalf("len = %d, want 1", next.Len())
	}
	next.Delete(pt2(0, 0), pt2(1, 1), 1)
	if tr.Len() != 1 {
		t.Fatal("mutating the empty-batch result changed the original")
	}

	// Empty batch on an empty tree also yields a usable tree.
	var empty RTreeG[int]
	next2 := mustReplace(t, &empty, []ReplaceItem[int]{})
	if next2.Len() != 0 {
		t.Fatal("empty tree copy wrong")
	}
}

// Failed ReplaceItems must leave both the receiver and previously published
// copies completely untouched.
func TestReplaceItemsCopyImmutability(t *testing.T) {
	var tr RTreeG[int]
	tr.Insert(pt2(0, 0), pt2(1, 1), 1)
	tr.Insert(pt2(2, 2), pt2(3, 3), 2)
	snapshot := tr.Copy()

	bad := []ReplaceItem[int]{
		mri(1, pt2(0, 0), pt2(1, 1), pt2(50, 50), pt2(51, 51)),
		mri(99, pt2(2, 2), pt2(3, 3), pt2(60, 60), pt2(61, 61)), // missing
	}
	replaceErr(t, &tr, bad)

	if tr.Len() != 2 || snapshot.Len() != 2 {
		t.Fatal("failed batch changed lengths")
	}
	for _, check := range []*RTreeG[int]{&tr, snapshot} {
		got := snapID(check)
		if got[1] != (rect[float64]{pt2(0, 0), pt2(1, 1)}) ||
			got[2] != (rect[float64]{pt2(2, 2), pt2(3, 3)}) {
			t.Fatal("failed batch mutated a tree")
		}
	}

	// A successful batch on the original likewise leaves the earlier copy put.
	ok := []ReplaceItem[int]{
		mri(1, pt2(0, 0), pt2(1, 1), pt2(-9, -9), pt2(-8, -8)),
	}
	next := mustReplace(t, &tr, ok)
	if snapID(snapshot)[1] != (rect[float64]{pt2(0, 0), pt2(1, 1)}) {
		t.Fatal("successful batch changed a previously copied tree")
	}
	if snapID(next)[1] != (rect[float64]{pt2(-9, -9), pt2(-8, -8)}) {
		t.Fatal("new frame wrong")
	}
}

// Randomized multi-leaf bulk swaps: each frame moves a large population in a
// way that is only consistent if the whole batch applies atomically.
func TestReplaceItemsBulkRandom(t *testing.T) {
	var tr RTreeG[int]
	const n = 5000
	// Put ids at x=id, then each frame reverses positions: id i and n-1-i
	// swap. Half-frame would place two ids at the same point.
	cur := make([]rect[float64], n)
	for i := 0; i < n; i++ {
		r := rect[float64]{pt2(float64(i), 0), pt2(float64(i), 0)}
		cur[i] = r
		tr.Insert(r.min, r.max, i)
	}
	makeItems := func() []ReplaceItem[int] {
		items := make([]ReplaceItem[int], n)
		for i := 0; i < n; i++ {
			j := n - 1 - i
			nr := rect[float64]{pt2(float64(j), 0), pt2(float64(j), 0)}
			items[i] = mri(i, cur[i].min, cur[i].max, nr.min, nr.max)
		}
		return items
	}
	for frame := 0; frame < 3; frame++ {
		next := mustReplace(t, &tr, makeItems())
		if err := rSane(next); err != nil {
			t.Fatalf("frame %d not sane: %v", frame, err)
		}
		seen := map[[2]float64]int{}
		next.Scan(func(min, max [2]float64, id int) bool {
			if prev, dup := seen[min]; dup {
				t.Fatalf("frame %d: ids %d and %d collide at %v (half-frame)",
					frame, prev, id, min)
			}
			seen[min] = id
			return true
		})
		if len(seen) != n {
			t.Fatalf("frame %d: observed %d positions, want %d",
				frame, len(seen), n)
		}
		for i := 0; i < n; i++ {
			j := n - 1 - i
			cur[i] = rect[float64]{pt2(float64(j), 0), pt2(float64(j), 0)}
		}
		tr = *next
	}
}

// Concurrent readers must never observe a half-frame state while a writer
// publishes successive atomic frames.
func TestReplaceItemsConcurrentNoHalfFrame(t *testing.T) {
	const n = 200
	var cur atomic.Pointer[RTreeG[int]]
	var tr RTreeG[int]
	for i := 0; i < n; i++ {
		r := rect[float64]{pt2(float64(i), float64(i)), pt2(float64(i), float64(i))}
		tr.Insert(r.min, r.max, i)
	}
	cur.Store(&tr)

	stop := make(chan struct{})
	var violations atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Even frames: ids at (i,i). Odd frames: ids at (i+10000, i+10000).
		// Regions are disjoint, so any mix is detectable.
		off := 0.0
		for {
			select {
			case <-stop:
				return
			default:
			}
			base := cur.Load()
			items := make([]ReplaceItem[int], n)
			if off == 0 {
				off = 10000
			} else {
				off = 0
			}
			for i := 0; i < n; i++ {
				var oldv float64
				if off == 10000 {
					oldv = float64(i)
				} else {
					oldv = float64(i) + 10000
				}
				nv := float64(i) + off
				items[i] = mri(i, pt2(oldv, oldv), pt2(oldv, oldv),
					pt2(nv, nv), pt2(nv, nv))
			}
			next, err := base.ReplaceItems(items, nil)
			if err != nil {
				panic(err)
			}
			cur.Store(next)
		}
	}()

	for read := 0; read < 20000; read++ {
		snap := cur.Load()
		low, high := 0, 0
		snap.Scan(func(min, max [2]float64, id int) bool {
			if min[0] >= 10000 {
				high++
			} else {
				low++
			}
			return true
		})
		if (low != n || high != 0) && (high != n || low != 0) {
			violations.Add(1)
		}
	}
	close(stop)
	<-done
	if violations.Load() != 0 {
		t.Fatalf("observed %d half-frame snapshots", violations.Load())
	}
}

// Custom identity rule: match by an ID field inside a struct rather than value
// equality of the whole payload.
type locObj struct {
	ID  int
	Tag string
}

func TestReplaceItemsCustomEquality(t *testing.T) {
	var tr RTreeG[locObj]
	eq := func(a, b locObj) bool { return a.ID == b.ID }
	r := rect[float64]{pt2(0, 0), pt2(1, 1)}
	tr.Insert(r.min, r.max, locObj{ID: 1, Tag: "a"})
	tr.Insert(r.min, r.max, locObj{ID: 2, Tag: "b"})

	next, err := tr.ReplaceItems([]ReplaceItem[locObj]{{
		OldMin: r.min, OldMax: r.max, OldData: locObj{ID: 2},
		NewMin: pt2(8, 8), NewMax: pt2(9, 9),
		NewData: locObj{ID: 2, Tag: "c"},
	}}, eq)
	if err != nil {
		t.Fatal(err)
	}
	var tags []locObj
	next.Scan(func(min, max [2]float64, o locObj) bool {
		tags = append(tags, o)
		return true
	})
	moved := false
	for _, o := range tags {
		if o.ID == 2 {
			if o.Tag != "c" {
				t.Fatalf("payload not updated: %+v", o)
			}
			moved = true
		}
		if o.ID == 1 && (o.Tag != "a") {
			t.Fatalf("untouched entry altered: %+v", o)
		}
	}
	if !moved {
		t.Fatal("custom-equality entry not replaced")
	}
}

// The generic numeric core works with integer coordinates too, including
// rejecting inverted new rectangles.
func TestReplaceItemsIntCoords(t *testing.T) {
	var tr RTreeGN[int32, int]
	tr.Insert([2]int32{0, 0}, [2]int32{1, 1}, 1)
	tr.Insert([2]int32{2, 2}, [2]int32{3, 3}, 2)
	next, err := tr.ReplaceItems([]ReplaceItemGN[int32, int]{
		{
			OldMin: [2]int32{0, 0}, OldMax: [2]int32{1, 1}, OldData: 1,
			NewMin: [2]int32{10, 10}, NewMax: [2]int32{11, 11}, NewData: 1,
		},
		{
			OldMin: [2]int32{2, 2}, OldMax: [2]int32{3, 3}, OldData: 2,
			NewMin: [2]int32{-5, -5}, NewMax: [2]int32{-4, -4}, NewData: 2,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	next.Scan(func(min, max [2]int32, id int) bool {
		count++
		return true
	})
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	_, err = tr.ReplaceItems([]ReplaceItemGN[int32, int]{
		{
			OldMin: [2]int32{0, 0}, OldMax: [2]int32{1, 1}, OldData: 1,
			NewMin: [2]int32{9, 9}, NewMax: [2]int32{8, 8}, NewData: 1,
		},
	}, nil)
	if err == nil {
		t.Fatal("inverted int rect should fail")
	}
}

// The non-generic RTree wrapper exposes the same atomic batch operation.
func TestReplaceItemsAnyWrapper(t *testing.T) {
	var tr RTree
	tr.Insert(pt2(0, 0), pt2(1, 1), "a")
	tr.Insert(pt2(2, 2), pt2(3, 3), "b")
	next, err := tr.ReplaceItems([]ReplaceItem[any]{
		{
			OldMin: pt2(0, 0), OldMax: pt2(1, 1), OldData: "a",
			NewMin: pt2(9, 9), NewMax: pt2(10, 10), NewData: "a",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.Len() != 2 {
		t.Fatalf("len = %d, want 2", next.Len())
	}
	var found bool
	next.Scan(func(min, max [2]float64, data any) bool {
		if data == "a" && min == pt2(9, 9) {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("wrapper did not apply replacement")
	}

	_, err = tr.ReplaceItems([]ReplaceItem[any]{
		{
			OldMin: pt2(2, 2), OldMax: pt2(3, 3), OldData: "missing",
			NewMin: pt2(0, 0), NewMax: pt2(-1, -1),
		},
	}, nil)
	rerr, ok := err.(*ReplaceError[any])
	if !ok {
		t.Fatalf("error type %T, want *ReplaceError[any]", err)
	}
	c := rerr.Conflicts[0]
	if !c.Missing || !c.InvalidNewRect {
		t.Fatalf("want Missing+InvalidNewRect, got %+v", c)
	}
	if tr.Len() != 2 {
		t.Fatal("failed wrapper batch mutated original")
	}
}
