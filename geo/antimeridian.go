package geo

import (
	"math"

	"github.com/paulmach/orb"
)

// SplitAntimeridian cuts a lon/lat LineString wherever it crosses the
// antimeridian, the ±180 degree longitude line. Each pair of adjacent points is
// interpreted along the shortest longitude direction, so for example a segment
// from longitude 179 to -179 is treated as a two degree step to the east rather
// than a 358 degree step to the west.
//
// Whenever a segment crosses the seam an intersection point is inserted on the
// boundary and the result is returned as multiple LineStrings. The piece before
// the crossing ends at +180 and the piece after it starts at -180. The
// intersection latitude is computed with linear interpolation; great circle
// interpolation is intentionally not used.
//
// Stable conventions are applied to the ambiguous cases:
//
//   - A segment whose longitude difference is exactly 180 degrees is kept in
//     place and is not considered to cross the antimeridian.
//   - Vertices that lie exactly on the seam (either +180 or -180) are shared
//     with both sides. When a contiguous run of seam vertices separates points
//     on opposite sides the run is split into two boundary points that share
//     the intersection latitude. When the run does not separate opposite sides
//     it keeps the single representation implied by the surrounding points.
//     In all cases the seam is rendered with the longitude adjacent to the
//     interior points of each piece (+180 on the western side, -180 on the
//     eastern side).
//
// The input LineString is never modified. Every returned piece contains at
// least two points. LineStrings with fewer than two points are returned as a
// single cloned piece.
func SplitAntimeridian(line orb.LineString) orb.MultiLineString {
	if len(line) < 2 {
		return orb.MultiLineString{line.Clone()}
	}

	// Unwrap the longitudes into a continuous domain, taking the shortest step
	// between adjacent points. Seam vertices (±180) are pinned to the exact
	// odd multiple of 180 they reach so a run of seam vertices shares a single
	// boundary. A raw difference of exactly 180 degrees is left untouched, the
	// tie-breaking convention for that case.
	unwrapped := make([]orb.Point, len(line))
	unwrapped[0] = line[0]
	if onSeam(unwrapped[0][0]) {
		unwrapped[0][0] = 180
	}
	for i := 1; i < len(line); i++ {
		delta := line[i][0] - line[i-1][0]
		if onSeam(line[i-1][0]) {
			delta = line[i][0] - unwrapped[i-1][0]
		}
		if delta > 180 {
			delta -= 360
		} else if delta < -180 {
			delta += 360
		}

		lon := unwrapped[i-1][0] + delta
		if onSeam(line[i][0]) {
			lon = nearestSeam(lon)
		}
		unwrapped[i] = orb.Point{lon, line[i][1]}
	}

	// cutEnd[i] marks the final vertex of a seam run that separates points on
	// opposite sides. The point finishes the current piece at +180 and starts
	// the next piece at -180.
	cutEnd := make([]bool, len(line))
	for i := 0; i < len(line); {
		if !onSeam(line[i][0]) {
			i++
			continue
		}

		runStart := i
		for i < len(line) && onSeam(line[i][0]) {
			i++
		}
		runEnd := i - 1

		boundary := unwrapped[runStart][0]
		before := seamSide(unwrapped, boundary, runStart-1)
		after := seamSide(unwrapped, boundary, runEnd+1)
		if before != 0 && after != 0 && before != after {
			cutEnd[runEnd] = true
		}
	}

	var result orb.MultiLineString

	points := []orb.Point{unwrapped[0]}
	flush := func() {
		if len(points) >= 2 {
			result = append(result, canonicalPiece(points))
		}
	}

	for i := 1; i < len(line); i++ {
		a := unwrapped[i-1]
		b := unwrapped[i]

		if boundary, frac, ok := seamCrossing(a[0], b[0]); ok {
			lat := a[1] + (b[1]-a[1])*frac
			points = append(points, orb.Point{boundary, lat})
			flush()

			points = []orb.Point{{boundary, lat}, b}
		} else {
			points = append(points, b)
		}

		if cutEnd[i] {
			flush()
			points = []orb.Point{unwrapped[i]}
		}
	}

	flush()

	return result
}

// canonicalPiece normalizes one continuous unwrapped piece back into the
// [-180, 180] longitude range. The cell is derived from any vertex strictly
// off the seam, which also selects the seam longitude (+180 or -180) adjacent
// to the interior points. A piece made up entirely of seam points renders as
// +180.
func canonicalPiece(points []orb.Point) orb.LineString {
	cell := 0.0
	haveReference := false
	for _, point := range points {
		if !onOddSeam(point[0]) {
			cell = math.Floor((point[0] + 180) / 360)
			haveReference = true
			break
		}
	}

	if !haveReference {
		// The piece consists only of seam points; render it at +180.
		cell = (points[0][0] - 180) / 360
	}

	line := make(orb.LineString, len(points))
	for i, point := range points {
		line[i] = orb.Point{point[0] - cell*360, point[1]}
	}

	return line
}

// onSeam reports whether the input longitude denotes the antimeridian. Both the
// +180 and -180 representations are accepted.
func onSeam(lon float64) bool {
	return lon == 180 || lon == -180
}

// nearestSeam snaps a longitude to the nearest odd multiple of 180, i.e. the
// nearest representation of the antimeridian in the continuous domain.
func nearestSeam(lon float64) float64 {
	return math.Round((lon+180)/360)*360 - 180
}

// onOddSeam reports whether the unwrapped longitude lies on an odd multiple of
// 180, i.e. any representation of the antimeridian.
func onOddSeam(lon float64) bool {
	return math.Mod(math.Abs(lon)-180, 360) == 0
}

// seamSide reports which side of the seam boundary a neighboring vertex lies on
// in the unwrapped domain. Strictly below is the western side (+1) and strictly
// above the eastern side (-1). An out of range index contributes no side (0).
func seamSide(unwrapped []orb.Point, boundary float64, index int) int {
	if index < 0 || index >= len(unwrapped) {
		return 0
	}
	if unwrapped[index][0] < boundary {
		return 1
	}
	return -1
}

// seamCrossing returns the antimeridian boundary (an odd multiple of 180)
// crossed strictly inside the segment from lon0 to lon1, along with the
// interpolation fraction. A boundary coinciding with either endpoint is not a
// crossing, which also implements the exactly 180 degree tie convention.
func seamCrossing(lon0, lon1 float64) (boundary float64, frac float64, ok bool) {
	lo, hi := lon0, lon1
	if lo > hi {
		lo, hi = hi, lo
	}

	first := int64(math.Floor(lo/360)) - 1
	last := int64(math.Ceil(hi/360)) + 1
	for k := first; k <= last; k++ {
		candidate := float64(k)*360 + 180
		if candidate > lo && candidate < hi {
			return candidate, (candidate - lon0) / (lon1 - lon0), true
		}
	}

	return 0, 0, false
}
