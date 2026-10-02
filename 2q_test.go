// Copyright IBM Corp. 2014, 2025
// SPDX-License-Identifier: MPL-2.0

package lru

import (
	"testing"
)

func Benchmark2Q_Rand(b *testing.B) {
	l, err := New2Q[int64, int64](8192)
	if err != nil {
		b.Fatalf("err: %v", err)
	}

	trace := make([]int64, b.N*2)
	for i := 0; i < b.N*2; i++ {
		trace[i] = getRand(b) % 32768
	}

	b.ResetTimer()

	var hit, miss int
	for i := 0; i < 2*b.N; i++ {
		if i%2 == 0 {
			l.Add(trace[i], trace[i])
		} else {
			if _, ok := l.Get(trace[i]); ok {
				hit++
			} else {
				miss++
			}
		}
	}
	b.Logf("hit: %d miss: %d ratio: %f", hit, miss, float64(hit)/float64(hit+miss))
}

func Benchmark2Q_Freq(b *testing.B) {
	l, err := New2Q[int64, int64](8192)
	if err != nil {
		b.Fatalf("err: %v", err)
	}

	trace := make([]int64, b.N*2)
	for i := 0; i < b.N*2; i++ {
		if i%2 == 0 {
			trace[i] = getRand(b) % 16384
		} else {
			trace[i] = getRand(b) % 32768
		}
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		l.Add(trace[i], trace[i])
	}
	var hit, miss int
	for i := 0; i < b.N; i++ {
		if _, ok := l.Get(trace[i]); ok {
			hit++
		} else {
			miss++
		}
	}
	b.Logf("hit: %d miss: %d ratio: %f", hit, miss, float64(hit)/float64(hit+miss))
}

func Test2Q_RandomOps(t *testing.T) {
	size := 128
	l, err := New2Q[int64, int64](128)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	n := 200000
	for i := 0; i < n; i++ {
		key := getRand(t) % 512
		r := getRand(t)
		switch r % 3 {
		case 0:
			l.Add(key, key)
		case 1:
			l.Get(key)
		case 2:
			l.Remove(key)
		}

		if l.recent.Len()+l.frequent.Len() > size {
			t.Fatalf("bad: recent: %d freq: %d",
				l.recent.Len(), l.frequent.Len())
		}
	}
}

func Test2Q_Get_RecentToFrequent(t *testing.T) {
	l, err := New2Q[int, int](128)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Touch all the entries, should be in t1
	for i := 0; i < 128; i++ {
		l.Add(i, i)
	}
	if n := l.recent.Len(); n != 128 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 0 {
		t.Fatalf("bad: %d", n)
	}

	// Get should upgrade to t2
	for i := 0; i < 128; i++ {
		if _, ok := l.Get(i); !ok {
			t.Fatalf("missing: %d", i)
		}
	}
	if n := l.recent.Len(); n != 0 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 128 {
		t.Fatalf("bad: %d", n)
	}

	// Get be from t2
	for i := 0; i < 128; i++ {
		if _, ok := l.Get(i); !ok {
			t.Fatalf("missing: %d", i)
		}
	}
	if n := l.recent.Len(); n != 0 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 128 {
		t.Fatalf("bad: %d", n)
	}
}

func Test2Q_Add_RecentToFrequent(t *testing.T) {
	l, err := New2Q[int, int](128)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Add initially to recent
	l.Add(1, 1)
	if n := l.recent.Len(); n != 1 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 0 {
		t.Fatalf("bad: %d", n)
	}

	// Add should upgrade to frequent
	l.Add(1, 1)
	if n := l.recent.Len(); n != 0 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 1 {
		t.Fatalf("bad: %d", n)
	}

	// Add should remain in frequent
	l.Add(1, 1)
	if n := l.recent.Len(); n != 0 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 1 {
		t.Fatalf("bad: %d", n)
	}
}

