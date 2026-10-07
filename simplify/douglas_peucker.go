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

	found := dpWorkerRange(ls, s.Threshold, mask, 0, len(ls)-1)
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

// simplifyAnchors runs Douglas-Peucker while keeping every point whose index
// is listed in anchors. The anchors act as mandatory break points: the path is
// simplified independently between consecutive anchors (and between the end
// points and their nearest anchors), so an anchor is retained even if it is
// nearly collinear with the surrounding points. Anchors refer to positions in
// the input slice, never coordinates, so duplicate coordinates with different
// business indices are treated as distinct points. The returned index map gives
// the original index of every point in the simplified result.
//
// If any anchor index is out of range the input is left unmodified.
func (s *DouglasPeuckerSimplifier) simplifyAnchors(
	ls orb.LineString,
	anchors []int,
) (orb.LineString, []int) {
	n := len(ls)
	if n == 0 {
		return ls, nil
	}

	// Validate before mutating anything so an illegal anchor leaves the input
	// untouched.
	for _, a := range anchors {
		if a < 0 || a >= n {
			return ls, nil
		}
	}

	mask := make([]byte, n)
	mask[0] = 1
	mask[n-1] = 1

	for _, a := range anchors {
		mask[a] = 1
	}

	// Ordered, unique list of mandatory points: the end points plus all
	// anchors. Duplicate anchors only need to act as a break point once.
	breaks := make([]int, 0, len(anchors)+2)
	for i, keep := range mask {
		if keep == 1 {
			breaks = append(breaks, i)
		}
	}
	sort.Ints(breaks)

	found := len(breaks)
	for k := 1; k < len(breaks); k++ {
		start := breaks[k-1]
		end := breaks[k]
		if end-start >= 2 {
			// The two break points themselves are already counted.
			found += dpWorkerRange(ls, s.Threshold, mask, start, end) - 2
		}
	}

	result := make(orb.LineString, 0, found)
	indexMap := make([]int, 0, found)
	for i, keep := range mask {
		if keep == 1 {
			result = append(result, ls[i])
			indexMap = append(indexMap, i)
		}
	}

	return result, indexMap
}

// dpWorkerRange does the recursive threshold checks for the points between the
// given start and end indices, which are assumed to already be marked as kept.
// It returns the number of marked points in the range, including start and end.
// Using an explicit stack rather than recursion gave a 4x speed up.
func dpWorkerRange(ls orb.LineString, threshold float64, mask []byte, start, end int) int {
	found := 2

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

// LineStringWithAnchors simplifies the linestring while keeping every vertex
// whose index is listed in anchors, e.g. intersections or toll points that
// must survive road simplification even when nearly collinear. The points
// between consecutive anchors are still simplified subject to the simplifier's
// threshold. It returns the simplified linestring and an index map from each
// output point back to its position in the input. The input linestring is not
// modified. If any anchor index is out of range, including negative, the input
// is returned unmodified with a nil index map. Duplicate coordinates are
// supported: anchors identify positions, not coordinates.
func (s *DouglasPeuckerSimplifier) LineStringWithAnchors(
	ls orb.LineString,
	anchors []int,
) (orb.LineString, []int) {
	return s.simplifyAnchors(ls, anchors)
}

// MultiLineString will simplify the multi-linestring using this simplifier.
func (s *DouglasPeuckerSimplifier) MultiLineString(mls orb.MultiLineString) orb.MultiLineString {
	return multiLineString(s, mls)
}

// Ring will simplify the ring using this simplifier.
func (s *DouglasPeuckerSimplifier) Ring(r orb.Ring) orb.Ring {
	return ring(s, r)
}

// RingWithAnchors simplifies the ring while keeping every vertex whose index
// is listed in anchors. For a closed ring the shared start/end point is always
// kept and anchors are free to straddle the closing edge. Simplification
// proceeds independently between consecutive mandatory points around the ring,
// with the shared start/end point itself acting as a mandatory break so the
// two arcs on either side of the closing edge are constrained separately.
// It returns the simplified ring and an index map from each output point back
// to its position in the input. The input ring is not modified. If any anchor
// index is out of range, including negative, the input is returned unmodified
// with a nil index map.
func (s *DouglasPeuckerSimplifier) RingWithAnchors(r orb.Ring, anchors []int) (orb.Ring, []int) {
	ls, indexMap := s.simplifyAnchors(orb.LineString(r), anchors)
	return orb.Ring(ls), indexMap
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
