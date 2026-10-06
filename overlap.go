package bloom

import (
	"fmt"
	"math"
)

// OverlapStatus describes how trustworthy an OverlapEstimate is.
type OverlapStatus int

const (
	// OverlapOK means all cardinality estimates were computed and the
	// inclusion-exclusion intersection was non-negative.
	OverlapOK OverlapStatus = iota
	// OverlapBothEmpty means both filters are empty. Cardinalities are 0
	// and the Jaccard index is undefined (reported as 0).
	OverlapBothEmpty
	// OverlapIdentical means the two filters hold exactly the same bits,
	// so the intersection equals the union and the Jaccard index is 1.
	OverlapIdentical
	// OverlapIntersectionClamped means the inclusion-exclusion estimate of
	// the intersection came out slightly negative. This is an artifact of
	// estimation error, not a real quantity; it is reported as 0.
	OverlapIntersectionClamped
	// OverlapSaturated means at least one of the filters (or their union)
	// has no unset bits left, so the cardinality estimator is undefined
	// (it would require log(0) or a division by zero). No estimates are
	// reported; all numeric fields are 0. Note that estimates also become
	// increasingly unstable as occupancy approaches this limit.
	OverlapSaturated
)

// String returns a human-readable description of the status.
func (s OverlapStatus) String() string {
	switch s {
	case OverlapOK:
		return "ok"
	case OverlapBothEmpty:
		return "both filters empty"
	case OverlapIdentical:
		return "filters identical"
	case OverlapIntersectionClamped:
		return "negative intersection clamped to zero"
	case OverlapSaturated:
		return "filter saturated, estimation unstable"
	default:
		return "unknown status"
	}
}

// OverlapEstimate holds rough estimates of the overlap between the sets
// stored in two compatible Bloom filters.
type OverlapEstimate struct {
	// Status indicates whether the numeric fields are meaningful.
	Status OverlapStatus
	// CardinalityA is the estimated number of items in the first filter.
	CardinalityA float64
	// CardinalityB is the estimated number of items in the second filter.
	CardinalityB float64
	// Union is the estimated number of items in A ∪ B, derived from the
	// occupancy of the bitwise OR of the two filters.
	Union float64
	// Intersection is the estimated number of items in A ∩ B, computed by
	// inclusion-exclusion: |A| + |B| - |A ∪ B|.
	Intersection float64
	// Jaccard is the estimated Jaccard index Intersection / Union.
	Jaccard float64
}

// estimateCardinality estimates the number of items from a bit count using
// the standard Bloom filter cardinality estimator
// n = -(m/k) * ln(1 - x/m), where x is the number of bits set.
// See https://en.wikipedia.org/wiki/Bloom_filter#Approximating_the_number_of_items_in_a_Bloom_filter
func estimateCardinality(m, k, x uint) float64 {
	return -float64(m) / float64(k) * math.Log(1-float64(x)/float64(m))
}

// EstimateOverlap estimates the cardinality of the sets stored in two Bloom
// filters, together with their intersection and Jaccard index.
//
// The two filters must be compatible: same bit size m and same number of
// hash functions k, otherwise an error is returned.
//
// Method: the cardinality of each filter is estimated from its occupancy
// (fraction of bits set), and the cardinality of the union is estimated from
// the occupancy of the bitwise OR of the two filters. The intersection then
// follows by inclusion-exclusion. Note that the popcount of the bitwise AND
// is NOT a valid estimate of the intersection (hash collisions set shared
// bits even for disjoint sets), so it is deliberately not used.
//
// Error assumptions: the estimator assumes the hash functions behave
// uniformly and independently, so each estimate carries a random error that
// grows sharply as occupancy approaches saturation. The intersection, being
// a difference of estimates, is the least reliable figure and may even come
// out negative (reported via OverlapIntersectionClamped). These are rough
// point estimates only; no confidence interval is computed or implied for a
// single measurement.
//
// The two input filters are not modified.
func EstimateOverlap(f, g *BloomFilter) (OverlapEstimate, error) {
	if f.m != g.m {
		return OverlapEstimate{}, fmt.Errorf("m's don't match: %d != %d", f.m, g.m)
	}
	if f.k != g.k {
		return OverlapEstimate{}, fmt.Errorf("k's don't match: %d != %d", f.k, g.k)
	}

	countA := f.b.Count()
	countB := g.b.Count()

	// Bitwise OR on a copy so the inputs are left untouched.
	unionBits := f.b.Clone()
	unionBits.InPlaceUnion(g.b)
	countUnion := unionBits.Count()

	// Both filters empty: cardinalities are exactly 0, Jaccard undefined.
	if countUnion == 0 {
		return OverlapEstimate{Status: OverlapBothEmpty}, nil
	}

	// Saturated: with no unset bits the estimator needs log(0) and would
	// divide by zero; refuse to return a plausible-looking number.
	if countA == f.m || countB == f.m || countUnion == f.m {
		return OverlapEstimate{Status: OverlapSaturated}, nil
	}

	est := OverlapEstimate{
		Status:       OverlapOK,
		CardinalityA: estimateCardinality(f.m, f.k, countA),
		CardinalityB: estimateCardinality(f.m, f.k, countB),
		Union:        estimateCardinality(f.m, f.k, countUnion),
	}

	if f.b.Equal(g.b) {
		est.Status = OverlapIdentical
		est.Intersection = est.Union
		est.Jaccard = 1
		return est, nil
	}

	est.Intersection = est.CardinalityA + est.CardinalityB - est.Union
	if est.Intersection < 0 {
		// Pure estimation noise: the true intersection cannot be negative.
		est.Intersection = 0
		est.Status = OverlapIntersectionClamped
	}
	est.Jaccard = est.Intersection / est.Union
	return est, nil
}
