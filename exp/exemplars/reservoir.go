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

// Package exemplars provides a bounded, time-windowed reservoir that retains
// candidate observations and selects exemplars from them according to an
// explicit, documented sampling rule.
//
// The reservoir addresses a common gap in the stable histogram exemplar
// support, where only the most recent observation per bucket is kept and
// earlier requests inside a latency spike are invisible to scrapes. Here, up
// to Config.Capacity observations from the last Config.Window are retained as
// candidates, and Select draws exemplars from the current candidate pool.
//
// Distribution guarantees, stated precisely:
//
//   - Retention is NOT uniform over the window. While the pool is not full,
//     every observation is retained. Once full, each new observation replaces
//     a uniformly random existing candidate (random replacement). Random
//     replacement is biased towards recent observations and is not equivalent
//     to reservoir (Algorithm R) sampling over the stream.
//   - Uniform selection (Config.Weighted == false) draws a uniform sample
//     without replacement from the candidates retained at selection time.
//     Uniformity holds over the retained pool only, not over all observations
//     in the window.
//   - Weighted selection (Config.Weighted == true) uses A-Res sampling: each
//     candidate receives key u^(1/w) with u uniform in (0,1] and w its
//     weight, and the k highest keys win. The probability of a candidate
//     being selected is proportional to its weight, without replacement,
//     over the retained pool only.
//
// Both the clock and the random source are injectable, so tests can
// reproduce exact selection outcomes. All methods are safe for concurrent
// use; the candidate count never exceeds Config.Capacity, and expired
// candidates are pruned lazily on Observe and Select, so neither concurrent
// Observe calls nor frequent scrapes can grow memory or cleanup work beyond
// O(Config.Capacity).
//
// This package is experimental and may change or be removed in the future.
package exemplars

import (
	"cmp"
	"maps"
	"math"
	"math/rand"
	"slices"
	"sync"
	"time"
)

// Default configuration values applied when Config fields are left at their
// zero value.
const (
	// DefaultCapacity is the default maximum number of retained candidates.
	DefaultCapacity = 64
	// DefaultWindow is the default retention window for candidates.
	DefaultWindow = 5 * time.Minute
)

// Clock reports the current time. It is injectable so tests can control
// window expiry deterministically.
type Clock interface {
	Now() time.Time
}

// Rand is the random source used for candidate replacement and exemplar
// selection. It matches the subset of *math/rand.Rand used by this package,
// so *rand.Rand satisfies it directly. Implementations do not need to be
// safe for concurrent use; the reservoir serializes all calls.
type Rand interface {
	Float64() float64
	Intn(n int) int
}

// realClock is the default Clock backed by time.Now.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Config configures a Reservoir. The zero value is valid and yields a
// reservoir with DefaultCapacity, DefaultWindow, uniform selection, a real
// clock, and a nondeterministically seeded random source.
type Config struct {
	// Capacity is the maximum number of candidates retained at any time.
	// Values <= 0 default to DefaultCapacity.
	Capacity int
	// Window is how long a candidate remains eligible for selection,
	// measured from its observation time. Values <= 0 default to
	// DefaultWindow.
	Window time.Duration
	// MaxExemplars is the number of exemplars returned by Exemplars.
	// Values <= 0 default to Capacity.
	MaxExemplars int
	// Weighted selects the sampling rule. If false, Select draws a uniform
	// sample without replacement from the retained candidates. If true,
	// Select draws with probability proportional to Observation.Weight
	// (A-Res sampling). See the package documentation for the exact
	// distribution guarantees of each mode.
	Weighted bool
	// Clock is the time source. If nil, a clock backed by time.Now is used.
	Clock Clock
	// Rand is the random source. If nil, a *rand.Rand seeded from the
	// current time is used. Supply a seeded *rand.Rand to reproduce
	// selection outcomes in tests.
	Rand Rand
}

// Observation is a single observed value offered to the reservoir.
type Observation struct {
	// Value is the observed value, exposed as the exemplar value.
	Value float64
	// Labels are the exemplar labels, typically carrying a trace ID. The
	// map is cloned on Observe, so the caller retains ownership and may
	// reuse or mutate it after Observe returns.
	Labels map[string]string
	// Time is the observation time. If zero, the reservoir's clock is
	// consulted at Observe time.
	Time time.Time
	// Weight is the selection weight used when Config.Weighted is true.
	// Values <= 0 are treated as 1. It is ignored in uniform mode.
	Weight float64
}

// Exemplar is a selected candidate ready for exposition.
type Exemplar struct {
	// Value is the observed value.
	Value float64
	// Labels are the exemplar labels. The map is owned by the caller and
	// is never shared with the reservoir's internal state.
	Labels map[string]string
	// Time is the observation time.
	Time time.Time
}

// candidate is a retained observation.
type candidate struct {
	value  float64
	labels map[string]string
	time   time.Time
	weight float64
}

