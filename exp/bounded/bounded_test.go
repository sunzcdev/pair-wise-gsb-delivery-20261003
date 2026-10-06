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

package bounded_test

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/prometheus/client_golang/exp/bounded"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// gatherMetricFamily gathers and returns the metric family with the given
// name. All assertions in this file are based on gathered output, never on
// internal bookkeeping.
func gatherMetricFamily(t *testing.T, reg *prometheus.Registry, name string) *dto.MetricFamily {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	t.Fatalf("metric family %q not found in gather output", name)
	return nil
}

// labelValue returns the value of the given label of the metric, or false.
func labelValue(m *dto.Metric, name string) (string, bool) {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue(), true
		}
	}
	return "", false
}

// countOverflowSeries returns the number of series in the family that carry
// the overflow marker in any label.
func countOverflowSeries(mf *dto.MetricFamily) int {
	var n int
	for _, m := range mf.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetValue() == bounded.OverflowLabelValue {
				n++
				break
			}
		}
	}
	return n
}

func counterTotal(mf *dto.MetricFamily) float64 {
	var total float64
	for _, m := range mf.GetMetric() {
		total += m.GetCounter().GetValue()
	}
	return total
}

func histogramTotals(mf *dto.MetricFamily) (count uint64, sum float64) {
	for _, m := range mf.GetMetric() {
		count += m.GetHistogram().GetSampleCount()
		sum += m.GetHistogram().GetSampleSum()
	}
	return count, sum
}

func newTestCounterVec(t *testing.T, maxSeries int) (*bounded.CounterVec, *prometheus.Registry) {
	t.Helper()
	vec, err := bounded.NewCounterVec(bounded.CounterVecConfig{
		Opts:           prometheus.CounterOpts{Name: "requests_total", Help: "Test counter."},
		VariableLabels: []string{"path", "code"},
		VecConfig: bounded.VecConfig{
			MaxSeries:      maxSeries,
			FoldableLabels: []string{"path"},
		},
	})
	if err != nil {
		t.Fatalf("creating CounterVec: %v", err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(vec)
	return vec, reg
}

func TestCounterVecOverflowPreservesTotal(t *testing.T) {
	vec, reg := newTestCounterVec(t, 3)

	// Three combinations are admitted, the rest overflows.
	for i, path := range []string{"/a", "/b", "/c", "/d", "/e"} {
		counter := vec.WithLabelValues(path, "200")
		for j := 0; j <= i; j++ {
			counter.Inc()
		}
	}
	// More observations for an already overflowed combination.
	vec.With(prometheus.Labels{"path": "/d", "code": "200"}).Add(10)

	mf := gatherMetricFamily(t, reg, "requests_total")

	if got, want := counterTotal(mf), float64(1+2+3+4+5+10); got != want {
		t.Errorf("total across gathered series = %v, want %v", got, want)
	}
	if got, want := len(mf.GetMetric()), 4; got != want {
		t.Errorf("gathered series = %d, want %d (3 admitted + 1 overflow)", got, want)
	}
	if got, want := countOverflowSeries(mf), 1; got != want {
		t.Errorf("overflow series = %d, want %d", got, want)
	}

	// The overflow series keeps the non-foldable label and folds the
	// foldable one. It aggregates /d (4+10) and /e (5).
	for _, m := range mf.GetMetric() {
		path, _ := labelValue(m, "path")
		code, _ := labelValue(m, "code")
		if path == bounded.OverflowLabelValue {
			if code != "200" {
				t.Errorf("overflow series has code %q, want %q", code, "200")
			}
			if got, want := m.GetCounter().GetValue(), float64(19); got != want {
				t.Errorf("overflow series value = %v, want %v", got, want)
			}
		}
	}
}

func TestCounterVecDistinctOverflowSeriesPerNonFoldableLabel(t *testing.T) {
	vec, reg := newTestCounterVec(t, 1)

	vec.WithLabelValues("/a", "200").Inc()
	vec.WithLabelValues("/b", "200").Inc()
	vec.WithLabelValues("/c", "404").Inc()
	vec.WithLabelValues("/d", "404").Inc()

	mf := gatherMetricFamily(t, reg, "requests_total")
	if got, want := counterTotal(mf), float64(4); got != want {
		t.Errorf("total across gathered series = %v, want %v", got, want)
	}
	// One admitted series plus one overflow series per distinct
	// non-foldable label value ("200" and "404").
	if got, want := countOverflowSeries(mf), 2; got != want {
		t.Errorf("overflow series = %d, want %d", got, want)
	}
}

func TestCounterVecConcurrentAdmission(t *testing.T) {
	const (
		maxSeries    = 10
		combinations = 100
		workers      = 8
	)
	vec, reg := newTestCounterVec(t, maxSeries)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < combinations; i++ {
				// Every worker touches every combination, so each
				// combination is submitted concurrently by multiple
				// goroutines.
				vec.WithLabelValues(fmt.Sprintf("/p%d", i), "200").Inc()
			}
		}(w)
	}
	wg.Wait()

	mf := gatherMetricFamily(t, reg, "requests_total")

	if got, want := counterTotal(mf), float64(workers*combinations); got != want {
		t.Errorf("total across gathered series = %v, want %v", got, want)
	}
	// Exactly maxSeries admitted series plus exactly one overflow series,
	// no matter how the goroutines interleaved.
	if got, want := len(mf.GetMetric()), maxSeries+1; got != want {
		t.Errorf("gathered series = %d, want %d", got, want)
	}
	if got, want := countOverflowSeries(mf), 1; got != want {
		t.Errorf("overflow series = %d, want %d", got, want)
	}
	// The overflow series aggregates all non-admitted combinations.
	for _, m := range mf.GetMetric() {
		if path, _ := labelValue(m, "path"); path == bounded.OverflowLabelValue {
			if got, want := m.GetCounter().GetValue(), float64(workers*(combinations-maxSeries)); got != want {
				t.Errorf("overflow series value = %v, want %v", got, want)
			}
		}
	}
}

