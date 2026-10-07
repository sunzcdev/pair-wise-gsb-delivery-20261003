package simplify

import (
	"sort"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/planar"
)

var _ orb.Simplifier = &DouglasPeuckerSimplifier{}

// A DouglasPeuckerSimplifier wraps the DouglasPeucker function.
type DouglasPeuckerSimplifier struct {
	Threshold float64
}

// DouglasPeucker creates a new DouglasPeuckerSimplifier.
func DouglasPeucker(threshold float64) *DouglasPeuckerSimplifier {
	return &DouglasPeuckerSimplifier{
		Threshold: threshold,
	}
}

func (s *DouglasPeuckerSimplifier) simplify(ls orb.LineString, area, wim bool) (orb.LineString, []int) {
	mask := make([]byte, len(ls))
	mask[0] = 1
	mask[len(mask)-1] = 1

	found := 2 + dpSimplifyRange(ls, s.Threshold, mask, 0, len(ls)-1)
	var indexMap []int
	if wim {
		indexMap = make([]int, 0, found)
	}

	count := 0
	for i, v := range mask {
		if v == 1 {
			ls[count] = ls[i]
			count++
			if wim {
				indexMap = append(indexMap, i)
			}
		}
	}

	return ls[:count], indexMap
}

// simplifyWithAnchors runs the Douglas-Peucker algorithm while guaranteeing
// that every index listed in anchors survives the simplification, even when
// the corresponding point is nearly collinear with its neighbours. The
// simplification is applied independently to each segment delimited by two
// consecutive anchors, still using the configured threshold.
//
// Anchor indices, not coordinates, identify the protected points, so two
// equal coordinates at different indices are treated as separate anchors.
// If any anchor index is outside [0, len(ls)) the input linestring is left
// untouched and returned together with a nil index map.
//
// The returned index map gives, for every point of the simplified line, the
// index of that point in the original linestring.
func (s *DouglasPeuckerSimplifier) simplifyWithAnchors(ls orb.LineString, anchors []int) (orb.LineString, []int) {
	if len(ls) <= 1 {
		return ls, nil
	}

	for _, a := range anchors {
		if a < 0 || a >= len(ls) {
			// invalid anchor index: do not modify the input
			return ls, nil
		}
	}

	// copy so reordering/dedup does not mutate the caller's slice
	bounds := make([]int, 0, len(anchors)+2)
	bounds = append(bounds, anchors...)
	// the endpoints are always kept; for closed rings this keeps both the
	// first and the repeated closing point, so an anchor across the closure
	// is handled as two ordinary (non-wrap) segments split at the seam.
	bounds = append(bounds, 0, len(ls)-1)
	sort.Ints(bounds)

	mask := make([]byte, len(ls))
	found := 0

	prev := -1
	for _, b := range bounds {
		if b == prev {
			continue
		}
		if prev >= 0 {
			found += dpSimplifyRange(ls, s.Threshold, mask, prev, b)
		}
		mask[b] = 1
		found++
		prev = b
	}

	indexMap := make([]int, 0, found)
	count := 0
	for i, v := range mask {
		if v == 1 {
			ls[count] = ls[i]
			count++
			indexMap = append(indexMap, i)
		}
	}

	return ls[:count], indexMap
}

// dpSimplifyRange marks the interior points of the inclusive range
// [start, end] that must be kept to satisfy the Douglas-Peucker threshold.
// The two endpoints are assumed to be marked by the caller. Using an explicit
// stack is ~4x faster than recursion here. It returns the number of interior
// points marked.
func dpSimplifyRange(ls orb.LineString, threshold float64, mask []byte, start, end int) int {
	found := 0
	var stack []int
	stack = append(stack, start, end)

	for len(stack) > 0 {
		start := stack[len(stack)-2]
		end := stack[len(stack)-1]

		// modify the line in place
		maxDist := 0.0
		maxIndex := 0

		for i := start + 1; i < end; i++ {
			dist := planar.DistanceFromSegmentSquared(ls[start], ls[end], ls[i])
			if dist > maxDist {
				maxDist = dist
				maxIndex = i
			}
		}

		if maxDist > threshold*threshold {
			found++
			mask[maxIndex] = 1

			stack[len(stack)-1] = maxIndex
			stack = append(stack, maxIndex, end)
		} else {
			stack = stack[:len(stack)-2]
		}
	}

	return found
}

