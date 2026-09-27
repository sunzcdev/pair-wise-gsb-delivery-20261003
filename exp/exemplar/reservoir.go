// Package exemplar provides a configurable, bounded reservoir that retains
// exemplar candidates observed within a sliding time window and selects one
// candidate to expose according to an explicit sampling policy.
//
// Unlike the exemplar storage built into native histograms, which keeps only
// a small spread of exemplars by value, a Reservoir keeps a bounded set of
// the most recent candidates within the window, so that investigations of
// latency spikes can still find exemplars from the first half of a scrape
// window.
//
// This package is experimental and may contain breaking changes or be
// removed in the future.
package exemplar

import (
	"maps"
	"math"
	"math/rand"
	"sync"
	"time"
)

const (
	// DefaultMaxCandidates is the number of candidates retained per window
	// when Config.MaxCandidates is zero.
	DefaultMaxCandidates = 16
	// DefaultWindow is the retention window used when Config.Window is zero
	// or negative.
	DefaultWindow = 5 * time.Minute
)

// Clock abstracts the source of time used for timestamps and expiry. It
// exists so tests can reproduce selection results deterministically.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}

// realClock is the Clock used when Config.Clock is nil.
type realClock struct{}

// Now returns time.Now().
func (realClock) Now() time.Time { return time.Now() }

// Policy is the explicit sampling rule a Reservoir uses to pick the exposed
// exemplar among the currently retained candidates.
type Policy int

const (
	// PolicyNewest deterministically selects the retained candidate with the
	// most recent timestamp. It involves no randomness at all.
	PolicyNewest Policy = iota
	// PolicyUniformRandom selects uniformly at random among the currently
	// retained, not yet expired candidates. Note that uniformity holds only
	// over the retained candidates, not over all observations in the window.
	// Retention itself is recency-biased: once the reservoir is full, the
	// oldest candidate is evicted to make room for a new one. Do not treat
	// the selection as a uniform sample of all requests seen in the window.
	PolicyUniformRandom
	// PolicyWeightedRandom selects among the currently retained candidates
	// with probability proportional to their Exemplar.Weight. It is a
	// weighted random pick over retained candidates only, subject to the
	// same recency-biased retention as PolicyUniformRandom.
	PolicyWeightedRandom
)

// Config configures a Reservoir. The zero value is valid and uses
// DefaultMaxCandidates, DefaultWindow, PolicyNewest, the system clock, and a
// randomly seeded random source.
type Config struct {
	// MaxCandidates bounds the number of candidates retained within the
	// window, and therefore bounds the memory used by the Reservoir. Zero
	// means DefaultMaxCandidates. A negative value disables the Reservoir:
	// Observe becomes a no-op and Exemplar always reports no exemplar.
	MaxCandidates int
	// Window is the retention window. Candidates older than Window are
	// expired. Zero or negative means DefaultWindow.
	Window time.Duration
	// Policy is the sampling rule used by Exemplar. The zero value is
	// PolicyNewest. Unknown values behave like PolicyNewest.
	Policy Policy
	// Clock is the time source used for timestamps and expiry. Nil means
	// the system clock. It is only called while the Reservoir lock is held.
	Clock Clock
	// Rand is the random source used by PolicyUniformRandom and
	// PolicyWeightedRandom. It is only used while the Reservoir lock is
	// held, so it does not need to be safe for concurrent use. Nil means a
	// randomly seeded source. Supply a seeded rand.Rand in tests to
	// reproduce selection results.
	Rand *rand.Rand
}

// Exemplar is a single candidate observation.
type Exemplar struct {
	// Value is the observed value, for example a request duration in
	// seconds.
	Value float64
	// Labels are the exemplar labels, for example a trace ID. The Reservoir
	// copies the map on Observe and on Exemplar, so callers retain ownership
	// of any map they pass in or receive.
	Labels map[string]string
	// Weight is the selection weight used by PolicyWeightedRandom. Values
	// less than or equal to zero, NaN, and positive infinity are normalized
	// to 1 on Observe, so the zero value behaves like an unweighted
	// observation.
	Weight float64
	// Timestamp is the observation time. It is set by the Reservoir on
	// Observe from the configured Clock; any value provided by the caller is
	// ignored.
	Timestamp time.Time
}

// Reservoir retains a bounded set of exemplar candidates observed within a
// sliding time window and selects one of them for exposition. It is safe for
// concurrent use. It starts no background goroutines: expiry of candidates
// happens lazily, on Observe, Exemplar, and Len calls, and every sweep costs
// at most O(MaxCandidates), so neither the candidate count nor the cleanup
// work can grow without bound under concurrent Observe calls and frequent
// scrapes.
type Reservoir struct {
	mu            sync.Mutex
	maxCandidates int
	window        time.Duration
	policy        Policy
	clock         Clock
	rand          *rand.Rand
	candidates    []Exemplar
}

