package geohash

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// referenceAdjacencies is a brute-force O(n^2) implementation working
// directly on floating-point bounding boxes, used to cross-check the
// indexed integer-grid implementation. Geohash box bounds are exact binary
// fractions, so float comparisons here are exact.
func referenceAdjacencies(hashes []string) []Adjacency {
	boxes := make([]Box, len(hashes))
	for i, hash := range hashes {
		boxes[i] = BoundingBox(hash)
	}

	var result []Adjacency
	for i := 0; i < len(hashes); i++ {
		for j := 0; j < len(hashes); j++ {
			if i == j {
				continue
			}
			if direction, ok := referenceDirection(boxes[i], boxes[j]); ok {
				result = append(result, newAdjacency(hashes[i], hashes[j], direction))
			}
		}
	}

	// Deduplicate unordered pairs and sort.
	seen := make(map[[2]string]bool)
	var deduped []Adjacency
	for _, adj := range result {
		key := [2]string{adj.A, adj.B}
		if !seen[key] {
			seen[key] = true
			deduped = append(deduped, adj)
		}
	}
	sort.Slice(deduped, func(i, j int) bool {
		if deduped[i].A != deduped[j].A {
			return deduped[i].A < deduped[j].A
		}
		return deduped[i].B < deduped[j].B
	})
	return deduped
}

// referenceDirection reports whether box b shares an edge with box a, and
// if so the direction of b from a. Longitude shifts of +/-360 degrees
// account for the seam.
func referenceDirection(a, b Box) (Direction, bool) {
	for _, shift := range []float64{0, 360, -360} {
		bs := Box{
			MinLat: b.MinLat,
			MaxLat: b.MaxLat,
			MinLng: b.MinLng + shift,
			MaxLng: b.MaxLng + shift,
		}
		latOverlap := a.MinLat < bs.MaxLat && bs.MinLat < a.MaxLat
		lngOverlap := a.MinLng < bs.MaxLng && bs.MinLng < a.MaxLng
		switch {
		case a.MaxLng == bs.MinLng && latOverlap:
			return East, true
		case a.MinLng == bs.MaxLng && latOverlap:
			return West, true
		case a.MaxLat == bs.MinLat && lngOverlap:
			return North, true
		case a.MinLat == bs.MaxLat && lngOverlap:
			return South, true
		}
	}
	return 0, false
}

// checkAgainstReference verifies that Adjacencies matches the brute-force
// reference and contains no duplicate pairs.
func checkAgainstReference(t *testing.T, hashes []string) []Adjacency {
	t.Helper()

	got, err := Adjacencies(hashes)
	if err != nil {
		t.Fatalf("Adjacencies returned error: %v", err)
	}

	expected := referenceAdjacencies(hashes)
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("mismatch against reference\ngot:      %v\nexpected: %v", got, expected)
	}

	seen := make(map[[2]string]bool)
	for _, adj := range got {
		key := [2]string{adj.A, adj.B}
		if seen[key] {
			t.Errorf("duplicate pair %v", key)
		}
		seen[key] = true
		if adj.A >= adj.B {
			t.Errorf("pair not normalized: %v", adj)
		}
	}
	return got
}

// TestAdjacenciesCoarseBesideFine verifies that a coarse shard is matched
// against every fine shard along its edge, without refining the coarse
// shard itself.
func TestAdjacenciesCoarseBesideFine(t *testing.T) {
	coarse := "ezs4"
	east := Neighbor(coarse, East)

	hashes := []string{coarse}
	alphabet := "0123456789bcdefghjkmnpqrstuvwxyz"
	for _, a := range alphabet {
		for _, b := range alphabet {
			hashes = append(hashes, east+string(a)+string(b))
		}
	}

	adjacencies := checkAgainstReference(t, hashes)

	// The coarse cell's eastern edge spans 32 fine cells.
	edgeCount := 0
	for _, adj := range adjacencies {
		if adj.A == coarse || adj.B == coarse {
			edgeCount++
			direction := adj.Direction
			if adj.B == coarse {
				direction = oppositeDirection(direction)
			}
			if direction != East {
				t.Errorf("fine shard %v not east of coarse shard", adj)
			}
		}
	}
	if edgeCount != 32 {
		t.Errorf("expected 32 fine shards along coarse edge, got %d", edgeCount)
	}
}

