package buntdb

import (
	"errors"
	"math"
	"sort"
)

// GeoUnit selects the unit of distance used by the geographic point
// indexes and queries.
type GeoUnit int

const (
	// GeoMeters reports distances in meters. This is the default unit.
	GeoMeters GeoUnit = iota
	// GeoKilometers reports distances in kilometers.
	GeoKilometers
	// GeoMiles reports distances in statute miles.
	GeoMiles
)

// Mean Earth radii, one per GeoUnit. These match the radius used by the
// haversine great-circle formula, so reported distances equal
// "radius * central-angle".
const (
	earthRadiusMeters     = 6371008.8
	earthRadiusKilometers = earthRadiusMeters / 1000.0
	earthRadiusMiles      = earthRadiusMeters / 1609.344
)

// Coordinate ranges accepted by geographic indexes and queries, in degrees.
const (
	GeoLongitudeMin = -180.0
	GeoLongitudeMax = 180.0
	GeoLatitudeMin  = -90.0
	GeoLatitudeMax  = 90.0
)

// ErrInvalidCoordinates is returned when a geographic query references a
// longitude/latitude outside of longitude [-180, 180] or
// latitude [-90, 90].
var ErrInvalidCoordinates = errors.New("invalid coordinates")

// geoPoint is an indexed geographic point. Coordinates are in degrees in
// (longitude, latitude) order, matching the Point()/IndexRect() helpers.
type geoPoint struct {
	lon, lat float64
	item     *dbItem
}

// geoNode is a node of the dynamic 2D KD-tree.
type geoNode struct {
	pt     geoPoint
	left   *geoNode
	right  *geoNode
	axis   int
	alive  bool
	count  int // live points in the subtree
	nodes  int // allocated nodes in the subtree (live + dead)
	dead   int // dead points in the subtree
	height int
	minKey string
	maxKey string
	minLon float64
	maxLon float64
	minLat float64
	maxLat float64
}

// geoTree is a dynamic KD-tree over geographic points. It supports
// incremental inserts/removes and rebuilds a subtree when it becomes
// unbalanced or accumulates too many tombstones, keeping height
// logarithmic.
type geoTree struct {
	root *geoNode
	rect func(item *dbItem) (min, max []float64)
}

func geoEarthRadius(unit GeoUnit) float64 {
	switch unit {
	case GeoKilometers:
		return earthRadiusKilometers
	case GeoMiles:
		return earthRadiusMiles
	default:
		return earthRadiusMeters
	}
}

// GeoIndexRect parses an item value as a geographic point in the standard
// rect format ("[lon lat],[lon lat]"). It is the rect function intended
// for CreateGeoIndex.
//
// Values that are not valid two-dimensional points, or whose coordinates
// fall outside of longitude [-180, 180] / latitude [-90, 90], return nil
// rects and are skipped by the geographic index. Such items remain in the
// database and in any non-geographic index.
func GeoIndexRect(item string) (min, max []float64) {
	min, max = IndexRect(item)
	if !geoRectValid(min, max) {
		return nil, nil
	}
	return min, max
}

func geoRectValid(min, max []float64) bool {
	if len(min) != 2 || len(max) != 2 {
		return false
	}
	if min[0] != max[0] || min[1] != max[1] {
		return false
	}
	return geoCoordValid(min[0], min[1])
}

func geoCoordValid(lon, lat float64) bool {
	if math.IsNaN(lon) || math.IsNaN(lat) {
		return false
	}
	return lon >= GeoLongitudeMin && lon <= GeoLongitudeMax &&
		lat >= GeoLatitudeMin && lat <= GeoLatitudeMax
}

func (tr *geoTree) insert(item *dbItem) {
	var min, max []float64
	if tr.rect != nil {
		min, max = tr.rect(item)
	}
	if !geoRectValid(min, max) {
		return
	}
	pt := geoPoint{lon: min[0], lat: min[1], item: item}
	tr.root = geoInsert(tr.root, pt, 0)
}

func (tr *geoTree) remove(item *dbItem) {
	var min, max []float64
	if tr.rect != nil {
		min, max = tr.rect(item)
	}
	if !geoRectValid(min, max) {
		return
	}
	tr.root = geoRemove(tr.root, min[0], min[1], item.key, 0)
}

func (tr *geoTree) count() int {
	if tr.root == nil {
		return 0
	}
	return tr.root.count
}

func geoAxisCoord(pt geoPoint, axis int) float64 {
	if axis == 0 {
		return pt.lon
	}
	return pt.lat
}

