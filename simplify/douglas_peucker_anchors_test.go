package simplify

import (
	"math"
	"math/rand"
	"reflect"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/planar"
)

// verifyAnchored checks the invariants of an anchored simplification:
//   - every output point matches the original point given by the index map,
//   - the index map is strictly increasing (monotonic),
//   - both endpoints are preserved,
//   - every anchor index is present in the map,
//   - every removed point lies within the threshold of the output segment
//     that replaced it.
func verifyAnchored(t *testing.T, original, reduced orb.LineString, indexMap, anchors []int, threshold float64) {
	t.Helper()

	if len(reduced) != len(indexMap) {
		t.Fatalf("reduced length %d != index map length %d", len(reduced), len(indexMap))
	}

	for i, oi := range indexMap {
		if oi < 0 || oi >= len(original) {
			t.Fatalf("index map entry %d out of range: %d", i, oi)
		}
		if reduced[i] != original[oi] {
			t.Fatalf("reduced[%d] %v != original[%d] %v", i, reduced[i], oi, original[oi])
		}
		if i > 0 && indexMap[i-1] >= oi {
			t.Fatalf("index map not strictly increasing: %v", indexMap)
		}
	}

	if len(indexMap) < 2 || indexMap[0] != 0 || indexMap[len(indexMap)-1] != len(original)-1 {
		t.Fatalf("endpoints not preserved: %v", indexMap)
	}

	kept := make(map[int]bool, len(indexMap))
	for _, oi := range indexMap {
		kept[oi] = true
	}
	for _, a := range anchors {
		if !kept[a] {
			t.Fatalf("anchor index %d missing from output: %v", a, indexMap)
		}
	}

	// locate, for each removed original point, the output segment spanning it
	next := 0
	for oi := range original {
		for next < len(indexMap) && indexMap[next] < oi {
			next++
		}

		if kept[oi] {
			continue
		}

		// find the surrounding kept points: the first kept index >= oi
		// (next) and the last kept index < oi (next-1)
		if next == 0 || next >= len(indexMap) {
			t.Fatalf("removed point %d not bracketed by output segments: %v", oi, indexMap)
		}
		a := original[indexMap[next-1]]
		b := original[indexMap[next]]
		d := math.Sqrt(planar.DistanceFromSegmentSquared(a, b, original[oi]))
		if d > threshold+1e-9 {
			t.Fatalf("removed point %d %v is %v > threshold %v from segment %v->%v",
				oi, original[oi], d, threshold, a, b)
		}
	}
}

func TestDouglasPeuckerAnchors_NearlyCollinear(t *testing.T) {
	// collinear-ish line; the slightly-off middle business points would be
	// removed by the plain algorithm at this threshold.
	original := orb.LineString{{0, 0}, {0.5, 0.01}, {1, 0}, {1.5, 0.01}, {2, 0}}
	threshold := 0.5

	reduced, im := DouglasPeucker(threshold).LineStringWithAnchors(original.Clone(), nil)
	if !reflect.DeepEqual(im, []int{0, 4}) {
		t.Fatalf("plain reduction changed: %v", im)
	}

	anchors := []int{2}
	reduced, im = DouglasPeucker(threshold).LineStringWithAnchors(original.Clone(), anchors)
	verifyAnchored(t, original, reduced, im, anchors, threshold)

	// the nearly collinear anchor must survive while points on both sides of
	// it are simplified independently
	if !reflect.DeepEqual(im, []int{0, 2, 4}) {
		t.Fatalf("anchor not preserved: %v", im)
	}
}

func TestDouglasPeuckerAnchors_DuplicateCoordinates(t *testing.T) {
	// indices 2 and 3 share the same coordinate but are distinct business
	// points (e.g. a billing point coincident with a vertex).
	original := orb.LineString{{0, 0}, {0, 2}, {1, 2}, {1, 2}, {2, 2}, {2, 0}}
	threshold := 5.0
	anchors := []int{2, 3}

	reduced, im := DouglasPeucker(threshold).LineStringWithAnchors(original.Clone(), anchors)
	verifyAnchored(t, original, reduced, im, anchors, threshold)

	// both duplicate-coordinate business indices must be retained
	if !reflect.DeepEqual(im, []int{0, 2, 3, 5}) {
		t.Fatalf("duplicate-coordinate anchors not both preserved: %v", im)
	}
	if reduced[1] != reduced[2] {
		t.Fatalf("expected duplicate coordinates in output: %v", reduced)
	}
}

func TestDouglasPeuckerAnchors_InvalidIndex(t *testing.T) {
	original := orb.LineString{{0, 0}, {1, 0}, {2, 0}}

	for _, bad := range []int{-1, 3, 99} {
		input := original.Clone()
		anchors := []int{1, bad}

		reduced, im := DouglasPeucker(1.0).LineStringWithAnchors(input, anchors)
		if im != nil {
			t.Fatalf("expected nil index map for invalid anchor %d, got %v", bad, im)
		}
		if !reflect.DeepEqual(orb.LineString(reduced), original) {
			t.Fatalf("input modified for invalid anchor %d: %v", bad, reduced)
		}
	}
}

