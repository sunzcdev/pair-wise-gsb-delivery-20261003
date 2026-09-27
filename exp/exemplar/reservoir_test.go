package exemplar_test

import (
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/exp/exemplar"
)

// fakeClock is a manually advanced exemplar.Clock for deterministic tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1700000000, 0)}
}

// Now returns the current fake time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the fake time forward by d.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestRetentionIsBoundedAndRecencyBiased(t *testing.T) {
	clock := newFakeClock()
	r := exemplar.NewReservoir(exemplar.Config{
		MaxCandidates: 4,
		Window:        10 * time.Second,
		Policy:        exemplar.PolicyNewest,
		Clock:         clock,
	})
	// Candidate i is observed at t=i seconds, so the clock ends at t=10.
	for i := range 10 {
		r.Observe(exemplar.Exemplar{Value: float64(i)})
		clock.Advance(time.Second)
	}
	if got := r.Len(); got != 4 {
		t.Fatalf("expected 4 retained candidates, got %d", got)
	}
	e, ok := r.Exemplar()
	if !ok {
		t.Fatal("expected an exemplar")
	}
	if e.Value != 9 {
		t.Fatalf("expected the newest candidate 9, got %v", e.Value)
	}
	// At t=16 the candidate observed at t=6 exceeds the 10s window. If the
	// retained set is exactly {6,7,8,9}, i.e. candidates 0-5 were evicted
	// oldest-first, exactly one candidate expires per second from now on.
	clock.Advance(6 * time.Second)
	if got := r.Len(); got != 3 {
		t.Fatalf("expected 3 retained candidates after the oldest expired, got %d", got)
	}
	clock.Advance(time.Second)
	if got := r.Len(); got != 2 {
		t.Fatalf("expected 2 retained candidates after another second, got %d", got)
	}
}

func TestWindowExpiry(t *testing.T) {
	clock := newFakeClock()
	r := exemplar.NewReservoir(exemplar.Config{
		MaxCandidates: 4,
		Window:        time.Minute,
		Clock:         clock,
	})
	r.Observe(exemplar.Exemplar{Value: 1})
	clock.Advance(30 * time.Second)
	if _, ok := r.Exemplar(); !ok {
		t.Fatal("expected the candidate to still be within the window")
	}
	clock.Advance(31 * time.Second)
	if _, ok := r.Exemplar(); ok {
		t.Fatal("expected all candidates to be expired")
	}
	if got := r.Len(); got != 0 {
		t.Fatalf("expected 0 retained candidates, got %d", got)
	}
}

func TestExpiredCandidatesAreEvictedBeforeLiveOnes(t *testing.T) {
	clock := newFakeClock()
	r := exemplar.NewReservoir(exemplar.Config{
		MaxCandidates: 2,
		Window:        10 * time.Second,
		Policy:        exemplar.PolicyNewest,
		Clock:         clock,
	})
	r.Observe(exemplar.Exemplar{Value: 1})
	clock.Advance(time.Second)
	r.Observe(exemplar.Exemplar{Value: 2})
	// The reservoir is now full. Advance to t=10, where the first candidate
	// (observed at t=0) is expired but the second (observed at t=1) is not.
	clock.Advance(9 * time.Second)
	// The new observation must reuse the expired slot, not evict candidate 2.
	r.Observe(exemplar.Exemplar{Value: 3})
	if got := r.Len(); got != 2 {
		t.Fatalf("expected 2 retained candidates, got %d", got)
	}
	// Advance to t=11 so that candidate 2 expires but candidate 3 does not.
	clock.Advance(time.Second)
	e, ok := r.Exemplar()
	if !ok {
		t.Fatal("expected an exemplar")
	}
	if e.Value != 3 {
		t.Fatalf("expected candidate 3 to survive, got %v", e.Value)
	}
}

