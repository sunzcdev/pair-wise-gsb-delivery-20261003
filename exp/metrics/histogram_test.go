package metrics_test

import (
	"math"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/exp/metrics"
)

func testBuckets() []float64 {
	return []float64{-10, -1, 0, 1, 10, 100}
}

func bucketCounts(snap metrics.WeightedHistogramSnapshot) []uint64 {
	counts := make([]uint64, len(snap.Buckets))
	for i, b := range snap.Buckets {
		counts[i] = b.Count
	}
	return counts
}

func assertSnapshot(t *testing.T, snap metrics.WeightedHistogramSnapshot, count uint64, sum float64, counts []uint64) {
	t.Helper()
	if snap.Count != count {
		t.Errorf("got total count %d, want %d", snap.Count, count)
	}
	if snap.Sum != sum {
		t.Errorf("got sum %v, want %v", snap.Sum, sum)
	}
	got := bucketCounts(snap)
	if len(got) != len(counts) {
		t.Fatalf("got %d buckets, want %d", len(got), len(counts))
	}
	for i := range counts {
		if got[i] != counts[i] {
			t.Errorf("bucket %d: got cumulative count %d, want %d", i, got[i], counts[i])
		}
	}
}

func TestWeightedHistogram(t *testing.T) {
	t.Run("single observations", func(t *testing.T) {
		h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: testBuckets()})
		h.Observe(0.5)
		h.Observe(0.5)
		h.Observe(50)
		assertSnapshot(t, h.Collect(), 3, 51, []uint64{0, 0, 0, 2, 2, 3})
	})

	t.Run("weighted observation equals repeated observations", func(t *testing.T) {
		h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: testBuckets()})
		h.ObserveWithWeight(0.5, 2)
		h.ObserveWithWeight(50, 1)
		assertSnapshot(t, h.Collect(), 3, 51, []uint64{0, 0, 0, 2, 2, 3})
	})

	t.Run("zero value is ready to use", func(t *testing.T) {
		var h metrics.WeightedHistogram
		h.Observe(5)
		h.ObserveWithWeight(-3, 4)
		snap := h.Collect()
		assertSnapshot(t, snap, 5, -7, nil)
		if snap.Buckets != nil {
			t.Errorf("got %d buckets from zero-value histogram, want none", len(snap.Buckets))
		}
	})

	t.Run("zero weight is a no-op", func(t *testing.T) {
		h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: testBuckets()})
		h.ObserveWithWeight(5, 0)
		h.ObserveWithWeight(math.NaN(), 0)
		assertSnapshot(t, h.Collect(), 0, 0, []uint64{0, 0, 0, 0, 0, 0})
	})

	t.Run("negative observations", func(t *testing.T) {
		h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: testBuckets()})
		h.ObserveWithWeight(-5, 3)
		h.Observe(-0.5)
		assertSnapshot(t, h.Collect(), 4, -15.5, []uint64{0, 3, 4, 4, 4, 4})
	})

	t.Run("bucket boundary is inclusive", func(t *testing.T) {
		h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: testBuckets()})
		h.ObserveWithWeight(1, 7)
		h.ObserveWithWeight(-10, 2)
		assertSnapshot(t, h.Collect(), 9, -13, []uint64{2, 2, 2, 9, 9, 9})
	})

	t.Run("unsorted and duplicate buckets are normalized", func(t *testing.T) {
		h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: []float64{10, 1, 10, math.NaN(), -1}})
		h.Observe(0)
		snap := h.Collect()
		if len(snap.Buckets) != 3 {
			t.Fatalf("got %d buckets, want 3", len(snap.Buckets))
		}
		for i, want := range []float64{-1, 1, 10} {
			if snap.Buckets[i].UpperBound != want {
				t.Errorf("bucket %d: got upper bound %v, want %v", i, snap.Buckets[i].UpperBound, want)
			}
		}
		assertSnapshot(t, snap, 1, 0, []uint64{0, 1, 1})
	})

	t.Run("total count saturates at math.MaxUint64", func(t *testing.T) {
		h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: testBuckets()})
		h.ObserveWithWeight(5, math.MaxUint64-1)
		h.ObserveWithWeight(5, 10)
		snap := h.Collect()
		if snap.Count != math.MaxUint64 {
			t.Errorf("got total count %d, want saturation at %d", snap.Count, uint64(math.MaxUint64))
		}
		assertSnapshot(t, snap, math.MaxUint64, snap.Sum, []uint64{0, 0, 0, 0, math.MaxUint64, math.MaxUint64})
	})

	t.Run("NaN observation only touches count and sum", func(t *testing.T) {
		h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: testBuckets()})
		h.ObserveWithWeight(math.NaN(), 2)
		snap := h.Collect()
		if snap.Count != 2 {
			t.Errorf("got total count %d, want 2", snap.Count)
		}
		if !math.IsNaN(snap.Sum) {
			t.Errorf("got sum %v, want NaN", snap.Sum)
		}
		for i, b := range snap.Buckets {
			if b.Count != 0 {
				t.Errorf("bucket %d: got cumulative count %d, want 0", i, b.Count)
			}
		}
	})
}

