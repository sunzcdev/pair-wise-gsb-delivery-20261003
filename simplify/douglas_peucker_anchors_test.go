package simplify_test

import (
	"math"
	"math/rand"
	"reflect"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/planar"
	"github.com/paulmach/orb/simplify"
)

// assertAnchorResult verifies the invariants required of a protected-vertex
// simplification:
//   - the index map aligns 1:1 with the output points,
//   - every map entry points at the exact output coordinate,
//   - the map is strictly increasing,
//   - both end points survive and their indices are preserved,
//   - every anchor survives at its original index,
//   - every removed vertex lies within the threshold of the output segment
//     that spans the gap it was removed from.
func assertAnchorResult(
	t *testing.T,
	original, out orb.LineString,
	indexMap []int,
	anchors []int,
	threshold float64,
	closed bool,
) {
	t.Helper()

	if len(out) != len(indexMap) {
		t.Fatalf("output length %d != index map length %d", len(out), len(indexMap))
	}

	n := len(original)
	for k, idx := range indexMap {
		if idx < 0 || idx >= n {
			t.Fatalf("index map entry out of range: %d", idx)
		}
		if k > 0 && idx <= indexMap[k-1] {
			t.Fatalf("index map not strictly increasing: %v", indexMap)
		}
		if out[k] != original[idx] {
			t.Fatalf("output point %v does not match original index %d (%v)", out[k], idx, original[idx])
		}
	}

	if len(indexMap) == 0 || indexMap[0] != 0 || indexMap[len(indexMap)-1] != n-1 {
		t.Fatalf("end points must be preserved, got map %v for %d input points", indexMap, n)
	}

	kept := make(map[int]bool, len(indexMap))
	for _, idx := range indexMap {
		kept[idx] = true
	}
	for _, a := range anchors {
		if !kept[a] {
			t.Fatalf("anchor %d was removed; map=%v", a, indexMap)
		}
	}

	if closed && out[0] != out[len(out)-1] {
		t.Fatalf("closed ring must stay closed: %v", out)
	}

	limit := threshold*threshold + 1e-9
	for k := 0; k+1 < len(indexMap); k++ {
		lo, hi := indexMap[k], indexMap[k+1]
		for j := lo + 1; j < hi; j++ {
			d2 := planar.DistanceFromSegmentSquared(out[k], out[k+1], original[j])
			if d2 > limit {
				t.Fatalf("removed point %d (%v) is %v from output segment %v->%v, threshold %v",
					j, original[j], math.Sqrt(d2), out[k], out[k+1], threshold)
			}
		}
	}
}

func TestLineStringWithAnchorsCollinear(t *testing.T) {
	// Index 1 sits exactly on the start->end axis and would otherwise be
	// removed; marking it as an anchor forces it to survive.
	ls := orb.LineString{{0, 0}, {1, 0}, {2, 0}, {3, 1}, {4, 0}, {5, 0}, {6, 0}}

	out, indexMap := simplify.DouglasPeucker(0.5).LineStringWithAnchors(ls.Clone(), []int{1})

	// After keeping index 1, the first segment is straight. From index 1 the
	// point at index 3 sticks out, and index 4 is far enough off the 3->6
	// chord (0.6 > 0.5) to be kept as well.
	expected := orb.LineString{{0, 0}, {1, 0}, {3, 1}, {4, 0}, {6, 0}}
	if !out.Equal(expected) {
		t.Fatalf("expected %v, got %v", expected, out)
	}
	if !reflect.DeepEqual(indexMap, []int{0, 1, 3, 4, 6}) {
		t.Fatalf("unexpected index map: %v", indexMap)
	}
	assertAnchorResult(t, ls, out, indexMap, []int{1}, 0.5, false)
}

