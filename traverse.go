package geohash

import (
	"errors"
	"math"
	"sort"
)

// TraversePoint is one vertex of a polyline given as (latitude, longitude) in
// degrees.
type TraversePoint struct {
	Lat float64
	Lng float64
}

// TraverseResult reports the outcome of a polyline grid traversal.
//
// Cells holds every grid cell intersected by the polyline, deduplicated and in
// first-contact order: a cell appears at the position where the polyline first
// touches it. When Truncated is true, the caller supplied a limit and the
// traversal produced more cells than that limit; in that case Cells contains
// only the first limit entries and the result must not be treated as complete.
type TraverseResult struct {
	Cells     []uint64
	Truncated bool
}

// TraversePolyline returns every fixed-precision integer geohash cell
// intersected by the polyline formed by points, in first-contact order.
//
// Unlike encoding each sampled vertex, this walks the line segments
// analytically, so cells crossed between two sample points are never lost. The
// output is deduplicated while preserving first-contact order.
//
// bits is the integer-geohash precision (as used by EncodeIntWithPrecision),
// which fixes the grid: latitude is divided into 2^floor(bits/2) rows and
// longitude into 2^ceil(bits/2) columns. bits must be between 2 and 64.
//
// Conventions:
//
//   - Longitude is wrapped along the shortest crossing direction. A segment
//     whose longitude difference is exactly half the globe (180 degrees) is
//     treated as travelling eastward.
//   - A point or segment lying on a grid line belongs to both adjacent cells.
//   - At a grid corner, the up to four incident cells are all emitted, ordered
//     by (column, row) index.
//   - Repeated or consecutive identical vertices are handled as a single
//     contact.
//
// limit, when greater than zero, caps the number of returned cells. If more
// than limit distinct cells are intersected, TraverseResult.Truncated is set
// to true. A zero limit means unlimited.
func TraversePolyline(points []TraversePoint, bits, limit uint) (TraverseResult, error) {
	if bits < 2 || bits > 64 {
		return TraverseResult{}, errors.New("bits must be between 2 and 64")
	}

	latBits := bits / 2
	lngBits := bits - latBits
	rows := int64(1) << latBits
	cols := int64(1) << lngBits

	latHeight := 180.0 / float64(rows)
	lngWidth := 360.0 / float64(cols)

	res := TraverseResult{Cells: []uint64{}}
	seen := make(map[uint64]struct{})

	emit := func(rawX, rawY int64) bool {
		if rawY < 0 {
			rawY = 0
		} else if rawY >= rows {
			rawY = rows - 1
		}

		col := rawX % cols
		if col < 0 {
			col += cols
		}

		hash := cellHash(col, rawY, latBits, lngBits)
		if _, ok := seen[hash]; ok {
			return true
		}
		if limit > 0 && uint(len(res.Cells)) >= limit {
			res.Truncated = true
			return false
		}
		seen[hash] = struct{}{}
		res.Cells = append(res.Cells, hash)
		return true
	}

	for i := 0; i+1 < len(points); i++ {
		a := points[i]
		b := points[i+1]

		if err := validatePoint(a); err != nil {
			return TraverseResult{}, err
		}
		if err := validatePoint(b); err != nil {
			return TraverseResult{}, err
		}

		// Unwrap longitude into continuous grid coordinates, choosing the
		// shortest direction. An exactly half-globe span goes east.
		gx0 := (a.Lng + 180) / lngWidth
		gx1 := (b.Lng + 180) / lngWidth
		dx := gx1 - gx0
		half := float64(cols) / 2
		// Tolerance of ~1e-9 degrees (~0.1 mm at the equator) for deciding
		// whether a span is exactly half the globe.
		halfEps := 1e-9 / lngWidth
		switch {
		case dx > half+halfEps:
			// Shortest path goes west.
			gx1 -= float64(cols)
		case dx < -half+halfEps:
			// Shortest path goes east; the exact-half case dx == -half is
			// included here, honouring the eastward convention.
			gx0 -= float64(cols)
		}

		gy0 := (a.Lat + 90) / latHeight
		gy1 := (b.Lat + 90) / latHeight

		if !traverseSegment(gx0, gy0, gx1, gy1, cols, rows, emit) {
			return res, nil
		}
	}

	if len(points) == 1 {
		p := points[0]
		if err := validatePoint(p); err != nil {
			return TraverseResult{}, err
		}
		gx := (p.Lng + 180) / lngWidth
		gy := (p.Lat + 90) / latHeight
		// A zero-length segment emits every cell incident to the point.
		traverseSegment(gx, gy, gx, gy, cols, rows, emit)
	}

	return res, nil
}

