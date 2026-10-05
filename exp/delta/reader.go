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
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// Gatherer is the subset of prometheus.Gatherer that a Reader needs. It is
// satisfied by *prometheus.Registry and by prometheus.HTTPClient-based
// gatherers. Defining it here keeps this package independent of the core
// client_golang module.
type Gatherer interface {
	// Gather returns metric families in the text exposition order.
	Gather() ([]*dto.MetricFamily, error)
}

// Option configures a Reader.
type Option func(*Reader)

// WithStaleness sets how long the baseline of a disappeared series is kept
// for reappearance detection. A series is reported as StatusDisappeared on
// every successful gather at which it is absent, but its baseline is only
// removed after it has been absent continuously for at least d. If it comes
// back before removal, it is reported as StatusReappeared; after removal it
// is reported as StatusNew. A zero or negative value keeps disappeared
// series forever. The default is 5 minutes.
func WithStaleness(d time.Duration) Option {
	return func(r *Reader) {
		r.staleness = d
	}
}

// WithClock overrides the clock used to timestamp reports and to expire
// disappeared series. It is mainly useful in tests with simulated time.
func WithClock(now func() time.Time) Option {
	return func(r *Reader) {
		r.now = now
	}
}

// Reader computes interval increments from repeated Gather calls.
//
// Every Reader owns a private baseline that is only updated from a
// successful Gather. Readers are safe for concurrent use and must not be
// copied after first use.
type Reader struct {
	gatherer  Gatherer
	staleness time.Duration
	now       func() time.Time

	mu         sync.Mutex
	primed     bool
	counters   map[string]*counterState
	histograms map[string]*histogramState
}

type counterState struct {
	family  string
	labels  map[string]string
	value   float64
	missing bool
	since   time.Time
}

type histogramState struct {
	family  string
	labels  map[string]string
	bounds  []float64
	buckets []float64
	count   float64
	sum     float64
	missing bool
	since   time.Time
}

