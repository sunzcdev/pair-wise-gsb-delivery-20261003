package geohash

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// Point is a latitude/longitude pair in degrees. Latitude is in [-90, 90]
// and longitude in [-180, 180]; both +180 and -180 are accepted longitudes
// and identified as the antimeridian.
type Point struct {
	Lat float64
	Lng float64
}

// Errors returned by polyline traversal functions.
var (
	// ErrEmptyPolyline is returned when no points are provided.
	ErrEmptyPolyline = errors.New("polyline must contain at least one point")
	// ErrInvalidCoord is returned when a coordinate is NaN, +/-Inf, or outside
	// the valid latitude/longitude range.
	ErrInvalidCoord = errors.New("invalid coordinate")
	// ErrInvalidPrecision is returned when the requested bit precision is zero
	// or greater than 64.
	ErrInvalidPrecision = errors.New("precision must be between 1 and 64 bits")
)

// lineTol is the tolerance applied during polyline traversal. Traversal
// parameters are merged when they coincide within this fraction of a segment,
// and coordinates within this fraction of a cell width are treated as lying
// on the shared grid line.
const lineTol = 1e-12

// maxLineCells bounds the work and memory of a single traversal. A polyline
// at 64-bit precision could otherwise generate billions of crossing events;
// hitting this cap is reported as an incomplete result rather than an
// unbounded allocation.
const maxLineCells = 1 << 20

// LineIntWithPrecision returns the fixed-precision integer geohashes of all
// grid cells intersected by the polyline connecting the given points.
//
// The result is an exact super-cover line traversal: every cell touched by
// any segment is reported, including cells touched only at a corner or along
// a grid edge. This is not an approximation by fixed-distance sampling.
//
// Cells are emitted in the order the polyline first reaches them, with each
// distinct cell appearing at most once. At a parameter value at which several
// cells are reached simultaneously, for example a segment passing exactly
// through a grid corner, the newly reached cells are ordered by increasing
// latitude row and then increasing longitude column in the unwrapped
// coordinate frame.
//
// Consecutive vertices are joined in longitude along the shortest crossing:
// the longitude delta lies in (-180, 180] degrees, so a delta of exactly 180
// degrees is taken to cross eastward, per the documented half-turn
// convention.
//
// If limit is positive it caps the number of returned cells. When the
// polyline intersects more than limit cells the function stops at the limit
// and returns complete=false; otherwise complete=true. A limit of zero means
// unlimited.
//
// bits is the total number of precision bits, with longitude as the most
// significant bit.
func LineIntWithPrecision(points []Point, bits, limit uint) (hashes []uint64, complete bool, err error) {
	if bits == 0 || bits > 64 {
		return nil, false, ErrInvalidPrecision
	}
	if len(points) == 0 {
		return nil, false, ErrEmptyPolyline
	}
	for _, p := range points {
		if !validCoord(p) {
			return nil, false, fmt.Errorf("%w: %v", ErrInvalidCoord, p)
		}
	}

	latBits := int(bits) / 2
	lngBits := int(bits) - latBits
	grid := lineGrid{
		latRows: 1 << uint(latBits),
		lngCols: 1 << uint(lngBits),
		latBits: uint(latBits),
		lngBits: uint(lngBits),
	}

	seen := make(map[uint64]struct{})
	capped := false

	emit := func(row, col int) {
		if capped {
			return
		}
		key := grid.hash(row, col)
		if _, ok := seen[key]; ok {
			return
		}
		if uint(len(seen)) >= maxLineCells {
			capped = true
			return
		}
		seen[key] = struct{}{}
		hashes = append(hashes, key)
		if limit != 0 && uint(len(hashes)) >= limit {
			capped = true
		}
	}

	emitCells := func(rows, cols []int) {
		for _, row := range rows {
			for _, col := range cols {
				emit(row, col)
			}
		}
	}

	// The first point is emitted before any segments so a single-point
	// polyline is supported.
	unwrapped := make([]float64, len(points))
	unwrapped[0] = points[0].Lng
	emitCells(grid.rowsAt(points[0].Lat), grid.colsAt(points[0].Lng))

	for i := 1; i < len(points) && !capped; i++ {
		a := points[i-1]
		b := points[i]
		unwrapped[i] = unwrapped[i-1] + shortestLngDelta(a.Lng, b.Lng)

		events, exhausted := segmentEvents(a, b, unwrapped[i-1], unwrapped[i], grid, maxLineCells)
		if exhausted {
			capped = true
			break
		}
		sort.Slice(events, func(i, j int) bool { return events[i].t < events[j].t })

		var groups []lineEvent
		for _, ev := range events {
			if len(groups) == 0 || ev.t-groups[len(groups)-1].t > lineTol {
				groups = append(groups, lineEvent{t: ev.t})
			}
			g := &groups[len(groups)-1]
			g.rows = unionInts(g.rows, ev.rows)
			g.cols = unionInts(g.cols, ev.cols)
		}

		for gi, g := range groups {
			if gi > 0 {
				mid := (groups[gi-1].t + g.t) / 2
				if mid-groups[gi-1].t > lineTol && g.t-mid > lineTol {
					lat := a.Lat + mid*(b.Lat-a.Lat)
					lng := unwrapped[i-1] + mid*(unwrapped[i]-unwrapped[i-1])
					emitCells(grid.rowsAt(lat), grid.colsAtWorld(lng))
				}
			}
			emitCells(g.rows, g.cols)
			if capped {
				break
			}
		}
	}

	return hashes, !capped, nil
}

