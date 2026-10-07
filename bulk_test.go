// Copyright 2021 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package rtree

import (
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"runtime/debug"
	"sort"
	"testing"
)

// fixedBulkRects generates a fixed distribution of mixed points and small
// rectangles. Every 50th entry shares its rectangle with the previous entry,
// so duplicate rectangles with distinct data values are present.
func fixedBulkRects(n int, seed int64) []EntryG[int] {
	rng := rand.New(rand.NewSource(seed))
	entries := make([]EntryG[int], n)
	for i := 0; i < n; i++ {
		min := [2]float64{rng.Float64()*360 - 180, rng.Float64()*180 - 90}
		max := min
		if rng.Intn(2) == 0 {
			max[0] += rng.Float64() * 0.5
			max[1] += rng.Float64() * 0.25
		}
		entries[i] = EntryG[int]{Min: min, Max: max, Data: i}
		if i > 0 && i%50 == 0 {
			entries[i].Min = entries[i-1].Min
			entries[i].Max = entries[i-1].Max
		}
	}
	return entries
}

func TestLoadValidation(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		var tr RTreeG[int]
		if err := tr.Load(nil); err != nil {
			t.Fatalf("empty load: %v", err)
		}
		if tr.Len() != 0 || tr.base.root != nil {
			t.Fatalf("empty load should be a no-op")
		}
		// Loading on the empty tree remains possible afterwards.
		if err := tr.Load([]EntryG[int]{
			{Min: [2]float64{0, 0}, Max: [2]float64{1, 1}, Data: 1},
		}); err != nil {
			t.Fatalf("load after empty: %v", err)
		}
	})

	cases := []struct {
		name string
		min  [2]float64
		max  [2]float64
	}{
		{"NaNMinX", [2]float64{math.NaN(), 0}, [2]float64{1, 1}},
		{"NaNMinY", [2]float64{0, math.NaN()}, [2]float64{1, 1}},
		{"NaNMaxX", [2]float64{0, 0}, [2]float64{math.NaN(), 1}},
		{"NaNMaxY", [2]float64{0, 0}, [2]float64{1, math.NaN()}},
		{"InvertedX", [2]float64{2, 0}, [2]float64{1, 1}},
		{"InvertedY", [2]float64{0, 2}, [2]float64{1, 1}},
		{"BothInverted", [2]float64{2, 2}, [2]float64{1, 1}},
	}
	for _, bad := range cases {
		t.Run(bad.name, func(t *testing.T) {
			entries := []EntryG[int]{
				{Min: [2]float64{0, 0}, Max: [2]float64{1, 1}, Data: 0},
				{Min: [2]float64{2, 2}, Max: [2]float64{3, 3}, Data: 1},
				{Min: bad.min, Max: bad.max, Data: 2},
				{Min: [2]float64{4, 4}, Max: [2]float64{5, 5}, Data: 3},
			}
			var tr RTreeG[int]
			if err := tr.Load(entries); err != ErrInvalidRect {
				t.Fatalf("expected ErrInvalidRect, got %v", err)
			}
			// No half-built tree may remain.
			if tr.Len() != 0 || tr.base.root != nil {
				t.Fatalf("invalid entries left a partial tree")
			}
			if min, max := tr.Bounds(); min != [2]float64{} || max != [2]float64{} {
				t.Fatalf("invalid entries changed bounds: %v %v", min, max)
			}
			// Valid entries can still be loaded after a rejected batch.
			if err := tr.Load(entries[:2]); err != nil {
				t.Fatalf("load after rejected batch: %v", err)
			}
			if tr.Len() != 2 {
				t.Fatalf("expected 2 items, got %d", tr.Len())
			}
		})
	}

	t.Run("Float32NaN", func(t *testing.T) {
		var tr RTreeGN[float32, int]
		err := tr.Load([]Entry[float32, int]{
			{Min: [2]float32{0, 0}, Max: [2]float32{float32(math.NaN()), 1}},
		})
		if err != ErrInvalidRect {
			t.Fatalf("expected ErrInvalidRect, got %v", err)
		}
	})

	t.Run("IntegerInverted", func(t *testing.T) {
		var tr RTreeGN[int, int]
		err := tr.Load([]Entry[int, int]{
			{Min: [2]int{5, 0}, Max: [2]int{1, 1}, Data: 0},
			{Min: [2]int{-3, -3}, Max: [2]int{-1, -1}, Data: 1},
		})
		if err != ErrInvalidRect {
			t.Fatalf("expected ErrInvalidRect, got %v", err)
		}
	})

	t.Run("NotEmpty", func(t *testing.T) {
		var tr RTreeG[int]
		tr.Insert([2]float64{0, 0}, [2]float64{1, 1}, 0)
		err := tr.Load([]EntryG[int]{
			{Min: [2]float64{2, 2}, Max: [2]float64{3, 3}, Data: 1},
		})
		if err != ErrNotEmpty {
			t.Fatalf("expected ErrNotEmpty, got %v", err)
		}
		if tr.Len() != 1 {
			t.Fatalf("rejected load changed item count")
		}
	})
}

