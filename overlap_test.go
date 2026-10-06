package bloom

import (
	"encoding/binary"
	"math"
	"math/rand"
	"testing"
)

// overlapTestKeys generates n distinct keys from a fixed-seed RNG so the
// tests are deterministic.
func overlapTestKeys(seed int64, n int) [][]byte {
	rng := rand.New(rand.NewSource(seed))
	keys := make([][]byte, n)
	for i := range keys {
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, rng.Uint64())
		keys[i] = key
	}
	return keys
}

func addKeys(f *BloomFilter, keys [][]byte) {
	for _, key := range keys {
		f.Add(key)
	}
}

func TestEstimateOverlapIncompatible(t *testing.T) {
	f := New(1000, 4)
	g := New(2000, 4)
	if _, err := EstimateOverlap(f, g); err == nil {
		t.Error("expected error for mismatched m")
	}
	h := New(1000, 5)
	if _, err := EstimateOverlap(f, h); err == nil {
		t.Error("expected error for mismatched k")
	}
}

func TestEstimateOverlapBothEmpty(t *testing.T) {
	f := New(1000, 4)
	g := New(1000, 4)
	est, err := EstimateOverlap(f, g)
	if err != nil {
		t.Fatal(err)
	}
	if est.Status != OverlapBothEmpty {
		t.Errorf("expected OverlapBothEmpty, got %v", est.Status)
	}
	if est.Union != 0 || est.Intersection != 0 || est.Jaccard != 0 {
		t.Errorf("expected zero estimates, got %+v", est)
	}
}

func TestEstimateOverlapIdentical(t *testing.T) {
	keys := overlapTestKeys(42, 500)
	f := New(10000, 4)
	addKeys(f, keys)
	g := f.Copy()
	est, err := EstimateOverlap(f, g)
	if err != nil {
		t.Fatal(err)
	}
	if est.Status != OverlapIdentical {
		t.Errorf("expected OverlapIdentical, got %v", est.Status)
	}
	if est.Jaccard != 1 {
		t.Errorf("expected Jaccard 1, got %v", est.Jaccard)
	}
	if est.Intersection != est.Union {
		t.Errorf("expected intersection == union, got %v != %v",
			est.Intersection, est.Union)
	}
}

func TestEstimateOverlapDisjoint(t *testing.T) {
	keysA := overlapTestKeys(1, 1000)
	keysB := overlapTestKeys(2, 1000)
	f := New(20000, 4)
	g := New(20000, 4)
	addKeys(f, keysA)
	addKeys(g, keysB)

	est, err := EstimateOverlap(f, g)
	if err != nil {
		t.Fatal(err)
	}
	if est.Status != OverlapOK && est.Status != OverlapIntersectionClamped {
		t.Errorf("unexpected status %v", est.Status)
	}
	// True overlap is 0; the estimate should be small relative to the sets.
	if est.Intersection > 100 {
		t.Errorf("disjoint sets: intersection estimate too large: %v",
			est.Intersection)
	}
	if est.Jaccard > 0.05 {
		t.Errorf("disjoint sets: Jaccard estimate too large: %v", est.Jaccard)
	}
	if math.Abs(est.CardinalityA-1000) > 100 ||
		math.Abs(est.CardinalityB-1000) > 100 {
		t.Errorf("cardinality estimates off: %v, %v",
			est.CardinalityA, est.CardinalityB)
	}
}

func TestEstimateOverlapPartial(t *testing.T) {
	shared := overlapTestKeys(10, 500)
	onlyA := overlapTestKeys(11, 500)
	onlyB := overlapTestKeys(12, 500)
	f := New(20000, 4)
	g := New(20000, 4)
	addKeys(f, shared)
	addKeys(f, onlyA)
	addKeys(g, shared)
	addKeys(g, onlyB)

	est, err := EstimateOverlap(f, g)
	if err != nil {
		t.Fatal(err)
	}
	if est.Status != OverlapOK {
		t.Errorf("unexpected status %v", est.Status)
	}
	// True values: |A| = |B| = 1000, |A ∩ B| = 500, |A ∪ B| = 1500,
	// Jaccard = 1/3. Allow generous slack for estimation error.
	if math.Abs(est.Intersection-500) > 150 {
		t.Errorf("intersection estimate off: %v", est.Intersection)
	}
	if math.Abs(est.Union-1500) > 200 {
		t.Errorf("union estimate off: %v", est.Union)
	}
	if math.Abs(est.Jaccard-1.0/3.0) > 0.1 {
		t.Errorf("Jaccard estimate off: %v", est.Jaccard)
	}
}

func TestEstimateOverlapSaturated(t *testing.T) {
	// Fill a small filter far beyond capacity so every bit is set.
	f := New(100, 4)
	g := New(100, 4)
	addKeys(f, overlapTestKeys(20, 5000))
	addKeys(g, overlapTestKeys(21, 5000))
	if f.b.Count() != f.m || g.b.Count() != g.m {
		t.Skip("filters not fully saturated on this seed")
	}
	est, err := EstimateOverlap(f, g)
	if err != nil {
		t.Fatal(err)
	}
	if est.Status != OverlapSaturated {
		t.Errorf("expected OverlapSaturated, got %v", est.Status)
	}
	if est.Union != 0 || est.Intersection != 0 ||
		!math.IsNaN(est.Jaccard) && est.Jaccard != 0 {
		t.Errorf("saturated estimate should not report numbers: %+v", est)
	}
}

func TestEstimateOverlapDoesNotModifyInputs(t *testing.T) {
	f := New(10000, 4)
	g := New(10000, 4)
	addKeys(f, overlapTestKeys(30, 500))
	addKeys(g, overlapTestKeys(31, 500))
	fCopy := f.Copy()
	gCopy := g.Copy()

	if _, err := EstimateOverlap(f, g); err != nil {
		t.Fatal(err)
	}
	if !f.Equal(fCopy) {
		t.Error("first filter was modified")
	}
	if !g.Equal(gCopy) {
		t.Error("second filter was modified")
	}
}
