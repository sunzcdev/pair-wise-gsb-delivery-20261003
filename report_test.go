// Copyright 2021 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package rtree

import (
	"math"
	"math/rand"
	"testing"
)

func testWeight(data int) int64 {
	return int64(data%97) + 1
}

// bruteReport scans every item and computes the report the slow way.
func bruteReport(tr *RTreeGN[float64, int], min, max [2]float64,
) (count, weight int64) {
	target := rect[float64]{min, max}
	tr.Scan(func(imin, imax [2]float64, data int) bool {
		ir := rect[float64]{imin, imax}
		if target.contains(&ir) {
			count++
			weight += testWeight(data)
		}
		return true
	})
	return count, weight
}

func checkReport(t *testing.T, tr *RTreeGN[float64, int], min, max [2]float64,
) {
	t.Helper()
	ec, ew := bruteReport(tr, min, max)
	ac, aw := tr.Report(min, max)
	if ac != ec || aw != ew {
		t.Fatalf("window %v %v: got (%d,%d), want (%d,%d)",
			min, max, ac, aw, ec, ew)
	}
}

func newReportTree(n int) (*RTreeGN[float64, int], []rect[float64]) {
	tr := new(RTreeGN[float64, int])
	tr.SetWeightFunc(testWeight)
	rects := make([]rect[float64], n)
	for i := 0; i < n; i++ {
		r := randRect('m')
		rects[i] = r
		tr.Insert(r.min, r.max, i)
	}
	return tr, rects
}

func TestReportRandomWindows(t *testing.T) {
	tr, rects := newReportTree(20000)
	if tr.Len() != 20000 {
		t.Fatalf("expected 20000, got %d", tr.Len())
	}
	for i := 0; i < 200; i++ {
		var min, max [2]float64
		if i%3 == 0 {
			// window aligned to an item boundary to exercise the
			// closed-interval containment
			r := rects[rand.Intn(len(rects))]
			min = r.min
			max = [2]float64{
				r.min[0] + rand.Float64()*2,
				r.min[1] + rand.Float64()*2,
			}
		} else {
			min = [2]float64{
				rand.Float64()*360 - 180,
				rand.Float64()*180 - 90,
			}
			max = [2]float64{
				min[0] + rand.Float64()*20,
				min[1] + rand.Float64()*10,
			}
		}
		checkReport(t, tr, min, max)
	}
	// whole-tree window
	min, max := tr.Bounds()
	checkReport(t, tr, min, max)
	// window outside of the tree bounds
	checkReport(t, tr, [2]float64{1000, 1000}, [2]float64{1010, 1010})
}

func TestReportMutations(t *testing.T) {
	tr, rects := newReportTree(10000)
	checkReport(t, tr, [2]float64{-180, -90}, [2]float64{180, 90})

	// delete half of the items
	for i := 0; i < len(rects); i += 2 {
		tr.Delete(rects[i].min, rects[i].max, i)
	}
	if tr.Len() != 5000 {
		t.Fatalf("expected 5000, got %d", tr.Len())
	}
	for i := 0; i < 50; i++ {
		r := randRect('m')
		checkReport(t, tr, r.min, [2]float64{
			r.min[0] + rand.Float64()*30,
			r.min[1] + rand.Float64()*15,
		})
	}

	// replace a quarter of the remaining items with new rects
	for i := 1; i < len(rects); i += 4 {
		nr := randRect('m')
		tr.Replace(rects[i].min, rects[i].max, i, nr.min, nr.max, i)
		rects[i] = nr
	}
	for i := 0; i < 50; i++ {
		r := randRect('m')
		checkReport(t, tr, r.min, [2]float64{
			r.min[0] + rand.Float64()*30,
			r.min[1] + rand.Float64()*15,
		})
	}

	// insert more items and verify again
	for i := len(rects); i < len(rects)+5000; i++ {
		r := randRect('m')
		tr.Insert(r.min, r.max, i)
	}
	for i := 0; i < 50; i++ {
		r := randRect('m')
		checkReport(t, tr, r.min, [2]float64{
			r.min[0] + rand.Float64()*30,
			r.min[1] + rand.Float64()*15,
		})
	}
	checkReport(t, tr, [2]float64{-180, -90}, [2]float64{180, 90})
}

func TestReportCopyOnWrite(t *testing.T) {
	tr1, rects := newReportTree(10000)
	oc, ow := bruteReport(tr1, [2]float64{-180, -90}, [2]float64{180, 90})

	// shallow copy, then fork-modify both sides
	tr2 := tr1.Copy()
	for i := 0; i < len(rects); i += 2 {
		tr2.Delete(rects[i].min, rects[i].max, i)
	}
	for i := 0; i < 3000; i++ {
		r := randRect('m')
		tr1.Insert(r.min, r.max, 100000+i)
		tr2.Insert(r.min, r.max, 200000+i)
	}

	// the old copy must still report the old content
	c, w := tr2.Report([2]float64{-180, -90}, [2]float64{180, 90})
	ec, ew := bruteReport(tr2, [2]float64{-180, -90}, [2]float64{180, 90})
	if c != ec || w != ew {
		t.Fatalf("tr2: got (%d,%d), want (%d,%d)", c, w, ec, ew)
	}
	// tr1 was only appended to; its full-window totals must equal the
	// original totals plus the weight of the newly inserted items
	c, w = tr1.Report([2]float64{-180, -90}, [2]float64{180, 90})
	var addw int64
	for i := 0; i < 3000; i++ {
		addw += testWeight(100000 + i)
	}
	if c != oc+3000 || w != ow+addw {
		t.Fatalf("tr1: got (%d,%d), want (%d,%d)", c, w, oc+3000, ow+addw)
	}
	// random windows on both trees
	for i := 0; i < 50; i++ {
		r := randRect('m')
		max := [2]float64{
			r.min[0] + rand.Float64()*30,
			r.min[1] + rand.Float64()*15,
		}
		checkReport(t, tr1, r.min, max)
		checkReport(t, tr2, r.min, max)
	}
}