// LineInt is LineIntWithPrecision at full 64-bit precision.
func LineInt(points []Point, limit uint) (hashes []uint64, complete bool, err error) {
	return LineIntWithPrecision(points, 64, limit)
}

// Line is the string-geohash form of LineIntWithPrecision, with chars
// base-32 characters (5 bits each, max 12).
func Line(points []Point, chars, limit uint) (hashes []string, complete bool, err error) {
	ints, complete, err := LineIntWithPrecision(points, 5*chars, limit)
	if err != nil {
		return nil, false, err
	}
	hashes = make([]string, len(ints))
	for i, h := range ints {
		hashes[i] = ConvertIntToString(h, chars)
	}
	return hashes, complete, nil
}

// lineGrid describes the cell decomposition at a fixed precision.
type lineGrid struct {
	latRows int  // number of latitude rows
	lngCols int  // number of longitude columns
	latBits uint // latitude index bits
	lngBits uint // longitude index bits
}

// hash builds the precision-truncated integer geohash of the cell at row r
// and unwrapped column c. Columns wrap periodically at the antimeridian.
func (g lineGrid) hash(r, c int) uint64 {
	row := int64(r)
	if row < 0 {
		row = 0
	} else if row >= int64(g.latRows) {
		row = int64(g.latRows) - 1
	}
	col := wrapInt64(int64(c), int64(g.lngCols))
	// Lift the cell indices to the top of their 32-bit words so the
	// interleaved precision bits are the most significant, then truncate to
	// bits, matching EncodeIntWithPrecision.
	fullRow := uint32(row) << uint(32-g.latBits)
	fullCol := uint32(col) << uint(32-g.lngBits)
	return interleave(fullRow, fullCol) >> (64 - g.latBits - g.lngBits)
}

// rowsAt returns the latitude rows incident to a latitude value. Interior
// values return one row; internal grid lines return the rows on both sides.
func (g lineGrid) rowsAt(lat float64) []int {
	lat = clamp(lat, -90, 90)
	f := (lat + 90) / 180 * float64(g.latRows)
	k := int(math.Floor(f + lineTol))
	if k < 0 {
		k = 0
	}
	if k >= g.latRows {
		k = g.latRows - 1
	}
	if math.Abs(f-math.Round(f)) <= lineTol && k > 0 && k < g.latRows {
		return []int{k - 1, k}
	}
	return []int{k}
}

// colsAt returns the longitude columns incident to a longitude in
// [-180, 180]. The antimeridian returns the columns on both sides.
func (g lineGrid) colsAt(lng float64) []int {
	return g.colsAtWorld(wrapLng180(lng))
}

// colsAtWorld returns the longitude columns incident to an unwrapped
// longitude, allowing values outside [-180, 180].
func (g lineGrid) colsAtWorld(lng float64) []int {
	f := (lng + 180) / 360 * float64(g.lngCols)
	k := int(math.Floor(f + lineTol))
	if math.Abs(f-math.Round(f)) <= lineTol {
		return []int{k - 1, k}
	}
	return []int{k}
}