// TestAdjacenciesCornerOnly verifies that cells meeting only at a corner
// point are not reported as neighbors.
func TestAdjacenciesCornerOnly(t *testing.T) {
	center := EncodeWithPrecision(10, 10, 6)
	east := Neighbor(center, East)
	north := Neighbor(center, North)
	northeast := Neighbor(center, NorthEast)

	hashes := []string{center, east, north, northeast}
	adjacencies := checkAgainstReference(t, hashes)

	// Four edge-sharing pairs: center-east, center-north, east-northeast,
	// north-northeast. The diagonal pairs (center, northeast) and
	// (east, north) touch only at a corner and must be absent.
	if len(adjacencies) != 4 {
		t.Fatalf("expected 4 adjacencies, got %d: %v", len(adjacencies), adjacencies)
	}
	for _, adj := range adjacencies {
		pair := [2]string{adj.A, adj.B}
		if pair == [2]string{center, northeast} || pair == [2]string{northeast, center} {
			t.Errorf("diagonal pair reported: %v", adj)
		}
		if pair == [2]string{east, north} || pair == [2]string{north, east} {
			t.Errorf("diagonal pair reported: %v", adj)
		}
	}
}

// TestAdjacenciesDateLine verifies that shards on opposite sides of the
// +/-180 degree seam are matched, including at mixed precisions.
func TestAdjacenciesDateLine(t *testing.T) {
	west := EncodeWithPrecision(0, -179.9, 3)
	east := EncodeWithPrecision(0, 179.9, 3)

	adjacencies := checkAgainstReference(t, []string{west, east})
	if len(adjacencies) != 1 {
		t.Fatalf("expected 1 adjacency across seam, got %v", adjacencies)
	}
	adj := adjacencies[0]
	// Longitude increases eastward and wraps at the seam, so the cell
	// starting at -180 is the eastern neighbor of the cell ending at +180.
	directionFromEast := adj.Direction
	if adj.B == east {
		directionFromEast = oppositeDirection(directionFromEast)
	}
	if directionFromEast != East {
		t.Errorf("expected western shard east of eastern shard, got %v", adj)
	}

	// Mixed precision across the seam: a coarse cell ending at +180
	// against the fine cells starting at -180.
	coarseEast := EncodeWithPrecision(0, 179.9, 2)
	var hashes []string
	alphabet := "0123456789bcdefghjkmnpqrstuvwxyz"
	westCoarse := EncodeWithPrecision(0, -179.9, 2)
	for _, a := range alphabet {
		for _, b := range alphabet {
			hashes = append(hashes, westCoarse+string(a)+string(b))
		}
	}
	hashes = append(hashes, coarseEast)

	adjacencies = checkAgainstReference(t, hashes)
	seamCount := 0
	for _, adj := range adjacencies {
		if adj.A == coarseEast || adj.B == coarseEast {
			seamCount++
		}
	}
	if seamCount != 32 {
		t.Errorf("expected 32 fine shards across the seam, got %d", seamCount)
	}
}

