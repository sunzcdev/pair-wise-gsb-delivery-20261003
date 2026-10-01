package buntdb

import (
	"container/heap"
	"math"
)

// vec3 is a unit-sphere point in Earth-centered coordinates.
type vec3 struct {
	x, y, z float64
}

func geoUnitVector(lon, lat float64) vec3 {
	r := math.Pi / 180
	cl, sl := math.Cos(lat*r), math.Sin(lat*r)
	return vec3{x: cl * math.Cos(lon*r), y: cl * math.Sin(lon*r), z: sl}
}

// geoQuery is a best-first KNN traversal of a geoTree. It visits tree
// nodes in non-decreasing order of a valid lower bound on the spherical
// distance, which lets subtrees be pruned once the current k-th nearest
// result is closer than anything the subtree can contain.
type geoQuery struct {
	tree   *geoTree
	lon    float64
	lat    float64
	unit   GeoUnit
	radius float64
	// q holds the query point at three longitude shifts. A longitude box
	// never straddles the +/-180 seam in the KD-tree representation, so a
	// query near the seam can only reach a box on the other side through a
	// +/-360 degree copy. Taking the minimum distance across the three
	// copies makes the lower bound seam-aware.
	q      [3]vec3
	boxes  int // KD-tree nodes expanded (test instrumentation)
	leaves int // leaf points evaluated (test instrumentation)
}

// geoQueueItem is either a tree node (box) or a leaf point. Entries are
// ordered by (chordSquared, key), the same ordering used for final results:
// spherical distance is a monotonic function of chord length, and ties
// resolve by key.
type geoQueueItem struct {
	node   *geoNode
	pt     *geoPoint
	chord2 float64
	key    string
	// boundKey is the largest key a box subtree can contain. It is only
	// set for box entries and used in the pruning test: a box is discarded
	// only when its bound is strictly farther, or ties with a larger key.
	boundKey string
}

type geoPriorityQueue []*geoQueueItem

func (q geoPriorityQueue) Len() int { return len(q) }

// geoChordEq reports whether two squared chord lengths are treated as
// equal for ordering. Points that are geometrically equidistant (e.g.
// symmetric around the query) can differ in the last floating-point bits,
// so a relative tolerance is used to give deterministic key-ascending tie
// ordering. It corresponds to well under a millimeter of great-circle
// distance on Earth, far smaller than any meaningful distance difference.
func geoChordEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return d <= 1e-10*scale
}

// geoChordLess orders by (distance, key) with near-equal distances
// collapsing onto the key comparison.
func geoChordLess(distA float64, keyA string, distB float64, keyB string) bool {
	if !geoChordEq(distA, distB) {
		return distA < distB
	}
	return keyA < keyB
}

func (q geoPriorityQueue) Less(i, j int) bool {
	return geoChordLess(q[i].chord2, q[i].key, q[j].chord2, q[j].key)
}

