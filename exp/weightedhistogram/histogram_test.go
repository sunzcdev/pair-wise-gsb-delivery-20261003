package weightedhistogram_test

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/exp/weightedhistogram"
)

// requireSnapshotEqual fails the test unless got matches want exactly in
// count and cumulative bucket counts. The sum is compared with a relative
// tolerance because a weighted observation computes v*weight in a single
// multiplication while repeated Observe calls accumulate rounding errors.
func requireSnapshotEqual(t *testing.T, got, want weightedhistogram.Snapshot) {
	t.Helper()
	if got.Count != want.Count {
		t.Errorf("count: got %d, want %d", got.Count, want.Count)
	}
	if len(got.Buckets) != len(want.Buckets) {
		t.Fatalf("bucket count: got %d, want %d", len(got.Buckets), len(want.Buckets))
	}
	for i := range got.Buckets {
		if got.Buckets[i].UpperBound != want.Buckets[i].UpperBound {
			t.Errorf("bucket %d upper bound: got %v, want %v", i, got.Buckets[i].UpperBound, want.Buckets[i].UpperBound)
		}
		if got.Buckets[i].CumulativeCount != want.Buckets[i].CumulativeCount {
			t.Errorf("bucket %d (le=%v): got cumulative count %d, want %d",
				i, got.Buckets[i].UpperBound, got.Buckets[i].CumulativeCount, want.Buckets[i].CumulativeCount)
		}
	}
	if math.IsNaN(want.Sum) {
		if !math.IsNaN(got.Sum) {
			t.Errorf("sum: got %v, want NaN", got.Sum)
		}
		return
	}
	if diff := math.Abs(got.Sum - want.Sum); diff > 1e-9*math.Max(1, math.Abs(want.Sum)) {
		t.Errorf("sum: got %v, want %v", got.Sum, want.Sum)
	}
}

// TestObserveWithWeightMatchesRepeatedObserve feeds random values with random
// small weights into a weighted histogram and replays the same observations
// one by one into a reference histogram. Both must end in the same state.
func TestObserveWithWeightMatchesRepeatedObserve(t *testing.T) {
	buckets := []float64{-1, 0, 0.5, 1, 2.5, 5, 10}
	weighted := weightedhistogram.New(weightedhistogram.Opts{Buckets: buckets})
	reference := weightedhistogram.New(weightedhistogram.Opts{Buckets: buckets})

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 2000; i++ {
		v := rng.NormFloat64() * 3
		weight := uint64(rng.Intn(16) + 1)
		if !weighted.ObserveWithWeight(v, weight) {
			t.Fatalf("ObserveWithWeight(%v, %d) unexpectedly rejected", v, weight)
		}
		for w := uint64(0); w < weight; w++ {
			reference.Observe(v)
		}
	}
	requireSnapshotEqual(t, weighted.Collect(), reference.Collect())
}

// TestHugeWeightIsASingleEvent observes a weight far too large to be replayed
// by looping and verifies the resulting state is exact.
func TestHugeWeightIsASingleEvent(t *testing.T) {
	h := weightedhistogram.New(weightedhistogram.Opts{Buckets: []float64{1, 2, 3}})
	const weight = uint64(1) << 60
	if !h.ObserveWithWeight(2.5, weight) {
		t.Fatal("ObserveWithWeight unexpectedly rejected")
	}
	requireSnapshotEqual(t, h.Collect(), weightedhistogram.Snapshot{
		Count: weight,
		Sum:   2.5 * float64(weight),
		Buckets: []weightedhistogram.Bucket{
			{UpperBound: 1, CumulativeCount: 0},
			{UpperBound: 2, CumulativeCount: 0},
			{UpperBound: 3, CumulativeCount: weight},
			{UpperBound: math.Inf(1), CumulativeCount: weight},
		},
	})
}

