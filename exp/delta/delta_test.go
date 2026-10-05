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

package delta_test

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/prometheus/client_golang/exp/delta"
)

const eps = 1e-9

// labelCmp compares label pairs as protobuf messages while treating empty
// and nil slices as equal.
var labelCmp = cmp.Options{protocmp.Transform(), cmpopts.EquateEmpty()}

// labelsOf is a convenience for constructing label pairs by name/value.
func labelsOf(pairs ...string) []*dto.LabelPair {
	if len(pairs)%2 != 0 {
		panic("labelsOf requires an even number of arguments")
	}
	labels := make([]*dto.LabelPair, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		name, value := pairs[i], pairs[i+1]
		labels = append(labels, &dto.LabelPair{Name: &name, Value: &value})
	}
	return labels
}

// findReport locates a report by family and labels, failing the test when
// it is absent.
func findReport(t *testing.T, reports []*delta.Report, family string, labels []*dto.LabelPair) *delta.Report {
	t.Helper()
	for _, report := range reports {
		if report.Family == family && cmp.Equal(report.Labels, labels, labelCmp) {
			return report
		}
	}
	t.Fatalf("report for family %s with labels %v not found in %d reports", family, labels, len(reports))
	return nil
}

// missingReport asserts that no report matches the given family and
// labels.
func missingReport(t *testing.T, reports []*delta.Report, family string, labels []*dto.LabelPair) {
	t.Helper()
	for _, report := range reports {
		if report.Family == family && cmp.Equal(report.Labels, labels, labelCmp) {
			t.Fatalf("unexpected report for family %s with labels %v", family, labels)
		}
	}
}

// gaugeCollector emits a gauge that the reader must ignore.
type gaugeCollector struct{}

func (gaugeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("ignored_gauge", "ignored gauge", nil, nil)
}

func (gaugeCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("ignored_gauge", "ignored gauge", nil, nil),
		prometheus.GaugeValue, 7,
	)
}

// snapshotCollector emits fixed counter and classic histogram values from
// a mutable snapshot so tests can model decreases, disappearances, and
// bucket layout changes without resetting real metrics.
type snapshotCollector struct {
	mu sync.Mutex

	counterPresent   bool
	counterNames     []string
	counterValues    []string
	counterValue     float64
	histPresent      bool
	histNames        []string
	histValues       []string
	histCount        uint64
	histSum          float64
	histBounds       []float64
	histCumulative   []uint64
	secondHist       bool
	secondCount      uint64
	secondBounds     []float64
	secondCumulative []uint64
}

func (s *snapshotCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("snap_counter", "snapshot counter", s.counterNames, nil)
	ch <- prometheus.NewDesc("snap_latency", "snapshot histogram", s.histNames, nil)
	ch <- prometheus.NewDesc("snap_restart", "snapshot reset histogram", nil, nil)
}

func (s *snapshotCollector) Collect(ch chan<- prometheus.Metric) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.counterPresent {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("snap_counter", "snapshot counter", s.counterNames, nil),
			prometheus.CounterValue, s.counterValue, s.counterValues...,
		)
	}
	if s.histPresent {
		buckets := map[float64]uint64{}
		for i, bound := range s.histBounds {
			buckets[bound] = s.histCumulative[i]
		}
		ch <- prometheus.MustNewConstHistogram(
			prometheus.NewDesc("snap_latency", "snapshot histogram", s.histNames, nil),
			s.histCount, s.histSum, buckets, s.histValues...,
		)
	}
	if s.secondHist {
		buckets := map[float64]uint64{}
		for i, bound := range s.secondBounds {
			buckets[bound] = s.secondCumulative[i]
		}
		ch <- prometheus.MustNewConstHistogram(
			prometheus.NewDesc("snap_restart", "snapshot reset histogram", nil, nil),
			s.secondCount, 0, buckets,
		)
	}
}

// flakyGatherer wraps a gatherer and fails a configurable number of times
// before passing through.
type flakyGatherer struct {
	inner   delta.Gatherer
	fail    bool
	failErr error
}

