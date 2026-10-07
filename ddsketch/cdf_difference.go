// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2026 Datadog, Inc.

package ddsketch

import (
	"errors"
	"math"
	"sort"

	"github.com/DataDog/sketches-go/ddsketch/store"
)

var (
	errNilSketch             = errors.New("sketch cannot be nil")
	errInvalidSketchCount    = errors.New("sketch count must be finite and positive")
	errInconsistentBinCounts = errors.New("sketch bin counts are inconsistent with its total count")
)

// CDFDifferenceReport contains rigorous bounds on the maximum vertical
// difference between two sketch CDFs. It deliberately does not report an exact
// Kolmogorov-Smirnov statistic or a statistical significance conclusion.
type CDFDifferenceReport struct {
	// LowerBound is a difference attained at WitnessThreshold for every
	// assignment of the bucketed values consistent with their value ranges.
	LowerBound float64

	// UpperBound is the largest CDF difference that assignments consistent
	// with both sketches can produce.
	UpperBound float64

	// WitnessThreshold is the value at which LowerBound is attained.
	WitnessThreshold float64

	FirstDistributionEmpty  bool
	SecondDistributionEmpty bool
	FirstHasCollapsedBins   bool
	SecondHasCollapsedBins  bool
}

// Limitations describes information that prevents the report from narrowing
// the bounds. It does not include significance or hypothesis-test language.
func (r *CDFDifferenceReport) Limitations() []string {
	limitations := make([]string, 0, 4)
	if r.FirstDistributionEmpty {
		limitations = append(limitations, "first distribution is empty")
	}
	if r.SecondDistributionEmpty {
		limitations = append(limitations, "second distribution is empty")
	}
	if r.FirstHasCollapsedBins {
		limitations = append(limitations, "first distribution has collapsed bins")
	}
	if r.SecondHasCollapsedBins {
		limitations = append(limitations, "second distribution has collapsed bins")
	}
	return limitations
}

// CDFDifferenceBounds returns lower and upper bounds on the maximum CDF
// difference of two sketches. The sketches may use different index mappings.
// Bin counts are normalized independently, so the input sample counts may
// differ.
func CDFDifferenceBounds(first, second *DDSketch) (*CDFDifferenceReport, error) {
	if first == nil || second == nil {
		return nil, errNilSketch
	}
	return first.cdfDifferenceBounds(second)
}

// CDFDifferenceBounds compares this sketch with other and returns the same
// bounds as the package-level CDFDifferenceBounds function.
func (s *DDSketch) CDFDifferenceBounds(other *DDSketch) (*CDFDifferenceReport, error) {
	if s == nil || other == nil {
		return nil, errNilSketch
	}
	return s.cdfDifferenceBounds(other)
}

func (s *DDSketch) cdfDifferenceBounds(other *DDSketch) (*CDFDifferenceReport, error) {
	first := s
	second := other

	firstBins, firstEmpty, firstCollapsed, err := probabilityBins(first)
	if err != nil {
		return nil, err
	}
	secondBins, secondEmpty, secondCollapsed, err := probabilityBins(second)
	if err != nil {
		return nil, err
	}
	firstDistribution := newCDFDistribution(firstBins)
	secondDistribution := newCDFDistribution(secondBins)

	report := &CDFDifferenceReport{
		LowerBound:              0,
		UpperBound:              0,
		WitnessThreshold:        0,
		FirstDistributionEmpty:  firstEmpty,
		SecondDistributionEmpty: secondEmpty,
		FirstHasCollapsedBins:   firstCollapsed,
		SecondHasCollapsedBins:  secondCollapsed,
	}

	thresholds := make(map[float64]struct{}, len(firstBins)*2+len(secondBins)*2+2)
	for _, bins := range [][]probabilityBin{firstBins, secondBins} {
		for _, bin := range bins {
			thresholds[bin.lower] = struct{}{}
			thresholds[bin.upper] = struct{}{}
		}
	}
	if len(thresholds) == 0 {
		return report, nil
	}

	orderedThresholds := make([]float64, 0, len(thresholds))
	for threshold := range thresholds {
		orderedThresholds = append(orderedThresholds, threshold)
	}
	sort.Float64s(orderedThresholds)

	for _, threshold := range orderedThresholds {
		firstLow, firstHigh := firstDistribution.cdfBounds(threshold)
		secondLow, secondHigh := secondDistribution.cdfBounds(threshold)

		lower := math.Max(0, math.Max(firstLow-secondHigh, secondLow-firstHigh))
		upper := math.Max(firstHigh-secondLow, secondHigh-firstLow)

		if lower > report.LowerBound {
			report.LowerBound = lower
			report.WitnessThreshold = threshold
		}
		if upper > report.UpperBound {
			report.UpperBound = upper
		}
	}

	report.LowerBound = clampProbability(report.LowerBound)
	report.UpperBound = clampProbability(report.UpperBound)
	return report, nil
}

type probabilityBin struct {
	lower          float64
	upper          float64
	lowerInclusive bool
	upperInclusive bool
	probability    float64
}

type cdfDistribution struct {
	byLower []probabilityBin
	byUpper []probabilityBin
}

func newCDFDistribution(bins []probabilityBin) cdfDistribution {
	byUpper := append([]probabilityBin(nil), bins...)
	sort.Slice(byUpper, func(i, j int) bool {
		return byUpper[i].upper < byUpper[j].upper
	})
	return cdfDistribution{byLower: bins, byUpper: byUpper}
}

