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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/prometheus/client_golang/exp"
)

var testTime = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

type fakeGatherer struct {
	mfs []*dto.MetricFamily
	err error
}

func (g *fakeGatherer) Gather() ([]*dto.MetricFamily, error) {
	return g.mfs, g.err
}

// testMetricFamilies returns a fresh set of metric families on every call, so
// that tests can detect mutations of the source data. It covers gauges,
// counters, and histograms with positive and negative samples as well as
// exemplars.
func testMetricFamilies() []*dto.MetricFamily {
	return []*dto.MetricFamily{
		{
			Name: proto.String("request_duration_milliseconds"),
			Help: proto.String("Request duration."),
			Type: dto.MetricType_HISTOGRAM.Enum(),
			Metric: []*dto.Metric{
				{
					Label: []*dto.LabelPair{{Name: proto.String("code"), Value: proto.String("200")}},
					Histogram: &dto.Histogram{
						SampleCount: proto.Uint64(3),
						SampleSum:   proto.Float64(1750.5),
						Bucket: []*dto.Bucket{
							{CumulativeCount: proto.Uint64(1), UpperBound: proto.Float64(-100)},
							{
								CumulativeCount: proto.Uint64(2),
								UpperBound:      proto.Float64(250),
								Exemplar: &dto.Exemplar{
									Label:     []*dto.LabelPair{{Name: proto.String("trace_id"), Value: proto.String("abc")}},
									Value:     proto.Float64(200),
									Timestamp: timestamppb.New(testTime),
								},
							},
							{CumulativeCount: proto.Uint64(3), UpperBound: proto.Float64(1000)},
						},
					},
				},
			},
		},
		{
			Name: proto.String("processing_milliseconds_total"),
			Help: proto.String("Total processing time."),
			Type: dto.MetricType_COUNTER.Enum(),
			Metric: []*dto.Metric{
				{
					Counter: &dto.Counter{
						Value: proto.Float64(2500),
						Exemplar: &dto.Exemplar{
							Label:     []*dto.LabelPair{{Name: proto.String("trace_id"), Value: proto.String("def")}},
							Value:     proto.Float64(1500),
							Timestamp: timestamppb.New(testTime),
						},
					},
				},
			},
		},
		{
			Name: proto.String("queue_offset_milliseconds"),
			Help: proto.String("Queue offset, may be negative."),
			Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{
				{Gauge: &dto.Gauge{Value: proto.Float64(-42.5)}},
				{Gauge: &dto.Gauge{Value: proto.Float64(10)}},
			},
		},
		{
			Name: proto.String("temperature_celsius"),
			Help: proto.String("Temperature, not converted."),
			Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{
				{Gauge: &dto.Gauge{Value: proto.Float64(21.5)}},
			},
		},
	}
}

var testConversions = []exp.UnitConversion{
	{Name: "request_duration_milliseconds", NewName: "request_duration_seconds", Factor: 0.001},
	{Name: "processing_milliseconds_total", NewName: "processing_seconds_total", Factor: 0.001},
	// A conversion keeping the original name only rescales the values.
	{Name: "queue_offset_milliseconds", Factor: 0.001},
}

func TestConvertingGathererGather(t *testing.T) {
	src := &fakeGatherer{mfs: testMetricFamilies()}
	g, err := exp.NewConvertingGatherer(src, testConversions)
	if err != nil {
		t.Fatalf("NewConvertingGatherer: %v", err)
	}
	got, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	want := []*dto.MetricFamily{
		{
			Name: proto.String("request_duration_seconds"),
			Help: proto.String("Request duration."),
			Type: dto.MetricType_HISTOGRAM.Enum(),
			Metric: []*dto.Metric{
				{
					Label: []*dto.LabelPair{{Name: proto.String("code"), Value: proto.String("200")}},
					Histogram: &dto.Histogram{
						// The count must not be scaled, only the sum.
						SampleCount: proto.Uint64(3),
						SampleSum:   proto.Float64(1.7505),
						Bucket: []*dto.Bucket{
							{CumulativeCount: proto.Uint64(1), UpperBound: proto.Float64(-0.1)},
							{
								CumulativeCount: proto.Uint64(2),
								UpperBound:      proto.Float64(0.25),
								Exemplar: &dto.Exemplar{
									Label:     []*dto.LabelPair{{Name: proto.String("trace_id"), Value: proto.String("abc")}},
									Value:     proto.Float64(0.2),
									Timestamp: timestamppb.New(testTime),
								},
							},
							{CumulativeCount: proto.Uint64(3), UpperBound: proto.Float64(1)},
						},
					},
				},
			},
		},
		{
			Name: proto.String("processing_seconds_total"),
			Help: proto.String("Total processing time."),
			Type: dto.MetricType_COUNTER.Enum(),
			Metric: []*dto.Metric{
				{
					Counter: &dto.Counter{
						Value: proto.Float64(2.5),
						Exemplar: &dto.Exemplar{
							Label:     []*dto.LabelPair{{Name: proto.String("trace_id"), Value: proto.String("def")}},
							Value:     proto.Float64(1.5),
							Timestamp: timestamppb.New(testTime),
						},
					},
				},
			},
		},
		{
			Name: proto.String("queue_offset_milliseconds"),
			Help: proto.String("Queue offset, may be negative."),
			Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{
				{Gauge: &dto.Gauge{Value: proto.Float64(-0.0425)}},
				{Gauge: &dto.Gauge{Value: proto.Float64(0.01)}},
			},
		},
		{
			Name: proto.String("temperature_celsius"),
			Help: proto.String("Temperature, not converted."),
			Type: dto.MetricType_GAUGE.Enum(),
			Metric: []*dto.Metric{
				{Gauge: &dto.Gauge{Value: proto.Float64(21.5)}},
			},
		},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("Gathered metric families differ (-want +got):\n%s", diff)
	}
}

