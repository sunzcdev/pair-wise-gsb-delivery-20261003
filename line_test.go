package geohash

import (
	"errors"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"time"
)

// cellHash2 builds the expected 2-bit geohash for a cell of the coarsest
// grid: rows split latitude at the equator, columns split longitude at the
// antimeridian. The two precision bits are (lngMSB, latMSB).
func cellHash2(row, col int) uint64 {
	return uint64((col&1)<<1 | (row & 1))
}

// TestLineHandComputed verifies traversal on the 2-bit grid, whose four cells
// span 90 degrees of latitude and 180 degrees of longitude and are small
// enough to check by hand.
func TestLineHandComputed(t *testing.T) {
	cases := []struct {
		name   string
		points []Point
		want   []uint64
	}{
		{
			// A single vertex on the corner where the equator meets the
			// antimeridian touches all four cells, emitted by increasing row
			// then increasing column.
			name:   "corner_point",
			points: []Point{{0, -180}},
			want:   []uint64{cellHash2(0, -1), cellHash2(0, 0), cellHash2(1, -1), cellHash2(1, 0)},
		},
		{
			// Short segment crossing the antimeridian without riding a grid
			// line: (45, 170) -> (45, -170) is 20 degrees eastward, staying
			// in the northern row across two columns.
			name:   "cross_antimeridian",
			points: []Point{{45, 170}, {45, -170}},
			want:   []uint64{cellHash2(1, 1), cellHash2(1, 0)},
		},
		{
			// Diagonal through the corner (0, 0): both rows and columns are
			// touched, with the simultaneous corner cells ordered row then
			// column.
			name:   "corner_diagonal",
			points: []Point{{-45, -90}, {45, 90}},
			want:   []uint64{0b00, 0b10, 0b01, 0b11},
		},
		{
			// Segment riding the equator across the antimeridian lies on a
			// latitude grid line the whole way, so it touches every coarse
			// cell of both columns.
			name:   "cross_antimeridian_on_gridline",
			points: []Point{{0, 170}, {0, -170}},
			want: []uint64{
				cellHash2(0, 1), cellHash2(1, 1),
				cellHash2(0, 0), cellHash2(1, 0),
			},
		},
		{
			// Segment along a vertical grid line: both neighboring columns.
			name:   "walk_grid_meridian",
			points: []Point{{0, 0}, {90, 0}},
			want:   []uint64{0b00, 0b10, 0b01, 0b11},
		},
		{
			// Repeated vertices change nothing: the segment is degenerate and
			// every cell is already present.
			name:   "repeated_vertices",
			points: []Point{{0, 0}, {90, 0}, {0, 0}},
			want:   []uint64{0b00, 0b10, 0b01, 0b11},
		},
		{
			// Exactly half a turn is taken eastward; the same columns are
			// reached either way, here along the equator.
			name:   "half_turn",
			points: []Point{{0, 0}, {0, -180}},
			want:   []uint64{0b00, 0b10, 0b01, 0b11},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, complete, err := LineIntWithPrecision(tc.points, 2, 0)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !complete {
				t.Fatalf("reported incomplete without a limit")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("hashes = %02b, want %02b", got, tc.want)
			}
		})
	}
}

