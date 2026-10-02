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
	"math"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

// Gatherer is the interface of the wrapped gatherer. It matches
// prometheus.Gatherer, so a *prometheus.Registry (or any other Gatherer from
// the stable module) can be used directly.
type Gatherer interface {
	Gather() ([]*dto.MetricFamily, error)
}

// UnitConversion describes a unit conversion applied to a single metric
// family, e.g. rescaling a latency metric from milliseconds to seconds.
type UnitConversion struct {
	// Name is the exact name of the metric family to convert.
	Name string
	// NewName is the name the converted metric family is exposed under. If
	// empty, the family keeps its original name and only its values are
	// rescaled.
	NewName string
	// Factor is the multiplier applied to sample values, histogram bucket
	// upper bounds, histogram sums, and exemplar values. Sample counts and
	// bucket counts are never scaled. Factor must be greater than zero and
	// neither NaN nor Inf.
	Factor float64
}

// ConvertingGatherer wraps a Gatherer and applies a configured set of unit
// conversions to the gathered metric families. Families without a matching
// conversion are passed through unchanged. Converted families are deep
// copies; the metric families returned by the wrapped Gatherer are never
// modified.
//
// If a conversion renames a family to a name that is also produced by any
// other family (converted or not), Gather returns an error instead of
// exposing two families under the same name.
type ConvertingGatherer struct {
	gatherer    Gatherer
	conversions map[string]UnitConversion
}

// NewConvertingGatherer returns a ConvertingGatherer wrapping gatherer and
// applying the given conversions. It returns an error if a conversion is
// invalid or if two conversions target the same metric family name.
func NewConvertingGatherer(gatherer Gatherer, conversions []UnitConversion) (*ConvertingGatherer, error) {
	if gatherer == nil {
		return nil, errors.New("exp: gatherer must not be nil")
	}
	byName := make(map[string]UnitConversion, len(conversions))
	for _, c := range conversions {
		if c.Name == "" {
			return nil, errors.New("exp: unit conversion with empty metric family name")
		}
		if c.Factor <= 0 || math.IsNaN(c.Factor) || math.IsInf(c.Factor, 0) {
			return nil, fmt.Errorf("exp: unit conversion for %q has invalid factor %v", c.Name, c.Factor)
		}
		if _, dup := byName[c.Name]; dup {
			return nil, fmt.Errorf("exp: duplicate unit conversion for metric family %q", c.Name)
		}
		byName[c.Name] = c
	}
	return &ConvertingGatherer{gatherer: gatherer, conversions: byName}, nil
}

// Gather implements Gatherer.
func (g *ConvertingGatherer) Gather() ([]*dto.MetricFamily, error) {
	mfs, err := g.gatherer.Gather()
	if err != nil {
		return nil, err
	}
	out := make([]*dto.MetricFamily, 0, len(mfs))
	// seen maps each exposed family name to the name of the family it
	// originates from, to detect name collisions introduced by conversions.
	seen := make(map[string]string, len(mfs))
	for _, mf := range mfs {
		src := mf.GetName()
		if conv, ok := g.conversions[src]; ok {
			mf, err = convertMetricFamily(mf, conv)
			if err != nil {
				return nil, err
			}
		}
		name := mf.GetName()
		if prev, dup := seen[name]; dup && prev != src {
			return nil, fmt.Errorf("exp: unit conversion of metric family %q collides with metric family %q, both are exposed as %q", src, prev, name)
		}
		seen[name] = src
		out = append(out, mf)
	}
	return out, nil
}

// convertMetricFamily returns a deep copy of mf with the conversion applied.
// The input family is never modified.
func convertMetricFamily(mf *dto.MetricFamily, conv UnitConversion) (*dto.MetricFamily, error) {
	out := proto.Clone(mf).(*dto.MetricFamily)
	if conv.NewName != "" {
		out.Name = proto.String(conv.NewName)
	}
	for _, m := range out.GetMetric() {
		var err error
		switch mf.GetType() {
		case dto.MetricType_GAUGE:
			convertGauge(m.GetGauge(), conv.Factor)
		case dto.MetricType_COUNTER:
			convertCounter(m.GetCounter(), conv.Factor)
		case dto.MetricType_HISTOGRAM:
			err = convertHistogram(m.GetHistogram(), conv)
		default:
			err = fmt.Errorf("exp: unit conversion of metric family %q: metric type %s is not supported", mf.GetName(), mf.GetType())
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// convertGauge scales the gauge value in place.
func convertGauge(g *dto.Gauge, factor float64) {
	if g == nil {
		return
	}
	*g.Value *= factor
}

// convertCounter scales the counter value and its exemplar in place.
func convertCounter(c *dto.Counter, factor float64) {
	if c == nil {
		return
	}
	*c.Value *= factor
	convertExemplar(c.Exemplar, factor)
}

// convertHistogram scales bucket upper bounds, the sample sum, and exemplar
// values in place. Sample and bucket counts are left untouched, as they do
// not carry the converted unit.
func convertHistogram(h *dto.Histogram, conv UnitConversion) error {
	if h == nil {
		return nil
	}
	if h.Schema != nil {
		return fmt.Errorf("exp: unit conversion of metric family %q: native histograms cannot be rescaled", conv.Name)
	}
	*h.SampleSum *= conv.Factor
	for _, b := range h.GetBucket() {
		*b.UpperBound *= conv.Factor
		convertExemplar(b.Exemplar, conv.Factor)
	}
	for _, e := range h.GetExemplars() {
		convertExemplar(e, conv.Factor)
	}
	return nil
}

// convertExemplar scales the exemplar value in place. Labels and timestamps
// are left untouched.
func convertExemplar(e *dto.Exemplar, factor float64) {
	if e == nil {
		return
	}
	*e.Value *= factor
}
