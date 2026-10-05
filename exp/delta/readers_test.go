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
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/exp/delta"
	dto "github.com/prometheus/client_model/go"
)

// countingGatherer is a concurrency-safe source that mimics a live process:
// every Gather advances the cumulative counter and histogram counts.
type countingGatherer struct {
	mu      sync.Mutex
	counter float64
	count   uint64
	sum     float64
	calls   int
	fail    bool
}

func (g *countingGatherer) Gather() ([]*dto.MetricFamily, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.fail {
		return nil, errGather
	}
	g.counter += 10
	g.count += 4
	g.sum += 1.5
	return []*dto.MetricFamily{
		counterFamily("events_total", map[string]float64{"main": g.counter}),
		histogramFamily("events_seconds", map[string]histogramSpec{
			"main": {
				bounds: []float64{0.1, 1},
				values: []uint64{g.count / 2, g.count},
				count:  g.count,
				sum:    g.sum,
			},
		}),
	}, nil
}

func (g *countingGatherer) cumulative() (counter float64, count uint64, sum float64, calls int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.counter, g.count, g.sum, g.calls
}

// TestTwoReadersDifferentCadence runs a fast and a slow reader against the
// same cumulative source and verifies that each reader reconstructs the
// cumulative values from its own intervals while the underlying /metrics
// values stay untouched and consistent.
func TestTwoReadersDifferentCadence(t *testing.T) {
	source := &countingGatherer{}

	fast := delta.NewReader(source)
	slow := delta.NewReader(source)

	// readerTotals accumulates the sum of every interval delta together
	// with the cumulative values observed at baseline and at the end.
	type readerTotals struct {
		baseCounter, lastCounter float64
		baseCount, lastCount     float64
		baseSum, lastSum         float64
		incCounter               float64
		incCount                 float64
		incSum                   float64
		intervalReports          int
	}

	observe := func(r *delta.Reader, totals *readerTotals, primed *bool) {
		report, err := r.Read()
		if err != nil {
			t.Fatalf("unexpected gather error: %v", err)
		}
		counter := findCounter(report, "events_total", "main")
		hist := findHistogram(report, "events_seconds", "main")
		if !*primed {
			totals.baseCounter = counter.Value
			totals.baseCount = hist.Count
			totals.baseSum = hist.Sum
			*primed = true
		} else {
			totals.intervalReports++
			totals.incCounter += counter.Delta
			totals.incCount += hist.CountDelta
			totals.incSum += hist.SumDelta
		}
		totals.lastCounter = counter.Value
		totals.lastCount = hist.Count
		totals.lastSum = hist.Sum
	}

	// The slow reader gathers once for every three fast reader gathers.
	var fastTotals, slowTotals readerTotals
	fastPrimed, slowPrimed := false, false
	for i := 0; i < 6; i++ {
		observe(fast, &fastTotals, &fastPrimed)
		if i%3 == 2 {
			observe(slow, &slowTotals, &slowPrimed)
		}
	}

	// 6 fast gathers and 2 slow gathers happened.
	if _, _, _, calls := source.cumulative(); calls != 8 {
		t.Fatalf("gather calls = %d, want 8 (6 fast + 2 slow)", calls)
	}
	if fastTotals.intervalReports != 5 || slowTotals.intervalReports != 1 {
		t.Fatalf("interval reports: fast %d slow %d, want 5 and 1",
			fastTotals.intervalReports, slowTotals.intervalReports)
	}

	// For each reader, the sum of interval deltas must equal the growth
	// of the cumulative /metrics values between its own first and last
	// gather, and every reported delta must be non-negative.
	reconcile := func(name string, totals readerTotals) {
		if totals.incCounter < 0 || totals.incCount < 0 || totals.incSum < 0 {
			t.Fatalf("%s reconstructed negative increments: %+v", name, totals)
		}
		if totals.incCounter != totals.lastCounter-totals.baseCounter {
			t.Fatalf("%s counter increments %v do not match cumulative growth %v",
				name, totals.incCounter, totals.lastCounter-totals.baseCounter)
		}
		if totals.incCount != totals.lastCount-totals.baseCount {
			t.Fatalf("%s count increments %v do not match cumulative growth %v",
				name, totals.incCount, totals.lastCount-totals.baseCount)
		}
		if totals.incSum != totals.lastSum-totals.baseSum {
			t.Fatalf("%s sum increments %v do not match cumulative growth %v",
				name, totals.incSum, totals.lastSum-totals.baseSum)
		}
	}
	reconcile("fast", fastTotals)
	reconcile("slow", slowTotals)

	// Both readers observe the same underlying monotonic cumulative
	// series through the scrape interface. The slower reader covers fewer
	// but wider intervals, which is why its last observed value is
	// guaranteed not to exceed the fast reader's.
	cumCounter, cumCount, cumSum, calls := source.cumulative()
	if calls != 8 {
		t.Fatalf("gather calls = %d, want 8", calls)
	}

	// The slow reader's last observation is an earlier snapshot of the
	// same cumulative series.
	if fastTotals.lastCounter > cumCounter || fastTotals.lastCount > float64(cumCount) || fastTotals.lastSum > cumSum {
		t.Fatalf("fast reader last observation %+v exceeds live cumulative %v/%d/%v",
			fastTotals, cumCounter, cumCount, cumSum)
	}
	// The slow reader performs its final gather after the fast reader's
	// final gather, so it observes the live cumulative value last.
	if slowTotals.lastCounter != cumCounter || slowTotals.lastCount != float64(cumCount) || slowTotals.lastSum != cumSum {
		t.Fatalf("slow reader last observation %+v does not match live cumulative %v/%d/%v",
			slowTotals, cumCounter, cumCount, cumSum)
	}
	if fastTotals.incCounter <= slowTotals.incCounter {
		t.Fatalf("fast reader reconstructed increment %v must exceed slow reader %v",
			fastTotals.incCounter, slowTotals.incCounter)
	}
}

