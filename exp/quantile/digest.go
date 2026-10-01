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

import "fmt"

// DigestVersion is the current version of the Digest format. Digests
// with a different version are rejected by MergeDigest and
// NewFromDigest.
const DigestVersion = 1

// DigestLevel is one compaction level of a Digest.
type DigestLevel struct {
	// Weight is the weight of every value in this level, always a
	// power of two.
	Weight uint64 `json:"weight"`
	// Values are the retained samples of this level.
	Values []float64 `json:"values"`
}

// Digest is the exported intermediate state of a Sketch. It is the
// mergeable equivalent of a prometheus.Summary's final result: workers
// export Digests of their local observations and a central instance
// merges them via MergeDigest or NewFromDigest.
//
// A Digest is a plain data structure with JSON tags, so it can be
// serialized with encoding/json (or any other codec) for transport. The
// Version field and the sketch parameters (BufferSize, MaxLevels) are
// validated on import, so mixing digests produced by incompatible
// configurations is rejected instead of silently corrupting results.
type Digest struct {
	// Version is the digest format version, see DigestVersion.
	Version uint32 `json:"version"`
	// BufferSize and MaxLevels are the sketch parameters the digest
	// was produced with. They must match the importing sketch.
	BufferSize uint32 `json:"buffer_size"`
	MaxLevels  uint32 `json:"max_levels"`
	// Count is the exact number of observations.
	Count uint64 `json:"count"`
	// Sum is the exact sum of all observations.
	Sum float64 `json:"sum"`
	// Min and Max are the exact extreme observations.
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	// ErrorBound is the accumulated absolute rank error budget. It
	// is carried over on merge so that the rank error guarantee of
	// the merged sketch remains valid.
	ErrorBound uint64 `json:"error_bound"`
	// Levels holds the compaction levels, sorted by weight ascending.
	Levels []DigestLevel `json:"levels"`
}

// Export returns the current state of the sketch as a Digest. The
// Digest is a deep copy and remains valid even if the sketch is
// modified afterwards.
func (s *Sketch) Export() Digest {
	d := Digest{
		Version:    DigestVersion,
		BufferSize: uint32(s.bufferSize),
		MaxLevels:  uint32(s.maxLevels),
		Count:      s.count,
		Sum:        s.sum,
		Min:        s.min,
		Max:        s.max,
		ErrorBound: s.errBound,
		Levels:     make([]DigestLevel, len(s.levels)),
	}
	for i := range s.levels {
		d.Levels[i] = DigestLevel{
			Weight: s.levels[i].weight,
			Values: append([]float64(nil), s.levels[i].buf...),
		}
	}
	return d
}

// NewFromDigest creates a new Sketch from a previously exported Digest.
// The digest's format version must match DigestVersion and its
// parameters must be valid, otherwise an error wrapping
// ErrIncompatibleDigest is returned.
func NewFromDigest(d Digest) (*Sketch, error) {
	if err := checkDigestVersion(d); err != nil {
		return nil, err
	}
	s, err := NewSketch(SketchConfig{BufferSize: int(d.BufferSize), MaxLevels: int(d.MaxLevels)})
	if err != nil {
		return nil, fmt.Errorf("quantile: invalid digest parameters: %w", err)
	}
	if err := s.load(d); err != nil {
		return nil, err
	}
	return s, nil
}

// MergeDigest merges a Digest exported by another Sketch into s. The
// digest's format version must match DigestVersion and its BufferSize
// and MaxLevels must match s, otherwise an error wrapping
// ErrIncompatibleDigest is returned and s is left unmodified.
func (s *Sketch) MergeDigest(d Digest) error {
	if err := checkDigestVersion(d); err != nil {
		return err
	}
	if err := s.checkParams(int(d.BufferSize), int(d.MaxLevels)); err != nil {
		return err
	}
	return s.load(d)
}

func checkDigestVersion(d Digest) error {
	if d.Version != DigestVersion {
		return fmt.Errorf(
			"quantile: unsupported digest version %d (supported: %d): %w",
			d.Version, DigestVersion, ErrIncompatibleDigest,
		)
	}
	return nil
}

// load merges the contents of a validated digest into s.
func (s *Sketch) load(d Digest) error {
	for _, l := range d.Levels {
		if l.Weight == 0 {
			return fmt.Errorf("quantile: digest contains a level with weight 0: %w", ErrIncompatibleDigest)
		}
	}
	for _, l := range d.Levels {
		s.absorb(l.Weight, l.Values)
	}
	s.count += d.Count
	s.sum += d.Sum
	s.errBound += d.ErrorBound
	if d.Count > 0 {
		if s.count == d.Count {
			s.min, s.max = d.Min, d.Max
		} else {
			if d.Min < s.min {
				s.min = d.Min
			}
			if d.Max > s.max {
				s.max = d.Max
			}
		}
	}
	return nil
}
