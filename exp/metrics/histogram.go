// Package metrics provides experimental metric types that may eventually
// graduate into the stable prometheus package.
//
// This package is experimental and may contain breaking changes or be removed
// in the future.
package metrics

import (
	"math"
	"slices"
	"sync"
)

// WeightedHistogram is a classic histogram that, in addition to single
// observations, accepts observations carrying an integer weight. A weighted
// observation ObserveWithWeight(v, w) is statistically equivalent to calling
// Observe(v) w times, but is applied as a single event in O(number of
// affected buckets) without any per-unit loop. This is useful when an
// upstream system has already aggregated identical measurements into
// (value, count) pairs.
//
// Behavior contract:
//   - The zero value is ready to use. It tracks the total count and sum but
//     has no explicit buckets.
//   - A weight of zero is a no-op: it does not count as an event and leaves
//     all counters unchanged.
//   - Negative observed values are valid observations. They are counted in
//     every bucket whose upper bound is greater than or equal to the value
//     and decrease the sum accordingly.
//   - All counters are uint64 and saturate: if an observation would push the
//     total count or a bucket count past math.MaxUint64, that counter is
//     clamped to math.MaxUint64 instead of wrapping around.
//   - Each weighted observation is applied atomically with respect to
//     Collect: a snapshot never observes a state where the total count has
//     been updated but the bucket counts have not caught up (or vice versa).
//
// A WeightedHistogram is safe for concurrent use.
type WeightedHistogram struct {
	// mu guards all fields below so that a weighted observation is visible
	// to Collect as one atomic event.
	mu sync.RWMutex
	// upperBounds holds the explicit bucket upper bounds in ascending
	// order. counts[i] is the cumulative number of observations less than
	// or equal to upperBounds[i].
	upperBounds []float64
	counts      []uint64
	// count is the total number of observed events (weights summed). It
	// equals the implicit +Inf bucket count.
	count uint64
	// sum is the sum of all observed values, each multiplied by its weight.
	sum float64
}

// WeightedHistogramOpts configures a WeightedHistogram. Its zero value is
// valid and equivalent to the zero value of WeightedHistogram itself.
type WeightedHistogramOpts struct {
	// Buckets holds the upper bounds of the explicit buckets. The slice is
	// copied, sorted, and de-duplicated by NewWeightedHistogram, so the
	// caller retains ownership and may pass the bounds in any order. The
	// +Inf bucket is implicit and must not be listed. NaN bounds are
	// dropped because no value can satisfy v <= NaN.
	Buckets []float64
}

// WeightedBucket is one bucket of a WeightedHistogramSnapshot.
type WeightedBucket struct {
	// UpperBound is the inclusive upper bound of the bucket.
	UpperBound float64
	// Count is the cumulative number of observed events with a value less
	// than or equal to UpperBound.
	Count uint64
}

// WeightedHistogramSnapshot is a consistent, point-in-time view of a
// WeightedHistogram. It is detached from the histogram: later observations
// do not mutate it, and the caller may retain it freely.
type WeightedHistogramSnapshot struct {
	// Count is the total number of observed events, i.e. the sum of all
	// weights. It equals the implicit +Inf bucket count.
	Count uint64
	// Sum is the sum of all observed values, each multiplied by its weight.
	Sum float64
	// Buckets holds the cumulative bucket counts in ascending upper-bound
	// order. It is nil when the histogram has no explicit buckets.
	Buckets []WeightedBucket
}

// NewWeightedHistogram returns a WeightedHistogram configured by opts.
func NewWeightedHistogram(opts WeightedHistogramOpts) *WeightedHistogram {
	bounds := make([]float64, 0, len(opts.Buckets))
	for _, b := range opts.Buckets {
		if !math.IsNaN(b) {
			bounds = append(bounds, b)
		}
	}
	slices.Sort(bounds)
	bounds = slices.Compact(bounds)
	return &WeightedHistogram{
		upperBounds: bounds,
		counts:      make([]uint64, len(bounds)),
	}
}

// Observe records a single observation of v. It is equivalent to
// ObserveWithWeight(v, 1).
func (h *WeightedHistogram) Observe(v float64) {
	h.ObserveWithWeight(v, 1)
}

// ObserveWithWeight records w observations of the value v as a single
// statistical event. See the WeightedHistogram documentation for the
// behavior of zero weights, negative values, and counter saturation.
func (h *WeightedHistogram) ObserveWithWeight(v float64, w uint64) {
	if w == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count = saturatingAdd(h.count, w)
	h.sum += v * float64(w)
	// upperBounds is sorted, so every bucket from the first matching index
	// onwards is affected. The weight is added to each of them directly;
	// there is no per-unit loop. For NaN no bucket matches, mirroring IEEE
	// comparison semantics.
	start, _ := slices.BinarySearch(h.upperBounds, v)
	for i := start; i < len(h.upperBounds); i++ {
		if v <= h.upperBounds[i] {
			h.counts[i] = saturatingAdd(h.counts[i], w)
		}
	}
}

// Collect returns a consistent snapshot of the histogram. It never observes
// a partially applied weighted observation: the total count, the sum, and
// all bucket counts belong to the same point in time.
func (h *WeightedHistogram) Collect() WeightedHistogramSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	snap := WeightedHistogramSnapshot{
		Count: h.count,
		Sum:   h.sum,
	}
	if len(h.upperBounds) > 0 {
		snap.Buckets = make([]WeightedBucket, len(h.upperBounds))
		for i, ub := range h.upperBounds {
			snap.Buckets[i] = WeightedBucket{UpperBound: ub, Count: h.counts[i]}
		}
	}
	return snap
}

// saturatingAdd returns a + b, clamped to math.MaxUint64 on overflow.
func saturatingAdd(a, b uint64) uint64 {
	if sum := a + b; sum >= a {
		return sum
	}
	return math.MaxUint64
}
