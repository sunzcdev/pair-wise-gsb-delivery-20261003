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
	"testing"
	"time"

	"github.com/prometheus/client_golang/exp/delta"
	dto "github.com/prometheus/client_model/go"
)

var errGather = errors.New("gather boom")

func readOK(t *testing.T, r *delta.Reader) *delta.Report {
	t.Helper()
	report, err := r.Read()
	if err != nil {
		t.Fatalf("unexpected gather error: %v", err)
	}
	return report
}

func expectGatherError(t *testing.T, r *delta.Reader) {
	t.Helper()
	if _, err := r.Read(); !errors.Is(err, errGather) {
		t.Fatalf("expected gather error %v, got %v", errGather, err)
	}
}

// mutableClock is a test clock advanced explicitly.
type mutableClock struct{ t time.Time }

func TestCounterBaselineIncrementsAndReset(t *testing.T) {
	const name = "requests_total"
	g := &scriptedGatherer{steps: []gatherStep{
		{families: []*dto.MetricFamily{counterFamily(name, map[string]float64{"a": 10})}},
		{families: []*dto.MetricFamily{counterFamily(name, map[string]float64{"a": 13})}},
		{err: errGather},
		// The failed gather must not advance the baseline, so the
		// increment is still measured against 13.
		{families: []*dto.MetricFamily{counterFamily(name, map[string]float64{"a": 20})}},
		// Counter reset, for example after a process restart.
		{families: []*dto.MetricFamily{counterFamily(name, map[string]float64{"a": 2})}},
		// Recovery after the reset uses the new baseline.
		{families: []*dto.MetricFamily{counterFamily(name, map[string]float64{"a": 7})}},
	}}
	r := delta.NewReader(g)

	first := readOK(t, r)
	if got := findCounter(first, name, "a"); got.Status != delta.StatusNew || got.Delta != 0 || got.Value != 10 {
		t.Fatalf("first read = %+v, want status new with zero delta at value 10", got)
	}

	second := readOK(t, r)
	if got := findCounter(second, name, "a"); got.Status != delta.StatusOK || got.Delta != 3 || got.Value != 13 {
		t.Fatalf("second read = %+v, want delta 3 at value 13", got)
	}

	expectGatherError(t, r)

	afterFailure := readOK(t, r)
	if got := findCounter(afterFailure, name, "a"); got.Status != delta.StatusOK || got.Delta != 7 || got.Value != 20 {
		t.Fatalf("post-failure read = %+v, want delta 7 (20-13) at value 20", got)
	}

	reset := readOK(t, r)
	if got := findCounter(reset, name, "a"); got.Status != delta.StatusReset || got.Delta != 0 || got.Value != 2 {
		t.Fatalf("reset read = %+v, want explicit reset with zero delta at value 2", got)
	}

	recovered := readOK(t, r)
	if got := findCounter(recovered, name, "a"); got.Status != delta.StatusOK || got.Delta != 5 || got.Value != 7 {
		t.Fatalf("post-reset read = %+v, want delta 5 (7-2) at value 7", got)
	}
}

