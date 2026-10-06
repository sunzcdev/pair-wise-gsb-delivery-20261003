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

// Package bounded provides experimental cardinality-bounded metric vectors.
//
// A bounded vector admits up to a configured maximum number of distinct
// label combinations as regular series. Observations for any further
// combination are folded into shared overflow series, in which the values of
// a configured subset of labels are replaced by OverflowLabelValue. Totals
// are never dropped: the sum over all admitted and overflow series always
// equals the sum over all observations.
//
// The overflow marker cannot collide with user series: OverflowLabelValue
// is reserved, and any observation carrying it as a label value is rejected
// with an error. A gathered series therefore carries the marker if and only
// if it is an overflow series.
//
// This package is experimental and may change or be removed in the future.
package bounded

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// OverflowLabelValue is the label value used to mark folded labels in
// overflow series.
//
// The value is reserved: the bounded vectors reject any observation that
// uses it as a label value, so no admitted (regular) series can ever carry
// it and the marker can never collide with a user-supplied label value.
// Reserving a value is the only way to guarantee this, because Prometheus
// label values may be any valid UTF-8 string, and the registry enforces
// valid UTF-8 label values at gather time, which rules out markers that
// are structurally illegal (such as invalid UTF-8 byte sequences).
const OverflowLabelValue = "__overflow__"

// keySeparator separates label values in bookkeeping keys. The byte 0xFF
// never occurs in valid UTF-8, and only combinations that pass the inner
// vector's UTF-8 validation are ever stored, which makes the joined keys of
// all stored entries unambiguous.
const keySeparator = "\xff"

// VecConfig configures the bounding behavior shared by CounterVec and
// HistogramVec.
type VecConfig struct {
	// MaxSeries is the maximum number of label combinations admitted as
	// regular series. Once MaxSeries combinations have been admitted,
	// observations for any new combination are folded into overflow series.
	// Zero means every observation is folded into an overflow series.
	//
	// Admission slots are not freed by Delete or DeleteLabelValues; only
	// Reset releases all slots. See the Delete documentation for the
	// rationale.
	MaxSeries int

	// FoldableLabels names the variable labels whose values are replaced by
	// OverflowLabelValue in overflow series. It must contain at least one
	// label, and every entry must be one of the vector's variable labels.
	//
	// Labels not listed here keep their original values in overflow series,
	// so the number of overflow series is bounded by the cardinality of the
	// non-foldable labels. Keep the non-foldable labels low-cardinality.
	FoldableLabels []string
}

// validate checks the configuration against the variable labels of the
// vector and returns the foldable flags indexed by label position.
func (c VecConfig) validate(variableLabels []string) ([]bool, error) {
	if c.MaxSeries < 0 {
		return nil, fmt.Errorf("MaxSeries must not be negative, got %d", c.MaxSeries)
	}
	if len(c.FoldableLabels) == 0 {
		return nil, errors.New("FoldableLabels must contain at least one label, otherwise overflow series would be unbounded")
	}
	foldable := make([]bool, len(variableLabels))
	for _, name := range c.FoldableLabels {
		idx := -1
		for i, vl := range variableLabels {
			if vl == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, fmt.Errorf("foldable label %q is not one of the variable labels %v", name, variableLabels)
		}
		foldable[idx] = true
	}
	return foldable, nil
}

// core implements the admission and folding logic shared by the typed
// vectors. It wraps a prometheus.MetricVec that holds both the admitted
// series and the overflow series.
type core struct {
	inner          *prometheus.MetricVec
	variableLabels []string
	foldable       []bool
	maxSeries      int

	mtx sync.RWMutex
	// admitted counts how many admission slots have ever been handed out
	// since the last Reset. It deliberately does not decrease on Delete.
	admitted int
	// entries maps the joined label values of admitted combinations to
	// their metric. Its size is bounded by maxSeries. Overflowed
	// combinations keep no per-combination state at all, so the memory
	// footprint stays bounded even under a label-value flood.
	entries map[string]prometheus.Metric
}

