//  Copyright (c) 2026 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vellum

import (
	"bytes"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

// buildInMemory constructs an FST from a sorted key/value map without touching
// the filesystem.
func buildInMemory(t *testing.T, m map[string]uint64) *FST {
	t.Helper()
	var buf bytes.Buffer
	b, err := New(&buf, nil)
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	if err := insertStringMap(b, m); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close builder: %v", err)
	}
	fst, err := Load(buf.Bytes())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return fst
}

// enumerateFilter is the reference implementation: enumerate every key under
// the prefix in order and keep those whose value is in [minVal, maxVal].
func enumerateFilter(t *testing.T, fst *FST, m map[string]uint64,
	prefix string, minVal, maxVal uint64) []kvPair {
	t.Helper()

	keys := make([]string, 0, len(m))
	for k := range m {
		if bytes.HasPrefix([]byte(k), []byte(prefix)) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	rv := make([]kvPair, 0)
	for _, k := range keys {
		v := m[k]
		if v >= minVal && v <= maxVal {
			rv = append(rv, kvPair{k, v})
		}
	}
	return rv
}

type kvPair struct {
	key string
	val uint64
}

func collectRange(t *testing.T, it *RangeIterator) []kvPair {
	t.Helper()
	rv := make([]kvPair, 0)
	if it.done {
		return rv
	}
	for {
		k, v := it.Current()
		rv = append(rv, kvPair{string(k), v})
		if err := it.Next(); err != nil {
			if err != ErrIteratorDone {
				t.Fatalf("next: %v", err)
			}
			break
		}
	}
	return rv
}

// sharedSuffixSample deliberately uses keys that all share the suffix "ax".
// During minimization the final "ax" chain is reused by multiple prefixes,
// while those prefixes arrive with very different accumulated (absolute)
// outputs.  Residual statistics must remain path independent to stay correct.
func sharedSuffixSample() map[string]uint64 {
	return map[string]uint64{
		"ax":   100,
		"bax":  50,
		"baxx": 500,
		"cax":  300,
		"dax":  499,
		"eax":  500,
		"fax":  501,
		"gax":  1000,
		"max":  math.MaxUint64,
		"tax":  0,
		"zax":  250,
	}
}

func TestPrefixRangeSharedSuffix(t *testing.T) {
	m := sharedSuffixSample()
	fst := buildInMemory(t, m)
	defer func() {
		if err := fst.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	stats, err := NewStateStats(fst)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	cases := []struct {
		prefix string
		min    uint64
		max    uint64
	}{
		{"", 100, 500},
		{"", 0, 0},
		{"", math.MaxUint64, math.MaxUint64},
		{"", 0, math.MaxUint64},
		{"", 501, 499}, // empty interval
		{"", 100, 100},
		{"", 499, 501},
		{"b", 0, math.MaxUint64},
		{"b", 50, 500},
		{"ba", 50, 500},
		{"c", 0, math.MaxUint64},
		{"m", 0, math.MaxUint64},
		{"m", math.MaxUint64 - 1, math.MaxUint64},
		{"t", 0, 0},
		{"zzz", 0, math.MaxUint64}, // prefix absent
		{"g", 1000, 1000},
	}

	for _, c := range cases {
		it, err := stats.RangeIterator([]byte(c.prefix), c.min, c.max)
		if err != nil {
			t.Fatalf("range %q [%d,%d]: %v", c.prefix, c.min, c.max, err)
		}
		got := collectRange(t, it)
		want := enumerateFilter(t, fst, m, c.prefix, c.min, c.max)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("range %q [%d,%d] mismatch\n got %v\nwant %v",
				c.prefix, c.min, c.max, got, want)
		}
		if err := it.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrefixRangeConvenience(t *testing.T) {
	m := sharedSuffixSample()
	fst := buildInMemory(t, m)
	defer func() {
		if err := fst.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	it, err := fst.PrefixRange(nil, 100, 500)
	if err != nil {
		t.Fatal(err)
	}
	got := collectRange(t, it)
	want := enumerateFilter(t, fst, m, "", 100, 500)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("convenience mismatch\n got %v\nwant %v", got, want)
	}
}

// TestStateStatsDoesNotMutateFST ensures building the auxiliary statistics is
// strictly read-only and never changes the encoded FST representation.
func TestStateStatsDoesNotMutateFST(t *testing.T) {
	m := sharedSuffixSample()
	var buf bytes.Buffer
	b, err := New(&buf, nil)
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	if err := insertStringMap(b, m); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	original := make([]byte, buf.Len())
	copy(original, buf.Bytes())

	fst, err := Load(buf.Bytes())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := NewStateStats(fst); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !bytes.Equal(original, buf.Bytes()) {
		t.Fatalf("FST bytes changed after building StateStats")
	}
	if err := fst.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestPrefixRangeRandomized builds many random dictionaries and compares the
// pruning iterator against enumeration-filtering for a variety of intervals.
func TestPrefixRangeRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	for iter := 0; iter < 200; iter++ {
		m := randomDict(rng, 1+rng.Intn(60))
		fst := buildInMemory(t, m)

		stats, err := NewStateStats(fst)
		if err != nil {
			fst.Close()
			t.Fatalf("stats: %v", err)
		}

		prefixes := []string{"", randomPrefix(rng)}
		for _, p := range prefixes {
			for q := 0; q < 6; q++ {
				lo, hi := randomInterval(rng)
				it, err := stats.RangeIterator([]byte(p), lo, hi)
				if err != nil {
					fst.Close()
					t.Fatalf("range: %v", err)
				}
				got := collectRange(t, it)
				want := enumerateFilter(t, fst, m, p, lo, hi)
				if !reflect.DeepEqual(got, want) {
					fst.Close()
					t.Fatalf("iter %d prefix %q [%d,%d] mismatch\n got %v\nwant %v\nm=%v",
						iter, p, lo, hi, got, want, m)
				}
				it.Close()
			}
		}

		if err := fst.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func randomDict(rng *rand.Rand, n int) map[string]uint64 {
	m := make(map[string]uint64, n)
	for len(m) < n {
		l := 1 + rng.Intn(5)
		buf := make([]byte, l)
		for i := range buf {
			// Restrict to a small alphabet to encourage shared suffixes.
			buf[i] = byte('a' + rng.Intn(4))
		}
		var v uint64
		switch rng.Intn(12) {
		case 0:
			v = 0
		case 1:
			v = math.MaxUint64
		case 2:
			v = uint64(rng.Intn(10))
		default:
			v = uint64(rng.Intn(2000))
		}
		m[string(buf)] = v
	}
	return m
}

func randomPrefix(rng *rand.Rand) string {
	l := rng.Intn(3)
	buf := make([]byte, l)
	for i := range buf {
		buf[i] = byte('a' + rng.Intn(4))
	}
	return string(buf)
}

func randomInterval(rng *rand.Rand) (uint64, uint64) {
	switch rng.Intn(8) {
	case 0:
		return 0, 0
	case 1:
		return math.MaxUint64, math.MaxUint64
	case 2:
		return 0, math.MaxUint64
	case 3:
		return 500, 499 // empty
	}
	a := uint64(rng.Intn(1000))
	b := uint64(rng.Intn(1000))
	if a > b {
		return b, a
	}
	return a, b
}