func TestLoadSizes(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	sizes := []int{0, 1, 2, 3, 63, 64, 65, 128, 4095, 4096, 4097,
		262144, 262145}
	for _, n := range sizes {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			entries := make([]EntryG[int], n)
			for i := range entries {
				min := [2]float64{rng.Float64() * 100, rng.Float64() * 100}
				max := [2]float64{min[0] + 1, min[1] + 1}
				entries[i] = EntryG[int]{Min: min, Max: max, Data: i}
			}
			var tr RTreeG[int]
			if err := tr.Load(entries); err != nil {
				t.Fatalf("load: %v", err)
			}
			if tr.Len() != n {
				t.Fatalf("expected %d items, got %d", n, tr.Len())
			}
			if err := bulkSane(&tr); err != nil {
				t.Fatalf("tree not sane: %v", err)
			}
			// Every entry must be present exactly once.
			seen := make(map[int]int, n)
			tr.Scan(func(min, max [2]float64, data int) bool {
				seen[data]++
				return true
			})
			if len(seen) != n {
				t.Fatalf("expected %d distinct items, got %d", n, len(seen))
			}
			for id, count := range seen {
				if count != 1 {
					t.Fatalf("item %d appeared %d times", id, count)
				}
			}
		})
	}
}

func TestLoadDuplicates(t *testing.T) {
	// All entries share the exact same rectangle but carry distinct data.
	const n = 200
	entries := make([]EntryG[int], n)
	rect := EntryG[int]{
		Min: [2]float64{-10, -10},
		Max: [2]float64{10, 10},
	}
	for i := range entries {
		entries[i] = rect
		entries[i].Data = i
	}
	var tr RTreeG[int]
	if err := tr.Load(entries); err != nil {
		t.Fatalf("load: %v", err)
	}
	if tr.Len() != n {
		t.Fatalf("expected %d duplicate items, got %d", n, tr.Len())
	}
	if err := bulkSane(&tr); err != nil {
		t.Fatalf("duplicate tree not sane: %v", err)
	}

	// Searching the shared rectangle must return every duplicate entry.
	var got []int
	tr.Search([2]float64{-10, -10}, [2]float64{10, 10},
		func(min, max [2]float64, data int) bool {
			got = append(got, data)
			return true
		})
	sort.Ints(got)
	if len(got) != n {
		t.Fatalf("expected %d search hits, got %d", n, len(got))
	}
	for i := 0; i < n; i++ {
		if got[i] != i {
			t.Fatalf("duplicate entry %d missing from search", i)
		}
	}

	// DeleteEqual with an identity predicate removes exactly one duplicate
	// per call and never removes a sibling entry.
	for i := 0; i < n; i++ {
		before := tr.Len()
		tr.DeleteEqual(rect.Min, rect.Max, i, func(a, b int) bool { return a == b })
		if tr.Len() != before-1 {
			t.Fatalf("delete %d removed the wrong number of items", i)
		}
	}
	if tr.Len() != 0 {
		t.Fatalf("expected empty tree, got %d", tr.Len())
	}
}