func validatePoint(p TraversePoint) error {
	if math.IsNaN(p.Lat) || math.IsNaN(p.Lng) {
		return errors.New("coordinates must not be NaN")
	}
	if p.Lat < -90 || p.Lat > 90 {
		return errors.New("latitude must be in [-90, 90]")
	}
	if p.Lng < -180 || p.Lng > 180 {
		return errors.New("longitude must be in [-180, 180]")
	}
	return nil
}

// TraversePolylineString is TraversePolyline with the cells returned as string
// geohashes of the given precision in characters (max 12).
func TraversePolylineString(points []TraversePoint, chars, limit uint) (cells []string, truncated bool, err error) {
	if chars > 12 || chars == 0 {
		return nil, false, errors.New("chars must be between 1 and 12")
	}
	bits := uint(5 * chars)
	res, err := TraversePolyline(points, bits, limit)
	if err != nil {
		return nil, false, err
	}
	cells = make([]string, len(res.Cells))
	for i, h := range res.Cells {
		cells[i] = ConvertIntToString(h, chars)
	}
	return cells, res.Truncated, nil
}

// gridIndex maps a coordinate value measured from the grid origin to the
// index of the cell that contains it. A value on a grid line snaps to the
// higher-index cell, which together with event-time emission implements the
// "grid line belongs to both sides" convention.
func gridIndex(v, step float64) int64 {
	q := math.Floor(v / step)
	r := math.Remainder(v, step)
	const eps = 1e-6
	if math.Abs(r) < eps*step {
		q = math.Round(v / step)
	}
	return int64(q)
}

// traverseSegment emits, in first-contact order, the cells touched by the
// closed segment from grid point (x0, y0) to (x1, y1), including both
// endpoints, using unwrapped (possibly out of range) longitude coordinates.
// emit returns false to stop early once a result limit is reached.
func traverseSegment(x0, y0, x1, y1 float64, cols, rows int64, emit func(rawX, rawY int64) bool) bool {
	type event struct {
		t    float64
		xHit bool
		yHit bool
	}

	// Event parameters: every grid-line crossing in each axis, plus both
	// endpoints. Sweeping them in increasing parameter order visits contacts
	// exactly and handles corner crossings and axis-aligned edge runs without
	// any sampling.
	events := []event{
		{t: 0, xHit: onGridLine(x0), yHit: onGridLine(y0)},
		{t: 1, xHit: onGridLine(x1), yHit: onGridLine(y1)},
	}

	addCrossings := func(a, b float64) {
		if b == a {
			return
		}
		lo := math.Min(a, b)
		hi := math.Max(a, b)
		first := math.Ceil(lo)
		last := math.Floor(hi)
		for k := int64(first); k <= int64(last); k++ {
			fk := float64(k)
			if fk <= lo || fk >= hi {
				continue
			}
			events = append(events, event{t: (fk - a) / (b - a)})
		}
	}

	// Tolerance for deciding two crossing parameters coincide at a grid
	// corner. Scaled by floating-point magnitude so that adjacent (but
	// distinct) sub-nanoscale crossings at 64-bit precision are not merged.
	epsFor := func(t float64) float64 {
		return 8e-16 * (1 + math.Abs(t))
	}

	before := len(events)
	addCrossings(x0, x1)
	for i := before; i < len(events); i++ {
		events[i].xHit = true
	}
	before = len(events)
	addCrossings(y0, y1)
	for i := before; i < len(events); i++ {
		events[i].yHit = true
	}

	// Group crossings that occur at the same parameter (grid corners).
	sort.Slice(events, func(i, j int) bool { return events[i].t < events[j].t })

	prevT := 0.0
	xConstOnLine := x1 == x0 && onGridLine(x0)
	yConstOnLine := y1 == y0 && onGridLine(y0)

	for len(events) > 0 {
		ev := events[0]
		t := ev.t
		xHit, yHit := ev.xHit, ev.yHit
		for len(events) > 0 && math.Abs(events[0].t-t) <= epsFor(t) {
			xHit = xHit || events[0].xHit
			yHit = yHit || events[0].yHit
			events = events[1:]
		}

		// Emit the interior of the interval leading into this event. A segment
		// that runs along a grid line reports both adjacent rows/columns.
		if t > prevT+epsFor(t) {
			tm := (prevT + t) / 2
			xm := x0 + tm*(x1-x0)
			ym := y0 + tm*(y1-y0)

			xIndices := []int64{gridIndex(xm, 1)}
			if xConstOnLine {
				xIndices = []int64{gridIndex(xm, 1), gridIndex(xm, 1) - 1}
			}
			yIndices := []int64{gridIndex(ym, 1)}
			if yConstOnLine {
				yIndices = []int64{gridIndex(ym, 1), gridIndex(ym, 1) - 1}
			}
			if !emitCartesian(xIndices, yIndices, cols, rows, emit) {
				return false
			}
		}

		x := x0 + t*(x1-x0)
		y := y0 + t*(y1-y0)

		// Cells touched at this event: the anchor cell plus, when on a grid
		// line, the cells on the other side of each line. Order by (column,
		// row) index for a deterministic corner convention.
		xIndices := []int64{gridIndex(x, 1)}
		if xHit || xConstOnLine {
			xIndices = append(xIndices, gridIndex(x, 1)-1)
		}
		yIndices := []int64{gridIndex(y, 1)}
		if yHit || yConstOnLine {
			yIndices = append(yIndices, gridIndex(y, 1)-1)
		}
		xIndices = uniqueSortedInt64(xIndices)
		yIndices = uniqueSortedInt64(yIndices)
		if !emitCartesian(xIndices, yIndices, cols, rows, emit) {
			return false
		}

		prevT = t
	}

	return true
}