// TestWeightedHistogramRandomizedEquivalence feeds random small weights into
// ObserveWithWeight and mirrors each event with w individual Observe calls on
// a second histogram. Both snapshots must be identical.
func TestWeightedHistogramRandomizedEquivalence(t *testing.T) {
	buckets := []float64{-100, -1, 0, 0.5, 1, 2, 5, 100}
	weighted := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: buckets})
	repeated := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: buckets})

	rng := rand.New(rand.NewPCG(42, 0))
	for range 2000 {
		// Integer values keep the float64 sum exact on both sides.
		v := float64(rng.IntN(401)) - 200 // Values in [-200, 200].
		w := uint64(rng.IntN(8) + 1)      // Weights in [1, 8].
		weighted.ObserveWithWeight(v, w)
		for range w {
			repeated.Observe(v)
		}
	}

	got, want := weighted.Collect(), repeated.Collect()
	if got.Count != want.Count {
		t.Errorf("total count: got %d, want %d", got.Count, want.Count)
	}
	if got.Sum != want.Sum {
		t.Errorf("sum: got %v, want %v", got.Sum, want.Sum)
	}
	gotCounts, wantCounts := bucketCounts(got), bucketCounts(want)
	for i := range wantCounts {
		if gotCounts[i] != wantCounts[i] {
			t.Errorf("bucket %d (le=%v): got %d, want %d", i, want.Buckets[i].UpperBound, gotCounts[i], wantCounts[i])
		}
	}
}

// TestWeightedHistogramHugeWeight observes a weight far too large to apply in
// a hidden per-unit loop within the test timeout, proving that the weight is
// added to each affected bucket directly.
func TestWeightedHistogramHugeWeight(t *testing.T) {
	const w = uint64(1) << 60 // Over 10^18: looping per unit would take years.
	h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: testBuckets()})
	h.ObserveWithWeight(5, w)
	snap := h.Collect()
	assertSnapshot(t, snap, w, 5*float64(w), []uint64{0, 0, 0, 0, w, w})
}

// TestWeightedHistogramConcurrentCollect hammers the histogram with weighted
// observations while collecting snapshots. Every snapshot must be internally
// consistent: cumulative bucket counts are non-decreasing and never exceed
// the total count, so a partially applied observation is never visible.
func TestWeightedHistogramConcurrentCollect(t *testing.T) {
	h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: testBuckets()})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				h.ObserveWithWeight(5, 3)
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		snap := h.Collect()
		prev := uint64(0)
		for i, b := range snap.Buckets {
			if b.Count < prev {
				t.Fatalf("bucket %d: cumulative count %d lower than previous %d", i, b.Count, prev)
			}
			if b.Count > snap.Count {
				t.Fatalf("bucket %d: cumulative count %d exceeds total count %d", i, b.Count, snap.Count)
			}
			prev = b.Count
		}
	}
}

func BenchmarkWeightedHistogram(b *testing.B) {
	buckets := testBuckets()
	b.Run("Observe", func(b *testing.B) {
		h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: buckets})
		for b.Loop() {
			h.Observe(5)
		}
	})
	b.Run("ObserveWithWeight", func(b *testing.B) {
		h := metrics.NewWeightedHistogram(metrics.WeightedHistogramOpts{Buckets: buckets})
		for b.Loop() {
			h.ObserveWithWeight(5, 1<<40)
		}
	})
}
