package bloom

import (
	"fmt"
	"math"
)

// OverlapStatus describes how trustworthy an OverlapEstimate is.
type OverlapStatus int

const (
	// OverlapOK means all estimates were computed without hitting any
	// degenerate condition.
	OverlapOK OverlapStatus = iota
	// OverlapBothEmpty means both filters are empty: cardinalities are 0
	// and the Jaccard similarity is undefined (reported as 0).
	OverlapBothEmpty
	// OverlapIdentical means the two filters are bit-for-bit identical,
	// so the intersection equals the union and Jaccard is exactly 1.
	OverlapIdentical
	// OverlapIntersectionClamped means the raw intersection estimate came
	// out negative because of estimation noise and was clamped to 0.
	// The true overlap is likely small relative to the set sizes.
	OverlapIntersectionClamped
	// OverlapSaturated means at least one of the filters (or their union)
	// has every bit set, so the cardinality formula diverges and no
	// meaningful estimate can be produced. Numeric fields are zero.
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
		return "filter saturated, estimates unstable"
	default:
		return "unknown status"
	}
}

// OverlapEstimate holds rough cardinality and overlap statistics for two
// compatible Bloom filters.
type OverlapEstimate struct {
	// CardinalityA and CardinalityB are the estimated number of items
	// added to each filter.
	CardinalityA float64
	CardinalityB float64
	// Union is the estimated number of distinct items across both filters.
	Union float64
	// Intersection is the estimated number of items present in both
	// filters, derived as CardinalityA + CardinalityB - Union.
	Intersection float64
	// Jaccard is Intersection / Union, in [0, 1].
	Jaccard float64
	// Status flags degenerate or unstable conditions; always check it
	// before trusting the numeric fields.
	Status OverlapStatus
}

// EstimateOverlap estimates the cardinality of each filter, of their union,
// their intersection and the Jaccard similarity, without modifying either
// filter. The two filters must have been created with the same m and k,
// otherwise an error is returned.
//
// Method and error assumptions:
//
//   - Cardinalities use the standard fill-ratio estimator
//     n = -(m/k) * ln(1 - x), where x is the fraction of set bits. It
//     assumes the hash locations are independent and uniform, which is an
//     approximation of what this implementation provides.
//   - The union cardinality is estimated from the fill ratio of the
//     bitwise OR of the two filters (computed on a copy). The intersection
//     is derived arithmetically as nA + nB - nUnion; the bitwise AND
//     popcount is NOT a valid intersection estimator and is never used.
//   - Estimation noise can make the raw intersection slightly negative
//     when the true overlap is small; it is then clamped to 0 and flagged
//     via OverlapIntersectionClamped.
//   - When a filter (or the union) is fully saturated the estimator
//     diverges (ln(0)); this is reported as OverlapSaturated instead of
//     returning a plausible-looking but meaningless number.
//
// These are single-point estimates: no confidence interval is computed or
// guaranteed for an individual call. Accuracy degrades as filters fill up,
// so treat the results as a rough trend indicator only.
func EstimateOverlap(f, g *BloomFilter) (OverlapEstimate, error) {
	if f.m != g.m {
		return OverlapEstimate{}, fmt.Errorf("m's don't match: %d != %d", f.m, g.m)
	}
	if f.k != g.k {
		return OverlapEstimate{}, fmt.Errorf("k's don't match: %d != %d", f.k, g.k)
	}

	countA := f.b.Count()
	countB := g.b.Count()

	if countA == 0 && countB == 0 {
		return OverlapEstimate{Status: OverlapBothEmpty}, nil
	}

	if f.b.Equal(g.b) {
		n := estimateCardinality(countA, f.m, f.k)
		return OverlapEstimate{
			CardinalityA: n,
			CardinalityB: n,
			Union:        n,
			Intersection: n,
			Jaccard:      1,
			Status:       OverlapIdentical,
		}, nil
	}

	// Union on a copy: neither filter is modified.
	unionBits := f.b.Union(g.b)
	countUnion := unionBits.Count()

	if countA >= f.m || countB >= f.m || countUnion >= f.m {
		return OverlapEstimate{Status: OverlapSaturated}, nil
	}

	est := OverlapEstimate{
		CardinalityA: estimateCardinality(countA, f.m, f.k),
		CardinalityB: estimateCardinality(countB, f.m, f.k),
		Union:        estimateCardinality(countUnion, f.m, f.k),
		Status:       OverlapOK,
	}

	est.Intersection = est.CardinalityA + est.CardinalityB - est.Union
	if est.Intersection < 0 {
		est.Intersection = 0
		est.Status = OverlapIntersectionClamped
	}
	if est.Union > 0 {
		est.Jaccard = est.Intersection / est.Union
	}
	return est, nil
}

// estimateCardinality approximates the number of items in a filter from the
// number of set bits: n = -(m/k) * ln(1 - count/m).
// https://en.wikipedia.org/wiki/Bloom_filter#Approximating_the_number_of_items_in_a_Bloom_filter
func estimateCardinality(count uint, m uint, k uint) float64 {
	return -float64(m) / float64(k) * math.Log(1-float64(count)/float64(m))
}