// bulkSane validates a bulk-loaded tree. Unlike rSane it tolerates tied
// minimum-x values on branch children, which occur naturally when duplicate
// or heavily overlapping rectangles are packed together.
func bulkSane(tr *RTreeG[int]) error {
	if tr.Len() == 0 {
		if tr.base.root != nil {
			return fmt.Errorf("non-nil root for empty tree")
		}
		return nil
	}
	if tr.base.root == nil {
		return fmt.Errorf("nil root for non-empty tree")
	}
	var height int
	n := tr.base.root
	for !n.leaf() {
		height++
		n = n.children()[0]
	}
	return bulkSaneNode(tr.base.rect, tr.base.root, height)
}

func bulkSaneNode(r rect[float64], n *node[float64, int], height int) error {
	if n.count < 1 || int(n.count) > maxEntries {
		return fmt.Errorf("invalid child count %d", n.count)
	}
	if n.leaf() == (height != 0) {
		return fmt.Errorf("leaf/branch mismatch at height %d", height)
	}
	if got := n.rect(); !r.equals(&got) {
		return fmt.Errorf("node rect %v does not match parent slot %v", got, r)
	}
	for i := 0; i < int(n.count); i++ {
		cr := n.rects[i]
		if math.IsNaN(cr.min[0]) || math.IsNaN(cr.min[1]) ||
			math.IsNaN(cr.max[0]) || math.IsNaN(cr.max[1]) ||
			cr.min[0] > cr.max[0] || cr.min[1] > cr.max[1] {
			return fmt.Errorf("invalid child rect %v", cr)
		}
		if !r.contains(&cr) {
			return fmt.Errorf("child rect %v outside parent %v", cr, r)
		}
		if !n.leaf() {
			child := n.children()[i]
			if child == nil {
				return fmt.Errorf("nil child at slot %d", i)
			}
			if err := bulkSaneNode(cr, child, height-1); err != nil {
				return err
			}
		}
	}
	return nil
}

func insertEntries(entries []EntryG[int]) *RTreeG[int] {
	var tr RTreeG[int]
	for _, e := range entries {
		tr.Insert(e.Min, e.Max, e.Data)
	}
	return &tr
}

func bulkEntries(entries []EntryG[int]) *RTreeG[int] {
	var tr RTreeG[int]
	if err := tr.Load(entries); err != nil {
		panic(err)
	}
	return &tr
}

// compareSearch runs many rectangle queries against both trees and verifies
// they return the same data items (order independent).
func compareSearch(t *testing.T, bulk, insert *RTreeG[int], rng *rand.Rand) {
	t.Helper()
	for q := 0; q < 500; q++ {
		lo := [2]float64{rng.Float64()*360 - 180, rng.Float64()*180 - 90}
		hi := [2]float64{
			lo[0] + rng.Float64()*40,
			lo[1] + rng.Float64()*20,
		}
		var a, b []int
		bulk.Search(lo, hi, func(min, max [2]float64, data int) bool {
			a = append(a, data)
			return true
		})
		insert.Search(lo, hi, func(min, max [2]float64, data int) bool {
			b = append(b, data)
			return true
		})
		sort.Ints(a)
		sort.Ints(b)
		if len(a) != len(b) {
			t.Fatalf("query %v %v: %d hits vs %d", lo, hi, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("query %v %v: hit mismatch at %d", lo, hi, i)
			}
		}
	}
}

