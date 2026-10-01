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

package quantile

import (
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// DefaultQuantiles are the quantiles exposed by a Collector if
// Opts.Quantiles is empty.
var DefaultQuantiles = []float64{0.5, 0.9, 0.99}

// Opts configures a Collector.
type Opts struct {
	// Namespace, Subsystem, and Name build the fully qualified metric
	// name, as in prometheus.BuildFQName. Name is mandatory.
	Namespace string
	Subsystem string
	Name      string
	// Help is the metric's help string. A generic default is used if
	// empty.
	Help string
	// ConstLabels are attached to every sample of the metric. The
	// map is retained by the Collector and must not be modified
	// after New returns.
	ConstLabels prometheus.Labels
	// Quantiles are the quantiles exposed in the Summary metric.
	// Each value must be in [0, 1]. Defaults to DefaultQuantiles.
	//
	// Note that, unlike prometheus.Summary's Objectives, no
	// per-quantile error tolerance is configured: the same rank
	// error bound (Sketch.RankErrorBound) applies to every exposed
	// quantile, and no bound on the numerical error of the exposed
	// values is provided.
	Quantiles []float64
	// BufferSize and MaxLevels configure the underlying sketch, see
	// SketchConfig. The memory footprint of the Collector is bounded
	// by (MaxLevels+1)*BufferSize retained samples.
	BufferSize int
	MaxLevels  int
}

// Collector is a prometheus.Collector backed by a mergeable quantile
// Sketch. It exposes the observations as a Summary metric whose
// quantiles are computed from the sketch. Unlike prometheus.Summary,
// its intermediate state can be exported as a Digest and merged into
// another Collector (or Sketch) with matching parameters, which makes
// it suitable for aggregating quantiles across workers.
//
// A Collector is safe for concurrent use.
type Collector struct {
	mu        sync.Mutex
	sketch    *Sketch
	desc      *prometheus.Desc
	quantiles []float64
}

// New returns a Collector configured by opts.
func New(opts Opts) (*Collector, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("quantile: Name must not be empty")
	}
	if opts.Help == "" {
		opts.Help = "Quantiles of observed values, estimated by a mergeable sketch."
	}
	quantiles := opts.Quantiles
	if len(quantiles) == 0 {
		quantiles = DefaultQuantiles
	}
	quantiles = append([]float64(nil), quantiles...)
	for _, q := range quantiles {
		if q < 0 || q > 1 {
			return nil, fmt.Errorf("quantile: quantile %f out of range [0, 1]", q)
		}
	}
	sketch, err := NewSketch(SketchConfig{BufferSize: opts.BufferSize, MaxLevels: opts.MaxLevels})
	if err != nil {
		return nil, err
	}
	return &Collector{
		sketch:    sketch,
		desc:      prometheus.NewDesc(prometheus.BuildFQName(opts.Namespace, opts.Subsystem, opts.Name), opts.Help, nil, opts.ConstLabels),
		quantiles: quantiles,
	}, nil
}

// Observe adds a single observation. NaN values are ignored.
func (c *Collector) Observe(v float64) {
	c.mu.Lock()
	c.sketch.Observe(v)
	c.mu.Unlock()
}

// Export returns the intermediate state of the underlying sketch as a
// Digest, which can be merged into another Sketch or Collector with
// matching parameters.
func (c *Collector) Export() Digest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sketch.Export()
}

// MergeDigest merges a Digest exported by another Sketch or Collector
// into c. See Sketch.MergeDigest for the compatibility rules.
func (c *Collector) MergeDigest(d Digest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sketch.MergeDigest(d)
}

// Sketch returns a snapshot of the underlying sketch, e.g. to inspect
// Sketch.RankErrorBound. The returned sketch is a deep copy.
func (c *Collector) Sketch() *Sketch {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, err := NewFromDigest(c.sketch.Export())
	if err != nil {
		// Cannot happen: a digest exported by a valid sketch is
		// always importable.
		panic(err)
	}
	return s
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect implements prometheus.Collector. It emits the current state
// as a Summary metric, so the output can be scraped by Prometheus like
// any other summary.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	count := c.sketch.Count()
	sum := c.sketch.Sum()
	quantiles := make(map[float64]float64, len(c.quantiles))
	if count > 0 {
		for _, q := range c.quantiles {
			quantiles[q] = c.sketch.Quantile(q)
		}
	}
	c.mu.Unlock()
	ch <- prometheus.MustNewConstSummary(c.desc, count, sum, quantiles)
}
