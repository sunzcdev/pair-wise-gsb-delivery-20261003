package geohash

import (
	"errors"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

const testAlphabet = "0123456789bcdefghjkmnpqrstuvwxyz"

// childHashes returns the 32 children of hash at one character higher
// precision.
func childHashes(hash string) []string {
	children := make([]string, 0, 32)
	for _, c := range testAlphabet {
		children = append(children, hash+string(c))
	}
	return children
}

func sortSharedEdges(edges []SharedEdge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].A != edges[j].A {
			return edges[i].A < edges[j].A
		}
		if edges[i].B != edges[j].B {
			return edges[i].B < edges[j].B
		}
		return edges[i].Direction < edges[j].Direction
	})
}

// referenceSharedEdges is a brute-force oracle based on float bounding
// boxes. Geohash cell boundaries are exact in float64, so edge comparisons
// are reliable.
func referenceSharedEdges(hashes []string) []SharedEdge {
	var edges []SharedEdge
	for i := 0; i < len(hashes); i++ {
		for j := i + 1; j < len(hashes); j++ {
			a, b := BoundingBox(hashes[i]), BoundingBox(hashes[j])
			latOverlap := math.Min(a.MaxLat, b.MaxLat) > math.Max(a.MinLat, b.MinLat)
			lngOverlap := math.Min(a.MaxLng, b.MaxLng) > math.Max(a.MinLng, b.MinLng)
			if latOverlap {
				if a.MaxLng == b.MinLng || (a.MaxLng == 180 && b.MinLng == -180) {
					edges = append(edges, SharedEdge{A: hashes[i], B: hashes[j], Direction: East})
				} else if b.MaxLng == a.MinLng || (b.MaxLng == 180 && a.MinLng == -180) {
					edges = append(edges, SharedEdge{A: hashes[j], B: hashes[i], Direction: East})
				}
			}
			if lngOverlap {
				if a.MaxLat == b.MinLat {
					edges = append(edges, SharedEdge{A: hashes[i], B: hashes[j], Direction: North})
				} else if b.MaxLat == a.MinLat {
					edges = append(edges, SharedEdge{A: hashes[j], B: hashes[i], Direction: North})
				}
			}
		}
	}
	return edges
}

// checkSharedEdges verifies SharedEdges against the brute-force oracle and
// checks for reverse duplicates.
func checkSharedEdges(t *testing.T, hashes []string) []SharedEdge {
	t.Helper()
	got, err := SharedEdges(hashes)
	if err != nil {
		t.Fatalf("SharedEdges(%v) error: %s", hashes, err)
	}
	want := referenceSharedEdges(hashes)
	sortSharedEdges(got)
	sortSharedEdges(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SharedEdges(%v) = %v, want %v", hashes, got, want)
	}
	seen := make(map[[2]string]bool)
	for _, e := range got {
		if seen[[2]string{e.B, e.A}] {
			t.Fatalf("reverse duplicate pair (%s, %s) in %v", e.A, e.B, got)
		}
		seen[[2]string{e.A, e.B}] = true
	}
	return got
}

func countEdges(edges []SharedEdge, from string, dir Direction) int {
	n := 0
	for _, e := range edges {
		if e.A == from && e.Direction == dir {
			n++
		}
	}
	return n
}

func TestSharedEdgesCoarseWithFineNeighbors(t *testing.T) {
	coarse := "ezs42"
	east := Neighbor(coarse, East)
	hashes := append([]string{coarse}, childHashes(east)...)
	edges := checkSharedEdges(t, hashes)
	// The 6-character children have 3 more latitude bits than the coarse
	// cell, so 8 of them line its eastern edge.
	if n := countEdges(edges, coarse, East); n != 8 {
		t.Fatalf("coarse cell has %d east edges, want 8", n)
	}
}

func TestSharedEdgesCornerTouching(t *testing.T) {
	c := "ezs42"
	n := Neighbor(c, North)
	ne := Neighbor(c, NorthEast)
	e := Neighbor(c, East)
	hashes := []string{c, n, e, ne}
	got, err := SharedEdges(hashes)
	if err != nil {
		t.Fatalf("SharedEdges(%v) error: %s", hashes, err)
	}
	// Four cells meeting at a corner: only true edge shares count, so the
	// diagonal pairs (c, ne) and (n, e) are excluded.
	want := []SharedEdge{
		{A: c, B: n, Direction: North},
		{A: c, B: e, Direction: East},
		{A: n, B: ne, Direction: East},
		{A: e, B: ne, Direction: North},
	}
	sortSharedEdges(got)
	sortSharedEdges(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SharedEdges(%v) = %v, want %v", hashes, got, want)
	}
}

