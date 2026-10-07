// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2021 Datadog, Inc.

package ddsketch

import (
	"math"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DataDog/sketches-go/ddsketch/mapping"
	"github.com/DataDog/sketches-go/ddsketch/store"
)

// weightedSample is an exact observation inserted with an explicit weight.
type weightedSample struct {
	value  float64
	weight float64
}

// Samples span negative, zero and positive values, cross the -1/0/1
// boundaries and include a range wide enough to force collapsing stores to
// fold their extreme bins.
var handCraftedSamples = []weightedSample{
	{-1e60, 1},
	{-100, 2},
	{-10, 1},
	{-1.5, 3},
	{-0.25, 4},
	{-1e-60, 1},
	{0, 5},
	{1e-60, 1},
	{0.25, 2},
	{1.5, 3},
	{10, 1},
	{100, 2},
	{1e60, 1},
}

func populateHandCrafted(t *testing.T, s *DDSketch) float64 {
	totalWeight := 0.0
	for _, sample := range handCraftedSamples {
		require.NoError(t, s.AddWithCount(sample.value, sample.weight))
		totalWeight += sample.weight
	}
	return totalWeight
}

func thresholdGrids() []float64 {
	thresholds := []float64{math.Inf(-1), math.Inf(1)}
	// Fine grid across the negative/zero/positive region, including thresholds
	// exactly at, just below and just above the sample values.
	for v := -200.0; v <= 200.0; v += 0.25 {
		thresholds = append(thresholds, v)
	}
	for _, sample := range handCraftedSamples {
		thresholds = append(thresholds, sample.value)
	}
	thresholds = append(thresholds, -1e61, -1e59, 1e-59, 1e59, 1e61)
	sort.Float64s(thresholds)
	return thresholds
}

func exactWeightAbove(t *testing.T, threshold float64, inclusive bool) float64 {
	total := 0.0
	for _, sample := range handCraftedSamples {
		if inclusive {
			if sample.value >= threshold {
				total += sample.weight
			}
		} else {
			if sample.value > threshold {
				total += sample.weight
			}
		}
	}
	return total
}

func TestCountAboveThresholdBoundsExactCount(t *testing.T) {
	indexMapping, err := mapping.NewLogarithmicMapping(0.1)
	require.NoError(t, err)

	sketches := map[string]*DDSketch{
		"dense": LogUnboundedDenseDDSketchMust(0.1),
		"buffered": NewDDSketch(mappingMust(0.01),
			store.NewBufferedPaginatedStore(), store.NewBufferedPaginatedStore()),
		"sparse": NewDDSketch(indexMapping,
			store.NewSparseStore(), store.NewSparseStore()),
		"collapsing_lowest":  collapsingLowestMust(0.1, 5),
		"collapsing_highest": collapsingHighestMust(0.1, 5),
	}

	totalWeight := 0.0
	for _, s := range sketches {
		totalWeight = populateHandCrafted(t, s)
	}

	tol := 1e-9*totalWeight + 1e-12
	for name, s := range sketches {
		for _, threshold := range thresholdGrids() {
			strictExact := exactWeightAbove(t, threshold, false)
			inclusiveExact := exactWeightAbove(t, threshold, true)

			strict, err := s.CountAbove(threshold)
			require.NoError(t, err, name)
			inclusive, err := s.CountAtOrAbove(threshold)
			require.NoError(t, err, name)

			// The true count must always lie within the returned bounds.
			assert.InDelta(t, totalWeight, strict.TotalWeight, tol, name)
			assert.LessOrEqual(t, strict.DefiniteWeight, strictExact+tol, "%s strict %g", name, threshold)
			assert.GreaterOrEqual(t, strict.PossibleWeight, strictExact-tol, "%s strict %g", name, threshold)
			assert.LessOrEqual(t, inclusive.DefiniteWeight, inclusiveExact+tol, "%s inclusive %g", name, threshold)
			assert.GreaterOrEqual(t, inclusive.PossibleWeight, inclusiveExact-tol, "%s inclusive %g", name, threshold)

			assert.LessOrEqual(t, strict.DefiniteWeight, strict.PossibleWeight+tol, name)
			assert.LessOrEqual(t, strict.PossibleWeight, strict.TotalWeight+tol, name)
			assert.LessOrEqual(t, inclusive.DefiniteWeight, inclusive.PossibleWeight+tol, name)
			assert.LessOrEqual(t, inclusive.PossibleWeight, inclusive.TotalWeight+tol, name)

			// >= is never stricter than >.
			assert.GreaterOrEqual(t, inclusive.DefiniteWeight+tol, strict.DefiniteWeight-tol, name)
			assert.GreaterOrEqual(t, inclusive.PossibleWeight, strict.PossibleWeight-tol, name)
		}
	}
}

