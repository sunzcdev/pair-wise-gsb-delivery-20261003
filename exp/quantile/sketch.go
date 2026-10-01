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

// Package quantile implements an experimental, mergeable quantile sketch
// together with a prometheus.Collector that exposes the sketch as a
// Summary metric.
//
// # Error model
//
// The sketch guarantees a bounded rank error: for any value x, the
// estimated number of observations below x differs from the true number
// by at most RankErrorBound()*N, where N is the total number of
// observations. The bound accumulates deterministically during
// compaction and therefore holds for one-shot sketches as well as for
// sketches merged from partial results in any order.
//
// The sketch does NOT guarantee any relative error on the values
// returned by Quantile. If the observed distribution has large gaps,
// the returned value may be far away from the ideal quantile value in
// absolute or relative terms, even though its rank is guaranteed to be
// close. Callers must not interpret the results as having a bounded
// numerical relative error.
//
// # Mergeability
//
// Unlike prometheus.Summary, whose results cannot be combined across
// processes, a Sketch can export its intermediate state as a Digest.
// Another Sketch with identical parameters can then merge that Digest,
// yielding a result whose rank error bound is the sum of the individual
// bounds. Digests carry a format version and the sketch parameters and
// are checked for compatibility before merging.
package quantile

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
)

const (
	// DefaultBufferSize is the default number of samples kept per
	// compaction level.
	DefaultBufferSize = 128
	// DefaultMaxLevels is the default maximum number of compaction
	// levels.
	DefaultMaxLevels = 32
)

// SketchConfig configures a Sketch.
type SketchConfig struct {
	// BufferSize is the number of samples buffered per compaction
	// level before the level is compacted. It must be even and at
	// least 4. Larger buffers reduce the rank error at the cost of
	// memory. Defaults to DefaultBufferSize.
	BufferSize int
	// MaxLevels caps the number of compaction levels and thereby the
	// memory footprint: a Sketch never retains more than
	// (MaxLevels+1)*BufferSize samples, including transient states
	// during Observe and Merge. Once the cap is reached, the top
	// level compacts into itself, which keeps memory bounded while
	// the accumulated rank error bound keeps tracking the additional
	// error. It must be at least 1. Defaults to DefaultMaxLevels.
	MaxLevels int
	// Rand is the source of randomness used to pick compaction
	// offsets. It is mainly useful to obtain reproducible results in
	// tests. If nil, the global math/rand/v2 source is used.
	Rand *rand.Rand
}

// compactor is a single level of the sketch. All values in buf have the
// same weight, which is always a power of two.
type compactor struct {
	weight uint64
	buf    []float64
}

// Sketch is a mergeable quantile sketch using hierarchical compaction,
// in the spirit of the KLL sketch. It is not safe for concurrent use;
// the Collector adds its own synchronization.
type Sketch struct {
	bufferSize int
	maxLevels  int
	levels     []compactor // Sorted by weight ascending.
	count      uint64
	sum        float64
	min        float64
	max        float64
	errBound   uint64 // Accumulated rank error budget from compactions.
	rand       *rand.Rand
}

// NewSketch returns an empty Sketch configured by cfg.
func NewSketch(cfg SketchConfig) (*Sketch, error) {
	if cfg.BufferSize == 0 {
		cfg.BufferSize = DefaultBufferSize
	}
	if cfg.MaxLevels == 0 {
		cfg.MaxLevels = DefaultMaxLevels
	}
	if cfg.BufferSize < 4 || cfg.BufferSize%2 != 0 {
		return nil, fmt.Errorf("quantile: BufferSize must be even and at least 4, got %d", cfg.BufferSize)
	}
	if cfg.MaxLevels < 1 {
		return nil, fmt.Errorf("quantile: MaxLevels must be at least 1, got %d", cfg.MaxLevels)
	}
	return &Sketch{
		bufferSize: cfg.BufferSize,
		maxLevels:  cfg.MaxLevels,
		rand:       cfg.Rand,
	}, nil
}