func TestCounterVecDeleteDoesNotFreeSlot(t *testing.T) {
	vec, reg := newTestCounterVec(t, 1)

	vec.WithLabelValues("/a", "200").Inc()
	if !vec.DeleteLabelValues("/a", "200") {
		t.Fatal("deleting admitted series returned false, want true")
	}
	// The slot is not freed: /b must overflow even though /a is gone.
	vec.WithLabelValues("/b", "200").Add(2)

	mf := gatherMetricFamily(t, reg, "requests_total")
	if got, want := counterTotal(mf), float64(2); got != want {
		t.Errorf("total across gathered series = %v, want %v", got, want)
	}
	if got, want := len(mf.GetMetric()), 1; got != want {
		t.Fatalf("gathered series = %d, want %d (overflow only)", got, want)
	}
	if path, _ := labelValue(mf.GetMetric()[0], "path"); path != bounded.OverflowLabelValue {
		t.Errorf("series path label = %q, want overflow marker", path)
	}

	// Deleting an overflowed combination is a no-op.
	if vec.DeleteLabelValues("/b", "200") {
		t.Error("deleting overflowed combination returned true, want false")
	}
	if got, want := counterTotal(gatherMetricFamily(t, reg, "requests_total")), float64(2); got != want {
		t.Errorf("total after deleting overflowed combination = %v, want %v", got, want)
	}

	// Reset releases all slots: /c is admitted as a regular series again.
	vec.Reset()
	vec.WithLabelValues("/c", "200").Inc()
	mf = gatherMetricFamily(t, reg, "requests_total")
	if got, want := len(mf.GetMetric()), 1; got != want {
		t.Fatalf("gathered series after Reset = %d, want %d", got, want)
	}
	if path, _ := labelValue(mf.GetMetric()[0], "path"); path != "/c" {
		t.Errorf("series path label after Reset = %q, want %q", path, "/c")
	}
}

func TestCounterVecOverflowMarkerCollision(t *testing.T) {
	if !utf8.ValidString(bounded.OverflowLabelValue) {
		t.Fatal("overflow marker must be valid UTF-8, the registry rejects invalid UTF-8 label values at gather time")
	}

	vec, reg := newTestCounterVec(t, 2)

	// Legal user values that merely look like an overflow marker are
	// admitted as regular series.
	vec.WithLabelValues("overflow", "200").Inc()
	vec.WithLabelValues("__overflow", "200").Inc()

	// The reserved marker itself is rejected.
	if _, err := vec.GetCounterWithLabelValues(bounded.OverflowLabelValue, "200"); err == nil {
		t.Error("using the reserved overflow marker as a label value returned no error")
	}
	if _, err := vec.GetCounterWith(prometheus.Labels{"path": bounded.OverflowLabelValue, "code": "200"}); err == nil {
		t.Error("using the reserved overflow marker in a Labels map returned no error")
	}

	mf := gatherMetricFamily(t, reg, "requests_total")
	if got, want := counterTotal(mf), float64(2); got != want {
		t.Errorf("total across gathered series = %v, want %v", got, want)
	}
	if got := countOverflowSeries(mf); got != 0 {
		t.Errorf("overflow series = %d, want 0", got)
	}
}