func TestCountAboveThresholdMonotonic(t *testing.T) {
	sketches := map[string]*DDSketch{
		"dense":              LogUnboundedDenseDDSketchMust(0.1),
		"collapsing_lowest":  collapsingLowestMust(0.1, 5),
		"collapsing_highest": collapsingHighestMust(0.1, 5),
	}
	totalWeight := 0.0
	for _, s := range sketches {
		totalWeight = populateHandCrafted(t, s)
	}
	tol := 1e-9*totalWeight + 1e-12

	for name, s := range sketches {
		thresholds := thresholdGrids()
		prevStrict, err := s.CountAbove(thresholds[0])
		require.NoError(t, err)
		prevInclusive, err := s.CountAtOrAbove(thresholds[0])
		require.NoError(t, err)
		for _, threshold := range thresholds[1:] {
			strict, err := s.CountAbove(threshold)
			require.NoError(t, err)
			inclusive, err := s.CountAtOrAbove(threshold)
			require.NoError(t, err)
			// As the threshold increases, neither bound may increase.
			assert.LessOrEqual(t, strict.DefiniteWeight, prevStrict.DefiniteWeight+tol, name)
			assert.LessOrEqual(t, strict.PossibleWeight, prevStrict.PossibleWeight+tol, name)
			assert.LessOrEqual(t, inclusive.DefiniteWeight, prevInclusive.DefiniteWeight+tol, name)
			assert.LessOrEqual(t, inclusive.PossibleWeight, prevInclusive.PossibleWeight+tol, name)
			prevStrict = strict
			prevInclusive = inclusive
		}
	}
}

func TestCountAboveEmptySketch(t *testing.T) {
	s := LogUnboundedDenseDDSketchMust(0.1)
	strict, err := s.CountAbove(0)
	require.NoError(t, err)
	assert.True(t, strict.NoSamples)
	assert.Zero(t, strict.DefiniteWeight)
	assert.Zero(t, strict.PossibleWeight)
	assert.Zero(t, strict.TotalWeight)

	inclusive, err := s.CountAtOrAbove(0)
	require.NoError(t, err)
	assert.True(t, inclusive.NoSamples)

	// NaN threshold on an empty sketch still reports the no-sample state.
	nan, err := s.CountAbove(math.NaN())
	require.ErrorIs(t, err, ErrNaNThreshold)
	assert.True(t, nan.NoSamples)
}

func TestCountAboveNaNThreshold(t *testing.T) {
	s := LogUnboundedDenseDDSketchMust(0.1)
	require.NoError(t, s.Add(1))
	_, err := s.CountAbove(math.NaN())
	assert.ErrorIs(t, err, ErrNaNThreshold)
	_, err = s.CountAtOrAbove(math.NaN())
	assert.ErrorIs(t, err, ErrNaNThreshold)
}

func TestCountAboveExactSummaryStatistics(t *testing.T) {
	s, err := NewDefaultDDSketchWithExactSummaryStatistics(0.1)
	require.NoError(t, err)
	totalWeight := 0.0
	for _, sample := range handCraftedSamples {
		require.NoError(t, s.AddWithCount(sample.value, sample.weight))
		totalWeight += sample.weight
	}
	tol := 1e-9*totalWeight + 1e-12

	for _, threshold := range thresholdGrids() {
		exact := exactWeightAbove(t, threshold, false)
		w, err := s.CountAbove(threshold)
		require.NoError(t, err)
		assert.False(t, w.NoSamples)
		assert.InDelta(t, totalWeight, w.TotalWeight, tol)
		assert.LessOrEqual(t, w.DefiniteWeight, exact+tol)
		assert.GreaterOrEqual(t, w.PossibleWeight, exact-tol)
	}
}

func TestCountAboveExtremeThresholds(t *testing.T) {
	s := LogUnboundedDenseDDSketchMust(0.1)
	totalWeight := populateHandCrafted(t, s)
	tol := 1e-9*totalWeight + 1e-12

	for _, query := range []func(float64) (ThresholdWeights, error){s.CountAbove, s.CountAtOrAbove} {
		none, err := query(math.Inf(1))
		require.NoError(t, err)
		assert.Zero(t, none.DefiniteWeight)
		assert.Zero(t, none.PossibleWeight)

		all, err := query(math.Inf(-1))
		require.NoError(t, err)
		assert.InDelta(t, totalWeight, all.DefiniteWeight, tol)
		assert.InDelta(t, totalWeight, all.PossibleWeight, tol)
	}
}

