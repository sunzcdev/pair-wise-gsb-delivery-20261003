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

package exemplars_test

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/exp/exemplars"
)

// fakeClock is a manually advanced Clock for deterministic expiry tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// scriptRand is a deterministic Rand cycling through scripted values, used
// to make candidate replacement outcomes exact.
type scriptRand struct {
	floats []float64
	ints   []int
	fi     int
	ii     int
}

func (r *scriptRand) Float64() float64 {
	v := r.floats[r.fi%len(r.floats)]
	r.fi++
	return v
}

func (r *scriptRand) Intn(n int) int {
	v := r.ints[r.ii%len(r.ints)] % n
	r.ii++
	return v
}

func observeN(r *exemplars.Reservoir, n int) {
	for i := 0; i < n; i++ {
		r.Observe(exemplars.Observation{
			Value:  float64(i),
			Labels: map[string]string{"trace_id": fmt.Sprintf("trace-%d", i)},
		})
	}
}

func TestRetentionIsBounded(t *testing.T) {
	r := exemplars.NewReservoir(exemplars.Config{
		Capacity: 10,
		Clock:    newFakeClock(),
		Rand:     rand.New(rand.NewSource(1)),
	})
	observeN(r, 1000)
	if got := r.Len(); got != 10 {
		t.Fatalf("expected candidate count to be bounded at 10, got %d", got)
	}
}

func TestRandomReplacementIsScriptable(t *testing.T) {
	clock := newFakeClock()
	r := exemplars.NewReservoir(exemplars.Config{
		Capacity: 2,
		Clock:    clock,
		// Intn always returns 0, so the oldest inserted slot is replaced.
		Rand: &scriptRand{floats: []float64{0.5}, ints: []int{0}},
	})
	r.Observe(exemplars.Observation{Value: 1})
	r.Observe(exemplars.Observation{Value: 2})
	r.Observe(exemplars.Observation{Value: 3})
	got := r.Select(10)
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(got))
	}
	values := map[float64]bool{got[0].Value: true, got[1].Value: true}
	if !values[3] || !values[2] {
		t.Fatalf("expected slot 0 to be replaced by value 3, got %v", values)
	}
}

func TestExpiryPrunesCandidates(t *testing.T) {
	clock := newFakeClock()
	r := exemplars.NewReservoir(exemplars.Config{
		Capacity: 10,
		Window:   time.Minute,
		Clock:    clock,
		Rand:     rand.New(rand.NewSource(1)),
	})
	observeN(r, 5)
	clock.advance(2 * time.Minute)
	if got := r.Len(); got != 0 {
		t.Fatalf("expected all candidates to expire, got %d", got)
	}
	if got := r.Select(5); got != nil {
		t.Fatalf("expected no exemplars after expiry, got %v", got)
	}
	// The pool must stay empty after pruned scrapes, and new observations
	// must be retained again.
	r.Observe(exemplars.Observation{Value: 42})
	if got := r.Len(); got != 1 {
		t.Fatalf("expected 1 candidate after new observation, got %d", got)
	}
}

func TestExpiredObservationDropped(t *testing.T) {
	clock := newFakeClock()
	r := exemplars.NewReservoir(exemplars.Config{
		Capacity: 10,
		Window:   time.Minute,
		Clock:    clock,
		Rand:     rand.New(rand.NewSource(1)),
	})
	r.Observe(exemplars.Observation{
		Value: 1,
		Time:  clock.Now().Add(-2 * time.Minute),
	})
	if got := r.Len(); got != 0 {
		t.Fatalf("expected already-expired observation to be dropped, got %d candidates", got)
	}
}

func TestUniformSelectionIsUniformOverPool(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	r := exemplars.NewReservoir(exemplars.Config{
		Capacity: 3,
		Clock:    newFakeClock(),
		Rand:     rng,
	})
	observeN(r, 3)
	counts := map[float64]int{}
	trials := 30000
	for i := 0; i < trials; i++ {
		got := r.Select(1)
		if len(got) != 1 {
			t.Fatalf("expected 1 exemplar, got %d", len(got))
		}
		counts[got[0].Value]++
	}
	for v := 0.0; v < 3; v++ {
		p := float64(counts[v]) / float64(trials)
		if p < 0.30 || p > 0.37 {
			t.Fatalf("expected roughly uniform selection, value %v picked with probability %v", v, p)
		}
	}
}