// NewReservoir returns a Reservoir configured by cfg.
func NewReservoir(cfg Config) *Reservoir {
	maxCandidates := cfg.MaxCandidates
	if maxCandidates == 0 {
		maxCandidates = DefaultMaxCandidates
	}
	window := cfg.Window
	if window <= 0 {
		window = DefaultWindow
	}
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}
	rnd := cfg.Rand
	if rnd == nil {
		rnd = rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // A cryptographic source is not needed for exemplar sampling.
	}
	return &Reservoir{
		maxCandidates: maxCandidates,
		window:        window,
		policy:        cfg.Policy,
		clock:         clock,
		rand:          rnd,
		candidates:    make([]Exemplar, 0, max(maxCandidates, 0)),
	}
}

// Observe records e as a candidate. The Labels map is copied. Expired
// candidates are swept first; if the Reservoir is still full afterwards, the
// oldest candidate is evicted. Observe on a disabled Reservoir
// (Config.MaxCandidates < 0) is a no-op.
func (r *Reservoir) Observe(e Exemplar) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxCandidates <= 0 {
		return
	}
	e.Timestamp = r.clock.Now()
	e.Weight = normalizeWeight(e.Weight)
	e.Labels = maps.Clone(e.Labels)
	r.expireLocked(e.Timestamp)
	r.candidates = append(r.candidates, e)
	if len(r.candidates) > r.maxCandidates {
		r.evictOldestLocked()
	}
}

// Exemplar expires stale candidates and then selects one of the retained
// candidates according to the configured Policy. It returns false if no
// candidate is currently retained. The Labels map of the returned Exemplar
// is a copy owned by the caller.
func (r *Reservoir) Exemplar() (Exemplar, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(r.clock.Now())
	if len(r.candidates) == 0 {
		return Exemplar{}, false
	}
	selected := r.candidates[r.selectLocked()]
	selected.Labels = maps.Clone(selected.Labels)
	return selected, true
}

// Len returns the number of currently retained, not yet expired candidates.
// It never exceeds the configured MaxCandidates.
func (r *Reservoir) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(r.clock.Now())
	return len(r.candidates)
}

// expireLocked removes all candidates that are older than the window
// relative to now. It costs O(len(r.candidates)).
func (r *Reservoir) expireLocked(now time.Time) {
	kept := r.candidates[:0]
	for _, c := range r.candidates {
		if now.Sub(c.Timestamp) < r.window {
			kept = append(kept, c)
		}
	}
	// Clear the tail so that expired label maps can be garbage collected.
	for i := len(kept); i < len(r.candidates); i++ {
		r.candidates[i] = Exemplar{}
	}
	r.candidates = kept
}

// evictOldestLocked removes the candidate with the oldest timestamp. It
// costs O(len(r.candidates)).
func (r *Reservoir) evictOldestLocked() {
	oldest := 0
	for i := 1; i < len(r.candidates); i++ {
		if r.candidates[i].Timestamp.Before(r.candidates[oldest].Timestamp) {
			oldest = i
		}
	}
	copy(r.candidates[oldest:], r.candidates[oldest+1:])
	r.candidates[len(r.candidates)-1] = Exemplar{}
	r.candidates = r.candidates[:len(r.candidates)-1]
}

// selectLocked returns the index of the candidate to expose according to the
// configured Policy. It requires at least one retained candidate.
func (r *Reservoir) selectLocked() int {
	switch r.policy {
	case PolicyUniformRandom:
		return r.rand.Intn(len(r.candidates))
	case PolicyWeightedRandom:
		total := 0.0
		for _, c := range r.candidates {
			total += c.Weight
		}
		target := r.rand.Float64() * total
		cumulative := 0.0
		for i, c := range r.candidates {
			cumulative += c.Weight
			if target < cumulative {
				return i
			}
		}
		// Guard against floating point rounding at the upper bound.
		return len(r.candidates) - 1
	default:
		// PolicyNewest and unknown values select the most recent candidate.
		newest := 0
		for i := 1; i < len(r.candidates); i++ {
			if !r.candidates[i].Timestamp.Before(r.candidates[newest].Timestamp) {
				newest = i
			}
		}
		return newest
	}
}

// normalizeWeight maps unusable weights to 1, so that the zero value of
// Exemplar behaves like an unweighted observation.
func normalizeWeight(w float64) float64 {
	if w <= 0 || math.IsNaN(w) || math.IsInf(w, 1) {
		return 1
	}
	return w
}