func TestPolicyNewestIsDeterministic(t *testing.T) {
	clock := newFakeClock()
	r := exemplar.NewReservoir(exemplar.Config{
		MaxCandidates: 8,
		Window:        time.Hour,
		Policy:        exemplar.PolicyNewest,
		Clock:         clock,
	})
	for i := range 5 {
		r.Observe(exemplar.Exemplar{Value: float64(i)})
		clock.Advance(time.Second)
	}
	for range 10 {
		e, ok := r.Exemplar()
		if !ok {
			t.Fatal("expected an exemplar")
		}
		if e.Value != 4 {
			t.Fatalf("PolicyNewest must always return the newest candidate 4, got %v", e.Value)
		}
	}
}

func TestPolicyUniformRandomIsReproducible(t *testing.T) {
	observe := func(r *exemplar.Reservoir) {
		for i := range 3 {
			r.Observe(exemplar.Exemplar{Value: float64(i)})
		}
	}
	r1 := exemplar.NewReservoir(exemplar.Config{
		MaxCandidates: 3,
		Window:        time.Hour,
		Policy:        exemplar.PolicyUniformRandom,
		Clock:         newFakeClock(),
		Rand:          rand.New(rand.NewSource(42)), //nolint:gosec // A deterministic seed is intended here.
	})
	r2 := exemplar.NewReservoir(exemplar.Config{
		MaxCandidates: 3,
		Window:        time.Hour,
		Policy:        exemplar.PolicyUniformRandom,
		Clock:         newFakeClock(),
		Rand:          rand.New(rand.NewSource(42)), //nolint:gosec // A deterministic seed is intended here.
	})
	observe(r1)
	observe(r2)
	seen := map[float64]bool{}
	for range 100 {
		e1, ok1 := r1.Exemplar()
		e2, ok2 := r2.Exemplar()
		if !ok1 || !ok2 {
			t.Fatal("expected exemplars")
		}
		if e1.Value != e2.Value {
			t.Fatalf("same seed must reproduce the same selection, got %v and %v", e1.Value, e2.Value)
		}
		seen[e1.Value] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected all 3 candidates to be selected over 100 draws, got %d", len(seen))
	}
}

func TestPolicyWeightedRandom(t *testing.T) {
	clock := newFakeClock()
	r := exemplar.NewReservoir(exemplar.Config{
		MaxCandidates: 2,
		Window:        time.Hour,
		Policy:        exemplar.PolicyWeightedRandom,
		Clock:         clock,
		Rand:          rand.New(rand.NewSource(7)), //nolint:gosec // A deterministic seed is intended here.
	})
	r.Observe(exemplar.Exemplar{Value: 1, Weight: 1})
	r.Observe(exemplar.Exemplar{Value: 2, Weight: 9})
	counts := map[float64]int{}
	for range 1000 {
		e, ok := r.Exemplar()
		if !ok {
			t.Fatal("expected an exemplar")
		}
		counts[e.Value]++
	}
	// With weights 1 and 9 the heavy candidate is expected 90% of the time.
	// The bound is deliberately loose but deterministic for a fixed seed.
	if counts[2] < 800 {
		t.Fatalf("expected the weight-9 candidate to dominate, got %d/1000", counts[2])
	}
}

func TestZeroAndInvalidWeightsAreNormalized(t *testing.T) {
	clock := newFakeClock()
	r := exemplar.NewReservoir(exemplar.Config{
		MaxCandidates: 2,
		Window:        time.Hour,
		Policy:        exemplar.PolicyWeightedRandom,
		Clock:         clock,
		Rand:          rand.New(rand.NewSource(3)), //nolint:gosec // A deterministic seed is intended here.
	})
	r.Observe(exemplar.Exemplar{Value: 1, Weight: 0})
	r.Observe(exemplar.Exemplar{Value: 2, Weight: -5})
	seen := map[float64]bool{}
	for range 100 {
		e, ok := r.Exemplar()
		if !ok {
			t.Fatal("expected an exemplar")
		}
		seen[e.Value] = true
	}
	if len(seen) != 2 {
		t.Fatalf("expected both normalized-weight candidates to be selectable, got %v", seen)
	}
}