func newCore(inner *prometheus.MetricVec, variableLabels []string, cfg VecConfig) (*core, error) {
	foldable, err := cfg.validate(variableLabels)
	if err != nil {
		return nil, err
	}
	return &core{
		inner:          inner,
		variableLabels: variableLabels,
		foldable:       foldable,
		maxSeries:      cfg.MaxSeries,
		entries:        map[string]prometheus.Metric{},
	}, nil
}

// get returns the metric responsible for the given label values, admitting
// a new series or resolving to an overflow series as needed.
func (c *core) get(lvs []string) (prometheus.Metric, error) {
	if len(lvs) != len(c.variableLabels) {
		return nil, fmt.Errorf("expected %d label values, got %d", len(c.variableLabels), len(lvs))
	}
	for _, lv := range lvs {
		if lv == OverflowLabelValue {
			return nil, fmt.Errorf("label value %q is reserved for overflow series", OverflowLabelValue)
		}
	}

	key := strings.Join(lvs, keySeparator)
	c.mtx.RLock()
	metric, ok := c.entries[key]
	c.mtx.RUnlock()
	if ok {
		return metric, nil
	}

	c.mtx.Lock()
	if metric, ok = c.entries[key]; ok {
		c.mtx.Unlock()
		return metric, nil
	}
	if c.admitted < c.maxSeries {
		// The admission decision and the bookkeeping update happen under
		// the same lock, so concurrent first-time submissions can never
		// exceed maxSeries admitted series, and the same combination is
		// admitted exactly once.
		c.admitted++
		metric, err := c.inner.GetMetricWithLabelValues(lvs...)
		if err != nil {
			// Unreachable: the label count was validated above and the
			// inner vector shares this vector's variable labels.
			c.admitted--
			c.mtx.Unlock()
			return nil, err
		}
		c.entries[key] = metric
		c.mtx.Unlock()
		return metric, nil
	}
	c.mtx.Unlock()

	// Overflow: fold the foldable label values and resolve through the
	// inner vector, which deduplicates concurrent creations of the same
	// overflow series. No per-combination state is kept on this path.
	return c.inner.GetMetricWithLabelValues(c.fold(lvs)...)
}

// fold returns a copy of lvs with all foldable label values replaced by
// OverflowLabelValue. The caller must not retain the returned slice.
func (c *core) fold(lvs []string) []string {
	folded := make([]string, len(lvs))
	for i, lv := range lvs {
		if c.foldable[i] {
			folded[i] = OverflowLabelValue
		} else {
			folded[i] = lv
		}
	}
	return folded
}

// labelValues maps a Labels map onto the variable label order. The caller
// must not retain the returned slice; the caller retains ownership of the
// labels map.
func (c *core) labelValues(labels prometheus.Labels) ([]string, error) {
	lvs := make([]string, len(c.variableLabels))
	for i, name := range c.variableLabels {
		lv, ok := labels[name]
		if !ok {
			return nil, fmt.Errorf("label name %q missing in label map", name)
		}
		lvs[i] = lv
	}
	if len(labels) != len(c.variableLabels) {
		return nil, fmt.Errorf("%d unexpected label(s) found in label map", len(labels)-len(c.variableLabels))
	}
	return lvs, nil
}

// delete removes the bookkeeping entry and the inner series for an admitted
// combination. It reports whether an admitted combination was deleted.
// Overflowed combinations hold no per-combination state, so deleting them
// is a no-op that returns false.
func (c *core) delete(lvs []string) bool {
	key := strings.Join(lvs, keySeparator)
	c.mtx.Lock()
	_, ok := c.entries[key]
	if ok {
		delete(c.entries, key)
	}
	c.mtx.Unlock()
	if !ok {
		return false
	}
	// The admission slot is intentionally not freed: c.admitted is left
	// unchanged, so a later combination is not admitted in its place.
	return c.inner.DeleteLabelValues(lvs...)
}

func (c *core) reset() {
	c.mtx.Lock()
	c.admitted = 0
	c.entries = map[string]prometheus.Metric{}
	c.mtx.Unlock()
	c.inner.Reset()
}

