// Copyright 2021 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package rtree

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// fixedRects generates a deterministic distribution of n rectangles that
// mimics the daily startup workload: mostly small uniform rectangles with a
// handful of duplicate copies and point items.
func fixedRects(n int64) []rect[float64] {
	rng := rand.New(rand.NewSource(20261007))
	rs := make([]rect[float64], 0, n)
	for i := int64(0); i < n; i++ {
		var r rect[float64]
		switch {
		case i%20 == 0 && len(rs) > 0:
			r = rs[int(rng.Int63())%len(rs)] // duplicate rectangle
		case i%7 == 0:
			x := rng.Float64()*360 - 180
			y := rng.Float64()*180 - 90
			r = rect[float64]{[2]float64{x, y}, [2]float64{x, y}}
		default:
			r = randRectFrom(rng, 'r')
		}
		rs = append(rs, r)
	}
	return rs
}

func randRectFrom(rng *rand.Rand, kind byte) rect[float64] {
	const rsize = 0.02
	r := rect[float64]{
		min: [2]float64{rng.Float64()*360 - 180, rng.Float64()*180 - 90},
	}
	r.max = r.min
	if kind == 'r' || (kind == 'm' && rng.Int()%2 == 0) {
		r.max[0] = r.min[0] + rng.Float64()*rsize
		r.max[1] = r.min[1] + rng.Float64()*rsize
	}
	return r
}

func rectItems(rs []rect[float64]) []Item[float64, int] {
	items := make([]Item[float64, int], len(rs))
	for i, r := range rs {
		items[i] = Item[float64, int]{Min: r.min, Max: r.max, Data: i}
	}
	return items
}

func insertTree(rs []rect[float64]) *RTreeG[int] {
	var tr RTreeG[int]
	for i, r := range rs {
		tr.Insert(r.min, r.max, i)
	}
	return &tr
}