func TestWeightedSelectionProportionalToWeight(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	r := exemplars.NewReservoir(exemplars.Config{
		Capacity: 10,
		Weighted: true,
		Clock:    newFakeClock(),
		Rand:     rng,
	})
	r.Observe(exemplars.Observation{Value: 100, Weight: 100})
	for i := 0; i < 9; i++ {
		r.Observe(exemplars.Observation{Value: float64(i), Weight: 1})
	}
	heavy := 0
	trials := 10000
	for i := 0; i < trials; i++ {
		if got := r.Select(1); got[0].Value == 100 {
			heavy++
		}
	}
	// Theoretical probability is 100/109 ~= 0.917.
	if p := float64(heavy) / float64(trials); p < 0.88 || p > 0.95 {
		t.Fatalf("expected weight-proportional selection ~= 0.917, got %v", p)
	}
}

func TestSelectionIsReproducible(t *testing.T) {
	newReservoir := func() *exemplars.Reservoir {
		r := exemplars.NewReservoir(exemplars.Config{
			Capacity: 8,
			Clock:    newFakeClock(),
			Rand:     rand.New(rand.NewSource(42)),
		})
		observeN(r, 8)
		return r
	}
	first := newReservoir().Select(4)
	second := newReservoir().Select(4)
	if len(first) != len(second) {
		t.Fatalf("selection length mismatch: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Value != second[i].Value {
			t.Fatalf("selection not reproducible at position %d: %v vs %v", i, first[i].Value, second[i].Value)
		}
	}
}

func TestSelectWithoutReplacement(t *testing.T) {
	r := exemplars.NewReservoir(exemplars.Config{
		Capacity: 5,
		Clock:    newFakeClock(),
		Rand:     rand.New(rand.NewSource(3)),
	})
	observeN(r, 5)
	got := r.Select(10)
	if len(got) != 5 {
		t.Fatalf("expected selection capped at pool size 5, got %d", len(got))
	}
	seen := map[float64]bool{}
	for _, e := range got {
		if seen[e.Value] {
			t.Fatalf("expected selection without replacement, got duplicate value %v", e.Value)
		}
		seen[e.Value] = true
	}
}

func TestLabelsAreCloned(t *testing.T) {
	r := exemplars.NewReservoir(exemplars.Config{
		Capacity: 1,
		Clock:    newFakeClock(),
		Rand:     rand.New(rand.NewSource(1)),
	})
	labels := map[string]string{"trace_id": "abc"}
	r.Observe(exemplars.Observation{Value: 1, Labels: labels})
	labels["trace_id"] = "mutated"
	got := r.Select(1)
	if got[0].Labels["trace_id"] != "abc" {
		t.Fatalf("expected reservoir to clone labels, got %q", got[0].Labels["trace_id"])
	}
	got[0].Labels["trace_id"] = "caller-mutated"
	if again := r.Select(1); again[0].Labels["trace_id"] != "abc" {
		t.Fatalf("expected caller to own returned labels, got %q", again[0].Labels["trace_id"])
	}
}

func TestNonPositiveWeightTreatedAsOne(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	r := exemplars.NewReservoir(exemplars.Config{
		Capacity: 2,
		Weighted: true,
		Clock:    newFakeClock(),
		Rand:     rng,
	})
	r.Observe(exemplars.Observation{Value: 1, Weight: 0})
	r.Observe(exemplars.Observation{Value: 2, Weight: -3})
	counts := map[float64]int{}
	trials := 20000
	for i := 0; i < trials; i++ {
		counts[r.Select(1)[0].Value]++
	}
	// Both weights collapse to 1, so selection must be roughly 50/50.
	if p := float64(counts[1]) / float64(trials); p < 0.46 || p > 0.54 {
		t.Fatalf("expected non-positive weights to behave as 1, got probability %v", p)
	}
}

func TestConcurrentObserveAndSelect(t *testing.T) {
	clock := newFakeClock()
	r := exemplars.NewReservoir(exemplars.Config{
		Capacity: 32,
		Window:   time.Minute,
		Clock:    clock,
		Rand:     rand.New(rand.NewSource(9)),
	})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				r.Observe(exemplars.Observation{
					Value:  float64(i),
					Labels: map[string]string{"trace_id": fmt.Sprintf("g%d-%d", g, i)},
				})
			}
		}(g)
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_ = r.Exemplars()
				clock.advance(time.Millisecond)
			}
		}()
	}
	wg.Wait()
	if got := r.Len(); got > 32 {
		t.Fatalf("candidate count exceeded capacity under concurrency: %d", got)
	}
}