// Observe adds a single observation to the sketch. NaN values are
// ignored, as they would corrupt the ordering invariants of the sketch.
func (s *Sketch) Observe(v float64) {
	if math.IsNaN(v) {
		return
	}
	if s.count == 0 {
		s.min, s.max = v, v
	} else {
		s.min = math.Min(s.min, v)
		s.max = math.Max(s.max, v)
	}
	s.count++
	s.sum += v
	if len(s.levels) == 0 || s.levels[0].weight != 1 {
		// The ingestion level has weight 1. It may be absent after
		// compactions or merges.
		s.levels = append(s.levels, compactor{})
		copy(s.levels[1:], s.levels)
		s.levels[0] = compactor{weight: 1, buf: make([]float64, 0, s.bufferSize)}
	}
	s.levels[0].buf = append(s.levels[0].buf, v)
	if len(s.levels[0].buf) >= s.bufferSize || len(s.levels) > s.maxLevels {
		s.normalize()
	}
}

// Count returns the total number of observations. It is exact.
func (s *Sketch) Count() uint64 {
	return s.count
}

// Sum returns the sum of all observations. It is exact.
func (s *Sketch) Sum() float64 {
	return s.sum
}

// RetainedSamples returns the number of samples currently held by the
// sketch. It never exceeds (MaxLevels+1)*BufferSize.
func (s *Sketch) RetainedSamples() int {
	n := 0
	for i := range s.levels {
		n += len(s.levels[i].buf)
	}
	return n
}

// RankErrorBound returns the current relative rank error bound epsilon:
// for any value x, the estimated number of observations below x is
// guaranteed to differ from the true number by at most epsilon*N, where
// N is Count(). The bound grows deterministically with every compaction
// and is carried over (additively) by Merge, so it remains valid for
// merged sketches regardless of the merge order.
func (s *Sketch) RankErrorBound() float64 {
	if s.count == 0 {
		return 0
	}
	return float64(s.errBound) / float64(s.count)
}

// Quantile returns an estimate of the q-quantile, for q in [0, 1]. The
// rank of the returned value is guaranteed to be within
// RankErrorBound()*Count() of q*Count(), but no bound on the numerical
// error of the returned value itself is provided (see the package
// documentation). The 0- and 1-quantiles are the exact minimum and
// maximum. Quantile returns NaN if the sketch is empty.
func (s *Sketch) Quantile(q float64) float64 {
	if s.count == 0 {
		return math.NaN()
	}
	if q <= 0 {
		return s.min
	}
	if q >= 1 {
		return s.max
	}
	type weightedValue struct {
		value  float64
		weight uint64
	}
	items := make([]weightedValue, 0, s.RetainedSamples())
	for i := range s.levels {
		for _, v := range s.levels[i].buf {
			items = append(items, weightedValue{value: v, weight: s.levels[i].weight})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].value < items[j].value })
	target := q * float64(s.count)
	var cum uint64
	for _, it := range items {
		cum += it.weight
		if float64(cum) >= target {
			return it.value
		}
	}
	return s.max
}

// Merge merges other into s. Both sketches must have been created with
// identical BufferSize and MaxLevels, otherwise an error is returned.
// The rank error bound of the result is the sum of the bounds of both
// inputs plus any additional error from compactions triggered by the
// merge, independent of the order in which merges happen.
func (s *Sketch) Merge(other *Sketch) error {
	if err := s.checkParams(other.bufferSize, other.maxLevels); err != nil {
		return err
	}
	for i := range other.levels {
		s.absorb(other.levels[i].weight, other.levels[i].buf)
	}
	s.count += other.count
	s.sum += other.sum
	s.errBound += other.errBound
	if other.count > 0 {
		if s.count == other.count {
			s.min, s.max = other.min, other.max
		} else {
			s.min = math.Min(s.min, other.min)
			s.max = math.Max(s.max, other.max)
		}
	}
	return nil
}