func (q geoPriorityQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

func (q *geoPriorityQueue) Push(x interface{}) { *q = append(*q, x.(*geoQueueItem)) }

func (q *geoPriorityQueue) Pop() interface{} {
	old := *q
	n := len(old)
	it := old[n-1]
	*q = old[:n-1]
	return it
}

// geoResult keeps the current k nearest points in a max-heap so the worst
// kept candidate is cheap to inspect and replace.
type geoResult struct {
	pt     geoPoint
	chord2 float64
}

type geoTopK []geoResult

func (r geoTopK) Len() int { return len(r) }

func (r geoTopK) Less(i, j int) bool {
	// Max-heap: "larger" means worse, i.e. farther distance, or on a tie
	// the larger key.
	if !geoChordEq(r[i].chord2, r[j].chord2) {
		return r[i].chord2 > r[j].chord2
	}
	return r[i].pt.item.key > r[j].pt.item.key
}

func (r geoTopK) Swap(i, j int) { r[i], r[j] = r[j], r[i] }

func (r *geoTopK) Push(x interface{}) { *r = append(*r, x.(geoResult)) }

func (r *geoTopK) Pop() interface{} {
	old := *r
	n := len(old)
	it := old[n-1]
	*r = old[:n-1]
	return it
}

// knn runs the nearest-neighbor search and calls iter for at most k points,
// nearest first, with the great-circle distance in the requested unit.
func (q *geoQuery) knn(k int, iter func(key, value string, dist float64) bool) {
	if k <= 0 || q.tree == nil || q.tree.root == nil || q.tree.root.count == 0 {
		return
	}
	pq := &geoPriorityQueue{}
	heap.Init(pq)
	root := q.tree.root
	heap.Push(pq, &geoQueueItem{
		node:     root,
		chord2:   q.boxLowerBound(root),
		key:      root.minKey,
		boundKey: root.maxKey,
	})

	var top geoTopK
	heap.Init(&top)

	for pq.Len() > 0 {
		entry := heap.Pop(pq).(*geoQueueItem)
		if entry.node != nil {
			// Prune a subtree only when its lower bound is strictly worse
			// than the current k-th nearest point even after allowing
			// equality slack for the subtree's smallest possible key.
			if top.Len() == k {
				worst := &top[0]
				if !boxCouldBeat(entry.chord2, worst.chord2, entry.boundKey, worst.pt.item.key) {
					continue
				}
			}
			q.expand(entry.node, pq)
			continue
		}
		// Once the best remaining leaf cannot beat the current k-th
		// nearest point, search is finished (queue is ordered).
		if top.Len() == k && !q.entryBetter(entry, &top[0]) {
			break
		}
		q.leaves++
		res := geoResult{pt: *entry.pt, chord2: entry.chord2}
		if top.Len() < k {
			heap.Push(&top, res)
		} else if q.resultBetter(res, top[0]) {
			top[0] = res
			heap.Fix(&top, 0)
		}
	}

	// Drain the max-heap into ascending (distance, key) order.
	final := make([]geoResult, 0, top.Len())
	for top.Len() > 0 {
		final = append(final, heap.Pop(&top).(geoResult))
	}
	for i := len(final) - 1; i >= 0; i-- {
		r := final[i]
		if !iter(r.pt.item.key, r.pt.item.val, q.chordToDistance(r.chord2)) {
			return
		}
	}
}

func (q *geoQuery) entryBetter(entry *geoQueueItem, worst *geoResult) bool {
	return geoChordLess(entry.chord2, entry.key, worst.chord2, worst.pt.item.key)
}

func (q *geoQuery) resultBetter(a geoResult, worst geoResult) bool {
	return geoChordLess(a.chord2, a.pt.item.key, worst.chord2, worst.pt.item.key)
}

// boxCouldBeat decides whether a box subtree with lower bound bound2 and
// largest contained key boxMaxKey can hold a point better than the current
// worst result (worst2, worstKey). The box only beats when its bound is
// closer, or on a distance tie it still contains a key smaller than
// worstKey.
func boxCouldBeat(bound2, worst2 float64, boxMaxKey, worstKey string) bool {
	if !geoChordEq(bound2, worst2) {
		return bound2 < worst2
	}
	// On a distance tie the subtree could produce a better ordering only
	// if it contains a key smaller than the worst kept key.
	return boxMaxKey < worstKey
}

func (q *geoQuery) expand(node *geoNode, pq *geoPriorityQueue) {
	q.boxes++
	if node.alive {
		chord2 := geoPointChordSquared(q, node.pt.lon, node.pt.lat)
		heap.Push(pq, &geoQueueItem{
			pt:     &node.pt,
			chord2: chord2,
			key:    node.pt.item.key,
		})
	}
	for _, child := range []*geoNode{node.left, node.right} {
		if child != nil && child.count > 0 {
			heap.Push(pq, &geoQueueItem{
				node:     child,
				chord2:   q.boxLowerBound(child),
				key:      child.minKey,
				boundKey: child.maxKey,
			})
		}
	}
}

// chordToDistance converts a squared unit-sphere chord length to a
// great-circle distance: chord = 2*sin(theta/2).
func (q *geoQuery) chordToDistance(chord2 float64) float64 {
	chord := math.Sqrt(math.Max(0, chord2))
	half := math.Min(1, chord/2)
	return q.radius * 2 * math.Asin(half)
}

// box is a 3D axis-aligned bounding box of the unit-sphere points that a
// longitude/latitude rectangle maps to.
type geoBox struct {
	minX, maxX float64
	minY, maxY float64
	minZ, maxZ float64
}

// geoBoxFor computes the tight-enclosing Cartesian box of every point with
// longitude in [minLon, maxLon] and latitude in [minLat, maxLat].
//
// Points at latitude lat are (r*cos(lon), r*sin(lon), sin(lat)) where
// r = cos(lat). As lat ranges over the interval, r ranges over
// [rmin, rmax]: crossing the equator makes rmax = 1 (the equator is
// included) and rmin = 0; within one hemisphere both are endpoint cosines.
// The x/y extrema combine that radius range with the exact min/max of
// cos/sin over the (non-wrapping) longitude interval, and z is simply
// [sin(minLat), sin(maxLat)].
func geoBoxFor(minLon, maxLon, minLat, maxLat float64) geoBox {
	r := math.Pi / 180
	cosA, cosB := math.Cos(minLat*r), math.Cos(maxLat*r)
	rmax, rmin := 1.0, 0.0
	if (minLat >= 0 && maxLat >= 0) || (minLat <= 0 && maxLat <= 0) {
		rmax = math.Max(math.Abs(cosA), math.Abs(cosB))
		rmin = math.Min(math.Abs(cosA), math.Abs(cosB))
	}
	cMin, cMax := trigIntervalExtrema(minLon, maxLon, true)
	sMin, sMax := trigIntervalExtrema(minLon, maxLon, false)
	// For a coefficient c in [cMin,cMax] and radius in [rmin,rmax], the
	// extrema of r*c pick rmax for the far-from-zero side and rmin on the
	// near side.
	xMin, xMax := 0.0, 0.0
	if cMin < 0 {
		xMin = rmax * cMin
	} else {
		xMin = rmin * cMin
	}
	if cMax > 0 {
		xMax = rmax * cMax
	} else {
		xMax = rmin * cMax
	}
	yMin, yMax := 0.0, 0.0
	if sMin < 0 {
		yMin = rmax * sMin
	} else {
		yMin = rmin * sMin
	}
	if sMax > 0 {
		yMax = rmax * sMax
	} else {
		yMax = rmin * sMax
	}
	return geoBox{
		minX: xMin, maxX: xMax,
		minY: yMin, maxY: yMax,
		minZ: math.Sin(minLat * r),
		maxZ: math.Sin(maxLat * r),
	}
}

// trigIntervalExtrema returns the min and max of cos(lon) (when cosine is
// true) or sin(lon) over the non-wrapping interval [minLon, maxLon] in
// degrees, considering the interval endpoints and any contained critical
// angles. Longitude intervals never straddle the +/-180 seam in the
// KD-tree.
func trigIntervalExtrema(minLon, maxLon float64, cosine bool) (mn, mx float64) {
	r := math.Pi / 180
	candidates := []float64{minLon, maxLon}
	if cosine {
		for _, t := range []float64{0, 180, -180} {
			if t >= minLon && t <= maxLon {
				candidates = append(candidates, t)
			}
		}
	} else {
		for _, t := range []float64{90, -90} {
			if t >= minLon && t <= maxLon {
				candidates = append(candidates, t)
			}
		}
	}
	mn, mx = math.Inf(1), math.Inf(-1)
	for _, lon := range candidates {
		v := math.Sin(lon * r)
		if cosine {
			v = math.Cos(lon * r)
		}
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	return mn, mx
}

func axisBoxDist(v, lo, hi float64) float64 {
	if v < lo {
		d := lo - v
		return d * d
	}
	if v > hi {
		d := v - hi
		return d * d
	}
	return 0
}

// boxLowerBound returns a lower bound, expressed as a squared unit-sphere
// chord length, from the query point to every live point in node's
// subtree.
//
// The KD-tree stores an axis-aligned longitude/latitude box. Mapping that
// box onto the unit sphere yields a set of points S, and the Cartesian box
// B = geoBoxFor(...) contains S. The squared Euclidean distance from the
// query vector to B is therefore no larger than its distance to any point
// of S, and Euclidean distance between unit vectors is exactly the chord
// length. Evaluating the query at longitude, longitude+360 and
// longitude-360 handles boxes on the opposite side of the 180th meridian.
func (q *geoQuery) boxLowerBound(node *geoNode) float64 {
	b := geoBoxFor(node.minLon, node.maxLon, node.minLat, node.maxLat)
	best := math.Inf(1)
	for i := range q.q {
		d := axisBoxDist(q.q[i].x, b.minX, b.maxX) +
			axisBoxDist(q.q[i].y, b.minY, b.maxY) +
			axisBoxDist(q.q[i].z, b.minZ, b.maxZ)
		if d < best {
			best = d
		}
	}
	// Scale down by a hair to absorb floating-point rounding; the value can
	// only become a weaker (still valid) bound, never stronger.
	return math.Max(0, best) * (1 - 1e-12)
}

// geoPointChordSquared returns the exact squared chord length from the
// query point to (lon, lat), using the longitude shift that gives the
// shortest separation across the seam.
func geoPointChordSquared(q *geoQuery, lon, lat float64) float64 {
	p := geoUnitVector(lon, lat)
	best := math.Inf(1)
	for i := range q.q {
		dx := q.q[i].x - p.x
		dy := q.q[i].y - p.y
		dz := q.q[i].z - p.z
		d := dx*dx + dy*dy + dz*dz
		if d < best {
			best = d
		}
	}
	if best > 4 {
		best = 4
	}
	return best
}

// GeoDistance reports the great-circle distance between two geographic
// points. Longitudes are in [-180, 180] and latitudes in [-90, 90], in
// degrees. Out-of-range coordinates return ErrInvalidCoordinates.
func GeoDistance(lon1, lat1, lon2, lat2 float64, unit GeoUnit) (float64, error) {
	if !geoCoordValid(lon1, lat1) || !geoCoordValid(lon2, lat2) {
		return 0, ErrInvalidCoordinates
	}
	a := geoUnitVector(lon1, lat1)
	b := geoUnitVector(lon2, lat2)
	dx := a.x - b.x
	dy := a.y - b.y
	dz := a.z - b.z
	chord := math.Sqrt(math.Min(4, dx*dx+dy*dy+dz*dz))
	return geoEarthRadius(unit) * 2 * math.Asin(math.Min(1, chord/2)), nil
}
