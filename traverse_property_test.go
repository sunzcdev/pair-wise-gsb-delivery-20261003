package geohash

import (
	"math/rand"
	"sort"
	"testing"
)

// denseSampleCells computes the set of cells touched by sampling every segment
// at many points, including every endpoint. Sampling cannot prove exactness,
// but every sampled cell must be present in the exact traversal result.
func denseSampleCells(t *testing.T, pts []TraversePoint, bits uint, steps int) map[uint64]struct{} {
	t.Helper()
	set := map[uint64]struct{}{}
	for i := 0; i+1 < len(pts); i++ {
		for s := 0; s <= steps; s++ {
			f := float64(s) / float64(steps)
			lat := pts[i].Lat + f*(pts[i+1].Lat-pts[i].Lat)
			lng := shortestLerpLng(pts[i].Lng, pts[i+1].Lng, f)
			set[EncodeIntWithPrecision(lat, lng, bits)] = struct{}{}
		}
	}
	if len(pts) == 1 {
		set[EncodeIntWithPrecision(pts[0].Lat, pts[0].Lng, bits)] = struct{}{}
	}
	return set
}

// shortestLerpLng interpolates longitude along the shortest path, used only by
// the sampling oracle in tests.
func shortestLerpLng(a, b, f float64) float64 {
	d := b - a
	switch {
	case d > 180:
		d -= 360
	case d < -180:
		d += 360
	}
	v := a + f*d
	if v > 180 {
		v -= 360
	}
	if v < -180 {
		v += 360
	}
	return v
}

func TestTraverseCoversDenseSampling(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 300; iter++ {
		bits := uint(2 + rng.Intn(20))
		n := 1 + rng.Intn(5)
		pts := make([]TraversePoint, n)
		for i := range pts {
			pts[i] = tp(-89+rng.Float64()*178, -179+rng.Float64()*358)
		}

		res, err := TraversePolyline(pts, bits, 0)
		if err != nil {
			t.Fatal(err)
		}

		sampled := denseSampleCells(t, pts, bits, 2000)
		got := map[uint64]struct{}{}
		for _, h := range res.Cells {
			if _, dup := got[h]; dup {
				t.Fatal("duplicate cell in result")
			}
			got[h] = struct{}{}
		}
		for h := range sampled {
			if _, ok := got[h]; !ok {
				t.Fatalf("bits %d: traversal missed sampled cell %d", bits, h)
			}
		}
	}
}

// TestTraverseCatchesCellSamplingMisses demonstrates the motivating failure:
// encoding only vertices can skip a cell crossed between samples, while the
// analytical traversal reports it.
func TestTraverseCatchesCellSamplingMisses(t *testing.T) {
	// At 5 bits the longitude grid lines are every 45 degrees. A single segment
	// from lng 10 to lng 100 has endpoints in columns 4 ([0,45]) and 6
	// ([90,135]), so encoding only the two vertices misses column 5
	// ([45,90]) that the segment passes through.
	const bits = uint(5)
	pts := []TraversePoint{tp(10, 10), tp(10, 100)}

	vertexCells := map[uint64]struct{}{}
	for _, p := range pts {
		vertexCells[EncodeIntWithPrecision(p.Lat, p.Lng, bits)] = struct{}{}
	}

	res, err := TraversePolyline(pts, bits, 0)
	if err != nil {
		t.Fatal(err)
	}
	full := map[uint64]struct{}{}
	for _, h := range res.Cells {
		full[h] = struct{}{}
	}

	// latBits=2 rows of 45deg put lat 10 in row 2; lngBits=3 columns put
	// lng 10/100 in columns 4/6, with column 5 crossed in between.
	middle := cellHash(5, 2, bits/2, bits-bits/2)
	if _, ok := vertexCells[middle]; ok {
		t.Fatal("test setup invalid: vertex encoding already includes column 5")
	}
	if _, ok := full[middle]; !ok {
		t.Fatal("exact traversal failed to report column 5 crossed mid-segment")
	}
}

