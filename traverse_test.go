package geohash

import (
	"math"
	"testing"
)

// At 3 bits of precision the grid is hand-computable: 2 latitude rows of 90
// degrees and 4 longitude columns of 90 degrees. The longitude grid lines are
// at -180, -90, 0, 90, 180: column 0 spans [-180,-90], column 1 [-90,0],
// column 2 [0,90], column 3 [90,180]. Latitude row 0 spans [-90,0] and row 1
// spans [0,90].

// hashCell decodes an integer hash back to its wrapped (column, row) at the
// given precision, so expectations are written in hand-computed grid indices.
func hashCell(h uint64, bits uint) (col, row int64) {
	latBits := bits / 2
	lngBits := bits - latBits
	full := h << (64 - bits)
	latIdx, lngIdx := deinterleave(full)
	row = int64(latIdx >> (32 - latBits))
	col = int64(lngIdx >> (32 - lngBits))
	return
}

func assertCellSequence(t *testing.T, got []uint64, bits uint, want [][2]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d want %d (hashes %v)", len(got), len(want), got)
	}
	for i, h := range got {
		col, row := hashCell(h, bits)
		if col != want[i][0] || row != want[i][1] {
			t.Fatalf("index %d: got (col=%d,row=%d) want (col=%d,row=%d)",
				i, col, row, want[i][0], want[i][1])
		}
	}
}

func tp(lat, lng float64) TraversePoint {
	return TraversePoint{Lat: lat, Lng: lng}
}

func TestTraverseSinglePointCorner(t *testing.T) {
	// The point (0, 0) is a grid corner touching four cells.
	res, err := TraversePolyline([]TraversePoint{tp(0, 0)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{1, 0}, {1, 1}, {2, 0}, {2, 1}})
	if res.Truncated {
		t.Fatal("unexpected truncation")
	}
}

func TestTraverseSinglePointInterior(t *testing.T) {
	// (10, 10) is strictly inside column 2, row 1.
	res, err := TraversePolyline([]TraversePoint{tp(10, 10)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{2, 1}})
}

func TestTraverseSegmentInsideOneCell(t *testing.T) {
	res, err := TraversePolyline([]TraversePoint{tp(10, 10), tp(20, 20)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{2, 1}})
}

func TestTraverseSegmentCrossesVerticalLine(t *testing.T) {
	// (10, 80) -> (10, 100) crosses the lng=90 line at fixed lat.
	res, err := TraversePolyline([]TraversePoint{tp(10, 80), tp(10, 100)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{2, 1}, {3, 1}})
}

func TestTraverseSegmentCrossesHorizontalLine(t *testing.T) {
	// (-10, 10) -> (10, 10) crosses the equator at fixed lng.
	res, err := TraversePolyline([]TraversePoint{tp(-10, 10), tp(10, 10)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{2, 0}, {2, 1}})
}

