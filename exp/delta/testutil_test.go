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
	"fmt"

	"github.com/prometheus/client_golang/exp/delta"
	dto "github.com/prometheus/client_model/go"
)

// scriptedGatherer returns the families of the current step. Each step
// either provides families or an error, which lets tests drive the reader
// through counter resets, disappearing series and gather failures without
// touching a real registry.
type scriptedGatherer struct {
	steps []gatherStep
	calls int
}

type gatherStep struct {
	families []*dto.MetricFamily
	err      error
}

func (g *scriptedGatherer) Gather() ([]*dto.MetricFamily, error) {
	if g.calls >= len(g.steps) {
		return nil, fmt.Errorf("unexpected gather call %d", g.calls)
	}
	step := g.steps[g.calls]
	g.calls++
	return step.families, step.err
}

func ptr[T any](v T) *T { return &v }

func labelPairs(pairs ...string) []*dto.LabelPair {
	if len(pairs)%2 != 0 {
		panic("labels must be name/value pairs")
	}
	out := make([]*dto.LabelPair, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, &dto.LabelPair{Name: ptr(pairs[i]), Value: ptr(pairs[i+1])})
	}
	return out
}

func counterFamily(name string, series map[string]float64) *dto.MetricFamily {
	metrics := make([]*dto.Metric, 0, len(series))
	for key, value := range series {
		metrics = append(metrics, &dto.Metric{
			Label:   labelPairs("series", key),
			Counter: &dto.Counter{Value: ptr(value)},
		})
	}
	return &dto.MetricFamily{
		Name:   ptr(name),
		Type:   ptr(dto.MetricType_COUNTER),
		Metric: metrics,
	}
}

// histogramSpec defines one classic histogram series with cumulative
// bucket counts at the given bounds.
type histogramSpec struct {
	bounds []float64
	values []uint64
	count  uint64
	sum    float64
}

func histogramFamily(name string, series map[string]histogramSpec) *dto.MetricFamily {
	metrics := make([]*dto.Metric, 0, len(series))
	for key, spec := range series {
		buckets := make([]*dto.Bucket, 0, len(spec.bounds))
		for i, bound := range spec.bounds {
			buckets = append(buckets, &dto.Bucket{
				UpperBound:      ptr(bound),
				CumulativeCount: ptr(spec.values[i]),
			})
		}
		metrics = append(metrics, &dto.Metric{
			Label: labelPairs("series", key),
			Histogram: &dto.Histogram{
				// Schema stays nil (0) so the family is treated as a
				// classic histogram.
				Bucket:      buckets,
				SampleCount: ptr(spec.count),
				SampleSum:   ptr(spec.sum),
			},
		})
	}
	return &dto.MetricFamily{
		Name:   ptr(name),
		Type:   ptr(dto.MetricType_HISTOGRAM),
		Metric: metrics,
	}
}

func findCounter(report *delta.Report, family, series string) delta.CounterDelta {
	for _, entry := range report.Counters {
		if entry.Family == family && entry.Labels["series"] == series {
			return entry
		}
	}
	panic(fmt.Sprintf("counter %s{series=%q} not found in report", family, series))
}

func findHistogram(report *delta.Report, family, series string) delta.HistogramDelta {
	for _, entry := range report.Histograms {
		if entry.Family == family && entry.Labels["series"] == series {
			return entry
		}
	}
	panic(fmt.Sprintf("histogram %s{series=%q} not found in report", family, series))
}