func TestTraverseLimitIsPrefix(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for iter := 0; iter < 100; iter++ {
		bits := uint(2 + rng.Intn(18))
		n := 2 + rng.Intn(4)
		pts := make([]TraversePoint, n)
		for i := range pts {
			pts[i] = tp(-89+rng.Float64()*178, -179+rng.Float64()*358)
		}

		full, err := TraversePolyline(pts, bits, 0)
		if err != nil {
			t.Fatal(err)
		}
		limit := uint(rng.Intn(len(full.Cells) + 2))
		res, err := TraversePolyline(pts, bits, limit)
		if err != nil {
			t.Fatal(err)
		}
		wantTruncated := limit > 0 && uint(len(full.Cells)) > limit
		if res.Truncated != wantTruncated {
			t.Fatalf("truncated=%v want %v (full=%d limit=%d)",
				res.Truncated, wantTruncated, len(full.Cells), limit)
		}
		wantLen := len(full.Cells)
		if limit > 0 && uint(wantLen) > limit {
			wantLen = int(limit)
		}
		if len(res.Cells) != wantLen {
			t.Fatalf("len=%d want %d", len(res.Cells), wantLen)
		}
		for i := range res.Cells {
			if res.Cells[i] != full.Cells[i] {
				t.Fatalf("limited result is not a prefix at %d", i)
			}
		}
	}
}

func TestTraverseHighPrecisionCornerAndEdge(t *testing.T) {
	bits := uint(64)
	latBits := bits / 2
	lngBits := bits - latBits
	rows := float64(int64(1) << latBits)
	cols := float64(int64(1) << lngBits)

	// A point very close to (but not on) a fine grid line returns one cell.
	lng := -180 + 7*(360/cols) + 0.25*(360/cols)
	lat := -90 + 9*(180/rows) + 0.25*(180/rows)
	res, err := TraversePolyline([]TraversePoint{tp(lat, lng)}, bits, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cells) != 1 {
		t.Fatalf("expected single cell, got %d", len(res.Cells))
	}

	// A diagonal through a fine corner reports the four incident cells.
	lng0 := -180 + 7*(360/cols)
	lat0 := -90 + 9*(180/rows)
	pts := []TraversePoint{
		tp(lat0+0.3*(180/rows), lng0+0.3*(360/cols)),
		tp(lat0-0.3*(180/rows), lng0-0.3*(360/cols)),
	}
	res2, err := TraversePolyline(pts, bits, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Cells) != 4 {
		t.Fatalf("expected 4 corner cells, got %d", len(res2.Cells))
	}
}

func TestTraverseLongEdgeRunLimit(t *testing.T) {
	// Along the equator at fine precision the true result is enormous; the
	// limit must stop traversal early with an explicit incomplete status, and
	// the returned prefix must be ordered without duplicates.
	const limit = 5000
	res, err := TraversePolyline([]TraversePoint{tp(0, -179.999), tp(0, 179.999)}, 64, limit)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("expected truncation for a huge edge run")
	}
	if len(res.Cells) != limit {
		t.Fatalf("expected exactly %d prefix cells, got %d", limit, len(res.Cells))
	}
	seen := map[uint64]struct{}{}
	for _, h := range res.Cells {
		if _, ok := seen[h]; ok {
			t.Fatal("duplicate cell")
		}
		seen[h] = struct{}{}
	}
}

func TestTraverseFirstContactOrder(t *testing.T) {
	// A hook-shaped polyline: first-contact order must match increasing time.
	pts := []TraversePoint{tp(10, 10), tp(10, 100), tp(80, 100)}
	res, err := TraversePolyline(pts, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Independently derive an order from contact time via fine sampling and
	// compare the relative order of a few known cells.
	type timed struct {
		h uint64
		t float64
	}
	first := map[uint64]float64{}
	steps := 200000
	// Segment 1.
	for s := 0; s <= steps; s++ {
		f := float64(s) / float64(steps)
		lat := 10.0
		lng := shortestLerpLng(10, 100, f)
		h := EncodeIntWithPrecision(lat, lng, 3)
		if _, ok := first[h]; !ok {
			first[h] = f * 0.5
		}
	}
	// Segment 2.
	for s := 0; s <= steps; s++ {
		f := float64(s) / float64(steps)
		lat := 10 + f*70
		lng := 100.0
		h := EncodeIntWithPrecision(lat, lng, 3)
		if _, ok := first[h]; !ok {
			first[h] = 0.5 + f*0.5
		}
	}

	timedCells := make([]timed, 0, len(first))
	for h, tm := range first {
		timedCells = append(timedCells, timed{h, tm})
	}
	sort.Slice(timedCells, func(i, j int) bool { return timedCells[i].t < timedCells[j].t })

	gotIndex := map[uint64]int{}
	for i, h := range res.Cells {
		gotIndex[h] = i
	}
	for i := 1; i < len(timedCells); i++ {
		prev := timedCells[i-1].h
		cur := timedCells[i].h
		if gotIndex[prev] > gotIndex[cur] {
			t.Fatalf("first-contact order violated: %d before %d", prev, cur)
		}
	}
}