// compareNearby verifies both trees return the same (data, distance) pairs in
// non-decreasing distance order. Items at equal distances (common for
// duplicate rectangles) may be visited in any order.
func compareNearby(t *testing.T, bulk, insert *RTreeG[int], rng *rand.Rand) {
	t.Helper()
	type hit struct {
		data int
		dist float64
	}
	run := func(tr *RTreeG[int], target [2]float64) []hit {
		var hits []hit
		dist := BoxDist[float64, int](target, target, nil)
		tr.Nearby(dist, func(min, max [2]float64, data int, d float64) bool {
			hits = append(hits, hit{data, d})
			return true
		})
		return hits
	}
	for q := 0; q < 100; q++ {
		target := [2]float64{rng.Float64()*360 - 180, rng.Float64()*180 - 90}
		a := run(bulk, target)
		b := run(insert, target)
		if len(a) != len(b) {
			t.Fatalf("nearby %v: %d hits vs %d", target, len(a), len(b))
		}
		for i := 1; i < len(a); i++ {
			if a[i].dist < a[i-1].dist {
				t.Fatalf("bulk nearby %v: distances not sorted", target)
			}
			if b[i].dist < b[i-1].dist {
				t.Fatalf("insert nearby %v: distances not sorted", target)
			}
		}
		key := func(h []hit) func(int) string {
			return func(i int) string {
				return fmt.Sprintf("%d:%.17g", h[i].data, h[i].dist)
			}
		}
		ka, kb := key(a), key(b)
		sa := make([]string, len(a))
		sb := make([]string, len(b))
		for i := range a {
			sa[i] = ka(i)
			sb[i] = kb(i)
		}
		sort.Strings(sa)
		sort.Strings(sb)
		for i := range sa {
			if sa[i] != sb[i] {
				t.Fatalf("nearby %v: hit set differs at %d: %s vs %s",
					target, i, sa[i], sb[i])
			}
		}
	}
}

func TestLoadParity(t *testing.T) {
	const n = 100_000
	entries := fixedBulkRects(n, 20240517)

	bulk := bulkEntries(entries)
	insert := insertEntries(entries)

	if bulk.Len() != n || insert.Len() != n {
		t.Fatalf("len: %d vs %d", bulk.Len(), insert.Len())
	}
	if err := bulkSane(bulk); err != nil {
		t.Fatalf("bulk tree not sane: %v", err)
	}
	bmin, bmax := bulk.Bounds()
	imin, imax := insert.Bounds()
	if bmin != imin || bmax != imax {
		t.Fatalf("bounds differ: %v %v vs %v %v", bmin, bmax, imin, imax)
	}

	rng := rand.New(rand.NewSource(777))
	t.Run("Search", func(t *testing.T) {
		compareSearch(t, bulk, insert, rng)
	})
	t.Run("Nearby", func(t *testing.T) {
		compareNearby(t, bulk, insert, rng)
	})

	t.Run("DeleteEqual", func(t *testing.T) {
		// Remove every third item from each tree using DeleteEqual, then
		// confirm both trees answer identically and remain valid.
		for i := 0; i < n; i += 3 {
			e := entries[i]
			bulk.DeleteEqual(e.Min, e.Max, i, func(a, b int) bool { return a == b })
			insert.DeleteEqual(e.Min, e.Max, i, func(a, b int) bool { return a == b })
		}
		want := n - (n+2)/3
		if bulk.Len() != want || insert.Len() != want {
			t.Fatalf("len after delete: %d vs %d, want %d",
				bulk.Len(), insert.Len(), want)
		}
		if err := bulkSane(bulk); err != nil {
			t.Fatalf("bulk tree not sane after deletes: %v", err)
		}
		rng := rand.New(rand.NewSource(909))
		compareSearch(t, bulk, insert, rng)
		compareNearby(t, bulk, insert, rng)

		// The surviving item sets must be identical, including the duplicate
		// rectangles whose deleted siblings shared their rectangle.
		got := make(map[int]bool)
		bulk.Scan(func(min, max [2]float64, data int) bool {
			got[data] = true
			return true
		})
		for i := 0; i < n; i++ {
			_, alive := got[i]
			wantAlive := i%3 != 0
			if alive != wantAlive {
				t.Fatalf("item %d presence: got %v want %v", i, alive, wantAlive)
			}
		}
	})
}

func TestLoadOrderIndependent(t *testing.T) {
	const n = 20_000
	entries := fixedBulkRects(n, 55512)
	a := bulkEntries(entries)

	shuffled := append([]EntryG[int]{}, entries...)
	rng := rand.New(rand.NewSource(12321))
	rng.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	b := bulkEntries(shuffled)

	sa := treeShape(a)
	sb := treeShape(b)
	if len(sa) != len(sb) {
		t.Fatalf("shape depth differs: %d vs %d", len(sa), len(sb))
	}
	for level := range sa {
		if len(sa[level]) != len(sb[level]) {
			t.Fatalf("level %d node count differs: %d vs %d",
				level, len(sa[level]), len(sb[level]))
		}
		for i := range sa[level] {
			if sa[level][i] != sb[level][i] {
				t.Fatalf("level %d node %d differs:\n%v\nvs\n%v",
					level, i, sa[level][i], sb[level][i])
			}
		}
	}
}

