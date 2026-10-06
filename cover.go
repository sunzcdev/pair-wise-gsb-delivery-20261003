// Cover computation: approximate a latitude/longitude rectangle with a
// budgeted set of geohash prefixes of varying precision.
package geohash

import "sort"

// CoverBox returns a set of geohash prefixes whose union completely covers
// the given box. Cells may have different precisions up to maxChars
// characters: cells fully contained in the box are kept whole, while cells
// straddling the boundary are subdivided until they either fall inside,
// reach maxChars precision, or the cell budget runs out. When subdividing a
// cell would exceed maxCells, the coarser parent cell is kept instead, so
// coverage of the box is never lost. The result never contains both a cell
// and one of its ancestors, and is returned in sorted (therefore stable)
// order. Boxes crossing the antimeridian (MinLng > MaxLng) are split at the
// date line and covered by a single shared budget.
func CoverBox(box Box, maxChars uint, maxCells int) []string {
	if maxChars > 12 {
		maxChars = 12
	}
	if maxCells < 1 {
		maxCells = 1
	}

	regions := splitBox(box)

	intersects := func(b Box) bool {
		for _, r := range regions {
			if b.MinLat <= r.MaxLat && r.MinLat <= b.MaxLat &&
				b.MinLng <= r.MaxLng && r.MinLng <= b.MaxLng {
				return true
			}
		}
		return false
	}
	contained := func(b Box) bool {
		for _, r := range regions {
			if r.MinLat <= b.MinLat && b.MaxLat <= r.MaxLat &&
				r.MinLng <= b.MinLng && b.MaxLng <= r.MaxLng {
				return true
			}
		}
		return false
	}

	var cover []string
	queue := []string{""}
	for len(queue) > 0 {
		cell := queue[0]
		queue = queue[1:]

		b := BoundingBox(cell)
		if !intersects(b) {
			continue
		}
		if contained(b) || uint(len(cell)) == maxChars {
			cover = append(cover, cell)
			continue
		}

		var children []string
		for i := 0; i < 32; i++ {
			child := cell + string(base32encoding.encode[i])
			if intersects(BoundingBox(child)) {
				children = append(children, child)
			}
		}
		if len(cover)+len(queue)+len(children) <= maxCells {
			queue = append(queue, children...)
		} else {
			cover = append(cover, cell)
		}
	}

	sort.Strings(cover)
	return cover
}

// splitBox decomposes a box into one or two non-wrapping boxes, splitting at
// the antimeridian when MinLng > MaxLng.
func splitBox(box Box) []Box {
	if box.MinLng <= box.MaxLng {
		return []Box{box}
	}
	return []Box{
		{MinLat: box.MinLat, MaxLat: box.MaxLat, MinLng: box.MinLng, MaxLng: 180},
		{MinLat: box.MinLat, MaxLat: box.MaxLat, MinLng: -180, MaxLng: box.MaxLng},
	}
}