// Simplify will run the simplification for any geometry type.
func (s *DouglasPeuckerSimplifier) Simplify(g orb.Geometry) orb.Geometry {
	return simplify(s, g)
}

// LineString will simplify the linestring using this simplifier.
func (s *DouglasPeuckerSimplifier) LineString(ls orb.LineString) orb.LineString {
	return lineString(s, ls)
}

// LineStringWithAnchors simplifies the linestring with the Douglas-Peucker
// algorithm while always keeping the points at the given anchor indices, even
// when they are nearly collinear with their neighbours. Between consecutive
// anchors the standard threshold-based simplification is applied.
//
// Anchors are matched by index, not coordinate, so duplicate coordinates at
// different business indices are preserved independently. If an anchor index
// is invalid the input linestring is returned unchanged with a nil map.
//
// It returns the simplified linestring and the index of each output point in
// the original linestring. The linestring may be modified in place, clone it
// first if the original must be preserved.
func (s *DouglasPeuckerSimplifier) LineStringWithAnchors(ls orb.LineString, anchors []int) (orb.LineString, []int) {
	return s.simplifyWithAnchors(ls, anchors)
}

// RingWithAnchors simplifies the ring with the Douglas-Peucker algorithm
// while always keeping the points at the given anchor indices. Both the first
// point and the repeated closing point of a closed ring are kept, and anchors
// that straddle the closure are honoured by simplifying the two linear
// segments on either side of the seam independently.
//
// Anchors are matched by index, not coordinate, so duplicate coordinates at
// different business indices are preserved independently. If an anchor index
// is invalid the input ring is returned unchanged with a nil map.
//
// It returns the simplified ring and the index of each output point in the
// original ring. The ring may be modified in place, clone it first if the
// original must be preserved.
func (s *DouglasPeuckerSimplifier) RingWithAnchors(r orb.Ring, anchors []int) (orb.Ring, []int) {
	ls, indexMap := s.simplifyWithAnchors(orb.LineString(r), anchors)
	return orb.Ring(ls), indexMap
}

// DouglasPeuckerWithAnchors is a convenience wrapper around
// DouglasPeucker(threshold).LineStringWithAnchors.
func DouglasPeuckerWithAnchors(threshold float64, ls orb.LineString, anchors []int) (orb.LineString, []int) {
	return DouglasPeucker(threshold).LineStringWithAnchors(ls, anchors)
}

// MultiLineString will simplify the multi-linestring using this simplifier.
func (s *DouglasPeuckerSimplifier) MultiLineString(mls orb.MultiLineString) orb.MultiLineString {
	return multiLineString(s, mls)
}

// Ring will simplify the ring using this simplifier.
func (s *DouglasPeuckerSimplifier) Ring(r orb.Ring) orb.Ring {
	return ring(s, r)
}

// Polygon will simplify the polygon using this simplifier.
func (s *DouglasPeuckerSimplifier) Polygon(p orb.Polygon) orb.Polygon {
	return polygon(s, p)
}

// MultiPolygon will simplify the multi-polygon using this simplifier.
func (s *DouglasPeuckerSimplifier) MultiPolygon(mp orb.MultiPolygon) orb.MultiPolygon {
	return multiPolygon(s, mp)
}

// Collection will simplify the collection using this simplifier.
func (s *DouglasPeuckerSimplifier) Collection(c orb.Collection) orb.Collection {
	return collection(s, c)
}