func TestTraverseDiagonalThroughCorner(t *testing.T) {
	// (10, 10) -> (-10, -10) passes exactly through corner (0,0).
	res, err := TraversePolyline([]TraversePoint{tp(10, 10), tp(-10, -10)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{
		{2, 1},
		{1, 0}, {1, 1}, {2, 0},
	})
}

func TestTraverseRunsAlongLatitudeLine(t *testing.T) {
	// East along the equator from lng 10 to 100: columns 2 then 3, both rows.
	res, err := TraversePolyline([]TraversePoint{tp(0, 10), tp(0, 100)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{
		{2, 0}, {2, 1},
		{3, 0}, {3, 1},
	})
}

func TestTraverseRunsAlongLongitudeLine(t *testing.T) {
	// North along the lng=0 line from lat -10 to 80: both columns 1,2 for
	// rows 0 and 1, with the equator corner emitting all four.
	res, err := TraversePolyline([]TraversePoint{tp(-10, 0), tp(80, 0)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{
		{1, 0}, {2, 0}, {1, 1}, {2, 1},
	})
}

func TestTraverseShortSegmentCrossesAntimeridianEast(t *testing.T) {
	// (10, 170) -> (10, -170): shortest path is east across the seam.
	res, err := TraversePolyline([]TraversePoint{tp(10, 170), tp(10, -170)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{3, 1}, {0, 1}})
}

func TestTraverseShortSegmentCrossesAntimeridianWest(t *testing.T) {
	// (10, -170) -> (10, 170): shortest path is west across the seam.
	res, err := TraversePolyline([]TraversePoint{tp(10, -170), tp(10, 170)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{0, 1}, {3, 1}})
}

func TestTraverseExactHalfGlobeGoesEast(t *testing.T) {
	// (10, 90) -> (10, -90) is exactly half the globe; convention: east.
	res, err := TraversePolyline([]TraversePoint{tp(10, 90), tp(10, -90)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Start on the line between cols 2 and 3; east path 3 -> 0 -> 1 (end line
	// between cols 0 and 1).
	assertCellSequence(t, res.Cells, 3, [][2]int64{
		{2, 1}, {3, 1},
		{0, 1}, {1, 1},
	})
}

func TestTraverseExactHalfGlobeReversedAlsoEast(t *testing.T) {
	// The reverse pair is also an exact half-globe span and must go east.
	res, err := TraversePolyline([]TraversePoint{tp(10, -90), tp(10, 90)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{
		{0, 1}, {1, 1},
		{2, 1}, {3, 1},
	})
}

func TestTraverseAlongEquatorCrossingSeam(t *testing.T) {
	// East along the equator from 170 to -170: both rows, columns 3 then 0.
	res, err := TraversePolyline([]TraversePoint{tp(0, 170), tp(0, -170)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{
		{3, 0}, {3, 1},
		{0, 0}, {0, 1},
	})
}

func TestTraverseDeduplicatesRepeatedVertices(t *testing.T) {
	// Back-and-forth path reports each cell once, in first-contact order.
	res, err := TraversePolyline([]TraversePoint{
		tp(10, 10), tp(10, 100), tp(10, 10),
	}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{2, 1}, {3, 1}})
}

func TestTraverseConsecutiveIdenticalVertices(t *testing.T) {
	res, err := TraversePolyline([]TraversePoint{
		tp(10, 10), tp(10, 10), tp(20, 20),
	}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{2, 1}})
}

func TestTraverseLimitTruncates(t *testing.T) {
	// Lat 10 keeps the path off the equator: east from col 2 through col 3.
	full, err := TraversePolyline([]TraversePoint{tp(10, 10), tp(10, 100)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Cells) != 2 {
		t.Fatalf("expected 2 cells, got %d", len(full.Cells))
	}

	// Run along the equator instead to produce 4 distinct cells, cap at 3.
	const limit = 3
	res, err := TraversePolyline([]TraversePoint{tp(0, 10), tp(0, 100)}, 3, limit)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("expected truncated result")
	}
	if len(res.Cells) != limit {
		t.Fatalf("expected %d cells, got %d", limit, len(res.Cells))
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{
		{2, 0}, {2, 1}, {3, 0},
	})
}

func TestTraverseLimitExactCountNotTruncated(t *testing.T) {
	// A limit equal to the true distinct count must not report truncation.
	res, err := TraversePolyline([]TraversePoint{tp(10, 10), tp(10, 100)}, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated {
		t.Fatal("should not be truncated when limit equals count")
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{2, 1}, {3, 1}})
}

func TestTraverseEmpty(t *testing.T) {
	res, err := TraversePolyline(nil, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cells) != 0 || res.Truncated {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

func TestTraversePolesAndMeridianBoundaries(t *testing.T) {
	// North pole on the 3-bit grid corner: one row (row 1) touched, columns.
	res, err := TraversePolyline([]TraversePoint{tp(90, 0)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Pole row clamps to the single top row; lng=0 line touches columns 1,2.
	assertCellSequence(t, res.Cells, 3, [][2]int64{{1, 1}, {2, 1}})

	// South pole.
	res, err = TraversePolyline([]TraversePoint{tp(-90, 0)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{1, 0}, {2, 0}})

	// +180 and -180 denote the same meridian, which touches columns 0 and 3.
	res, err = TraversePolyline([]TraversePoint{tp(10, 180)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{0, 1}, {3, 1}})

	res, err = TraversePolyline([]TraversePoint{tp(10, -180)}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertCellSequence(t, res.Cells, 3, [][2]int64{{0, 1}, {3, 1}})
}

func TestTraversePolylineJoinsShareVertexCells(t *testing.T) {
	// Two segments meeting at a grid corner: cells incident at the join are
	// emitted once and ordering is globally first-contact.
	pts := []TraversePoint{
		tp(10, 10),
		tp(0, 0),
		tp(-10, 10),
	}
	res, err := TraversePolyline(pts, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uint64]struct{}{}
	for _, h := range res.Cells {
		if _, dup := seen[h]; dup {
			t.Fatalf("duplicate cell %d", h)
		}
		seen[h] = struct{}{}
	}
	// Approach cell (2,1); corner four cells; exit cell (2,0). Unique set:
	assertCellSequence(t, res.Cells, 3, [][2]int64{
		{2, 1},
		{1, 0}, {1, 1}, {2, 0},
	})
}

func TestTraverseInvalidInputs(t *testing.T) {
	cases := []struct {
		name   string
		points []TraversePoint
		bits   uint
	}{
		{"bits too small", []TraversePoint{tp(0, 0)}, 1},
		{"bits too large", []TraversePoint{tp(0, 0)}, 65},
		{"lat too high", []TraversePoint{tp(91, 0)}, 3},
		{"lat too low", []TraversePoint{tp(-91, 0)}, 3},
		{"lng too high", []TraversePoint{tp(0, 181)}, 3},
		{"lng too low", []TraversePoint{tp(0, -181)}, 3},
		{"nan", []TraversePoint{{Lat: math.NaN(), Lng: 0}}, 3},
		{"nan in segment", []TraversePoint{tp(0, 0), {Lat: 0, Lng: math.NaN()}}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := TraversePolyline(c.points, c.bits, 0); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestTraverseStringVersion(t *testing.T) {
	cells, truncated, err := TraversePolylineString(
		[]TraversePoint{tp(10, 170), tp(10, -170)}, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	resInt, err := TraversePolyline([]TraversePoint{tp(10, 170), tp(10, -170)}, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != len(resInt.Cells) {
		t.Fatalf("length mismatch %d vs %d", len(cells), len(resInt.Cells))
	}
	for i, h := range resInt.Cells {
		if cells[i] != ConvertIntToString(h, 1) {
			t.Fatalf("index %d: got %q want %q", i, cells[i], ConvertIntToString(h, 1))
		}
	}
}

func TestTraverseStringInvalidChars(t *testing.T) {
	if _, _, err := TraversePolylineString(nil, 0, 0); err == nil {
		t.Fatal("expected error for zero chars")
	}
	if _, _, err := TraversePolylineString(nil, 13, 0); err == nil {
		t.Fatal("expected error for >12 chars")
	}
}

func TestCellHashMatchesEncoder(t *testing.T) {
	// For cells whose center is comfortably interior, the direct index hash
	// must agree with the package encoder.
	for _, bits := range []uint{2, 3, 5, 12, 32, 64} {
		latBits := bits / 2
		lngBits := bits - latBits
		rows := int64(1) << latBits
		cols := int64(1) << lngBits
		col := int64(1)
		row := rows / 3
		if col >= cols {
			col = 0
		}
		lng := -180 + (float64(col)+0.5)*(360.0/float64(cols))
		lat := -90 + (float64(row)+0.5)*(180.0/float64(rows))
		want := EncodeIntWithPrecision(lat, lng, bits)
		if got := cellHash(col, row, latBits, lngBits); got != want {
			t.Fatalf("bits %d cell (%d,%d): got %d want %d", bits, col, row, got, want)
		}
	}
}