// TestZeroWeightIsNoOp verifies that a zero weight changes nothing and
// reports success, including on a histogram that is completely full.
func TestZeroWeightIsNoOp(t *testing.T) {
	h := weightedhistogram.New(weightedhistogram.Opts{Buckets: []float64{1}})
	if !h.ObserveWithWeight(0.5, 0) {
		t.Fatal("zero weight must report success")
	}
	requireSnapshotEqual(t, h.Collect(), weightedhistogram.Snapshot{
		Buckets: []weightedhistogram.Bucket{
			{UpperBound: 1},
			{UpperBound: math.Inf(1)},
		},
	})

	full := weightedhistogram.New(weightedhistogram.Opts{Buckets: []float64{1}})
	if !full.ObserveWithWeight(0.5, weightedhistogram.MaxCount) {
		t.Fatal("filling the histogram to MaxCount must succeed")
	}
	if !full.ObserveWithWeight(0.5, 0) {
		t.Fatal("zero weight must report success even on a full histogram")
	}
	if got := full.Collect().Count; got != weightedhistogram.MaxCount {
		t.Fatalf("count: got %d, want %d", got, uint64(weightedhistogram.MaxCount))
	}
}

// TestNegativeObservations verifies that negative values are counted in the
// matching buckets and decrease the sum.
func TestNegativeObservations(t *testing.T) {
	h := weightedhistogram.New(weightedhistogram.Opts{Buckets: []float64{-1, 0, 1}})
	if !h.ObserveWithWeight(-0.5, 3) {
		t.Fatal("ObserveWithWeight unexpectedly rejected")
	}
	h.Observe(-2)
	requireSnapshotEqual(t, h.Collect(), weightedhistogram.Snapshot{
		Count: 4,
		Sum:   -3.5,
		Buckets: []weightedhistogram.Bucket{
			{UpperBound: -1, CumulativeCount: 1},
			{UpperBound: 0, CumulativeCount: 4},
			{UpperBound: 1, CumulativeCount: 4},
			{UpperBound: math.Inf(1), CumulativeCount: 4},
		},
	})
}

// TestMaxCountRejectsOverflow verifies that an observation pushing the total
// count beyond MaxCount is rejected as a whole, leaving count, sum, and all
// buckets untouched.
func TestMaxCountRejectsOverflow(t *testing.T) {
	h := weightedhistogram.New(weightedhistogram.Opts{Buckets: []float64{1}})
	if !h.ObserveWithWeight(0.5, weightedhistogram.MaxCount-1) {
		t.Fatal("filling the histogram to MaxCount-1 must succeed")
	}
	before := h.Collect()
	if h.ObserveWithWeight(0.5, 2) {
		t.Fatal("observation overflowing MaxCount must be rejected")
	}
	requireSnapshotEqual(t, h.Collect(), before)

	if !h.ObserveWithWeight(0.5, 1) {
		t.Fatal("observation filling the histogram exactly to MaxCount must succeed")
	}
	if got := h.Collect().Count; got != weightedhistogram.MaxCount {
		t.Fatalf("count: got %d, want %d", got, uint64(weightedhistogram.MaxCount))
	}
	if h.ObserveWithWeight(0.5, 1) {
		t.Fatal("observation on a full histogram must be rejected")
	}
}

// TestNaNObservation verifies that NaN is counted in the total count and the
// +Inf bucket only, and poisons the sum.
func TestNaNObservation(t *testing.T) {
	h := weightedhistogram.New(weightedhistogram.Opts{Buckets: []float64{1, 2}})
	if !h.ObserveWithWeight(math.NaN(), 2) {
		t.Fatal("ObserveWithWeight unexpectedly rejected")
	}
	requireSnapshotEqual(t, h.Collect(), weightedhistogram.Snapshot{
		Count: 2,
		Sum:   math.NaN(),
		Buckets: []weightedhistogram.Bucket{
			{UpperBound: 1, CumulativeCount: 0},
			{UpperBound: 2, CumulativeCount: 0},
			{UpperBound: math.Inf(1), CumulativeCount: 2},
		},
	})
}