// lineEvent marks a point on a segment (parameter t in [0, 1]) and the grid
// rows and columns reached there.
type lineEvent struct {
	t    float64
	rows []int
	cols []int
}

// segmentEvents constructs the endpoint and grid-crossing events of one
// segment. u0 and u1 are the unwrapped endpoint longitudes. exhausted is true
// when more than budget crossings would be generated.
func segmentEvents(a, b Point, u0, u1 float64, g lineGrid, budget uint) (events []lineEvent, exhausted bool) {
	events = []lineEvent{
		{t: 0, rows: g.rowsAt(a.Lat), cols: g.colsAt(a.Lng)},
		{t: 1, rows: g.rowsAt(b.Lat), cols: g.colsAt(b.Lng)},
	}

	// Latitude grid-line crossings. At such a crossing the latitude index is
	// exact; the longitude uses membership in the potentially adjacent column.
	if dLat := b.Lat - a.Lat; dLat != 0 && g.latRows > 1 {
		first := 1
		last := g.latRows - 1
		if dLat < 0 {
			first, last = last, first
		}
		for k := first; ; k += sign(dLat) {
			if uint(len(events)) >= budget {
				return nil, true
			}
			lat := -90 + 180*float64(k)/float64(g.latRows)
			t := (lat - a.Lat) / dLat
			if t > lineTol && t < 1-lineTol {
				lng := u0 + t*(u1-u0)
				events = append(events, lineEvent{
					t:    t,
					rows: []int{k - 1, k},
					cols: g.colsAtWorld(lng),
				})
			}
			if k == last {
				break
			}
		}
	}

	// Longitude grid-line crossings in the unwrapped frame, so antimeridian
	// crossings require no special casing.
	if dLng := u1 - u0; dLng != 0 {
		f0 := (u0 + 180) / 360 * float64(g.lngCols)
		f1 := (u1 + 180) / 360 * float64(g.lngCols)
		k0 := int(math.Ceil(math.Min(f0, f1) - lineTol))
		k1 := int(math.Floor(math.Max(f0, f1) + lineTol))
		for k := k0; k <= k1; k++ {
			if uint(len(events)) >= budget {
				return nil, true
			}
			f := float64(k)
			t := (f - f0) / (f1 - f0)
			if t > lineTol && t < 1-lineTol {
				lat := a.Lat + t*(b.Lat-a.Lat)
				events = append(events, lineEvent{
					t:    t,
					rows: g.rowsAt(lat),
					cols: []int{k - 1, k},
				})
			}
		}
	}

	return events, false
}

// shortestLngDelta returns the signed longitude change from a to b along the
// shortest crossing. The result is in (-180, 180]; a delta of exactly 180
// degrees is positive (eastward).
func shortestLngDelta(a, b float64) float64 {
	d := wrapLng180(b - a)
	if d == -180 {
		return 180
	}
	return d
}

// wrapLng180 canonicalizes a longitude into [-180, 180].
func wrapLng180(x float64) float64 {
	x = math.Mod(x+180, 360)
	if x < 0 {
		x += 360
	}
	return x - 180
}

// wrapInt64 maps an integer index into [0, n).
func wrapInt64(x, n int64) int64 {
	x %= n
	if x < 0 {
		x += n
	}
	return x
}

func validCoord(p Point) bool {
	if math.IsNaN(p.Lat) || math.IsInf(p.Lat, 0) ||
		math.IsNaN(p.Lng) || math.IsInf(p.Lng, 0) {
		return false
	}
	return p.Lat >= -90 && p.Lat <= 90 && p.Lng >= -180 && p.Lng <= 180
}

func clamp(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

func sign(x float64) int {
	if x > 0 {
		return 1
	}
	return -1
}

// unionInts merges two sorted, deduplicated slices into one.
func unionInts(a, b []int) []int {
	out := make([]int, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		var v int
		switch {
		case j == len(b) || (i < len(a) && a[i] < b[j]):
			v = a[i]
			i++
		case i == len(a) || b[j] < a[i]:
			v = b[j]
			j++
		default:
			v = a[i]
			i++
			j++
		}
		out = append(out, v)
	}
	return out
}
