// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026 Datadog, Inc.

package ddsketch

import (
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/DataDog/sketches-go/ddsketch/mapping"
	"github.com/DataDog/sketches-go/ddsketch/store"
	"github.com/stretchr/testify/require"
)

func TestCDFDifferenceBoundsContainExactSampleDistance(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	for caseIndex := 0; caseIndex < 20; caseIndex++ {
		firstSamples := make([]float64, 20+caseIndex)
		secondSamples := make([]float64, 31+caseIndex)
		for i := range firstSamples {
			firstSamples[i] = 0.05 + rng.ExpFloat64()
		}
		for i := range secondSamples {
			secondSamples[i] = 0.75 + rng.ExpFloat64()
		}

		first := newSketchWithSamples(t, func() (*DDSketch, error) {
			return LogUnboundedDenseDDSketch(0.02)
		}, firstSamples)
		second := newSketchWithSamples(t, func() (*DDSketch, error) {
			return LogUnboundedDenseDDSketch(0.08)
		}, secondSamples)

		report, err := CDFDifferenceBounds(first, second)
		require.NoError(t, err)

		exact := exactSampleKS(firstSamples, secondSamples)
		require.LessOrEqual(t, report.LowerBound-1e-12, exact)
		require.GreaterOrEqual(t, report.UpperBound+1e-12, exact)
		require.LessOrEqual(t, report.LowerBound-1e-12, report.UpperBound)
		require.True(t, isFinite(report.WitnessThreshold))

		witnessFirstLow, witnessFirstHigh := firstDistributionBinsForTest(t, first, report.WitnessThreshold)
		witnessSecondLow, witnessSecondHigh := secondDistributionBinsForTest(t, second, report.WitnessThreshold)
		witnessLower := math.Max(witnessFirstLow-witnessSecondHigh, witnessSecondLow-witnessFirstHigh)
		require.InDelta(t, report.LowerBound, math.Max(0, witnessLower), 1e-12)

		swapped, err := CDFDifferenceBounds(second, first)
		require.NoError(t, err)
		require.InDelta(t, report.LowerBound, swapped.LowerBound, 1e-12)
		require.InDelta(t, report.UpperBound, swapped.UpperBound, 1e-12)
		require.InDelta(t, report.WitnessThreshold, swapped.WitnessThreshold, 1e-12)
		require.Equal(t, report.FirstDistributionEmpty, swapped.SecondDistributionEmpty)
		require.Equal(t, report.SecondDistributionEmpty, swapped.FirstDistributionEmpty)
		require.Equal(t, report.FirstHasCollapsedBins, swapped.SecondHasCollapsedBins)
		require.Equal(t, report.SecondHasCollapsedBins, swapped.FirstHasCollapsedBins)
	}
}

func TestCDFDifferenceBoundsDifferentMappings(t *testing.T) {
	firstSamples := []float64{0.1, 0.4, 1.2, 3.0, 9.0}
	secondSamples := []float64{0.2, 0.5, 1.4, 4.0, 10.0, 20.0}

	firstMapping, err := mapping.NewLogarithmicMapping(0.02)
	require.NoError(t, err)
	secondMapping, err := mapping.NewCubicallyInterpolatedMapping(0.08)
	require.NoError(t, err)

	first := newSketchWithSamples(t, func() (*DDSketch, error) {
		return NewDDSketch(firstMapping, store.NewDenseStore(), store.NewDenseStore()), nil
	}, firstSamples)
	second := newSketchWithSamples(t, func() (*DDSketch, error) {
		return NewDDSketch(secondMapping, store.NewDenseStore(), store.NewDenseStore()), nil
	}, secondSamples)

	report, err := CDFDifferenceBounds(first, second)
	require.NoError(t, err)
	exact := exactSampleKS(firstSamples, secondSamples)
	require.GreaterOrEqual(t, report.LowerBound, 0.0)
	require.GreaterOrEqual(t, report.UpperBound+1e-12, exact)
	require.LessOrEqual(t, report.LowerBound-1e-12, exact)
}

func TestCDFDifferenceBoundsNegativeAndZeroValues(t *testing.T) {
	first := newSketchWithSamples(t, func() (*DDSketch, error) {
		return LogUnboundedDenseDDSketch(0.02)
	}, []float64{-10, -1, 0, 1, 10})
	second := newSketchWithSamples(t, func() (*DDSketch, error) {
		return LogUnboundedDenseDDSketch(0.07)
	}, []float64{-8, -0.5, 0, 2, 8, 12})
	firstSamples := []float64{-10, -1, 0, 1, 10}
	secondSamples := []float64{-8, -0.5, 0, 2, 8, 12}

	report, err := CDFDifferenceBounds(first, second)
	require.NoError(t, err)

	exact := exactSampleKS(firstSamples, secondSamples)
	require.LessOrEqual(t, report.LowerBound-1e-12, exact)
	require.GreaterOrEqual(t, report.UpperBound+1e-12, exact)

	bins, _, _, err := probabilityBins(first)
	require.NoError(t, err)
	distribution := newCDFDistribution(bins)
	require.True(t, distribution.hasProbabilityAtThreshold(t, 0))
}

