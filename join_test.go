package rtree

import (
	"math/rand"
	"sort"
	"testing"
)

type joinItem struct {
	id       int
	data     int
	min, max [2]float64
}

type joinPair struct{ a, b int }

func (p joinPair) less(q joinPair) bool {
	if p.a != q.a {
		return p.a < q.a
	}
	return p.b < q.b
}

func sortPairs(pairs []joinPair) {
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].less(pairs[j])
	})
}

func samePairs(a, b []joinPair) bool {
	if len(a) != len(b) {
		return false
	}
	sortPairs(a)
	sortPairs(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func canonicalPairs(pairs []joinPair) []joinPair {
	out := make([]joinPair, len(pairs))
	copy(out, pairs)
	for i := range out {
		if out[i].b < out[i].a {
			out[i].a, out[i].b = out[i].b, out[i].a
		}
	}
	return out
}

func rectsOverlap64(aMin, aMax, bMin, bMax [2]float64, touches bool) bool {
	if touches {
		return !(bMin[0] > aMax[0] || bMax[0] < aMin[0] ||
			bMin[1] > aMax[1] || bMax[1] < aMin[1])
	}
	return bMin[0] < aMax[0] && bMax[0] > aMin[0] &&
		bMin[1] < aMax[1] && bMax[1] > aMin[1]
}

// naiveJoin is the unpruned cartesian-product reference. Item identity is
// always the slice position; identical rectangles or identical data never
// collapse two distinct entries.
func naiveJoin(a, b []joinItem, self, touches, unique, includeSelf bool,
) []joinPair {
	var pairs []joinPair
	for i := 0; i < len(a); i++ {
		for j := 0; j < len(b); j++ {
			if self {
				if unique && j < i {
					continue
				}
				if i == j && !includeSelf {
					continue
				}
			}
			if !rectsOverlap64(a[i].min, a[i].max, b[j].min, b[j].max,
				touches) {
				continue
			}
			pairs = append(pairs, joinPair{a[i].data, b[j].data})
		}
	}
	return pairs
}

func buildJoinTree(items []joinItem) *RTreeG[int] {
	tr := new(RTreeG[int])
	for _, it := range items {
		tr.Insert(it.min, it.max, it.data)
	}
	return tr
}

func collectJoin(tr1, tr2 *RTreeG[int], opts *JoinOptionsG[int],
	limit int,
) (pairs []joinPair, stopped bool, visits int) {
	pairs = nil
	visits = tr1.Join(tr2, opts,
		func(min1, max1 [2]float64, data1 int,
			min2, max2 [2]float64, data2 int,
		) bool {
			if limit > 0 && len(pairs) >= limit {
				stopped = true
				return false
			}
			pairs = append(pairs, joinPair{data1, data2})
			return true
		})
	return pairs, stopped, visits
}

func randJoinItems(n int, span, size float64) []joinItem {
	items := make([]joinItem, n)
	for i := range items {
		min := [2]float64{
			rand.Float64()*span - span/2,
			rand.Float64()*span - span/2,
		}
		max := [2]float64{
			min[0] + rand.Float64()*size,
			min[1] + rand.Float64()*size,
		}
		items[i] = joinItem{id: i, data: i, min: min, max: max}
	}
	return items
}

// TestJoinNaive compares the dual-tree Join against a plain cartesian
// product on small random data, for two distinct trees and for self joins.
func TestJoinNaive(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		na := 1 + rand.Intn(40)
		nb := 1 + rand.Intn(40)
		itemsA := randJoinItems(na, 10, 4)
		itemsB := randJoinItems(nb, 10, 4)
		trA := buildJoinTree(itemsA)
		trB := buildJoinTree(itemsB)
		for _, touches := range []bool{false, true} {
			opts := &JoinOptionsG[int]{Touches: touches}
			got, _, _ := collectJoin(trA, trB, opts, 0)
			want := naiveJoin(itemsA, itemsB, false, touches, false, false)
			if !samePairs(got, want) {
				t.Fatalf("iter=%d cross join mismatch (touches=%v): "+
					"got %d pairs, want %d", iter, touches,
					len(got), len(want))
			}
		}

		// Self join across the full option matrix.
		for _, touches := range []bool{false, true} {
			for _, unique := range []bool{false, true} {
				for _, includeSelf := range []bool{false, true} {
					opts := &JoinOptionsG[int]{
						Touches: touches,
						Unique:  unique,
						Self:    includeSelf,
					}
					got, _, _ := collectJoin(trA, trA, opts, 0)
					want := naiveJoin(itemsA, itemsA, true, touches,
						unique, includeSelf)
					if unique && !samePairs(
						canonicalPairs(got), canonicalPairs(want)) {
						t.Fatalf("self join mismatch: touches=%v "+
							"unique=%v self=%v: got %d pairs, want %d",
							touches, unique, includeSelf,
							len(got), len(want))
					} else if !unique && !samePairs(got, want) {
						t.Fatalf("self join mismatch: touches=%v "+
							"unique=%v self=%v: got %d pairs, want %d",
							touches, unique, includeSelf,
							len(got), len(want))
					}
				}
			}
		}
	}
}

// TestJoinDuplicates ensures that distinct entries sharing identical
// rectangles, and even identical data, are still treated as separate
// entries: deduplication must only be driven by the Unique option on a
// self join, never by rect/data equality.
func TestJoinDuplicates(t *testing.T) {
	// Three distinct entries, same rect, same data value.
	items := []joinItem{
		{data: 7, min: [2]float64{0, 0}, max: [2]float64{1, 1}},
		{data: 7, min: [2]float64{0, 0}, max: [2]float64{1, 1}},
		{data: 7, min: [2]float64{0, 0}, max: [2]float64{1, 1}},
	}
	tr := buildJoinTree(items)

	got, _, _ := collectJoin(tr, tr,
		&JoinOptionsG[int]{Unique: true}, 0)
	// 3 choose 2 = 3 distinct pairs even though every pair is (7, 7).
	if len(got) != 3 {
		t.Fatalf("unique self join: got %d pairs, want 3", len(got))
	}

	got, _, _ = collectJoin(tr, tr,
		&JoinOptionsG[int]{Unique: true, Self: true}, 0)
	// 3 choose 2 plus the three self pairs = 6.
	if len(got) != 6 {
		t.Fatalf("unique self join w/ self: got %d pairs, want 6",
			len(got))
	}

	// Two distinct trees with identical content is not a self join:
	// every cross combination must be reported.
	tr2 := buildJoinTree(append([]joinItem(nil), items...))
	got, _, _ = collectJoin(tr, tr2, &JoinOptionsG[int]{}, 0)
	if len(got) != 9 {
		t.Fatalf("cross join of identical trees: got %d pairs, want 9",
			len(got))
	}
}

// TestJoinTouches checks boundary-only contacts explicitly.
func TestJoinTouches(t *testing.T) {
	items := []joinItem{
		{data: 0, min: [2]float64{0, 0}, max: [2]float64{1, 1}},
		{data: 1, min: [2]float64{1, 0}, max: [2]float64{2, 1}}, // edge
		{data: 2, min: [2]float64{1, 1}, max: [2]float64{2, 2}}, // corner
		{data: 3, min: [2]float64{0.5, 0.5}, max: [2]float64{1.5, 1.5}},
		{data: 4, min: [2]float64{3, 3}, max: [2]float64{4, 4}}, // away
	}
	tr := buildJoinTree(items)

	got, _, _ := collectJoin(tr, tr,
		&JoinOptionsG[int]{Unique: true}, 0)
	wantSet := map[joinPair]bool{
		{0, 3}: true,
		{1, 3}: true,
		{2, 3}: true,
	}
	if len(got) != len(wantSet) {
		t.Fatalf("strict overlap: got %d pairs %v, want %d",
			len(got), got, len(wantSet))
	}
	for _, p := range got {
		if !wantSet[p] {
			t.Fatalf("strict overlap: unexpected pair %v", p)
		}
	}

	got, _, _ = collectJoin(tr, tr,
		&JoinOptionsG[int]{Touches: true, Unique: true}, 0)
	for p := range wantSet {
		// strict pairs must remain present
		var found bool
		for _, q := range got {
			if q == p {
				found = true
			}
		}
		if !found {
			t.Fatalf("touches: missing strict pair %v", p)
		}
	}
	touchPairs := map[joinPair]bool{
		{0, 1}: true, // shared edge
		{0, 2}: true, // shared corner
		{1, 2}: true, // shared edge
	}
	for p := range touchPairs {
		var found bool
		for _, q := range got {
			if q == p {
				found = true
			}
		}
		if !found {
			t.Fatalf("touches: missing boundary pair %v in %v", p, got)
		}
	}
	for _, p := range got {
		if p.a == 4 || p.b == 4 {
			t.Fatalf("touches: far entry paired: %v", p)
		}
	}
}

// TestJoinEarlyStop verifies that returning false halts the traversal and
// that the trees remain fully usable afterwards.
func TestJoinEarlyStop(t *testing.T) {
	items := randJoinItems(200, 10, 4)
	tr := buildJoinTree(items)
	const limit = 5
	got, stopped, _ := collectJoin(tr, tr,
		&JoinOptionsG[int]{Unique: true, Self: true}, limit)
	if !stopped {
		t.Fatalf("expected early stop")
	}
	if len(got) != limit {
		t.Fatalf("got %d reported pairs, want %d", len(got), limit)
	}
	// Full traversal must still work and match the reference.
	rest, _, _ := collectJoin(tr, tr,
		&JoinOptionsG[int]{Unique: true, Self: true}, 0)
	want := naiveJoin(items, items, true, false, true, true)
	if !samePairs(canonicalPairs(rest), canonicalPairs(want)) {
		t.Fatalf("tree unusable after early stop: %d vs %d",
			len(rest), len(want))
	}
	if err := rSane(tr); err != nil {
		t.Fatalf("tree not sane after early stop: %v", err)
	}
}

// TestJoinCopySnapshot verifies that Join does not mutate the tree and that
// Copy snapshots are unaffected by joins and later edits.
func TestJoinCopySnapshot(t *testing.T) {
	items := randJoinItems(100, 10, 4)
	tr := buildJoinTree(items)
	snap := tr.Copy()

	opts := &JoinOptionsG[int]{Unique: true, Self: true, Touches: true}
	before, _, _ := collectJoin(tr, tr, opts, 0)

	// Mutate the working tree after taking the snapshot.
	tr.Insert([2]float64{-4.9, -4.9}, [2]float64{-4.7, -4.7}, 99999)
	tr.Delete(items[0].min, items[0].max, items[0].data)

	snapPairs, _, _ := collectJoin(snap, snap, opts, 0)
	if !samePairs(snapPairs, before) {
		t.Fatalf("snapshot changed: %d vs %d pairs",
			len(snapPairs), len(before))
	}
	if err := rSane(snap); err != nil {
		t.Fatalf("snapshot not sane: %v", err)
	}
	if err := rSane(tr); err != nil {
		t.Fatalf("working tree not sane: %v", err)
	}
}

// TestJoinEmpty checks nil and empty inputs.
func TestJoinEmpty(t *testing.T) {
	var empty RTreeG[int]
	items := randJoinItems(10, 10, 4)
	tr := buildJoinTree(items)

	if v := empty.Join(tr, nil,
		func([2]float64, [2]float64, int, [2]float64, [2]float64, int,
		) bool {
			t.Fatalf("unexpected pair")
			return true
		}); v != 0 {
		t.Fatalf("empty join visits = %d, want 0", v)
	}
	if v := tr.Join(&empty, nil,
		func([2]float64, [2]float64, int, [2]float64, [2]float64, int,
		) bool {
			t.Fatalf("unexpected pair")
			return true
		}); v != 0 {
		t.Fatalf("empty join visits = %d, want 0", v)
	}
}

// TestJoinNumeric exercises the fully generic RTreeGN with an int
// coordinate type.
func TestJoinNumeric(t *testing.T) {
	var tr1, tr2 RTreeGN[int, string]
	tr1.Insert([2]int{0, 0}, [2]int{2, 2}, "a")
	tr1.Insert([2]int{10, 10}, [2]int{12, 12}, "b")
	tr2.Insert([2]int{1, 1}, [2]int{3, 3}, "c")
	tr2.Insert([2]int{11, 11}, [2]int{13, 13}, "d")
	tr2.Insert([2]int{2, 2}, [2]int{3, 3}, "e") // touches "a"

	var strict []string
	tr1.Join(&tr2, &JoinOptionsGN[int, string]{},
		func(min1, max1 [2]int, d1 string, min2, max2 [2]int, d2 string,
		) bool {
			strict = append(strict, d1+"-"+d2)
			return true
		})
	sort.Strings(strict)
	if len(strict) != 2 || strict[0] != "a-c" || strict[1] != "b-d" {
		t.Fatalf("strict int join: %v", strict)
	}

	var touched []string
	tr1.Join(&tr2, &JoinOptionsGN[int, string]{Touches: true},
		func(min1, max1 [2]int, d1 string, min2, max2 [2]int, d2 string,
		) bool {
			touched = append(touched, d1+"-"+d2)
			return true
		})
	sort.Strings(touched)
	if len(touched) != 3 ||
		touched[0] != "a-c" || touched[1] != "a-e" || touched[2] != "b-d" {
		t.Fatalf("touches int join: %v", touched)
	}
}

func clusterItems(clusters, per int, gap, size float64) []joinItem {
	var items []joinItem
	id := 0
	for c := 0; c < clusters; c++ {
		cx := float64(c) * gap
		cy := float64(c) * gap
		for k := 0; k < per; k++ {
			min := [2]float64{
				cx + rand.Float64()*size,
				cy + rand.Float64()*size,
			}
			max := [2]float64{
				min[0] + rand.Float64()*size,
				min[1] + rand.Float64()*size,
			}
			items = append(items, joinItem{
				id: id, data: id, min: min, max: max,
			})
			id++
		}
	}
	return items
}

// TestJoinSeparatedClusters records node-pair visits on data split into
// disjoint spatial clusters, and checks that pruning skips entire
// cross-cluster subtrees.
func TestJoinSeparatedClusters(t *testing.T) {
	const clusters = 5
	const per = 300
	// size << gap, so every cluster is far from every other cluster.
	itemsA := clusterItems(clusters, per, 10000, 10)
	itemsB := clusterItems(clusters, per, 10000, 10)
	trA := buildJoinTree(itemsA)
	trB := buildJoinTree(itemsB)

	// Sanity: the trees are multi-level, so node-level pruning matters.
	if trA.base.root.leaf() || trB.base.root.leaf() {
		t.Fatalf("expected multi-level trees")
	}

	visits := trA.Join(trB, &JoinOptionsG[int]{},
		func(min1, max1 [2]float64, data1 int,
			min2, max2 [2]float64, data2 int,
		) bool {
			return true
		})

	// Cross-cluster pairs must be exactly zero by construction; only the
	// matching clusters overlap.
	var pairCount int
	for _, a := range itemsA {
		for _, b := range itemsB {
			if rectsOverlap64(a.min, a.max, b.min, b.max, false) {
				pairCount++
			}
		}
	}

	// Record the visit count for inspection.
	t.Logf("separated clusters: %d items/tree, %d reported pairs, "+
		"%d node-pair visits", len(itemsA), pairCount, visits)

	// Compute how many node pairs an unpruned descent would visit and
	// require bounding-box pruning to skip at least half of them. With
	// five mutually disjoint clusters the expected ratio is about 1/5.
	unpruned := unprunedJoinVisits(trA.base.root, trB.base.root)
	if visits*2 >= unpruned {
		t.Fatalf("pruning ineffective: visits=%d unpruned=%d",
			visits, unpruned)
	}

	// Two trees whose bounding boxes are disjoint must visit nothing.
	var left, right RTreeG[int]
	for i := 0; i < 200; i++ {
		x := float64(i)
		left.Insert([2]float64{x, 0}, [2]float64{x + 0.5, 0.5}, i)
		right.Insert([2]float64{x + 100000, 100000},
			[2]float64{x + 100000.5, 100000.5}, i)
	}
	got := 0
	v := left.Join(&right, &JoinOptionsG[int]{},
		func([2]float64, [2]float64, int, [2]float64, [2]float64, int,
		) bool {
			got++
			return true
		})
	if got != 0 || v != 0 {
		t.Fatalf("disjoint trees: %d pairs, %d visits", got, v)
	}
}

// unprunedJoinVisits mirrors joinNodes' recursion without any bounding-box
// pruning, counting the node pairs that would be visited.
func unprunedJoinVisits(n1, n2 *node[float64, int]) int {
	count := 1
	if n1.leaf() && n2.leaf() {
		return count
	}
	if n1.leaf() {
		for _, c := range n2.children()[:n2.count] {
			count += unprunedJoinVisits(n1, c)
		}
		return count
	}
	if n2.leaf() {
		for _, c := range n1.children()[:n1.count] {
			count += unprunedJoinVisits(c, n2)
		}
		return count
	}
	for _, c1 := range n1.children()[:n1.count] {
		for _, c2 := range n2.children()[:n2.count] {
			count += unprunedJoinVisits(c1, c2)
		}
	}
	return count
}