func Test2Q_Add_RecentEvict(t *testing.T) {
	l, err := New2Q[int, int](4)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Add 1,2,3,4,5 -> Evict 1
	l.Add(1, 1)
	l.Add(2, 2)
	l.Add(3, 3)
	l.Add(4, 4)
	l.Add(5, 5)
	if n := l.recent.Len(); n != 4 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.recentEvict.Len(); n != 1 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 0 {
		t.Fatalf("bad: %d", n)
	}

	// Pull in the recently evicted
	l.Add(1, 1)
	if n := l.recent.Len(); n != 3 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.recentEvict.Len(); n != 1 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 1 {
		t.Fatalf("bad: %d", n)
	}

	// Add 6, should cause another recent evict
	l.Add(6, 6)
	if n := l.recent.Len(); n != 3 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.recentEvict.Len(); n != 2 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 1 {
		t.Fatalf("bad: %d", n)
	}
}

func Test2Q_Resize(t *testing.T) {
	l, err := New2Q[int, int](100)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Touch all the entries, should be in t1
	for i := 0; i < 100; i++ {
		l.Add(i, i)
	}

	evicted := l.Resize(50)
	if evicted != 50 {
		t.Fatalf("bad: %d", evicted)
	}

	if n := l.recent.Len(); n != 50 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 0 {
		t.Fatalf("bad: %d", n)
	}

	l, err = New2Q[int, int](100)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for i := 0; i < 100; i++ {
		l.Add(i, i)
	}

	for i := 0; i < 50; i++ {
		l.Add(i, i)
	}

	evicted = l.Resize(50)
	if evicted != 50 {
		t.Fatalf("bad: %d", evicted)
	}

	if n := l.recent.Len(); n != 12 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 38 {
		t.Fatalf("bad: %d", n)
	}

	l, err = New2Q[int, int](100)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for i := 0; i < 100; i++ {
		l.Add(i, i)
		l.Add(i, i)
	}

	evicted = l.Resize(50)
	if evicted != 50 {
		t.Fatalf("bad: %d", evicted)
	}

	if n := l.recent.Len(); n != 0 {
		t.Fatalf("bad: %d", n)
	}
	if n := l.frequent.Len(); n != 50 {
		t.Fatalf("bad: %d", n)
	}
}

func Test2Q(t *testing.T) {
	l, err := New2Q[int, int](128)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	for i := 0; i < 256; i++ {
		l.Add(i, i)
	}
	if l.Len() != 128 {
		t.Fatalf("bad len: %v", l.Len())
	}
	if l.Cap() != 128 {
		t.Fatalf("expect %d, but %d", 128, l.Cap())
	}

	for i, k := range l.Keys() {
		if v, ok := l.Get(k); !ok || v != k || v != i+128 {
			t.Fatalf("bad key: %v", k)
		}
	}
	for i, v := range l.Values() {
		if v != i+128 {
			t.Fatalf("bad key: %v", v)
		}
	}
	for i := 0; i < 128; i++ {
		if _, ok := l.Get(i); ok {
			t.Fatalf("should be evicted")
		}
	}
	for i := 128; i < 256; i++ {
		if _, ok := l.Get(i); !ok {
			t.Fatalf("should not be evicted")
		}
	}
	for i := 128; i < 192; i++ {
		l.Remove(i)
		if _, ok := l.Get(i); ok {
			t.Fatalf("should be deleted")
		}
	}

	l.Purge()
	if l.Len() != 0 {
		t.Fatalf("bad len: %v", l.Len())
	}
	if _, ok := l.Get(200); ok {
		t.Fatalf("should contain nothing")
	}
}

// Test that Contains doesn't update recent-ness
func Test2Q_Contains(t *testing.T) {
	l, err := New2Q[int, int](2)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	l.Add(1, 1)
	l.Add(2, 2)
	if !l.Contains(1) {
		t.Errorf("1 should be contained")
	}

	l.Add(3, 3)
	if l.Contains(1) {
		t.Errorf("Contains should not have updated recent-ness of 1")
	}
}

// Test that Peek doesn't update recent-ness
func Test2Q_Peek(t *testing.T) {
	l, err := New2Q[int, int](2)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	l.Add(1, 1)
	l.Add(2, 2)
	if v, ok := l.Peek(1); !ok || v != 1 {
		t.Errorf("1 should be set to 1: %v, %v", v, ok)
	}

	l.Add(3, 3)
	if l.Contains(1) {
		t.Errorf("should not have updated recent-ness of 1")
	}
}

