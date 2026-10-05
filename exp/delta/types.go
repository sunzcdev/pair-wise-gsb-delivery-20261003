// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package delta derives interval increments of cumulative Prometheus
// metrics from a Gatherer without mutating the source metrics.
//
// A service usually exposes its metrics in the cumulative Prometheus text
// format through an /metrics endpoint while an internal statistics backend
// expects per-interval increments (for example one report every minute).
// A [Reader] bridges the two worlds: every call to [Reader.Read] runs
// Gather on the wrapped [Gatherer], compares the result against the reader's
// private baseline and emits [CounterDelta] and [HistogramDelta] values.
//
// Each Reader keeps an independent baseline, so multiple readers with
// different cadences can be attached to the same Gatherer. Readers never
// call Reset or any other mutating method on the original metrics; only the
// gathered DTO values are inspected.
//
// Discontinuities are reported explicitly instead of being hidden behind a
// negative subtraction:
//
//   - [StatusReset] means the cumulative value went backwards, typically
//     because the process or counter was restarted. The increment for the
//     interval is zero and the new cumulative value becomes the baseline.
//   - [StatusDisappeared] means the series was present in the previous
//     successful gather but is absent now.
//   - [StatusReappeared] means the series was absent in at least one
//     successful gather and is present again. The interval increment cannot
//     be related to the last known baseline, so it is reported as zero and
//     the new value becomes the baseline.
//   - [StatusNew] means the series is observed for the first time.
//   - [StatusBucketLayoutChanged] is specific to classic histograms and
//     means the set of bucket upper bounds changed. Bucket increments for
//     the interval are not meaningful, so they are reported as zero and the
//     buckets are re-baselined.
//
// A failed Gather never advances the baseline: no deltas are produced for
// the interval and the next successful Read is compared against the state
// from the last successful Gather.
package delta

import "time"

// Status classifies the relationship of a series to the reader's baseline.
type Status int

const (
	// StatusOK means the series existed in the baseline and its value is
	// monotonically non-decreasing. The reported increment is valid.
	StatusOK Status = iota
	// StatusNew means the series has never been observed by this reader
	// before. No increment can be computed, so the increment is zero and
	// the current value becomes the baseline.
	StatusNew
	// StatusReset means a cumulative value decreased compared with the
	// baseline. The increment is not computed (reported as zero) and the
	// current value becomes the new baseline.
	StatusReset
	// StatusDisappeared means the series was present in the previous
	// successful gather but is absent from the current one. Value fields
	// carry no increment in this status.
	StatusDisappeared
	// StatusReappeared means the series disappeared in at least one
	// successful gather and is present again. The increment across the
	// gap cannot be related to the stale baseline, so it is reported as
	// zero and the current value becomes the baseline.
	StatusReappeared
	// StatusBucketLayoutChanged means the bucket upper bounds of a
	// classic histogram differ from the baseline. Bucket and count/sum
	// increments for the interval are reported as zero and the histogram
	// is re-baselined.
	StatusBucketLayoutChanged
)

// String returns a stable textual representation of the status, suitable
// for log lines and statistics backends.
func (s Status) String() string {
	switch s {
	case StatusNew:
		return "new"
	case StatusReset:
		return "reset"
	case StatusDisappeared:
		return "disappeared"
	case StatusReappeared:
		return "reappeared"
	case StatusBucketLayoutChanged:
		return "bucket_layout_changed"
	default:
		return "ok"
	}
}

// CounterDelta is the interval state of one counter series.
type CounterDelta struct {
	// Family is the metric family name, for example "http_requests_total".
	Family string
	// Labels are the fully qualified series labels, sorted by label name.
	// The map is owned by the report and may be retained by the receiver.
	Labels map[string]string
	// Delta is the counter increment since the reader's baseline. It is
	// zero for every status other than StatusOK.
	Delta float64
	// Value is the current cumulative counter value as returned by Gather.
	Value float64
	// Status classifies the series for this interval.
	Status Status

	key string
}

// BucketDelta is the interval increment of one classic histogram bucket.
type BucketDelta struct {
	// UpperBound is the inclusive upper bound of the bucket.
	UpperBound float64
	// Delta is the increment of the bucket's cumulative count since the
	// reader's baseline. It is zero whenever the bucket layout changed or
	// the histogram reset.
	Delta float64
	// Cumulative is the current cumulative count of the bucket as
	// returned by Gather, including the implicit +Inf bucket.
	Cumulative float64
}

// HistogramDelta is the interval state of one classic histogram series.
type HistogramDelta struct {
	// Family is the metric family name, for example "rpc_duration_seconds".
	Family string
	// Labels are the fully qualified series labels, sorted by label name.
	// The map is owned by the report and may be retained by the receiver.
	Labels map[string]string
	// Buckets holds one entry per current bucket upper bound, ordered by
	// increasing upper bound and including the implicit +Inf bucket.
	Buckets []BucketDelta
	// CountDelta is the increment of the total observation count since
	// the reader's baseline. It is zero for every status other than
	// StatusOK.
	CountDelta float64
	// Count is the current cumulative observation count as returned by
	// Gather.
	Count float64
	// SumDelta is the increment of the observation sum since the reader's
	// baseline. It is zero for every status other than StatusOK and when
	// the sum is not a finite number.
	SumDelta float64
	// Sum is the current cumulative observation sum as returned by Gather.
	Sum float64
	// Status classifies the series for this interval.
	Status Status

	key string
}

// Report is the result of one successful [Reader.Read] call.
type Report struct {
	// Time is the wall-clock time at which Gather was invoked.
	Time time.Time
	// Counters holds one entry per counter series, including series that
	// disappeared during the interval. Entries are sorted by family name
	// and then by label set.
	Counters []CounterDelta
	// Histograms holds one entry per classic histogram series, including
	// series that disappeared during the interval. Entries are sorted by
	// family name and then by label set.
	Histograms []HistogramDelta
}