func TestCountAboveZeroCrossingOrder(t *testing.T) {
	s := LogUnboundedDenseDDSketchMust(0.01)
	require.NoError(t, s.AddWithCount(-2, 10))
	require.NoError(t, s.AddWithCount(-0.5, 20))
	require.NoError(t, s.AddWithCount(0, 30))
	require.NoError(t, s.AddWithCount(0.5, 40))
	require.NoError(t, s.AddWithCount(2, 50))

	// The zero bin spans [-miv, miv], so threshold 0 straddles it for both
	// inequalities: its weight stays possible rather than definite. Only the
	// positive bins are definite, with or without equality.
	strict, err := s.CountAbove(0)
	require.NoError(t, err)
	assert.InDelta(t, 90, strict.DefiniteWeight, 1e-9)
	assert.InDelta(t, 120, strict.PossibleWeight, 1e-9)

	inclusive, err := s.CountAtOrAbove(0)
	require.NoError(t, err)
	// Equality does not resolve the straddling bin: some zero-bin values are
	// strictly negative.
	assert.InDelta(t, 90, inclusive.DefiniteWeight, 1e-9)
	assert.InDelta(t, 120, inclusive.PossibleWeight, 1e-9)

	// Threshold 1 lies between the 0.5 and 2 bins: both bounds equal the exact
	// weight, no bin straddles.
	atOne, err := s.CountAtOrAbove(1)
	require.NoError(t, err)
	assert.Equal(t, 50.0, atOne.DefiniteWeight)
	assert.Equal(t, 50.0, atOne.PossibleWeight)

	// Negative threshold -1 keeps the ordering: the -2 bin is excluded while
	// the -0.5, zero and positive bins are all definite.
	neg, err := s.CountAtOrAbove(-1)
	require.NoError(t, err)
	assert.InDelta(t, 140, neg.PossibleWeight, 1e-9)
	assert.InDelta(t, 140, neg.DefiniteWeight, 1e-9)
}

func TestCountAboveStrictVsInclusiveAtBinBoundary(t *testing.T) {
	s := LogUnboundedDenseDDSketchMust(0.01)
	for _, v := range []float64{0.5, 1, 1.5, 2, 3} {
		require.NoError(t, s.Add(v))
	}

	// At a bin lower boundary, values equal to the boundary satisfy >= but not
	// >, so the inclusive definite weight must be one weight higher.
	m := s.IndexMapping
	boundary := m.LowerBound(m.Index(1))
	strict, err := s.CountAbove(boundary)
	require.NoError(t, err)
	inclusive, err := s.CountAtOrAbove(boundary)
	require.NoError(t, err)
	assert.InDelta(t, inclusive.DefiniteWeight, strict.DefiniteWeight+1, 1e-9)
	assert.InDelta(t, inclusive.PossibleWeight, strict.PossibleWeight, 1e-9)
}

func TestCountAboveAfterReweight(t *testing.T) {
	s := LogUnboundedDenseDDSketchMust(0.1)
	totalWeight := populateHandCrafted(t, s)
	require.NoError(t, s.Reweight(3))
	totalWeight *= 3
	tol := 1e-9*totalWeight + 1e-12

	for _, threshold := range thresholdGrids() {
		strictExact := exactWeightAbove(t, threshold, false) * 3
		inclusiveExact := exactWeightAbove(t, threshold, true) * 3
		strict, err := s.CountAbove(threshold)
		require.NoError(t, err)
		inclusive, err := s.CountAtOrAbove(threshold)
		require.NoError(t, err)
		assert.InDelta(t, totalWeight, strict.TotalWeight, tol)
		assert.LessOrEqual(t, strict.DefiniteWeight, strictExact+tol)
		assert.GreaterOrEqual(t, strict.PossibleWeight, strictExact-tol)
		assert.LessOrEqual(t, inclusive.DefiniteWeight, inclusiveExact+tol)
		assert.GreaterOrEqual(t, inclusive.PossibleWeight, inclusiveExact-tol)
	}
}

func LogUnboundedDenseDDSketchMust(relativeAccuracy float64) *DDSketch {
	s, err := LogUnboundedDenseDDSketch(relativeAccuracy)
	if err != nil {
		panic(err)
	}
	return s
}

func mappingMust(relativeAccuracy float64) mapping.IndexMapping {
	m, err := mapping.NewDefaultMapping(relativeAccuracy)
	if err != nil {
		panic(err)
	}
	return m
}

func collapsingLowestMust(relativeAccuracy float64, maxNumBins int) *DDSketch {
	s, err := LogCollapsingLowestDenseDDSketch(relativeAccuracy, maxNumBins)
	if err != nil {
		panic(err)
	}
	return s
}

func collapsingHighestMust(relativeAccuracy float64, maxNumBins int) *DDSketch {
	s, err := LogCollapsingHighestDenseDDSketch(relativeAccuracy, maxNumBins)
	if err != nil {
		panic(err)
	}
	return s
}