func Test2Q_AdaptiveParams_Validation(t *testing.T) {
	cases := []struct {
		name                       string
		size                       int
		recentRatio, ghostRatio    float64
		threshold                  int
		minRecent, maxRecent, step float64
	}{
		{"bad size", 0, 0.25, 0.5, 1, 0.25, 0.75, 0.25},
		{"bad recent ratio", 8, -0.1, 0.5, 1, 0.25, 0.75, 0.25},
		{"bad ghost ratio", 8, 0.25, 1.1, 1, 0.25, 0.75, 0.25},
		{"bad threshold", 8, 0.25, 0.5, 0, 0.25, 0.75, 0.25},
		{"bad min recent", 8, 0.25, 0.5, 1, -0.1, 0.75, 0.25},
		{"bad max recent", 8, 0.25, 0.5, 1, 0.25, 1.1, 0.25},
		{"min above max", 8, 0.25, 0.5, 1, 0.75, 0.25, 0.25},
		{"bad step", 8, 0.25, 0.5, 1, 0.25, 0.75, -0.1},
	}
	for _, tc := range cases {
		_, err := New2QAdaptiveParams[int, int](tc.size, tc.recentRatio,
			tc.ghostRatio, tc.threshold, tc.minRecent, tc.maxRecent, tc.step)
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
	}

	// The initial recent target is clamped into the [min, max] bounds
	l, err := New2QAdaptiveParams[int, int](8, 0.0, 0.5, 1, 0.25, 0.5, 0.25)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if l.recentSize != 2 {
		t.Fatalf("bad: %d", l.recentSize)
	}
	l, err = New2QAdaptiveParams[int, int](8, 1.0, 0.5, 1, 0.25, 0.5, 0.25)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if l.recentSize != 4 {
		t.Fatalf("bad: %d", l.recentSize)
	}
}

func Test2Q_AdaptiveThreshold_Promote(t *testing.T) {
	l, err := New2QAdaptiveParams[int, int](8, 0.25, 0.5, 2, 0.25, 0.75, 0.25)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	l.Add(1, 1)
	if _, ok := l.Get(1); !ok {
		t.Fatalf("missing: 1")
	}
	if l.frequent.Contains(1) {
		t.Fatalf("promoted before reaching the threshold")
	}
	if !l.recent.Contains(1) {
		t.Fatalf("should still be recent")
	}

	if _, ok := l.Get(1); !ok {
		t.Fatalf("missing: 1")
	}
	if !l.frequent.Contains(1) {
		t.Fatalf("should be promoted at the threshold")
	}
	if l.recent.Contains(1) {
		t.Fatalf("should have left recent")
	}
	if _, ok := l.recentHits[1]; ok {
		t.Fatalf("heat should be cleared on promotion")
	}
}

func Test2Q_AdaptiveThreshold_HeatHygiene(t *testing.T) {
	l, err := New2QAdaptiveParams[int, int](16, 0.25, 0.5, 3, 0.25, 0.75, 0.25)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	l.Add(1, 1)
	l.Get(1)
	l.Get(1)
	if got := l.recentHits[1]; got != 2 {
		t.Fatalf("bad: %d", got)
	}

	// Replacing the value resets the heat and keeps the key in recent
	l.Add(1, 100)
	if got := l.recentHits[1]; got != 0 {
		t.Fatalf("bad: %d", got)
	}
	if !l.recent.Contains(1) || l.frequent.Contains(1) {
		t.Fatalf("replaced key should stay in recent")
	}
	if v, ok := l.Peek(1); !ok || v != 100 {
		t.Fatalf("bad: %v %v", v, ok)
	}

	// Promotion requires a fresh set of hits after the replace
	l.Get(1)
	l.Get(1)
	if l.frequent.Contains(1) {
		t.Fatalf("promoted before the fresh threshold")
	}
	l.Get(1)
	if !l.frequent.Contains(1) {
		t.Fatalf("should be promoted")
	}

	// Remove clears the heat of the key
	l.Add(2, 2)
	l.Get(2)
	if got := l.recentHits[2]; got != 1 {
		t.Fatalf("bad: %d", got)
	}
	l.Remove(2)
	if _, ok := l.recentHits[2]; ok {
		t.Fatalf("heat should be cleared on remove")
	}

	// Purge clears all heat
	l.Add(3, 3)
	l.Get(3)
	l.Purge()
	if len(l.recentHits) != 0 {
		t.Fatalf("heat should be cleared on purge")
	}
}