func TestCounterDisappearReappearAndExpiry(t *testing.T) {
	const name = "jobs_total"
	clock := &mutableClock{t: time.Unix(1000, 0)}
	g := &scriptedGatherer{steps: []gatherStep{
		{families: []*dto.MetricFamily{counterFamily(name, map[string]float64{"a": 5, "b": 9})}},
		// Series a disappears.
		{families: []*dto.MetricFamily{counterFamily(name, map[string]float64{"b": 11})}},
		// a reappears within the staleness window.
		{families: []*dto.MetricFamily{counterFamily(name, map[string]float64{"a": 1, "b": 14})}},
		// b disappears.
		{families: []*dto.MetricFamily{counterFamily(name, map[string]float64{"a": 4})}},
		// One more gather without b, used to prune its expired baseline.
		{families: []*dto.MetricFamily{counterFamily(name, map[string]float64{"a": 5})}},
	}}
	r := delta.NewReader(g,
		delta.WithStaleness(2*time.Minute),
		delta.WithClock(func() time.Time { return clock.t }),
	)

	readOK(t, r)

	gone := readOK(t, r)
	if got := findCounter(gone, name, "a"); got.Status != delta.StatusDisappeared {
		t.Fatalf("expected a disappeared, got %+v", got)
	}
	if got := findCounter(gone, name, "b"); got.Status != delta.StatusOK || got.Delta != 2 {
		t.Fatalf("expected b delta 2, got %+v", got)
	}

	clock.t = clock.t.Add(30 * time.Second)
	back := readOK(t, r)
	if got := findCounter(back, name, "a"); got.Status != delta.StatusReappeared || got.Delta != 0 || got.Value != 1 {
		t.Fatalf("expected a reappeared with zero delta at value 1, got %+v", got)
	}
	if got := findCounter(back, name, "b"); got.Status != delta.StatusOK || got.Delta != 3 {
		t.Fatalf("expected b delta 3, got %+v", got)
	}

	clock.t = clock.t.Add(5 * time.Minute)
	expired := readOK(t, r)
	if got := findCounter(expired, name, "b"); got.Status != delta.StatusDisappeared {
		t.Fatalf("expected b disappeared before expiry, got %+v", got)
	}
	if got := findCounter(expired, name, "a"); got.Status != delta.StatusOK || got.Delta != 3 {
		t.Fatalf("expected a delta 3, got %+v", got)
	}

	// Advance beyond the staleness window measured from b's first
	// absence (t+1m) and gather once without b so its baseline is pruned.
	clock.t = clock.t.Add(2 * time.Minute)
	pruned := readOK(t, r)
	if got := findCounter(pruned, name, "b"); got.Status != delta.StatusDisappeared {
		t.Fatalf("expected b to remain reported as disappeared until pruned, got %+v", got)
	}
	if got := findCounter(pruned, name, "a"); got.Status != delta.StatusOK || got.Delta != 1 {
		t.Fatalf("expected a delta 1 while b is pruned, got %+v", got)
	}

	// b is past the staleness window now and surfaces as StatusNew
	// instead of StatusReappeared.
	g.steps = append(g.steps, gatherStep{
		families: []*dto.MetricFamily{counterFamily(name, map[string]float64{"a": 6, "b": 100})},
	})
	fresh := readOK(t, r)
	if got := findCounter(fresh, name, "b"); got.Status != delta.StatusNew || got.Value != 100 {
		t.Fatalf("expected expired b to be reported as new at value 100, got %+v", got)
	}
	if got := findCounter(fresh, name, "a"); got.Status != delta.StatusOK || got.Delta != 1 {
		t.Fatalf("expected a delta 1 after pruning, got %+v", got)
	}
}

