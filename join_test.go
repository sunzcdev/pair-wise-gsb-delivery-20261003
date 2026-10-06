package rtree

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

type testRect struct {
	id         int
	minX, minY float64
	maxX, maxY float64
}

func rectsHit(a, b testRect, strict bool) bool {
	if strict {
		return a.minX < b.maxX && b.minX < a.maxX &&
			a.minY < b.maxY && b.minY < a.maxY
	}
	return !(b.minX > a.maxX || b.maxX < a.minX ||
		b.minY > a.maxY || b.maxY < a.minY)
}

func buildJoinTree(rs []testRect) *RTreeG[int] {
	var tr RTreeG[int]
	for _, r := range rs {
		tr.Insert([2]float64{r.minX, r.minY}, [2]float64{r.maxX, r.maxY}, r.id)
	}
	return &tr
}

type joinPair struct {
	a, b int
}

func collectJoin(tr1, tr2 *RTreeG[int], opts JoinOptions) []joinPair {
	var pairs []joinPair
	tr1.Join(tr2, opts,
		func(min1, max1 [2]float64, d1 int, min2, max2 [2]float64, d2 int) bool {
			pairs = append(pairs, joinPair{d1, d2})
			return true
		},
	)
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].a != pairs[j].a {
			return pairs[i].a < pairs[j].a
		}
		return pairs[i].b < pairs[j].b
	})
	return pairs
}

// naiveJoin is the brute-force Cartesian-product reference. Identity is the
// slice index (one per physical entry), never the rectangle or data.
func naiveJoin(rs1, rs2 []testRect, same bool, opts JoinOptions) []joinPair {
	var pairs []joinPair
	for i, a := range rs1 {
		for j, b := range rs2 {
			if same {
				if i == j && !opts.Self {
					continue
				}
				if opts.Unique && j < i {
					continue
				}
			}
			if rectsHit(a, b, opts.Strict) {
				pairs = append(pairs, joinPair{a.id, b.id})
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].a != pairs[j].a {
			return pairs[i].a < pairs[j].a
		}
		return pairs[i].b < pairs[j].b
	})
	return pairs
}

func joinOpts() []JoinOptions {
	var opts []JoinOptions
	for _, strict := range []bool{false, true} {
		for _, unique := range []bool{false, true} {
			for _, self := range []bool{false, true} {
				opts = append(opts, JoinOptions{
					Strict: strict, Unique: unique, Self: self,
				})
			}
		}
	}
	return opts
}

func randTestRects(n int, rng *rand.Rand) []testRect {
	rs := make([]testRect, n)
	for i := range rs {
		r := randRect('m')
		rs[i] = testRect{
			id: i, minX: r.min[0], minY: r.min[1],
			maxX: r.max[0], maxY: r.max[1],
		}
	}
	return rs
}

func TestJoinRandomNaive(t *testing.T) {
	rng := rand.New(rand.NewSource(seed))
	for iter := 0; iter < 50; iter++ {
		n := rng.Intn(120) + 1
		rs1 := randTestRects(n, rng)
		rs2 := randTestRects(rng.Intn(120)+1, rng)
		tr1 := buildJoinTree(rs1)
		tr2 := buildJoinTree(rs2)
		for _, opts := range joinOpts() {
			got := collectJoin(tr1, tr2, opts)
			want := naiveJoin(rs1, rs2, false, opts)
			if !pairsEqual(got, want) {
				t.Fatalf("cross join mismatch opts=%+v:\n got %v\nwant %v",
					opts, got, want)
			}
			got = collectJoin(tr1, tr1, opts)
			want = naiveJoin(rs1, rs1, true, opts)
			if !pairsEqual(got, want) {
				t.Fatalf("self join mismatch opts=%+v:\n got %v\nwant %v",
					opts, got, want)
			}
		}
	}
}

