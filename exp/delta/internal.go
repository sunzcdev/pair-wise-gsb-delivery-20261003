// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package delta

import (
	"sort"
	"strconv"
	"strings"

	dto "github.com/prometheus/client_model/go"
)

// entry is the per-series baseline kept by a Reader.
type entry struct {
	// kind is the kind of the series.
	kind Kind
	// family is the metric family name.
	family string
	// labels are the sorted, Reader-owned label pairs of the series.
	labels []*dto.LabelPair

	// counterValue is the cumulative counter value for counter series.
	counterValue float64

	// histCount is the cumulative sample count for histogram series.
	histCount float64
	// histSum is the cumulative sample sum, or nil when not exposed.
	histSum *float64
	// bounds are the bucket upper bounds, in Gather order.
	bounds []float64
	// bucketVals are the cumulative bucket counts, aligned with bounds.
	bucketVals []float64

	// gone marks a tombstone for a series that disappeared.
	gone bool
}

// buildCounterReport diffs a counter against its baseline.
func buildCounterReport(cur, old *entry) *Report {
	report := &Report{
		Kind:   KindCounter,
		Family: cur.family,
		Labels: cloneLabels(cur.labels),
		Counter: &CounterReport{
			Value: cur.counterValue,
		},
	}

	switch {
	case old == nil:
		report.State = StateNew
	case old.gone:
		report.State = StateReappeared
	default:
		diff := cur.counterValue - old.counterValue
		switch {
		case diff > 0:
			report.State = StateIncrease
			report.Counter.Delta = diff
		case diff == 0:
			report.State = StateUnchanged
		default:
			// The cumulative counter decreased: report the activity since
			// the restart instead of a negative difference.
			report.State = StateCounterReset
			report.Counter.Delta = nonNegative(cur.counterValue)
		}
	}

	return report
}

// buildHistogramReport diffs a classic histogram against its baseline.
func buildHistogramReport(cur, old *entry) *Report {
	report := &Report{
		Kind:   KindHistogram,
		Family: cur.family,
		Labels: cloneLabels(cur.labels),
		Histogram: &HistogramReport{
			Count: cur.histCount,
			Sum:   cloneFloat(cur.histSum),
		},
	}

	switch {
	case old == nil:
		report.State = StateNew
		report.Histogram.Buckets = bucketReports(cur.bounds, cur.bucketVals, nil)
	case old.gone:
		report.State = StateReappeared
		report.Histogram.Buckets = bucketReports(cur.bounds, cur.bucketVals, nil)
	default:
		boundsChanged := !sameBounds(old.bounds, cur.bounds)
		reset := cur.histCount < old.histCount ||
			anyBucketReset(old.bucketVals, cur.bucketVals, boundsChanged)

		switch {
		case boundsChanged:
			// Bucket counts are not comparable across layouts, so their
			// increments are zero. Count and sum remain comparable, and a
			// count decrease across the change is clamped rather than
			// reported as a negative number.
			report.State = StateBucketsChanged
			report.Histogram.CountDelta = safeDiff(cur.histCount, old.histCount)
			report.Histogram.SumDelta = sumDelta(cur.histSum, old.histSum)
			report.Histogram.Buckets = bucketReports(cur.bounds, cur.bucketVals, nil)
		case reset:
			report.State = StateCounterReset
			report.Histogram.CountDelta = nonNegative(cur.histCount)
			report.Histogram.SumDelta = resetSumDelta(cur.histSum)
			// After a restart the new cumulative buckets start at zero,
			// so the current cumulative values are the post-restart
			// distribution; a zero baseline derives it directly.
			report.Histogram.Buckets = bucketReports(
				cur.bounds, cur.bucketVals, make([]float64, len(cur.bucketVals)))
		default:
			report.Histogram.CountDelta = cur.histCount - old.histCount
			report.Histogram.SumDelta = sumDelta(cur.histSum, old.histSum)
			report.Histogram.Buckets = bucketReports(cur.bounds, cur.bucketVals, old.bucketVals)
			unchanged := report.Histogram.CountDelta == 0 &&
				(report.Histogram.SumDelta == nil || *report.Histogram.SumDelta == 0)
			if unchanged {
				report.State = StateUnchanged
			} else {
				report.State = StateIncrease
			}
		}
	}

	return report
}

// buildDisappearedReport emits the terminal transition for a vanished
// series. All increments are zero and the last observed cumulative values
// are reported.
func buildDisappearedReport(old *entry) *Report {
	report := &Report{
		Kind:   old.kind,
		Family: old.family,
		Labels: cloneLabels(old.labels),
		State:  StateDisappeared,
	}

	switch old.kind {
	case KindCounter:
		report.Counter = &CounterReport{Value: old.counterValue}
	case KindHistogram:
		report.Histogram = &HistogramReport{
			Count:   old.histCount,
			Sum:     cloneFloat(old.histSum),
			Buckets: bucketReports(old.bounds, old.bucketVals, nil),
		}
	}

	return report
}

// anyBucketReset reports whether any aligned cumulative bucket decreased.
// Mismatched layouts are handled separately as a bucket layout change.
func anyBucketReset(oldVals, curVals []float64, boundsChanged bool) bool {
	if boundsChanged || len(oldVals) != len(curVals) {
		return false
	}
	for i := range curVals {
		if curVals[i] < oldVals[i] {
			return true
		}
	}
	return false
}

