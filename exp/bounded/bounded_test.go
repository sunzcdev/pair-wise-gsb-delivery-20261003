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
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/exp/bounded"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestCounterVecOverflow verifies that combinations beyond the capacity are
// merged into the overflow series and that the scraped totals still account
// for every observation.
func TestCounterVecOverflow(t *testing.T) {
	reg := prometheus.NewRegistry()
	cv := bounded.NewCounterVec(
		prometheus.CounterOpts{Name: "requests_total", Help: "Test counter."},
		[]string{"code", "request"},
		bounded.Config{Capacity: 2, CollapsibleLabels: []string{"request"}},
	)
	reg.MustRegister(cv)

	cv.WithLabelValues("200", "/a").Inc()
	cv.WithLabelValues("200", "/b").Add(2)
	cv.WithLabelValues("200", "/c").Add(4)  // Over capacity, merged into the overflow series.
	cv.WithLabelValues("404", "/c").Add(8)  // Over capacity, different non-collapsible label.
	cv.WithLabelValues("404", "/d").Add(16) // Over capacity, same overflow series as the previous one.

	// The scraped sum is 1+2+4+8+16=31, i.e. no observation is lost.
	expected := `
# HELP requests_total Test counter.
# TYPE requests_total counter
requests_total{code="200",request="/a"} 1
requests_total{code="200",request="/b"} 2
requests_total{code="200",request="__overflow__"} 4
requests_total{code="404",request="__overflow__"} 24
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "requests_total"); err != nil {
		t.Fatal(err)
	}
}

// TestHistogramVecOverflow verifies the overflow behavior of bounded
// histograms and cross-checks the scraped total count and sum against the
// observed values.
func TestHistogramVecOverflow(t *testing.T) {
	reg := prometheus.NewRegistry()
	hv := bounded.NewHistogramVec(
		prometheus.HistogramOpts{Name: "request_duration_seconds", Help: "Test histogram.", Buckets: []float64{0.1, 1}},
		[]string{"code", "request"},
		bounded.Config{Capacity: 2, CollapsibleLabels: []string{"request"}},
	)
	reg.MustRegister(hv)

	hv.WithLabelValues("200", "/a").Observe(0.05)
	hv.WithLabelValues("200", "/b").Observe(0.5)
	hv.WithLabelValues("200", "/c").Observe(0.2) // Over capacity.
	hv.WithLabelValues("404", "/x").Observe(2)   // Over capacity.

	expected := `
# HELP request_duration_seconds Test histogram.
# TYPE request_duration_seconds histogram
request_duration_seconds_bucket{code="200",request="/a",le="0.1"} 1
request_duration_seconds_bucket{code="200",request="/a",le="1"} 1
request_duration_seconds_bucket{code="200",request="/a",le="+Inf"} 1
request_duration_seconds_sum{code="200",request="/a"} 0.05
request_duration_seconds_count{code="200",request="/a"} 1
request_duration_seconds_bucket{code="200",request="/b",le="0.1"} 0
request_duration_seconds_bucket{code="200",request="/b",le="1"} 1
request_duration_seconds_bucket{code="200",request="/b",le="+Inf"} 1
request_duration_seconds_sum{code="200",request="/b"} 0.5
request_duration_seconds_count{code="200",request="/b"} 1
request_duration_seconds_bucket{code="200",request="__overflow__",le="0.1"} 0
request_duration_seconds_bucket{code="200",request="__overflow__",le="1"} 1
request_duration_seconds_bucket{code="200",request="__overflow__",le="+Inf"} 1
request_duration_seconds_sum{code="200",request="__overflow__"} 0.2
request_duration_seconds_count{code="200",request="__overflow__"} 1
request_duration_seconds_bucket{code="404",request="__overflow__",le="0.1"} 0
request_duration_seconds_bucket{code="404",request="__overflow__",le="1"} 0
request_duration_seconds_bucket{code="404",request="__overflow__",le="+Inf"} 1
request_duration_seconds_sum{code="404",request="__overflow__"} 2
request_duration_seconds_count{code="404",request="__overflow__"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "request_duration_seconds"); err != nil {
		t.Fatal(err)
	}

	// Cross-check the totals from the gathered families rather than the
	// internal state: every observation must be accounted for.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var count uint64
	var sum float64
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			count += m.GetHistogram().GetSampleCount()
			sum += m.GetHistogram().GetSampleSum()
		}
	}
	if want := uint64(4); count != want {
		t.Errorf("got total sample count %d, want %d", count, want)
	}
	if want := 0.05 + 0.5 + 0.2 + 2; sum != want {
		t.Errorf("got total sample sum %g, want %g", sum, want)
	}
}