// TestAdjacenciesPolarRow verifies a row of cells along the northernmost
// latitude band: they form an east-west ring across the seam and have no
// neighbors to the north.
func TestAdjacenciesPolarRow(t *testing.T) {
	seen := make(map[string]bool)
	var hashes []string
	for lng := -180.0; lng < 180.0; lng += 1.0 {
		hash := EncodeWithPrecision(89.9, lng, 2)
		if !seen[hash] {
			seen[hash] = true
			hashes = append(hashes, hash)
		}
	}
	if len(hashes) != 32 {
		t.Fatalf("expected 32 cells in polar row, got %d", len(hashes))
	}

	adjacencies := checkAgainstReference(t, hashes)

	// The row is a ring: 32 east-west adjacencies, no north-south ones.
	if len(adjacencies) != 32 {
		t.Fatalf("expected 32 adjacencies in polar ring, got %d: %v", len(adjacencies), adjacencies)
	}
	degree := make(map[string]int)
	for _, adj := range adjacencies {
		if adj.Direction != East && adj.Direction != West {
			t.Errorf("unexpected non-latitudinal adjacency: %v", adj)
		}
		degree[adj.A]++
		degree[adj.B]++
	}
	for _, hash := range hashes {
		if degree[hash] != 2 {
			t.Errorf("cell %s has %d neighbors, expected 2", hash, degree[hash])
		}
	}
}

// TestAdjacenciesOverlap verifies that overlapping inputs are rejected with
// an error naming a concrete overlapping pair.
func TestAdjacenciesOverlap(t *testing.T) {
	cases := [][]string{
		{"ezs4", "ezs42qq"},
		{"ezs42qq", "ezs4"},
		{"ezs4", "ezs4"},
		{"u4pru", "ezs4", "u4pruyd"},
	}
	for _, hashes := range cases {
		_, err := Adjacencies(hashes)
		if err == nil {
			t.Errorf("expected overlap error for %v", hashes)
			continue
		}
		if !strings.Contains(err.Error(), "overlapping") {
			t.Errorf("error does not describe overlap: %v", err)
		}
	}

	// The error must name a genuinely overlapping pair.
	_, err := Adjacencies([]string{"ezs4", "u4pruydqqvj", "u4pru"})
	if err == nil {
		t.Fatal("expected overlap error")
	}
	if !strings.Contains(err.Error(), `"u4pru"`) || !strings.Contains(err.Error(), `"u4pruydqqvj"`) {
		t.Errorf("error does not name the overlapping pair: %v", err)
	}
}

// TestAdjacenciesInvalid verifies invalid hashes are rejected.
func TestAdjacenciesInvalid(t *testing.T) {
	if _, err := Adjacencies([]string{"ezs4", "not!valid"}); err == nil {
		t.Error("expected error for invalid hash")
	}
	if _, err := Adjacencies([]string{"thishashiswaytoolong"}); err == nil {
		t.Error("expected error for overlong hash")
	}
}

// TestAdjacenciesStable verifies the output is deterministic regardless of
// input order.
func TestAdjacenciesStable(t *testing.T) {
	hashes := []string{"ezs4", "ezs5", "ezs7", "ezsh9", "xbp", "8bp"}
	base, err := Adjacencies(hashes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 20; trial++ {
		shuffled := make([]string, len(hashes))
		copy(shuffled, hashes)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		got, err := Adjacencies(shuffled)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("unstable output for permutation %v\ngot:  %v\nbase: %v", shuffled, got, base)
		}
	}
}

// TestAdjacenciesRandomMixed builds random non-overlapping mixed-precision
// coverings and cross-checks against the reference implementation.
func TestAdjacenciesRandomMixed(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := "0123456789bcdefghjkmnpqrstuvwxyz"

	for trial := 0; trial < 50; trial++ {
		// Start from a coarse grid and randomly subdivide some cells.
		var hashes []string
		for i := 0; i < 8; i++ {
			hash := string(alphabet[rng.Intn(32)]) + string(alphabet[rng.Intn(32)])
			for rng.Intn(2) == 0 && len(hash) < 6 {
				hash += string(alphabet[rng.Intn(32)])
			}
			hashes = append(hashes, hash)
		}

		// Skip sets that happen to overlap.
		if _, err := Adjacencies(hashes); err != nil {
			continue
		}

		t.Run(fmt.Sprintf("trial%d", trial), func(t *testing.T) {
			checkAgainstReference(t, hashes)
		})
	}
}