func TestHistogramIncrementsBucketsResetAndLayoutChange(t *testing.T) {
	const name = "latency_seconds"
	base := histogramSpec{
		bounds: []float64{0.1, 1},
		values: []uint64{2, 5},
		count:  5,
		sum:    3.5,
	}
	grown := histogramSpec{
		bounds: []float64{0.1, 1},
		values: []uint64{4, 9},
		count:  9,
		sum:    8.0,
	}
	// Same bounds but cumulative bucket counts decreased: an explicit
	// reset instead of a negative subtraction.
	shrunk := histogramSpec{
		bounds: []float64{0.1, 1},
		values: []uint64{1, 2},
		count:  2,
		sum:    0.4,
	}
	recovered := histogramSpec{
		bounds: []float64{0.1, 1},
		values: []uint64{2, 4},
		count:  4,
		sum:    1.2,
	}
	// Bucket layout changed: a 0.5 boundary was added and the old
	// boundaries moved.
	relayout := histogramSpec{
		bounds: []float64{0.1, 0.5, 1},
		values: []uint64{2, 3, 4},
		count:  4,
		sum:    1.3,
	}
	relayoutGrown := histogramSpec{
		bounds: []float64{0.1, 0.5, 1},
		values: []uint64{3, 5, 7},
		count:  7,
		sum:    2.0,
	}
	g := &scriptedGatherer{steps: []gatherStep{
		{families: []*dto.MetricFamily{histogramFamily(name, map[string]histogramSpec{"h": base})}},
		{families: []*dto.MetricFamily{histogramFamily(name, map[string]histogramSpec{"h": grown})}},
		{err: errGather},
		// The failed gather does not advance the baseline.
		{families: []*dto.MetricFamily{histogramFamily(name, map[string]histogramSpec{"h": shrunk})}},
		{families: []*dto.MetricFamily{histogramFamily(name, map[string]histogramSpec{"h": recovered})}},
		{families: []*dto.MetricFamily{histogramFamily(name, map[string]histogramSpec{"h": relayout})}},
		{families: []*dto.MetricFamily{histogramFamily(name, map[string]histogramSpec{"h": relayoutGrown})}},
	}}
	r := delta.NewReader(g)

	first := readOK(t, r)
	if got := findHistogram(first, name, "h"); got.Status != delta.StatusNew {
		t.Fatalf("first read status = %s, want new", got.Status)
	}

	second := readOK(t, r)
	got := findHistogram(second, name, "h")
	if got.Status != delta.StatusOK || got.CountDelta != 4 || got.Count != 9 || got.SumDelta != 4.5 || got.Sum != 8 {
		t.Fatalf("grown histogram = %+v, want ok count 4/9 sum 4.5/8", got)
	}
	wantBuckets := []delta.BucketDelta{
		{UpperBound: 0.1, Delta: 2, Cumulative: 4},
		{UpperBound: 1, Delta: 4, Cumulative: 9},
		{UpperBound: math.Inf(1), Delta: 4, Cumulative: 9},
	}
	if diff := compareBuckets(wantBuckets, got.Buckets); diff != "" {
		t.Fatalf("grown buckets mismatch:\n%s", diff)
	}

	expectGatherError(t, r)

	reset := readOK(t, r)
	got = findHistogram(reset, name, "h")
	if got.Status != delta.StatusReset {
		t.Fatalf("expected reset status, got %s", got.Status)
	}
	for _, bucket := range got.Buckets {
		if bucket.Delta != 0 {
			t.Fatalf("reset bucket %v delta = %v, want 0", bucket.UpperBound, bucket.Delta)
		}
	}
	if got.CountDelta != 0 || got.SumDelta != 0 {
		t.Fatalf("reset count/sum delta = %v/%v, want 0", got.CountDelta, got.SumDelta)
	}

	rec := readOK(t, r)
	got = findHistogram(rec, name, "h")
	if got.Status != delta.StatusOK || got.CountDelta != 2 || !almostEqual(got.SumDelta, 0.8) {
		t.Fatalf("post-reset histogram = %+v, want ok with count delta 2 and sum delta 0.8", got)
	}

	changed := readOK(t, r)
	got = findHistogram(changed, name, "h")
	if got.Status != delta.StatusBucketLayoutChanged {
		t.Fatalf("expected bucket layout changed, got %s", got.Status)
	}
	if len(got.Buckets) != 4 {
		t.Fatalf("expected 4 buckets including +Inf, got %d", len(got.Buckets))
	}
	for _, bucket := range got.Buckets {
		if bucket.Delta != 0 {
			t.Fatalf("layout-change bucket %v delta = %v, want 0", bucket.UpperBound, bucket.Delta)
		}
	}
	if got.CountDelta != 0 || got.SumDelta != 0 {
		t.Fatalf("layout-change count/sum delta = %v/%v, want 0", got.CountDelta, got.SumDelta)
	}

	afterLayout := readOK(t, r)
	got = findHistogram(afterLayout, name, "h")
	if got.Status != delta.StatusOK || got.CountDelta != 3 || got.SumDelta != 0.7 {
		t.Fatalf("post-layout histogram = %+v, want ok with count delta 3 and sum delta 0.7", got)
	}
	wantBuckets = []delta.BucketDelta{
		{UpperBound: 0.1, Delta: 1, Cumulative: 3},
		{UpperBound: 0.5, Delta: 2, Cumulative: 5},
		{UpperBound: 1, Delta: 3, Cumulative: 7},
		{UpperBound: math.Inf(1), Delta: 3, Cumulative: 7},
	}
	if diff := compareBuckets(wantBuckets, got.Buckets); diff != "" {
		t.Fatalf("post-layout buckets mismatch:\n%s", diff)
	}
}

func TestHistogramDisappearReappear(t *testing.T) {
	const name = "sizes_bytes"
	spec := histogramSpec{
		bounds: []float64{10, 100},
		values: []uint64{1, 3},
		count:  3,
		sum:    120,
	}
	g := &scriptedGatherer{steps: []gatherStep{
		{families: []*dto.MetricFamily{histogramFamily(name, map[string]histogramSpec{"h": spec})}},
		{families: []*dto.MetricFamily{}},
		{families: []*dto.MetricFamily{histogramFamily(name, map[string]histogramSpec{"h": spec})}},
	}}
	r := delta.NewReader(g)

	readOK(t, r)
	gone := readOK(t, r)
	if got := findHistogram(gone, name, "h"); got.Status != delta.StatusDisappeared {
		t.Fatalf("expected disappeared, got %s", got.Status)
	}
	back := readOK(t, r)
	got := findHistogram(back, name, "h")
	if got.Status != delta.StatusReappeared {
		t.Fatalf("expected reappeared, got %s", got.Status)
	}
	for _, bucket := range got.Buckets {
		if bucket.Delta != 0 {
			t.Fatalf("reappeared bucket %v delta = %v, want 0", bucket.UpperBound, bucket.Delta)
		}
	}
}

func compareBuckets(want, got []delta.BucketDelta) string {
	if len(want) != len(got) {
		return "length differs"
	}
	for i := range want {
		if want[i].UpperBound != got[i].UpperBound ||
			want[i].Delta != got[i].Delta ||
			want[i].Cumulative != got[i].Cumulative {
			return "bucket mismatch"
		}
	}
	return ""
}

func almostEqual(a, b float64) bool {
	diff := math.Abs(a - b)
	return diff < 1e-9 || diff < 1e-9*math.Max(math.Abs(a), math.Abs(b))
}
