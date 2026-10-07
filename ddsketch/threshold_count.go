// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2021 Datadog, Inc.

package ddsketch

import (
	"errors"
	"math"

	"github.com/DataDog/sketches-go/ddsketch/store"
)

var (
	// ErrNaNThreshold is returned when the threshold of a threshold count query
	// is NaN, against which no value is greater than or equal.
	ErrNaNThreshold = errors.New("the threshold cannot be NaN")
)

// ThresholdWeights holds the result of a threshold count query. Values tracked
// by the sketch are grouped into bins, and a query can only locate the
// threshold at bin granularity. A bin whose value range straddles the
// threshold cannot be classified without guessing how its values are
// distributed within it, so its weight is left out of DefiniteWeight and
// included in PossibleWeight instead.
type ThresholdWeights struct {
	// DefiniteWeight is the weight of the values that are guaranteed to satisfy
	// the threshold condition.
	DefiniteWeight float64
	// PossibleWeight is the weight of the values that may satisfy the threshold
	// condition, i.e. a guaranteed upper bound on the true weight.
	PossibleWeight float64
	// TotalWeight is the total weight tracked by the sketch.
	TotalWeight float64
	// NoSamples reports whether the sketch was empty, in which case the three
	// weights are all zero and no bound can be asserted.
	NoSamples bool
}

// CountAbove returns weights about the values v such that v > threshold.
// It returns an error if threshold is NaN.
func (s *DDSketch) CountAbove(threshold float64) (ThresholdWeights, error) {
	return s.countAboveThreshold(threshold, false, s.GetCount())
}

// CountAtOrAbove returns weights about the values v such that v >= threshold.
// It returns an error if threshold is NaN.
func (s *DDSketch) CountAtOrAbove(threshold float64) (ThresholdWeights, error) {
	return s.countAboveThreshold(threshold, true, s.GetCount())
}

func (s *DDSketch) countAboveThreshold(threshold float64, inclusive bool, totalWeight float64) (ThresholdWeights, error) {
	if math.IsNaN(threshold) {
		return ThresholdWeights{NoSamples: s.IsEmpty()}, ErrNaNThreshold
	}
	result := ThresholdWeights{TotalWeight: totalWeight}
	if result.TotalWeight == 0 {
		result.NoSamples = true
		return result, nil
	}

	var definite, possible float64
	accumulate := func(lower, upper float64, weight float64) {
		// The bin is definite when its lowest possible value satisfies the
		// condition; it is possible when its highest possible value satisfies
		// it. Bins straddling the threshold fall in the possible-but-not
		// definite range.
		var isDefinite, isPossible bool
		if inclusive {
			isDefinite = lower >= threshold
			isPossible = upper >= threshold
		} else {
			isDefinite = lower > threshold
			isPossible = upper > threshold
		}
		if isDefinite {
			definite += weight
		}
		if isPossible {
			possible += weight
		}
	}

	s.positiveValueStore.ForEach(func(index int, count float64) bool {
		lower := s.IndexMapping.LowerBound(index)
		upper := s.IndexMapping.LowerBound(index + 1)
		if folded, ok := s.positiveValueStore.(store.FoldedBinStore); ok {
			foldedBelow, foldedAbove := folded.IsBinFolded(index)
			if foldedBelow {
				// The lowest positive indexable values may have been folded in.
				lower = 0
			}
			if foldedAbove {
				upper = math.Inf(1)
			}
		}
		accumulate(lower, upper, count)
		return false
	})

	s.negativeValueStore.ForEach(func(index int, count float64) bool {
		// Negative values are indexed by their magnitude: the bin covers
		// (-LowerBound(index+1), -LowerBound(index)].
		upper := -s.IndexMapping.LowerBound(index)
		lower := -s.IndexMapping.LowerBound(index + 1)
		if folded, ok := s.negativeValueStore.(store.FoldedBinStore); ok {
			foldedBelow, foldedAbove := folded.IsBinFolded(index)
			if foldedBelow {
				// Folded indices below the labeled magnitude index hold
				// magnitudes down to the smallest indexable one, i.e. negative
				// values arbitrarily close to zero.
				upper = 0
			}
			if foldedAbove {
				// Folded indices above the labeled magnitude index hold
				// arbitrarily large magnitudes, i.e. values arbitrarily far
				// below zero.
				lower = math.Inf(-1)
			}
		}
		accumulate(lower, upper, count)
		return false
	})

	if s.zeroCount != 0 {
		// The zero bin is a closed interval spanning the values that are too
		// close to zero to be indexed.
		zeroExtent := s.MinIndexableValue()
		accumulate(-zeroExtent, zeroExtent, s.zeroCount)
	}

	result.DefiniteWeight = definite
	result.PossibleWeight = possible
	return result, nil
}