// NewReader creates a Reader that derives increments from g. The first
// successful Read establishes the baseline and reports all present counter
// and classic histogram series as StatusNew.
func NewReader(g Gatherer, opts ...Option) *Reader {
	r := &Reader{
		gatherer:   g,
		staleness:  5 * time.Minute,
		now:        time.Now,
		counters:   map[string]*counterState{},
		histograms: map[string]*histogramState{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Read gathers the metrics once and returns the interval increments since
// the last successful Read.
//
// If Gather fails, Read returns the gather error together with a nil report
// and leaves the baseline untouched, so the next successful Read compares
// against the same baseline again.
func (r *Reader) Read() (*Report, error) {
	readAt := r.now()
	families, err := r.gatherer.Gather()
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	report := &Report{Time: readAt}

	seenCounters := make(map[string]struct{}, len(r.counters))
	seenHistograms := make(map[string]struct{}, len(r.histograms))

	for _, family := range families {
		switch family.GetType() {
		case dto.MetricType_COUNTER:
			for _, metric := range family.GetMetric() {
				labels, key := seriesIdentity(family.GetName(), metric.GetLabel())
				seenCounters[key] = struct{}{}
				report.Counters = append(report.Counters,
					r.consumeCounter(family.GetName(), labels, key, metric.GetCounter().GetValue(), readAt))
			}
		case dto.MetricType_HISTOGRAM:
			for _, metric := range family.GetMetric() {
				if metric.GetHistogram().GetSchema() != 0 {
					// Native histograms use a different bucket model and
					// are intentionally not supported by this reader.
					continue
				}
				labels, key := seriesIdentity(family.GetName(), metric.GetLabel())
				seenHistograms[key] = struct{}{}
				report.Histograms = append(report.Histograms,
					r.consumeHistogram(family.GetName(), labels, key, metric.GetHistogram(), readAt))
			}
		}
	}

	for key, state := range r.counters {
		if _, ok := seenCounters[key]; ok {
			continue
		}
		report.Counters = append(report.Counters, state.disappear(key, readAt))
	}
	for key, state := range r.histograms {
		if _, ok := seenHistograms[key]; ok {
			continue
		}
		report.Histograms = append(report.Histograms, state.disappear(key, readAt))
	}

	r.expire(readAt)
	r.primed = true

	sort.Slice(report.Counters, func(i, j int) bool {
		return report.Counters[i].key < report.Counters[j].key
	})
	sort.Slice(report.Histograms, func(i, j int) bool {
		return report.Histograms[i].key < report.Histograms[j].key
	})

	return report, nil
}

func (r *Reader) consumeCounter(family string, labels map[string]string, key string, value float64, at time.Time) CounterDelta {
	delta := CounterDelta{Family: family, Labels: labels, Value: value, key: key}

	state, ok := r.counters[key]
	if !ok {
		r.counters[key] = &counterState{
			family: family,
			labels: labels,
			value:  value,
			since:  at,
		}
		delta.Status = StatusNew
		return delta
	}

	if state.missing {
		state.missing = false
		state.value = value
		state.since = at
		delta.Status = StatusReappeared
		return delta
	}

	if value < state.value {
		delta.Delta = 0
		delta.Status = StatusReset
		state.value = value
		state.since = at
		return delta
	}

	delta.Delta = value - state.value
	delta.Status = StatusOK
	state.value = value
	return delta
}

func (r *Reader) consumeHistogram(family string, labels map[string]string, key string, hist *dto.Histogram, at time.Time) HistogramDelta {
	bounds, cumulative := classicBuckets(hist)
	count := histogramCount(hist)
	sum := hist.GetSampleSum()

	delta := HistogramDelta{
		Family:  family,
		Labels:  labels,
		Buckets: bucketDeltas(bounds, cumulative),
		Count:   count,
		Sum:     sum,
		key:     key,
	}

	state, ok := r.histograms[key]
	if !ok {
		r.histograms[key] = &histogramState{
			family:  family,
			labels:  labels,
			bounds:  bounds,
			buckets: cumulative,
			count:   count,
			sum:     sum,
			since:   at,
		}
		delta.Status = StatusNew
		return delta
	}

	if state.missing {
		state.missing = false
		state.rebaseline(bounds, cumulative, count, sum, at)
		delta.Status = StatusReappeared
		return delta
	}

	if !sameBounds(state.bounds, bounds) {
		state.rebaseline(bounds, cumulative, count, sum, at)
		delta.Buckets = bucketDeltas(bounds, cumulative)
		delta.Status = StatusBucketLayoutChanged
		return delta
	}

	layoutReset := false
	perBucket := make([]BucketDelta, len(bounds))
	for i := range bounds {
		d := cumulative[i] - state.buckets[i]
		if d < 0 {
			layoutReset = true
		}
		perBucket[i] = BucketDelta{UpperBound: bounds[i], Delta: d, Cumulative: cumulative[i]}
	}

	countReset := count < state.count
	sumReset := finite(sum) && finite(state.sum) && sum < state.sum

	if layoutReset || countReset || sumReset {
		state.rebaseline(bounds, cumulative, count, sum, at)
		delta.Buckets = bucketDeltas(bounds, cumulative)
		delta.Status = StatusReset
		return delta
	}

	delta.Buckets = perBucket
	delta.CountDelta = count - state.count
	if finite(sum) && finite(state.sum) {
		delta.SumDelta = sum - state.sum
	}
	delta.Status = StatusOK
	state.buckets = cumulative
	state.count = count
	state.sum = sum
	return delta
}

func (s *counterState) disappear(key string, at time.Time) CounterDelta {
	if !s.missing {
		s.missing = true
		s.since = at
	}
	return CounterDelta{
		Family: s.family,
		Labels: s.labels,
		Value:  s.value,
		Status: StatusDisappeared,
		key:    key,
	}
}

func (s *histogramState) disappear(key string, at time.Time) HistogramDelta {
	if !s.missing {
		s.missing = true
		s.since = at
	}
	return HistogramDelta{
		Family:  s.family,
		Labels:  s.labels,
		Buckets: bucketDeltas(s.bounds, s.buckets),
		Count:   s.count,
		Sum:     s.sum,
		Status:  StatusDisappeared,
		key:     key,
	}
}

func (s *histogramState) rebaseline(bounds, cumulative []float64, count, sum float64, at time.Time) {
	s.bounds = bounds
	s.buckets = cumulative
	s.count = count
	s.sum = sum
	s.since = at
}

func (r *Reader) expire(at time.Time) {
	if r.staleness <= 0 {
		return
	}
	for key, state := range r.counters {
		if state.missing && at.Sub(state.since) >= r.staleness {
			delete(r.counters, key)
		}
	}
	for key, state := range r.histograms {
		if state.missing && at.Sub(state.since) >= r.staleness {
			delete(r.histograms, key)
		}
	}
}

// seriesIdentity extracts the label set and a deterministic series key from
// gathered label pairs. The label pairs are copied into a sorted map so the
// caller may freely reuse or mutate the gathered DTOs.
func seriesIdentity(family string, pairs []*dto.LabelPair) (map[string]string, string) {
	labels := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		labels[pair.GetName()] = pair.GetValue()
	}

	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString(family)
	for _, name := range names {
		b.WriteByte(0)
		b.WriteString(name)
		b.WriteByte(1)
		b.WriteString(labels[name])
	}
	return labels, b.String()
}

// classicBuckets converts the gathered bucket representation into parallel
// slices of upper bounds and cumulative counts, appending the implicit +Inf
// bucket backed by the histogram sample count.
func classicBuckets(hist *dto.Histogram) (bounds, cumulative []float64) {
	raw := hist.GetBucket()
	bounds = make([]float64, 0, len(raw)+1)
	cumulative = make([]float64, 0, len(raw)+1)
	for _, bucket := range raw {
		bounds = append(bounds, bucket.GetUpperBound())
		cumulative = append(cumulative, bucketCumulative(bucket))
	}
	bounds = append(bounds, math.Inf(1))
	cumulative = append(cumulative, histogramCount(hist))
	return bounds, cumulative
}

func bucketCumulative(bucket *dto.Bucket) float64 {
	if f := bucket.GetCumulativeCountFloat(); f > 0 {
		return f
	}
	return float64(bucket.GetCumulativeCount())
}

func histogramCount(hist *dto.Histogram) float64 {
	if f := hist.GetSampleCountFloat(); f > 0 {
		return f
	}
	return float64(hist.GetSampleCount())
}

func bucketDeltas(bounds, cumulative []float64) []BucketDelta {
	deltas := make([]BucketDelta, len(bounds))
	for i := range bounds {
		deltas[i] = BucketDelta{UpperBound: bounds[i], Cumulative: cumulative[i]}
	}
	return deltas
}

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

func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