func TestCDFDifferenceBoundsEmptyDistributions(t *testing.T) {
	first, err := LogUnboundedDenseDDSketch(0.02)
	require.NoError(t, err)
	second, err := LogUnboundedDenseDDSketch(0.02)
	require.NoError(t, err)
	require.NoError(t, second.Add(1))

	report, err := CDFDifferenceBounds(first, second)
	require.NoError(t, err)
	require.InDelta(t, 0, report.LowerBound, 1e-12)
	require.InDelta(t, 1, report.UpperBound, 1e-12)
	require.True(t, report.FirstDistributionEmpty)
	require.False(t, report.SecondDistributionEmpty)
	require.Len(t, report.Limitations(), 1)

	emptyReport, err := CDFDifferenceBounds(first, first)
	require.NoError(t, err)
	require.True(t, emptyReport.FirstDistributionEmpty)
	require.True(t, emptyReport.SecondDistributionEmpty)
	require.Len(t, emptyReport.Limitations(), 2)
}

func TestCDFDifferenceBoundsCollapsedBins(t *testing.T) {
	samples := []float64{-1e50, -1, -1e-50, 1e-50, 1, 1e50}

	for _, constructor := range []func() (*DDSketch, error){
		func() (*DDSketch, error) {
			return LogCollapsingLowestDenseDDSketch(0.1, 4)
		},
		func() (*DDSketch, error) {
			return LogCollapsingHighestDenseDDSketch(0.1, 4)
		},
	} {
		collapsed := newSketchWithSamples(t, constructor, samples)
		otherSamples := []float64{-8, -2, -0.5, 0.5, 2, 8}
		other := newSketchWithSamples(t, func() (*DDSketch, error) {
			return LogUnboundedDenseDDSketch(0.05)
		}, otherSamples)

		require.True(t, collapsed.HasCollapsedBins())
		report, err := CDFDifferenceBounds(collapsed, other)
		require.NoError(t, err)
		require.True(t, report.FirstHasCollapsedBins)
		require.False(t, report.SecondHasCollapsedBins)

		exact := exactSampleKS(samples, otherSamples)
		require.LessOrEqual(t, report.LowerBound-1e-12, exact)
		require.GreaterOrEqual(t, report.UpperBound+1e-12, exact)
		require.Contains(t, report.Limitations()[0], "collapsed")
	}
}

func TestCDFDifferenceBoundsRejectsNilSketch(t *testing.T) {
	sketch, err := LogUnboundedDenseDDSketch(0.02)
	require.NoError(t, err)

	_, err = CDFDifferenceBounds(nil, sketch)
	require.ErrorIs(t, err, errNilSketch)
	_, err = CDFDifferenceBounds(sketch, nil)
	require.ErrorIs(t, err, errNilSketch)

	var nilSketch *DDSketch
	_, err = nilSketch.CDFDifferenceBounds(sketch)
	require.ErrorIs(t, err, errNilSketch)
}

func TestCDFDifferenceBoundsMethodMatchesFunction(t *testing.T) {
	first := newSketchWithSamples(t, func() (*DDSketch, error) {
		return LogUnboundedDenseDDSketch(0.02)
	}, []float64{0.2, 1, 4})
	second := newSketchWithSamples(t, func() (*DDSketch, error) {
		return LogUnboundedDenseDDSketch(0.07)
	}, []float64{0.3, 2, 5, 9})

	functionReport, err := CDFDifferenceBounds(first, second)
	require.NoError(t, err)
	methodReport, err := first.CDFDifferenceBounds(second)
	require.NoError(t, err)
	require.Equal(t, functionReport, methodReport)
}

func newSketchWithSamples(t *testing.T, constructor func() (*DDSketch, error), samples []float64) *DDSketch {
	t.Helper()
	sketch, err := constructor()
	require.NoError(t, err)
	for _, sample := range samples {
		require.NoError(t, sketch.Add(sample))
	}
	return sketch
}

func firstDistributionBinsForTest(t *testing.T, sketch *DDSketch, threshold float64) (float64, float64) {
	t.Helper()
	bins, _, _, err := probabilityBins(sketch)
	require.NoError(t, err)
	return newCDFDistribution(bins).cdfBounds(threshold)
}

func secondDistributionBinsForTest(t *testing.T, sketch *DDSketch, threshold float64) (float64, float64) {
	return firstDistributionBinsForTest(t, sketch, threshold)
}

func (d cdfDistribution) hasProbabilityAtThreshold(t *testing.T, threshold float64) bool {
	t.Helper()
	low, high := d.cdfBounds(threshold)
	return high > low
}

func exactSampleKS(first, second []float64) float64 {
	first = append([]float64(nil), first...)
	second = append([]float64(nil), second...)
	sort.Float64s(first)
	sort.Float64s(second)

	thresholds := append(append([]float64(nil), first...), second...)
	sort.Float64s(thresholds)

	maxDistance := 0.0
	for _, threshold := range thresholds {
		firstCount := sort.Search(len(first), func(i int) bool {
			return first[i] > threshold
		})
		secondCount := sort.Search(len(second), func(i int) bool {
			return second[i] > threshold
		})
		firstCDF := float64(firstCount) / float64(len(first))
		secondCDF := float64(secondCount) / float64(len(second))
		maxDistance = math.Max(maxDistance, math.Abs(firstCDF-secondCDF))
	}
	return maxDistance
}