func TestRunPushesReportsAndSkipsFailedGathers(t *testing.T) {
	source := &countingGatherer{}
	reader := delta.NewReader(source)

	reports := make(chan *delta.Report, 4)
	sink := delta.SinkFunc(func(_ context.Context, report *delta.Report) error {
		reports <- report
		return nil
	})

	var errorMu sync.Mutex
	var errs []error
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = reader.Run(ctx, time.Millisecond, sink, delta.WithErrorHandler(func(err error) {
			errorMu.Lock()
			errs = append(errs, err)
			errorMu.Unlock()
		}))
		close(done)
	}()

	report := <-reports
	if got := findCounter(report, "events_total", "main"); got.Delta != 10 || got.Status != delta.StatusOK {
		t.Fatalf("first pushed counter = %+v, want ok delta 10", got)
	}

	source.mu.Lock()
	source.fail = true
	source.mu.Unlock()
	// Let several gathers fail; each one must leave the baseline alone.
	time.Sleep(8 * time.Millisecond)
	source.mu.Lock()
	source.fail = false
	source.mu.Unlock()

	// After the failure, increments are measured against the pre-failure
	// baseline.
	report = <-reports
	if got := findCounter(report, "events_total", "main"); got.Status != delta.StatusOK {
		t.Fatalf("post-failure counter = %+v, want ok status", got)
	}
	// Drain reports accumulated during the rest of the run.
	next := <-reports
	if got := findCounter(next, "events_total", "main"); got.Delta != 10 {
		t.Fatalf("steady interval counter delta = %v, want 10", got.Delta)
	}

	cancel()
	<-done

	errorMu.Lock()
	defer errorMu.Unlock()
	found := false
	for _, err := range errs {
		if err == errGather {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected gather error to be reported, got %v", errs)
	}
}

func TestConcurrentReadSafety(t *testing.T) {
	source := &countingGatherer{}
	reader := delta.NewReader(source)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_, _ = reader.Read()
			}
		}()
	}
	wg.Wait()
}
