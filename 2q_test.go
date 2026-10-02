// Copyright IBM Corp. 2014, 2025
// SPDX-License-Identifier: MPL-2.0

package lru

import (
	"testing"
)

func setOf(keys []int) map[int]struct{} {
	set := make(map[int]struct{}, len(keys))
	for _, k := range keys {
		set[k] = struct{}{}
	}
	return set
}

func assertSet(t *testing.T, name string, got []int, want map[int]struct{}) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", name, got, want)
	}
	for _, k := range got {
		if _, ok := want[k]; !ok {
			t.Fatalf("%s: unexpected key %d, got %v, want %v", name, k, got, want)
		}
	}
}

// assertConserved verifies the capacity invariants of the cache after
// every operation: total occupancy never exceeds capacity, the two live
// queues partition Len without duplicates, and ghost heat never leaks
// into the hit counters.
func assertConserved(t *testing.T, l *TwoQueueCache[int, int]) {
	t.Helper()
	if l.Len() > l.Cap() {
		t.Fatalf("capacity exceeded: len %d cap %d", l.Len(), l.Cap())
	}
	if n := l.recent.Len() + l.frequent.Len(); n != l.Len() {
		t.Fatalf("queue lengths do not add up: recent %d frequent %d len %d",
			l.recent.Len(), l.frequent.Len(), l.Len())
	}
	seen := make(map[int]struct{}, l.Len())
	for _, k := range l.recent.Keys() {
		if _, dup := seen[k]; dup {
			t.Fatalf("key %d present more than once", k)
		}
		seen[k] = struct{}{}
		if _, ok := l.recentHits[k]; !ok {
			t.Fatalf("recent key %d has no hit counter", k)
		}
	}
	for _, k := range l.frequent.Keys() {
		if _, dup := seen[k]; dup {
			t.Fatalf("key %d present more than once", k)
		}
		seen[k] = struct{}{}
		if _, ok := l.recentHits[k]; ok {
			t.Fatalf("frequent key %d still has a hit counter", k)
		}
	}
	for _, k := range l.recentEvict.Keys() {
		if _, ok := l.recentHits[k]; ok {
			t.Fatalf("ghost key %d must not carry heat", k)
		}
	}
}

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

// fixedTrace is a fixed access sequence made of a cyclic hot set
// interleaved with a one-off scan. The cyclic hot set {90, 91, 92} is
// established first with one insert followed by repeated hits; a single
// stream of scan keys 0..15 is then walked once, touching the hot set
// once more in the middle. Capacity invariants are checked after every
// operation.
func fixedTrace(t *testing.T, l *TwoQueueCache[int, int]) {
	for _, k := range []int{90, 91, 92} {
		l.Add(k, k)
		assertConserved(t, l)
	}
	for round := 0; round < 3; round++ {
		for _, k := range []int{90, 91, 92} {
			l.Get(k)
			assertConserved(t, l)
		}
	}
	for k := 0; k < 16; k++ {
		l.Add(k, k)
		assertConserved(t, l)
		l.Get(k)
		assertConserved(t, l)
		if k == 7 {
			l.Get(90)
			l.Get(91)
			l.Get(92)
			assertConserved(t, l)
		}
	}
}

