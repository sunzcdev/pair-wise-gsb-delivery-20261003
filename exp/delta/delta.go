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

// Package delta derives interval increments of cumulative Prometheus
// metrics from Gather results without mutating the underlying metrics.
//
// A Reader keeps an independent per-series baseline and turns every
// successful Gather into the increments observed since the previous
// successful one. Counter decreases, metrics that disappear and reappear,
// and classic histogram bucket layout changes are reported as distinct
// states instead of being silently clamped or subtracted into negative
// values. If a Gather fails, the baseline is left untouched and the next
// successful Gather is diffed against the last successful one.
//
// This package only processes counters and classic (explicit-bucket)
// histograms. Gauges, summaries, untyped families, and native histograms
// are ignored.
//
// This package is experimental and its API may change or be removed.
package delta

import (
	"sort"
	"sync"

	dto "github.com/prometheus/client_model/go"
)

// State classifies how a single series behaved relative to a reader's
// previous successful Gather.
type State int

const (
	// StateUnknown is the zero value and is never reported.
	StateUnknown State = iota
	// StateNew means the series was observed for the first time by this
	// reader. No interval is defined, so every increment is zero and the
	// current cumulative value is used as the baseline.
	StateNew
	// StateIncrease means the cumulative value grew monotonically and the
	// reported increments are straightforward non-negative differences.
	StateIncrease
	// StateUnchanged means the cumulative value did not move.
	StateUnchanged
	// StateCounterReset means a cumulative value decreased, i.e. the source
	// counter or histogram restarted. The series is rebaselined to the
	// current cumulative value and the reported increments represent the
	// activity accumulated since the restart, so they are never negative.
	StateCounterReset
	// StateDisappeared means the series was present in the previous Gather
	// but absent from the current one. Every increment is zero and the
	// reported cumulative value is the last one that was observed.
	StateDisappeared
	// StateReappeared means the series is present again after having been
	// reported as StateDisappeared. It is intentionally distinct from
	// StateNew so callers can tell a restarted-but-still-known series apart
	// from a brand-new one. Every increment is zero and the current
	// cumulative value becomes the new baseline.
	StateReappeared
	// StateBucketsChanged means the upper bounds of a classic histogram do
	// not match the bounds of the previous Gather. Per-bucket increments
	// cannot be compared across layouts and are therefore zero while the
	// histogram is rebaselined. Sample count and sample sum are still
	// diffed because they are layout-independent.
	StateBucketsChanged
)

// String returns the lowercase name of the State, matching the style used
// by Prometheus telemetry.
func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateIncrease:
		return "increase"
	case StateUnchanged:
		return "unchanged"
	case StateCounterReset:
		return "counter_reset"
	case StateDisappeared:
		return "disappeared"
	case StateReappeared:
		return "reappeared"
	case StateBucketsChanged:
		return "buckets_changed"
	default:
		return "unknown"
	}
}

// Kind identifies the metric kind a Report describes.
type Kind int

const (
	// KindUnknown is the zero value and is never reported.
	KindUnknown Kind = iota
	// KindCounter is a counter family.
	KindCounter
	// KindHistogram is a classic histogram family.
	KindHistogram
)

// String returns the lowercase name of the Kind.
func (k Kind) String() string {
	switch k {
	case KindCounter:
		return "counter"
	case KindHistogram:
		return "histogram"
	default:
		return "unknown"
	}
}

// Gatherer is the subset of prometheus.Gatherer that a Reader consumes.
// *prometheus.Registry satisfies it directly.
type Gatherer interface {
	// Gather returns the current set of metric families.
	Gather() ([]*dto.MetricFamily, error)
}

// Reader converts cumulative Gather results into per-interval increments.
//
// Each Reader owns its baseline and is safe for concurrent use. Multiple
// readers may wrap the same Gatherer and run at different frequencies;
// they never influence each other and never reset or otherwise mutate the
// underlying metrics. Baselines, including those of series that have
// disappeared, are retained for the lifetime of the Reader so that a
// reappearing series can always be distinguished from a genuinely new
// one.
type Reader struct {
	gatherer Gatherer

	mu      sync.Mutex
	entries map[string]*entry
}

// Report describes the interval increment of a single series. Exactly one
// of Counter and Histogram is populated, according to Kind. The Labels
// slice is always a fresh copy that callers may retain and mutate.
type Report struct {
	// Kind is the kind of the reported series.
	Kind Kind
	// Family is the metric family name, without the "_bucket", "_sum", or
	// "_count" suffixes used in the text exposition format.
	Family string
	// Labels are the series labels, sorted by label name.
	Labels []*dto.LabelPair
	// State classifies the interval.
	State State

	// Counter is populated when Kind is KindCounter.
	Counter *CounterReport
	// Histogram is populated when Kind is KindHistogram.
	Histogram *HistogramReport
}

// CounterReport is the interval increment of a counter.
type CounterReport struct {
	// Value is the current cumulative value, as exposed in the Gather.
	Value float64
	// Delta is the increment over the interval. It is zero for
	// StateNew, StateReappeared, and StateDisappeared, and non-negative for
	// every other state.
	Delta float64
}