func geoInsert(node *geoNode, pt geoPoint, depth int) *geoNode {
	axis := depth % 2
	if node == nil {
		n := &geoNode{pt: pt, axis: axis, alive: true}
		n.pull()
		return n
	}
	if node.alive && node.pt.item == pt.item {
		// re-inserting the same dbItem is a no-op
		return node
	}
	coord := geoAxisCoord(pt, axis)
	nodeCoord := geoAxisCoord(node.pt, axis)
	// The right child holds strictly-greater coordinates, or equal
	// coordinates with a greater-than-or-equal key. This matches both the
	// rebuild split and the delete search path.
	if coord > nodeCoord || (coord == nodeCoord && pt.item.key >= node.pt.item.key) {
		node.right = geoInsert(node.right, pt, depth+1)
	} else {
		node.left = geoInsert(node.left, pt, depth+1)
	}
	node.pull()
	if geoNodeImbalanced(node) {
		node = geoRebuild(node)
	}
	return node
}

// geoNodeImbalanced reports whether a subtree has grown too tall relative
// to its live size or accumulated too many tombstones. The height
// threshold (about 2*log2(n) + 2) keeps the tree within a constant factor
// of balanced even under adversarial insertion order; the tombstone
// threshold bounds wasted traversal after deletes.
func geoNodeImbalanced(node *geoNode) bool {
	if node == nil {
		return false
	}
	if node.dead > 0 && node.nodes >= 2*node.count+8 {
		return true
	}
	return node.height > 2*int(math.Ceil(math.Log2(float64(node.count+1))))+2
}

func geoRemove(node *geoNode, lon, lat float64, key string, depth int) *geoNode {
	if node == nil {
		return nil
	}
	axis := depth % 2
	coord := lon
	if axis == 1 {
		coord = lat
	}
	nodeCoord := geoAxisCoord(node.pt, axis)
	if node.alive && coord == nodeCoord && node.pt.item.key == key {
		node.alive = false
		node.pt.item = nil
		node.pull()
	} else if coord > nodeCoord || (coord == nodeCoord && key >= node.pt.item.key) {
		node.right = geoRemove(node.right, lon, lat, key, depth+1)
		node.pull()
	} else {
		node.left = geoRemove(node.left, lon, lat, key, depth+1)
		node.pull()
	}
	if node.count == 0 {
		return nil
	}
	if geoNodeImbalanced(node) {
		node = geoRebuild(node)
	}
	return node
}

func geoRebuild(node *geoNode) *geoNode {
	pts := make([]geoPoint, 0, node.count)
	geoCollect(node, &pts)
	if len(pts) == 0 {
		return nil
	}
	return geoBuild(pts, 0)
}

func geoCollect(node *geoNode, pts *[]geoPoint) {
	if node == nil {
		return
	}
	geoCollect(node.left, pts)
	if node.alive {
		*pts = append(*pts, node.pt)
	}
	geoCollect(node.right, pts)
}

func geoBuild(pts []geoPoint, depth int) *geoNode {
	if len(pts) == 0 {
		return nil
	}
	axis := depth % 2
	sort.Slice(pts, func(i, j int) bool {
		ci := geoAxisCoord(pts[i], axis)
		cj := geoAxisCoord(pts[j], axis)
		if ci != cj {
			return ci < cj
		}
		return pts[i].item.key < pts[j].item.key
	})
	mid := (len(pts) - 1) / 2
	node := &geoNode{pt: pts[mid], axis: axis, alive: true}
	node.left = geoBuild(pts[:mid], depth+1)
	node.right = geoBuild(pts[mid+1:], depth+1)
	node.pull()
	return node
}

// pull recomputes aggregate subtree metadata from the node itself and its
// two children.
func (node *geoNode) pull() {
	node.count = 0
	node.nodes = 1
	node.dead = 0
	node.height = 1
	inf := math.Inf(1)
	node.minLon, node.maxLon = inf, -inf
	node.minLat, node.maxLat = inf, -inf
	node.minKey = ""
	node.maxKey = ""
	if node.alive {
		node.count = 1
		node.minKey = node.pt.item.key
		node.maxKey = node.pt.item.key
		node.minLon, node.maxLon = node.pt.lon, node.pt.lon
		node.minLat, node.maxLat = node.pt.lat, node.pt.lat
	}
	for _, child := range []*geoNode{node.left, node.right} {
		if child == nil || child.count == 0 {
			continue
		}
		node.count += child.count
		node.nodes += child.nodes
		node.dead += child.dead
		if child.height+1 > node.height {
			node.height = child.height + 1
		}
		if node.minKey == "" || child.minKey < node.minKey {
			node.minKey = child.minKey
		}
		if child.maxKey > node.maxKey {
			node.maxKey = child.maxKey
		}
		if child.minLon < node.minLon {
			node.minLon = child.minLon
		}
		if child.maxLon > node.maxLon {
			node.maxLon = child.maxLon
		}
		if child.minLat < node.minLat {
			node.minLat = child.minLat
		}
		if child.maxLat > node.maxLat {
			node.maxLat = child.maxLat
		}
	}
}