// Test2Q_OldVsNew_FixedTrace replays the same fixed sequence in the
// classic mode and in the thresholded mode and asserts the concrete
// survivor sets plus capacity conservation. The threshold protects the
// cyclic hot set from one-off scan pollution, while the classic mode
// promotes each scanned key into frequent on the second touch.
func Test2Q_OldVsNew_FixedTrace(t *testing.T) {
	// Classic mode: the second touch on each scan key (its Get)
	// promotes it into frequent, so the frequent queue churns through
	// the scan. The hot set is evicted before its mid-scan revisit.
	old, err := New2QParams[int, int](8, 0.25, 0.5)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	fixedTrace(t, old)
	assertConserved(t, old)

	if n := old.frequent.Len(); n != 8 {
		t.Fatalf("old frequent len: got %d, want 8", n)
	}
	if n := old.recent.Len(); n != 0 {
		t.Fatalf("old recent len: got %d, want 0", n)
	}
	if n := old.Len(); n != 8 {
		t.Fatalf("old len: got %d, want 8", n)
	}
	wantOld := setOf([]int{8, 9, 10, 11, 12, 13, 14, 15})
	assertSet(t, "old survivors", old.Keys(), wantOld)

	// New mode: scan keys only get one hit and never reach the
	// promotion threshold, so the cyclic hot set survives in frequent
	// while the tail of the scan stays in recent.
	neu, err := New2QWithConfig[int, int](TwoQueueConfig{
		Size:          8,
		RecentRatio:   0.25,
		GhostRatio:    0.5,
		PromotionHits: 3,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	fixedTrace(t, neu)
	assertConserved(t, neu)

	if n := neu.frequent.Len(); n != 3 {
		t.Fatalf("new frequent len: got %d, want 3", n)
	}
	if n := neu.recent.Len(); n != 5 {
		t.Fatalf("new recent len: got %d, want 5", n)
	}
	if n := neu.Len(); n != 8 {
		t.Fatalf("new len: got %d, want 8", n)
	}
	assertSet(t, "new frequent", neu.frequent.Keys(), setOf([]int{90, 91, 92}))
	assertSet(t, "new recent", neu.recent.Keys(), setOf([]int{11, 12, 13, 14, 15}))

	// The surviving sets genuinely differ: the hot set only survives
	// in the new mode.
	for _, k := range []int{90, 91, 92} {
		if _, ok := wantOld[k]; ok {
			t.Fatalf("hot key %d must be evicted in the old mode", k)
		}
	}
	for _, k := range []int{90, 91, 92} {
		if v, ok := neu.Get(k); !ok || v != k {
			t.Fatalf("hot key %d must survive in the new mode", k)
		}
	}
	for _, k := range []int{8, 9, 10} {
		if _, ok := neu.Peek(k); ok {
			t.Fatalf("scan key %d must be evicted in the new mode", k)
		}
	}
}

// Test2Q_PromotionHits checks that a recent entry is promoted only
// after the configured number of hits, while re-adding it replaces
// its value without disturbing its accumulated heat.
func Test2Q_PromotionHits(t *testing.T) {
	l, err := New2QWithConfig[int, int](TwoQueueConfig{
		Size:          4,
		RecentRatio:   1.0,
		GhostRatio:    0.5,
		PromotionHits: 3,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	l.Add(1, 1)
	l.Add(2, 2)
	l.Add(3, 3)
	l.Add(4, 4)
	if n := l.recent.Len(); n != 4 {
		t.Fatalf("recent len: got %d, want 4", n)
	}

	// Two hits are not enough to promote key 1.
	l.Get(1)
	l.Get(1)
	if n := l.recent.Len(); n != 4 {
		t.Fatalf("recent len: got %d, want 4", n)
	}
	if n := l.frequent.Len(); n != 0 {
		t.Fatalf("frequent len: got %d, want 0", n)
	}
	if n := l.recentHits[1]; n != 2 {
		t.Fatalf("hits for 1: got %d, want 2", n)
	}

	// Replacing key 1 keeps its heat, so it needs one more hit to
	// promote.
	l.Add(1, 10)
	if n := l.recentHits[1]; n != 2 {
		t.Fatalf("hits for 1 after replace: got %d, want 2", n)
	}
	if v, ok := l.recent.Peek(1); !ok || v != 10 {
		t.Fatalf("key 1 value: got %d, want 10", v)
	}

	// The third hit promotes key 1.
	l.Get(1)
	if v, ok := l.Get(1); !ok || v != 10 {
		t.Fatalf("key 1 value: got %d, want 10", v)
	}
	if n := l.frequent.Len(); n != 1 {
		t.Fatalf("frequent len: got %d, want 1", n)
	}
	if n := l.recent.Len(); n != 3 {
		t.Fatalf("recent len: got %d, want 3", n)
	}
	if _, ok := l.recentHits[1]; ok {
		t.Fatalf("promoted key 1 must not keep heat")
	}
	assertConserved(t, l)
}

// Test2Q_AdaptiveRatio checks that each ghost hit raises the target
// recent ratio by a fixed step, clamped to the upper bound, while the
// non-adaptive mode never moves the ratio.
func Test2Q_AdaptiveRatio(t *testing.T) {
	build := func(adapt bool) *TwoQueueCache[int, int] {
		l, err := New2QWithConfig[int, int](TwoQueueConfig{
			Size:            20,
			RecentRatio:     0.2,
			GhostRatio:      1.0,
			PromotionHits:   2,
			AdaptRatio:      adapt,
			RecentRatioMin:  0.2,
			RecentRatioMax:  0.5,
			RecentRatioStep: 0.1,
		})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		return l
	}

	// Fill the cache with fresh keys: nothing promotes (the threshold
	// is two hits) and nothing is evicted until capacity is reached.
	l := build(true)
	for k := 0; k < 20; k++ {
		l.Add(k, k)
	}
	if n := l.recent.Len(); n != 20 {
		t.Fatalf("recent len: got %d, want 20", n)
	}
	if n := l.frequent.Len(); n != 0 {
		t.Fatalf("frequent len: got %d, want 0", n)
	}
	if n := l.recentEvict.Len(); n != 0 {
		t.Fatalf("ghost len: got %d, want 0", n)
	}
	if l.recentRatio != 0.2 || l.recentSize != 4 {
		t.Fatalf("ratio before adaptation: got %f (%d), want 0.2 (4)",
			l.recentRatio, l.recentSize)
	}

	// Push three more keys through so the oldest recent keys 0, 1, 2
	// land in ghost, then revive them. Each ghost hit raises the
	// target recent ratio by exactly one fixed step until it reaches
	// the upper bound.
	for k := 20; k < 23; k++ {
		l.Add(k, k)
	}
	assertSet(t, "ghost", l.recentEvict.Keys(), setOf([]int{0, 1, 2}))

	// First ghost hit: 0.2 + 0.1, target recent grows 4 -> 6.
	l.Add(0, 0)
	if d := l.recentRatio - 0.3; d > 1e-9 || d < -1e-9 {
		t.Fatalf("ratio after first ghost hit: got %.16f", l.recentRatio)
	}
	if n := l.recentSize; n != 6 {
		t.Fatalf("recentSize after first ghost hit: got %d, want 6", n)
	}
	if _, ok := l.recentHits[0]; ok {
		t.Fatalf("ghost-revived key must not carry heat")
	}

	// Second ghost hit: 0.3 + 0.1, target recent grows 6 -> 8.
	l.Add(1, 1)
	if d := l.recentRatio - 0.4; d > 1e-9 || d < -1e-9 {
		t.Fatalf("ratio after second ghost hit: got %.16f", l.recentRatio)
	}
	if n := l.recentSize; n != 8 {
		t.Fatalf("recentSize after second ghost hit: got %d, want 8", n)
	}

	// Further ghost hits clamp at the upper bound of 0.5 (size 10).
	l.Add(2, 2)
	l.Add(5, 5)
	l.Add(6, 6)
	if d := l.recentRatio - 0.5; d > 1e-9 || d < -1e-9 {
		t.Fatalf("ratio at upper bound: got %.16f", l.recentRatio)
	}
	if n := l.recentSize; n != 10 {
		t.Fatalf("recentSize at upper bound: got %d, want 10", n)
	}
	assertConserved(t, l)

	// The non-adaptive cache never adapts on ghost hits.
	plain := build(false)
	for k := 0; k < 20; k++ {
		plain.Add(k, k)
	}
	for k := 20; k < 23; k++ {
		plain.Add(k, k)
	}
	plain.Add(0, 0)
	if plain.recentRatio != 0.2 || plain.recentSize != 4 {
		t.Fatalf("non-adaptive ratio moved: %f (%d)",
			plain.recentRatio, plain.recentSize)
	}
	assertConserved(t, plain)
}

// Test2Q_HeatCleared covers removal, eviction into ghost, purge and
// replacement: every one of these must clear the corresponding heat.
func Test2Q_HeatCleared(t *testing.T) {
	newCache := func(t *testing.T) *TwoQueueCache[int, int] {
		t.Helper()
		l, err := New2QWithConfig[int, int](TwoQueueConfig{
			Size:          4,
			RecentRatio:   1.0,
			GhostRatio:    0.5,
			PromotionHits: 3,
		})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		return l
	}

	t.Run("remove", func(t *testing.T) {
		l := newCache(t)
		l.Add(1, 1)
		l.Add(2, 2)
		l.Get(1)
		l.Remove(1)
		if _, ok := l.recentHits[1]; ok {
			t.Fatalf("Remove must clear heat")
		}
	})

	t.Run("evict to ghost", func(t *testing.T) {
		// Use a separate, wider cache so the heated keys remain the
		// oldest eviction candidates once it fills.
		l, err := New2QWithConfig[int, int](TwoQueueConfig{
			Size:          6,
			RecentRatio:   1.0,
			GhostRatio:    0.5,
			PromotionHits: 3,
		})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		// Insert and heat keys 1 and 2 first; every later key is
		// strictly newer in the recency order.
		l.Add(1, 1)
		l.Add(2, 2)
		l.Get(1)
		l.Get(2)
		for k := 3; k <= 6; k++ {
			l.Add(k, k)
		}
		if l.recentHits[1] != 1 || l.recentHits[2] != 1 {
			t.Fatalf("precondition: hits 1=%d 2=%d, want 1",
				l.recentHits[1], l.recentHits[2])
		}
		// Cache is full; adding fresh key 7 evicts oldest recent key 1.
		l.Add(7, 7)
		if _, ok := l.recentHits[1]; ok {
			t.Fatalf("eviction into ghost must clear heat")
		}
		if !l.recentEvict.Contains(1) {
			t.Fatalf("key 1 should be a ghost entry")
		}
		// Adding fresh key 8 evicts key 2, which also carried heat.
		l.Add(8, 8)
		if _, ok := l.recentHits[2]; ok {
			t.Fatalf("eviction into ghost must clear heat")
		}
		if !l.recentEvict.Contains(2) {
			t.Fatalf("key 2 should be a ghost entry")
		}
		assertConserved(t, l)
	})

	t.Run("readd keeps heat", func(t *testing.T) {
		// Re-adding an existing recent key only replaces the value; it
		// must not reset or inflate the heat, and the threshold still
		// governs promotion.
		l, err := New2QWithConfig[int, int](TwoQueueConfig{
			Size:          2,
			RecentRatio:   1.0,
			GhostRatio:    0.5,
			PromotionHits: 2,
		})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		l.Add(1, 1)
		l.Get(1)
		l.Add(1, 10)
		if n := l.recentHits[1]; n != 1 {
			t.Fatalf("re-adding must keep heat, got %d, want 1", n)
		}
		if v, ok := l.recent.Peek(1); !ok || v != 10 {
			t.Fatalf("re-adding must update the value, got %d", v)
		}
		l.Get(1)
		if n := l.frequent.Len(); n != 1 {
			t.Fatalf("expected promotion after second hit, frequent len %d", n)
		}
		assertConserved(t, l)
	})

	t.Run("purge", func(t *testing.T) {
		l := newCache(t)
		l.Add(3, 3)
		l.Add(4, 4)
		l.Get(3)
		l.Add(5, 5)
		l.Purge()
		if len(l.recentHits) != 0 {
			t.Fatalf("Purge must clear all heat, got %d entries", len(l.recentHits))
		}
		if l.Len() != 0 || l.recentEvict.Len() != 0 {
			t.Fatalf("Purge must empty all queues")
		}
		// Re-adding a previously purged key starts with no heat.
		l.Add(3, 30)
		if l.recentHits[3] != 0 {
			t.Fatalf("re-added key must start with zero heat, got %d", l.recentHits[3])
		}
		assertConserved(t, l)
	})
}

func Test2Q_InvalidConfig(t *testing.T) {
	cases := map[string]TwoQueueConfig{
		"zero size":          {Size: 0},
		"negative recent":    {Size: 1, RecentRatio: -0.1},
		"recent over one":    {Size: 1, RecentRatio: 1.1},
		"negative ghost":     {Size: 1, GhostRatio: -0.1},
		"ghost over one":     {Size: 1, GhostRatio: 1.1},
		"negative promotion": {Size: 1, PromotionHits: -1},
		"min over max":       {Size: 1, AdaptRatio: true, RecentRatioMin: 0.9, RecentRatioMax: 0.1},
		"ratio below min":    {Size: 1, RecentRatio: 0.05, AdaptRatio: true},
		"ratio above max":    {Size: 1, RecentRatio: 0.95, AdaptRatio: true},
		"zero step":          {Size: 1, AdaptRatio: true, RecentRatioStep: -0.1},
		"step over one":      {Size: 1, AdaptRatio: true, RecentRatioStep: 1.1},
	}
	for name, config := range cases {
		if _, err := New2QWithConfig[int, int](config); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}