func (f *flakyGatherer) Gather() ([]*dto.MetricFamily, error) {
	if f.fail {
		return nil, f.failErr
	}
	return f.inner.Gather()
}

// assertCounter verifies a counter report's state and values.
func assertCounter(t *testing.T, report *delta.Report, state delta.State, value, deltaValue float64) {
	t.Helper()
	if report.Kind != delta.KindCounter {
		t.Fatalf("expected counter kind, got %s", report.Kind)
	}
	if report.State != state {
		t.Fatalf("expected state %s, got %s", state, report.State)
	}
	if report.Counter == nil {
		t.Fatalf("counter report payload is nil")
	}
	if diff := report.Counter.Value - value; math.Abs(diff) > eps {
		t.Fatalf("counter value: expected %v, got %v", value, report.Counter.Value)
	}
	if diff := report.Counter.Delta - deltaValue; math.Abs(diff) > eps {
		t.Fatalf("counter delta: expected %v, got %v", deltaValue, report.Counter.Delta)
	}
}

// assertBucket verifies a single per-bucket interval increment.
func assertBucket(t *testing.T, buckets []delta.BucketReport, index int, bound, cumulative, deltaValue float64) {
	t.Helper()
	if index >= len(buckets) {
		t.Fatalf("bucket index %d out of range (have %d buckets)", index, len(buckets))
	}
	bucket := buckets[index]
	if diff := bucket.UpperBound - bound; math.Abs(diff) > eps {
		t.Fatalf("bucket %d bound: expected %v, got %v", index, bound, bucket.UpperBound)
	}
	if diff := bucket.Cumulative - cumulative; math.Abs(diff) > eps {
		t.Fatalf("bucket %d cumulative: expected %v, got %v", index, cumulative, bucket.Cumulative)
	}
	if diff := bucket.Delta - deltaValue; math.Abs(diff) > eps {
		t.Fatalf("bucket %d delta: expected %v, got %v", index, deltaValue, bucket.Delta)
	}
}

// counterValueFromGather reads the raw cumulative counter value from a
// Gather result, exercising the original scrape interface unchanged.
func counterValueFromGather(t *testing.T, families []*dto.MetricFamily, name string, labels []*dto.LabelPair) float64 {
	t.Helper()
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if cmp.Equal(metric.Label, labels, labelCmp) {
				return metric.Counter.GetValue()
			}
		}
	}
	t.Fatalf("counter family %s not found in Gather result", name)
	return 0
}