// TestCounterVecConcurrentAdmission hammers a bounded counter from many
// goroutines with distinct label combinations and verifies via the scraped
// output that the capacity is never exceeded and no observation is lost.
func TestCounterVecConcurrentAdmission(t *testing.T) {
	const (
		capacity   = 16
		goroutines = 8
		perWorker  = 64
	)
	reg := prometheus.NewRegistry()
	cv := bounded.NewCounterVec(
		prometheus.CounterOpts{Name: "requests_total", Help: "Test counter."},
		[]string{"request"},
		bounded.Config{Capacity: capacity, CollapsibleLabels: []string{"request"}},
	)
	reg.MustRegister(cv)

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				cv.WithLabelValues(fmt.Sprintf("worker-%d-req-%d", g, i)).Inc()
			}
		}()
	}
	wg.Wait()

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var series, sum float64
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			series++
			sum += m.GetCounter().GetValue()
		}
	}
	// With a single collapsible label there is exactly one overflow series,
	// so the number of series is the capacity plus one.
	if want := float64(capacity + 1); series != want {
		t.Errorf("got %v series, want %v (capacity plus one overflow series)", series, want)
	}
	if want := float64(goroutines * perWorker); sum != want {
		t.Errorf("got scraped total %v, want %v", sum, want)
	}
}

// TestCounterVecDeleteFreesCapacity verifies that deleting an admitted
// combination frees its capacity slot and that the overflow series can be
// deleted explicitly via the overflow value.
func TestCounterVecDeleteFreesCapacity(t *testing.T) {
	reg := prometheus.NewRegistry()
	cv := bounded.NewCounterVec(
		prometheus.CounterOpts{Name: "requests_total", Help: "Test counter."},
		[]string{"request"},
		bounded.Config{Capacity: 1, CollapsibleLabels: []string{"request"}},
	)
	reg.MustRegister(cv)

	cv.WithLabelValues("/a").Inc()
	cv.WithLabelValues("/b").Add(2) // Over capacity, merged into the overflow series.

	if deleted := cv.DeleteLabelValues("/a"); !deleted {
		t.Fatal("expected deletion of the admitted combination to succeed")
	}
	if deleted := cv.DeleteLabelValues("/never-admitted"); deleted {
		t.Fatal("expected deletion of an unknown combination to report false")
	}

	// The freed slot allows the next combination to be admitted with its own
	// series.
	cv.WithLabelValues("/c").Add(4)

	expected := `
# HELP requests_total Test counter.
# TYPE requests_total counter
requests_total{request="/c"} 4
requests_total{request="__overflow__"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "requests_total"); err != nil {
		t.Fatal(err)
	}

	// The overflow series is deleted by addressing it with the overflow
	// value.
	if deleted := cv.DeleteLabelValues("__overflow__"); !deleted {
		t.Fatal("expected deletion of the overflow series to succeed")
	}
	expected = `
# HELP requests_total Test counter.
# TYPE requests_total counter
requests_total{request="/c"} 4
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "requests_total"); err != nil {
		t.Fatal(err)
	}
}

// TestOverflowValueReserved verifies that the overflow value cannot be used
// as a user label value in a collapsible label position, while it remains a
// legal value for non-collapsible labels.
func TestOverflowValueReserved(t *testing.T) {
	reg := prometheus.NewRegistry()
	cv := bounded.NewCounterVec(
		prometheus.CounterOpts{Name: "requests_total", Help: "Test counter."},
		[]string{"code", "request"},
		bounded.Config{Capacity: 1, CollapsibleLabels: []string{"request"}},
	)
	reg.MustRegister(cv)

	if _, err := cv.GetMetricWithLabelValues("200", "__overflow__"); err == nil {
		t.Fatal("expected an error for the reserved overflow value in a collapsible label")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected WithLabelValues to panic for the reserved overflow value")
			}
		}()
		cv.WithLabelValues("200", "__overflow__")
	}()

	// The overflow value is a legal user value in a non-collapsible label.
	cv.WithLabelValues("__overflow__", "/a").Inc()
	expected := `
# HELP requests_total Test counter.
# TYPE requests_total counter
requests_total{code="__overflow__",request="/a"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "requests_total"); err != nil {
		t.Fatal(err)
	}
}

// TestCustomOverflowValue verifies that a custom overflow value is used for
// the overflow series.
func TestCustomOverflowValue(t *testing.T) {
	reg := prometheus.NewRegistry()
	cv := bounded.NewCounterVec(
		prometheus.CounterOpts{Name: "requests_total", Help: "Test counter."},
		[]string{"request"},
		bounded.Config{Capacity: 0, CollapsibleLabels: []string{"request"}, OverflowValue: "other"},
	)
	reg.MustRegister(cv)

	cv.WithLabelValues("/a").Inc()
	cv.WithLabelValues("/b").Add(2)

	expected := `