func Test2Q_AdaptiveThreshold_RatioBounds(t *testing.T) {
	l, err := New2QAdaptiveParams[int, int](8, 0.25, 0.5, 1, 0.25, 0.5, 0.25)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Overflow the cache to seed the ghost list
	for i := 1; i <= 12; i++ {
		l.Add(i, i)
	}
	if l.recentSize != 2 {
		t.Fatalf("bad: %d", l.recentSize)
	}

	// Each ghost hit grows the recent target by one step (2 entries)
	l.Add(1, 1)
	if l.recentSize != 4 {
		t.Fatalf("bad: %d", l.recentSize)
	}

	// The target is capped at the max ratio (0.5 * 8 = 4)
	l.Add(2, 2)
	if l.recentSize != 4 {
		t.Fatalf("bad: %d", l.recentSize)
	}
	if n := l.recent.Len() + l.frequent.Len(); n > 8 {
		t.Fatalf("over capacity: %d", n)
	}
}

// Test2Q_AdaptiveThreshold_ScanComparison replays a fixed access sequence
// mixing a cyclic hot set with one-time scans through the classic 2Q and
// the adaptive 2Q, and compares the exact retained key sets.
func Test2Q_AdaptiveThreshold_ScanComparison(t *testing.T) {
	const size = 8

	type op struct {
		get bool
		key int
	}
	var ops []op
	add := func(k int) { ops = append(ops, op{false, k}) }
	get := func(k int) { ops = append(ops, op{true, k}) }

	// Cyclic hot set, each key seen three times up front
	for _, k := range []int{1, 2, 3} {
		add(k)
		get(k)
		get(k)
	}
	// First one-time scan
	for k := 10; k <= 17; k++ {
		add(k)
	}
	// Hot set re-access, then lukewarm keys seen exactly twice
	for _, k := range []int{1, 2, 3} {
		get(k)
	}
	for _, k := range []int{20, 21, 22} {
		add(k)
		get(k)
	}
	// Second one-time scan
	for k := 30; k <= 37; k++ {
		add(k)
	}
	// Re-request of a scanned-out lukewarm key (ghost hit)
	add(22)
	get(22)
	// Final one-time scan
	for k := 40; k <= 42; k++ {
		add(k)
	}

	classic, err := New2QParams[int, int](size, Default2QRecentRatio, Default2QGhostEntries)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	adaptive, err := New2QAdaptiveParams[int, int](size, 0.25, 0.5, 2, 0.25, 0.75, 0.25)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Replay the same fixed sequence against both caches, checking
	// capacity conservation after every single operation
	for i, o := range ops {
		for _, l := range []*TwoQueueCache[int, int]{classic, adaptive} {
			if o.get {
				l.Get(o.key)
			} else {
				l.Add(o.key, o.key)
			}
			if n := l.recent.Len() + l.frequent.Len(); n > size {
				t.Fatalf("op %d: over capacity: %d", i, n)
			}
		}
	}

	keySet := func(l *TwoQueueCache[int, int]) map[int]bool {
		out := make(map[int]bool)
		for _, k := range l.Keys() {
			out[k] = true
		}
		return out
	}
	check := func(name string, l *TwoQueueCache[int, int], want []int) {
		if l.Len() != size {
			t.Fatalf("%s: bad len: %d", name, l.Len())
		}
		got := keySet(l)
		if len(got) != len(want) {
			t.Fatalf("%s: bad keys: %v", name, got)
		}
		for _, k := range want {
			if !got[k] {
				t.Fatalf("%s: missing key %d in %v", name, k, got)
			}
		}
	}

	// The classic cache promotes the lukewarm keys 20,21,22 on their
	// second touch, so they survive the scans in the frequent list
	check("classic", classic, []int{1, 2, 3, 20, 21, 22, 41, 42})

	// The adaptive cache requires a second hit before promoting, so the
	// lukewarm keys are scanned out; 22 is recovered via the ghost list
	// and the grown recent target retains more of the final scan
	check("adaptive", adaptive, []int{1, 2, 3, 22, 37, 40, 41, 42})

	// The ghost hit grew the recent target by exactly one step
	if adaptive.recentSize != 4 {
		t.Fatalf("bad: %d", adaptive.recentSize)
	}
	if classic.recentSize != 2 {
		t.Fatalf("bad: %d", classic.recentSize)
	}
}