// TestReaderTwoFrequencies runs two readers at different cadences against
// the same metrics and checks that each reports its own intervals, that
// the slower reader sees the telescoped sum of the faster reader's
// intervals, and that the original Gather output stays cumulative.
func TestReaderTwoFrequencies(t *testing.T) {
	snap := &snapshotCollector{
		counterPresent: true,
		counterNames:   []string{"code"},
		counterValues:  []string{"200"},
		counterValue:   10,
		histPresent:    true,
		histNames:      []string{"route"},
		histValues:     []string{"/api"},
		histCount:      12,
		histSum:        24,
		// Distribution: 4 in (-Inf,1], 3 in (1,2], 5 in (2,+Inf).
		histBounds:     []float64{1, 2},
		histCumulative: []uint64{4, 7},
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(snap, gaugeCollector{})

	fast := delta.NewReader(registry)
	slow := delta.NewReader(registry)

	code200 := labelsOf("code", "200")
	routeAPI := labelsOf("route", "/api")

	// First read for both readers establishes baselines as StateNew.
	fastReports, err := fast.Read()
	if err != nil {
		t.Fatalf("initial fast Read: %v", err)
	}
	assertCounter(t, findReport(t, fastReports, "snap_counter", code200), delta.StateNew, 10, 0)
	firstHist := findReport(t, fastReports, "snap_latency", routeAPI)
	if firstHist.State != delta.StateNew || firstHist.Histogram.Count != 12 ||
		firstHist.Histogram.CountDelta != 0 || *firstHist.Histogram.Sum != 24 {
		t.Fatalf("unexpected initial histogram report: %+v", firstHist)
	}

	slowReports, err := slow.Read()
	if err != nil {
		t.Fatalf("initial slow Read: %v", err)
	}
	assertCounter(t, findReport(t, slowReports, "snap_counter", code200), delta.StateNew, 10, 0)

	// Advance to an intermediate state; only the fast reader ticks.
	snap.mu.Lock()
	snap.counterValue = 13
	snap.histCount = 18
	snap.histSum = 39
	// Distribution: +3 in (-Inf,1], +3 in (1,2], total +6.
	snap.histCumulative = []uint64{7, 13}
	snap.mu.Unlock()

	fastReports, err = fast.Read()
	if err != nil {
		t.Fatalf("intermediate fast Read: %v", err)
	}
	assertCounter(t, findReport(t, fastReports, "snap_counter", code200), delta.StateIncrease, 13, 3)
	hist := findReport(t, fastReports, "snap_latency", routeAPI)
	if hist.State != delta.StateIncrease || hist.Histogram.CountDelta != 6 ||
		*hist.Histogram.SumDelta != 15 {
		t.Fatalf("unexpected intermediate histogram report: %+v", hist.Histogram)
	}
	if len(hist.Histogram.Buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(hist.Histogram.Buckets))
	}
	assertBucket(t, hist.Histogram.Buckets, 0, 1, 7, 3)
	assertBucket(t, hist.Histogram.Buckets, 1, 2, 13, 3)

	// The slow reader has not ticked, so its view still covers the full
	// span once it reads. Advance to the final state first.
	snap.mu.Lock()
	snap.counterValue = 18
	snap.histCount = 24
	snap.histSum = 63
	// From the intermediate state: +2 in (-Inf,1], +2 in (1,2], +2 above.
	snap.histCumulative = []uint64{9, 17}
	snap.mu.Unlock()

	// Fast reader reports only the second sub-interval.
	fastReports, err = fast.Read()
	if err != nil {
		t.Fatalf("final fast Read: %v", err)
	}
	assertCounter(t, findReport(t, fastReports, "snap_counter", code200), delta.StateIncrease, 18, 5)
	hist = findReport(t, fastReports, "snap_latency", routeAPI)
	if hist.Histogram.CountDelta != 6 || *hist.Histogram.SumDelta != 24 {
		t.Fatalf("unexpected second fast histogram report: %+v", hist.Histogram)
	}
	assertBucket(t, hist.Histogram.Buckets, 0, 1, 9, 2)
	assertBucket(t, hist.Histogram.Buckets, 1, 2, 17, 2)

	// Slow reader reports the telescoped interval from the baseline.
	slowReports, err = slow.Read()
	if err != nil {
		t.Fatalf("final slow Read: %v", err)
	}
	assertCounter(t, findReport(t, slowReports, "snap_counter", code200), delta.StateIncrease, 18, 8)
	hist = findReport(t, slowReports, "snap_latency", routeAPI)
	if hist.State != delta.StateIncrease || hist.Histogram.CountDelta != 12 ||
		*hist.Histogram.SumDelta != 39 {
		t.Fatalf("unexpected slow histogram report: %+v", hist.Histogram)
	}
	assertBucket(t, hist.Histogram.Buckets, 0, 1, 9, 5)
	assertBucket(t, hist.Histogram.Buckets, 1, 2, 17, 5)

	// The two readers must have independent baselines: another fast read is
	// unchanged while the slow reader still holds its older baseline.
	fastReports, err = fast.Read()
	if err != nil {
		t.Fatalf("unchanged fast Read: %v", err)
	}
	assertCounter(t, findReport(t, fastReports, "snap_counter", code200), delta.StateUnchanged, 18, 0)
	hist = findReport(t, fastReports, "snap_latency", routeAPI)
	if hist.State != delta.StateUnchanged {
		t.Fatalf("expected unchanged histogram, got %s", hist.State)
	}

	// The original scrape interface keeps exposing cumulative values, and
	// gauges are never reported as increments.
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("raw Gather: %v", err)
	}
	if got := counterValueFromGather(t, families, "snap_counter", code200); got != 18 {
		t.Fatalf("raw cumulative counter: expected 18, got %v", got)
	}
	for _, report := range fastReports {
		if report.Family == "ignored_gauge" {
			t.Fatalf("gauge must not be reported as a delta series")
		}
	}
}