func TestConvertingGathererLeavesSourceUnchanged(t *testing.T) {
	mfs := testMetricFamilies()
	golden := make([]*dto.MetricFamily, 0, len(mfs))
	for _, mf := range mfs {
		golden = append(golden, proto.Clone(mf).(*dto.MetricFamily))
	}
	src := &fakeGatherer{mfs: mfs}
	g, err := exp.NewConvertingGatherer(src, testConversions)
	if err != nil {
		t.Fatalf("NewConvertingGatherer: %v", err)
	}
	got, err := g.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if diff := cmp.Diff(golden, mfs, protocmp.Transform()); diff != "" {
		t.Errorf("Source metric families were modified (-want +got):\n%s", diff)
	}
	// Unconverted families must be passed through as the same object.
	if got[len(got)-1] != mfs[len(mfs)-1] {
		t.Errorf("Unconverted family was not passed through as the same object.")
	}
	// Converted families must be new objects.
	for i, name := range []string{"request_duration_seconds", "processing_seconds_total", "queue_offset_milliseconds"} {
		if got[i] == mfs[i] {
			t.Errorf("Converted family %q aliases the source object.", name)
		}
	}
}

func TestConvertingGathererNameCollision(t *testing.T) {
	for _, tc := range []struct {
		name        string
		families    []string
		conversions []exp.UnitConversion
	}{
		{
			name:     "converted name collides with existing family",
			families: []string{"latency_milliseconds", "latency_seconds"},
			conversions: []exp.UnitConversion{
				{Name: "latency_milliseconds", NewName: "latency_seconds", Factor: 0.001},
			},
		},
		{
			name:     "two conversions produce the same name",
			families: []string{"latency_milliseconds", "latency_microseconds"},
			conversions: []exp.UnitConversion{
				{Name: "latency_milliseconds", NewName: "latency_seconds", Factor: 0.001},
				{Name: "latency_microseconds", NewName: "latency_seconds", Factor: 0.000001},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mfs []*dto.MetricFamily
			for _, n := range tc.families {
				mfs = append(mfs, &dto.MetricFamily{
					Name:   proto.String(n),
					Type:   dto.MetricType_GAUGE.Enum(),
					Metric: []*dto.Metric{{Gauge: &dto.Gauge{Value: proto.Float64(1)}}},
				})
			}
			g, err := exp.NewConvertingGatherer(&fakeGatherer{mfs: mfs}, tc.conversions)
			if err != nil {
				t.Fatalf("NewConvertingGatherer: %v", err)
			}
			if _, err := g.Gather(); err == nil || !strings.Contains(err.Error(), "collides") {
				t.Errorf("Expected a name collision error, got %v.", err)
			}
		})
	}
}

func TestConvertingGathererUnsupportedTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		mf   *dto.MetricFamily
	}{
		{
			name: "summary",
			mf: &dto.MetricFamily{
				Name:   proto.String("latency_milliseconds"),
				Type:   dto.MetricType_SUMMARY.Enum(),
				Metric: []*dto.Metric{{Summary: &dto.Summary{SampleCount: proto.Uint64(1), SampleSum: proto.Float64(1)}}},
			},
		},
		{
			name: "native histogram",
			mf: &dto.MetricFamily{
				Name: proto.String("latency_milliseconds"),
				Type: dto.MetricType_HISTOGRAM.Enum(),
				Metric: []*dto.Metric{{Histogram: &dto.Histogram{
					SampleCount: proto.Uint64(1),
					SampleSum:   proto.Float64(1),
					Schema:      proto.Int32(0),
				}}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := exp.NewConvertingGatherer(&fakeGatherer{mfs: []*dto.MetricFamily{tc.mf}}, []exp.UnitConversion{
				{Name: "latency_milliseconds", NewName: "latency_seconds", Factor: 0.001},
			})
			if err != nil {
				t.Fatalf("NewConvertingGatherer: %v", err)
			}
			if _, err := g.Gather(); err == nil {
				t.Error("Expected an error, got nil.")
			}
		})
	}
}

func TestConvertingGathererPropagatesGatherError(t *testing.T) {
	gatherErr := errors.New("gather failed")
	g, err := exp.NewConvertingGatherer(&fakeGatherer{err: gatherErr}, testConversions)
	if err != nil {
		t.Fatalf("NewConvertingGatherer: %v", err)
	}
	if _, err := g.Gather(); !errors.Is(err, gatherErr) {
		t.Errorf("Expected the wrapped gather error, got %v.", err)
	}
}

func TestNewConvertingGathererValidation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gatherer    exp.Gatherer
		conversions []exp.UnitConversion
	}{
		{
			name:     "nil gatherer",
			gatherer: nil,
		},
		{
			name:        "empty metric family name",
			gatherer:    &fakeGatherer{},
			conversions: []exp.UnitConversion{{Factor: 1}},
		},
		{
			name:        "zero factor",
			gatherer:    &fakeGatherer{},
			conversions: []exp.UnitConversion{{Name: "a", Factor: 0}},
		},
		{
			name:        "negative factor",
			gatherer:    &fakeGatherer{},
			conversions: []exp.UnitConversion{{Name: "a", Factor: -1}},
		},
		{
			name:        "duplicate conversion",
			gatherer:    &fakeGatherer{},
			conversions: []exp.UnitConversion{{Name: "a", Factor: 1}, {Name: "a", Factor: 2}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exp.NewConvertingGatherer(tc.gatherer, tc.conversions); err == nil {
				t.Error("Expected an error, got nil.")
			}
		})
	}
}