func TestLineStringWithAnchorsDuplicateCoordinates(t *testing.T) {
	// Repeated coordinates carry distinct business indices and must not be
	// collapsed purely by coordinate.
	ls := orb.LineString{{0, 0}, {2, 0}, {2, 0}, {2, 0}, {4, 0}}

	out, indexMap := simplify.DouglasPeucker(1).LineStringWithAnchors(ls.Clone(), []int{2})
	if !out.Equal(orb.LineString{{0, 0}, {2, 0}, {4, 0}}) {
		t.Fatalf("expected single anchored duplicate kept, got %v", out)
	}
	if !reflect.DeepEqual(indexMap, []int{0, 2, 4}) {
		t.Fatalf("unexpected index map: %v", indexMap)
	}

	// Two distinct indices sharing a coordinate are both anchors.
	out, indexMap = simplify.DouglasPeucker(1).LineStringWithAnchors(ls.Clone(), []int{1, 2})
	if !reflect.DeepEqual(indexMap, []int{0, 1, 2, 4}) {
		t.Fatalf("expected both duplicate anchors kept, got map %v / points %v", indexMap, out)
	}
	if out[1] != out[2] {
		t.Fatalf("anchored duplicates should both be present at the same coordinate, got %v", out)
	}
}

func TestLineStringWithAnchorsInvalidIndex(t *testing.T) {
	original := orb.LineString{{0, 0}, {1, 0}, {2, 0}, {3, 0}, {4, 0}}

	for _, bad := range [][]int{{-1}, {5}, {1, 10}, {0, -2}} {
		input := original.Clone()
		out, indexMap := simplify.DouglasPeucker(1).LineStringWithAnchors(input, bad)
		if !out.Equal(original) {
			t.Fatalf("anchor %v: input must be returned unmodified, got %v", bad, out)
		}
		if indexMap != nil {
			t.Fatalf("anchor %v: expected nil index map, got %v", bad, indexMap)
		}
		if len(out) != len(original) {
			t.Fatalf("anchor %v: input length changed", bad)
		}
	}
}

func TestLineStringWithAnchorsShortInput(t *testing.T) {
	// Empty, single and two-point inputs cannot be reduced.
	for _, ls := range []orb.LineString{nil, {}, {{1, 1}}, {{0, 0}, {1, 1}}} {
		out, indexMap := simplify.DouglasPeucker(1).LineStringWithAnchors(ls, nil)
		if len(out) != len(ls) {
			t.Fatalf("short input must be unchanged, got %v from %v", out, ls)
		}
		if len(indexMap) != len(ls) {
			t.Fatalf("expected identity index map, got %v for %v", indexMap, ls)
		}
		for i, idx := range indexMap {
			if idx != i {
				t.Fatalf("expected identity index map, got %v", indexMap)
			}
		}
	}

	// An illegal anchor on a single-point input leaves it untouched.
	single := orb.LineString{{1, 1}}
	out, indexMap := simplify.DouglasPeucker(1).LineStringWithAnchors(single, []int{1})
	if len(out) != 1 || indexMap != nil {
		t.Fatalf("invalid anchor must not modify single point, got %v / %v", out, indexMap)
	}
}

func TestLineStringWithAnchorsUnsortedAndDuplicates(t *testing.T) {
	ls := orb.LineString{{0, 0}, {1, 0}, {2, 0}, {3, 1}, {4, 0}, {5, 0}, {6, 0}}
	anchors := []int{5, 1, 1, 3}

	out, indexMap := simplify.DouglasPeucker(0.5).LineStringWithAnchors(ls.Clone(), anchors)
	assertAnchorResult(t, ls, out, indexMap, []int{1, 3, 5}, 0.5, false)

	for _, a := range []int{1, 3, 5} {
		found := false
		for _, idx := range indexMap {
			if idx == a {
				found = true
			}
		}
		if !found {
			t.Fatalf("anchor %d missing from map %v", a, indexMap)
		}
	}
}

func TestRingWithAnchorsAcrossClosingEdge(t *testing.T) {
	// Closed square-ish ring; anchors straddle the shared start/end vertex,
	// i.e. one sits near the end and another near the beginning of the slice.
	r := orb.Ring{
		{0, 0}, {1, 0}, {2, 0}, {4, 0},
		{4, 2}, {2, 3}, {0, 2}, {0, 0},
	}
	anchors := []int{6, 1}

	out, indexMap := simplify.DouglasPeucker(0.6).RingWithAnchors(r.Clone(), anchors)

	if out[0] != (orb.Point{0, 0}) || out[len(out)-1] != (orb.Point{0, 0}) {
		t.Fatalf("ring closure must be preserved, got %v", out)
	}
	assertAnchorResult(t, orb.LineString(r), orb.LineString(out), indexMap, anchors, 0.6, true)
}