func TestLoadCopyOnWrite(t *testing.T) {
	entries := fixedBulkRects(10_000, 31337)
	base := bulkEntries(entries)

	cp := base.Copy()
	// Mutating the copy via Insert and Delete must not change the original
	// bulk-loaded tree.
	cp.Insert([2]float64{-179, -89}, [2]float64{-178, -88}, 1_000_000)
	if cp.Len() != base.Len()+1 {
		t.Fatalf("copy insert: %d vs %d", cp.Len(), base.Len())
	}
	cp.Delete(entries[10].Min, entries[10].Max, 10)
	if cp.Len() != base.Len() {
		t.Fatalf("copy delete: %d vs %d", cp.Len(), base.Len())
	}

	if base.Len() != len(entries) {
		t.Fatalf("original tree changed: %d", base.Len())
	}
	got := 0
	base.Search(entries[10].Min, entries[10].Max,
		func(min, max [2]float64, data int) bool {
			if data == 10 {
				got++
			}
			return true
		})
	if got == 0 {
		t.Fatalf("item deleted from original through copy")
	}
	var extra int
	base.Search([2]float64{-179, -89}, [2]float64{-178, -88},
		func(min, max [2]float64, data int) bool {
			if data == 1_000_000 {
				extra++
			}
			return true
		})
	if extra != 0 {
		t.Fatalf("copy insert leaked into original tree")
	}
	if err := bulkSane(base); err != nil {
		t.Fatalf("original not sane: %v", err)
	}
	if err := bulkSane(cp); err != nil {
		t.Fatalf("copy not sane: %v", err)
	}

	// The original tree must remain mutable after the bulk build.
	base.Insert([2]float64{170, 80}, [2]float64{171, 81}, 2_000_000)
	found := false
	base.Search([2]float64{170, 80}, [2]float64{171, 81},
		func(min, max [2]float64, data int) bool {
			if data == 2_000_000 {
				found = true
			}
			return true
		})
	if !found {
		t.Fatalf("post-load insert not searchable")
	}
	if err := bulkSane(base); err != nil {
		t.Fatalf("original not sane after post-load insert: %v", err)
	}
}

func TestLoadInterface(t *testing.T) {
	// Exercises the non-generic RTree.Load wrapper.
	entries := []EntryG[any]{
		{Min: [2]float64{0, 0}, Max: [2]float64{1, 1}, Data: "a"},
		{Min: [2]float64{2, 2}, Max: [2]float64{3, 3}, Data: "b"},
		{Min: [2]float64{0.5, 0.5}, Max: [2]float64{2.5, 2.5}, Data: "c"},
	}
	var tr RTree
	if err := tr.Load(entries); err != nil {
		t.Fatalf("load: %v", err)
	}
	if tr.Len() != 3 {
		t.Fatalf("expected 3 items, got %d", tr.Len())
	}
	var got []string
	tr.Search([2]float64{0.9, 0.9}, [2]float64{1.9, 1.9},
		func(min, max [2]float64, data any) bool {
			got = append(got, data.(string))
			return true
		})
	sort.Strings(got)
	if fmt.Sprint(got) != "[a c]" {
		t.Fatalf("expected [a c], got %v", got)
	}
}

// treeShape returns the sorted bounding rectangles of every node per level,
// level 0 being the root. Equal rectangles (from duplicate inputs) remain
// repeated so node count is part of the signature.
func treeShape(tr *RTreeG[int]) [][]rect[float64] {
	var levels [][]rect[float64]
	var walk func(n *node[float64, int], depth int)
	walk = func(n *node[float64, int], depth int) {
		for depth >= len(levels) {
			levels = append(levels, nil)
		}
		levels[depth] = append(levels[depth], n.rect())
		if !n.leaf() {
			for _, child := range n.children()[:n.count] {
				walk(child, depth+1)
			}
		}
	}
	if tr.base.root != nil {
		walk(tr.base.root, 0)
	}
	for _, level := range levels {
		sort.Slice(level, func(i, j int) bool {
			return rectCenterLessAxis(level[i], level[j], 0)
		})
	}
	return levels
}

