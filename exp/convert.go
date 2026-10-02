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

package exp

import (
	"errors"
	"fmt"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

// Gatherer is the interface of the wrapped gatherer. It is compatible with
// prometheus.Gatherer, so a *prometheus.Registry can be wrapped directly.
type Gatherer interface {
	Gather() ([]*dto.MetricFamily, error)
}

// Conversion describes how a single metric family is renamed and rescaled,
// e.g. exposing a legacy milliseconds metric as seconds.
type Conversion struct {
	// From is the exact name of the metric family to convert.
	From string
	// To is the name the metric family is exposed as after the conversion.
	To string
	// Factor is the multiplier turning an old value into a new one, i.e.
	// new = old * Factor. For histograms it is applied to the sum, the
	// classic bucket upper bounds and exemplar values, but never to counts.
	Factor float64
}

// ConvertingGatherer wraps a Gatherer and exposes selected metric families
// under a new name with their values rescaled by a configured factor. It
// allows changing the unit of exported metrics without touching the
// instrumentation. Metric families without a matching Conversion are passed
// through unchanged. The wrapped Gatherer and the metric families it returns
// are never modified; converted families are deep copies.
//
// Only gauge, counter and histogram families can be converted. Gather
// returns an error if a Conversion matches a family of any other type, or if
// a converted name collides with the name of another gathered family.
type ConvertingGatherer struct {
	gatherer    Gatherer
	conversions map[string]Conversion
}

// NewConvertingGatherer returns a ConvertingGatherer wrapping gatherer and
// applying the given conversions. It returns an error if gatherer is nil or
// if the conversions are inconsistent, e.g. contain empty names, duplicate
// sources or duplicate targets.
func NewConvertingGatherer(gatherer Gatherer, conversions []Conversion) (*ConvertingGatherer, error) {
	if gatherer == nil {
		return nil, errors.New("exp: gatherer must not be nil")
	}
	byFrom := make(map[string]Conversion, len(conversions))
	byTo := make(map[string]struct{}, len(conversions))
	for _, c := range conversions {
		if c.From == "" || c.To == "" {
			return nil, fmt.Errorf("exp: conversion source and target names must not be empty: %+v", c)
		}
		if _, dup := byFrom[c.From]; dup {
			return nil, fmt.Errorf("exp: duplicate conversion for metric family %q", c.From)
		}
		if _, dup := byTo[c.To]; dup {
			return nil, fmt.Errorf("exp: conversions target the same metric family name %q", c.To)
		}
		byFrom[c.From] = c
		byTo[c.To] = struct{}{}
	}
	return &ConvertingGatherer{gatherer: gatherer, conversions: byFrom}, nil
}

// Gather implements Gatherer. It is safe for concurrent use.
func (g *ConvertingGatherer) Gather() ([]*dto.MetricFamily, error) {
	mfs, err := g.gatherer.Gather()
	if err != nil {
		return nil, err
	}
	out := make([]*dto.MetricFamily, 0, len(mfs))
	// seen maps each exposed family name to the name of the gathered family
	// it originates from, to detect collisions introduced by the conversion.
	seen := make(map[string]string, len(mfs))
	for _, mf := range mfs {
		from := mf.GetName()
		to := from
		c, convert := g.conversions[from]
		if convert {
			to = c.To
		}
		if origin, dup := seen[to]; dup {
			return nil, fmt.Errorf("exp: conversion of metric family %q to %q collides with metric family %q", from, to, origin)
		}
		seen[to] = from
		if !convert {
			out = append(out, mf)
			continue
		}
		converted, err := convertMetricFamily(mf, c)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

// convertMetricFamily returns a deep copy of mf renamed to c.To with its
// values multiplied by c.Factor. Counts are never rescaled.
func convertMetricFamily(mf *dto.MetricFamily, c Conversion) (*dto.MetricFamily, error) {
	typ := mf.GetType()
	switch typ {
	case dto.MetricType_GAUGE, dto.MetricType_COUNTER, dto.MetricType_HISTOGRAM:
	default:
		return nil, fmt.Errorf("exp: cannot convert metric family %q of type %s: only gauge, counter and histogram are supported", mf.GetName(), typ)
	}
	out := proto.Clone(mf).(*dto.MetricFamily)
	out.Name = proto.String(c.To)
	for _, m := range out.GetMetric() {
		switch typ {
		case dto.MetricType_GAUGE:
			g := m.GetGauge()
			g.Value = proto.Float64(g.GetValue() * c.Factor)
		case dto.MetricType_COUNTER:
			cnt := m.GetCounter()
			cnt.Value = proto.Float64(cnt.GetValue() * c.Factor)
			if e := cnt.GetExemplar(); e != nil {
				e.Value = proto.Float64(e.GetValue() * c.Factor)
			}
		case dto.MetricType_HISTOGRAM:
			h := m.GetHistogram()
			h.SampleSum = proto.Float64(h.GetSampleSum() * c.Factor)
			for _, b := range h.GetBucket() {
				b.UpperBound = proto.Float64(b.GetUpperBound() * c.Factor)
				if e := b.GetExemplar(); e != nil {
					e.Value = proto.Float64(e.GetValue() * c.Factor)
				}
			}
		}
	}
	return out, nil
}
