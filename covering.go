package geohash

import (
	"errors"
	"fmt"
	"math"
)

// CoveringConfig controls the behavior of Covering.
type CoveringConfig struct {
	// MaxChars is the maximum length, in characters, of the returned
	// geohash prefixes. Zero defaults to the maximum of 12.
	MaxChars uint

	// MaxCells is a budget on the number of returned prefixes. If the
	// covering at MaxChars precision does not fit, coarser precisions are
	// used until it does. Zero means no limit; note that an unlimited
	// budget combined with a large radius and high precision can be
	// expensive.
	MaxCells int
}

// coveringTolerance is the slack, in radians, added to the query radius
// when deciding whether a cell may be discarded. It guards against floating
// point error in the distance computations so that the covering stays
// conservative.
const coveringTolerance = 1e-9

// Covering returns a conservative set of geohash prefixes covering the
// spherical cap of the given radius around the point (lat, lng). Distances
// are measured on a sphere of radius sphereRadius, in the same units as
// radius; no ellipsoid model is used.
//
// A cell is excluded only when it is provably disjoint from the cap: the
// test uses the minimum distance from the center to the whole cell, never
// the distance to the cell center. Therefore every point within radius of
// the center is contained in at least one returned prefix.
//
// Caps crossing the antimeridian or enclosing a pole are handled; a cap
// containing a pole spans the full range of longitudes. The radius must not
// exceed half the sphere circumference (pi*sphereRadius); larger radii are
// rejected with an error. A radius of exactly half the circumference covers
// the entire sphere.
//
// All returned prefixes have the same length of at most cfg.MaxChars
// characters. If cfg.MaxCells is too small for the covering at that
// precision, the result is coarsened to the finest precision that fits the
// budget. In the extreme case the result is a single empty prefix, which
// matches the whole sphere.
func Covering(lat, lng, radius, sphereRadius float64, cfg CoveringConfig) ([]string, error) {
	if math.IsNaN(lat) || lat < -90 || lat > 90 {
		return nil, fmt.Errorf("geohash: invalid latitude %v", lat)
	}
	if math.IsNaN(lng) || math.IsInf(lng, 0) {
		return nil, fmt.Errorf("geohash: invalid longitude %v", lng)
	}
	if math.IsNaN(radius) || radius < 0 {
		return nil, fmt.Errorf("geohash: invalid radius %v", radius)
	}
	if math.IsNaN(sphereRadius) || math.IsInf(sphereRadius, 0) || sphereRadius <= 0 {
		return nil, fmt.Errorf("geohash: invalid sphere radius %v", sphereRadius)
	}
	if radius > math.Pi*sphereRadius {
		return nil, fmt.Errorf("geohash: radius %v exceeds half the sphere circumference", radius)
	}
	maxChars := cfg.MaxChars
	if maxChars == 0 {
		maxChars = 12
	}
	if maxChars > 12 {
		return nil, fmt.Errorf("geohash: max chars %d exceeds 12", maxChars)
	}
	if cfg.MaxCells < 0 {
		return nil, fmt.Errorf("geohash: invalid max cells %d", cfg.MaxCells)
	}

	lng = wrapLng(lng)
	alpha := radius / sphereRadius

	// Refine the covering level by level, keeping the finest level that
	// fits the cell budget. The number of covering cells is nondecreasing
	// in precision, so the search can stop as soon as a level exceeds the
	// budget.
	best := []string{""}
	level := best
	for chars := uint(1); chars <= maxChars; chars++ {
		var next []string
		for _, parent := range level {
			for i := 0; i < 32; i++ {
				child := parent + string(base32encoding.encode[i])
				if circleMayIntersectBox(lat, lng, alpha, BoundingBox(child)) {
					next = append(next, child)
				}
			}
		}
		if len(next) == 0 {
			break
		}
		if cfg.MaxCells > 0 && len(next) > cfg.MaxCells {
			break
		}
		best = next
		level = next
	}
	if len(best) == 0 {
		return nil, errors.New("geohash: empty covering")
	}
	return best, nil
}

// circleMayIntersectBox reports whether the spherical cap of angular radius
// alpha (in radians) around (lat, lng) may intersect the box. It is
// conservative: false is returned only when the box is provably disjoint
// from the cap.
func circleMayIntersectBox(lat, lng, alpha float64, b Box) bool {
	return minAngularDistanceToBox(lat, lng, b) <= alpha+coveringTolerance
}

// minAngularDistanceToBox returns the minimum angular distance, in radians,
// from the point (lat, lng) to any point of the box. Latitudes and
// longitudes are in degrees. Longitude differences are measured on the
// circle, so boxes adjacent to the antimeridian are handled correctly.
func minAngularDistanceToBox(lat, lng float64, b Box) float64 {
	// Longitude offset from the point to the box: zero if lng falls inside
	// the box's longitude range, otherwise the wrapped angular distance to
	// the nearest edge.
	var dLng float64
	if lng < b.MinLng || lng > b.MaxLng {
		dLng = math.Min(
			math.Abs(wrapLng(lng-b.MinLng)),
			math.Abs(wrapLng(lng-b.MaxLng)),
		)
	}

	rad := math.Pi / 180
	latRad := lat * rad
	sinLat, cosLat := math.Sin(latRad), math.Cos(latRad)
	cosDLng := math.Cos(dLng * rad)

	// The closest point of the box lies on the nearest longitude edge, at
	// the latitude x maximizing
	//
	//	cos(d) = sin(lat)sin(x) + cos(lat)cos(x)cos(dLng)
	//
	// The unconstrained maximizer is x* = atan2(sin(lat),
	// cos(lat)cos(dLng)); the maximum over the box's latitude range is
	// attained at x* clamped to the range or at an edge. Evaluating the
	// edges also covers boxes containing a pole, where the distance to the
	// pole is independent of longitude.
	xStar := math.Atan2(sinLat, cosLat*cosDLng) / rad
	maxCos := -2.0
	for _, xDeg := range []float64{b.MinLat, b.MaxLat, clamp(xStar, b.MinLat, b.MaxLat)} {
		x := xDeg * rad
		c := sinLat*math.Sin(x) + cosLat*math.Cos(x)*cosDLng
		if c > maxCos {
			maxCos = c
		}
	}
	return math.Acos(clamp(maxCos, -1, 1))
}

// wrapLng normalizes a longitude in degrees to the range [-180, 180).
func wrapLng(lng float64) float64 {
	lng = math.Mod(lng, 360)
	if lng >= 180 {
		lng -= 360
	} else if lng < -180 {
		lng += 360
	}
	return lng
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