func TestRingWithAnchorsClosureIndex(t *testing.T) {
	r := orb.Ring{
		{0, 0}, {1, 0}, {2, 0}, {4, 0},
		{4, 2}, {2, 3}, {0, 2}, {0, 0},
	}
	// Referencing the first/last indices that share a coordinate is valid and
	// must keep both entries.
	out, indexMap := simplify.DouglasPeucker(0.6).RingWithAnchors(
		r.Clone(), []int{0, len(r) - 1, 3})
	if indexMap[0] != 0 || indexMap[len(indexMap)-1] != len(r)-1 {
		t.Fatalf("closure indices must be preserved, got %v", indexMap)
	}
	assertAnchorResult(t, orb.LineString(r), orb.LineString(out), indexMap,
		[]int{0, 3, len(r) - 1}, 0.6, true)

	// An out of range anchor leaves the ring alone.
	invalid, im := simplify.DouglasPeucker(0.6).RingWithAnchors(r.Clone(), []int{len(r)})
	if !invalid.Equal(r) || im != nil {
		t.Fatalf("invalid ring anchor must leave input unmodified, got %v / %v", invalid, im)
	}
}

// TestAnchorRandom walks many deterministic open and closed paths, forcing a
// spread of anchors, and checks all structural invariants and per-segment
// error bounds.
func TestAnchorRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	for iter := 0; iter < 200; iter++ {
		n := 4 + rng.Intn(40)
		ls := make(orb.LineString, n)
		for i := range ls {
			dev := 0.0
			if rng.Intn(2) == 0 {
				dev = rng.Float64()*4 - 2
			}
			ls[i] = orb.Point{float64(i), dev}
		}

		threshold := rng.Float64() * 2

		anchors := []int{}
		for i := 1; i < n-1; i++ {
			if rng.Intn(3) == 0 {
				anchors = append(anchors, i)
			}
		}

		out, indexMap := simplify.DouglasPeucker(threshold).
			LineStringWithAnchors(ls.Clone(), anchors)
		assertAnchorResult(t, ls, out, indexMap, anchors, threshold, false)

		// Re-run as a closed ring by reusing the path with a duplicated
		// start point at the end.
		ring := make(orb.Ring, 0, n+1)
		ring = append(ring, ls...)
		ring = append(ring, ls[0])

		ringAnchors := make([]int, 0, len(anchors))
		for _, a := range anchors {
			ringAnchors = append(ringAnchors, a)
		}
		// Deliberately place anchors on both sides of the closure so every
		// ring case exercises the closing edge.
		ringAnchors = append(ringAnchors, len(ring)-2)
		if len(ring) >= 3 {
			ringAnchors = append(ringAnchors, 1)
		}

		rout, rmap := simplify.DouglasPeucker(threshold).RingWithAnchors(ring.Clone(), ringAnchors)
		assertAnchorResult(t, orb.LineString(ring), orb.LineString(rout), rmap,
			ringAnchors, threshold, true)
	}
}

// TestAnchorsMatchesPlainWithoutForcedPoints shows a forced anchor that DP
// already keeps changes nothing, and the plain simplification without anchors
// remains available through the existing LineString/Ring methods.
func TestAnchorsMatchesPlainWithoutForcedPoints(t *testing.T) {
	ls := orb.LineString{{0, 0}, {1, 0}, {2, 1}, {3, 0}, {4, 0}}
	threshold := 0.5

	plain := simplify.DouglasPeucker(threshold).LineString(ls.Clone())

	// Index 2 (the tall point) is kept by DP anyway.
	anchored, indexMap := simplify.DouglasPeucker(threshold).LineStringWithAnchors(ls.Clone(), []int{2})
	if !anchored.Equal(plain) {
		t.Fatalf("naturally kept anchor changed result: %v vs %v", anchored, plain)
	}
	assertAnchorResult(t, ls, anchored, indexMap, []int{2}, threshold, false)
}