// HistogramReport is the interval increment of a classic histogram.
type HistogramReport struct {
	// Count is the current cumulative sample count, as exposed in the
	// Gather.
	Count float64
	// CountDelta is the interval increment of the sample count. It is zero
	// for StateNew, StateReappeared, and StateDisappeared and never
	// negative otherwise.
	CountDelta float64
	// Sum is the current cumulative sample sum, as exposed in the Gather.
	// It is nil when the source histogram does not expose a sample sum.
	Sum *float64
	// SumDelta is the interval increment of the sample sum. It is nil when
	// the histogram exposes no sample sum, or when no comparable baseline
	// exists (StateNew, StateReappeared, StateDisappeared). A sample sum is
	// not guaranteed to be monotonic because observations may be negative,
	// so this value may be negative even without a reset and is never
	// clamped.
	SumDelta *float64
	// Buckets hold the non-cumulative number of observations that fell into
	// each bucket during the interval, derived from the difference of
	// adjacent cumulative buckets, in the order and with the upper bounds of
	// the current Gather. All increments are zero for StateNew,
	// StateReappeared, StateDisappeared, and StateBucketsChanged, and
	// non-negative otherwise.
	Buckets []BucketReport
}

// BucketReport is the interval increment of one cumulative histogram
// bucket.
type BucketReport struct {
	// UpperBound is the inclusive upper bound of the bucket.
	UpperBound float64
	// Cumulative is the current cumulative bucket count.
	Cumulative float64
	// Delta is the number of observations that fell into this bucket during
	// the interval (not the cumulative bucket increment). It is never
	// negative.
	Delta float64
}

// NewReader returns a Reader that derives increments from the given
// Gatherer. The returned Reader starts without a baseline, so the first
// successful Read reports every series as StateNew.
func NewReader(gatherer Gatherer) *Reader {
	return &Reader{
		gatherer: gatherer,
		entries:  map[string]*entry{},
	}
}

// Read performs one Gather and returns the interval increment of every
// counter and classic histogram series since the previous successful
// Read.
//
// Series present in the previous Gather but absent from the current one
// are emitted once with StateDisappeared and retained as a tombstone so a
// later return is reported as StateReappeared rather than StateNew. The
// returned slice is sorted by family name, labels, and kind and is owned
// by the caller.
//
// If Gather returns an error, Read returns that error and a nil slice and
// does not advance the baseline; retrying after a failure diffs against
// the last successful Gather.
func (r *Reader) Read() ([]*Report, error) {
	families, err := r.gatherer.Gather()
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	previous := r.entries
	current := make(map[string]*entry, len(previous))
	reports := make([]*Report, 0)
	seen := make(map[string]struct{}, len(previous))

	for _, family := range families {
		if family == nil || family.Name == nil || family.Type == nil {
			continue
		}
		familyName := family.GetName()
		switch family.GetType() {
		case dto.MetricType_COUNTER:
			for _, metric := range family.Metric {
				if metric == nil || metric.Counter == nil || metric.Counter.Value == nil {
					continue
				}
				key := seriesKey(familyName, metric.Label)
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}

				newEntry := &entry{
					kind:         KindCounter,
					family:       familyName,
					labels:       cloneLabels(metric.Label),
					counterValue: metric.Counter.GetValue(),
				}
				current[key] = newEntry
				reports = append(reports, buildCounterReport(newEntry, previous[key]))
			}
		case dto.MetricType_HISTOGRAM:
			for _, metric := range family.Metric {
				if metric == nil || metric.Histogram == nil {
					continue
				}
				histogram := metric.Histogram
				if isNativeHistogram(histogram) {
					continue
				}
				key := seriesKey(familyName, metric.Label)
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}

				bounds, cumulative := bucketValues(histogram)
				newEntry := &entry{
					kind:       KindHistogram,
					family:     familyName,
					labels:     cloneLabels(metric.Label),
					histCount:  histogramCount(histogram),
					histSum:    cloneFloat(histogram.SampleSum),
					bounds:     bounds,
					bucketVals: cumulative,
				}
				current[key] = newEntry
				reports = append(reports, buildHistogramReport(newEntry, previous[key]))
			}
		default:
			// Gauges, summaries, untyped families, and native histograms
			// are not cumulative counter-style series and are ignored.
			continue
		}
	}

	for key, old := range previous {
		if _, ok := seen[key]; ok {
			continue
		}
		if old.gone {
			// The series was already tombstoned and is still absent.
			// Keep the tombstone and do not emit another transition.
			current[key] = old
			continue
		}
		reports = append(reports, buildDisappearedReport(old))
		current[key] = &entry{
			kind:         old.kind,
			family:       old.family,
			labels:       old.labels,
			counterValue: old.counterValue,
			histCount:    old.histCount,
			histSum:      old.histSum,
			bounds:       old.bounds,
			bucketVals:   old.bucketVals,
			gone:         true,
		}
	}

	r.entries = current
	sortReports(reports)
	return reports, nil
}

// sortReports orders reports deterministically by family, labels, and
// kind.
func sortReports(reports []*Report) {
	sort.Slice(reports, func(i, j int) bool {
		a, b := reports[i], reports[j]
		if a.Family != b.Family {
			return a.Family < b.Family
		}
		if labels := compareLabels(a.Labels, b.Labels); labels != 0 {
			return labels < 0
		}
		return a.Kind < b.Kind
	})
}