func TestDouglasPeuckerAnchors_DoesNotMutateAnchorSlice(t *testing.T) {
	original := orb.LineString{{0, 0}, {1, 1}, {2, 0}, {3, 1}, {4, 0}}
	anchors := []int{3, 1, 1, 0} // unsorted with duplicates
	saved := append([]int(nil), anchors...)

	_, im := DouglasPeucker(2.0).LineStringWithAnchors(original.Clone(), anchors)
	if !reflect.DeepEqual(anchors, saved) {
		t.Fatalf("anchor slice mutated: %v -> %v", saved, anchors)
	}

	want := map[int]bool{0: true, 1: true, 3: true, 4: true}
	got := map[int]bool{}
	for _, i := range im {
		got[i] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected kept indices: %v", im)
	}
}

func TestDouglasPeuckerAnchors_ClosedRingAcrossClosure(t *testing.T) {
	// a closed square with two collinear points along each edge; index 6 is
	// on the last edge and straddles the closure with the start.
	original := orb.Ring{
		{0, 0}, {0.33, 0}, {0.66, 0},
		{1, 0}, {1, 0.5}, {1, 1},
		{0.66, 1}, {0.33, 1},
		{0, 0},
	}
	threshold := 0.5
	anchors := []int{6}

	reduced, im := DouglasPeucker(threshold).RingWithAnchors(original.Clone(), anchors)

	if reduced[0] != reduced[len(reduced)-1] {
		t.Fatalf("closed ring no longer closed: %v", reduced)
	}
	verifyAnchored(t, orb.LineString(original), orb.LineString(reduced), im, anchors, threshold)

	// collinear edge points are removed while the closing pair and the anchor
	// near the closure survive
	if !reflect.DeepEqual(im, []int{0, 3, 6, 8}) {
		t.Fatalf("unexpected ring simplification: %v", im)
	}
}

func TestDouglasPeuckerAnchors_ShortInputs(t *testing.T) {
	if r, im := DouglasPeucker(1).LineStringWithAnchors(nil, nil); len(r) != 0 || im != nil {
		t.Fatalf("empty linestring: %v %v", r, im)
	}

	single := orb.LineString{{1, 2}}
	r, im := DouglasPeucker(1).LineStringWithAnchors(single, nil)
	if len(r) != 1 || im != nil {
		t.Fatalf("single point linestring: %v %v", r, im)
	}

	// an anchor into a 1-point linestring is invalid
	if _, im := DouglasPeucker(1).LineStringWithAnchors(single, []int{1}); im != nil {
		t.Fatalf("expected nil map for anchor out of range: %v", im)
	}
}

func TestDouglasPeuckerAnchors_MatchesPlainWhenNoAnchors(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	original := make(orb.LineString, 60)
	for i := range original {
		original[i] = orb.Point{rng.NormFloat64() * 10, rng.NormFloat64() * 10}
	}

	threshold := 1.5
	plain := DouglasPeucker(threshold).LineString(original.Clone())
	anchored, im := DouglasPeucker(threshold).LineStringWithAnchors(original.Clone(), nil)

	if !plain.Equal(anchored) {
		t.Fatalf("no-anchor result %v != plain result %v", anchored, plain)
	}
	for i, p := range anchored {
		if p != plain[i] {
			t.Fatalf("no-anchor result diverged at %d", i)
		}
	}
	verifyAnchored(t, original, anchored, im, nil, threshold)
}

func TestDouglasPeuckerAnchors_Randomized(t *testing.T) {
	rng := rand.New(rand.NewSource(7))

	for iter := 0; iter < 200; iter++ {
		n := 3 + rng.Intn(50)
		original := make(orb.LineString, n)
		for i := range original {
			original[i] = orb.Point{float64(rng.Intn(20)) * rng.Float64(), float64(rng.Intn(20)) * rng.Float64()}
		}

		closed := iter%2 == 0
		if closed {
			original[n-1] = original[0]
		}

		anchors := []int{}
		for i := 0; i < 1+rng.Intn(4); i++ {
			anchors = append(anchors, rng.Intn(n))
		}

		threshold := rng.Float64() * 3.0

		if closed {
			reduced, im := DouglasPeucker(threshold).RingWithAnchors(orb.Ring(original).Clone(), anchors)
			if reduced[0] != reduced[len(reduced)-1] {
				t.Fatalf("iter %d: ring opened by simplification", iter)
			}
			verifyAnchored(t, orb.LineString(original), orb.LineString(reduced), im, anchors, threshold)
		} else {
			reduced, im := DouglasPeucker(threshold).LineStringWithAnchors(original.Clone(), anchors)
			verifyAnchored(t, original, reduced, im, anchors, threshold)
		}
	}
}

func TestDouglasPeuckerWithAnchors_Function(t *testing.T) {
	original := orb.LineString{{0, 0}, {0.5, 0.01}, {1, 0}}
	reduced, im := DouglasPeuckerWithAnchors(0.5, original.Clone(), []int{1})
	verifyAnchored(t, original, reduced, im, []int{1}, 0.5)
	if !reflect.DeepEqual(im, []int{0, 1, 2}) {
		t.Fatalf("unexpected index map: %v", im)
	}
}