func (d cdfDistribution) cdfBounds(threshold float64) (float64, float64) {
	fixedEnd := sort.Search(len(d.byUpper), func(i int) bool {
		bin := d.byUpper[i]
		return threshold < bin.upper || (threshold == bin.upper && !bin.upperInclusive)
	})
	startedEnd := sort.Search(len(d.byLower), func(i int) bool {
		bin := d.byLower[i]
		return threshold < bin.lower || (threshold == bin.lower && !bin.lowerInclusive)
	})

	fixed := 0.0
	for i := 0; i < fixedEnd; i++ {
		fixed += d.byUpper[i].probability
	}
	started := 0.0
	for i := 0; i < startedEnd; i++ {
		started += d.byLower[i].probability
	}

	return clampProbability(fixed), clampProbability(started)
}

func probabilityBins(s *DDSketch) ([]probabilityBin, bool, bool, error) {
	count := s.GetCount()
	if !isFinite(count) || count < 0 {
		return nil, false, false, errInvalidSketchCount
	}
	negativeCollapse := collapseDirection(s.GetNegativeValueStore())
	positiveCollapse := collapseDirection(s.GetPositiveValueStore())
	negativeCollapsed := negativeCollapse != noCollapsedBins
	positiveCollapsed := positiveCollapse != noCollapsedBins
	collapsed := negativeCollapsed || positiveCollapsed
	if count == 0 {
		return nil, true, collapsed, nil
	}

	bins := make([]probabilityBin, 0)
	total := kahanTotal{}
	negativeStore := s.GetNegativeValueStore()
	positiveStore := s.GetPositiveValueStore()
	negativeMinIndex, _ := negativeStore.MinIndex()
	negativeMaxIndex, _ := negativeStore.MaxIndex()
	positiveMinIndex, _ := positiveStore.MinIndex()
	positiveMaxIndex, _ := positiveStore.MaxIndex()
	appendStore := func(iterate func(func(index int, mass float64) bool), positive bool, direction int) {
		iterate(func(index int, binCount float64) bool {
			if !isFinite(binCount) || binCount < 0 {
				return false
			}
			total.add(binCount)
			bin := probabilityBin{probability: binCount / count}
			switch {
			case positive && direction == highestCollapsedBins && index == positiveMaxIndex:
				bin.lower = s.LowerBound(index)
				bin.upper = s.MaxIndexableValue()
				bin.lowerInclusive = true
				bin.upperInclusive = true
			case positive && direction == lowestCollapsedBins && index == positiveMinIndex:
				bin.lower = s.MinIndexableValue()
				bin.upper = s.LowerBound(index + 1)
				bin.lowerInclusive = false
				bin.upperInclusive = false
			case positive:
				bin.lower = s.LowerBound(index)
				bin.upper = s.LowerBound(index + 1)
				bin.lowerInclusive = true
				bin.upperInclusive = false
			case !positive && direction == highestCollapsedBins && index == negativeMaxIndex:
				bin.lower = -s.MaxIndexableValue()
				bin.upper = -s.LowerBound(index)
				bin.lowerInclusive = true
				bin.upperInclusive = true
			case !positive && direction == lowestCollapsedBins && index == negativeMinIndex:
				bin.lower = -s.LowerBound(index + 1)
				bin.upper = -s.MinIndexableValue()
				bin.lowerInclusive = false
				bin.upperInclusive = false
			default:
				bin.lower = -s.LowerBound(index + 1)
				bin.upper = -s.LowerBound(index)
				bin.lowerInclusive = false
				bin.upperInclusive = true
			}
			if positive && bin.lower == s.MinIndexableValue() {
				bin.lowerInclusive = false
			}
			if !positive && bin.upper <= -s.MinIndexableValue() {
				bin.upperInclusive = false
			}
			bins = append(bins, bin)
			return false
		})
	}

	appendStore(func(f func(int, float64) bool) {
		negativeStore.ForEach(func(index int, count float64) bool {
			return f(index, count)
		})
	}, false, negativeCollapse)
	if zeroCount := s.GetZeroCount(); zeroCount != 0 {
		if !isFinite(zeroCount) || zeroCount < 0 {
			return nil, false, false, errInvalidSketchCount
		}
		total.add(zeroCount)
		bins = append(bins, probabilityBin{
			lower:          -s.MinIndexableValue(),
			upper:          s.MinIndexableValue(),
			lowerInclusive: false,
			upperInclusive: true,
			probability:    zeroCount / count,
		})
	}
	appendStore(func(f func(int, float64) bool) {
		positiveStore.ForEach(func(index int, count float64) bool {
			return f(index, count)
		})
	}, true, positiveCollapse)

	if !isFinite(total.sum) || math.Abs(total.sum-count) > math.Max(1e-12, 1e-9*count) {
		return nil, false, false, errInconsistentBinCounts
	}
	for _, bin := range bins {
		if !isFinite(bin.lower) || !isFinite(bin.upper) || bin.lower > bin.upper || !isFinite(bin.probability) {
			return nil, false, false, errInconsistentBinCounts
		}
	}

	sort.Slice(bins, func(i, j int) bool {
		return bins[i].lower < bins[j].lower
	})
	return bins, false, collapsed, nil
}

const (
	noCollapsedBins = iota
	lowestCollapsedBins
	highestCollapsedBins
)

func collapseDirection(s interface{}) int {
	switch value := s.(type) {
	case *store.CollapsingLowestDenseStore:
		if value.IsCollapsed() {
			return lowestCollapsedBins
		}
	case *store.CollapsingHighestDenseStore:
		if value.IsCollapsed() {
			return highestCollapsedBins
		}
	}
	return noCollapsedBins
}

type kahanTotal struct {
	sum          float64
	compensation float64
}

func (t *kahanTotal) add(value float64) {
	adjusted := value - t.compensation
	total := t.sum + adjusted
	t.compensation = total - t.sum - adjusted
	t.sum = total
}

func clampProbability(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
