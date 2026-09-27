// Package weightedhistogram provides an experimental Histogram metric that
// supports integer-weighted observations.
//
// Upstream pipelines often pre-aggregate identical measurements into a
// (value, count) pair. Replaying such a pair by calling Observe in a loop is
// wasteful, so this package lets callers record the whole pair as a single
// weighted observation via ObserveWithWeight.
//
// This package is experimental and may change or be removed in the future.
package weightedhistogram

import (
	"math"
	"sort"
	"sync"
)

// MaxCount is the maximum total number of observations a Histogram can hold.
// A weighted observation that would push the total count beyond MaxCount is
// rejected as a whole: it changes nothing and ObserveWithWeight returns
// false. Counts therefore never wrap around and never saturate partially.
const MaxCount = math.MaxUint64

// DefaultBuckets are the bucket upper bounds used by a zero-value Histogram
// and by New when Opts.Buckets is nil. They match prometheus.DefBuckets.
var DefaultBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// Opts configures a Histogram created via New.
type Opts struct {
	// Buckets defines the upper bounds of the histogram buckets in strictly
	// increasing order. The +Inf bucket is implicit and must not be included;
	// a trailing +Inf bound is silently dropped. The slice is copied, so the
	// caller retains ownership and may reuse it after New returns. If nil,
	// DefaultBuckets are used. New panics if the bounds are not strictly
	// increasing.
	Buckets []float64
}

// Histogram counts observations into cumulative buckets. Observing a value v
// with weight w is statistically equivalent to observing v w times, but costs
// O(number of buckets) instead of O(w).
//
// Bucket counts are stored cumulatively: an observation increments every
// bucket whose upper bound is greater than or equal to the observed value.
// Each weighted observation is applied under a single lock, so all affected
// buckets, the total count, and the sum are updated as one atomic statistical
// event. A concurrent Collect always sees a consistent snapshot and never an
// intermediate state such as an already-incremented count with buckets that
// have not caught up yet.
//
// The zero value is ready to use and behaves like a Histogram created with
// New and default buckets.
//
// A Histogram must not be copied after first use.
type Histogram struct {
	mu sync.Mutex
	// upperBounds holds the sorted finite bucket upper bounds.
	upperBounds []float64
	// cumulative holds cumulative bucket counts with len(upperBounds)+1
	// elements. cumulative[i] counts all observations with a value less than
	// or equal to upperBounds[i]; the last element is the +Inf bucket and
	// therefore always equals the total observation count.
	cumulative []uint64
	// sum is the sum of all observed values, each multiplied by its weight.
	sum float64
}

// New returns a Histogram configured by opts. See Opts for the exact
// semantics and validation of the bucket configuration.
func New(opts Opts) *Histogram {
	h := &Histogram{}
	h.initLocked(validateAndCopyBuckets(opts.Buckets))
	return h
}

// Observe records a single observation of v. It never fails to apply because
// a single observation can only overflow the count if the histogram is
// already full, in which case the observation is silently dropped. Use
// ObserveWithWeight if the rejection needs to be detected.
func (h *Histogram) Observe(v float64) {
	h.ObserveWithWeight(v, 1)
}

// ObserveWithWeight records weight observations of the value v as a single
// atomic statistical event: the total count, the sum, and every bucket with
// an upper bound greater than or equal to v are all updated together before
// the call returns, and no concurrent Collect can observe a partially applied
// update.
//
// The weight is an integer repetition count, not a scaling factor for the
// value: it multiplies how often v is counted, never v itself.
//
// Special cases:
//   - A weight of zero is a no-op and reports success.
//   - Negative values are observed like any other value: they are counted in
//     the matching buckets and decrease the sum.
//   - A NaN value is counted in the total count and the +Inf bucket only, and
//     turns the sum into NaN.
//   - If the total count would exceed MaxCount, the whole observation is
//     rejected: nothing is updated and false is returned. Otherwise true is
//     returned.
func (h *Histogram) ObserveWithWeight(v float64, weight uint64) bool {
	if weight == 0 {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.initLocked(nil)
	if total := h.cumulative[len(h.cumulative)-1]; weight > MaxCount-total {
		return false
	}
	for i := sort.SearchFloat64s(h.upperBounds, v); i < len(h.cumulative); i++ {
		h.cumulative[i] += weight
	}
	h.sum += v * float64(weight)
	return true
}

// Bucket is one bucket of a Snapshot. Buckets are cumulative: CumulativeCount
// covers all observations with a value less than or equal to UpperBound.
type Bucket struct {
	UpperBound      float64
	CumulativeCount uint64
}

// Snapshot is a consistent, point-in-time view of a Histogram. The last entry
// of Buckets is the implicit +Inf bucket, and its CumulativeCount always
// equals Count.
type Snapshot struct {
	Count   uint64
	Sum     float64
	Buckets []Bucket
}

// Collect returns a consistent snapshot of the histogram. Because each
// weighted observation is applied atomically under the histogram's lock, the
// returned snapshot never reflects a partially applied observation: the total
// count, the sum, and all bucket counts always belong to the same logical
// state.
func (h *Histogram) Collect() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.initLocked(nil)
	snap := Snapshot{
		Count:   h.cumulative[len(h.cumulative)-1],
		Sum:     h.sum,
		Buckets: make([]Bucket, len(h.cumulative)),
	}
	for i, count := range h.cumulative {
		upperBound := math.Inf(1)
		if i < len(h.upperBounds) {
			upperBound = h.upperBounds[i]
		}
		snap.Buckets[i] = Bucket{UpperBound: upperBound, CumulativeCount: count}
	}
	return snap
}

// initLocked lazily initializes the buckets of a zero-value Histogram. If
// buckets is non-nil, it is installed unconditionally; otherwise the buckets
// of a zero-value Histogram default to DefaultBuckets. The caller must hold
// h.mu.
func (h *Histogram) initLocked(buckets []float64) {
	if buckets == nil {
		if h.upperBounds != nil {
			return
		}
		buckets = validateAndCopyBuckets(nil)
	}
	h.upperBounds = buckets
	h.cumulative = make([]uint64, len(buckets)+1)
}

// validateAndCopyBuckets validates the given upper bounds and returns a copy
// the Histogram can own. A nil input yields a copy of DefaultBuckets, and a
// trailing +Inf bound is dropped because the +Inf bucket is implicit. It
// panics if the bounds are not strictly increasing.
func validateAndCopyBuckets(buckets []float64) []float64 {
	if buckets == nil {
		buckets = DefaultBuckets
	}
	upperBounds := make([]float64, len(buckets))
	copy(upperBounds, buckets)
	for i, upperBound := range upperBounds {
		if i < len(upperBounds)-1 {
			if upperBound >= upperBounds[i+1] {
				panic("weightedhistogram: bucket upper bounds must be in strictly increasing order")
			}
		} else if math.IsInf(upperBound, 1) {
			// The +Inf bucket is implicit. Remove it here.
			upperBounds = upperBounds[:i]
		}
	}
	return upperBounds
}
