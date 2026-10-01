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

package quantile_test

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/prometheus/client_golang/exp/quantile"
)

// datasets returns the acceptance-test data sets: uniform, highly
// skewed, and dominated by duplicate values.
func datasets(n int, rng *rand.Rand) map[string][]float64 {
	uniform := make([]float64, n)
	for i := range uniform {
		uniform[i] = rng.Float64() * 1000
	}
	skewed := make([]float64, n)
	for i := range skewed {
		// Log-normal with a heavy right tail.
		skewed[i] = math.Exp(rng.NormFloat64() * 2)
	}
	duplicates := make([]float64, n)
	for i := range duplicates {
		if rng.Float64() < 0.9 {
			duplicates[i] = 42
		} else {
			duplicates[i] = rng.Float64()
		}
	}
	return map[string][]float64{
		"uniform":    uniform,
		"skewed":     skewed,
		"duplicates": duplicates,
	}
}

// rankBounds returns the number of elements strictly below v and the
// number of elements below or equal to v in sorted data.
func rankBounds(sorted []float64, v float64) (lo, hi int) {
	lo = sort.SearchFloat64s(sorted, v)
	hi = sort.Search(len(sorted), func(i int) bool { return sorted[i] > v })
	return lo, hi
}

// checkQuantiles verifies the rank error guarantee of s against the
// exact sorted data for a range of quantiles.
func checkQuantiles(t *testing.T, s *quantile.Sketch, sorted []float64) {
	t.Helper()
	n := float64(len(sorted))
	bound := s.RankErrorBound() * n
	for _, q := range []float64{0.01, 0.1, 0.25, 0.5, 0.75, 0.9, 0.99} {
		v := s.Quantile(q)
		lo, hi := rankBounds(sorted, v)
		if float64(lo) > q*n+bound || float64(hi) < q*n-bound {
			t.Errorf(
				"quantile %v: returned value %v with true rank in [%d, %d], want rank within %v of %v",
				q, v, lo, hi, bound, q*n,
			)
		}
	}
}

