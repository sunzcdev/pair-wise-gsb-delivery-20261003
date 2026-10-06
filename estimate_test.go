package bloom

import (
	"encoding/binary"
	"math"
	"math/rand"
	"testing"
)

// fillFilter adds n pseudo-random keys drawn from rng to f.
func fillFilter(f *BloomFilter, rng *rand.Rand, n int) {
	key := make([]byte, 8)
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint64(key, rng.Uint64())
		f.Add(key)
	}
}

func TestEstimateOverlapIncompatible(t *testing.T) {
	if _, err := EstimateOverlap(New(1000, 5), New(2000, 5)); err == nil {
		t.Error("expected error for mismatched m")
	}
	if _, err := EstimateOverlap(New(1000, 5), New(1000, 4)); err == nil {
		t.Error("expected error for mismatched k")
	}
}

func TestEstimateOverlapBothEmpty(t *testing.T) {
	est, err := EstimateOverlap(New(1000, 5), New(1000, 5))
	if err != nil {
		t.Fatal(err)
	}
	if est.Status != OverlapBothEmpty {
		t.Errorf("expected OverlapBothEmpty, got %v", est.Status)
	}
	if est.Union != 0 || est.Intersection != 0 || est.Jaccard != 0 {
		t.Errorf("expected zero-valued estimate, got %+v", est)
	}
}

func TestEstimateOverlapIdentical(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	f := New(100000, 5)
	fillFilter(f, rng, 5000)
	g := f.Copy()

	est, err := EstimateOverlap(f, g)
	if err != nil {
		t.Fatal(err)
	}
	if est.Status != OverlapIdentical {
		t.Errorf("expected OverlapIdentical, got %v", est.Status)
	}
	if est.Jaccard != 1 {
		t.Errorf("expected Jaccard 1, got %f", est.Jaccard)
	}
	if math.Abs(est.CardinalityA-5000)/5000 > 0.05 {
		t.Errorf("cardinality estimate %f too far from 5000", est.CardinalityA)
	}
}

func TestEstimateOverlapDisjoint(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	m, k := uint(1000000), uint(5)
	f, g := New(m, k), New(m, k)
	fillFilter(f, rng, 10000)
	fillFilter(g, rng, 10000)

	beforeF, beforeG := f.b.Count(), g.b.Count()
	est, err := EstimateOverlap(f, g)
	if err != nil {
		t.Fatal(err)
	}
	if f.b.Count() != beforeF || g.b.Count() != beforeG {
		t.Error("EstimateOverlap modified a filter")
	}
	if est.Status != OverlapOK && est.Status != OverlapIntersectionClamped {
		t.Errorf("unexpected status %v", est.Status)
	}
	// Two disjoint 10k sets: union ~20k, intersection ~0, Jaccard ~0.
	if math.Abs(est.Union-20000)/20000 > 0.05 {
		t.Errorf("union estimate %f too far from 20000", est.Union)
	}
	if est.Jaccard > 0.02 {
		t.Errorf("expected near-zero Jaccard for disjoint sets, got %f", est.Jaccard)
	}
}

func TestEstimateOverlapPartial(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	m, k := uint(1000000), uint(5)

	// Shared pool of 5000 keys, plus 10000 private keys per filter:
	// true |A| = |B| = 15000, |intersection| = 5000, |union| = 25000,
	// true Jaccard = 5000/25000 = 0.2.
	shared := rand.New(rand.NewSource(99))
	sharedKeys := make([][]byte, 5000)
	for i := range sharedKeys {
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, shared.Uint64())
		sharedKeys[i] = key
	}

	f, g := New(m, k), New(m, k)
	for _, key := range sharedKeys {
		f.Add(key)
		g.Add(key)
	}
	fillFilter(f, rng, 10000)
	fillFilter(g, rng, 10000)

	est, err := EstimateOverlap(f, g)
	if err != nil {
		t.Fatal(err)
	}
	if est.Status != OverlapOK {
		t.Errorf("expected OverlapOK, got %v", est.Status)
	}
	if math.Abs(est.Intersection-5000)/5000 > 0.15 {
		t.Errorf("intersection estimate %f too far from 5000", est.Intersection)
	}
	if math.Abs(est.Jaccard-0.2)/0.2 > 0.15 {
		t.Errorf("Jaccard estimate %f too far from 0.2", est.Jaccard)
	}
}

func TestEstimateOverlapSaturated(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	m, k := uint(1000), uint(5)
	f, g := New(m, k), New(m, k)
	// Push far more items than the filter can hold: every bit gets set.
	fillFilter(f, rng, 100000)
	fillFilter(g, rng, 100000)

	est, err := EstimateOverlap(f, g)
	if err != nil {
		t.Fatal(err)
	}
	// Identical saturated filters short-circuit as identical; use a
	// saturated filter against a fresh one to exercise the saturated path.
	if est.Status != OverlapIdentical {
		t.Errorf("expected OverlapIdentical for two saturated copies, got %v", est.Status)
	}

	est, err = EstimateOverlap(f, New(m, k))
	if err != nil {
		t.Fatal(err)
	}
	if est.Status != OverlapSaturated {
		t.Errorf("expected OverlapSaturated, got %v", est.Status)
	}
	if est.Jaccard != 0 || est.Union != 0 {
		t.Errorf("saturated estimate should carry no plausible numbers, got %+v", est)
	}
}
