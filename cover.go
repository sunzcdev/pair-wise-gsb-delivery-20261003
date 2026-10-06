package geohash

import (
	"math"
	"sort"
)

// CoverCircle returns a set of geohash prefixes whose union conservatively
// covers the spherical cap of the given radius around the center point
// (lat, lng). It is intended for dispatch-style queries: every point within
// radius of the center is guaranteed to fall inside one of the returned
// geohash cells.
//
// Distances are modeled on a sphere of the caller-provided sphereRadius, in
// the same unit as radius (for example both in meters). No ellipsoid
// correction is applied or implied.
//
// The cover is refined character by character up to maxChars (clamped to the
// range [1, 12]). If budget is positive and the cover at some precision would
// exceed budget cells, the coarser cover from the previous precision level is
// returned instead. A budget of zero or less means no limit. If even the
// coarsest (single character) cover exceeds the budget, the result degrades
// to the whole sphere.
//
// A cell is discarded only when it is provably disjoint from the cap, that
// is, when the exact minimum distance from the center to any point of the
// cell exceeds the cap radius. Cell-center distance is never used to include
// or exclude a cell, so cells merely touching the cap boundary are retained.
//
// Caps crossing a pole span the full circle of longitudes; this is handled
// naturally by the exact point-to-cell distance test. If radius corresponds
// to at least half the sphere circumference (angular radius >= pi), the cap
// is the entire sphere and the result is the single empty prefix "", which
// matches every geohash.
func CoverCircle(lat, lng, radius, sphereRadius float64, maxChars uint, budget int) []string {
	if sphereRadius <= 0 || math.IsNaN(sphereRadius) || math.IsNaN(radius) || radius < 0 {
		return nil
	}
	if maxChars > 12 {
		maxChars = 12
	}
	if maxChars < 1 {
		maxChars = 1
	}

	theta := radius / sphereRadius
	if theta >= math.Pi {
		return []string{""}
	}

	var prev []string
	for chars := uint(1); chars <= maxChars; chars++ {
		cover := coverCircleAtPrecision(lat, lng, theta, chars)
		if budget > 0 && len(cover) > budget {
			if prev == nil {
				return []string{""}
			}
			return prev
		}
		prev = cover
	}
	return prev
}

// coverCircleAtPrecision returns all geohash prefixes of the given length
// whose cells may intersect the cap of angular radius theta (radians) around
// (lat, lng). Subtrees are pruned only when proven disjoint.
func coverCircleAtPrecision(lat, lng, theta float64, chars uint) []string {
	var out []string
	var walk func(prefix string)
	walk = func(prefix string) {
		if uint(len(prefix)) == chars {
			out = append(out, prefix)
			return
		}
		// Keep cells within a small relative tolerance of the cap radius, so
		// floating-point error in the distance test can only ever make the
		// cover more conservative, never drop a truly intersecting cell.
		limit := theta*(1+1e-9) + 1e-12
		for i := 0; i < 32; i++ {
			child := prefix + string(base32encoding.encode[i])
			box := BoundingBox(child)
			if minAngularDistanceToBox(lat, lng, box) <= limit {
				walk(child)
			}
		}
	}
	walk("")
	sort.Strings(out)
	return out
}

// angularDistance returns the central angle in radians between the points
// (lat1, lng1) and (lat2, lng2), using the haversine formula for numerical
// stability.
func angularDistance(lat1, lng1, lat2, lng2 float64) float64 {
	phi1 := lat1 * math.Pi / 180
	phi2 := lat2 * math.Pi / 180
	dphi := phi2 - phi1
	dlambda := (lng2 - lng1) * math.Pi / 180
	a := math.Sin(dphi/2)*math.Sin(dphi/2) +
		math.Cos(phi1)*math.Cos(phi2)*math.Sin(dlambda/2)*math.Sin(dlambda/2)
	if a > 1 {
		a = 1
	}
	return 2 * math.Asin(math.Sqrt(a))
}

// lngIntervalDistance returns the circular distance in degrees from lng to
// the interval [a, b], that is zero when lng lies inside and otherwise the
// shortest wrapped distance to an endpoint.
func lngIntervalDistance(lng, a, b float64) float64 {
	if a <= lng && lng <= b {
		return 0
	}
	return math.Min(circularDegreeDistance(lng, a), circularDegreeDistance(lng, b))
}

// circularDegreeDistance returns the shortest distance in degrees between two
// longitudes on the circle, in the range [0, 180].
func circularDegreeDistance(x, y float64) float64 {
	d := math.Abs(x - y)
	if d > 180 {
		d = 360 - d
	}
	return d
}

func clampFloat(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// minAngularDistanceToBox returns the exact minimum angular distance in
// radians from the point (lat, lng) to any point of the lat/lng box. The box
// is an ordinary geohash cell: it never wraps the antimeridian and never
// crosses a pole. Longitude wrap-around of the query point is handled.
//
// The minimum is attained either on the same meridian as the point (when its
// longitude falls inside the box) or on one of the two bounding meridians of
// the box. Along a bounding meridian the distance is sinusoidal in latitude
// with a single stationary point at atan(tan(lat)/cos(dlng)); that point is
// a minimum when cos(dlng) > 0 and a maximum otherwise, so the minimum over
// the segment is found among the clamped stationary point and the two
// endpoints. This yields an exact distance, so a cell may be discarded if
// and only if the result exceeds the cap radius.
func minAngularDistanceToBox(lat, lng float64, b Box) float64 {
	clampedLat := clampFloat(lat, b.MinLat, b.MaxLat)
	dlng := lngIntervalDistance(lng, b.MinLng, b.MaxLng)
	if dlng == 0 {
		return math.Abs(lat-clampedLat) * math.Pi / 180
	}
	best := math.Inf(1)
	for _, boundary := range []float64{b.MinLng, b.MaxLng} {
		delta := circularDegreeDistance(lng, boundary)
		if delta == 0 {
			continue
		}
		cosDelta := math.Cos(delta * math.Pi / 180)
		phiStar := math.Atan(math.Tan(lat*math.Pi/180)/cosDelta) * 180 / math.Pi
		for _, phi := range []float64{
			clampFloat(phiStar, b.MinLat, b.MaxLat),
			b.MinLat,
			b.MaxLat,
		} {
			if d := angularDistance(lat, lng, phi, boundary); d < best {
				best = d
			}
		}
	}
	return best
}
