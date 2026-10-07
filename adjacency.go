package geohash

import (
	"fmt"
	"sort"
)

// SharedEdge describes a pair of geohash cells that share a portion of an
// edge. Cells touching only at a corner are not considered to share an
// edge. Direction records the position of B relative to A and is always
// North or East, so each unordered pair of adjacent cells is reported
// exactly once.
type SharedEdge struct {
	A, B      string
	Direction Direction
}

// OverlapError is returned by SharedEdges when two input geohashes overlap.
type OverlapError struct {
	A, B string
}

// Error implements the error interface.
func (e *OverlapError) Error() string {
	return fmt.Sprintf("geohash: hashes %q and %q overlap", e.A, e.B)
}

// SharedEdges returns the pairs of the given geohashes whose cells share a
// portion of an edge. The input hashes may have mixed precisions and must
// not overlap; if any two hashes overlap an *OverlapError is returned
// identifying the pair. Edges crossing the 180/-180 longitude seam are
// included, while cells on the polar rows have no neighbor across the pole.
// The result is deterministically ordered and contains no duplicate pairs.
func SharedEdges(hashes []string) ([]SharedEdge, error) {
	cells := make([]gridCell, len(hashes))
	for i, hash := range hashes {
		cell, err := newGridCell(hash)
		if err != nil {
			return nil, err
		}
		cells[i] = cell
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].lo < cells[j].lo })

	// Detect overlaps with a sweep over the sorted intervals. Geohash cell
	// intervals either nest or are disjoint, so any cell overlapping the
	// current one is contained in the interval with the maximum hi seen so
	// far.
	maxHi := uint64(0)
	maxIdx := 0
	for i := range cells {
		if i > 0 && cells[i].lo <= maxHi {
			return nil, &OverlapError{A: cells[maxIdx].hash, B: cells[i].hash}
		}
		if i == 0 || cells[i].hi > maxHi {
			maxHi = cells[i].hi
			maxIdx = i
		}
	}

	// Scanning only North and East from each cell reports every shared edge
	// exactly once: for each adjacent pair exactly one cell lies to the
	// south or west of the other.
	var edges []SharedEdge
	for i := range cells {
		edges = append(edges, cellSharedEdges(cells, i, North)...)
		edges = append(edges, cellSharedEdges(cells, i, East)...)
	}
	return edges, nil
}

// cellSharedEdges returns the shared edges between cells[i] and its
// neighbors in the given direction, which must be North or East.
func cellSharedEdges(cells []gridCell, i int, dir Direction) []SharedEdge {
	a := cells[i]
	var latInt, lngInt uint32
	switch dir {
	case North:
		if a.latHi >= 1<<32 {
			return nil // north pole: no neighbor across it
		}
		latInt, lngInt = uint32(a.latHi), uint32(a.lngLo)
	case East:
		// The uint32 conversion wraps the 180/-180 longitude seam.
		latInt, lngInt = uint32(a.latLo), uint32(a.lngHi)
	}

	// Box of a's neighbor in the given direction at a's own precision, as
	// inclusive bounds in integer geohash space. Any cell sharing a's edge
	// overlaps this box, hence contains it or is contained in it.
	bits := a.bits
	nlo := interleave(latInt, lngInt) >> (64 - bits) << (64 - bits)
	nhi := nlo + (uint64(1) << (64 - bits)) - 1

	var edges []SharedEdge
	try := func(j int) {
		if j == i {
			return
		}
		c := cells[j]
		var ok bool
		switch dir {
		case North:
			ok = c.latLo == a.latHi && intervalOverlap(a.lngLo, a.lngHi, c.lngLo, c.lngHi)
		case East:
			ok = uint32(c.lngLo) == uint32(a.lngHi) && intervalOverlap(a.latLo, a.latHi, c.latLo, c.latHi)
		}
		if ok {
			edges = append(edges, SharedEdge{A: a.hash, B: c.hash, Direction: dir})
		}
	}

	// Coarser candidate: the unique cell containing the neighbor box, if
	// any, immediately precedes it in sorted order.
	start := sort.Search(len(cells), func(j int) bool { return cells[j].lo >= nlo })
	if start > 0 && cells[start-1].hi >= nlo {
		try(start - 1)
	}
	// Equal or finer candidates: cells contained in the neighbor box.
	for j := start; j < len(cells) && cells[j].lo <= nhi; j++ {
		try(j)
	}
	return edges
}

// intervalOverlap reports whether the half-open intervals [lo1, hi1) and
// [lo2, hi2) overlap with positive length.
func intervalOverlap(lo1, hi1, lo2, hi2 uint64) bool {
	return lo1 < hi2 && lo2 < hi1
}

// gridCell is a geohash cell on the integer grid, with latitudes and
// longitudes scaled by 2^32. The lo and hi fields are the inclusive bounds
// of the cell in integer geohash space.
type gridCell struct {
	hash         string
	bits         uint
	lo, hi       uint64
	latLo, latHi uint64
	lngLo, lngHi uint64
}

// newGridCell converts a string geohash to its integer grid representation.
func newGridCell(hash string) (gridCell, error) {
	if err := Validate(hash); err != nil {
		return gridCell{}, fmt.Errorf("geohash: invalid hash %q: %s", hash, err)
	}
	inthash, bits := ConvertStringToInt(hash)
	lo := inthash << (64 - bits)
	hi := lo + (uint64(1) << (64 - bits)) - 1
	latInt, lngInt := deinterleave(lo)
	latBits := bits / 2
	lngBits := bits - latBits
	latSize := uint64(1) << (32 - latBits)
	lngSize := uint64(1) << (32 - lngBits)
	return gridCell{
		hash:  hash,
		bits:  bits,
		lo:    lo,
		hi:    hi,
		latLo: uint64(latInt),
		latHi: uint64(latInt) + latSize,
		lngLo: uint64(lngInt),
		lngHi: uint64(lngInt) + lngSize,
	}, nil
}
