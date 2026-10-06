package geohash

import (
	"errors"
	"math"
	"sort"
)

var (
	errInvalidCoverBounds    = errors.New("geohash: invalid cover bounds")
	errInvalidCoverPrecision = errors.New("geohash: cover precision must be between 1 and 12")
	errInvalidCoverGridLimit = errors.New("geohash: maximum number of grids must be at least 1")
)

type coverInterval struct {
	min float64
	max float64
}

type coverQuery struct {
	lat coverInterval
	lng []coverInterval
}

type coverNode struct {
	hash string
	box  Box
}

func newCoverQuery(b Box) (coverQuery, error) {
	if math.IsNaN(b.MinLat) || math.IsNaN(b.MaxLat) ||
		math.IsNaN(b.MinLng) || math.IsNaN(b.MaxLng) ||
		math.IsInf(b.MinLat, 0) || math.IsInf(b.MaxLat, 0) ||
		math.IsInf(b.MinLng, 0) || math.IsInf(b.MaxLng, 0) {
		return coverQuery{}, errInvalidCoverBounds
	}
	if b.MinLat < -90 || b.MinLat > 90 || b.MaxLat < -90 || b.MaxLat > 90 {
		return coverQuery{}, errInvalidCoverBounds
	}
	if b.MinLng < -180 || b.MinLng > 180 || b.MaxLng < -180 || b.MaxLng > 180 {
		return coverQuery{}, errInvalidCoverBounds
	}
	if b.MinLat > b.MaxLat {
		return coverQuery{}, errInvalidCoverBounds
	}

	var lng []coverInterval
	if b.MinLng == b.MaxLng {
		minLng := b.MinLng
		if minLng == 180 {
			minLng = -180
		}
		lng = []coverInterval{{min: minLng, max: minLng}}
	} else {
		minLng := b.MinLng
		maxLng := b.MaxLng

		// Longitude 180 and -180 are the same meridian. Canonicalizing the
		// endpoints avoids an extra zero-width piece when a wrapping rectangle
		// starts or ends on the date line.
		if minLng == 180 {
			minLng = -180
		}
		if maxLng == -180 {
			maxLng = 180
		}

		if minLng <= maxLng {
			lng = []coverInterval{{min: minLng, max: maxLng}}
		} else {
			lng = []coverInterval{
				{min: minLng, max: 180},
				{min: -180, max: maxLng},
			}
		}
	}

	return coverQuery{
		lat: coverInterval{min: b.MinLat, max: b.MaxLat},
		lng: lng,
	}, nil
}

func (q coverQuery) contains(b Box) bool {
	if b.MinLat < q.lat.min || b.MaxLat > q.lat.max {
		return false
	}
	for _, interval := range q.lng {
		if b.MinLng >= interval.min && b.MaxLng <= interval.max {
			return true
		}
	}
	return false
}

func coverIntervalsOverlap(cellMin, cellMax, queryMin, queryMax, upperBound float64) bool {
	if queryMin < queryMax {
		return cellMin < queryMax && cellMax > queryMin
	}

	q := queryMin
	if q == upperBound {
		return cellMin <= q && cellMax == q
	}
	return cellMin <= q && q < cellMax
}

func (q coverQuery) intersects(b Box) bool {
	if !coverIntervalsOverlap(b.MinLat, b.MaxLat, q.lat.min, q.lat.max, 90) {
		return false
	}
	for _, interval := range q.lng {
		if coverIntervalsOverlap(b.MinLng, b.MaxLng, interval.min, interval.max, 180) {
			return true
		}
	}
	return false
}

func intersectingCoverNodes(node coverNode, q coverQuery) []coverNode {
	children := make([]coverNode, 0, 32)
	for i := 0; i < 32; i++ {
		hash := node.hash + string(base32encoding.encode[i])
		box := BoundingBox(hash)
		if q.intersects(box) {
			children = append(children, coverNode{hash: hash, box: box})
		}
	}
	return children
}

// Cover returns geohash prefixes forming a covering of the rectangle.
//
// maxPrecision is the finest prefix length in characters, from 1 to 12. A
// geohash cell that is completely inside the rectangle is not subdivided. The
// result never contains more than maxGrids prefixes; when refining a cell
// would exceed that budget, the coarser parent is retained instead.
//
// A rectangle with MinLng greater than MaxLng crosses the antimeridian. The
// empty prefix represents the whole-world root cell and is returned when no
// finer one-cell covering is possible. The result is sorted, contains no
// duplicates, and never contains both a prefix and one of its descendants.
func (b Box) Cover(maxPrecision uint, maxGrids int) ([]string, error) {
	if maxPrecision == 0 || maxPrecision > 12 {
		return nil, errInvalidCoverPrecision
	}
	if maxGrids < 1 {
		return nil, errInvalidCoverGridLimit
	}

	q, err := newCoverQuery(b)
	if err != nil {
		return nil, err
	}

	root := coverNode{
		hash: "",
		box:  Box{MinLat: -90, MaxLat: 90, MinLng: -180, MaxLng: 180},
	}
	if q.contains(root.box) {
		return []string{""}, nil
	}

	active := []coverNode{root}
	leaves := make([]string, 0, 1)

	for len(active) > 0 {
		node := active[0]
		active = active[1:]

		if uint(len(node.hash)) == maxPrecision {
			leaves = append(leaves, node.hash)
			continue
		}

		children := intersectingCoverNodes(node, q)
		if len(children) == 0 {
			leaves = append(leaves, node.hash)
			continue
		}

		if len(children) > maxGrids-len(leaves)-len(active) {
			leaves = append(leaves, node.hash)
			continue
		}

		for _, child := range children {
			if uint(len(child.hash)) == maxPrecision || q.contains(child.box) {
				leaves = append(leaves, child.hash)
			} else {
				active = append(active, child)
			}
		}
	}

	sort.Strings(leaves)
	return leaves, nil
}