// CounterVecConfig configures a CounterVec.
type CounterVecConfig struct {
	// Opts are the options for the underlying counters, as for
	// prometheus.NewCounterVec.
	Opts prometheus.CounterOpts
	// VariableLabels are the names of the variable labels, as for
	// prometheus.NewCounterVec.
	VariableLabels []string
	// VecConfig configures the bounding behavior.
	VecConfig
}

// CounterVec is a cardinality-bounded variant of prometheus.CounterVec.
// See the package documentation for the bounding semantics. Create one with
// NewCounterVec.
type CounterVec struct {
	inner *prometheus.CounterVec
	core  *core
}

// NewCounterVec returns a bounded CounterVec or an error if the
// configuration is invalid.
func NewCounterVec(cfg CounterVecConfig) (*CounterVec, error) {
	inner := prometheus.NewCounterVec(cfg.Opts, cfg.VariableLabels)
	core, err := newCore(inner.MetricVec, cfg.VariableLabels, cfg.VecConfig)
	if err != nil {
		return nil, err
	}
	return &CounterVec{inner: inner, core: core}, nil
}

// GetCounterWithLabelValues returns the Counter for the given label values
// (same order as the variable labels). The first MaxSeries distinct
// combinations get their own series; observations for later combinations
// are folded into overflow series. An error is returned if the number of
// label values is wrong or a label value equals the reserved
// OverflowLabelValue.
func (v *CounterVec) GetCounterWithLabelValues(lvs ...string) (prometheus.Counter, error) {
	metric, err := v.core.get(lvs)
	if err != nil {
		return nil, err
	}
	return metric.(prometheus.Counter), nil
}

// GetCounterWith returns the Counter for the given Labels map. It behaves
// like GetCounterWithLabelValues. The map is not retained beyond the call.
func (v *CounterVec) GetCounterWith(labels prometheus.Labels) (prometheus.Counter, error) {
	lvs, err := v.core.labelValues(labels)
	if err != nil {
		return nil, err
	}
	return v.GetCounterWithLabelValues(lvs...)
}

// WithLabelValues works like GetCounterWithLabelValues but panics on error.
func (v *CounterVec) WithLabelValues(lvs ...string) prometheus.Counter {
	counter, err := v.GetCounterWithLabelValues(lvs...)
	if err != nil {
		panic(err)
	}
	return counter
}

// With works like GetCounterWith but panics on error. The map is not
// retained beyond the call.
func (v *CounterVec) With(labels prometheus.Labels) prometheus.Counter {
	counter, err := v.GetCounterWith(labels)
	if err != nil {
		panic(err)
	}
	return counter
}

// DeleteLabelValues deletes the admitted series with the given label values
// and reports whether such a series existed.
//
// Deleting an admitted series does not free its admission slot: slots are
// only ever released by Reset. A combination submitted after its slot was
// deleted is folded into an overflow series instead of being re-admitted.
// This keeps series from oscillating between admitted and overflow (which
// would look like counter resets to rate()) and prevents delete/recreate
// cycles from bypassing the cardinality bound.
//
// Deleting a combination that was folded into an overflow series is a no-op
// returning false: overflowed combinations hold no per-combination state,
// and the shared overflow series (which may aggregate other combinations)
// is retained so that totals are never lost.
func (v *CounterVec) DeleteLabelValues(lvs ...string) bool {
	return v.core.delete(lvs)
}

// Delete deletes the admitted series matching the given Labels map. See
// DeleteLabelValues for the slot-release semantics.
func (v *CounterVec) Delete(labels prometheus.Labels) bool {
	lvs, err := v.core.labelValues(labels)
	if err != nil {
		return false
	}
	return v.core.delete(lvs)
}

// Reset deletes all admitted and overflow series and releases all admission
// slots, returning the vector to its initial state.
func (v *CounterVec) Reset() {
	v.core.reset()
}

// Describe implements prometheus.Collector.
func (v *CounterVec) Describe(ch chan<- *prometheus.Desc) {
	v.inner.Describe(ch)
}