func newSketch(t *testing.T, bufferSize, maxLevels int, seed uint64) *quantile.Sketch {
	t.Helper()
	s, err := quantile.NewSketch(quantile.SketchConfig{
		BufferSize: bufferSize,
		MaxLevels:  maxLevels,
		Rand:       rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// mergeViaDigests exports every sketch to a Digest, round-trips it
// through JSON as a worker boundary would, and merges the digests into
// a fresh sketch in the given order.
func mergeViaDigests(t *testing.T, sketches []*quantile.Sketch, order []int) *quantile.Sketch {
	t.Helper()
	merged := newSketch(t, 64, 32, 1)
	for _, i := range order {
		d := sketches[i].Export()
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var decoded quantile.Digest
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if err := merged.MergeDigest(decoded); err != nil {
			t.Fatal(err)
		}
	}
	return merged
}

func TestRankErrorBoundOneShotAndMerged(t *testing.T) {
	const (
		n      = 50000
		shards = 7
	)
	rng := rand.New(rand.NewPCG(42, 0))
	for name, data := range datasets(n, rng) {
		t.Run(name, func(t *testing.T) {
			sorted := append([]float64(nil), data...)
			sort.Float64s(sorted)

			// One-shot: observe everything into a single sketch.
			oneShot := newSketch(t, 64, 32, 1)
			for _, v := range data {
				oneShot.Observe(v)
			}
			checkQuantiles(t, oneShot, sorted)

			// Shard the data across workers, one sketch each.
			workers := make([]*quantile.Sketch, shards)
			for i := range workers {
				workers[i] = newSketch(t, 64, 32, uint64(i+2))
			}
			for i, v := range data {
				workers[i%shards].Observe(v)
			}

			// Merge in different orders: forward, reverse, and a
			// pairwise tree. Digests cross a JSON round-trip each
			// time, as they would across worker boundaries.
			orders := map[string][]int{
				"forward": {0, 1, 2, 3, 4, 5, 6},
				"reverse": {6, 5, 4, 3, 2, 1, 0},
				"tree":    {0, 1, 2, 3, 4, 5, 6}, // Merged pairwise below.
			}
			for orderName, order := range orders {
				t.Run(orderName, func(t *testing.T) {
					var merged *quantile.Sketch
					if orderName == "tree" {
						// Pairwise tree merge: (0+1), (2+3),
						// (4+5), then combine with 6.
						pairs := make([]*quantile.Sketch, 0, shards/2+1)
						for i := 0; i+1 < shards; i += 2 {
							pairs = append(pairs, mergeViaDigests(t, workers, []int{i, i + 1}))
						}
						pairs = append(pairs, mergeViaDigests(t, workers, []int{shards - 1}))
						merged = pairs[0]
						for _, p := range pairs[1:] {
							if err := merged.Merge(p); err != nil {
								t.Fatal(err)
							}
						}
					} else {
						merged = mergeViaDigests(t, workers, order)
					}

					if merged.Count() != oneShot.Count() {
						t.Errorf("count: got %d, want %d", merged.Count(), oneShot.Count())
					}
					// Float addition is not associative, so the
					// sharded sum may differ slightly from the
					// one-shot sum.
					if diff := math.Abs(merged.Sum() - oneShot.Sum()); diff > 1e-9*math.Abs(oneShot.Sum()) {
						t.Errorf("sum: got %v, want %v", merged.Sum(), oneShot.Sum())
					}
					checkQuantiles(t, merged, sorted)
					t.Logf("rank error bound: %.4f (one-shot %.4f)", merged.RankErrorBound(), oneShot.RankErrorBound())
				})
			}
		})
	}
}

func TestRankErrorBoundWithMemoryCap(t *testing.T) {
	const n = 100000
	rng := rand.New(rand.NewPCG(7, 0))
	data := datasets(n, rng)["skewed"]
	sorted := append([]float64(nil), data...)
	sort.Float64s(sorted)

	// A tight memory cap forces the top level to compact into
	// itself. The rank error bound must still hold.
	oneShot := newSketch(t, 32, 4, 1)
	for _, v := range data {
		oneShot.Observe(v)
	}
	if got, want := oneShot.RetainedSamples(), 4*32; got > want {
		t.Errorf("retained samples %d exceed memory cap %d", got, want)
	}
	checkQuantiles(t, oneShot, sorted)

	// The same must hold when merging capped sketches.
	merged := newSketch(t, 32, 4, 2)
	half := newSketch(t, 32, 4, 3)
	for i, v := range data {
		if i%2 == 0 {
			merged.Observe(v)
		} else {
			half.Observe(v)
		}
	}
	if err := merged.MergeDigest(half.Export()); err != nil {
		t.Fatal(err)
	}
	if got, want := merged.RetainedSamples(), 4*32; got > want {
		t.Errorf("retained samples %d exceed memory cap %d", got, want)
	}
	checkQuantiles(t, merged, sorted)
}

func TestDigestCompatibilityChecks(t *testing.T) {
	s := newSketch(t, 64, 32, 1)
	for i := 0; i < 1000; i++ {
		s.Observe(float64(i))
	}
	d := s.Export()

	t.Run("version mismatch", func(t *testing.T) {
		bad := d
		bad.Version++
		if err := s.MergeDigest(bad); !errors.Is(err, quantile.ErrIncompatibleDigest) {
			t.Errorf("got %v, want ErrIncompatibleDigest", err)
		}
		if _, err := quantile.NewFromDigest(bad); !errors.Is(err, quantile.ErrIncompatibleDigest) {
			t.Errorf("got %v, want ErrIncompatibleDigest", err)
		}
	})
	t.Run("buffer size mismatch", func(t *testing.T) {
		bad := d
		bad.BufferSize = 128
		if err := s.MergeDigest(bad); !errors.Is(err, quantile.ErrIncompatibleDigest) {
			t.Errorf("got %v, want ErrIncompatibleDigest", err)
		}
	})
	t.Run("max levels mismatch", func(t *testing.T) {
		bad := d
		bad.MaxLevels = 16
		if err := s.MergeDigest(bad); !errors.Is(err, quantile.ErrIncompatibleDigest) {
			t.Errorf("got %v, want ErrIncompatibleDigest", err)
		}
	})
	t.Run("sketch parameter mismatch", func(t *testing.T) {
		other := newSketch(t, 128, 32, 1)
		if err := s.Merge(other); !errors.Is(err, quantile.ErrIncompatibleDigest) {
			t.Errorf("got %v, want ErrIncompatibleDigest", err)
		}
	})
	t.Run("failed merge leaves sketch unmodified", func(t *testing.T) {
		before := s.Export()
		bad := d
		bad.Version++
		if err := s.MergeDigest(bad); err == nil {
			t.Fatal("expected an error")
		}
		after := s.Export()
		if before.Count != after.Count || before.Sum != after.Sum || before.ErrorBound != after.ErrorBound {
			t.Error("sketch was modified by a rejected digest")
		}
	})
	t.Run("round trip", func(t *testing.T) {
		clone, err := quantile.NewFromDigest(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []float64{0.1, 0.5, 0.9} {
			if got, want := clone.Quantile(q), s.Quantile(q); got != want {
				t.Errorf("quantile %v: got %v, want %v", q, got, want)
			}
		}
		if clone.Count() != s.Count() || clone.Sum() != s.Sum() {
			t.Error("count or sum mismatch after round trip")
		}
	})
}

func TestInvalidConfig(t *testing.T) {
	for _, cfg := range []quantile.SketchConfig{
		{BufferSize: 3},
		{BufferSize: 5},
		{BufferSize: 4, MaxLevels: -1},
	} {
		if _, err := quantile.NewSketch(cfg); err == nil {
			t.Errorf("NewSketch(%+v): expected an error", cfg)
		}
	}
}

func TestEmptySketch(t *testing.T) {
	s := newSketch(t, 64, 32, 1)
	if got := s.Quantile(0.5); !math.IsNaN(got) {
		t.Errorf("empty sketch quantile: got %v, want NaN", got)
	}
	if got := s.RankErrorBound(); got != 0 {
		t.Errorf("empty sketch rank error bound: got %v, want 0", got)
	}
	s.Observe(math.NaN())
	if s.Count() != 0 {
		t.Error("NaN observations must be ignored")
	}
}

func TestExactMinMax(t *testing.T) {
	s := newSketch(t, 8, 2, 1)
	rng := rand.New(rand.NewPCG(3, 0))
	data := datasets(10000, rng)["uniform"]
	sorted := append([]float64(nil), data...)
	sort.Float64s(sorted)
	for _, v := range data {
		s.Observe(v)
	}
	if got, want := s.Quantile(0), sorted[0]; got != want {
		t.Errorf("0-quantile: got %v, want exact min %v", got, want)
	}
	if got, want := s.Quantile(1), sorted[len(sorted)-1]; got != want {
		t.Errorf("1-quantile: got %v, want exact max %v", got, want)
	}
}
