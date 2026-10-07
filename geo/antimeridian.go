package geo

import (
	"math"

	"github.com/paulmach/orb"
)

// SplitLineStringAntimeridian cuts a lon/lat line string into pieces at the
// antimeridian (longitude ±180) and returns the pieces in their original
// order. It is meant for consumers, e.g. map tile services, that need a
// track crossing from e.g. 179 to -179 degrees as short segments on both
// sides of the seam instead of one long line across the map.
//
// Conventions:
//   - Adjacent points are understood to be connected along the shortest
//     longitude direction. Points exactly 180 degrees apart are tied; the
//     tie is resolved towards the east (positive longitude direction).
//   - Where the path crosses the antimeridian an intersection point is
//     inserted. Its latitude is linearly interpolated between the two
//     endpoints (no great circle interpolation). The piece on the eastern
//     side ends at longitude 180 and the next piece resumes at -180, and
//     vice versa for westward crossings.
//   - A vertex lying exactly on the antimeridian never causes a cut; it
//     stays, unchanged, inside the current piece.
//   - The input is not modified and the result shares no memory with it.
//     For input with at least 2 points every returned piece has at least
//     2 points, i.e. no meaningless one-point line strings are produced.
func SplitLineStringAntimeridian(ls orb.LineString) orb.MultiLineString {
	if len(ls) == 0 {
		return nil
	}
	if len(ls) == 1 {
		return orb.MultiLineString{ls.Clone()}
	}

	result := make(orb.MultiLineString, 0, 2)
	current := make(orb.LineString, 0, len(ls))
	current = append(current, ls[0])

	prev := ls[0]
	prevUnwrapped := ls[0][0]

	for i := 1; i < len(ls); i++ {
		p := ls[i]
		delta := wrapLongitudeDelta(p[0] - prev[0])
		unwrapped := prevUnwrapped + delta

		if seam, crossed := crossedAntimeridian(prevUnwrapped, unwrapped); crossed {
			t := (seam - prevUnwrapped) / delta
			lat := prev[1] + t*(p[1]-prev[1])

			if delta > 0 {
				current = append(current, orb.Point{180, lat})
				result = append(result, current)
				current = orb.LineString{{-180, lat}}
			} else {
				current = append(current, orb.Point{-180, lat})
				result = append(result, current)
				current = orb.LineString{{180, lat}}
			}
		}

		current = append(current, p)
		prev = p
		prevUnwrapped = unwrapped
	}

	return append(result, current)
}

// wrapLongitudeDelta reduces a longitude difference to the range (-180, 180],
// i.e. the shortest way around the globe. Ties of exactly 180 degrees resolve
// to the positive (eastward) direction.
func wrapLongitudeDelta(d float64) float64 {
	d = math.Mod(d, 360)
	if d <= -180 {
		d += 360
	} else if d > 180 {
		d -= 360
	}
	return d
}

// crossedAntimeridian returns the unwrapped longitude of the antimeridian
// (180 + 360*k for some integer k) strictly between from and to, if any.
// Endpoints exactly on the seam do not count as crossings.
func crossedAntimeridian(from, to float64) (float64, bool) {
	if from < to {
		seam := 180 + 360*(math.Floor((from-180)/360)+1)
		return seam, seam < to
	}
	if from > to {
		seam := -180 + 360*(math.Ceil((from+180)/360)-1)
		return seam, seam > to
	}
	return 0, false
}