// Collect implements prometheus.Collector.
func (v *CounterVec) Collect(ch chan<- prometheus.Metric) {
	v.inner.Collect(ch)
}

// HistogramVecConfig configures a HistogramVec.
type HistogramVecConfig struct {
	// Opts are the options for the underlying histograms, as for
	// prometheus.NewHistogramVec.
	Opts prometheus.HistogramOpts
	// VariableLabels are the names of the variable labels, as for
	// prometheus.NewHistogramVec.
	VariableLabels []string
	// VecConfig configures the bounding behavior.
	VecConfig
}

// HistogramVec is a cardinality-bounded variant of
// prometheus.HistogramVec. See the package documentation for the bounding
// semantics. Create one with NewHistogramVec.
type HistogramVec struct {
	inner *prometheus.HistogramVec
	core  *core
}

// NewHistogramVec returns a bounded HistogramVec or an error if the
// configuration is invalid.
func NewHistogramVec(cfg HistogramVecConfig) (*HistogramVec, error) {
	inner := prometheus.NewHistogramVec(cfg.Opts, cfg.VariableLabels)
	core, err := newCore(inner.MetricVec, cfg.VariableLabels, cfg.VecConfig)
	if err != nil {
		return nil, err
	}
	return &HistogramVec{inner: inner, core: core}, nil
}

// GetObserverWithLabelValues returns the Observer for the given label
// values (same order as the variable labels). The first MaxSeries distinct
// combinations get their own series; observations for later combinations
// are folded into overflow series. An error is returned if the number of
// label values is wrong or a label value equals the reserved
// OverflowLabelValue.
func (v *HistogramVec) GetObserverWithLabelValues(lvs ...string) (prometheus.Observer, error) {
	metric, err := v.core.get(lvs)
	if err != nil {
		return nil, err
	}
	return metric.(prometheus.Observer), nil
}

// GetObserverWith returns the Observer for the given Labels map. It behaves
// like GetObserverWithLabelValues. The map is not retained beyond the call.
func (v *HistogramVec) GetObserverWith(labels prometheus.Labels) (prometheus.Observer, error) {
	lvs, err := v.core.labelValues(labels)
	if err != nil {
		return nil, err
	}
	return v.GetObserverWithLabelValues(lvs...)
}

// WithLabelValues works like GetObserverWithLabelValues but panics on
// error.
func (v *HistogramVec) WithLabelValues(lvs ...string) prometheus.Observer {
	observer, err := v.GetObserverWithLabelValues(lvs...)
	if err != nil {
		panic(err)
	}
	return observer
}

// With works like GetObserverWith but panics on error. The map is not
// retained beyond the call.
func (v *HistogramVec) With(labels prometheus.Labels) prometheus.Observer {
	observer, err := v.GetObserverWith(labels)
	if err != nil {
		panic(err)
	}
	return observer
}

// DeleteLabelValues deletes the admitted series with the given label values
// and reports whether such a series existed. See
// CounterVec.DeleteLabelValues for the slot-release semantics, which are
// identical here.
func (v *HistogramVec) DeleteLabelValues(lvs ...string) bool {
	return v.core.delete(lvs)
}

// Delete deletes the admitted series matching the given Labels map. See
// CounterVec.DeleteLabelValues for the slot-release semantics, which are
// identical here.
func (v *HistogramVec) Delete(labels prometheus.Labels) bool {
	lvs, err := v.core.labelValues(labels)
	if err != nil {
		return false
	}
	return v.core.delete(lvs)
}

// Reset deletes all admitted and overflow series and releases all admission
// slots, returning the vector to its initial state.
func (v *HistogramVec) Reset() {
	v.core.reset()
}

// Describe implements prometheus.Collector.
func (v *HistogramVec) Describe(ch chan<- *prometheus.Desc) {
	v.inner.Describe(ch)
}

// Collect implements prometheus.Collector.
func (v *HistogramVec) Collect(ch chan<- prometheus.Metric) {
	v.inner.Collect(ch)
}
