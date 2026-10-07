// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).

package ddsketch

import (
	"math"

	"github.com/DataDog/sketches-go/ddsketch/store"
)

// ThresholdCount holds the weights (counts) of the values in a sketch relative
// to a threshold. Because the sketch only stores value ranges per bin, the
// exact number of values above the threshold cannot always be recovered: when a
// bin straddles the threshold, its weight is not attributed by assuming a
// representative value but kept as part of the uncertain weight.
//
// The three weights always satisfy:
//
//	DefinitelyAbove <= (unknown true count above the threshold) <= PossiblyAbove <= Total.
type ThresholdCount struct {
	// definitelyAbove is the weight that is guaranteed to be above the
	// threshold.
	definitelyAbove float64
	// possiblyAbove is the weight that may be above the threshold: it is the
	// guaranteed weight plus the weight of the bins whose value range straddles
	// the threshold.
	possiblyAbove float64
	// total is the total weight stored in the sketch.
	total float64
}

// DefinitelyAbove returns the weight of the values that are guaranteed to be
// above the threshold.
func (c ThresholdCount) DefinitelyAbove() float64 {
	return c.definitelyAbove
}

// PossiblyAbove returns the weight of the values that may be above the
// threshold. It includes the guaranteed weight as well as the weight of the
// bins whose value range straddles the threshold.
func (c ThresholdCount) PossiblyAbove() float64 {
	return c.possiblyAbove
}

// Total returns the total weight stored in the sketch.
func (c ThresholdCount) Total() float64 {
	return c.total
}

// CountAboveThreshold returns, for each weight, whether it is strictly greater
// than threshold. It returns an error if the sketch is empty.
func (s *DDSketch) CountAboveThreshold(threshold float64) (ThresholdCount, error) {
	return s.thresholdCount(threshold, false)
}

// CountAtOrAboveThreshold returns, for each weight, whether it is greater than
// or equal to threshold. It returns an error if the sketch is empty.
func (s *DDSketch) CountAtOrAboveThreshold(threshold float64) (ThresholdCount, error) {
	return s.thresholdCount(threshold, true)
}

func (s *DDSketch) thresholdCount(threshold float64, orEqual bool) (ThresholdCount, error) {
	if s.IsEmpty() {
		return ThresholdCount{}, errEmptySketch
	}
	var count ThresholdCount

	add := func(bin binBounds, weight float64) {
		count.total += weight
		if bin.definitelyAbove(threshold, orEqual) {
			count.definitelyAbove += weight
		}
		if bin.possiblyAbove(threshold, orEqual) {
			count.possiblyAbove += weight
		}
	}

	// The zero bin covers every value in [-MinIndexableValue, MinIndexableValue].
	minIndexable := s.MinIndexableValue()
	add(binBounds{
		lo:          -minIndexable,
		loInclusive: true,
		hi:          minIndexable,
		hiInclusive: true,
	}, s.zeroCount)

	s.positiveValueStore.ForEach(func(index int, weight float64) bool {
		add(s.positiveBinBounds(index), weight)
		return false
	})
	s.negativeValueStore.ForEach(func(index int, weight float64) bool {
		add(s.negativeBinBounds(index), weight)
		return false
	})

	return count, nil
}

// binBounds describes the half-open/closed value range covered by a bin.
type binBounds struct {
	lo, hi                   float64
	loInclusive, hiInclusive bool
}

// definitelyAbove reports whether every value in the bin is above the
// threshold.
func (b binBounds) definitelyAbove(threshold float64, orEqual bool) bool {
	if orEqual {
		// No value of the bin may be strictly below the threshold.
		return b.lo >= threshold
	}
	// Every value of the bin must be strictly above the threshold. If the lower
	// bound is exclusive, values equal to the threshold are not in the bin, so a
	// lower bound equal to the threshold is sufficient.
	return b.lo > threshold || (!b.loInclusive && b.lo == threshold)
}

// possiblyAbove reports whether some value in the bin may be above the
// threshold.
func (b binBounds) possiblyAbove(threshold float64, orEqual bool) bool {
	if orEqual {
		// The maximum value of the bin must not be strictly below the threshold.
		return b.hi > threshold || (b.hiInclusive && b.hi == threshold)
	}
	// The maximum value of the bin must be strictly above the threshold.
	return b.hi > threshold
}

// positiveBinBounds returns the value range covered by a bin of the positive
// value store. Collapsed edge bins get wider, conservative bounds instead of
// their single-index bounds.
func (s *DDSketch) positiveBinBounds(index int) binBounds {
	minIndexable := s.MinIndexableValue()
	hi := s.LowerBound(index + 1)
	if index == collapsedLowestEdge(s.positiveValueStore) {
		// The bin also holds every lower positive index, down to the smallest
		// indexable positive value.
		return binBounds{lo: minIndexable, loInclusive: false, hi: hi, hiInclusive: false}
	}
	if index == collapsedHighestEdge(s.positiveValueStore) {
		// The bin also holds every higher positive index, up to the largest
		// indexable positive value.
		return binBounds{lo: s.LowerBound(index), loInclusive: true, hi: s.MaxIndexableValue(), hiInclusive: true}
	}
	return binBounds{lo: s.LowerBound(index), loInclusive: true, hi: hi, hiInclusive: false}
}

// negativeBinBounds returns the value range covered by a bin of the negative
// value store, where the store index encodes the magnitude of the values.
// Collapsed edge bins get wider, conservative bounds instead of their
// single-index bounds.
func (s *DDSketch) negativeBinBounds(index int) binBounds {
	minIndexable := s.MinIndexableValue()
	lo := -s.LowerBound(index + 1)
	if index == collapsedLowestEdge(s.negativeValueStore) {
		// The bin also holds negative values with the smallest magnitudes, up to
		// the zero bin.
		magnitudeHi := math.Max(s.LowerBound(index+1), minIndexable)
		return binBounds{lo: -magnitudeHi, loInclusive: false, hi: -minIndexable, hiInclusive: false}
	}
	if index == collapsedHighestEdge(s.negativeValueStore) {
		// The bin also holds negative values with the largest magnitudes, down
		// to the most negative indexable value.
		return binBounds{lo: -s.MaxIndexableValue(), loInclusive: true, hi: lo, hiInclusive: false}
	}
	return binBounds{lo: lo, loInclusive: false, hi: -s.LowerBound(index), hiInclusive: true}
}

// collapsedLowestEdge returns the index of the bin that accumulates the
// collapsed lowest indices, or an index that no bin can hold if the store does
// not collapse its lowest bins.
func collapsedLowestEdge(s store.Store) int {
	if c, ok := s.(interface{ IsCollapsed() bool }); ok && c.IsCollapsed() {
		if minIndex, err := s.MinIndex(); err == nil {
			return minIndex
		}
	}
	return math.MinInt32 - 1
}

// collapsedHighestEdge returns the index of the bin that accumulates the
// collapsed highest indices, or an index that no bin can hold if the store does
// not collapse its highest bins.
func collapsedHighestEdge(s store.Store) int {
	if c, ok := s.(interface{ IsCollapsed() bool }); ok && c.IsCollapsed() {
		if maxIndex, err := s.MaxIndex(); err == nil {
			return maxIndex
		}
	}
	return math.MaxInt32 + 1
}