// onGridLine reports whether v sits on an integer grid line.
func onGridLine(v float64) bool {
	return math.Abs(math.Remainder(v, 1)) < 1e-9
}

// emitCartesian emits the Cartesian product of column and row indices, ordered
// by wrapped (column, row) index so the corner convention stays stable across
// the antimeridian and the poles.
func emitCartesian(xs, ys []int64, cols, rows int64, emit func(rawX, rawY int64) bool) bool {
	key := func(v, mod int64) int64 {
		k := v % mod
		if k < 0 {
			k += mod
		}
		return k
	}
	xSorted := sortedByKey(xs, func(v int64) int64 { return key(v, cols) })
	ySorted := sortedByKey(ys, func(v int64) int64 {
		if v < 0 {
			return 0
		}
		if v >= rows {
			return rows - 1
		}
		return v
	})
	for _, x := range xSorted {
		for _, y := range ySorted {
			if !emit(x, y) {
				return false
			}
		}
	}
	return true
}

// cellHash encodes a wrapped column and row into an integer geohash with the
// given number of longitude and latitude bits, matching
// EncodeIntWithPrecision.
func cellHash(col, row int64, latBits, lngBits uint) uint64 {
	latShift := uint(32) - latBits
	lngShift := uint(32) - lngBits
	lat := uint32(row) << latShift
	lng := uint32(col) << lngShift
	hash := interleave(lat, lng)
	totalBits := latBits + lngBits
	return hash >> (64 - totalBits)
}

func sortedInt64(a []int64) []int64 {
	out := append([]int64(nil), a...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// sortedByKey returns a deduplicated, sorted by key(v). Equal-key inputs are
// emitted once (they map to the same wrapped cell).
func sortedByKey(a []int64, key func(int64) int64) []int64 {
	out := append([]int64(nil), a...)
	sort.Slice(out, func(i, j int) bool {
		ki, kj := key(out[i]), key(out[j])
		if ki != kj {
			return ki < kj
		}
		return out[i] < out[j]
	})
	if len(out) < 2 {
		return out
	}
	uniq := out[:1]
	for _, v := range out[1:] {
		if key(v) != key(uniq[len(uniq)-1]) {
			uniq = append(uniq, v)
		}
	}
	return uniq
}

// uniqueSortedInt64 sorts a and removes duplicates, preserving ascending
// order.
func uniqueSortedInt64(a []int64) []int64 {
	s := sortedInt64(a)
	if len(s) < 2 {
		return s
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