// sameBounds reports whether two bucket upper-bound sequences match.
func sameBounds(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// bucketReports assembles per-bucket reports. Delta is the number of
// observations that fell into the bucket during the interval, i.e. the
// non-cumulative per-bucket count derived from adjacent cumulative bucket
// values. When previous is nil every increment is zero (new, reappeared,
// disappeared, or changed layout); otherwise the aligned cumulative
// values are diffed first. A zero-length previous slice means the current
// cumulative values themselves are the post-restart distribution. Every
// reported Delta is non-negative.
func bucketReports(bounds, cumulative, previous []float64) []BucketReport {
	buckets := make([]BucketReport, 0, len(bounds))
	cumulativeDelta := make([]float64, len(bounds))
	for i, bound := range bounds {
		value := cumulative[i]
		buckets = append(buckets, BucketReport{
			UpperBound: bound,
			Cumulative: value,
		})
		if previous != nil && i < len(previous) {
			cumulativeDelta[i] = nonNegative(value - previous[i])
		}
	}
	// Convert cumulative bucket increments into per-bucket interval counts
	// by subtracting the previous (lower) bucket's cumulative increment.
	var below float64
	for i := range buckets {
		buckets[i].Delta = nonNegative(cumulativeDelta[i] - below)
		below = cumulativeDelta[i]
	}
	return buckets
}

// safeDiff returns the non-negative difference, treating a decrease as a
// restart and reporting the current cumulative value.
func safeDiff(cur, old float64) float64 {
	return nonNegative(cur - old)
}

// sumDelta returns the arithmetic sample-sum difference, or nil when
// either side is missing. Unlike counts, the sample sum is not guaranteed
// to be monotonic (observations may be negative), so the value is not
// clamped and may be negative.
func sumDelta(cur, old *float64) *float64 {
	if cur == nil || old == nil {
		return nil
	}
	value := *cur - *old
	return &value
}

// resetSumDelta reports the current cumulative sum as the increment after
// a restart, or nil when no sum is exposed.
func resetSumDelta(cur *float64) *float64 {
	if cur == nil {
		return nil
	}
	value := nonNegative(*cur)
	return &value
}

// nonNegative clamps a difference to zero so the API never emits a
// negative increment.
func nonNegative(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}

// histogramCount returns the sample count, preferring the float field
// used when the histogram exposes fractional counts.
func histogramCount(h *dto.Histogram) float64 {
	if h.SampleCountFloat != nil {
		return h.GetSampleCountFloat()
	}
	return float64(h.GetSampleCount())
}

// bucketCount returns the cumulative bucket count, preferring the float
// field when present.
func bucketCount(b *dto.Bucket) float64 {
	if b.CumulativeCountFloat != nil {
		return b.GetCumulativeCountFloat()
	}
	return float64(b.GetCumulativeCount())
}

// bucketValues extracts the bucket upper bounds and cumulative counts in
// Gather order.
func bucketValues(h *dto.Histogram) (bounds []float64, cumulative []float64) {
	bounds = make([]float64, 0, len(h.Bucket))
	cumulative = make([]float64, 0, len(h.Bucket))
	for _, bucket := range h.Bucket {
		if bucket == nil || bucket.UpperBound == nil {
			continue
		}
		bounds = append(bounds, bucket.GetUpperBound())
		cumulative = append(cumulative, bucketCount(bucket))
	}
	return bounds, cumulative
}

// isNativeHistogram reports whether the histogram uses the sparse,
// schema-based native encoding rather than explicit classic buckets.
func isNativeHistogram(h *dto.Histogram) bool {
	return h.GetSchema() != 0
}

// cloneLabels returns a sorted copy of the label pairs that is independent
// of the Gather result and owned by the Reader.
func cloneLabels(labels []*dto.LabelPair) []*dto.LabelPair {
	cloned := make([]*dto.LabelPair, 0, len(labels))
	for _, pair := range labels {
		if pair == nil {
			continue
		}
		name := pair.GetName()
		value := pair.GetValue()
		cloned = append(cloned, &dto.LabelPair{Name: &name, Value: &value})
	}
	sort.Slice(cloned, func(i, j int) bool {
		return cloned[i].GetName() < cloned[j].GetName()
	})
	return cloned
}

// cloneFloat returns an independent copy of a *float64, preserving nil.
func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// seriesKey builds a collision-free key for a family and its label set.
// Length prefixes make the encoding unambiguous even for unusual label
// values.
func seriesKey(family string, labels []*dto.LabelPair) string {
	var builder strings.Builder
	writePrefixed := func(value string) {
		builder.WriteString(strconv.Itoa(len(value)))
		builder.WriteByte(':')
		builder.WriteString(value)
	}

	writePrefixed(family)

	indexes := make([]int, len(labels))
	for i := range indexes {
		indexes[i] = i
	}
	sort.Slice(indexes, func(i, j int) bool {
		return labels[indexes[i]].GetName() < labels[indexes[j]].GetName()
	})
	for _, index := range indexes {
		pair := labels[index]
		if pair == nil {
			continue
		}
		writePrefixed(pair.GetName())
		writePrefixed(pair.GetValue())
	}
	return builder.String()
}

// compareLabels compares two sorted label sets lexicographically by name
// and then value, returning -1, 0, or 1.
func compareLabels(a, b []*dto.LabelPair) int {
	length := len(a)
	if len(b) < length {
		length = len(b)
	}
	for i := 0; i < length; i++ {
		switch {
		case a[i].GetName() < b[i].GetName():
			return -1
		case a[i].GetName() > b[i].GetName():
			return 1
		case a[i].GetValue() < b[i].GetValue():
			return -1
		case a[i].GetValue() > b[i].GetValue():
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	default:
		return 0
	}
}