func TestHistogramVecOverflowPreservesCountAndSum(t *testing.T) {
	vec, err := bounded.NewHistogramVec(bounded.HistogramVecConfig{
		Opts: prometheus.HistogramOpts{
			Name:    "request_duration_seconds",
			Help:    "Test histogram.",
			Buckets: []float64{0.1, 1, 10},
		},
		VariableLabels: []string{"path", "method"},
		VecConfig: bounded.VecConfig{
			MaxSeries:      2,
			FoldableLabels: []string{"path"},
		},
	})
	if err != nil {
		t.Fatalf("creating HistogramVec: %v", err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(vec)

	var wantSum float64
	var wantCount uint64
	for i, path := range []string{"/a", "/b", "/c", "/d"} {
		for j := 0; j <= i; j++ {
			v := float64(i+1) * 0.05
			vec.WithLabelValues(path, "GET").Observe(v)
			wantSum += v
			wantCount++
		}
	}

	mf := gatherMetricFamily(t, reg, "request_duration_seconds")
	count, sum := histogramTotals(mf)
	if count != wantCount {
		t.Errorf("total sample count across gathered series = %d, want %d", count, wantCount)
	}
	if math.Abs(sum-wantSum) > 1e-9 {
		t.Errorf("total sample sum across gathered series = %v, want %v", sum, wantSum)
	}
	if got, want := len(mf.GetMetric()), 3; got != want {
		t.Errorf("gathered series = %d, want %d (2 admitted + 1 overflow)", got, want)
	}
	if got, want := countOverflowSeries(mf), 1; got != want {
		t.Errorf("overflow series = %d, want %d", got, want)
	}
}

func TestVecConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  bounded.VecConfig
	}{
		{name: "negative MaxSeries", cfg: bounded.VecConfig{MaxSeries: -1, FoldableLabels: []string{"path"}}},
		{name: "no foldable labels", cfg: bounded.VecConfig{MaxSeries: 1}},
		{name: "unknown foldable label", cfg: bounded.VecConfig{MaxSeries: 1, FoldableLabels: []string{"unknown"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bounded.NewCounterVec(bounded.CounterVecConfig{
				Opts:           prometheus.CounterOpts{Name: "test_total", Help: "Test."},
				VariableLabels: []string{"path"},
				VecConfig:      tc.cfg,
			})
			if err == nil {
				t.Error("expected an error, got nil")
			}
			_, err = bounded.NewHistogramVec(bounded.HistogramVecConfig{
				Opts:           prometheus.HistogramOpts{Name: "test_seconds", Help: "Test."},
				VariableLabels: []string{"path"},
				VecConfig:      tc.cfg,
			})
			if err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestCounterVecLabelCountMismatch(t *testing.T) {
	vec, _ := newTestCounterVec(t, 1)
	if _, err := vec.GetCounterWithLabelValues("/a"); err == nil {
		t.Error("expected an error for too few label values, got nil")
	}
	if _, err := vec.GetCounterWith(prometheus.Labels{"path": "/a"}); err == nil {
		t.Error("expected an error for a missing label, got nil")
	}
	if _, err := vec.GetCounterWith(prometheus.Labels{"path": "/a", "code": "200", "extra": "x"}); err == nil {
		t.Error("expected an error for an unexpected label, got nil")
	}
}

// TestCounterVecOverflowMarkerInExposition sanity-checks that the marker is
// visually recognizable in text exposition output.
func TestCounterVecOverflowMarkerInExposition(t *testing.T) {
	vec, reg := newTestCounterVec(t, 0)
	vec.WithLabelValues("/a", "200").Inc()

	mf := gatherMetricFamily(t, reg, "requests_total")
	var buf strings.Builder
	for _, m := range mf.GetMetric() {
		for _, lp := range m.GetLabel() {
			buf.WriteString(lp.GetValue())
		}
	}
	if !strings.Contains(buf.String(), "overflow") {
		t.Errorf("exposition output %q does not contain a recognizable overflow marker", buf.String())
	}
}