type treeStats struct {
	height  int // root-to-leaf edges; a single leaf is height 0
	nodes   int // total node allocations (leaves + branches)
	leaves  int
	overlap float64 // summed pairwise overlap area inside branch nodes
}

func gatherStats(tr *RTreeG[int]) treeStats {
	var s treeStats
	var walk func(n *node[float64, int]) int
	walk = func(n *node[float64, int]) int {
		s.nodes++
		if n.leaf() {
			s.leaves++
			return 0
		}
		rects := n.rects[:n.count]
		for i := 0; i < len(rects); i++ {
			for j := i + 1; j < len(rects); j++ {
				s.overlap += rectOverlapArea(rects[i], rects[j])
			}
		}
		depth := 0
		for _, child := range n.children()[:n.count] {
			if d := walk(child); d > depth {
				depth = d
			}
		}
		return depth + 1
	}
	if tr.base.root != nil {
		s.height = walk(tr.base.root)
	}
	return s
}

func rectOverlapArea(a, b rect[float64]) float64 {
	minx := math.Max(a.min[0], b.min[0])
	maxx := math.Min(a.max[0], b.max[0])
	miny := math.Max(a.min[1], b.min[1])
	maxy := math.Min(a.max[1], b.max[1])
	if maxx <= minx || maxy <= miny {
		return 0
	}
	return (maxx - minx) * (maxy - miny)
}

func measureAlloc(entries []EntryG[int], bulkLoad bool) (allocs, bytes uint64) {
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	runtime.GC()
	var m1, m2 runtime.MemStats
	runtime.ReadMemStats(&m1)
	if bulkLoad {
		_ = bulkEntries(entries)
	} else {
		_ = insertEntries(entries)
	}
	runtime.ReadMemStats(&m2)
	return m2.Mallocs - m1.Mallocs, m2.TotalAlloc - m1.TotalAlloc
}

func TestLoadMetrics(t *testing.T) {
	const n = 100_000
	// Distinct rectangles only: spatial packing quality is what is being
	// measured here.
	rng := rand.New(rand.NewSource(987654321))
	entries := make([]EntryG[int], n)
	for i := range entries {
		min := [2]float64{rng.Float64()*360 - 180, rng.Float64()*180 - 90}
		max := [2]float64{min[0] + rng.Float64()*0.5, min[1] + rng.Float64()*0.25}
		entries[i] = EntryG[int]{Min: min, Max: max, Data: i}
	}

	bulk := bulkEntries(entries)
	insert := insertEntries(entries)
	sb := gatherStats(bulk)
	si := gatherStats(insert)
	ab, bb := measureAlloc(entries, true)
	ai, bi := measureAlloc(entries, false)

	fmt.Printf("\n== bulk load vs insert (%d fixed-distribution rects) ==\n", n)
	fmt.Printf("%-18s %12s %12s\n", "metric", "bulk", "insert")
	fmt.Printf("%-18s %12d %12d\n", "height", sb.height, si.height)
	fmt.Printf("%-18s %12d %12d\n", "nodes", sb.nodes, si.nodes)
	fmt.Printf("%-18s %12d %12d\n", "leaves", sb.leaves, si.leaves)
	fmt.Printf("%-18s %12.1f %12.1f\n", "overlap-area", sb.overlap, si.overlap)
	fmt.Printf("%-18s %12d %12d\n", "allocations", ab, ai)
	fmt.Printf("%-18s %12d %12d\n", "bytes-alloc", bb, bi)

	if sb.height > si.height {
		t.Fatalf("bulk height %d exceeds insert height %d", sb.height, si.height)
	}
	if sb.nodes > si.nodes {
		t.Fatalf("bulk nodes %d exceed insert nodes %d", sb.nodes, si.nodes)
	}
	if sb.overlap > si.overlap {
		t.Fatalf("bulk overlap %.1f exceeds insert overlap %.1f",
			sb.overlap, si.overlap)
	}
	if ab >= ai {
		t.Fatalf("bulk allocs %d not fewer than insert allocs %d", ab, ai)
	}
}
