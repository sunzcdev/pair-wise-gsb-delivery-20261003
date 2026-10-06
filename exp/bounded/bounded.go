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

// Package bounded provides experimental metric vectors that bound the number
// of admitted label combinations.
//
// A bounded vector admits the first Config.Capacity distinct label
// combinations it observes and gives each of them its own series, exactly
// like a regular prometheus vector. Once the capacity is exhausted,
// observations for further combinations are merged into a shared overflow
// series in which the collapsible labels (see Config.CollapsibleLabels) carry
// the overflow value. No observation is ever dropped, so totals (e.g. the
// sum of a counter or the count and sum of a histogram) stay correct even
// under label cardinality explosions caused by untrusted input.
//
// The overflow value is reserved: submitting it as a user label value in a
// collapsible label position is rejected, so the overflow series can never be
// confused with a regular, admitted series.
//
// All methods are safe for concurrent use. Admission is serialized, so
// goroutines racing to submit new combinations can never exceed the
// configured capacity.
//
// This package is experimental and its API may change or be removed in the
// future.
package bounded

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// DefaultOverflowValue is the label value used to mark the overflow series
// when Config.OverflowValue is left empty.
const DefaultOverflowValue = "__overflow__"

// Config configures the bounded vectors created by NewCounterVec and
// NewHistogramVec.
type Config struct {
	// Capacity is the maximum number of distinct label combinations that get
	// their own series. Observations for combinations beyond the capacity are
	// merged into the overflow series. A capacity of zero sends every
	// observation to the overflow series. Deleting an admitted combination
	// frees its slot, see DeleteLabelValues.
	Capacity int

	// CollapsibleLabels lists the names of the labels that may be collapsed
	// into the overflow value once the capacity is exhausted. At least one
	// collapsible label is required and every listed name must be a label of
	// the vector. Labels not listed here keep their original values in the
	// overflow series, so their cardinality should be naturally bounded.
	CollapsibleLabels []string

	// OverflowValue is the label value marking the overflow series in the
	// collapsible label positions. It defaults to DefaultOverflowValue. The
	// value is reserved: submitting it as a user label value in a collapsible
	// label position is rejected, so the overflow series can never collide
	// with a legitimate user series.
	OverflowValue string
}

// metricVec is the subset of the typed prometheus vector implementations
// (e.g. *prometheus.CounterVec and *prometheus.HistogramVec) that the bounded
// vectors build upon.
type metricVec[M any] interface {
	GetMetricWithLabelValues(lvs ...string) (M, error)
	DeleteLabelValues(lvs ...string) bool
	Reset()
	Describe(ch chan<- *prometheus.Desc)
	Collect(ch chan<- prometheus.Metric)
}

// vec implements the admission control shared by CounterVec and HistogramVec.
// M is the metric handle type of the wrapped vector and V is the wrapped
// vector type.
type vec[M any, V metricVec[M]] struct {
	inner V

	labelNames    []string
	collapsible   []bool // Indexed like labelNames.
	overflowValue string
	capacity      int

	mtx sync.Mutex
	// admitted maps the encoded label values of admitted combinations to
	// their metric handles. len(admitted) never exceeds capacity.
	admitted map[string]M
	// overflow maps the encoded non-collapsible label values to the shared
	// overflow metric handle for that combination.
	overflow map[string]M
}

// newVec validates cfg and wraps inner with admission control. It panics on
// invalid configuration, consistent with the prometheus constructors. The
// provided labelNames slice is cloned and never retained.
func newVec[M any, V metricVec[M]](inner V, labelNames []string, cfg Config) *vec[M, V] {
	if cfg.Capacity < 0 {
		panic(fmt.Errorf("bounded: negative capacity %d", cfg.Capacity))
	}
	if len(cfg.CollapsibleLabels) == 0 {
		panic(errors.New("bounded: at least one collapsible label is required"))
	}
	overflowValue := cfg.OverflowValue
	if overflowValue == "" {
		overflowValue = DefaultOverflowValue
	}
	collapsible := make([]bool, len(labelNames))
	for _, name := range cfg.CollapsibleLabels {
		i := slices.Index(labelNames, name)
		if i < 0 {
			panic(fmt.Errorf("bounded: collapsible label %q is not a label of the vector", name))
		}
		collapsible[i] = true
	}
	return &vec[M, V]{
		inner:         inner,
		labelNames:    slices.Clone(labelNames),
		collapsible:   collapsible,
		overflowValue: overflowValue,
		capacity:      cfg.Capacity,
		admitted:      map[string]M{},
		overflow:      map[string]M{},
	}
}