func TestLabelsAreCopiedOnObserveAndExemplar(t *testing.T) {
	r := exemplar.NewReservoir(exemplar.Config{
		MaxCandidates: 1,
		Window:        time.Hour,
		Clock:         newFakeClock(),
	})
	labels := map[string]string{"trace_id": "abc"}
	r.Observe(exemplar.Exemplar{Value: 1, Labels: labels})
	labels["trace_id"] = "mutated"
	e, ok := r.Exemplar()
	if !ok {
		t.Fatal("expected an exemplar")
	}
	if e.Labels["trace_id"] != "abc" {
		t.Fatalf("expected the reservoir to own a copy of the labels, got %q", e.Labels["trace_id"])
	}
	e.Labels["trace_id"] = "mutated"
	e2, ok := r.Exemplar()
	if !ok {
		t.Fatal("expected an exemplar")
	}
	if e2.Labels["trace_id"] != "abc" {
		t.Fatalf("expected the returned labels to be a copy, got %q", e2.Labels["trace_id"])
	}
}

func TestDisabledReservoir(t *testing.T) {
	r := exemplar.NewReservoir(exemplar.Config{
		MaxCandidates: -1,
		Clock:         newFakeClock(),
	})
	r.Observe(exemplar.Exemplar{Value: 1})
	if got := r.Len(); got != 0 {
		t.Fatalf("expected a disabled reservoir to retain nothing, got %d", got)
	}
	if _, ok := r.Exemplar(); ok {
		t.Fatal("expected a disabled reservoir to expose no exemplar")
	}
}

func TestZeroConfigUsesDefaults(t *testing.T) {
	clock := newFakeClock()
	r := exemplar.NewReservoir(exemplar.Config{Clock: clock})
	for i := range 2 * exemplar.DefaultMaxCandidates {
		r.Observe(exemplar.Exemplar{Value: float64(i)})
		clock.Advance(time.Second)
	}
	if got := r.Len(); got != exemplar.DefaultMaxCandidates {
		t.Fatalf("expected the default of %d candidates, got %d", exemplar.DefaultMaxCandidates, got)
	}
	// The zero-value policy is PolicyNewest.
	e, ok := r.Exemplar()
	if !ok {
		t.Fatal("expected an exemplar")
	}
	if want := float64(2*exemplar.DefaultMaxCandidates - 1); e.Value != want {
		t.Fatalf("expected the zero-value policy to select the newest candidate %v, got %v", want, e.Value)
	}
	// The retained candidates were observed 1 to 32 seconds ago, so they
	// survive almost a full default window from now.
	clock.Advance(exemplar.DefaultWindow - 40*time.Second)
	if _, ok := r.Exemplar(); !ok {
		t.Fatal("expected candidates to survive within the default window")
	}
	clock.Advance(40 * time.Second)
	if _, ok := r.Exemplar(); ok {
		t.Fatal("expected candidates to expire after the default window")
	}
}

func TestConcurrentObserveAndExemplar(t *testing.T) {
	clock := newFakeClock()
	r := exemplar.NewReservoir(exemplar.Config{
		MaxCandidates: 8,
		Window:        10 * time.Millisecond,
		Policy:        exemplar.PolicyUniformRandom,
		Clock:         clock,
		Rand:          rand.New(rand.NewSource(1)), //nolint:gosec // A deterministic seed is intended here.
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				r.Observe(exemplar.Exemplar{Value: float64(i)})
				clock.Advance(time.Millisecond)
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				r.Exemplar()
				if got := r.Len(); got > 8 {
					t.Errorf("candidate count exceeded the bound: %d", got)
				}
			}
		}()
	}
	wg.Wait()
	if got := r.Len(); got > 8 {
		t.Fatalf("candidate count exceeded the bound: %d", got)
	}
}