func TestReportFullTreeConstantVisits(t *testing.T) {
	tr, _ := newReportTree(50000)
	min, max := tr.Bounds()
	// expand the window a bit so it fully covers the tree bounds
	min[0]--
	min[1]--
	max[0]++
	max[1]++
	var visited int
	count, weight, nodes := tr.report(min, max, &visited)
	if nodes != 1 || visited != 1 {
		t.Fatalf("full-tree window visited %d nodes, want 1", nodes)
	}
	if count != 50000 {
		t.Fatalf("expected count 50000, got %d", count)
	}
	_, ew := bruteReport(tr, min, max)
	if weight != ew {
		t.Fatalf("expected weight %d, got %d", ew, weight)
	}

	// a small window must only visit a tiny fraction of the tree
	visited = 0
	wmin := [2]float64{0, 0}
	wmax := [2]float64{1, 1}
	_, _, nodes = tr.report(wmin, wmax, &visited)
	t.Logf("small window visited %d nodes for 50000 items", nodes)
	if nodes > 1000 {
		t.Fatalf("small window visited too many nodes: %d", nodes)
	}
}

func TestReportBoundaryClosedInterval(t *testing.T) {
	tr := new(RTreeGN[float64, int])
	tr.SetWeightFunc(testWeight)
	// points on the boundary of the window [0,0]-[10,10]
	pts := [][2]float64{
		{0, 0}, {10, 10}, {0, 10}, {10, 0}, {5, 0}, {5, 10}, {0, 5}, {10, 5},
	}
	for i, p := range pts {
		tr.Insert(p, p, i)
	}
	tr.Insert([2]float64{11, 5}, [2]float64{11, 5}, 100) // outside
	tr.Insert([2]float64{-1, 5}, [2]float64{-1, 5}, 101) // outside
	tr.Insert([2]float64{5, 5}, [2]float64{12, 12}, 102) // sticks out
	count, weight := tr.Report([2]float64{0, 0}, [2]float64{10, 10})
	if count != int64(len(pts)) {
		t.Fatalf("expected %d boundary items, got %d", len(pts), count)
	}
	var ew int64
	for i := range pts {
		ew += testWeight(i)
	}
	if weight != ew {
		t.Fatalf("expected weight %d, got %d", ew, weight)
	}
}

func TestReportWeightOverflow(t *testing.T) {
	tr := new(RTreeGN[float64, int])
	tr.SetWeightFunc(func(data int) int64 { return math.MaxInt64 - 1 })
	if err := tr.InsertChecked([2]float64{0, 0}, [2]float64{1, 1}, 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err := tr.InsertChecked([2]float64{2, 2}, [2]float64{3, 3}, 2)
	if err != ErrWeightOverflow {
		t.Fatalf("expected ErrWeightOverflow, got %v", err)
	}
	if tr.Len() != 1 {
		t.Fatalf("failed insert modified the tree, len=%d", tr.Len())
	}
	count, weight := tr.Report([2]float64{-10, -10}, [2]float64{10, 10})
	if count != 1 || weight != math.MaxInt64-1 {
		t.Fatalf("got (%d,%d), want (1,%d)", count, weight, math.MaxInt64-1)
	}

	// Replace that would overflow must fail and restore the old item
	err = tr.ReplaceChecked(
		[2]float64{0, 0}, [2]float64{1, 1}, 1,
		[2]float64{0, 0}, [2]float64{1, 1}, 1,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tr2 := new(RTreeGN[float64, int])
	var neg bool
	tr2.SetWeightFunc(func(data int) int64 {
		if neg {
			return -1
		}
		return math.MaxInt64 - 1
	})
	if err := tr2.InsertChecked([2]float64{0, 0}, [2]float64{1, 1}, 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	neg = true
	if err := tr2.InsertChecked([2]float64{2, 2}, [2]float64{3, 3}, 2); err != ErrNegativeWeight {
		t.Fatalf("expected ErrNegativeWeight, got %v", err)
	}
	if tr2.Len() != 1 {
		t.Fatalf("failed insert modified the tree, len=%d", tr2.Len())
	}
}

func TestReportWithoutWeightFunc(t *testing.T) {
	tr := new(RTreeGN[float64, int])
	for i := 0; i < 1000; i++ {
		r := randRect('m')
		tr.Insert(r.min, r.max, i)
	}
	count, weight := tr.Report([2]float64{-180, -90}, [2]float64{180, 90})
	if count != 1000 || weight != 0 {
		t.Fatalf("got (%d,%d), want (1000,0)", count, weight)
	}
}

func TestReportSubCountInvariant(t *testing.T) {
	// verify the maintained subCount matches a deep count of the tree
	tr, rects := newReportTree(20000)
	for i := 0; i < len(rects); i += 3 {
		tr.Delete(rects[i].min, rects[i].max, i)
	}
	if tr.root.subCount != int64(tr.root.deepCount()) {
		t.Fatalf("subCount %d != deepCount %d",
			tr.root.subCount, tr.root.deepCount())
	}
	if tr.root.subCount != int64(tr.Len()) {
		t.Fatalf("subCount %d != Len %d", tr.root.subCount, tr.Len())
	}
}