// getMetric returns the metric handle for the provided label values, which is
// either the handle of the admitted combination or the shared overflow
// handle. It is safe for concurrent use.
func (v *vec[M, V]) getMetric(lvs []string) (M, error) {
	var zero M
	if len(lvs) != len(v.labelNames) {
		return zero, fmt.Errorf("bounded: expected %d label values, got %d", len(v.labelNames), len(lvs))
	}
	for i, lv := range lvs {
		if v.collapsible[i] && lv == v.overflowValue {
			return zero, fmt.Errorf("bounded: label value %q is reserved for the overflow series (label %q)", v.overflowValue, v.labelNames[i])
		}
	}
	key := encodeKey(lvs)
	v.mtx.Lock()
	defer v.mtx.Unlock()
	if m, ok := v.admitted[key]; ok {
		return m, nil
	}
	if len(v.admitted) < v.capacity {
		m, err := v.inner.GetMetricWithLabelValues(lvs...)
		if err != nil {
			return zero, err
		}
		v.admitted[key] = m
		return m, nil
	}
	return v.overflowMetricLocked(lvs)
}

// overflowMetricLocked returns the shared overflow metric handle for the
// provided label values, creating the overflow series on first use. v.mtx
// must be held.
func (v *vec[M, V]) overflowMetricLocked(lvs []string) (M, error) {
	var zero M
	key := encodeOverflowKey(lvs, v.collapsible)
	if m, ok := v.overflow[key]; ok {
		return m, nil
	}
	olvs := make([]string, len(lvs))
	for i, lv := range lvs {
		if v.collapsible[i] {
			olvs[i] = v.overflowValue
		} else {
			olvs[i] = lv
		}
	}
	m, err := v.inner.GetMetricWithLabelValues(olvs...)
	if err != nil {
		return zero, err
	}
	v.overflow[key] = m
	return m, nil
}

// labelValues extracts the label values from labels in the order of the
// vector's label names. The provided map is only read, never retained.
func (v *vec[M, V]) labelValues(labels prometheus.Labels) ([]string, error) {
	if len(labels) != len(v.labelNames) {
		return nil, fmt.Errorf("bounded: expected %d labels, got %d", len(v.labelNames), len(labels))
	}
	lvs := make([]string, len(v.labelNames))
	for i, name := range v.labelNames {
		lv, ok := labels[name]
		if !ok {
			return nil, fmt.Errorf("bounded: missing label %q", name)
		}
		lvs[i] = lv
	}
	return lvs, nil
}

// deleteLabelValues deletes the series with the provided label values and
// reports whether a series was deleted. Deleting an admitted combination
// frees its capacity slot, so a subsequently submitted combination can be
// admitted again. Passing the overflow value in every collapsible label
// position deletes the overflow series for the remaining label values; the
// overflow series never occupies a capacity slot.
func (v *vec[M, V]) deleteLabelValues(lvs []string) bool {
	if len(lvs) != len(v.labelNames) {
		return false
	}
	v.mtx.Lock()
	defer v.mtx.Unlock()
	if v.isOverflowValues(lvs) {
		key := encodeOverflowKey(lvs, v.collapsible)
		if _, ok := v.overflow[key]; !ok {
			return false
		}
		delete(v.overflow, key)
		return v.inner.DeleteLabelValues(lvs...)
	}
	key := encodeKey(lvs)
	if _, ok := v.admitted[key]; !ok {
		return false
	}
	delete(v.admitted, key)
	return v.inner.DeleteLabelValues(lvs...)
}

// isOverflowValues reports whether the provided label values address an
// overflow series, i.e. every collapsible position carries the overflow
// value.
func (v *vec[M, V]) isOverflowValues(lvs []string) bool {
	for i, lv := range lvs {
		if v.collapsible[i] && lv != v.overflowValue {
			return false
		}
	}
	return true
}

// reset deletes all admitted and overflow series and frees all capacity
// slots.
func (v *vec[M, V]) reset() {
	v.mtx.Lock()
	defer v.mtx.Unlock()
	v.admitted = map[string]M{}
	v.overflow = map[string]M{}
	v.inner.Reset()
}

// encodeKey encodes label values into an unambiguous map key by
// length-prefixing each value.
func encodeKey(lvs []string) string {
	var sb strings.Builder
	for _, lv := range lvs {
		sb.WriteString(strconv.Itoa(len(lv)))
		sb.WriteByte(':')
		sb.WriteString(lv)
	}
	return sb.String()
}

// encodeOverflowKey encodes only the non-collapsible label values into an
// unambiguous map key, as those identify an overflow series.
func encodeOverflowKey(lvs []string, collapsible []bool) string {
	var sb strings.Builder
	for i, lv := range lvs {
		if collapsible[i] {
			continue
		}
		sb.WriteString(strconv.Itoa(len(lv)))
		sb.WriteByte(':')
		sb.WriteString(lv)
	}
	return sb.String()
}