// TestReaderFailedGatherDoesNotAdvanceBaseline verifies that a Gather
// error leaves the baseline untouched and that the next successful read
// diffs against the last successful one.
func TestReaderFailedGatherDoesNotAdvanceBaseline(t *testing.T) {
	snap := &snapshotCollector{
		counterPresent: true,
		counterValue:   5,
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(snap)

	flaky := &flakyGatherer{inner: registry, failErr: errors.New("scrape unavailable")}
	reader := delta.NewReader(flaky)

	if _, err := reader.Read(); err != nil {
		t.Fatalf("initial Read: %v", err)
	}

	snap.mu.Lock()
	snap.counterValue = 9
	snap.mu.Unlock()

	// Force the next Gather to fail; no report is produced.
	flaky.fail = true
	if reports, err := reader.Read(); err == nil {
		t.Fatalf("expected Gather error, got reports %v", reports)
	}
	flaky.fail = false

	snap.mu.Lock()
	snap.counterValue = 12
	snap.mu.Unlock()

	// The successful read must span across the failed one: 12 - 5 = 7.
	reports, err := reader.Read()
	if err != nil {
		t.Fatalf("recovered Read: %v", err)
	}
	assertCounter(t, findReport(t, reports, "snap_counter", nil), delta.StateIncrease, 12, 7)
}

// TestReaderCounterDecreaseIsReset makes sure a decreasing counter is
// classified as a reset and never produces a negative increment.
func TestReaderCounterDecreaseIsReset(t *testing.T) {
	snap := &snapshotCollector{
		counterPresent: true,
		counterValue:   100,
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(snap)
	reader := delta.NewReader(registry)

	if _, err := reader.Read(); err != nil {
		t.Fatalf("initial Read: %v", err)
	}

	// The source counter restarts at 40 (process restarted).
	snap.mu.Lock()
	snap.counterValue = 40
	snap.mu.Unlock()

	reports, err := reader.Read()
	if err != nil {
		t.Fatalf("reset Read: %v", err)
	}
	report := findReport(t, reports, "snap_counter", nil)
	assertCounter(t, report, delta.StateCounterReset, 40, 40)

	// After the reset the counter grows normally again.
	snap.mu.Lock()
	snap.counterValue = 45
	snap.mu.Unlock()
	reports, err = reader.Read()
	if err != nil {
		t.Fatalf("post-reset Read: %v", err)
	}
	assertCounter(t, findReport(t, reports, "snap_counter", nil), delta.StateIncrease, 45, 5)
}

// TestReaderDisappearAndReappear distinguishes a returning series from a
// brand-new one, including when the family loses all of its series in
// between.
func TestReaderDisappearAndReappear(t *testing.T) {
	snap := &snapshotCollector{
		counterPresent: true,
		counterNames:   []string{"job"},
		counterValues:  []string{"a"},
		counterValue:   3,
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(snap)
	reader := delta.NewReader(registry)

	jobA := labelsOf("job", "a")
	if _, err := reader.Read(); err != nil {
		t.Fatalf("initial Read: %v", err)
	}

	// Series disappears.
	snap.mu.Lock()
	snap.counterPresent = false
	snap.mu.Unlock()

	reports, err := reader.Read()
	if err != nil {
		t.Fatalf("disappear Read: %v", err)
	}
	assertCounter(t, findReport(t, reports, "snap_counter", jobA), delta.StateDisappeared, 3, 0)

	// A read while still absent must not re-emit the disappearance.
	reports, err = reader.Read()
	if err != nil {
		t.Fatalf("still-absent Read: %v", err)
	}
	missingReport(t, reports, "snap_counter", jobA)

	// It reappears with a restarted cumulative value.
	snap.mu.Lock()
	snap.counterPresent = true
	snap.counterValue = 1
	snap.mu.Unlock()

	reports, err = reader.Read()
	if err != nil {
		t.Fatalf("reappear Read: %v", err)
	}
	assertCounter(t, findReport(t, reports, "snap_counter", jobA), delta.StateReappeared, 1, 0)

	// A genuinely different series is StateNew, not StateReappeared.
	snap.mu.Lock()
	snap.counterValues = []string{"b"}
	snap.counterValue = 9
	snap.mu.Unlock()
	jobB := labelsOf("job", "b")
	reports, err = reader.Read()
	if err != nil {
		t.Fatalf("new-series Read: %v", err)
	}
	assertCounter(t, findReport(t, reports, "snap_counter", jobB), delta.StateNew, 9, 0)
	assertCounter(t, findReport(t, reports, "snap_counter", jobA), delta.StateDisappeared, 1, 0)
}

// TestReaderHistogramBucketLayoutChange verifies that a change in bucket
// upper bounds is surfaced explicitly, with zero per-bucket increments and
// a rebaseline, while layout-independent count and sum keep diffing.
func TestReaderHistogramBucketLayoutChange(t *testing.T) {
	snap := &snapshotCollector{
		histPresent:    true,
		histCount:      10,
		histSum:        10,
		histBounds:     []float64{1, 2},
		histCumulative: []uint64{5, 8},
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(snap)
	reader := delta.NewReader(registry)

	if _, err := reader.Read(); err != nil {
		t.Fatalf("initial Read: %v", err)
	}

	// The histogram is rebuilt with different bucket bounds but keeps
	// accumulating overall.
	snap.mu.Lock()
	snap.histCount = 14
	snap.histSum = 18
	snap.histBounds = []float64{0.5, 1, 2, 4}
	snap.histCumulative = []uint64{2, 6, 11, 13}
	snap.mu.Unlock()

	reports, err := reader.Read()
	if err != nil {
		t.Fatalf("layout-change Read: %v", err)
	}
	report := findReport(t, reports, "snap_latency", nil)
	if report.State != delta.StateBucketsChanged {
		t.Fatalf("expected buckets_changed, got %s", report.State)
	}
	if report.Histogram.CountDelta != 4 || *report.Histogram.SumDelta != 8 {
		t.Fatalf("count/sum must keep diffing: %+v", report.Histogram)
	}
	if len(report.Histogram.Buckets) != 4 {
		t.Fatalf("expected 4 current-layout buckets, got %d", len(report.Histogram.Buckets))
	}
	for i, bucket := range report.Histogram.Buckets {
		if bucket.Delta != 0 {
			t.Fatalf("bucket %d delta must be zero across layout change, got %v", i, bucket.Delta)
		}
	}

	// After the layout change, increments are computed against the new
	// layout.
	snap.mu.Lock()
	snap.histCount = 20
	snap.histSum = 34
	snap.histCumulative = []uint64{4, 10, 16, 19}
	snap.mu.Unlock()

	reports, err = reader.Read()
	if err != nil {
		t.Fatalf("post-layout Read: %v", err)
	}
	report = findReport(t, reports, "snap_latency", nil)
	if report.State != delta.StateIncrease || report.Histogram.CountDelta != 6 ||
		*report.Histogram.SumDelta != 16 {
		t.Fatalf("unexpected post-layout report: %+v", report.Histogram)
	}
	assertBucket(t, report.Histogram.Buckets, 0, 0.5, 4, 2)
	assertBucket(t, report.Histogram.Buckets, 1, 1, 10, 2)
	assertBucket(t, report.Histogram.Buckets, 2, 2, 16, 1)
	assertBucket(t, report.Histogram.Buckets, 3, 4, 19, 1)
}

// TestReaderHistogramReset verifies that a classic histogram whose
// cumulative counts decreased is reported as a reset with non-negative
// post-restart bucket increments.
func TestReaderHistogramReset(t *testing.T) {
	snap := &snapshotCollector{
		histPresent:    true,
		histCount:      50,
		histSum:        100,
		histBounds:     []float64{1, 2},
		histCumulative: []uint64{20, 40},
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(snap)
	reader := delta.NewReader(registry)

	if _, err := reader.Read(); err != nil {
		t.Fatalf("initial Read: %v", err)
	}

	// Restart: cumulative counts go back down.
	snap.mu.Lock()
	snap.histCount = 5
	snap.histSum = 6
	snap.histCumulative = []uint64{2, 4}
	snap.mu.Unlock()

	reports, err := reader.Read()
	if err != nil {
		t.Fatalf("reset Read: %v", err)
	}
	report := findReport(t, reports, "snap_latency", nil)
	if report.State != delta.StateCounterReset {
		t.Fatalf("expected counter_reset, got %s", report.State)
	}
	if report.Histogram.CountDelta != 5 {
		t.Fatalf("reset count delta must be post-restart count, got %v", report.Histogram.CountDelta)
	}
	if *report.Histogram.SumDelta != 6 {
		t.Fatalf("reset sum delta must be post-restart sum, got %v", *report.Histogram.SumDelta)
	}
	assertBucket(t, report.Histogram.Buckets, 0, 1, 2, 2)
	assertBucket(t, report.Histogram.Buckets, 1, 2, 4, 2)
}

// TestReaderDoesNotMutateMetrics confirms the reader never calls Reset and
// that repeated gathers keep exposing the same cumulative totals.
func TestReaderDoesNotMutateMetrics(t *testing.T) {
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "live_requests"})
	registry := prometheus.NewRegistry()
	registry.MustRegister(counter)
	reader := delta.NewReader(registry)

	counter.Add(10)
	if _, err := reader.Read(); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	counter.Add(3)
	if _, err := reader.Read(); err != nil {
		t.Fatalf("second Read: %v", err)
	}
	counter.Add(2)
	if _, err := reader.Read(); err != nil {
		t.Fatalf("third Read: %v", err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("raw Gather: %v", err)
	}
	if got := counterValueFromGather(t, families, "live_requests", nil); got != 15 {
		t.Fatalf("counter must remain cumulative at 15, got %v", got)
	}
}

// TestReaderConcurrentReads exercises concurrent Readers sharing one
// registry and concurrent calls on a single Reader.
func TestReaderConcurrentReads(t *testing.T) {
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "conc_counter"})
	registry := prometheus.NewRegistry()
	registry.MustRegister(counter)

	readers := make([]*delta.Reader, 4)
	for i := range readers {
		readers[i] = delta.NewReader(registry)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				counter.Inc()
			}
		}
	}()

	for _, reader := range readers {
		reader := reader
		wg.Add(2)
		go func() {
			defer wg.Done()
			deadline := time.After(100 * time.Millisecond)
			for {
				select {
				case <-stop:
					return
				case <-deadline:
					return
				default:
					reports, err := reader.Read()
					if err != nil {
						t.Errorf("concurrent Read: %v", err)
						return
					}
					for _, report := range reports {
						if report.Counter != nil && report.Counter.Delta < 0 {
							t.Errorf("negative counter delta: %v", report.Counter.Delta)
							return
						}
					}
				}
			}
		}()
		go func() {
			defer wg.Done()
			deadline := time.After(80 * time.Millisecond)
			for {
				select {
				case <-stop:
					return
				case <-deadline:
					return
				default:
					if _, err := reader.Read(); err != nil {
						t.Errorf("concurrent Read: %v", err)
						return
					}
				}
			}
		}()
	}

	time.Sleep(120 * time.Millisecond)
	close(stop)
	wg.Wait()
}