func TestSharedEdgesLongitudeSeam(t *testing.T) {
	e := EncodeWithPrecision(0, 179.999999, 6)
	w := EncodeWithPrecision(0, -179.999999, 6)
	if BoundingBox(e).MaxLng != 180 || BoundingBox(w).MinLng != -180 {
		t.Fatal("test cells do not straddle the longitude seam")
	}
	got, err := SharedEdges([]string{e, w})
	if err != nil {
		t.Fatalf("SharedEdges error: %s", err)
	}
	want := []SharedEdge{{A: e, B: w, Direction: East}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SharedEdges seam = %v, want %v", got, want)
	}
}

func TestSharedEdgesLongitudeSeamMixedPrecision(t *testing.T) {
	e := EncodeWithPrecision(0, 179.999999, 5)
	w := EncodeWithPrecision(0, -179.999999, 5)
	if BoundingBox(e).MaxLng != 180 || BoundingBox(w).MinLng != -180 {
		t.Fatal("test cells do not straddle the longitude seam")
	}
	hashes := append([]string{e}, childHashes(w)...)
	edges := checkSharedEdges(t, hashes)
	if n := countEdges(edges, e, East); n != 8 {
		t.Fatalf("coarse cell has %d east edges across the seam, want 8", n)
	}
}

func TestSharedEdgesPolarRow(t *testing.T) {
	top := EncodeWithPrecision(89.999999, 0, 6)
	if BoundingBox(top).MaxLat != 90 {
		t.Fatal("test cell is not on the polar row")
	}
	east := Neighbor(top, East)
	west := Neighbor(top, West)
	bottom := EncodeWithPrecision(-89.999999, 0, 6)
	hashes := []string{top, east, west, bottom}
	got, err := SharedEdges(hashes)
	if err != nil {
		t.Fatalf("SharedEdges(%v) error: %s", hashes, err)
	}
	// The polar row has east/west neighbors only: nothing across the pole.
	want := []SharedEdge{
		{A: west, B: top, Direction: East},
		{A: top, B: east, Direction: East},
	}
	sortSharedEdges(got)
	sortSharedEdges(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SharedEdges(%v) = %v, want %v", hashes, got, want)
	}
}

func TestSharedEdgesMixedPrecisions(t *testing.T) {
	base := "ezs42"
	nb := Neighbors(base)
	hashes := []string{
		base,
		nb[NorthEast], nb[SouthEast], nb[South],
		nb[SouthWest], nb[West], nb[NorthWest],
	}
	// Subdivide the north and east neighbors one level down.
	hashes = append(hashes, childHashes(nb[North])...)
	hashes = append(hashes, childHashes(nb[East])...)
	checkSharedEdges(t, hashes)
}

func TestSharedEdgesOverlap(t *testing.T) {
	cases := [][]string{
		{"ezs42", "ezs421"},
		{"ezs421", "ezs42"},
		{"ezs42", "ezs42"},
		{"ez", "ezs", "ezs42"},
		{"u4pruydqqvj", "u4pruydqqvj0"},
	}
	for _, hashes := range cases {
		_, err := SharedEdges(hashes)
		var oerr *OverlapError
		if !errors.As(err, &oerr) {
			t.Fatalf("SharedEdges(%v) error = %v, want *OverlapError", hashes, err)
		}
		// The reported pair must actually overlap: one is a prefix of the
		// other.
		a, b := oerr.A, oerr.B
		if len(a) > len(b) {
			a, b = b, a
		}
		if b[:len(a)] != a {
			t.Fatalf("SharedEdges(%v) reported non-overlapping pair (%q, %q)", hashes, oerr.A, oerr.B)
		}
	}
}

func TestSharedEdgesStable(t *testing.T) {
	base := "ezs42"
	nb := Neighbors(base)
	hashes := []string{base}
	hashes = append(hashes, nb...)
	hashes = append(hashes, childHashes(nb[East])...)
	// Remove the unsubdivided east neighbor to avoid overlap.
	hashes = append(hashes[:3], hashes[4:]...)

	want, err := SharedEdges(hashes)
	if err != nil {
		t.Fatalf("SharedEdges error: %s", err)
	}

	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 10; trial++ {
		shuffled := make([]string, len(hashes))
		copy(shuffled, hashes)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		got, err := SharedEdges(shuffled)
		if err != nil {
			t.Fatalf("SharedEdges error: %s", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("SharedEdges output depends on input order:\n%v\nwant\n%v", got, want)
		}
	}
}

func TestSharedEdgesTrivial(t *testing.T) {
	for _, hashes := range [][]string{nil, {"ezs42"}} {
		edges, err := SharedEdges(hashes)
		if err != nil {
			t.Fatalf("SharedEdges(%v) error: %s", hashes, err)
		}
		if len(edges) != 0 {
			t.Fatalf("SharedEdges(%v) = %v, want none", hashes, edges)
		}
	}
}
