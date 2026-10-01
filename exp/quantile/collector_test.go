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

package quantile_test

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/exp/quantile"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCollectorExposition(t *testing.T) {
	c, err := quantile.New(quantile.Opts{
		Name:       "request_duration_seconds",
		Help:       "Request duration distribution.",
		Quantiles:  []float64{0.5, 0.9, 0.99},
		BufferSize: 2048, // Larger than the observation count: exact results.
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 1000; i++ {
		c.Observe(float64(i))
	}

	expected := `
		# HELP request_duration_seconds Request duration distribution.
		# TYPE request_duration_seconds summary
		request_duration_seconds{quantile="0.5"} 500
		request_duration_seconds{quantile="0.9"} 900
		request_duration_seconds{quantile="0.99"} 990
		request_duration_seconds_sum 500500
		request_duration_seconds_count 1000
	`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Error(err)
	}
}

// TestCollectorScrapedByPrometheus performs an end-to-end scrape
// through promhttp and verifies that the exposition parses cleanly.
func TestCollectorScrapedByPrometheus(t *testing.T) {
	c, err := quantile.New(quantile.Opts{
		Namespace:  "test",
		Name:       "quantiles",
		BufferSize: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 100; i++ {
		c.Observe(float64(i))
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("scrape returned status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE test_quantiles summary",
		`test_quantiles{quantile="0.5"} 50`,
		"test_quantiles_count 100",
		"test_quantiles_sum 5050",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scraped output does not contain %q", want)
		}
	}
}

// TestCollectorMergeAcrossWorkers simulates workers exporting digests
// that are merged into a central collector, which is then scraped.
func TestCollectorMergeAcrossWorkers(t *testing.T) {
	newWorker := func() *quantile.Collector {
		c, err := quantile.New(quantile.Opts{
			Name:       "worker_metric",
			Quantiles:  []float64{0.5, 0.9},
			BufferSize: 256,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	workers := []*quantile.Collector{newWorker(), newWorker(), newWorker()}
	for _, w := range workers {
		for j := 0; j < 5000; j++ {
			if j < 4900 {
				// Duplicate-heavy data: the median and the
				// 0.9-quantile are 42 regardless of the
				// random compaction offsets.
				w.Observe(42)
			} else {
				w.Observe(float64(j % 100))
			}
		}
	}

	central := newWorker()
	for _, w := range workers {
		if err := central.MergeDigest(w.Export()); err != nil {
			t.Fatal(err)
		}
	}

	expected := `
		# HELP worker_metric Quantiles of observed values, estimated by a mergeable sketch.
		# TYPE worker_metric summary
		worker_metric{quantile="0.5"} 42
		worker_metric{quantile="0.9"} 42
		worker_metric_sum 632250
		worker_metric_count 15000
	`
	if err := testutil.CollectAndCompare(central, strings.NewReader(expected)); err != nil {
		t.Error(err)
	}
	if bound := central.Sketch().RankErrorBound(); bound <= 0 {
		t.Errorf("expected a positive rank error bound after merging, got %v", bound)
	}
}

func TestCollectorInvalidOpts(t *testing.T) {
	if _, err := quantile.New(quantile.Opts{}); err == nil {
		t.Error("expected an error for an empty name")
	}
	if _, err := quantile.New(quantile.Opts{Name: "x", Quantiles: []float64{1.5}}); err == nil {
		t.Error("expected an error for an out-of-range quantile")
	}
	if _, err := quantile.New(quantile.Opts{Name: "x", BufferSize: 3}); err == nil {
		t.Error("expected an error for an invalid buffer size")
	}
}

func ExampleCollector() {
	c, _ := quantile.New(quantile.Opts{Name: "example_quantiles", BufferSize: 1024})
	for i := 1; i <= 100; i++ {
		c.Observe(float64(i))
	}
	fmt.Println(c.Sketch().Count())
	// Output: 100
}