# HELP requests_total Test counter.
# TYPE requests_total counter
requests_total{request="other"} 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "requests_total"); err != nil {
		t.Fatal(err)
	}
}

// TestGetMetricWithLabels verifies the map-based access paths, including
// error handling for inconsistent label sets.
func TestGetMetricWithLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	cv := bounded.NewCounterVec(
		prometheus.CounterOpts{Name: "requests_total", Help: "Test counter."},
		[]string{"code", "request"},
		bounded.Config{Capacity: 1, CollapsibleLabels: []string{"request"}},
	)
	reg.MustRegister(cv)

	if _, err := cv.GetMetricWith(prometheus.Labels{"code": "200"}); err == nil {
		t.Fatal("expected an error for a missing label")
	}
	if _, err := cv.GetMetricWith(prometheus.Labels{"code": "200", "request": "/a", "extra": "x"}); err == nil {
		t.Fatal("expected an error for an extra label")
	}
	if _, err := cv.GetMetricWithLabelValues("200"); err == nil {
		t.Fatal("expected an error for too few label values")
	}

	cv.With(prometheus.Labels{"code": "200", "request": "/a"}).Inc()
	cv.With(prometheus.Labels{"code": "200", "request": "/b"}).Add(2) // Over capacity.

	expected := `
# HELP requests_total Test counter.
# TYPE requests_total counter
requests_total{code="200",request="/a"} 1
requests_total{code="200",request="__overflow__"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "requests_total"); err != nil {
		t.Fatal(err)
	}

	if deleted := cv.Delete(prometheus.Labels{"code": "200", "request": "/a"}); !deleted {
		t.Fatal("expected map-based deletion of the admitted combination to succeed")
	}
	if deleted := cv.Delete(prometheus.Labels{"code": "200"}); deleted {
		t.Fatal("expected map-based deletion with a missing label to report false")
	}
}

// TestReset verifies that Reset removes all series and frees all capacity
// slots.
func TestReset(t *testing.T) {
	reg := prometheus.NewRegistry()
	cv := bounded.NewCounterVec(
		prometheus.CounterOpts{Name: "requests_total", Help: "Test counter."},
		[]string{"request"},
		bounded.Config{Capacity: 1, CollapsibleLabels: []string{"request"}},
	)
	reg.MustRegister(cv)

	cv.WithLabelValues("/a").Inc()
	cv.WithLabelValues("/b").Inc()
	cv.Reset()

	// After the reset the capacity is free again, so the next combination is
	// admitted with its own series.
	cv.WithLabelValues("/c").Add(3)
	expected := `
# HELP requests_total Test counter.
# TYPE requests_total counter
requests_total{request="/c"} 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "requests_total"); err != nil {
		t.Fatal(err)
	}
}

// TestNewVecPanics verifies that invalid configurations panic, consistent
// with the prometheus constructors.
func TestNewVecPanics(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  bounded.Config
	}{
		{name: "negative capacity", cfg: bounded.Config{Capacity: -1, CollapsibleLabels: []string{"request"}}},
		{name: "no collapsible labels", cfg: bounded.Config{Capacity: 1}},
		{name: "unknown collapsible label", cfg: bounded.Config{Capacity: 1, CollapsibleLabels: []string{"unknown"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("expected NewCounterVec to panic")
				}
			}()
			bounded.NewCounterVec(
				prometheus.CounterOpts{Name: "requests_total", Help: "Test counter."},
				[]string{"code", "request"},
				tc.cfg,
			)
		})
	}
}
