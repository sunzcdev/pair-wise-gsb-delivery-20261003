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

package bounded

import (
	"github.com/prometheus/client_golang/prometheus"
)

// HistogramVec is a bounded variant of *prometheus.HistogramVec. It admits up
// to Config.Capacity distinct label combinations with their own series and
// merges all further combinations into the shared overflow series, see the
// package documentation for details.
//
// This is an experimental API and may change or be removed in the future.
type HistogramVec struct {
	vec *vec[prometheus.Observer, *prometheus.HistogramVec]
}

// Compile-time check that HistogramVec implements prometheus.Collector.
var _ prometheus.Collector = (*HistogramVec)(nil)

// NewHistogramVec returns a bounded HistogramVec. It panics on invalid
// configuration, consistent with prometheus.NewHistogramVec.
func NewHistogramVec(opts prometheus.HistogramOpts, labelNames []string, cfg Config) *HistogramVec {
	return &HistogramVec{vec: newVec(prometheus.NewHistogramVec(opts, labelNames), labelNames, cfg)}
}

// GetMetricWithLabelValues returns the Observer for the given label values,
// which is the Observer of the admitted combination or, once the capacity is
// exhausted, the shared overflow Observer. It returns an error if the number
// of label values is wrong or a collapsible label position carries the
// reserved overflow value.
func (v *HistogramVec) GetMetricWithLabelValues(lvs ...string) (prometheus.Observer, error) {
	return v.vec.getMetric(lvs)
}

// WithLabelValues works as GetMetricWithLabelValues but panics on error,
// consistent with (*prometheus.HistogramVec).WithLabelValues.
func (v *HistogramVec) WithLabelValues(lvs ...string) prometheus.Observer {
	o, err := v.vec.getMetric(lvs)
	if err != nil {
		panic(err)
	}
	return o
}

// GetMetricWith works as GetMetricWithLabelValues but takes a map of
// labels. The map is only read, never retained.
func (v *HistogramVec) GetMetricWith(labels prometheus.Labels) (prometheus.Observer, error) {
	lvs, err := v.vec.labelValues(labels)
	if err != nil {
		return nil, err
	}
	return v.vec.getMetric(lvs)
}

// With works as GetMetricWith but panics on error, consistent with
// (*prometheus.HistogramVec).With. The map is only read, never retained.
func (v *HistogramVec) With(labels prometheus.Labels) prometheus.Observer {
	o, err := v.GetMetricWith(labels)
	if err != nil {
		panic(err)
	}
	return o
}

// DeleteLabelValues deletes the series with the given label values and
// reports whether a series was deleted. Deleting an admitted combination
// frees its capacity slot. Passing the overflow value in every collapsible
// label position deletes the overflow series for the remaining label values.
func (v *HistogramVec) DeleteLabelValues(lvs ...string) bool {
	return v.vec.deleteLabelValues(lvs)
}

// Delete works as DeleteLabelValues but takes a map of labels. The map is
// only read, never retained.
func (v *HistogramVec) Delete(labels prometheus.Labels) bool {
	lvs, err := v.vec.labelValues(labels)
	if err != nil {
		return false
	}
	return v.vec.deleteLabelValues(lvs)
}

// Reset deletes all admitted and overflow series and frees all capacity
// slots.
func (v *HistogramVec) Reset() {
	v.vec.reset()
}

// Describe implements prometheus.Collector.
func (v *HistogramVec) Describe(ch chan<- *prometheus.Desc) {
	v.vec.inner.Describe(ch)
}

// Collect implements prometheus.Collector.
func (v *HistogramVec) Collect(ch chan<- prometheus.Metric) {
	v.vec.inner.Collect(ch)
}