func TestLoadWrappers(t *testing.T) {
	// The non-generic RTree (any data) wrapper.
	var anyTr RTree
	err := anyTr.Load([]Item[float64, any]{
		{Min: [2]float64{0, 0}, Max: [2]float64{1, 1}, Data: "a"},
		{Min: [2]float64{0, 0}, Max: [2]float64{1, 1}, Data: "b"},
		{Min: [2]float64{2, 2}, Max: [2]float64{3, 3}, Data: "c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if anyTr.Len() != 3 {
		t.Fatalf("any tree len=%d", anyTr.Len())
	}
	var got []string
	anyTr.Search([2]float64{0, 0}, [2]float64{1, 1},
		func(_, _ [2]float64, data any) bool {
			got = append(got, data.(string))
			return true
		})
	sort.Strings(got)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("duplicate any items not retained: %v", got)
	}
	if err := anyTr.Load(nil); err != nil {
		t.Fatal(err)
	}
	if anyTr.Len() != 0 {
		t.Fatalf("nil load did not clear: %d", anyTr.Len())
	}

	// The generic numeric (int) variant exercises integer validation and
	// building paths.
	var intTr RTreeGN[int, string]
	err = intTr.Load([]Item[int, string]{
		{Min: [2]int{0, 0}, Max: [2]int{0, 0}, Data: "p"},
		{Min: [2]int{1, 1}, Max: [2]int{2, 2}, Data: "q"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if intTr.Len() != 2 {
		t.Fatalf("int tree len=%d", intTr.Len())
	}
}

func TestLoadCopyOnWrite(t *testing.T) {
	rs := fixedRects(3000)
	tr := loadTree(rs)
	cp := tr.Copy()

	// Mutating the copy via inserts and deletes must not affect the original.
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 500; i++ {
		r := randRectFrom(rng, 'm')
		cp.Insert(r.min, r.max, 100000+i)
	}
	for i := 0; i < 1000; i++ {
		r := rs[i]
		cp.DeleteEqual(r.min, r.max, i, func(a, b int) bool { return a == b })
	}
	if tr.Len() != len(rs) {
		t.Fatalf("original mutated through copy: len=%d want %d",
			tr.Len(), len(rs))
	}
	if err := bulkSane(tr.base.root); err != nil {
		t.Fatalf("original bulk tree corrupted: %v", err)
	}
	if err := bulkSane(cp.base.root); err != nil {
		t.Fatalf("modified copy not sane: %v", err)
	}
	if cp.Len() != len(rs)-1000+500 {
		t.Fatalf("copy len=%d", cp.Len())
	}
}

func loadTree(rs []rect[float64]) *RTreeG[int] {
	var tr RTreeG[int]
	if err := tr.Load(rectItems(rs)); err != nil {
		panic(err)
	}
	return &tr
}

// searchAll collects every item intersecting the query rectangle.
func searchAll(tr *RTreeG[int], min, max [2]float64) []int {
	var out []int
	tr.Search(min, max, func(_, _ [2]float64, data int) bool {
		out = append(out, data)
		return true
	})
	sort.Ints(out)
	return out
}

func intsEqual(a, b []int) bool {
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

func TestLoadValidation(t *testing.T) {
	// Empty input is valid and clears the tree.
	var empty RTreeG[int]
	if err := empty.Load(nil); err != nil {
		t.Fatalf("nil load: %v", err)
	}
	if empty.Len() != 0 || empty.base.root != nil {
		t.Fatal("empty load should leave an empty tree")
	}
	empty.Insert([2]float64{0, 0}, [2]float64{1, 1}, 1)
	if err := empty.Load([]Item[float64, int]{}); err != nil {
		t.Fatalf("empty slice load: %v", err)
	}
	if empty.Len() != 0 {
		t.Fatalf("expected cleared tree, got %d", empty.Len())
	}

	// Valid points and zero-area rects.
	good := []Item[float64, int]{
		{Min: [2]float64{0, 0}, Max: [2]float64{0, 0}, Data: 0},
		{Min: [2]float64{-1, -2}, Max: [2]float64{3, 4}, Data: 1},
	}
	var ok RTreeG[int]
	if err := ok.Load(good); err != nil {
		t.Fatalf("valid load: %v", err)
	}

	nan := math.NaN()
	badCases := [][]Item[float64, int]{
		{{Min: [2]float64{nan, 0}, Max: [2]float64{1, 1}}},
		{{Min: [2]float64{0, nan}, Max: [2]float64{1, 1}}},
		{{Min: [2]float64{0, 0}, Max: [2]float64{nan, 1}}},
		{{Min: [2]float64{0, 0}, Max: [2]float64{1, nan}}},
		{{Min: [2]float64{2, 0}, Max: [2]float64{1, 1}}}, // inverted x
		{{Min: [2]float64{0, 2}, Max: [2]float64{1, 1}}}, // inverted y
	}
	for ci, items := range badCases {
		var tr RTreeG[int]
		if err := tr.Load(items); err != ErrInvalidRect {
			t.Fatalf("case %d: expected ErrInvalidRect, got %v", ci, err)
		}
		if tr.Len() != 0 || tr.base.root != nil {
			t.Fatalf("case %d: invalid input left a half-built tree", ci)
		}
	}

	// A bad item late in a large valid-looking batch must not build anything.
	rs := fixedRects(5000)
	items := rectItems(rs)
	items[len(items)-1].Min[0] = math.NaN()
	var tr RTreeG[int]
	if err := tr.Load(items); err != ErrInvalidRect {
		t.Fatalf("expected ErrInvalidRect, got %v", err)
	}
	if tr.Len() != 0 || tr.base.root != nil {
		t.Fatal("failed bulk load mutated the tree")
	}

	// Invalid load on a populated tree must leave prior contents untouched.
	good2 := loadTree(rs[:100])
	err := good2.Load([]Item[float64, int]{
		{Min: [2]float64{0, 0}, Max: [2]float64{1, 1}, Data: 999},
		{Min: [2]float64{5, 5}, Max: [2]float64{4, 4}, Data: 1000},
	})
	if err != ErrInvalidRect {
		t.Fatalf("expected ErrInvalidRect, got %v", err)
	}
	if good2.Len() != 100 {
		t.Fatalf("invalid load changed existing tree: len=%d", good2.Len())
	}
	if got := len(searchAll(good2, rs[0].min, rs[0].max)); got == 0 {
		t.Fatal("existing data disappeared after failed load")
	}

	// A valid load on a populated tree atomically replaces prior contents.
	if err := good2.Load(rectItems(rs[:5])); err != nil {
		t.Fatalf("replace load: %v", err)
	}
	if good2.Len() != 5 {
		t.Fatalf("replaced tree len=%d want 5", good2.Len())
	}
	if got := len(searchAll(good2, rs[50].min, rs[50].max)); got != 0 {
		t.Fatalf("old entries survived replacement: %d", got)
	}

	// NaN rejection also applies to the float32 generic numeric type.
	var tr32 RTreeGN[float32, int]
	err = tr32.Load([]Item[float32, int]{
		{Min: [2]float32{0, 0}, Max: [2]float32{float32(math.NaN()), 1}},
	})
	if err != ErrInvalidRect {
		t.Fatalf("float32 NaN: expected ErrInvalidRect, got %v", err)
	}
	// Integers have no NaN but inverted rects are still rejected.
	var tri RTreeGN[int, int]
	err = tri.Load([]Item[int, int]{
		{Min: [2]int{9, 0}, Max: [2]int{1, 1}},
	})
	if err != ErrInvalidRect {
		t.Fatalf("int inverted: expected ErrInvalidRect, got %v", err)
	}
}

func TestLoadCapacities(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	sizes := []int{
		0,
		1, // less than one leaf worth of entries: single-item root leaf
		2,
		maxEntries - 1,
		maxEntries,
		maxEntries + 1,
		2 * maxEntries,
		2*maxEntries + 1,
		4097, // just beyond the two-level capacity (64*64 = 4096)
		10000,
	}
	for _, n := range sizes {
		rs := make([]rect[float64], n)
		for i := range rs {
			rs[i] = randRectFrom(rng, 'm')
		}
		var tr RTreeG[int]
		if err := tr.Load(rectItems(rs)); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if tr.Len() != n {
			t.Fatalf("n=%d: len=%d", n, tr.Len())
		}
		if n == 0 {
			if tr.base.root != nil {
				t.Fatalf("n=%d: expected nil root", n)
			}
			continue
		}
		if err := bulkSane(tr.base.root); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if got := tr.base.root.deepCount(); got != n {
			t.Fatalf("n=%d: deep count=%d", n, got)
		}
		h := bulkHeight(tr.base.root)
		wantH := 0
		for cap_ := maxEntries; n > cap_; cap_ *= maxEntries {
			wantH++
		}
		if h != wantH {
			t.Fatalf("n=%d: height=%d want %d", n, h, wantH)
		}
	}
}

// bulkSane verifies node counts, matching child rectangles, non-decreasing
// branch order (equal rectangles allowed), and that every branch below the
// root holds at least two children.
func bulkSane[N numeric, T any](root *node[N, T]) error {
	var check func(n *node[N, T], isRoot bool) error
	check = func(n *node[N, T], isRoot bool) error {
		if n.count < 1 || int(n.count) > maxEntries {
			return fmt.Errorf("invalid node count %d", n.count)
		}
		if n.leaf() {
			return nil
		}
		if !isRoot && n.count < 2 {
			return fmt.Errorf("branch with %d children", n.count)
		}
		for i := 0; i < int(n.count); i++ {
			ch := n.children()[i]
			if ch == nil {
				return errors.New("nil child")
			}
			cr := ch.rect()
			if !n.rects[i].equals(&cr) {
				return errors.New("child rect mismatch")
			}
			if i > 0 && rectLessX(n.rects[i], n.rects[i-1]) {
				return errors.New("branch not ordered by min x")
			}
			if err := check(ch, false); err != nil {
				return err
			}
		}
		return nil
	}
	return check(root, true)
}

func bulkHeight[N numeric, T any](n *node[N, T]) int {
	h := 0
	for !n.leaf() {
		n = n.children()[0]
		h++
	}
	return h
}

// dumpNode renders the structural (rectangle only) shape of a tree. Trees
// built from the same multiset of rectangles in any order must render
// identically.
func dumpNode(b *strings.Builder, n *node[float64, int]) {
	b.WriteByte('(')
	for i := 0; i < int(n.count); i++ {
		r := n.rects[i]
		fmt.Fprintf(b, "%v/%v", r.min, r.max)
		if i+1 < int(n.count) {
			b.WriteByte(',')
		}
		if !n.leaf() {
			dumpNode(b, n.children()[i])
		}
	}
	b.WriteByte(')')
}

func treeDump(tr *RTreeG[int]) string {
	var b strings.Builder
	dumpNode(&b, tr.base.root)
	return b.String()
}

func TestLoadOrderIndependent(t *testing.T) {
	rs := fixedRects(20000)
	a := loadTree(rs)

	shuffled := append([]rect[float64]{}, rs...)
	rng := rand.New(rand.NewSource(7))
	rng.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	// Relabel data ids so each rectangle still carries its original id;
	// rectItems would otherwise bind ids to positions.
	items := make([]Item[float64, int], len(shuffled))
	orig := make(map[[4]float64][]int)
	for i, r := range rs {
		key := [4]float64{r.min[0], r.min[1], r.max[0], r.max[1]}
		orig[key] = append(orig[key], i)
	}
	used := make(map[[4]float64]int)
	for i, r := range shuffled {
		key := [4]float64{r.min[0], r.min[1], r.max[0], r.max[1]}
		items[i] = Item[float64, int]{
			Min: r.min, Max: r.max, Data: orig[key][used[key]],
		}
		used[key]++
	}
	var b RTreeG[int]
	if err := b.Load(items); err != nil {
		t.Fatal(err)
	}
	if da, db := treeDump(a), treeDump(&b); da != db {
		t.Fatal("bulk tree shape depends on input order")
	}
	if bulkHeight(a.base.root) != bulkHeight(b.base.root) {
		t.Fatal("height differs across shuffled loads")
	}

	// A distinct-rectangles distribution must place items identically too.
	uniq := uniqueRects(5000)
	d1 := treeDump(loadTree(uniq))
	rev := append([]rect[float64]{}, uniq...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	d2 := treeDump(loadTree(rev))
	if d1 != d2 {
		t.Fatal("unique-rect tree shape depends on input order")
	}
}

func uniqueRects(n int) []rect[float64] {
	rs := make([]rect[float64], n)
	for i := range rs {
		x := float64(i%200) * 0.9
		y := float64(i/200) * 0.9
		r := rect[float64]{
			min: [2]float64{x, y},
			max: [2]float64{x + 0.4, y + 0.4},
		}
		rs[i] = r
	}
	return rs
}

func TestLoadDuplicatesRetained(t *testing.T) {
	const copies = 5
	var rs []rect[float64]
	for i := 0; i < copies; i++ {
		rs = append(rs, rect[float64]{
			min: [2]float64{10, 10}, max: [2]float64{20, 20},
		})
	}
	rs = append(rs, fixedRects(2000)...)
	tr := loadTree(rs)
	if tr.Len() != len(rs) {
		t.Fatalf("len=%d want %d", tr.Len(), len(rs))
	}
	var got []int
	tr.Search([2]float64{10, 10}, [2]float64{20, 20},
		func(_, _ [2]float64, data int) bool {
			if data < copies {
				got = append(got, data)
			}
			return true
		})
	if len(got) != copies {
		t.Fatalf("expected %d duplicate entries, got %d", copies, len(got))
	}

	// DeleteEqual with identity eq removes only one duplicate per call.
	var count int
	tr.Scan(func(_, _ [2]float64, data int) bool {
		if data < copies {
			count++
		}
		return true
	})
	for i := 0; i < copies; i++ {
		before := tr.Len()
		tr.DeleteEqual(
			[2]float64{10, 10}, [2]float64{20, 20}, i,
			func(a, b int) bool { return a == b },
		)
		if tr.Len() != before-1 {
			t.Fatalf("delete %d removed wrong number of items", i)
		}
	}
	var remaining int
	tr.Search([2]float64{10, 10}, [2]float64{20, 20},
		func(_, _ [2]float64, data int) bool {
			if data < copies {
				remaining++
			}
			return true
		})
	if remaining != 0 {
		t.Fatalf("%d duplicates survived DeleteEqual", remaining)
	}
}

func TestLoadMatchesInsert(t *testing.T) {
	rs := fixedRects(20000)
	bulk := loadTree(rs)
	ins := insertTree(rs)

	if bulk.Len() != ins.Len() {
		t.Fatalf("len bulk=%d insert=%d", bulk.Len(), ins.Len())
	}
	if err := bulkSane(bulk.base.root); err != nil {
		t.Fatalf("bulk tree not sane: %v", err)
	}

	// Search equivalence over random windows and exact-item queries.
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 2000; i++ {
		w := randRectFrom(rng, 'r')
		a := searchAll(bulk, w.min, w.max)
		b := searchAll(ins, w.min, w.max)
		if !intsEqual(a, b) {
			t.Fatalf("window %v mismatch: %d vs %d", w, len(a), len(b))
		}
	}
	for i := 0; i < 2000; i++ {
		r := rs[rng.Intn(len(rs))]
		a := searchAll(bulk, r.min, r.max)
		b := searchAll(ins, r.min, r.max)
		if !intsEqual(a, b) {
			t.Fatalf("item query %d mismatch", i)
		}
	}

	// Nearby equivalence: same items reported in the same distance order.
	points := make([][2]float64, 500)
	for i := range points {
		points[i] = [2]float64{
			rng.Float64()*360 - 180, rng.Float64()*180 - 90,
		}
	}
	for _, pt := range points {
		seq := func(tr *RTreeG[int]) []knnResult {
			var out []knnResult
			tr.Nearby(
				BoxDist[float64, int](pt, pt, nil),
				func(_, _ [2]float64, data int, dist float64) bool {
					out = append(out, knnResult{data, dist})
					return true
				},
			)
			return out
		}
		a, b := seq(bulk), seq(ins)
		if !knnEqual(a, b) {
			t.Fatalf("nearby mismatch from %v (len %d vs %d)",
				pt, len(a), len(b))
		}
	}

	// DeleteEqual equivalence: delete every item with its exact identity;
	// both trees must drain in the same number of successful removals.
	order := rng.Perm(len(rs))
	for _, i := range order {
		r := rs[i]
		bulk.DeleteEqual(r.min, r.max, i, func(a, b int) bool { return a == b })
		ins.DeleteEqual(r.min, r.max, i, func(a, b int) bool { return a == b })
	}
	if bulk.Len() != 0 || ins.Len() != 0 {
		t.Fatalf("after deletes: bulk=%d insert=%d", bulk.Len(), ins.Len())
	}
	if bulk.base.root != nil || ins.base.root != nil {
		t.Fatal("trees should be fully drained")
	}
}

type knnResult struct {
	data int
	dist float64
}

// knnEqual compares two kNN streams. Equal distances may be reported in any
// order (rectangles at identical box distance), so ties are grouped.
func knnEqual(a, b []knnResult) bool {
	if len(a) != len(b) {
		return false
	}
	canonical := func(in []knnResult) []knnResult {
		out := append([]knnResult{}, in...)
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].dist != out[j].dist {
				return out[i].dist < out[j].dist
			}
			return out[i].data < out[j].data
		})
		return out
	}
	a, b = canonical(a), canonical(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type treeStats struct {
	height  int
	nodes   int
	overlap float64
}

func collectStats(tr *RTreeG[int]) treeStats {
	var st treeStats
	var walk func(n *node[float64, int])
	walk = func(n *node[float64, int]) {
		st.nodes++
		if !n.leaf() {
			st.overlap += pairwiseOverlap(n)
			for i := 0; i < int(n.count); i++ {
				walk(n.children()[i])
			}
		}
	}
	walk(tr.base.root)
	st.height = bulkHeight(tr.base.root)
	return st
}

// pairwiseOverlap sums the pairwise intersection area of a node's child
// rectangles, a standard packing-quality metric (lower is better).
func pairwiseOverlap(n *node[float64, int]) float64 {
	var total float64
	for i := 0; i < int(n.count); i++ {
		for j := i + 1; j < int(n.count); j++ {
			total += rectIntersectionArea(&n.rects[i], &n.rects[j])
		}
	}
	return total
}

func rectIntersectionArea(a, b *rect[float64]) float64 {
	minx := math.Max(a.min[0], b.min[0])
	miny := math.Max(a.min[1], b.min[1])
	maxx := math.Min(a.max[0], b.max[0])
	maxy := math.Min(a.max[1], b.max[1])
	if maxx <= minx || maxy <= miny {
		return 0
	}
	return (maxx - minx) * (maxy - miny)
}

func TestLoadQualityComparison(t *testing.T) {
	rs := fixedRects(100000)

	allocsLoad := testing.AllocsPerRun(3, func() {
		var tr RTreeG[int]
		if err := tr.Load(rectItems(rs)); err != nil {
			t.Fatal(err)
		}
	})
	allocsInsert := testing.AllocsPerRun(1, func() {
		var tr RTreeG[int]
		for i, r := range rs {
			tr.Insert(r.min, r.max, i)
		}
	})
	bulk := loadTree(rs)
	ins := insertTree(rs)

	sb, si := collectStats(bulk), collectStats(ins)
	t.Logf("items=%d", len(rs))
	t.Logf("%-8s %8s %14s %14s %12s",
		"method", "height", "nodes", "overlap", "allocs/op")
	t.Logf("%-8s %8d %14d %14.1f %12.1f",
		"insert", si.height, si.nodes, si.overlap, allocsInsert)
	t.Logf("%-8s %8d %14d %14.1f %12.1f",
		"bulk", sb.height, sb.nodes, sb.overlap, allocsLoad)

	if sb.height > si.height {
		t.Fatalf("bulk height %d exceeds insert height %d",
			sb.height, si.height)
	}
	if sb.overlap >= si.overlap {
		t.Fatalf("bulk overlap %.1f not lower than insert %.1f",
			sb.overlap, si.overlap)
	}
	if allocsLoad >= allocsInsert {
		t.Fatalf("bulk allocs %.0f not lower than insert %.0f",
			allocsLoad, allocsInsert)
	}
	if sb.nodes > si.nodes {
		t.Fatalf("bulk uses more nodes %d than insert %d",
			sb.nodes, si.nodes)
	}
}