func (s *Sketch) checkParams(bufferSize, maxLevels int) error {
	if bufferSize != s.bufferSize || maxLevels != s.maxLevels {
		return fmt.Errorf(
			"quantile: incompatible sketch parameters (BufferSize %d vs %d, MaxLevels %d vs %d): %w",
			bufferSize, s.bufferSize, maxLevels, s.maxLevels, ErrIncompatibleDigest,
		)
	}
	return nil
}

// compactAt compacts the level at index idx: it sorts the buffer and
// keeps one randomly chosen value per adjacent pair, then pushes the
// kept values to the level with twice the weight. An odd leftover value
// keeps its weight. If the top level is compacted while the level cap
// is reached, it compacts into itself with doubled weight, keeping the
// memory footprint bounded. Compaction conserves the total weight.
func (s *Sketch) compactAt(idx int) {
	if len(s.levels[idx].buf) < 2 {
		return
	}
	c := &s.levels[idx]
	sort.Float64s(c.buf)
	n := len(c.buf)
	m := n - n%2 // Only the even prefix is compacted.
	offset := 0
	if s.rand != nil {
		offset = s.rand.IntN(2)
	} else {
		offset = rand.IntN(2)
	}
	kept := make([]float64, 0, m/2)
	for i := offset; i < m; i += 2 {
		kept = append(kept, c.buf[i])
	}
	// An odd leftover value keeps its current weight.
	var leftover []float64
	if n%2 == 1 {
		leftover = append(leftover, c.buf[n-1])
	}
	w := c.weight
	s.errBound += w
	if idx == len(s.levels)-1 && len(s.levels) >= s.maxLevels {
		// The level cap is reached: compact the top level into
		// itself, doubling its weight.
		c.buf = kept
		c.weight = 2 * w
		if len(leftover) > 0 {
			// The leftover keeps the old weight and therefore
			// belongs to a level below the top.
			s.absorb(w, leftover)
		}
		return
	}
	if len(leftover) > 0 {
		// The level stays with the leftover at its current weight.
		c.buf = leftover
	} else {
		// Drop the emptied level.
		s.levels = append(s.levels[:idx], s.levels[idx+1:]...)
	}
	s.absorb(2*w, kept)
}

// absorb inserts items of the given weight into the matching level,
// creating the level if needed, and restores the sketch invariants.
func (s *Sketch) absorb(weight uint64, items []float64) {
	if len(items) == 0 {
		return
	}
	idx := sort.Search(len(s.levels), func(i int) bool { return s.levels[i].weight >= weight })
	switch {
	case idx < len(s.levels) && s.levels[idx].weight == weight:
		s.levels[idx].buf = append(s.levels[idx].buf, items...)
	default:
		s.levels = append(s.levels, compactor{})
		copy(s.levels[idx+1:], s.levels[idx:])
		s.levels[idx] = compactor{weight: weight, buf: append([]float64(nil), items...)}
	}
	s.normalize()
}

// normalize restores the sketch invariants: at most maxLevels levels
// and every level below its buffer capacity.
func (s *Sketch) normalize() {
	for len(s.levels) > 0 {
		if len(s.levels) > s.maxLevels {
			idx := s.lowestCompactableLevel()
			if idx < 0 {
				// Every level holds a single value. This
				// transient state resolves with the next
				// observation and is still bounded in
				// memory.
				return
			}
			s.compactAt(idx)
			continue
		}
		full := -1
		for i := range s.levels {
			if len(s.levels[i].buf) >= s.bufferSize {
				full = i
				break
			}
		}
		if full < 0 {
			return
		}
		s.compactAt(full)
	}
}

// lowestCompactableLevel returns the index of the lowest level holding
// at least two values, or -1 if there is none.
func (s *Sketch) lowestCompactableLevel() int {
	for i := range s.levels {
		if len(s.levels[i].buf) >= 2 {
			return i
		}
	}
	return -1
}

// ErrIncompatibleDigest is returned (possibly wrapped) when a Digest or
// a Sketch cannot be merged because its format version or its
// parameters do not match the receiver.
var ErrIncompatibleDigest = errors.New("incompatible quantile digest")