func pairsEqual(a, b []joinPair) bool {
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

// TestJoinBoundaryContact checks two rectangles that only share an edge.
func TestJoinBoundaryContact(t *testing.T) {
	rs := []testRect{
		{id: 0, minX: 0, minY: 0, maxX: 10, maxY: 10},
		{id: 1, minX: 10, minY: 0, maxX: 20, maxY: 10},
	}
	tr := buildJoinTree(rs)
	touching := collectJoin(tr, tr, JoinOptions{Unique: true})
	if !pairsEqual(touching, []joinPair{{0, 1}}) {
		t.Fatalf("boundary contact should count as intersect, got %v", touching)
	}
	strict := collectJoin(tr, tr, JoinOptions{Unique: true, Strict: true})
	if len(strict) != 0 {
		t.Fatalf("strict join should exclude edge touch, got %v", strict)
	}
}

// TestJoinDistinctEntriesWithSameRectAndData ensures identical geometry and
// data never collapse pairs: entries are identified by their physical slots.
func TestJoinDistinctEntriesWithSameRectAndData(t *testing.T) {
	rs := []testRect{
		{id: 7, minX: 0, minY: 0, maxX: 10, maxY: 10},
		{id: 7, minX: 0, minY: 0, maxX: 10, maxY: 10},
		{id: 7, minX: 0, minY: 0, maxX: 10, maxY: 10},
	}
	tr := buildJoinTree(rs)
	// Unique + no self: the three distinct entries still yield 3 pairs.
	got := collectJoin(tr, tr, JoinOptions{Unique: true})
	if len(got) != 3 {
		t.Fatalf("expected 3 pairs among three identical entries, got %d: %v",
			len(got), got)
	}
	for _, p := range got {
		if p.a != 7 || p.b != 7 {
			t.Fatalf("all data values should be 7, got %v", p)
		}
	}
	// Including self pairs adds exactly the three (entry, entry) pairs.
	got = collectJoin(tr, tr, JoinOptions{Unique: true, Self: true})
	if len(got) != 6 {
		t.Fatalf("expected 6 pairs with self, got %d: %v", len(got), got)
	}
	// Every reported pair carries the same data, yet pairs are still
	// enumerated by physical entry; verify self identification with unique
	// data values below.
	var tr2 RTreeG[int]
	for i := 0; i < 3; i++ {
		tr2.Insert([2]float64{0, 0}, [2]float64{10, 10}, i)
	}
	selfPairs := collectJoin(&tr2, &tr2, JoinOptions{Unique: true, Self: true})
	selfCount := 0
	for _, p := range selfPairs {
		if p.a == p.b {
			selfCount++
		}
	}
	if selfCount != 3 {
		t.Fatalf("expected 3 self pairs with unique data, got %d in %v",
			selfCount, selfPairs)
	}
}

// TestJoinEarlyStop verifies returning false from the callback halts the
// traversal and does not corrupt the tree.
func TestJoinEarlyStop(t *testing.T) {
	rs := make([]testRect, 40)
	for i := range rs {
		rs[i] = testRect{
			id: i, minX: float64(i), minY: 0,
			maxX: float64(i) + 2, maxY: 2,
		}
	}
	tr := buildJoinTree(rs)
	var n int
	tr.Join(tr, JoinOptions{},
		func(min1, max1 [2]float64, d1 int, min2, max2 [2]float64, d2 int) bool {
			n++
			return n < 5
		},
	)
	if n != 5 {
		t.Fatalf("expected exactly 5 callbacks, got %d", n)
	}
	if tr.Len() != len(rs) {
		t.Fatalf("tree length changed after stopped join: %d", tr.Len())
	}
	full := collectJoin(tr, tr, JoinOptions{Unique: true})
	want := naiveJoin(rs, rs, true, JoinOptions{Unique: true})
	if !pairsEqual(full, want) {
		t.Fatal("tree content changed after stopped join")
	}
}

// TestJoinCopySnapshot verifies that a join on a copied snapshot is not
// affected by later inserts into the original tree.
func TestJoinCopySnapshot(t *testing.T) {
	rs1 := []testRect{{id: 0, minX: 0, minY: 0, maxX: 5, maxY: 5}}
	tr := buildJoinTree(rs1)
	snap := tr.Copy()
	tr.Insert([2]float64{4, 4}, [2]float64{6, 6}, 1)
	before := collectJoin(snap, snap, JoinOptions{Unique: true, Self: true})
	// Snapshot holds one entry: only its self pair survives.
	if !pairsEqual(before, []joinPair{{0, 0}}) {
		t.Fatalf("snapshot should only contain original entry, got %v", before)
	}
	if snap.Len() != 1 || tr.Len() != 2 {
		t.Fatalf("unexpected lengths snap=%d tr=%d", snap.Len(), tr.Len())
	}
}

// joinWithVisits runs the internal join core while counting node-pair visits.
func joinWithVisits(tr1, tr2 *RTreeG[int], opts JoinOptions) ([]joinPair, int) {
	var pairs []joinPair
	var visits int
	hit := func(a, b *rect[float64]) bool {
		if opts.Strict {
			return a.overlaps(b)
		}
		return a.intersects(b)
	}
	joinNodes(tr1.base.root, tr2.base.root,
		tr1 == tr2, tr1 == tr2 && opts.Unique, opts.Self, hit,
		func(min1, max1 [2]float64, d1 int,
			min2, max2 [2]float64, d2 int,
		) bool {
			pairs = append(pairs, joinPair{d1, d2})
			return true
		}, &visits)
	return pairs, visits
}

// TestJoinSeparatedClustersNodeVisits records node visits and asserts that
// separated clusters are pruned at node level rather than leaf level.
func TestJoinSeparatedClustersNodeVisits(t *testing.T) {
	const perCluster = 80
	var clusterA, clusterB []testRect
	for i := 0; i < perCluster; i++ {
		clusterA = append(clusterA, testRect{
			id: i, minX: float64(i % 8), minY: float64(i / 8),
			maxX: float64(i%8) + 0.9, maxY: float64(i/8) + 0.9,
		})
		clusterB = append(clusterB, testRect{
			id: perCluster + i, minX: 1000 + float64(i%8),
			minY: float64(i / 8),
			maxX: 1000 + float64(i%8) + 0.9,
			maxY: float64(i/8) + 0.9,
		})
	}
	trA := buildJoinTree(clusterA)
	trB := buildJoinTree(clusterB)
	pairs, visits := joinWithVisits(trA, trB, JoinOptions{})
	if len(pairs) != 0 {
		t.Fatalf("expected no intersecting pairs across separated trees, got %d",
			len(pairs))
	}
	if visits != 1 {
		t.Fatalf("expected root-pair pruning (1 node-pair visit), got %d",
			visits)
	}

	// A single tree holding both separated clusters: during its self join the
	// two cluster subtrees must never be paired together.
	rs := append(append([]testRect{}, clusterA...), clusterB...)
	tr1 := buildJoinTree(rs)
	// Self join: branches inside each cluster must be explored, but the two
	// top-level cluster branches never pair with each other.
	selfPairs, selfVisits := joinWithVisits(tr1, tr1, JoinOptions{Unique: true})
	naive := naiveJoin(rs, rs, true, JoinOptions{Unique: true})
	if !pairsEqual(selfPairs, naive) {
		t.Fatalf("self join pairs mismatch: got %d want %d",
			len(selfPairs), len(naive))
	}
	if len(selfPairs) != 0 {
		t.Fatalf("non-overlapping grid entries should produce no pairs, got %d",
			len(selfPairs))
	}
	t.Logf("separated-cluster self join: %d entries, %d pairs, %d node visits",
		len(rs), len(selfPairs), selfVisits)

	// Overlapping clusters must explore far more node pairs, confirming the
	// counter measures actual traversal rather than staying at the root.
	var dense []testRect
	for i := 0; i < len(rs); i++ {
		dense = append(dense, testRect{
			id: i, minX: float64(i%16) * 0.5,
			minY: float64(i/16) * 0.5,
			maxX: float64(i%16)*0.5 + 0.75,
			maxY: float64(i/16)*0.5 + 0.75,
		})
	}
	denseTree := buildJoinTree(dense)
	_, denseVisits := joinWithVisits(denseTree, denseTree,
		JoinOptions{Unique: true})
	if denseVisits <= selfVisits {
		t.Fatalf("dense join should visit more node pairs than separated "+
			"clusters: dense=%d separated=%d", denseVisits, selfVisits)
	}
}

// TestJoinCrossDataTypes joins trees with different data types through the
// generic free function.
func TestJoinCrossDataTypes(t *testing.T) {
	var tr1 RTreeG[int]
	var tr2 RTreeG[string]
	tr1.Insert([2]float64{0, 0}, [2]float64{10, 10}, 42)
	tr2.Insert([2]float64{5, 5}, [2]float64{15, 15}, "warehouse")
	tr2.Insert([2]float64{100, 100}, [2]float64{101, 101}, "remote")
	var got []string
	JoinG(&tr1, &tr2, JoinOptions{},
		func(min1, max1 [2]float64, d1 int,
			min2, max2 [2]float64, d2 string,
		) bool {
			got = append(got, fmt.Sprintf("%d-%s", d1, d2))
			return true
		},
	)
	if len(got) != 1 || got[0] != "42-warehouse" {
		t.Fatalf("unexpected cross-type pairs: %v", got)
	}
}

// TestJoinFloat32 exercises the numeric-generic core with a non-float64
// coordinate type.
func TestJoinFloat32(t *testing.T) {
	var tr RTreeGN[float32, int]
	tr.Insert([2]float32{0, 0}, [2]float32{10, 10}, 1)
	tr.Insert([2]float32{10, 0}, [2]float32{20, 10}, 2)
	var n int
	tr.Join(&tr, JoinOptions{Unique: true, Strict: true},
		func(min1, max1 [2]float32, d1 int,
			min2, max2 [2]float32, d2 int,
		) bool {
			n++
			return true
		},
	)
	if n != 0 {
		t.Fatalf("strict float32 join should exclude edge touch, got %d", n)
	}
	tr.Join(&tr, JoinOptions{Unique: true},
		func(min1, max1 [2]float32, d1 int,
			min2, max2 [2]float32, d2 int,
		) bool {
			n++
			return true
		},
	)
	if n != 1 {
		t.Fatalf("float32 boundary join expected one pair, got %d", n)
	}
}

// TestJoinRTreeAndEmpty exercises the non-generic wrapper and nil/empty
// trees, which must produce no pairs and never panic.
func TestJoinRTreeAndEmpty(t *testing.T) {
	var tr1, tr2 RTree
	tr1.Insert([2]float64{0, 0}, [2]float64{10, 10}, "a")
	tr2.Insert([2]float64{5, 5}, [2]float64{15, 15}, "b")
	tr2.Insert([2]float64{100, 100}, [2]float64{101, 101}, "c")
	var pairs []joinPair
	tr1.Join(&tr2, JoinOptions{},
		func(min1, max1 [2]float64, d1 any,
			min2, max2 [2]float64, d2 any,
		) bool {
			pairs = append(pairs, joinPair{
				int(d1.(string)[0] - 'a'),
				int(d2.(string)[0] - 'a'),
			})
			return true
		},
	)
	if len(pairs) != 1 || pairs[0] != (joinPair{0, 1}) {
		t.Fatalf("unexpected RTree pairs: %v", pairs)
	}

	var empty RTree
	called := false
	empty.Join(&tr1, JoinOptions{},
		func(min1, max1 [2]float64, d1 any,
			min2, max2 [2]float64, d2 any,
		) bool {
			called = true
			return true
		},
	)
	tr1.Join(nil, JoinOptions{},
		func(min1, max1 [2]float64, d1 any,
			min2, max2 [2]float64, d2 any,
		) bool {
			called = true
			return true
		},
	)
	if called {
		t.Fatal("nil/empty joins must not invoke the callback")
	}
}

// TestJoinPointSelfStrict checks that a zero-area point paired with itself is
// excluded under Strict even when Self is requested.
func TestJoinPointSelfStrict(t *testing.T) {
	var tr RTreeG[int]
	tr.Insert([2]float64{3, 3}, [2]float64{3, 3}, 0)
	if got := collectJoin(&tr, &tr, JoinOptions{Self: true, Strict: true}); len(got) != 0 {
		t.Fatalf("point self pair must be excluded under Strict, got %v", got)
	}
	if got := collectJoin(&tr, &tr, JoinOptions{Self: true}); !pairsEqual(got, []joinPair{{0, 0}}) {
		t.Fatalf("point self pair expected without Strict, got %v", got)
	}
}