// Reservoir retains a bounded pool of candidate observations inside a
// sliding time window and selects exemplars from it. Use NewReservoir to
// create one. All methods are safe for concurrent use.
type Reservoir struct {
	capacity     int
	window       time.Duration
	maxExemplars int
	weighted     bool
	clock        Clock
	rand         Rand

	mu         sync.Mutex
	candidates []candidate
}

// NewReservoir returns a Reservoir configured by cfg, applying documented
// defaults for zero-valued fields.
func NewReservoir(cfg Config) *Reservoir {
	if cfg.Capacity <= 0 {
		cfg.Capacity = DefaultCapacity
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	if cfg.MaxExemplars <= 0 {
		cfg.MaxExemplars = cfg.Capacity
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Reservoir{
		capacity:     cfg.Capacity,
		window:       cfg.Window,
		maxExemplars: cfg.MaxExemplars,
		weighted:     cfg.Weighted,
		clock:        cfg.Clock,
		rand:         cfg.Rand,
		candidates:   make([]candidate, 0, cfg.Capacity),
	}
}

// Observe offers obs to the reservoir. Observations already expired relative
// to the reservoir's clock are dropped. If the pool is full, obs replaces a
// uniformly random existing candidate; as documented on the package, this
// biases retention towards recent observations.
func (r *Reservoir) Observe(obs Observation) {
	now := r.clock.Now()
	if obs.Time.IsZero() {
		obs.Time = now
	}
	if obs.Time.Before(now.Add(-r.window)) {
		return
	}
	weight := obs.Weight
	if weight <= 0 {
		weight = 1
	}
	c := candidate{
		value:  obs.Value,
		labels: maps.Clone(obs.Labels),
		time:   obs.Time,
		weight: weight,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	if len(r.candidates) < r.capacity {
		r.candidates = append(r.candidates, c)
		return
	}
	r.candidates[r.rand.Intn(len(r.candidates))] = c
}

// Select draws up to k exemplars from the candidates retained at call time,
// using the sampling rule configured by Config.Weighted. It returns nil if k
// <= 0 or no candidates are retained. The returned exemplars are deep copies;
// the caller owns their label maps.
func (r *Reservoir) Select(k int) []Exemplar {
	if k <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.clock.Now())
	if len(r.candidates) == 0 {
		return nil
	}
	if k > len(r.candidates) {
		k = len(r.candidates)
	}
	var picked []int
	if r.weighted {
		picked = topKWeighted(r.candidates, k, r.rand)
	} else {
		picked = sampleUniform(len(r.candidates), k, r.rand)
	}
	exemplars := make([]Exemplar, 0, len(picked))
	for _, idx := range picked {
		c := r.candidates[idx]
		exemplars = append(exemplars, Exemplar{
			Value:  c.value,
			Labels: maps.Clone(c.labels),
			Time:   c.time,
		})
	}
	return exemplars
}

// Exemplars draws up to Config.MaxExemplars exemplars. It is shorthand for
// Select with the configured maximum.
func (r *Reservoir) Exemplars() []Exemplar {
	return r.Select(r.maxExemplars)
}

// Len returns the number of candidates currently retained, after pruning
// expired ones.
func (r *Reservoir) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.clock.Now())
	return len(r.candidates)
}

// pruneLocked drops candidates whose observation time is older than the
// retention window relative to now. The caller must hold r.mu. Pruning is
// O(capacity) and reuses the backing array, so frequent calls stay bounded.
func (r *Reservoir) pruneLocked(now time.Time) {
	cutoff := now.Add(-r.window)
	kept := r.candidates[:0]
	for _, c := range r.candidates {
		if !c.time.Before(cutoff) {
			kept = append(kept, c)
		}
	}
	// Zero the tail so pruned label maps are not retained by the array.
	for i := len(kept); i < len(r.candidates); i++ {
		r.candidates[i] = candidate{}
	}
	r.candidates = kept
}

// sampleUniform returns k distinct indices of a pool of size n, drawn
// uniformly without replacement via a partial Fisher-Yates shuffle.
func sampleUniform(n, k int, rnd Rand) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	for i := 0; i < k; i++ {
		j := i + rnd.Intn(n-i)
		idx[i], idx[j] = idx[j], idx[i]
	}
	return idx[:k]
}

// topKWeighted returns the indices of the k candidates with the highest
// A-Res keys u^(1/w), where u is uniform in (0,1] and w is the candidate
// weight. This selects without replacement with probability proportional to
// weight.
func topKWeighted(pool []candidate, k int, rnd Rand) []int {
	type keyed struct {
		idx int
		key float64
	}
	keys := make([]keyed, len(pool))
	for i, c := range pool {
		// Map u from [0,1) to (0,1] so keys stay finite and positive.
		u := 1 - rnd.Float64()
		keys[i] = keyed{idx: i, key: math.Pow(u, 1/c.weight)}
	}
	slices.SortFunc(keys, func(a, b keyed) int { return cmp.Compare(b.key, a.key) })
	picked := make([]int, k)
	for i := range picked {
		picked[i] = keys[i].idx
	}
	return picked
}