// TestZeroValueUsesDefaultBuckets verifies that an uninitialized Histogram is
// ready to use and falls back to DefaultBuckets.
func TestZeroValueUsesDefaultBuckets(t *testing.T) {
	var h weightedhistogram.Histogram
	h.Observe(0.3)
	snap := h.Collect()
	if snap.Count != 1 {
		t.Fatalf("count: got %d, want 1", snap.Count)
	}
	if want := len(weightedhistogram.DefaultBuckets) + 1; len(snap.Buckets) != want {
		t.Fatalf("bucket count: got %d, want %d", len(snap.Buckets), want)
	}
	for i, upperBound := range weightedhistogram.DefaultBuckets {
		if snap.Buckets[i].UpperBound != upperBound {
			t.Errorf("bucket %d upper bound: got %v, want %v", i, snap.Buckets[i].UpperBound, upperBound)
		}
	}
}

// TestCollectNeverSeesPartialUpdates hammers a histogram with weighted
// observations from multiple goroutines while a collector goroutine verifies
// that every snapshot is internally consistent: cumulative counts are
// non-decreasing and the +Inf bucket always equals the total count. Run with
// -race to also catch unsynchronized access.
func TestCollectNeverSeesPartialUpdates(t *testing.T) {
	h := weightedhistogram.New(weightedhistogram.Opts{Buckets: []float64{1, 2, 3, 4, 5}})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for {
				select {
				case <-stop:
					return
				default:
					h.ObserveWithWeight(rng.Float64()*6, uint64(rng.Intn(8)+1))
				}
			}
		}(int64(g))
	}

	for i := 0; i < 10000; i++ {
		snap := h.Collect()
		prev := uint64(0)
		for _, b := range snap.Buckets {
			if b.CumulativeCount < prev {
				t.Fatalf("cumulative counts decreased: %d after %d", b.CumulativeCount, prev)
			}
			prev = b.CumulativeCount
		}
		if last := snap.Buckets[len(snap.Buckets)-1]; last.CumulativeCount != snap.Count {
			t.Fatalf("inconsistent snapshot: +Inf bucket has %d, count has %d", last.CumulativeCount, snap.Count)
		}
	}
	close(stop)
	wg.Wait()
}

// TestBucketValidation verifies the constructor's bucket handling.
func TestBucketValidation(t *testing.T) {
	for _, buckets := range [][]float64{
		{2, 1},
		{1, 1},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New with buckets %v must panic", buckets)
				}
			}()
			weightedhistogram.New(weightedhistogram.Opts{Buckets: buckets})
		}()
	}

	h := weightedhistogram.New(weightedhistogram.Opts{Buckets: []float64{1, 2, math.Inf(1)}})
	if got := len(h.Collect().Buckets); got != 3 {
		t.Fatalf("trailing +Inf bound must be dropped: got %d buckets, want 3", got)
	}

	// The constructor copies the bucket slice, so later mutations of the
	// caller's slice must not affect the histogram.
	buckets := []float64{1, 2}
	owned := weightedhistogram.New(weightedhistogram.Opts{Buckets: buckets})
	buckets[0] = -100
	if got := owned.Collect().Buckets[0].UpperBound; got != 1 {
		t.Fatalf("bucket slice was not copied: first upper bound is %v, want 1", got)
	}
}

// BenchmarkObserveWithWeight measures the cost of a weighted observation for
// weights differing by a factor of one million. If ObserveWithWeight were
// implemented by looping over Observe internally, the large weight would be
// roughly a million times slower; instead both sub-benchmarks must show
// approximately equal ns/op, proving the weight is applied arithmetically.
func BenchmarkObserveWithWeight(b *testing.B) {
	for _, weight := range []uint64{1, 1_000_000} {
		b.Run(fmt.Sprintf("weight=%d", weight), func(b *testing.B) {
			h := weightedhistogram.New(weightedhistogram.Opts{})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h.ObserveWithWeight(0.5, weight)
			}
		})
	}
}