// TestLineLimit checks that a positive limit truncates output and reports
// incompleteness, while the returned prefix equals the unlimited traversal.
func TestLineLimit(t *testing.T) {
	points := []Point{{0, -180}}
	full, complete, err := LineIntWithPrecision(points, 2, 0)
	if err != nil || !complete {
		t.Fatalf("unlimited traversal failed: complete=%v err=%v", complete, err)
	}
	if len(full) != 4 {
		t.Fatalf("expected 4 corner cells, got %d", len(full))
	}

	got, complete, err := LineIntWithPrecision(points, 2, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if complete {
		t.Fatalf("expected incomplete status when the limit is exceeded")
	}
	if !reflect.DeepEqual(got, full[:2]) {
		t.Fatalf("truncated = %02b, want prefix %02b", got, full[:2])
	}
}

// TestLineHardCap checks that a traversal which would enumerate an enormous
// number of high-precision cells stops promptly and reports incompleteness.
func TestLineHardCap(t *testing.T) {
	// At 64 bits a 300-degree longitude sweep crosses billions of columns;
	// this must return quickly with complete=false rather than allocating.
	points := []Point{{1, 0}, {1, -60}}
	done := make(chan struct{})
	var (
		complete bool
		err      error
	)
	go func() {
		_, complete, err = LineInt(points, 0)
		close(done)
	}()
	select {
	case <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if complete {
			t.Fatalf("expected incomplete status at the safety cap")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("traversal did not stop at the safety cap")
	}
}

// TestLineErrors covers input validation.
func TestLineErrors(t *testing.T) {
	if _, _, err := LineIntWithPrecision(nil, 2, 0); !errors.Is(err, ErrEmptyPolyline) {
		t.Fatalf("empty polyline: got %v", err)
	}
	if _, _, err := LineIntWithPrecision([]Point{{0, 0}}, 0, 0); !errors.Is(err, ErrInvalidPrecision) {
		t.Fatalf("zero precision: got %v", err)
	}
	if _, _, err := LineIntWithPrecision([]Point{{0, 0}}, 65, 0); !errors.Is(err, ErrInvalidPrecision) {
		t.Fatalf("precision above 64: got %v", err)
	}

	bad := []Point{
		{math.NaN(), 0},
		{0, math.Inf(1)},
		{90.000001, 0},
		{0, -180.000001},
	}
	for _, p := range bad {
		if _, _, err := LineIntWithPrecision([]Point{p}, 2, 0); !errors.Is(err, ErrInvalidCoord) {
			t.Fatalf("coordinate %v: got %v", p, err)
		}
	}
}

// TestLineString checks the string form against the integer form, including
// the incomplete flag and first-visit order.
func TestLineString(t *testing.T) {
	points := []Point{{0, 170}, {0, -170}}

	ints, complete, err := LineIntWithPrecision(points, 5, 0)
	if err != nil || !complete {
		t.Fatalf("integer traversal failed: complete=%v err=%v", complete, err)
	}

	strs, complete, err := Line(points, 1, 0)
	if err != nil || !complete {
		t.Fatalf("string traversal failed: complete=%v err=%v", complete, err)
	}
	if len(strs) != len(ints) {
		t.Fatalf("got %d strings for %d integers", len(strs), len(ints))
	}
	for i, h := range ints {
		if want := ConvertIntToString(h, 1); want != strs[i] {
			t.Fatalf("string %d = %q, want %q", i, strs[i], want)
		}
	}

	if _, complete, _ := Line(points, 1, 1); complete {
		t.Fatalf("expected incomplete status when the limit is exceeded")
	}
}

// TestLineHighPrecision checks a 0.1 degree horizontal segment crossing the
// antimeridian at 32-bit precision: the last eastern cell ends exactly at
// longitude 180 and the first western cell starts at -180.
func TestLineHighPrecision(t *testing.T) {
	points := []Point{{Lat: 0.05, Lng: 179.95}, {Lat: 0.05, Lng: -179.95}}
	got, complete, err := LineIntWithPrecision(points, 32, 0)
	if err != nil || !complete {
		t.Fatalf("traversal failed: complete=%v err=%v", complete, err)
	}
	if len(got) != 20 {
		t.Fatalf("expected 20 cells, got %d", len(got))
	}

	// Every returned cell must actually contain the fixed latitude band.
	for _, h := range got {
		box := BoundingBoxIntWithPrecision(h, 32)
		if !(box.MinLat <= 0.05 && 0.05 <= box.MaxLat) {
			t.Fatalf("cell %08x box %v does not contain latitude", h, box)
		}
	}

	// The pair straddling the seam meet exactly at +/-180.
	east := BoundingBoxIntWithPrecision(got[9], 32)
	west := BoundingBoxIntWithPrecision(got[10], 32)
	if east.MaxLng != 180 || west.MinLng != -180 {
		t.Fatalf("seam not covered: east=%v west=%v", east, west)
	}

	// Deduplicated and first-visit ordered: cells advance monotonically in
	// longitude within each side of the seam.
	for i := 1; i < 10; i++ {
		if got[i] == got[i-1] {
			t.Fatalf("duplicate cell at %d", i)
		}
	}
}

// TestLineAgainstOracle compares traversal against an independent analytic
// reference that enumerates every cell and solves for the parameter interval
// in which the segment lies inside the cell's closed box.
func TestLineAgainstOracle(t *testing.T) {
	const trials = 400
	for trial := 0; trial < trials; trial++ {
		bits := uint(1 + rand.Intn(12))
		n := 1 + rand.Intn(4)
		points := make([]Point, n)
		for i := range points {
			points[i] = Point{
				Lat: -90 + 180*rand.Float64(),
				Lng: -180 + 360*rand.Float64(),
			}
		}

		got, complete, err := LineIntWithPrecision(points, bits, 0)
		if err != nil || !complete {
			t.Fatalf("traversal failed: complete=%v err=%v", complete, err)
		}

		want := oracleLineHashes(points, bits)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("bits=%d points=%v\ngot  %0*b\nwant %0*b",
				bits, points, int(bits), got, int(bits), want)
		}
	}
}

// oracleLineHashes is a brute-force reference: for every cell it computes
// the earliest parameter at which the polyline intersects the cell's closed
// box, then orders cells by first contact.
func oracleLineHashes(points []Point, bits uint) []uint64 {
	latBits := int(bits) / 2
	lngBits := int(bits) - latBits
	rows := 1 << uint(latBits)
	cols := 1 << uint(lngBits)
	g := lineGrid{latRows: rows, lngCols: cols, latBits: uint(latBits), lngBits: uint(lngBits)}

	type contact struct {
		hash uint64
		when float64
		row  int
		col  int
	}
	var contacts []contact

	u0 := points[0].Lng
	base := 0.0
	for s := 0; s < len(points)-1; s++ {
		a, b := points[s], points[s+1]
		u1 := u0 + shortestLngDelta(a.Lng, b.Lng)

		for r := 0; r < rows; r++ {
			for c := 0; c < cols; c++ {
				tmin, tmax := 0.0, 1.0

				lat0, lat1 := math.Min(a.Lat, b.Lat), math.Max(a.Lat, b.Lat)
				latLo := -90 + 180*float64(r)/float64(rows)
				latHi := latLo + 180/float64(rows)
				if lat0 > latHi+lineTol || lat1 < latLo-lineTol {
					continue
				}
				if dLat := b.Lat - a.Lat; dLat != 0 {
					t1 := (latLo - a.Lat) / dLat
					t2 := (latHi - a.Lat) / dLat
					tmin = math.Max(tmin, math.Min(t1, t2))
					tmax = math.Min(tmax, math.Max(t1, t2))
				}

				lngLo := -180 + 360*float64(c)/float64(cols)
				lngHi := lngLo + 360/float64(cols)

				// The segment is expressed in an unwrapped longitude frame,
				// so test every periodic copy of the column's longitude band.
				first := math.Inf(1)
				intersects := false
				for k := -2; k <= 2; k++ {
					shift := 360 * float64(k)
					lo, hi := lngLo+shift, lngHi+shift
					ltMin, ltMax := tmin, tmax
					if dLng := u1 - u0; dLng != 0 {
						t1 := (lo - u0) / dLng
						t2 := (hi - u0) / dLng
						ltMin = math.Max(ltMin, math.Min(t1, t2))
						ltMax = math.Min(ltMax, math.Max(t1, t2))
					} else if u0 < lo-lineTol || u0 > hi+lineTol {
						continue
					}
					if ltMin <= ltMax+lineTol {
						intersects = true
						if entry := clamp(ltMin, 0, 1); entry < first {
							first = entry
						}
					}
				}

				if intersects {
					contacts = append(contacts, contact{
						hash: g.hash(r, c),
						when: base + first,
						row:  r,
						col:  c,
					})
				}
			}
		}

		base++
		u0 = u1
	}

	// A single-point polyline: only the endpoint incident cells apply.
	if len(points) == 1 {
		rs := g.rowsAt(points[0].Lat)
		cs := g.colsAt(points[0].Lng)
		for _, r := range rs {
			for _, c := range cs {
				contacts = append(contacts, contact{hash: g.hash(r, c), when: 0, row: r, col: c})
			}
		}
	}

	sort.Slice(contacts, func(i, j int) bool {
		a, b := contacts[i], contacts[j]
		if math.Abs(a.when-b.when) > lineTol {
			return a.when < b.when
		}
		if a.row != b.row {
			return a.row < b.row
		}
		return a.col < b.col
	})

	seen := map[uint64]bool{}
	var out []uint64
	for _, ct := range contacts {
		if seen[ct.hash] {
			continue
		}
		seen[ct.hash] = true
		out = append(out, ct.hash)
	}
	return out
}
