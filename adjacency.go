// Adjacency detection between geohash shards of mixed precision.
package geohash

import (
	"fmt"
	"sort"
	"strings"
)

// Adjacency describes two shards that share a portion of an edge. Shards
// that merely touch at a corner point are not considered adjacent.
type Adjacency struct {
	// A and B are the geohash prefixes of the two shards, ordered so that
	// A < B lexicographically.
	A, B string

	// Direction is the cardinal direction of B as seen from A across their
	// shared edge: North, East, South or West.
	Direction Direction
}

// fullGridRange is the size of the integer grid covering the full latitude
// or longitude range. Coordinates are expressed in units of 1/2^32 of the
// full range, so every geohash box maps to exact integer bounds.
const fullGridRange = uint64(1) << 32

// shardCell is a geohash prefix expressed as a rectangle on the integer
// grid. Longitude is interpreted modulo fullGridRange so that cells
// wrapping the +/-180 degree seam can be matched.
type shardCell struct {
	hash   string
	minLat uint64
	maxLat uint64
	minLng uint64
	maxLng uint64
}

// newShardCell computes the integer grid rectangle for a geohash prefix.
func newShardCell(hash string) shardCell {
	bits := uint(5 * len(hash))
	inthash := base32encoding.Decode(hash)
	latInt, lngInt := deinterleave(inthash << (64 - bits))
	latBits := bits / 2
	lngBits := bits - latBits
	cell := shardCell{hash: hash}
	cell.minLat = uint64(latInt)
	cell.maxLat = cell.minLat + (fullGridRange >> latBits)
	cell.minLng = uint64(lngInt)
	cell.maxLng = cell.minLng + (fullGridRange >> lngBits)
	return cell
}

// Adjacencies returns all pairs of shards that share a portion of an edge,
// together with the direction of the shared edge. The input is a set of
// geohash prefixes of arbitrary mixed precisions that are expected to be
// mutually non-overlapping, as produced by a covering of a region.
//
// Shards of different precisions are matched directly: a coarse shard may
// border many fine shards along one edge. Pairs touching only at a corner
// point are excluded, while edges shared across the +/-180 degree longitude
// seam are included.
//
// The result is sorted by (A, B) and contains each unordered pair exactly
// once. If any two input prefixes overlap, an error naming one such pair is
// returned.
func Adjacencies(hashes []string) ([]Adjacency, error) {
	cells := make([]shardCell, len(hashes))
	for i, hash := range hashes {
		if err := Validate(hash); err != nil {
			return nil, fmt.Errorf("geohash: invalid shard %q: %v", hash, err)
		}
		cells[i] = newShardCell(hash)
	}

	if err := checkOverlaps(hashes); err != nil {
		return nil, err
	}

	// Index cells by their western and southern edges so candidate
	// neighbors can be looked up directly instead of comparing all pairs
	// or refining everything to a common precision.
	byMinLng := make(map[uint64][]int, len(cells))
	byMinLat := make(map[uint64][]int, len(cells))
	for i := range cells {
		byMinLng[cells[i].minLng] = append(byMinLng[cells[i].minLng], i)
		byMinLat[cells[i].minLat] = append(byMinLat[cells[i].minLat], i)
	}

	var adjacencies []Adjacency
	for i := range cells {
		cell := &cells[i]

		// Cells whose western edge coincides with this cell's eastern
		// edge. The modulo handles the longitude seam: a cell ending at
		// +180 degrees wraps to the cells starting at -180 degrees.
		for _, j := range byMinLng[cell.maxLng%fullGridRange] {
			other := &cells[j]
			if overlap1D(cell.minLat, cell.maxLat, other.minLat, other.maxLat) {
				adjacencies = append(adjacencies, newAdjacency(cell.hash, other.hash, East))
			}
		}

		// Cells whose southern edge coincides with this cell's northern
		// edge. Latitude does not wrap at the poles.
		for _, j := range byMinLat[cell.maxLat] {
			other := &cells[j]
			if overlap1D(cell.minLng, cell.maxLng, other.minLng, other.maxLng) {
				adjacencies = append(adjacencies, newAdjacency(cell.hash, other.hash, North))
			}
		}
	}

	sort.Slice(adjacencies, func(i, j int) bool {
		if adjacencies[i].A != adjacencies[j].A {
			return adjacencies[i].A < adjacencies[j].A
		}
		return adjacencies[i].B < adjacencies[j].B
	})
	return adjacencies, nil
}

// checkOverlaps returns an error naming a pair of overlapping prefixes, or
// nil if the set is mutually non-overlapping. Geohash cells are nested by
// string prefix, so two cells overlap exactly when one prefix is a prefix
// of the other; such pairs are adjacent in sorted order.
func checkOverlaps(hashes []string) error {
	sorted := make([]string, len(hashes))
	copy(sorted, hashes)
	sort.Strings(sorted)
	for i := 0; i+1 < len(sorted); i++ {
		if strings.HasPrefix(sorted[i+1], sorted[i]) {
			return fmt.Errorf("geohash: overlapping shards %q and %q", sorted[i], sorted[i+1])
		}
	}
	return nil
}

// newAdjacency builds an Adjacency for a pair, normalizing the order so the
// lexicographically smaller hash is A and flipping the direction to match.
func newAdjacency(a, b string, direction Direction) Adjacency {
	if a < b {
		return Adjacency{A: a, B: b, Direction: direction}
	}
	return Adjacency{A: b, B: a, Direction: oppositeDirection(direction)}
}

// oppositeDirection returns the direction 180 degrees from d.
func oppositeDirection(d Direction) Direction {
	return (d + 4) % 8
}

// overlap1D reports whether the intervals [amin, amax) and [bmin, bmax)
// overlap with positive length. Touching at a single point does not count.
func overlap1D(amin, amax, bmin, bmax uint64) bool {
	return amin < bmax && bmin < amax
}
