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

package exp_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/prometheus/client_golang/exp"
)

type fakeGatherer struct {
	mfs []*dto.MetricFamily
}

func (g fakeGatherer) Gather() ([]*dto.MetricFamily, error) {
	return g.mfs, nil
}

func TestNewConvertingGathererValidation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gatherer    exp.Gatherer
		conversions []exp.Conversion
	}{
		{
			name:     "nil gatherer",
			gatherer: nil,
		},
		{
			name:        "empty source name",
			gatherer:    fakeGatherer{},
			conversions: []exp.Conversion{{From: "", To: "b", Factor: 1}},
		},
		{
			name:        "empty target name",
			gatherer:    fakeGatherer{},
			conversions: []exp.Conversion{{From: "a", To: "", Factor: 1}},
		},
		{
			name:     "duplicate source name",
			gatherer: fakeGatherer{},
			conversions: []exp.Conversion{
				{From: "a", To: "b", Factor: 1},
				{From: "a", To: "c", Factor: 1},
			},
		},
		{
			name:     "duplicate target name",
			gatherer: fakeGatherer{},
			conversions: []exp.Conversion{
				{From: "a", To: "c", Factor: 1},
				{From: "b", To: "c", Factor: 1},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exp.NewConvertingGatherer(tc.gatherer, tc.conversions); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func testMetricFamilies() []*dto.MetricFamily {
	return []*dto.MetricFamily{
		{
			Name: proto.String("queue_milliseconds"),
			Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{
				{
					Label: []*dto.LabelPair{{Name: proto.String("queue"), Value: proto.String("a")}},
					Gauge: &dto.Gauge{Value: proto.Float64(1500)},
				},
				{
					Label: []*dto.LabelPair{{Name: proto.String("queue"), Value: proto.String("b")}},
					Gauge: &dto.Gauge{Value: proto.Float64(-250)},
				},
			},
		},
		{
			Name: proto.String("requests_milliseconds_total"),
			Type: dto.MetricType_COUNTER.Enum(),
			Metric: []*dto.Metric{
				{
					Counter: &dto.Counter{
						Value: proto.Float64(2000),
						Exemplar: &dto.Exemplar{
							Label: []*dto.LabelPair{{Name: proto.String("traceID"), Value: proto.String("abc")}},
							Value: proto.Float64(120),
						},
					},
				},
			},
		},
		{
			Name: proto.String("latency_milliseconds"),
			Type: dto.MetricType_HISTOGRAM.Enum(),
			Metric: []*dto.Metric{
				{
					Histogram: &dto.Histogram{
						SampleCount: proto.Uint64(3),
						SampleSum:   proto.Float64(700),
						Bucket: []*dto.Bucket{
							{
								CumulativeCount: proto.Uint64(1),
								UpperBound:      proto.Float64(100),
								Exemplar:        &dto.Exemplar{Value: proto.Float64(-40)},
							},
							{
								CumulativeCount: proto.Uint64(2),
								UpperBound:      proto.Float64(500),
							},
							{
								CumulativeCount: proto.Uint64(3),
								UpperBound:      proto.Float64(1e9),
							},
						},
					},
				},
			},
		},
		{
			Name: proto.String("workers"),
			Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{
				{Gauge: &dto.Gauge{Value: proto.Float64(7)}},
			},
		},
	}
}

func TestConvertingGatherer(t *testing.T) {
	conversions := []exp.Conversion{
		{From: "queue_milliseconds", To: "queue_seconds", Factor: 0.001},
		{From: "requests_milliseconds_total", To: "requests_seconds_total", Factor: 0.001},
		{From: "latency_milliseconds", To: "latency_seconds", Factor: 0.001},
	}

	mfs := testMetricFamilies()
	// Snapshot the gathered data to verify the wrapped gatherer's families
	// are not modified by the conversion.
	snapshot := make([]*dto.MetricFamily, 0, len(mfs))
	for _, mf := range mfs {
		snapshot = append(snapshot, proto.Clone(mf).(*dto.MetricFamily))
	}

	g, err := exp.NewConvertingGatherer(fakeGatherer{mfs: mfs}, conversions)
	if err != nil {
		t.Fatalf("unexpected error creating the converting gatherer: %v", err)
	}
	got, err := g.Gather()
	if err != nil {
		t.Fatalf("unexpected error gathering: %v", err)
	}

	want := []*dto.MetricFamily{
		{
			Name: proto.String("queue_seconds"),
			Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{
				{
					Label: []*dto.LabelPair{{Name: proto.String("queue"), Value: proto.String("a")}},
					Gauge: &dto.Gauge{Value: proto.Float64(1.5)},
				},
				{
					Label: []*dto.LabelPair{{Name: proto.String("queue"), Value: proto.String("b")}},
					Gauge: &dto.Gauge{Value: proto.Float64(-0.25)},
				},
			},
		},
		{
			Name: proto.String("requests_seconds_total"),
			Type: dto.MetricType_COUNTER.Enum(),
			Metric: []*dto.Metric{
				{
					Counter: &dto.Counter{
						Value: proto.Float64(2),
						Exemplar: &dto.Exemplar{
							Label: []*dto.LabelPair{{Name: proto.String("traceID"), Value: proto.String("abc")}},
							Value: proto.Float64(0.12),
						},
					},
				},
			},
		},
		{
			Name: proto.String("latency_seconds"),
			Type: dto.MetricType_HISTOGRAM.Enum(),
			Metric: []*dto.Metric{
				{
					Histogram: &dto.Histogram{
						SampleCount: proto.Uint64(3),
						SampleSum:   proto.Float64(0.7000000000000001),
						Bucket: []*dto.Bucket{
							{
								CumulativeCount: proto.Uint64(1),
								UpperBound:      proto.Float64(0.1),
								Exemplar:        &dto.Exemplar{Value: proto.Float64(-0.04)},
							},
							{
								CumulativeCount: proto.Uint64(2),
								UpperBound:      proto.Float64(0.5),
							},
							{
								CumulativeCount: proto.Uint64(3),
								UpperBound:      proto.Float64(1e6),
							},
						},
					},
				},
			},
		},
		{
			Name: proto.String("workers"),
			Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{
				{Gauge: &dto.Gauge{Value: proto.Float64(7)}},
			},
		},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Fatalf("unexpected converted metric families (-want +got):\n%s", diff)
	}

	// Untouched families must be passed through as is, not copied.
	if got[3] != mfs[3] {
		t.Fatal("expected unconverted metric family to be passed through by reference")
	}

	// The wrapped gatherer's families must not be modified.
	if diff := cmp.Diff(snapshot, mfs, protocmp.Transform()); diff != "" {
		t.Fatalf("wrapped gatherer's metric families were modified (-want +got):\n%s", diff)
	}
}

func TestConvertingGathererNameCollision(t *testing.T) {
	mfs := []*dto.MetricFamily{
		{
			Name:   proto.String("duration_milliseconds"),
			Type:   dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{{Gauge: &dto.Gauge{Value: proto.Float64(1)}}},
		},
		{
			Name:   proto.String("duration_seconds"),
			Type:   dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{{Gauge: &dto.Gauge{Value: proto.Float64(2)}}},
		},
	}
	g, err := exp.NewConvertingGatherer(fakeGatherer{mfs: mfs}, []exp.Conversion{
		{From: "duration_milliseconds", To: "duration_seconds", Factor: 0.001},
	})
	if err != nil {
		t.Fatalf("unexpected error creating the converting gatherer: %v", err)
	}
	if _, err := g.Gather(); err == nil {
		t.Fatal("expected a name collision error, got nil")
	}
}

func TestConvertingGathererUnsupportedType(t *testing.T) {
	mfs := []*dto.MetricFamily{
		{
			Name:   proto.String("latency_milliseconds"),
			Type:   dto.MetricType_SUMMARY.Enum(),
			Metric: []*dto.Metric{{Summary: &dto.Summary{SampleCount: proto.Uint64(1), SampleSum: proto.Float64(1)}}},
		},
	}
	g, err := exp.NewConvertingGatherer(fakeGatherer{mfs: mfs}, []exp.Conversion{
		{From: "latency_milliseconds", To: "latency_seconds", Factor: 0.001},
	})
	if err != nil {
		t.Fatalf("unexpected error creating the converting gatherer: %v", err)
	}
	if _, err := g.Gather(); err == nil {
		t.Fatal("expected an unsupported type error, got nil")
	}
}
